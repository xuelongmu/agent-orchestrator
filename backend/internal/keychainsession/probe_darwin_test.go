//go:build darwin

package keychainsession

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestProbeChecksActiveTmuxServer(t *testing.T) {
	originalLookPath := lookPath
	originalRunCommand := runCommand
	t.Cleanup(func() {
		lookPath = originalLookPath
		runCommand = originalRunCommand
	})
	lookPath = func(string) (string, error) { return "/opt/homebrew/bin/tmux", nil }

	var commands [][]string
	runCommand = func(_ context.Context, name string, args ...string) ([]byte, error) {
		commands = append(commands, append([]string{name}, args...))
		if len(args) > 0 && args[0] == "run-shell" {
			return nil, errors.New("exit status 36")
		}
		return nil, nil
	}

	result := Probe(context.Background())
	if result.Available || !strings.Contains(result.Detail, "active tmux server") {
		t.Fatalf("Probe() = %+v, want active tmux failure", result)
	}
	if len(commands) != 3 || commands[2][1] != "run-shell" ||
		!strings.Contains(commands[2][2], "/usr/bin/security show-keychain-info") {
		t.Fatalf("commands = %#v, want daemon, list-sessions, and tmux run-shell probes", commands)
	}
}

func TestProbePassesWithoutActiveTmuxServer(t *testing.T) {
	originalLookPath := lookPath
	originalRunCommand := runCommand
	t.Cleanup(func() {
		lookPath = originalLookPath
		runCommand = originalRunCommand
	})
	lookPath = func(string) (string, error) { return "/opt/homebrew/bin/tmux", nil }
	runCommand = func(_ context.Context, _ string, args ...string) ([]byte, error) {
		if len(args) > 0 && args[0] == "list-sessions" {
			return []byte("no server running on /private/tmp/tmux-501/default"), errors.New("exit status 1")
		}
		return nil, nil
	}

	result := Probe(context.Background())
	if !result.Available || !strings.Contains(result.Detail, "no active tmux server") {
		t.Fatalf("Probe() = %+v, want daemon-only success", result)
	}
}

func TestProbeChecksEmptyButActiveTmuxServer(t *testing.T) {
	originalLookPath := lookPath
	originalRunCommand := runCommand
	t.Cleanup(func() {
		lookPath = originalLookPath
		runCommand = originalRunCommand
	})
	lookPath = func(string) (string, error) { return "/opt/homebrew/bin/tmux", nil }
	runShellCalled := false
	runCommand = func(_ context.Context, _ string, args ...string) ([]byte, error) {
		switch {
		case len(args) > 0 && args[0] == "list-sessions":
			return []byte("no sessions"), errors.New("exit status 1")
		case len(args) > 0 && args[0] == "run-shell":
			runShellCalled = true
			return nil, errors.New("exit status 36")
		default:
			return nil, nil
		}
	}

	result := Probe(context.Background())
	if result.Available || !runShellCalled || !strings.Contains(result.Detail, "active tmux server") {
		t.Fatalf("Probe() = %+v, runShellCalled=%v; want empty server failure", result, runShellCalled)
	}
}
