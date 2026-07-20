//go:build darwin

package tmux

import (
	"context"
	"os"
	"testing"

	"golang.org/x/sys/unix"
)

func TestDarwinProcessIdentityUsesNumericGetsidAndStartIdentity(t *testing.T) {
	ctx := context.Background()
	pid := os.Getpid()
	table := osProcessTable{runner: execRunner{}, timeout: defaultTimeout}
	identity, err := table.Identity(ctx, pid)
	if err != nil {
		t.Fatalf("Identity(%d): %v", pid, err)
	}
	wantSID, err := unix.Getsid(pid)
	if err != nil {
		t.Fatalf("unix.Getsid(%d): %v", pid, err)
	}
	if identity.pid != pid || identity.sessionID != wantSID || identity.started == "" {
		t.Fatalf("identity = %#v, want pid=%d sid=%d and a kernel start token", identity, pid, wantSID)
	}

	snapshot, err := table.Snapshot(ctx, map[int]struct{}{wantSID: {}})
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	for _, process := range snapshot {
		if process == identity {
			return
		}
	}
	t.Fatalf("current identity %#v missing from numeric-SID snapshot", identity)
}
