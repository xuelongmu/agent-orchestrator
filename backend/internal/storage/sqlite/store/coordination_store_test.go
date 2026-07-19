package store_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	sqlitestore "github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

func testClaim(token string, pid int, now time.Time) sqlitestore.CoordinationClaim {
	return sqlitestore.CoordinationClaim{
		Key: "exclusive-db-writer", OwnerToken: token, OwnerPID: pid,
		ClaimedAt: now, LeaseExpiresAt: now.Add(10 * time.Second),
	}
}

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
	now := time.Now().UTC()
	var wg sync.WaitGroup
	for _, candidate := range []struct {
		store *sqlite.Store
		claim sqlitestore.CoordinationClaim
	}{{first, testClaim("first", 101, now)}, {second, testClaim("second", 202, now)}} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, acquired, err := candidate.store.TryAcquireCoordinationClaim(context.Background(), candidate.claim)
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

func TestCoordinationClaimSurvivesReopenAndReleaseChecksToken(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	claimedAt := time.Now().UTC().Truncate(time.Second)

	first, err := sqlite.Open(dir)
	if err != nil {
		t.Fatalf("open first store: %v", err)
	}
	if _, acquired, err := first.TryAcquireCoordinationClaim(ctx, testClaim("owner-a", 101, claimedAt)); err != nil || !acquired {
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
	claim, acquired, err := second.TryAcquireCoordinationClaim(ctx, testClaim("owner-b", 101, claimedAt.Add(time.Second)))
	if err != nil {
		t.Fatalf("competing claim: %v", err)
	}
	if acquired || claim.OwnerToken != "owner-a" || claim.OwnerPID != 101 || !claim.ClaimedAt.Equal(claimedAt) {
		t.Fatalf("reopened claim = %+v acquired=%v, want durable owner-a generation", claim, acquired)
	}
	if released, err := second.ReleaseCoordinationClaim(ctx, claim.Key, "owner-b"); err != nil || released {
		t.Fatalf("stale-token release=%v err=%v, want guarded no-op", released, err)
	}
	if released, err := second.ReleaseCoordinationClaim(ctx, claim.Key, "owner-a"); err != nil || !released {
		t.Fatalf("owner release=%v err=%v, want success", released, err)
	}
}

func TestCoordinationRenewalRequiresPersistedExpiryAdvance(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	original := testClaim("owner-a", 101, now)
	if _, acquired, err := store.TryAcquireCoordinationClaim(ctx, original); err != nil || !acquired {
		t.Fatalf("initial claim acquired=%v err=%v", acquired, err)
	}
	backwardNow := now.Add(-5 * time.Second)
	if renewed, err := store.RenewCoordinationClaim(ctx, original.Key, original.OwnerToken, backwardNow, backwardNow.Add(10*time.Second)); err != nil || renewed {
		t.Fatalf("backward renewal=%v err=%v, want fail-closed no-op", renewed, err)
	}
	current, acquired, err := store.TryAcquireCoordinationClaim(ctx, testClaim("owner-b", 202, now))
	if err != nil || acquired {
		t.Fatalf("read current via contender acquired=%v err=%v", acquired, err)
	}
	if !current.LeaseExpiresAt.Equal(original.LeaseExpiresAt) {
		t.Fatalf("expiry shortened to %s, want %s", current.LeaseExpiresAt, original.LeaseExpiresAt)
	}
	forwardNow := now.Add(time.Second)
	forwardExpiry := forwardNow.Add(10 * time.Second)
	if renewed, err := store.RenewCoordinationClaim(ctx, original.Key, original.OwnerToken, forwardNow, forwardExpiry); err != nil || !renewed {
		t.Fatalf("forward renewal=%v err=%v", renewed, err)
	}
	current, acquired, err = store.TryAcquireCoordinationClaim(ctx, testClaim("owner-b", 202, now))
	if err != nil || acquired || !current.LeaseExpiresAt.Equal(forwardExpiry) {
		t.Fatalf("renewed claim=%+v acquired=%v err=%v", current, acquired, err)
	}
}

func TestCoordinationAcquireAndReleaseRaceNeverObservesMissingClaim(t *testing.T) {
	dir := t.TempDir()
	owner, err := sqlite.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = owner.Close() })
	contender, err := sqlite.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = contender.Close() })

	ctx := context.Background()
	for i := range 25 {
		now := time.Now().UTC()
		ownerClaim := testClaim("owner-"+string(rune('a'+i)), 101, now)
		if _, acquired, err := owner.TryAcquireCoordinationClaim(ctx, ownerClaim); err != nil || !acquired {
			t.Fatalf("iteration %d seed acquired=%v err=%v", i, acquired, err)
		}
		start := make(chan struct{})
		errCh := make(chan error, 2)
		go func() {
			<-start
			_, err := owner.ReleaseCoordinationClaim(ctx, ownerClaim.Key, ownerClaim.OwnerToken)
			errCh <- err
		}()
		go func() {
			<-start
			_, _, err := contender.TryAcquireCoordinationClaim(ctx, testClaim("contender", 202, now))
			errCh <- err
		}()
		close(start)
		for range 2 {
			if err := <-errCh; err != nil {
				t.Fatalf("iteration %d release/acquire race: %v", i, err)
			}
		}
		_, _ = contender.ReleaseCoordinationClaim(ctx, ownerClaim.Key, "contender")
		_, _ = owner.ReleaseCoordinationClaim(ctx, ownerClaim.Key, ownerClaim.OwnerToken)
	}
}
