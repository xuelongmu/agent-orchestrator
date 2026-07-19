package daemon

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
)

func TestAcquireDaemonCoordinationRefusesLiveOwner(t *testing.T) {
	store, err := sqlite.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if _, acquired, err := store.TryAcquireCoordinationClaim(context.Background(), daemonCoordinationClaim, os.Getpid(), time.Now().UTC()); err != nil || !acquired {
		t.Fatalf("seed live owner acquired=%v err=%v", acquired, err)
	}

	_, err = acquireDaemonCoordination(context.Background(), store, os.Getpid()+1, 0)
	if err == nil || !strings.Contains(err.Error(), "already claimed") {
		t.Fatalf("acquire error = %v, want live-owner refusal", err)
	}
}

func TestAcquireDaemonCoordinationTakesOverVerifiedStaleOwner(t *testing.T) {
	store, err := sqlite.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	const stalePID = 424242
	if _, acquired, err := store.TryAcquireCoordinationClaim(context.Background(), daemonCoordinationClaim, stalePID, time.Now().UTC()); err != nil || !acquired {
		t.Fatalf("seed stale owner acquired=%v err=%v", acquired, err)
	}

	release, err := acquireDaemonCoordination(context.Background(), store, os.Getpid(), stalePID)
	if err != nil {
		t.Fatalf("take over stale owner: %v", err)
	}
	if err := release(context.Background()); err != nil {
		t.Fatalf("release takeover: %v", err)
	}
	if _, acquired, err := store.TryAcquireCoordinationClaim(context.Background(), daemonCoordinationClaim, os.Getpid()+1, time.Now().UTC()); err != nil || !acquired {
		t.Fatalf("claim after release acquired=%v err=%v", acquired, err)
	}
}
