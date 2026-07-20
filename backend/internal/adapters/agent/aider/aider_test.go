package aider

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters"
	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/agent/gitexclude"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func TestManifest(t *testing.T) {
	m := (&Plugin{}).Manifest()
	if m.ID != "aider" {
		t.Fatalf("ID = %q, want aider", m.ID)
	}
	if m.Name != "Aider" {
		t.Fatalf("Name = %q, want Aider", m.Name)
	}
	hasAgent := false
	for _, c := range m.Capabilities {
		if c == adapters.CapabilityAgent {
			hasAgent = true
		}
	}
	if !hasAgent {
		t.Fatal("missing CapabilityAgent")
	}
}

func TestGetConfigSpecEmpty(t *testing.T) {
	spec, err := (&Plugin{}).GetConfigSpec(context.Background())
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(spec.Fields) != 0 {
		t.Fatalf("expected no fields, got %d", len(spec.Fields))
	}
}

func TestGetPromptDeliveryStrategy(t *testing.T) {
	s, err := (&Plugin{}).GetPromptDeliveryStrategy(context.Background(), ports.LaunchConfig{})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if s != ports.PromptDeliveryAfterStart {
		t.Fatalf("strategy = %q, want %q", s, ports.PromptDeliveryAfterStart)
	}
}

func TestGetLaunchCommandOmitsPromptForInteractiveDelivery(t *testing.T) {
	p := &Plugin{resolvedBinary: "aider"}
	cmd, err := p.GetLaunchCommand(context.Background(), ports.LaunchConfig{
		Prompt: "add a health check",
	})
	if err != nil {
		t.Fatal(err)
	}

	want := []string{"aider", "--no-check-update", "--no-stream", "--no-pretty", "--no-gitignore"}
	if !reflect.DeepEqual(cmd, want) {
		t.Fatalf("unexpected command\nwant: %#v\n got: %#v", want, cmd)
	}
	for _, arg := range cmd {
		if arg == "-m" || arg == "add a health check" {
			t.Fatalf("cmd = %#v unexpectedly contains prompt argv", cmd)
		}
	}
}

func TestGetLaunchCommandOmitsPromptFlagWhenEmpty(t *testing.T) {
	p := &Plugin{resolvedBinary: "aider"}
	cmd, err := p.GetLaunchCommand(context.Background(), ports.LaunchConfig{})
	if err != nil {
		t.Fatal(err)
	}

	want := []string{"aider", "--no-check-update", "--no-stream", "--no-pretty", "--no-gitignore"}
	if !reflect.DeepEqual(cmd, want) {
		t.Fatalf("cmd = %#v, want %#v", cmd, want)
	}
	for _, arg := range cmd {
		if arg == "-m" {
			t.Fatalf("cmd = %#v unexpectedly contains -m for empty prompt", cmd)
		}
	}
}

func TestGetLaunchCommandAlwaysAppendsStableFlags(t *testing.T) {
	p := &Plugin{resolvedBinary: "aider"}
	cmd, err := p.GetLaunchCommand(context.Background(), ports.LaunchConfig{Prompt: "do the thing"})
	if err != nil {
		t.Fatal(err)
	}

	for _, want := range []string{"--no-check-update", "--no-stream", "--no-pretty", "--no-gitignore"} {
		found := false
		for _, arg := range cmd {
			if arg == want {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("cmd = %#v missing stable launch flag %q", cmd, want)
		}
	}
}

func TestGetLaunchCommandMapsPermissionModes(t *testing.T) {
	tests := []struct {
		name       string
		mode       ports.PermissionMode
		wantFlags  []string
		wantAbsent []string
	}{
		{
			name:       "default omits approval flags",
			mode:       ports.PermissionModeDefault,
			wantFlags:  nil,
			wantAbsent: []string{"--yes-always", "--no-auto-commits"},
		},
		{
			name:       "empty omits approval flags",
			mode:       "",
			wantFlags:  nil,
			wantAbsent: []string{"--yes-always", "--no-auto-commits"},
		},
		{
			name:       "accept edits applies but leaves uncommitted",
			mode:       ports.PermissionModeAcceptEdits,
			wantFlags:  []string{"--yes-always", "--no-auto-commits"},
			wantAbsent: nil,
		},
		{
			name:       "auto applies and auto-commits",
			mode:       ports.PermissionModeAuto,
			wantFlags:  []string{"--yes-always"},
			wantAbsent: []string{"--no-auto-commits"},
		},
		{
			name:       "bypass collapses onto auto",
			mode:       ports.PermissionModeBypassPermissions,
			wantFlags:  []string{"--yes-always"},
			wantAbsent: []string{"--no-auto-commits"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &Plugin{resolvedBinary: "aider"}
			cmd, err := p.GetLaunchCommand(context.Background(), ports.LaunchConfig{
				Prompt:      "do the thing",
				Permissions: tt.mode,
			})
			if err != nil {
				t.Fatal(err)
			}

			for _, want := range tt.wantFlags {
				found := false
				for _, arg := range cmd {
					if arg == want {
						found = true
						break
					}
				}
				if !found {
					t.Fatalf("cmd = %#v missing expected flag %q", cmd, want)
				}
			}
			for _, want := range []string{"--no-check-update", "--no-stream", "--no-pretty", "--no-gitignore"} {
				if !containsArg(cmd, want) {
					t.Fatalf("cmd = %#v missing stable launch flag %q", cmd, want)
				}
			}
			for _, absent := range tt.wantAbsent {
				for _, arg := range cmd {
					if arg == absent {
						t.Fatalf("cmd = %#v unexpectedly contains %q", cmd, absent)
					}
				}
			}
		})
	}
}

func TestPreLaunchLocallyIgnoresAllAiderArtifacts(t *testing.T) {
	repo := t.TempDir()
	runGit(t, repo, "init", "--quiet")
	workspace := filepath.Join(repo, "nested", "workspace")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}

	excludePath := strings.TrimSpace(runGit(t, workspace, "rev-parse", "--git-path", "info/exclude"))
	if !filepath.IsAbs(excludePath) {
		excludePath = filepath.Join(workspace, excludePath)
	}
	if err := os.WriteFile(excludePath, []byte("user-local-artifact\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	p := &Plugin{resolvedBinary: "aider"}
	cfg := ports.LaunchConfig{WorkspacePath: workspace}
	if err := p.PreLaunch(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if err := p.PreLaunch(context.Background(), cfg); err != nil {
		t.Fatalf("second PreLaunch: %v", err)
	}

	data, err := os.ReadFile(excludePath)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(data); !strings.Contains(got, "user-local-artifact\n") {
		t.Fatalf("local exclude lost existing content: %q", got)
	} else if count := strings.Count(got, aiderArtifactIgnorePattern); count != 1 {
		t.Fatalf("local exclude pattern count = %d, want 1: %q", count, got)
	}
	if _, err := os.Stat(filepath.Join(workspace, ".gitignore")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("tracked .gitignore was created or inaccessible: %v", err)
	}

	artifacts := []string{
		".aider.input.history",
		".aider.chat.history.md",
		filepath.Join(".aider.tags.cache.v4", "cache.db"),
	}
	for _, artifact := range artifacts {
		path := filepath.Join(workspace, artifact)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("private session state"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if status := runGit(t, workspace, "status", "--porcelain", "--untracked-files=all"); status != "" {
		t.Fatalf("Aider artifacts dirtied worktree: %q", status)
	}
}

func TestPreLaunchNonGitWorkspaceIsNoOp(t *testing.T) {
	workspace := t.TempDir()
	if err := (&Plugin{}).PreLaunch(context.Background(), ports.LaunchConfig{WorkspacePath: workspace}); err != nil {
		t.Fatalf("PreLaunch in scratch workspace: %v", err)
	}
	entries, err := os.ReadDir(workspace)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("PreLaunch mutated scratch workspace: %#v", entries)
	}
}

func TestPreLaunchMalformedGitMetadataFails(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, ".git"), []byte("gitdir: missing-git-dir\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := (&Plugin{}).PreLaunch(context.Background(), ports.LaunchConfig{WorkspacePath: workspace}); err == nil {
		t.Fatal("PreLaunch with malformed Git metadata succeeded, want error")
	}
}

func TestPreLaunchLinkedWorktreeUsesGitResolvedExclude(t *testing.T) {
	repo := t.TempDir()
	runGit(t, repo, "init", "--quiet")
	runGit(t, repo, "config", "user.name", "AO Test")
	runGit(t, repo, "config", "user.email", "ao-test@example.com")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "add", "README.md")
	runGit(t, repo, "commit", "--quiet", "-m", "test fixture")

	linked := filepath.Join(t.TempDir(), "linked")
	runGit(t, repo, "worktree", "add", "--quiet", "-b", "aider-linked-test", linked)
	if err := (&Plugin{}).PreLaunch(context.Background(), ports.LaunchConfig{WorkspacePath: linked}); err != nil {
		t.Fatal(err)
	}

	excludePath := runGit(t, linked, "rev-parse", "--git-path", "info/exclude")
	data, err := os.ReadFile(excludePath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), aiderArtifactIgnorePattern) {
		t.Fatalf("linked-worktree exclude missing %q: %q", aiderArtifactIgnorePattern, data)
	}
	if err := os.WriteFile(filepath.Join(linked, ".aider.chat.history.md"), []byte("private"), 0o600); err != nil {
		t.Fatal(err)
	}
	if status := runGit(t, linked, "status", "--porcelain", "--untracked-files=all"); status != "" {
		t.Fatalf("Aider artifact dirtied linked worktree: %q", status)
	}
}

func TestPreLaunchSerializesLinkedWorktreeAndOtherAdapterExcludeUpdates(t *testing.T) {
	repo := t.TempDir()
	runGit(t, repo, "init", "--quiet")
	runGit(t, repo, "config", "user.name", "AO Test")
	runGit(t, repo, "config", "user.email", "ao-test@example.com")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "add", "README.md")
	runGit(t, repo, "commit", "--quiet", "-m", "test fixture")

	linkedRoot := t.TempDir()
	linkedAider := filepath.Join(linkedRoot, "aider")
	linkedOther := filepath.Join(linkedRoot, "other")
	runGit(t, repo, "worktree", "add", "--quiet", "-b", "aider-lock-test", linkedAider)
	runGit(t, repo, "worktree", "add", "--quiet", "-b", "other-lock-test", linkedOther)
	excludePath := runGit(t, linkedAider, "rev-parse", "--git-path", "info/exclude")
	otherExclude := runGit(t, linkedOther, "rev-parse", "--git-path", "info/exclude")
	if otherExclude != excludePath {
		t.Fatalf("linked worktrees resolved different excludes: %q != %q", otherExclude, excludePath)
	}
	if err := os.WriteFile(excludePath, []byte("# existing\n/user-entry\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	unlock, err := gitexclude.Acquire(excludePath+".ao.lock", nil)
	if err != nil {
		t.Fatal(err)
	}
	locked := true
	defer func() {
		if locked {
			unlock()
		}
	}()

	blocked := make(chan struct{}, 2)
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		errs <- (&Plugin{}).preLaunch(context.Background(), ports.LaunchConfig{WorkspacePath: linkedAider}, func() {
			blocked <- struct{}{}
		})
	}()
	go func() {
		defer wg.Done()
		errs <- gitexclude.EnsurePattern(otherExclude, "/.github/agents/ao-test.agent.md", "# agent-orchestrator Copilot session files", func() {
			blocked <- struct{}{}
		})
	}()
	for range 2 {
		<-blocked
	}
	if data, err := os.ReadFile(excludePath); err != nil {
		t.Fatal(err)
	} else if string(data) != "# existing\n/user-entry\n" {
		t.Fatalf("exclude changed while common lock was held:\n%s", data)
	}

	unlock()
	locked = false
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}

	data, err := os.ReadFile(excludePath)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"/user-entry", aiderArtifactIgnorePattern, "/.github/agents/ao-test.agent.md"} {
		if count := strings.Count(string(data), want+"\n"); count != 1 {
			t.Fatalf("exclude entry %q count = %d, want 1:\n%s", want, count, data)
		}
	}
}

func containsArg(args []string, want string) bool {
	for _, arg := range args {
		if arg == want {
			return true
		}
	}
	return false
}

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmdArgs := append([]string{"-C", dir}, args...)
	out, err := exec.Command("git", cmdArgs...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestGetLaunchCommandSystemPromptFileUsesReadOnlyContext(t *testing.T) {
	p := &Plugin{resolvedBinary: "aider"}
	cmd, err := p.GetLaunchCommand(context.Background(), ports.LaunchConfig{
		Prompt:           "do the thing",
		SystemPromptFile: "/tmp/system.md",
	})
	if err != nil {
		t.Fatal(err)
	}

	want := []string{"aider", "--no-check-update", "--no-stream", "--no-pretty", "--no-gitignore", "--read", "/tmp/system.md"}
	if !reflect.DeepEqual(cmd, want) {
		t.Fatalf("cmd = %#v, want %#v", cmd, want)
	}
}

func TestGetLaunchCommandInlineSystemPromptIsDropped(t *testing.T) {
	p := &Plugin{resolvedBinary: "aider"}
	cmd, err := p.GetLaunchCommand(context.Background(), ports.LaunchConfig{
		Prompt:       "do the thing",
		SystemPrompt: "inline ignored",
	})
	if err != nil {
		t.Fatal(err)
	}

	want := []string{"aider", "--no-check-update", "--no-stream", "--no-pretty", "--no-gitignore"}
	if !reflect.DeepEqual(cmd, want) {
		t.Fatalf("cmd = %#v, want %#v", cmd, want)
	}
	for _, arg := range cmd {
		if arg == "--read" {
			t.Fatalf("cmd = %#v unexpectedly contains --read for inline system prompt", cmd)
		}
		if arg == "inline ignored" {
			t.Fatalf("cmd = %#v unexpectedly contains inline system prompt text", cmd)
		}
	}
}

func TestGetRestoreCommandAlwaysFalse(t *testing.T) {
	p := &Plugin{}
	cmd, ok, err := p.GetRestoreCommand(context.Background(), ports.RestoreConfig{
		Session: ports.SessionRef{
			Metadata: map[string]string{ports.MetadataKeyAgentSessionID: "abc123"},
		},
		Permissions: ports.PermissionModeBypassPermissions,
	})
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatalf("ok=true, want false (aider has no resume-by-id)")
	}
	if cmd != nil {
		t.Fatalf("cmd = %#v, want nil", cmd)
	}
}

func TestGetAgentHooksNoOp(t *testing.T) {
	if err := (&Plugin{}).GetAgentHooks(context.Background(), ports.WorkspaceHookConfig{WorkspacePath: t.TempDir()}); err != nil {
		t.Fatalf("GetAgentHooks err = %v, want nil", err)
	}
}

func TestSessionInfoNoOp(t *testing.T) {
	info, ok, err := (&Plugin{}).SessionInfo(context.Background(), ports.SessionRef{
		Metadata: map[string]string{ports.MetadataKeyAgentSessionID: "abc123"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatalf("ok=true with info %#v, want no-op false", info)
	}
	if !reflect.DeepEqual(info, ports.SessionInfo{}) {
		t.Fatalf("info = %#v, want zero", info)
	}
}

func TestContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := (&Plugin{}).GetConfigSpec(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("GetConfigSpec err = %v, want context.Canceled", err)
	}
	if _, err := (&Plugin{}).GetLaunchCommand(ctx, ports.LaunchConfig{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("GetLaunchCommand err = %v, want context.Canceled", err)
	}
	if _, err := (&Plugin{}).GetPromptDeliveryStrategy(ctx, ports.LaunchConfig{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("GetPromptDeliveryStrategy err = %v, want context.Canceled", err)
	}
	if err := (&Plugin{}).PreLaunch(ctx, ports.LaunchConfig{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("PreLaunch err = %v, want context.Canceled", err)
	}
	if err := (&Plugin{}).GetAgentHooks(ctx, ports.WorkspaceHookConfig{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("GetAgentHooks err = %v, want context.Canceled", err)
	}
	if _, _, err := (&Plugin{}).GetRestoreCommand(ctx, ports.RestoreConfig{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("GetRestoreCommand err = %v, want context.Canceled", err)
	}
	if _, _, err := (&Plugin{}).SessionInfo(ctx, ports.SessionRef{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("SessionInfo err = %v, want context.Canceled", err)
	}
}

func TestResolveAiderBinaryFallback(t *testing.T) {
	// When the binary is not on PATH or any well-known location, the resolver
	// MUST surface ports.ErrAgentBinaryNotFound rather than a silent string
	// fallback that lets a missing CLI launch into an empty tmux pane.
	bin, err := ResolveAiderBinary(context.Background())
	if err != nil {
		if !errors.Is(err, ports.ErrAgentBinaryNotFound) {
			t.Fatalf("err = %v, want ports.ErrAgentBinaryNotFound", err)
		}
		return
	}
	if bin == "" {
		t.Fatal("ResolveAiderBinary returned empty string with no error")
	}
}
