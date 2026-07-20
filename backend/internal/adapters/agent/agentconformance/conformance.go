// Package agentconformance provides a reusable contract test for agent
// adapters. It is intentionally imported only by tests: adapters remain opaque
// ports.Agent implementations and need no production-only conformance API.
package agentconformance

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

var (
	hookCommandPattern    = regexp.MustCompile("\\bao hooks ([a-z0-9-]+) ([^\\s\\\"'`]+)")
	hookTemplatePattern   = regexp.MustCompile(`\bao hooks ([a-z0-9-]+) \$\{hookName\}`)
	hookInvocationPattern = regexp.MustCompile(`\bcallHookSync\("([^"]+)"`)
)

var normalizedHookEvents = map[string]bool{
	"after-agent":           true,
	"after-tool":            true,
	"before-agent":          true,
	"session-start":         true,
	"user-prompt-submit":    true,
	"pre-tool-use":          true,
	"post-tool-use":         true,
	"post-tool-use-failure": true,
	"permission-request":    true,
	"permission-result":     true,
	"stop":                  true,
	"stop-failure":          true,
	"notification":          true,
	"session-end":           true,
}

// RegistryOptions supplies the externally-known sides of the adapter contract.
// BinaryNames are test-only executable names placed on an isolated PATH; they
// let GetLaunchCommand be exercised without depending on or invoking real CLIs.
type RegistryOptions struct {
	KnownHarnesses         []domain.AgentHarness
	BinaryNames            map[domain.AgentHarness][]string
	KnownHookTokens        []string
	SupportsHookToken      func(token string) bool
	DispatchHook           func(token, event string, payload []byte) (domain.ActivityState, bool)
	AllowsMetadataOnlyHook func(token, event string) bool
}

// RunRegistry exercises every constructor result without filtering non-Agent
// adapters. The all-at-once form also catches duplicate ids and drift between
// the registry and the domain harness vocabulary.
func RunRegistry(t *testing.T, registered []adapters.Adapter, opts RegistryOptions) {
	t.Helper()

	if len(registered) != len(opts.KnownHarnesses) {
		t.Errorf("registered adapter count = %d, known harness count = %d", len(registered), len(opts.KnownHarnesses))
	}

	known := make(map[domain.AgentHarness]struct{}, len(opts.KnownHarnesses))
	for _, harness := range opts.KnownHarnesses {
		if _, duplicate := known[harness]; duplicate {
			t.Errorf("known harness %q appears more than once", harness)
		}
		known[harness] = struct{}{}
	}

	seen := make(map[domain.AgentHarness]struct{}, len(registered))
	foundHookTokens := make(map[string]struct{})
	for i, adapter := range registered {
		if isNil(adapter) {
			t.Errorf("constructor %d returned nil", i)
			continue
		}
		manifest := adapter.Manifest()
		harness := domain.AgentHarness(manifest.ID)
		if _, ok := known[harness]; !ok {
			t.Errorf("constructor %d manifest id %q is not a known harness", i, manifest.ID)
		}
		if _, duplicate := seen[harness]; duplicate {
			t.Errorf("manifest id %q is registered more than once", manifest.ID)
		}
		seen[harness] = struct{}{}

		agent, ok := adapter.(ports.Agent)
		if !ok {
			t.Errorf("constructor %d (%q) does not implement ports.Agent", i, manifest.ID)
			continue
		}

		t.Run(string(harness), func(t *testing.T) {
			hooks := runAdapter(t, harness, manifest, agent, opts)
			for _, hook := range hooks {
				foundHookTokens[hook.Token] = struct{}{}
			}
		})
	}

	for _, harness := range opts.KnownHarnesses {
		if _, ok := seen[harness]; !ok {
			t.Errorf("known harness %q has no registered adapter", harness)
		}
	}
	for _, token := range opts.KnownHookTokens {
		if _, ok := foundHookTokens[token]; !ok {
			t.Errorf("activity dispatcher token %q is not reachable from any adapter hook", token)
		}
	}
}

func runAdapter(t *testing.T, harness domain.AgentHarness, manifest adapters.Manifest, agent ports.Agent, opts RegistryOptions) []HookCommand {
	t.Helper()
	validateManifest(t, manifest)

	sandbox := t.TempDir()
	isolateUserEnvironment(t, sandbox)
	binDir := filepath.Join(sandbox, "bin")
	installFakeBinaries(t, binDir, opts.BinaryNames[harness])
	t.Setenv("PATH", binDir)

	workspace := filepath.Join(sandbox, "workspace")
	dataDir := filepath.Join(sandbox, "data")
	for _, dir := range []string{workspace, dataDir} {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatalf("create conformance directory: %v", err)
		}
	}
	standingPromptFile := filepath.Join(dataDir, "standing-prompt.md")
	if err := os.WriteFile(standingPromptFile, []byte("AO conformance standing instructions\n"), 0o600); err != nil {
		t.Fatalf("write standing prompt fixture: %v", err)
	}
	hookCfg := ports.WorkspaceHookConfig{
		DataDir:          dataDir,
		Env:              map[string]string{"KIMI_CODE_HOME": filepath.Join(dataDir, "kimi")},
		SessionID:        "conformance-session",
		SystemPromptFile: standingPromptFile,
		WorkspacePath:    workspace,
	}
	launchCfg := ports.LaunchConfig{
		DataDir:          dataDir,
		SessionID:        "conformance-session",
		SystemPromptFile: standingPromptFile,
		WorkspacePath:    workspace,
	}
	sessionRef := ports.SessionRef{
		ID:            "conformance-session",
		Metadata:      map[string]string{},
		WorkspacePath: workspace,
	}
	validateCancellation(t, agent, launchCfg, hookCfg, ports.RestoreConfig{Session: sessionRef}, sessionRef)

	spec, err := agent.GetConfigSpec(context.Background())
	if err != nil {
		t.Fatalf("GetConfigSpec: %v", err)
	}
	validateConfigSpec(t, spec)

	if err := agent.GetAgentHooks(context.Background(), hookCfg); err != nil {
		t.Fatalf("GetAgentHooks in sandbox: %v", err)
	}
	workspaceArtifacts := readSandboxFiles(t, workspace)

	var launchText strings.Builder
	for _, cfg := range []ports.LaunchConfig{
		{Kind: domain.KindWorker},
		{Kind: domain.KindWorker, Prompt: "ao-conformance-worker-prompt"},
		{Kind: domain.KindOrchestrator},
		{Kind: domain.KindOrchestrator, Prompt: "ao-conformance-orchestrator-prompt"},
	} {
		cfg.DataDir = dataDir
		cfg.SessionID = "conformance-session"
		cfg.SystemPromptFile = standingPromptFile
		cfg.WorkspacePath = workspace
		strategy, err := agent.GetPromptDeliveryStrategy(context.Background(), cfg)
		if err != nil {
			t.Errorf("GetPromptDeliveryStrategy(%s, prompt=%t): %v", cfg.Kind, cfg.Prompt != "", err)
			continue
		}
		launch, err := agent.GetLaunchCommand(context.Background(), cfg)
		if err != nil {
			t.Errorf("GetLaunchCommand(%s, prompt=%t) with fake CLI: %v", cfg.Kind, cfg.Prompt != "", err)
			continue
		}
		if len(launch) == 0 || strings.TrimSpace(launch[0]) == "" {
			t.Errorf("GetLaunchCommand(%s, prompt=%t) returned an empty command", cfg.Kind, cfg.Prompt != "")
			continue
		}
		validatePromptDelivery(t, strategy, cfg, launch, workspaceArtifacts)
		launchText.WriteString(strings.Join(launch, "\n"))
		launchText.WriteByte('\n')
	}

	hookText := launchText.String() + readSandboxFiles(t, sandbox)
	hooks, err := hookCommands(hookText)
	if err != nil {
		t.Error(err)
	}
	validateHookDispatch(t, hooks, opts)
	return hooks
}

func validatePromptDelivery(
	t *testing.T,
	strategy ports.PromptDeliveryStrategy,
	cfg ports.LaunchConfig,
	launch []string,
	workspaceArtifacts string,
) {
	t.Helper()
	legal := slices.Contains([]ports.PromptDeliveryStrategy{
		ports.PromptDeliveryInCommand,
		ports.PromptDeliveryAfterStart,
		ports.PromptDeliveryCustomAgent,
	}, strategy)
	if !legal {
		t.Errorf("prompt delivery strategy = %q", strategy)
		return
	}
	containsPrompt := cfg.Prompt != "" && strings.Contains(strings.Join(launch, "\x00"), cfg.Prompt)
	switch strategy {
	case ports.PromptDeliveryInCommand:
		if cfg.Prompt != "" && !containsPrompt {
			t.Errorf("in-command prompt %q is absent from launch argv %q", cfg.Prompt, launch)
		}
	case ports.PromptDeliveryAfterStart:
		if containsPrompt {
			t.Errorf("after-start prompt %q leaked into launch argv %q", cfg.Prompt, launch)
		}
	case ports.PromptDeliveryCustomAgent:
		if cfg.Prompt != "" {
			t.Errorf("custom-agent delivery cannot carry a separate prompt %q", cfg.Prompt)
		}
		validateCustomAgentInstructions(t, cfg, workspaceArtifacts)
	}
}

func validateCustomAgentInstructions(t *testing.T, cfg ports.LaunchConfig, workspaceArtifacts string) {
	t.Helper()
	if err := customAgentInstructionsError(cfg, workspaceArtifacts); err != nil {
		t.Error(err)
	}
}

func customAgentInstructionsError(cfg ports.LaunchConfig, workspaceArtifacts string) error {
	if strings.TrimSpace(cfg.SystemPrompt) == "" && strings.TrimSpace(cfg.SystemPromptFile) == "" {
		return errors.New("custom-agent delivery has no standing SystemPrompt or SystemPromptFile")
	}
	if strings.TrimSpace(workspaceArtifacts) == "" {
		return errors.New("custom-agent delivery generated no workspace artifact")
	}

	evidence := strings.TrimSpace(cfg.SystemPrompt) != "" && strings.Contains(workspaceArtifacts, cfg.SystemPrompt)
	if promptFile := strings.TrimSpace(cfg.SystemPromptFile); promptFile != "" {
		for _, reference := range []string{promptFile, filepath.ToSlash(promptFile), "file://" + filepath.ToSlash(promptFile)} {
			if strings.Contains(workspaceArtifacts, reference) {
				evidence = true
			}
		}
		prompt, err := os.ReadFile(promptFile) //nolint:gosec // test-owned standing prompt fixture
		if err != nil {
			return fmt.Errorf("read custom-agent SystemPromptFile: %w", err)
		} else if body := strings.TrimSpace(string(prompt)); body != "" && strings.Contains(workspaceArtifacts, body) {
			evidence = true
		}
	}
	if !evidence {
		return errors.New("custom-agent workspace artifacts do not contain or reference standing instructions")
	}
	return nil
}

func validateHookDispatch(t *testing.T, hooks []HookCommand, opts RegistryOptions) {
	t.Helper()
	signaled := map[string]bool{}
	seenToken := map[string]bool{}
	for _, hook := range hooks {
		seenToken[hook.Token] = true
		if !normalizedHookEvents[hook.Event] {
			t.Errorf("generated hook %s/%s uses an unknown event", hook.Token, hook.Event)
			continue
		}
		if opts.SupportsHookToken == nil || !opts.SupportsHookToken(hook.Token) {
			t.Errorf("hook token %q has no activity dispatcher", hook.Token)
			continue
		}
		if opts.DispatchHook == nil {
			t.Errorf("hook token %q cannot be behaviorally dispatched", hook.Token)
			continue
		}
		state, ok := opts.DispatchHook(hook.Token, hook.Event, hookPayload(hook.Event))
		if ok {
			if !legalActivityState(state) {
				t.Errorf("dispatch %s/%s returned invalid activity state %q", hook.Token, hook.Event, state)
			}
			signaled[hook.Token] = true
		} else if opts.AllowsMetadataOnlyHook == nil || !opts.AllowsMetadataOnlyHook(hook.Token, hook.Event) {
			t.Errorf("generated hook %s/%s has no activity dispatch result", hook.Token, hook.Event)
		}
	}
	for token := range seenToken {
		if !signaled[token] {
			t.Errorf("hook token %q has no generated event that produces activity", token)
		}
		if _, ok := opts.DispatchHook(token, "agentconformance-unknown", nil); ok {
			t.Errorf("hook token %q accepts an unknown event", token)
		}
	}
}

func hookPayload(event string) []byte {
	if event == "notification" {
		return []byte(`{"notification_type":"agent_needs_input"}`)
	}
	return []byte(`{}`)
}

func legalActivityState(state domain.ActivityState) bool {
	return slices.Contains([]domain.ActivityState{
		domain.ActivityActive, domain.ActivityIdle, domain.ActivityWaitingInput,
		domain.ActivityBlocked, domain.ActivityRateLimited, domain.ActivityExited,
	}, state)
}

func validateManifest(t *testing.T, manifest adapters.Manifest) {
	t.Helper()
	for label, value := range map[string]string{
		"id": manifest.ID, "name": manifest.Name, "description": manifest.Description, "version": manifest.Version,
	} {
		if strings.TrimSpace(value) == "" {
			t.Errorf("manifest %s is empty", label)
		}
	}
	seen := map[adapters.Capability]bool{}
	for _, capability := range manifest.Capabilities {
		if capability == "" {
			t.Error("manifest contains an empty capability")
		}
		if seen[capability] {
			t.Errorf("manifest capability %q appears more than once", capability)
		}
		seen[capability] = true
	}
	if !seen[adapters.CapabilityAgent] {
		t.Error("manifest does not advertise CapabilityAgent")
	}
}

func validateConfigSpec(t *testing.T, spec ports.ConfigSpec) {
	t.Helper()
	seen := map[string]bool{}
	legalTypes := []ports.ConfigFieldType{
		ports.ConfigFieldString, ports.ConfigFieldBool, ports.ConfigFieldNumber,
		ports.ConfigFieldStringList, ports.ConfigFieldEnum,
	}
	for i, field := range spec.Fields {
		name := fmt.Sprintf("field[%d]", i)
		if strings.TrimSpace(field.Key) == "" {
			t.Errorf("%s key is empty", name)
		} else if seen[field.Key] {
			t.Errorf("config key %q appears more than once", field.Key)
		}
		seen[field.Key] = true
		if strings.TrimSpace(field.Description) == "" {
			t.Errorf("config key %q description is empty", field.Key)
		}
		if !slices.Contains(legalTypes, field.Type) {
			t.Errorf("config key %q has invalid type %q", field.Key, field.Type)
			continue
		}
		if field.Type == ports.ConfigFieldEnum {
			if len(field.Enum) == 0 {
				t.Errorf("enum config key %q has no values", field.Key)
			}
			seenValue := map[string]bool{}
			for _, value := range field.Enum {
				if strings.TrimSpace(value) == "" || seenValue[value] {
					t.Errorf("enum config key %q has empty or duplicate value %q", field.Key, value)
				}
				seenValue[value] = true
			}
			if field.Default != nil {
				value, ok := field.Default.(string)
				if !ok || !slices.Contains(field.Enum, value) {
					t.Errorf("enum config key %q default %#v is not an enum value", field.Key, field.Default)
				}
			}
		} else if len(field.Enum) != 0 {
			t.Errorf("non-enum config key %q declares enum values", field.Key)
		}
		if field.Default != nil && !defaultMatchesType(field.Default, field.Type) {
			t.Errorf("config key %q default %#v does not match type %q", field.Key, field.Default, field.Type)
		}
	}
}

func defaultMatchesType(value any, typ ports.ConfigFieldType) bool {
	switch typ {
	case ports.ConfigFieldString, ports.ConfigFieldEnum:
		_, ok := value.(string)
		return ok
	case ports.ConfigFieldBool:
		_, ok := value.(bool)
		return ok
	case ports.ConfigFieldNumber:
		switch reflect.TypeOf(value).Kind() {
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
			reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
			reflect.Float32, reflect.Float64:
			return true
		default:
			return false
		}
	case ports.ConfigFieldStringList:
		_, ok := value.([]string)
		return ok
	default:
		return false
	}
}

func validateCancellation(
	t *testing.T,
	agent ports.Agent,
	launchCfg ports.LaunchConfig,
	hookCfg ports.WorkspaceHookConfig,
	restoreCfg ports.RestoreConfig,
	session ports.SessionRef,
) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	assertCanceled(t, "GetConfigSpec", func() error { _, err := agent.GetConfigSpec(ctx); return err })
	assertCanceled(t, "GetLaunchCommand", func() error { _, err := agent.GetLaunchCommand(ctx, launchCfg); return err })
	assertCanceled(t, "GetPromptDeliveryStrategy", func() error { _, err := agent.GetPromptDeliveryStrategy(ctx, launchCfg); return err })
	assertCanceled(t, "GetAgentHooks", func() error { return agent.GetAgentHooks(ctx, hookCfg) })
	assertCanceled(t, "GetRestoreCommand", func() error { _, _, err := agent.GetRestoreCommand(ctx, restoreCfg); return err })
	assertCanceled(t, "SessionInfo", func() error { _, _, err := agent.SessionInfo(ctx, session); return err })
}

func assertCanceled(t *testing.T, method string, call func() error) {
	t.Helper()
	if err := call(); !errors.Is(err, context.Canceled) {
		t.Errorf("%s with canceled context returned %v, want context.Canceled", method, err)
	}
}

func isolateUserEnvironment(t *testing.T, root string) {
	t.Helper()
	home := filepath.Join(root, "home")
	appData := filepath.Join(root, "appdata")
	localAppData := filepath.Join(root, "localappdata")
	tempDir := filepath.Join(root, "tmp")
	for _, dir := range []string{home, appData, localAppData, tempDir} {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatalf("create environment sandbox: %v", err)
		}
	}
	for key, value := range map[string]string{
		"HOME": home, "USERPROFILE": home, "XDG_CONFIG_HOME": filepath.Join(home, ".config"),
		"APPDATA": appData, "LOCALAPPDATA": localAppData,
		"TMP": tempDir, "TEMP": tempDir, "TMPDIR": tempDir,
		"AUTOHAND_CONFIG": filepath.Join(root, "autohand", "config.json"),
		"KIMI_CODE_HOME":  filepath.Join(root, "kimi-code-home"),
	} {
		t.Setenv(key, value)
	}
}

func installFakeBinaries(t *testing.T, dir string, names []string) {
	t.Helper()
	if len(names) == 0 {
		t.Fatal("no fake CLI names configured for adapter")
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("create fake CLI dir: %v", err)
	}
	for _, name := range names {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("conformance test placeholder\n"), 0o600); err != nil {
			t.Fatalf("write fake CLI %q: %v", name, err)
		}
		if err := os.Chmod(path, 0o700); err != nil { //nolint:gosec // G302: owner-only test fixture must be executable
			t.Fatalf("make fake CLI %q executable: %v", name, err)
		}
		if runtime.GOOS == "windows" && filepath.Ext(name) == "" {
			if err := os.WriteFile(path+".cmd", []byte("@exit /b 0\r\n"), 0o600); err != nil {
				t.Fatalf("write fake CLI %q: %v", name+".cmd", err)
			}
		}
	}
}

func readSandboxFiles(t *testing.T, root string) string {
	t.Helper()
	var out strings.Builder
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.Type().IsRegular() || strings.Contains(path, string(filepath.Separator)+"bin"+string(filepath.Separator)) {
			return nil
		}
		data, err := os.ReadFile(path) //nolint:gosec // test-owned sandbox
		if err != nil {
			return err
		}
		out.Write(data)
		out.WriteByte('\n')
		return nil
	})
	if err != nil {
		t.Fatalf("read hook sandbox: %v", err)
	}
	return out.String()
}

// HookCommand is one generated callback edge from an agent hook into AO's
// activity dispatcher.
type HookCommand struct {
	Token string
	Event string
}

func hookCommands(text string) ([]HookCommand, error) {
	seen := map[HookCommand]bool{}
	var hooks []HookCommand
	uncommented := withoutSourceComments(text)
	add := func(hook HookCommand) {
		if !seen[hook] {
			hooks = append(hooks, hook)
			seen[hook] = true
		}
	}
	for _, match := range hookCommandPattern.FindAllStringSubmatch(uncommented, -1) {
		if match[2] == "${hookName}" {
			continue // correlated with concrete callHookSync invocations below
		}
		add(HookCommand{Token: match[1], Event: match[2]})
	}
	// Embedded JS/TS plugins build `ao hooks <token> ${hookName}` and call a
	// local dispatcher with concrete normalized event names. Correlate those
	// executable pieces instead of accepting examples written only in comments.
	for _, template := range hookTemplatePattern.FindAllStringSubmatch(uncommented, -1) {
		for _, invocation := range hookInvocationPattern.FindAllStringSubmatch(uncommented, -1) {
			add(HookCommand{Token: template[1], Event: invocation[1]})
		}
	}
	var unknown []string
	for _, hook := range hooks {
		if !normalizedHookEvents[hook.Event] {
			unknown = append(unknown, hook.Token+"/"+hook.Event)
		}
	}
	if len(unknown) != 0 {
		return hooks, fmt.Errorf("unknown executable hook events: %s", strings.Join(unknown, ", "))
	}
	return hooks, nil
}

// withoutSourceComments removes JavaScript/TypeScript line and block comments
// while preserving quoted command values and template literals. Hook assets
// include documentation examples, which must not count as executable callback
// edges in the conformance contract.
func withoutSourceComments(text string) string {
	var out strings.Builder
	var quote byte
	escaped := false
	lineComment := false
	blockComment := false
	for i := 0; i < len(text); i++ {
		ch := text[i]
		if lineComment {
			if ch == '\n' {
				lineComment = false
				out.WriteByte(ch)
			}
			continue
		}
		if blockComment {
			if ch == '*' && i+1 < len(text) && text[i+1] == '/' {
				blockComment = false
				i++
			} else if ch == '\n' {
				out.WriteByte(ch)
			}
			continue
		}
		if quote != 0 {
			out.WriteByte(ch)
			if escaped {
				escaped = false
			} else if ch == '\\' {
				escaped = true
			} else if ch == quote {
				quote = 0
			}
			continue
		}
		if ch == '\'' || ch == '"' || ch == '`' {
			quote = ch
			out.WriteByte(ch)
			continue
		}
		if ch == '/' && i+1 < len(text) {
			switch text[i+1] {
			case '/':
				lineComment = true
				i++
				continue
			case '*':
				blockComment = true
				i++
				continue
			}
		}
		out.WriteByte(ch)
	}
	return out.String()
}

func isNil(value any) bool {
	if value == nil {
		return true
	}
	v := reflect.ValueOf(value)
	switch v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return v.IsNil()
	default:
		return false
	}
}
