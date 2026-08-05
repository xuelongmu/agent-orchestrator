// Package launchagenthandoff serves one private environment to a macOS
// LaunchAgent over a protected local socket while Electron starts it.
package launchagenthandoff

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"
)

// LockTimeout bounds the wait for a concurrent handoff to release the lock. It
// matches the caller's own readiness timeout, so a contended launch surfaces the
// lock wait rather than a generic startup stall.
const LockTimeout = 30 * time.Second

type request struct {
	Environment *map[string]string `json:"environment,omitempty"`
	SocketPath  string             `json:"socket_path,omitempty"`
}

// Run holds the handoff lock for the call's duration, then reads one JSON
// request, reports socket readiness, serves the environment once, and removes
// the socket when Electron sends a release line or its pipe closes. The helper
// is a separate process so Electron crashes still clean up.
//
// The lock is taken before the request is read so a second launch waits its turn
// instead of racing to bind the same socket path.
func Run(ctx context.Context, in io.Reader, out io.Writer, lockPath string) error {
	releaseLock, err := acquireLock(ctx, lockPath, LockTimeout)
	if err != nil {
		return err
	}
	defer releaseLock()
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
