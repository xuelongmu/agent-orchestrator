//go:build darwin

package launchagenthandoff

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"syscall"
	"time"
)

// How often a blocked acquisition retries. flock(2) has no timed variant, so the
// wait is a poll; the interval is short enough that a normal handoff waiting on
// a departing one is not visibly delayed.
const lockRetryInterval = 50 * time.Millisecond

// acquireLock takes the exclusive handoff lock, waiting up to timeout for a
// concurrent handoff to finish. The returned release closes the descriptor,
// which is what drops the lock; the kernel also drops it if the process dies,
// so a crashed helper cannot wedge later launches.
//
// This is deliberately in-process. It previously ran the helper under
// `/usr/bin/lockf`, which does not exist on macOS — that is a FreeBSD utility —
// so the handoff failed with ENOENT before it ever started and the daemon never
// launched. macOS ships no lockf(1) or flock(1), so there is no drop-in
// replacement to spawn.
func acquireLock(ctx context.Context, path string, timeout time.Duration) (func(), error) {
	if path == "" {
		return nil, errors.New("handoff lock path is required")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, fmt.Errorf("create handoff lock directory: %w", err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open handoff lock: %w", err)
	}
	deadline := time.Now().Add(timeout)
	for {
		err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() { _ = file.Close() }, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			_ = file.Close()
			return nil, fmt.Errorf("lock handoff: %w", err)
		}
		if !time.Now().Before(deadline) {
			_ = file.Close()
			return nil, fmt.Errorf("timed out after %s waiting for the handoff lock at %s", timeout, path)
		}
		timer := time.NewTimer(lockRetryInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			_ = file.Close()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

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
