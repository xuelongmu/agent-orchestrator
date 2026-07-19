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

var hookCommandPattern = regexp.MustCompile(`\bao hooks ([a-z0-9-]+) ([a-z0-9-]+)`)

var normalizedHookEvents = map[string]bool{
	"session-start":         true,
	"user-prompt-submit":    true,
	"pre-tool-use":          true,
	"post-tool-use":         true,
	"post-tool-use-failure": true,
	"permission-request":    true,
	"stop":                  true,
	"stop-failure":          true,
	"notification":          true,
	"session-end":           true,
}

// RegistryOptions supplies the externally-known sides of the adapter contract.
// BinaryNames are test-only executable names placed on an isolated PATH; they
// let GetLaunchCommand be exercised without depending on or invoking real CLIs.
type RegistryOptions struct {
	KnownHarnesses    []domain.AgentHarness
	BinaryNames       map[domain.AgentHarness][]string
	KnownHookTokens   []string
	SupportsHookToken func(token string) bool
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
			tokens := runAdapter(t, harness, manifest, agent, opts)
			for _, token := range tokens {
				foundHookTokens[token] = struct{}{}
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

func runAdapter(t *testing.T, harness domain.AgentHarness, manifest adapters.Manifest, agent ports.Agent, opts RegistryOptions) []string {
	t.Helper()
	validateManifest(t, manifest)
	validateCancellation(t, agent)

	spec, err := agent.GetConfigSpec(context.Background())
	if err != nil {
		t.Fatalf("GetConfigSpec: %v", err)
	}
	validateConfigSpec(t, spec)

	for _, cfg := range []ports.LaunchConfig{
		{},
		{Kind: domain.KindWorker, Prompt: "test prompt"},
		{Kind: domain.KindOrchestrator},
		{Kind: domain.KindOrchestrator, Prompt: "test prompt"},
	} {
		strategy, err := agent.GetPromptDeliveryStrategy(context.Background(), cfg)
		if err != nil {
			t.Errorf("GetPromptDeliveryStrategy(%s, prompt=%t): %v", cfg.Kind, cfg.Prompt != "", err)
			continue
		}
		if !slices.Contains([]ports.PromptDeliveryStrategy{
			ports.PromptDeliveryInCommand,
			ports.PromptDeliveryAfterStart,
			ports.PromptDeliveryCustomAgent,
		}, strategy) {
			t.Errorf("GetPromptDeliveryStrategy(%s, prompt=%t) = %q", cfg.Kind, cfg.Prompt != "", strategy)
		}
	}

	sandbox := t.TempDir()
	isolateUserEnvironment(t, sandbox)
	binDir := filepath.Join(sandbox, "bin")
	installFakeBinaries(t, binDir, opts.BinaryNames[harness])
	t.Setenv("PATH", binDir)

	workspace := filepath.Join(sandbox, "workspace")
	dataDir := filepath.Join(sandbox, "data")
	if err := os.MkdirAll(workspace, 0o750); err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	hookCfg := ports.WorkspaceHookConfig{
		DataDir:       dataDir,
		Env:           map[string]string{"KIMI_CODE_HOME": filepath.Join(dataDir, "kimi")},
		SessionID:     "conformance-session",
		WorkspacePath: workspace,
	}
	if err := agent.GetAgentHooks(context.Background(), hookCfg); err != nil {
		t.Fatalf("GetAgentHooks in sandbox: %v", err)
	}

	launch, err := agent.GetLaunchCommand(context.Background(), ports.LaunchConfig{
		DataDir:       dataDir,
		Kind:          domain.KindWorker,
		Prompt:        "test prompt",
		SessionID:     "conformance-session",
		WorkspacePath: workspace,
	})
	if err != nil {
		t.Fatalf("GetLaunchCommand with fake CLI: %v", err)
	}
	if len(launch) == 0 || strings.TrimSpace(launch[0]) == "" {
		t.Error("GetLaunchCommand returned an empty command")
	}

	hookText := strings.Join(launch, "\n") + "\n" + readSandboxFiles(t, sandbox)
	tokens := hookTokens(hookText)
	for _, token := range tokens {
		if opts.SupportsHookToken == nil || !opts.SupportsHookToken(token) {
			t.Errorf("hook token %q has no activity dispatcher", token)
		}
	}
	return tokens
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

func validateCancellation(t *testing.T, agent ports.Agent) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	assertCanceled(t, "GetConfigSpec", func() error { _, err := agent.GetConfigSpec(ctx); return err })
	assertCanceled(t, "GetLaunchCommand", func() error { _, err := agent.GetLaunchCommand(ctx, ports.LaunchConfig{}); return err })
	assertCanceled(t, "GetPromptDeliveryStrategy", func() error { _, err := agent.GetPromptDeliveryStrategy(ctx, ports.LaunchConfig{}); return err })
	assertCanceled(t, "GetAgentHooks", func() error { return agent.GetAgentHooks(ctx, ports.WorkspaceHookConfig{}) })
	assertCanceled(t, "GetRestoreCommand", func() error { _, _, err := agent.GetRestoreCommand(ctx, ports.RestoreConfig{}); return err })
	assertCanceled(t, "SessionInfo", func() error { _, _, err := agent.SessionInfo(ctx, ports.SessionRef{}); return err })
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

func hookTokens(text string) []string {
	seen := map[string]bool{}
	var tokens []string
	for _, match := range hookCommandPattern.FindAllStringSubmatch(text, -1) {
		// Comments can discuss "ao hooks must never ...". Only normalized hook
		// event names establish an executable callback edge.
		if !normalizedHookEvents[match[2]] {
			continue
		}
		if !seen[match[1]] {
			tokens = append(tokens, match[1])
			seen[match[1]] = true
		}
	}
	return tokens
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
