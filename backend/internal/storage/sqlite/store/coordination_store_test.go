package store_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
)

func TestCoordinationClaimHasExactlyOneConcurrentWinner(t *testing.T) {
	dir := t.TempDir()
	first, err := sqlite.Open(dir)
	if err != nil {
		t.Fatalf("open first store: %v", err)
	}
	t.Cleanup(func() { _ = first.Close() })
	second, err := sqlite.Open(dir)
	if err != nil {
		t.Fatalf("open second store: %v", err)
	}
	t.Cleanup(func() { _ = second.Close() })

	start := make(chan struct{})
	results := make(chan bool, 2)
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for _, candidate := range []struct {
		store *sqlite.Store
		pid   int
	}{{first, 101}, {second, 202}} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, acquired, err := candidate.store.TryAcquireCoordinationClaim(context.Background(), "daemon", candidate.pid, time.Now().UTC())
			results <- acquired
			errs <- err
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("claim error: %v", err)
		}
	}
	winners := 0
	for acquired := range results {
		if acquired {
			winners++
		}
	}
	if winners != 1 {
		t.Fatalf("claim winners = %d, want exactly 1", winners)
	}
}

func TestCoordinationClaimSurvivesReopenAndReleaseChecksOwner(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	claimedAt := time.Now().UTC().Truncate(time.Second)

	first, err := sqlite.Open(dir)
	if err != nil {
		t.Fatalf("open first store: %v", err)
	}
	if _, acquired, err := first.TryAcquireCoordinationClaim(ctx, "daemon", 101, claimedAt); err != nil || !acquired {
		t.Fatalf("initial claim acquired=%v err=%v", acquired, err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("close first store: %v", err)
	}

	second, err := sqlite.Open(dir)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	t.Cleanup(func() { _ = second.Close() })
	claim, acquired, err := second.TryAcquireCoordinationClaim(ctx, "daemon", 202, claimedAt.Add(time.Second))
	if err != nil {
		t.Fatalf("competing claim: %v", err)
	}
	if acquired || claim.OwnerPID != 101 || !claim.ClaimedAt.Equal(claimedAt) {
		t.Fatalf("reopened claim = %+v acquired=%v, want durable owner 101", claim, acquired)
	}
	if released, err := second.ReleaseCoordinationClaim(ctx, "daemon", 202); err != nil || released {
		t.Fatalf("non-owner release=%v err=%v, want guarded no-op", released, err)
	}
	if released, err := second.ReleaseCoordinationClaim(ctx, "daemon", 101); err != nil || !released {
		t.Fatalf("owner release=%v err=%v, want success", released, err)
	}
}
