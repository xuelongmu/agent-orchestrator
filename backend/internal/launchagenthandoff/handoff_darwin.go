//go:build darwin

package launchagenthandoff

import (
	"context"
	"fmt"
	"net"
	"os"
	"sort"
)

func prepareEnvironmentSocket(
	socketPath string,
	environment map[string]string,
) (<-chan error, func(), error) {
	if socketPath == "" {
		return nil, nil, fmt.Errorf("environment socket path is required")
	}
	if info, err := os.Lstat(socketPath); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return nil, nil, fmt.Errorf("environment socket path already exists and is not a socket")
		}
		if err := os.Remove(socketPath); err != nil {
			return nil, nil, fmt.Errorf("remove stale environment socket: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return nil, nil, fmt.Errorf("inspect environment socket: %w", err)
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socketPath, Net: "unix"})
	if err != nil {
		return nil, nil, fmt.Errorf("listen on environment socket: %w", err)
	}
	if err := os.Chmod(socketPath, 0o600); err != nil {
		_ = listener.Close()
		_ = os.Remove(socketPath)
		return nil, nil, fmt.Errorf("protect environment socket: %w", err)
	}

	delivered := make(chan error, 1)
	go func() {
		conn, err := listener.AcceptUnix()
		if err != nil {
			delivered <- err
			return
		}
		defer func() { _ = conn.Close() }()
		keys := make([]string, 0, len(environment))
		for key := range environment {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			if _, err := conn.Write([]byte(key + "=" + environment[key] + "\x00")); err != nil {
				delivered <- err
				return
			}
		}
		_, err = conn.Write([]byte("AO_HANDOFF_COMPLETE=1\x00"))
		delivered <- err
	}()
	cleanup := func() {
		_ = listener.Close()
		_ = os.Remove(socketPath)
	}
	return delivered, cleanup, nil
}

func waitForEnvironmentDelivery(
	ctx context.Context,
	delivered <-chan error,
	released <-chan struct{},
) error {
	select {
	case err := <-delivered:
		if err != nil {
			return fmt.Errorf("deliver launch environment: %w", err)
		}
		return nil
	case <-released:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
