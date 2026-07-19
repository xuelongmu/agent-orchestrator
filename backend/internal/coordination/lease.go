// Package coordination owns the exclusive SQLite-writer lease shared by the
// daemon and exceptional direct-DB commands such as legacy import.
package coordination

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	sqlitestore "github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

const (
	claimKey          = "exclusive-db-writer"
	leaseDuration     = 15 * time.Second
	leaseRenewEvery   = 5 * time.Second
	acquireCASRetries = 3
)

var errLeaseLost = errors.New("exclusive database-writer lease lost")

type store interface {
	TryAcquireCoordinationClaim(ctx context.Context, claim sqlitestore.CoordinationClaim) (sqlitestore.CoordinationClaim, bool, error)
	TakeOverCoordinationClaim(ctx context.Context, expectedOwnerToken string, now time.Time, claim sqlitestore.CoordinationClaim) (bool, error)
	RenewCoordinationClaim(ctx context.Context, key, ownerToken string, now, leaseExpiresAt time.Time) (bool, error)
	ReleaseCoordinationClaim(ctx context.Context, key, ownerToken string) (bool, error)
}

// Lease is one token-fenced generation of the exclusive database writer.
type Lease struct {
	store    store
	token    string
	ownerPID int
	duration time.Duration
	// confirmedUntil is the holder's local monotonic watchdog deadline for
	// the lease established by acquireAt. Only the renewal goroutine advances it.
	confirmedUntil time.Time
}

// OpenExclusive opens the canonical SQLite store and immediately acquires its
// exclusive-writer lease. It is the shared boundary for the daemon and every
// exceptional direct-DB writer. On claim failure it closes the store before
// returning, so callers can never receive an unfenced writable handle.
func OpenExclusive(ctx context.Context, dataDir string, ownerPID int) (*sqlite.Store, *Lease, error) {
	store, err := sqlite.Open(dataDir)
	if err != nil {
		return nil, nil, err
	}
	lease, err := Acquire(ctx, store, ownerPID)
	if err != nil {
		_ = store.Close()
		return nil, nil, err
	}
	return store, lease, nil
}

// Acquire claims the exclusive database writer or refuses while an unexpired
// holder exists. The random token, not the diagnostic PID, is the fencing
// generation, so PID reuse cannot impersonate a crashed holder.
func Acquire(ctx context.Context, st store, ownerPID int) (*Lease, error) {
	// Keep the monotonic component for the local watchdog. acquireAt derives a
	// UTC wall-clock value separately for the cross-process SQLite lease.
	return acquireAt(ctx, st, ownerPID, uuid.NewString(), time.Now(), leaseDuration)
}

func acquireAt(ctx context.Context, st store, ownerPID int, token string, now time.Time, duration time.Duration) (*Lease, error) {
	if token == "" || ownerPID <= 0 || duration <= 0 {
		return nil, errors.New("invalid exclusive database-writer lease")
	}
	lease := &Lease{store: st, token: token, ownerPID: ownerPID, duration: duration, confirmedUntil: now.Add(duration)}
	wallNow := now.UTC()
	for range acquireCASRetries {
		desired := lease.claimAt(wallNow)
		current, acquired, err := st.TryAcquireCoordinationClaim(ctx, desired)
		if err != nil {
			return nil, err
		}
		if acquired {
			return lease, nil
		}
		if current.LeaseExpiresAt.After(wallNow) {
			return nil, fmt.Errorf("exclusive database writer leased by pid %d until %s", current.OwnerPID, current.LeaseExpiresAt.UTC().Format(time.RFC3339Nano))
		}
		taken, err := st.TakeOverCoordinationClaim(ctx, current.OwnerToken, wallNow, desired)
		if err != nil {
			return nil, err
		}
		if taken {
			return lease, nil
		}
		// A concurrent renewal or takeover changed the guarded token or expiry.
	}
	return nil, errors.New("exclusive database-writer lease changed repeatedly")
}

func (l *Lease) claimAt(now time.Time) sqlitestore.CoordinationClaim {
	return sqlitestore.CoordinationClaim{
		Key:            claimKey,
		OwnerToken:     l.token,
		OwnerPID:       l.ownerPID,
		ClaimedAt:      now,
		LeaseExpiresAt: now.Add(l.duration),
	}
}

func (l *Lease) renewAt(ctx context.Context, now time.Time) (bool, error) {
	return l.store.RenewCoordinationClaim(ctx, claimKey, l.token, now, now.Add(l.duration))
}

// Release removes the claim only if this Lease's token still owns it.
func (l *Lease) Release(ctx context.Context) error {
	_, err := l.store.ReleaseCoordinationClaim(ctx, claimKey, l.token)
	return err
}

// Maintain renews the lease until ctx ends. Any renewal error or ownership
// loss fails closed by cancelling the caller's root context. Daemon and import
// root all database-writing side effects in that context.
func (l *Lease) Maintain(ctx context.Context, cancel context.CancelFunc, log *slog.Logger) <-chan struct{} {
	ticker := time.NewTicker(leaseRenewEvery)
	watchdog := newTimerWatchdog(l.confirmedUntil, cancel)
	done := maintainWithTicks(ctx, l, ticker.C, time.Now, watchdog, cancel, log)
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		<-done
		ticker.Stop()
	}()
	return stopped
}

// maintainWithTicks exists so fencing tests can drive renewal deterministically
// without sleeping. Production calls Maintain, which supplies a real ticker.
func maintainWithTicks(ctx context.Context, lease *Lease, ticks <-chan time.Time, now func() time.Time, watchdog leaseWatchdog, cancel context.CancelFunc, log *slog.Logger) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer func() {
			watchdog.Stop()
			close(done)
		}()
		confirmedUntil := lease.confirmedUntil
		for {
			select {
			case <-ctx.Done():
				return
			case _, ok := <-ticks:
				if !ok {
					cancel()
					return
				}
				// Sample at execution, never from time.Ticker.C: a queued ticker
				// payload can predate a scheduler or DB stall by longer than the lease.
				executedAt := now()
				if !executedAt.Before(confirmedUntil) {
					logLeaseLoss(log, errLeaseLost)
					cancel()
					return
				}
				renewed, err := lease.renewAt(ctx, executedAt.UTC())
				completedAt := now()
				if err == nil && renewed && completedAt.Before(confirmedUntil) {
					// A backward wall-clock adjustment must not shorten the local
					// watchdog. The SQL renewal applies the same MAX behavior.
					candidate := executedAt.Add(lease.duration)
					if candidate.After(confirmedUntil) {
						confirmedUntil = candidate
					}
					if !watchdog.Advance(confirmedUntil) {
						logLeaseLoss(log, errLeaseLost)
						cancel()
						return
					}
					continue
				}
				if err == nil {
					err = errLeaseLost
				}
				logLeaseLoss(log, err)
				cancel()
				return
			}
		}
	}()
	return done
}

func logLeaseLoss(log *slog.Logger, err error) {
	if log != nil {
		log.Error("exclusive database-writer lease lost; stopping all work", "err", err)
	}
}

type leaseWatchdog interface {
	Advance(deadline time.Time) bool
	Stop()
}

// timerWatchdog is independent of the renewal goroutine. It cancels the root
// context at the last locally confirmed expiry even if a renewal DB call or the
// scheduler stalls. Advance and the callback are mutex-fenced: a renewal racing
// the expiry loses closed rather than reviving an already-fired generation.
type timerWatchdog struct {
	mu      sync.Mutex
	timer   *time.Timer
	fired   bool
	stopped bool
	cancel  context.CancelFunc
}

func newTimerWatchdog(deadline time.Time, cancel context.CancelFunc) *timerWatchdog {
	w := &timerWatchdog{cancel: cancel}
	w.timer = time.AfterFunc(time.Until(deadline), w.fire)
	return w
}

func (w *timerWatchdog) fire() {
	w.mu.Lock()
	if w.fired || w.stopped {
		w.mu.Unlock()
		return
	}
	w.fired = true
	w.mu.Unlock()
	w.cancel()
}

func (w *timerWatchdog) Advance(deadline time.Time) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.fired || w.stopped {
		return false
	}
	if !w.timer.Stop() {
		// The callback is already running or has fired. Mark the watchdog fired;
		// fire observes this flag and cancellation below is the single outcome.
		w.fired = true
		return false
	}
	w.timer.Reset(max(time.Until(deadline), time.Nanosecond))
	return true
}

func (w *timerWatchdog) Stop() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.stopped {
		return
	}
	w.stopped = true
	w.timer.Stop()
}
