// Package dependency reconciles durable task prerequisites into exactly-once
// session launch promotions. It never decides or writes parent terminal state;
// it consumes completion facts already persisted by handoff and PR pipelines.
package dependency

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// Store is the durable graph and promotion-claim boundary.
type Store interface {
	ListReadyDependencySessions(context.Context) ([]domain.SessionID, error)
	ListDependencyHandoffs(context.Context, domain.SessionID) ([]domain.DependencyHandoff, error)
	ReserveDependencyPromotion(context.Context, domain.SessionID, string, time.Time) (bool, error)
	CompleteDependencyPromotion(context.Context, domain.SessionID, string, time.Time) (bool, error)
	ReleaseDependencyPromotion(context.Context, domain.SessionID, string, time.Time) (bool, error)
	RecoverDependencyPromotions(context.Context, time.Time) (int64, error)
	RecoverStaleDependencyPromotions(context.Context, time.Time, time.Time) (int64, error)
}

// Recover clears reservations abandoned by a prior daemon. The caller must
// hold AO's process-wide exclusive coordination lease before invoking it.
func (s *Scheduler) Recover(ctx context.Context) error {
	s.reconcileMu.Lock()
	defer s.reconcileMu.Unlock()
	_, err := s.store.RecoverDependencyPromotions(ctx, s.clock())
	return err
}

// CompleteRecovered consumes the still-owned token only after Session Manager
// has probed and adopted the persisted runtime during boot reconciliation.
func (s *Scheduler) CompleteRecovered(ctx context.Context, id domain.SessionID, token string) error {
	completed, err := s.completePromotion(id, token)
	if err != nil {
		return err
	}
	if !completed {
		return fmt.Errorf("complete recovered dependency promotion %s: reservation token was lost", id)
	}
	return nil
}

// ReleaseRecovered returns a dead, fully-cleaned in-flight launch to the ready
// queue. Session Manager calls it only after its token-fenced runtime/workspace
// cleanup and narrow metadata reset have both succeeded.
func (s *Scheduler) ReleaseRecovered(ctx context.Context, id domain.SessionID, token string) error {
	return s.releaseReservation(ctx, id, token)
}

// Launcher starts a child after its promotion claim has committed.
type Launcher interface {
	LaunchPromoted(context.Context, domain.SessionID, string, []domain.DependencyHandoff) (domain.SessionRecord, error)
}

type recoveryLauncher interface {
	RecoverPromotedDependencyLaunches(context.Context) error
}

const promotionReservationLease = 30 * time.Minute

// Scheduler is safe for concurrent completion signals. The SQLite reservation
// is the authoritative in-process and cross-process exactly-once boundary.
type Scheduler struct {
	store    Store
	launcher Launcher
	clock    func() time.Time
	logger   *slog.Logger
	lifetime context.Context
	wait     func(context.Context, time.Duration, <-chan struct{}, bool) bool
	wake     chan struct{}
	// reconcileMu serializes recovery and launch. A second completion signal
	// must not interpret the first signal's predicted runtime handle as an
	// abandoned attempt while that launch is still provisioning.
	reconcileMu sync.Mutex
}

// New constructs a scheduler. Nil clock/logger use production defaults.
func New(store Store, launcher Launcher, clock func() time.Time, logger *slog.Logger) *Scheduler {
	if clock == nil {
		clock = func() time.Time { return time.Now().UTC() }
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Scheduler{store: store, launcher: launcher, clock: clock, logger: logger, lifetime: context.Background(), wait: waitDelay, wake: make(chan struct{}, 1)}
}

// Wake requests an asynchronous reconcile without entering scheduler locks on
// the caller's stack. Lifecycle invokes this while it may be nested under
// Session Manager locks, avoiding lock-order cycles with promotion launch.
func (s *Scheduler) Wake() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// Start runs daemon-lifetime reconciliation with bounded retry backoff. It is
// intentionally independent of completion callbacks: a transient launch or
// store failure must become retryable in the same daemon even if no unrelated
// event arrives. Reconcile's mutex remains the single in-process critical
// section shared with synchronous triggers and boot recovery.
func (s *Scheduler) Start(ctx context.Context, minDelay, maxDelay time.Duration) <-chan struct{} {
	done := make(chan struct{})
	if minDelay <= 0 {
		minDelay = 2 * time.Second
	}
	if maxDelay < minDelay {
		maxDelay = 30 * time.Second
	}
	go func() {
		defer close(done)
		delay := minDelay
		backingOff := false
		for {
			if !s.wait(ctx, delay, s.wake, !backingOff) {
				return
			}
			retryable, err := s.reconcile(ctx)
			if err != nil {
				s.logger.Error("dependency reconcile loop failed", "error", err)
			}
			if retryable || err != nil {
				backingOff = true
				if delay < maxDelay {
					delay *= 2
					if delay > maxDelay {
						delay = maxDelay
					}
				}
				continue
			}
			delay = minDelay
			backingOff = false
		}
	}()
	return done
}

func waitDelay(ctx context.Context, delay time.Duration, wake <-chan struct{}, allowWake bool) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	if !allowWake {
		select {
		case <-ctx.Done():
			return false
		case <-timer.C:
			return true
		}
	}
	select {
	case <-ctx.Done():
		return false
	case <-wake:
		return true
	case <-timer.C:
		return true
	}
}

// SetLifetimeContext binds all detached completion/release work to the daemon's
// exclusive-writer lease lifetime. It must be called during daemon wiring,
// before concurrent reconciliation begins.
func (s *Scheduler) SetLifetimeContext(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}
	s.lifetime = ctx
}

// Reconcile promotes every currently-ready child at most once. Launch failures
// are logged after the durable claim; they do not make a completed parent's
// handoff request fail or permit a second launch attempt to race the first.
func (s *Scheduler) Reconcile(ctx context.Context) error {
	_, err := s.reconcile(ctx)
	return err
}

func (s *Scheduler) reconcile(ctx context.Context) (bool, error) {
	s.reconcileMu.Lock()
	defer s.reconcileMu.Unlock()
	ctx, cancel := s.operationContext(ctx)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return false, err
	}
	retryable := false
	var reconcileErrs []error
	if recovery, ok := s.launcher.(recoveryLauncher); ok {
		if err := recovery.RecoverPromotedDependencyLaunches(ctx); err != nil {
			s.logger.Error("dependency promotion recovery incomplete", "error", err)
			retryable = true
		}
	}
	now := s.clock()
	if _, err := s.store.RecoverStaleDependencyPromotions(ctx, now, now.Add(-promotionReservationLease)); err != nil {
		s.logger.Error("dependency promotion stale-claim recovery incomplete", "error", err)
		reconcileErrs = append(reconcileErrs, err)
	}
	ids, err := s.store.ListReadyDependencySessions(ctx)
	if err != nil {
		return retryable, errors.Join(append(reconcileErrs, err)...)
	}
	for _, id := range ids {
		token, err := promotionToken()
		if err != nil {
			return retryable, errors.Join(append(reconcileErrs, err)...)
		}
		claimedAt := s.clock()
		claimed, err := s.store.ReserveDependencyPromotion(ctx, id, token, claimedAt)
		if err != nil {
			reconcileErrs = append(reconcileErrs, fmt.Errorf("reserve dependency promotion %s: %w", id, err))
			continue
		}
		if !claimed {
			continue
		}
		// The reservation is the promotion linearization point. Read immutable
		// sealed handoffs only after it commits, so a handoff sealed before the
		// claim cannot be omitted from this launch's prompt snapshot.
		handoffs, err := s.store.ListDependencyHandoffs(ctx, id)
		if err != nil {
			releaseErr := s.releaseReservation(ctx, id, token)
			reconcileErrs = append(reconcileErrs, errors.Join(fmt.Errorf("load dependency handoffs for %s: %w", id, err), releaseErr))
			continue
		}
		if s.launcher == nil {
			reconcileErrs = append(reconcileErrs, errors.Join(fmt.Errorf("promote dependency session %s: launcher is not configured", id), s.releaseReservation(ctx, id, token)))
			continue
		}
		if _, err := s.launcher.LaunchPromoted(ctx, id, token, handoffs); err != nil {
			retryable = true
			s.logger.Error("dependency promotion launch failed", "sessionID", id, "error", err)
			var retained interface{ RetainDependencyReservation() bool }
			if errors.As(err, &retained) && retained.RetainDependencyReservation() {
				continue
			}
			if releaseErr := s.releaseReservation(ctx, id, token); releaseErr != nil {
				reconcileErrs = append(reconcileErrs, releaseErr)
			}
			continue
		}
		completed, err := s.completePromotion(id, token)
		if err != nil {
			reconcileErrs = append(reconcileErrs, fmt.Errorf("complete dependency promotion %s: %w", id, err))
			continue
		}
		if !completed {
			reconcileErrs = append(reconcileErrs, fmt.Errorf("complete dependency promotion %s: reservation token was lost", id))
		}
	}
	return retryable, errors.Join(reconcileErrs...)
}

func (s *Scheduler) releaseReservation(ctx context.Context, id domain.SessionID, token string) error {
	cleanupCtx, cancel := context.WithTimeout(s.lifetime, 2*time.Second)
	defer cancel()
	if err := cleanupCtx.Err(); err != nil {
		return err
	}
	_, err := s.store.ReleaseDependencyPromotion(cleanupCtx, id, token, s.clock())
	return err
}

func (s *Scheduler) completePromotion(id domain.SessionID, token string) (bool, error) {
	completeCtx, cancel := context.WithTimeout(s.lifetime, 2*time.Second)
	defer cancel()
	if err := completeCtx.Err(); err != nil {
		return false, err
	}
	return s.store.CompleteDependencyPromotion(completeCtx, id, token, s.clock())
}

func (s *Scheduler) operationContext(request context.Context) (context.Context, context.CancelFunc) {
	if request == nil {
		request = context.Background()
	}
	// Derive from daemon ownership so an already-cancelled lifetime is visible
	// synchronously before the first store call. Request cancellation is the
	// secondary signal and may never extend the exclusive-writer lifetime.
	ctx, cancel := context.WithCancel(s.lifetime)
	stop := context.AfterFunc(request, cancel)
	if request.Err() != nil {
		cancel()
	}
	return ctx, func() {
		stop()
		cancel()
	}
}

func promotionToken() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("create dependency promotion token: %w", err)
	}
	return hex.EncodeToString(raw[:]), nil
}
