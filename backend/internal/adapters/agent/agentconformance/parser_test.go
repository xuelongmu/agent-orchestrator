package agentconformance

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters"
	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/agent/agentbase"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func TestHookCommandsUsesExecutableContent(t *testing.T) {
	text := `
// ao hooks comment-only stop
/*
ao hooks block-comment session-end
callHookSync("permission-request", {})
*/
{"command":"ao hooks literal user-prompt-submit"}
function hookCmd(hookName: string) {
  return ` + "`exec ao hooks template ${hookName}`" + `
}
callHookSync("session-start", {})
callHookSync("stop", {}) // ao hooks inline-comment notification
`
	want := []HookCommand{
		{Token: "literal", Event: "user-prompt-submit"},
		{Token: "template", Event: "session-start"},
		{Token: "template", Event: "stop"},
	}
	got, err := hookCommands(text)
	if err != nil {
		t.Fatalf("hookCommands error: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("hookCommands = %#v, want %#v", got, want)
	}
}

func TestHookCommandsRetainsUnknownExecutableEvents(t *testing.T) {
	text := `
// ao hooks ignored comment-typo
/* ao hooks ignored block-typo */
{"commands":[
  "ao hooks literal stop_failure",
  "ao hooks literal stop.failure",
  "ao hooks literal Stop"
]}
function hookCmd(hookName: string) {
  return ` + "`exec ao hooks template ${hookName}`" + `
}
callHookSync("stop_failure", {})
callHookSync("stop.failure", {})
callHookSync("Stop", {})
callHookSync("stop", {})
`
	want := []HookCommand{
		{Token: "literal", Event: "stop_failure"},
		{Token: "literal", Event: "stop.failure"},
		{Token: "literal", Event: "Stop"},
		{Token: "template", Event: "stop_failure"},
		{Token: "template", Event: "stop.failure"},
		{Token: "template", Event: "Stop"},
		{Token: "template", Event: "stop"},
	}
	got, err := hookCommands(text)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("hookCommands = %#v, want %#v", got, want)
	}
	for _, unknown := range []string{
		"literal/stop_failure", "literal/stop.failure", "literal/Stop",
		"template/stop_failure", "template/stop.failure", "template/Stop",
	} {
		if err == nil || !strings.Contains(err.Error(), unknown) {
			t.Errorf("hookCommands error = %v, want unknown event %q", err, unknown)
		}
	}
}

func TestCustomAgentInstructionsRequireGeneratedEvidence(t *testing.T) {
	promptFile := filepath.Join(t.TempDir(), "standing.md")
	if err := os.WriteFile(promptFile, []byte("standing instructions\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := ports.LaunchConfig{SystemPromptFile: promptFile}

	if err := customAgentInstructionsError(cfg, `{"prompt":"file://`+filepath.ToSlash(promptFile)+`"}`); err != nil {
		t.Fatalf("referenced standing prompt rejected: %v", err)
	}
	if err := customAgentInstructionsError(cfg, `{"name":"ao"}`); err == nil {
		t.Fatal("unreferenced standing prompt passed custom-agent validation")
	}
	if err := customAgentInstructionsError(ports.LaunchConfig{}, `{"name":"ao"}`); err == nil {
		t.Fatal("missing standing prompt passed custom-agent validation")
	}
}

func TestRunAdapterSandboxesCancellationProbe(t *testing.T) {
	for _, key := range []string{"HOME", "PATH", "AUTOHAND_CONFIG", "KIMI_CODE_HOME"} {
		t.Setenv(key, "outside-conformance-sandbox")
	}

	agent := &cancellationProbeAgent{}
	runAdapter(t, "probe", adapters.Manifest{
		ID:           "probe",
		Name:         "Probe",
		Description:  "Conformance cancellation probe",
		Version:      "0.0.1",
		Capabilities: []adapters.Capability{adapters.CapabilityAgent},
	}, agent, RegistryOptions{
		BinaryNames: map[domain.AgentHarness][]string{"probe": {"probe"}},
	})

	if !agent.sawCanceledHook {
		t.Fatal("GetAgentHooks did not receive the cancellation probe")
	}
	for key, value := range agent.canceledEnv {
		if value == "" || value == "outside-conformance-sandbox" {
			t.Errorf("%s was not sandboxed before cancellation probe: %q", key, value)
		}
	}
	if agent.canceledHookCfg.DataDir == "" || agent.canceledHookCfg.WorkspacePath == "" {
		t.Fatalf("canceled hook config has empty sandbox paths: %#v", agent.canceledHookCfg)
	}
	if got := agent.canceledHookCfg.Env["KIMI_CODE_HOME"]; got == "" || !strings.HasPrefix(got, agent.canceledHookCfg.DataDir) {
		t.Errorf("canceled hook KIMI_CODE_HOME = %q, want path under %q", got, agent.canceledHookCfg.DataDir)
	}
}

func TestIsolateUserEnvironmentSandboxesKimiCodeHome(t *testing.T) {
	t.Setenv("KIMI_CODE_HOME", filepath.Join("outside", "kimi"))
	root := t.TempDir()
	isolateUserEnvironment(t, root)
	if got, want := os.Getenv("KIMI_CODE_HOME"), filepath.Join(root, "kimi-code-home"); got != want {
		t.Fatalf("KIMI_CODE_HOME = %q, want %q", got, want)
	}
}

type cancellationProbeAgent struct {
	agentbase.Base
	sawCanceledHook bool
	canceledHookCfg ports.WorkspaceHookConfig
	canceledEnv     map[string]string
}

func (a *cancellationProbeAgent) GetLaunchCommand(ctx context.Context, cfg ports.LaunchConfig) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	cmd := []string{"probe"}
	if cfg.Prompt != "" {
		cmd = append(cmd, cfg.Prompt)
	}
	return cmd, nil
}

func (a *cancellationProbeAgent) GetAgentHooks(ctx context.Context, cfg ports.WorkspaceHookConfig) error {
	if err := ctx.Err(); err != nil {
		a.sawCanceledHook = true
		a.canceledHookCfg = cfg
		a.canceledEnv = map[string]string{}
		for _, key := range []string{"HOME", "PATH", "AUTOHAND_CONFIG", "KIMI_CODE_HOME"} {
			a.canceledEnv[key] = os.Getenv(key)
		}
		return err
	}
	return nil
}
