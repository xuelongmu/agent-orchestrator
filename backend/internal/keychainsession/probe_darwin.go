//go:build darwin

package keychainsession

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

var (
	lookPath   = exec.LookPath
	runCommand = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		return exec.CommandContext(ctx, name, args...).CombinedOutput()
	}
)

func securityFailureDetail(ctx context.Context, err error) string {
	detail := "security show-keychain-info failed"
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		detail = fmt.Sprintf("%s (exit %d)", detail, exitErr.ExitCode())
	}
	if ctx.Err() != nil {
		detail = fmt.Sprintf("%s: %v", detail, ctx.Err())
	}
	return detail
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

// Probe checks both the daemon and an existing tmux server. A tmux server
// survives daemon replacement and owns the audit session inherited by current
// and future worker panes, so daemon-only success is not sufficient.
func Probe(ctx context.Context) Result {
	home, err := os.UserHomeDir()
	if err != nil {
		return Result{Supported: true, Detail: fmt.Sprintf("resolve home directory: %v", err)}
	}
	keychain := filepath.Join(home, "Library", "Keychains", "login.keychain-db")
	if output, err := runCommand(ctx, "/usr/bin/security", "show-keychain-info", keychain); err != nil {
		_ = output // security output can contain local paths; the exit is sufficient.
		return Result{Supported: true, Detail: "daemon " + securityFailureDetail(ctx, err)}
	}

	tmuxPath, err := lookPath("tmux")
	if err != nil {
		return Result{
			Supported: true,
			Available: true,
			Detail:    "daemon login-keychain interaction succeeded; no tmux executable is active",
		}
	}
	output, err := runCommand(ctx, tmuxPath, "list-sessions")
	if err != nil {
		message := strings.ToLower(string(output))
		if strings.Contains(message, "no server running") {
			return Result{
				Supported: true,
				Available: true,
				Detail:    "daemon login-keychain interaction succeeded; no active tmux server",
			}
		}
		if !strings.Contains(message, "no sessions") {
			return Result{Supported: true, Detail: "could not determine the active tmux server audit session"}
		}
	}

	command := "/usr/bin/security show-keychain-info " + shellQuote(keychain)
	if output, err := runCommand(ctx, tmuxPath, "run-shell", command); err != nil {
		_ = output
		return Result{
			Supported: true,
			Detail:    "active tmux server " + securityFailureDetail(ctx, err),
		}
	}
	return Result{
		Supported: true,
		Available: true,
		Detail:    "daemon and active tmux server login-keychain interaction succeeded",
	}
}
