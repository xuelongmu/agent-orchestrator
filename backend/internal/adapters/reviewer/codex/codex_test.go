package codex

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"slices"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

type captureAgent struct {
	got  ports.LaunchConfig
	argv []string
}

func (a *captureAgent) GetConfigSpec(context.Context) (ports.ConfigSpec, error) {
	return ports.ConfigSpec{}, nil
}
func (a *captureAgent) GetLaunchCommand(_ context.Context, cfg ports.LaunchConfig) ([]string, error) {
	a.got = cfg
	if a.argv != nil {
		return append([]string(nil), a.argv...), nil
	}
	return []string{"agent", "--", cfg.Prompt}, nil
}
func (a *captureAgent) GetPromptDeliveryStrategy(context.Context, ports.LaunchConfig) (ports.PromptDeliveryStrategy, error) {
	return ports.PromptDeliveryInCommand, nil
}
func (a *captureAgent) GetAgentHooks(context.Context, ports.WorkspaceHookConfig) error { return nil }
func (a *captureAgent) GetRestoreCommand(context.Context, ports.RestoreConfig) ([]string, bool, error) {
	return nil, false, nil
}
func (a *captureAgent) SessionInfo(context.Context, ports.SessionRef) (ports.SessionInfo, bool, error) {
	return ports.SessionInfo{}, false, nil
}

func TestReviewCommandUsesReadOnlySandbox(t *testing.T) {
	t.Setenv("AO_PORT", "3103")
	t.Setenv("AO_DATA_DIR", "/tmp/ao data")
	t.Setenv("AO_RUN_FILE", "/tmp/ao data/running.json")
	agent := &captureAgent{}
	r := &Reviewer{agent: agent}

	got, err := r.ReviewCommand(context.Background(), ports.ReviewInvocation{
		ReviewerID:    "review-w1",
		WorkspacePath: "/ws/w1",
		Prompt:        "review it",
		SystemPrompt:  "review only",
	})
	if err != nil {
		t.Fatalf("ReviewCommand: %v", err)
	}

	want := []string{
		"agent",
		"--sandbox", "read-only",
		"-c", `shell_environment_policy.set.AO_PORT="3103"`,
		"-c", `shell_environment_policy.set.AO_DATA_DIR="/tmp/ao data"`,
		"-c", `shell_environment_policy.set.AO_RUN_FILE="/tmp/ao data/running.json"`,
		"--", "review it",
	}
	if !slices.Equal(got.Argv, want) {
		t.Fatalf("argv = %#v, want %#v", got.Argv, want)
	}
	if agent.got.Permissions != ports.PermissionModeAuto {
		t.Fatalf("permissions = %q, want auto", agent.got.Permissions)
	}
	if agent.got.SystemPrompt != "review only" {
		t.Fatalf("system prompt = %q", agent.got.SystemPrompt)
	}
}

func TestReviewMessageReturnsTaskPrompt(t *testing.T) {
	got, err := (&Reviewer{}).ReviewMessage(context.Background(), ports.ReviewInvocation{Prompt: "next review"})
	if err != nil {
		t.Fatalf("ReviewMessage: %v", err)
	}
	if got != "next review" {
		t.Fatalf("message = %q", got)
	}
}

const reviewerArgvHelperEnv = "AO_TEST_CODEX_REVIEWER_ARGV_HELPER"

func TestMain(m *testing.M) {
	if os.Getenv(reviewerArgvHelperEnv) != "" {
		_ = json.NewEncoder(os.Stdout).Encode(os.Args[1:])
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestReviewCommandExecutesPromptAsOneArgvElement(t *testing.T) {
	for _, name := range []string{"AO_PORT", "AO_DATA_DIR", "AO_RUN_FILE"} {
		t.Setenv(name, "")
	}
	prompts := map[string]string{
		"whitespace":     "review these changes carefully",
		"quotes":         `review "quoted" and 'single-quoted' text`,
		"metacharacters": `review & echo bad | rm -rf nope; $(touch nope) > output`,
		"unicode":        "review café, 東京, and 🚀",
		"multiline":      "review the first line\nand the second line\r\nand the fourth",
	}

	for name, prompt := range prompts {
		t.Run(name, func(t *testing.T) {
			helper, err := os.Executable()
			if err != nil {
				t.Fatalf("resolve test executable: %v", err)
			}
			agent := &captureAgent{argv: []string{helper, "exec", "--", prompt}}
			r := &Reviewer{agent: agent}
			spec, err := r.ReviewCommand(context.Background(), ports.ReviewInvocation{Prompt: prompt})
			if err != nil {
				t.Fatalf("ReviewCommand: %v", err)
			}

			cmd := exec.Command(spec.Argv[0], spec.Argv[1:]...) // #nosec G204 -- test helper path and fixed argv
			cmd.Env = append(os.Environ(), reviewerArgvHelperEnv+"=1")
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("execute reviewer argv: %v: %s", err, out)
			}
			var got []string
			if err := json.Unmarshal(out, &got); err != nil {
				t.Fatalf("decode helper argv %q: %v", out, err)
			}
			if !slices.Equal(got, spec.Argv[1:]) {
				t.Fatalf("executed argv = %#v, want %#v", got, spec.Argv[1:])
			}
			if got[len(got)-1] != prompt {
				t.Fatalf("executed prompt = %q, want one argv element %q", got[len(got)-1], prompt)
			}
		})
	}
}
