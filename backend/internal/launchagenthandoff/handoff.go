// Package launchagenthandoff serves one private environment to a macOS
// LaunchAgent over a protected local socket while Electron starts it.
package launchagenthandoff

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
)

type request struct {
	Environment *map[string]string `json:"environment,omitempty"`
	SocketPath  string             `json:"socket_path,omitempty"`
}

// Run reads one JSON request, reports socket readiness, serves the environment
// once, then removes the socket when Electron sends a release line or its pipe
// closes. The helper is a separate process so Electron crashes still clean up.
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
	released := make(chan struct{})
	go func() {
		_, _ = reader.ReadBytes('\n')
		close(released)
	}()
	var delivered <-chan error
	cleanup := func() {}
	if req.Environment != nil {
		delivered, cleanup, err = prepareEnvironmentSocket(req.SocketPath, *req.Environment)
		if err != nil {
			return err
		}
	}
	defer cleanup()
	if _, err := io.WriteString(out, "ready\n"); err != nil {
		return fmt.Errorf("report handoff readiness: %w", err)
	}
	if delivered != nil {
		if err := waitForEnvironmentDelivery(ctx, delivered, released); err != nil {
			return err
		}
		select {
		case <-released:
			return nil
		default:
		}
		if _, err := io.WriteString(out, "delivered\n"); err != nil {
			return fmt.Errorf("report environment delivery: %w", err)
		}
	}
	select {
	case <-ctx.Done():
	case <-released:
	}
	return nil
}
