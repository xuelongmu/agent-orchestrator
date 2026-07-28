// Package launchagenthandoff temporarily injects a private environment into
// the macOS GUI launchd domain while Electron starts one LaunchAgent.
package launchagenthandoff

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
)

type request struct {
	Environment map[string]string `json:"environment"`
}

// Run reads one JSON request, applies it, reports readiness, then restores the
// previous launchd environment when Electron sends a release line or its pipe
// closes. The helper is a separate process so Electron crashes still run the
// restoration defer.
func Run(ctx context.Context, in io.Reader, out io.Writer) error {
	reader := bufio.NewReader(in)
	line, err := reader.ReadBytes('\n')
	if err != nil {
		return fmt.Errorf("read handoff request: %w", err)
	}
	var req request
	if err := json.Unmarshal(line, &req); err != nil {
		return fmt.Errorf("decode handoff request: %w", err)
	}
	restore, err := install(ctx, req.Environment)
	if err != nil {
		return err
	}
	defer func() { _ = restore() }()
	if _, err := io.WriteString(out, "ready\n"); err != nil {
		return fmt.Errorf("report handoff readiness: %w", err)
	}

	released := make(chan struct{})
	go func() {
		_, _ = reader.ReadBytes('\n')
		close(released)
	}()
	select {
	case <-ctx.Done():
	case <-released:
	}
	if err := restore(); err != nil {
		return err
	}
	restore = func() error { return nil }
	return nil
}
