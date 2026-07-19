package coordination

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	sqlitestore "github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

type fakeWatchdog struct {
	mu       sync.Mutex
	deadline time.Time
	stopped  bool
	cancel   context.CancelFunc
	advanced chan time.Time
}

type takeoverHookStore struct {
	store
	reads            int
	observed         sqlitestore.CoordinationClaim
	observedAcquired bool
	beforeTakeover   func()
}

func (s *takeoverHookStore) TryAcquireCoordinationClaim(ctx context.Context, claim sqlitestore.CoordinationClaim) (sqlitestore.CoordinationClaim, bool, error) {
	if s.reads == 0 {
		current, acquired, err := s.store.TryAcquireCoordinationClaim(ctx, claim)
		if err != nil {
			return sqlitestore.CoordinationClaim{}, false, err
		}
		s.observed = current
		s.observedAcquired = acquired
	}
	if s.reads < 2 {
		s.reads++
		return s.observed, s.observedAcquired, nil
	}
	return s.store.TryAcquireCoordinationClaim(ctx, claim)
}

func (s *takeoverHookStore) TakeOverCoordinationClaim(ctx context.Context, expectedOwnerToken string, now time.Time, claim sqlitestore.CoordinationClaim) (bool, error) {
	if s.beforeTakeover != nil {
		hook := s.beforeTakeover
		s.beforeTakeover = nil
		hook()
	}
	return s.store.TakeOverCoordinationClaim(ctx, expectedOwnerToken, now, claim)
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
	waits := 0
	lease, err := acquireWithClock(
		context.Background(), store, 101, "replacement-generation", 10*time.Second,
		func() time.Time { return now.Add(10 * time.Second) },
		func(_ context.Context, duration time.Duration) error {
			waits++
			if duration != 10*time.Second {
				t.Fatalf("quarantine=%s, want 10s", duration)
			}
			return nil
		},
	)
	if err != nil || lease.token != "replacement-generation" {
		t.Fatalf("expiry takeover lease=%+v err=%v", lease, err)
	}
	if waits != 1 {
		t.Fatalf("quarantine waits=%d, want 1", waits)
	}
}

func TestForwardWallJumpRenewalBlocksTakeover(t *testing.T) {
	store := openStore(t)
	ctx := context.Background()
	base := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	holder, err := acquireAt(ctx, store, 101, "holder", base, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}

	jumped := base.Add(365 * 24 * time.Hour)
	waits := 0
	lease, err := acquireWithClock(
		ctx, store, 202, "contender", 10*time.Second,
		func() time.Time { return jumped },
		func(_ context.Context, duration time.Duration) error {
			waits++
			if duration != 10*time.Second {
				t.Fatalf("quarantine=%s, want 10s", duration)
			}
			renewed, renewErr := holder.renewAt(ctx, base.Add(5*time.Second))
			if renewErr != nil || !renewed {
				t.Fatalf("holder renewal=%v err=%v", renewed, renewErr)
			}
			return nil
		},
	)
	if err == nil || lease != nil {
		t.Fatalf("forward jump stole renewed holder: lease=%+v err=%v", lease, err)
	}
	if waits != 1 {
		t.Fatalf("quarantine waits=%d, want 1", waits)
	}
	if renewed, renewErr := holder.renewAt(ctx, base.Add(6*time.Second)); renewErr != nil || !renewed {
		t.Fatalf("holder lost ownership: renewed=%v err=%v", renewed, renewErr)
	}
}

func TestForwardWallJumpCrashedHolderIsTakenOverAfterQuarantine(t *testing.T) {
	store := openStore(t)
	ctx := context.Background()
	base := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	if _, err := acquireAt(ctx, store, 101, "crashed", base, 10*time.Second); err != nil {
		t.Fatal(err)
	}

	jumped := base.Add(365 * 24 * time.Hour)
	waited := time.Duration(0)
	lease, err := acquireWithClock(
		ctx, store, 202, "successor", 10*time.Second,
		func() time.Time { return jumped },
		func(_ context.Context, duration time.Duration) error {
			waited += duration
			return nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if lease.token != "successor" {
		t.Fatalf("token=%q, want successor", lease.token)
	}
	if waited != 10*time.Second {
		t.Fatalf("quarantine=%s, want watchdog bound 10s", waited)
	}
}

func TestRenewalRacingTakeoverCASPreservesHolder(t *testing.T) {
	underlying := openStore(t)
	ctx := context.Background()
	base := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	holder, err := acquireAt(ctx, underlying, 101, "holder", base, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}

	hooked := &takeoverHookStore{store: underlying}
	hooked.beforeTakeover = func() {
		renewed, renewErr := holder.renewAt(ctx, base.Add(5*time.Second))
		if renewErr != nil || !renewed {
			t.Fatalf("racing renewal=%v err=%v", renewed, renewErr)
		}
	}
	jumped := base.Add(365 * 24 * time.Hour)
	lease, err := acquireWithClock(
		ctx, hooked, 202, "contender", 10*time.Second,
		func() time.Time { return jumped },
		func(context.Context, time.Duration) error { return nil },
	)
	if err == nil || lease != nil {
		t.Fatalf("racing renewal was overwritten: lease=%+v err=%v", lease, err)
	}
	if hooked.reads != 2 || hooked.beforeTakeover != nil {
		t.Fatalf("reads=%d hook pending=%v, want two stable reads then CAS hook", hooked.reads, hooked.beforeTakeover != nil)
	}
	if renewed, renewErr := holder.renewAt(ctx, base.Add(6*time.Second)); renewErr != nil || !renewed {
		t.Fatalf("renewed holder lost ownership: renewed=%v err=%v", renewed, renewErr)
	}
}

func TestTakeoverQuarantineHonorsCancellation(t *testing.T) {
	store := openStore(t)
	base := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	holder, err := acquireAt(context.Background(), store, 101, "holder", base, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	lease, err := acquireWithClock(
		ctx, store, 202, "contender", 10*time.Second,
		func() time.Time { return base.Add(24 * time.Hour) },
		func(ctx context.Context, _ time.Duration) error {
			cancel()
			return waitMonotonic(ctx, time.Hour)
		},
	)
	if !errors.Is(err, context.Canceled) || lease != nil {
		t.Fatalf("cancelled takeover lease=%+v err=%v", lease, err)
	}
	if renewed, renewErr := holder.renewAt(context.Background(), base.Add(5*time.Second)); renewErr != nil || !renewed {
		t.Fatalf("cancelled contender changed holder: renewed=%v err=%v", renewed, renewErr)
	}
}

func TestStaleObservedTokenCannotTakeOverNewGeneration(t *testing.T) {
	store := openStore(t)
	ctx := context.Background()
	base := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	if _, err := acquireAt(ctx, store, 101, "observed", base, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	replacement := sqlitestore.CoordinationClaim{
		Key:            claimKey,
		OwnerToken:     "replacement",
		OwnerPID:       202,
		ClaimedAt:      base.Add(10 * time.Second),
		LeaseExpiresAt: base.Add(20 * time.Second),
	}
	lease, err := acquireWithClock(
		ctx, store, 303, "stale-contender", 10*time.Second,
		func() time.Time { return base.Add(24 * time.Hour) },
		func(_ context.Context, _ time.Duration) error {
			taken, takeoverErr := store.TakeOverCoordinationClaim(ctx, "observed", base.Add(10*time.Second), replacement)
			if takeoverErr != nil || !taken {
				t.Fatalf("replacement takeover=%v err=%v", taken, takeoverErr)
			}
			return nil
		},
	)
	if err == nil || lease != nil {
		t.Fatalf("stale token took new generation: lease=%+v err=%v", lease, err)
	}
	if renewed, renewErr := store.RenewCoordinationClaim(ctx, claimKey, "replacement", base.Add(11*time.Second), base.Add(21*time.Second)); renewErr != nil || !renewed {
		t.Fatalf("replacement was overwritten: renewed=%v err=%v", renewed, renewErr)
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
