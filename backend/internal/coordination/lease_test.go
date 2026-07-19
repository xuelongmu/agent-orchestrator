package coordination

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
)

type fakeWatchdog struct {
	mu       sync.Mutex
	deadline time.Time
	stopped  bool
	cancel   context.CancelFunc
	advanced chan time.Time
}

func (w *fakeWatchdog) Advance(deadline time.Time) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.stopped {
		return false
	}
	w.deadline = deadline
	if w.advanced != nil {
		w.advanced <- deadline
	}
	return true
}

func (w *fakeWatchdog) Stop() {
	w.mu.Lock()
	w.stopped = true
	w.mu.Unlock()
}

func (w *fakeWatchdog) fire() {
	w.cancel()
}

func openStore(t *testing.T) *sqlite.Store {
	t.Helper()
	store, err := sqlite.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func TestLiveOwnerIsNotStolenAfterFailedHealthProbe(t *testing.T) {
	store := openStore(t)
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	if _, err := acquireAt(context.Background(), store, 101, "generation-a", now, 10*time.Second); err != nil {
		t.Fatalf("acquire live owner: %v", err)
	}
	// No run-file or health result participates in leasing. Even the same PID
	// (the PID-reuse case) cannot acquire a different generation before expiry.
	if _, err := acquireAt(context.Background(), store, 101, "generation-b", now.Add(time.Second), 10*time.Second); err == nil {
		t.Fatal("live unexpired generation was stolen")
	}
}

func TestCrashBeforeRunFileWithPIDReuseIsReclaimableAfterExpiry(t *testing.T) {
	store := openStore(t)
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	if _, err := acquireAt(context.Background(), store, 101, "crashed-generation", now, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	lease, err := acquireAt(context.Background(), store, 101, "replacement-generation", now.Add(10*time.Second), 10*time.Second)
	if err != nil || lease.token != "replacement-generation" {
		t.Fatalf("expiry takeover lease=%+v err=%v", lease, err)
	}
}

func TestStaleOwnerCannotRenewOrReleaseSuccessor(t *testing.T) {
	store := openStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	stale, err := acquireAt(ctx, store, 101, "stale", now, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	successor, err := acquireAt(ctx, store, 202, "successor", now.Add(10*time.Second), 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if renewed, err := stale.renewAt(ctx, now.Add(11*time.Second)); err != nil || renewed {
		t.Fatalf("stale renewal=%v err=%v, want fenced no-op", renewed, err)
	}
	if err := stale.Release(ctx); err != nil {
		t.Fatalf("stale release: %v", err)
	}
	if renewed, err := successor.renewAt(ctx, now.Add(11*time.Second)); err != nil || !renewed {
		t.Fatalf("successor was released/changed: renewed=%v err=%v", renewed, err)
	}
}

func TestDelayedTickerPayloadCannotRenewAfterExecutionTimeExpiry(t *testing.T) {
	store := openStore(t)
	base := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	lease, err := acquireAt(context.Background(), store, 101, "owner", base, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	workCtx, cancel := context.WithCancel(context.Background())
	ticks := make(chan time.Time, 1)
	watchdog := &fakeWatchdog{deadline: lease.confirmedUntil, cancel: cancel}
	done := maintainWithTicks(workCtx, lease, ticks, func() time.Time { return base.Add(11 * time.Second) }, watchdog, cancel, nil)
	ticks <- base.Add(time.Second) // stale queued payload must be ignored
	<-done
	if workCtx.Err() == nil {
		t.Fatal("holder work remained live after execution-time expiry")
	}
}

func TestBackwardClockRenewalDoesNotShortenLocalWatchdog(t *testing.T) {
	store := openStore(t)
	base := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	lease, err := acquireAt(context.Background(), store, 101, "owner", base, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	workCtx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	ticks := make(chan time.Time, 1)
	advanced := make(chan time.Time, 1)
	watchdog := &fakeWatchdog{deadline: lease.confirmedUntil, cancel: cancel, advanced: advanced}
	now := func() time.Time { return base.Add(-5 * time.Second) }
	done := maintainWithTicks(workCtx, lease, ticks, now, watchdog, cancel, nil)
	ticks <- base
	deadline := <-advanced
	if !deadline.Equal(base.Add(10 * time.Second)) {
		t.Fatalf("local deadline shortened to %s", deadline)
	}
	cancel()
	<-done
}

func TestRunningHolderStopsWorkAfterSuccessorTakesLease(t *testing.T) {
	store := openStore(t)
	base := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	stale, err := acquireAt(context.Background(), store, os.Getpid(), "stale", base, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := acquireAt(context.Background(), store, os.Getpid(), "successor", base.Add(10*time.Second), 10*time.Second); err != nil {
		t.Fatal(err)
	}
	workCtx, cancel := context.WithCancel(context.Background())
	ticks := make(chan time.Time, 1)
	watchdog := &fakeWatchdog{deadline: stale.confirmedUntil, cancel: cancel}
	// Simulate the old process's wall clock lagging the database/successor. Its
	// local watchdog has not fired, but token-fenced renewal must still cancel it.
	done := maintainWithTicks(workCtx, stale, ticks, func() time.Time { return base.Add(9 * time.Second) }, watchdog, cancel, nil)
	ticks <- base
	<-done
	if workCtx.Err() == nil {
		t.Fatal("old holder kept running after token-fenced renewal loss")
	}
}

func TestWatchdogStopsWorkWhenRenewalIsDelayed(t *testing.T) {
	store := openStore(t)
	base := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	lease, err := acquireAt(context.Background(), store, 101, "owner", base, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	workCtx, cancel := context.WithCancel(context.Background())
	ticks := make(chan time.Time)
	watchdog := &fakeWatchdog{deadline: lease.confirmedUntil, cancel: cancel}
	done := maintainWithTicks(workCtx, lease, ticks, func() time.Time { return base }, watchdog, cancel, nil)
	watchdog.fire()
	<-done
	if workCtx.Err() == nil {
		t.Fatal("watchdog expiry did not stop holder work")
	}
}
