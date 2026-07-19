package daemon

import (
	"context"
	"fmt"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/processalive"
	sqlitestore "github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

const daemonCoordinationClaim = "daemon"

type coordinationClaimer interface {
	TryAcquireCoordinationClaim(ctx context.Context, key string, ownerPID int, claimedAt time.Time) (sqlitestore.CoordinationClaim, bool, error)
	TakeOverCoordinationClaim(ctx context.Context, key string, expectedOwnerPID, ownerPID int, claimedAt time.Time) (bool, error)
	ReleaseCoordinationClaim(ctx context.Context, key string, ownerPID int) (bool, error)
}

// acquireDaemonCoordination closes the startup window before running.json is
// published. A daemon acquires this durable claim before it starts lifecycle,
// SCM, tracker-intake, or reconcile work. The normal run-file/health probe is
// still the user-facing fast path; this claim is the atomic cross-process backstop.
func acquireDaemonCoordination(ctx context.Context, store coordinationClaimer, ownerPID, verifiedStalePID int) (func(context.Context) error, error) {
	for range 3 {
		claim, acquired, err := store.TryAcquireCoordinationClaim(ctx, daemonCoordinationClaim, ownerPID, time.Now().UTC())
		if err != nil {
			return nil, err
		}
		if acquired {
			return func(releaseCtx context.Context) error {
				_, err := store.ReleaseCoordinationClaim(releaseCtx, daemonCoordinationClaim, ownerPID)
				return err
			}, nil
		}

		// A matching run-file was already probed and shown not to be an AO
		// daemon. This handles Windows PID reuse without allowing a second
		// concurrently-starting daemon (which has not published a run-file yet)
		// to steal the first starter's fresh claim.
		stale := claim.OwnerPID == verifiedStalePID || !processalive.Alive(claim.OwnerPID)
		if !stale {
			return nil, fmt.Errorf("daemon startup already claimed by pid %d; refusing to start", claim.OwnerPID)
		}
		taken, err := store.TakeOverCoordinationClaim(ctx, daemonCoordinationClaim, claim.OwnerPID, ownerPID, time.Now().UTC())
		if err != nil {
			return nil, err
		}
		if taken {
			return func(releaseCtx context.Context) error {
				_, err := store.ReleaseCoordinationClaim(releaseCtx, daemonCoordinationClaim, ownerPID)
				return err
			}, nil
		}
		// Another successor changed the owner after our read. Re-read and either
		// recognize our idempotent claim or report the fresh live owner.
	}
	return nil, fmt.Errorf("daemon startup coordination changed repeatedly; refusing to start")
}
