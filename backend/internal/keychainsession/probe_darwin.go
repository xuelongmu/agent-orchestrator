//go:build darwin

package keychainsession

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// Probe runs inside the daemon process tree, so securityd evaluates the exact
// audit session inherited by daemon-spawned tmux servers and worker panes.
func Probe(ctx context.Context) Result {
	home, err := os.UserHomeDir()
	if err != nil {
		return Result{Supported: true, Detail: fmt.Sprintf("resolve home directory: %v", err)}
	}
	keychain := filepath.Join(home, "Library", "Keychains", "login.keychain-db")
	if output, err := exec.CommandContext(ctx, "/usr/bin/security", "show-keychain-info", keychain).CombinedOutput(); err != nil {
		detail := "security show-keychain-info failed"
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			detail = fmt.Sprintf("%s (exit %d)", detail, exitErr.ExitCode())
		}
		if ctx.Err() != nil {
			detail = fmt.Sprintf("%s: %v", detail, ctx.Err())
		}
		_ = output // security output can contain local paths; the exit is sufficient.
		return Result{Supported: true, Detail: detail}
	}
	return Result{Supported: true, Available: true, Detail: "login-keychain interaction succeeded"}
}
