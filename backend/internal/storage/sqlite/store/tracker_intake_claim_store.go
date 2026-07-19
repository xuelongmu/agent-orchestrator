package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/gen"
)

const trackerIntakeCompleted = "completed"

const trackerIntakeAdmitted = "admitted"

const trackerIntakeSpawning = "spawning"

// TrackerIntakeHasCapacity is the cheap pre-list guard used to preserve the
// observer's existing behavior of not calling the tracker while a project is
// full. ClaimTrackerIntakeIssue repeats the check under BEGIN IMMEDIATE, so
// this snapshot is only an optimization and never authorizes a spawn.
func (s *Store) TrackerIntakeHasCapacity(ctx context.Context, projectID domain.ProjectID, maxConcurrent int, now time.Time) (bool, error) {
	if projectID == "" || maxConcurrent <= 0 || now.IsZero() {
		return false, fmt.Errorf("invalid tracker intake capacity project=%q max=%d", projectID, maxConcurrent)
	}
	used, err := s.qr.CountTrackerIntakeCapacityUsed(ctx, gen.CountTrackerIntakeCapacityUsedParams{
		ProjectID: projectID, ProjectID_2: string(projectID),
		LeaseExpiresAt: now, LeaseExpiresAt_2: now, LeaseExpiresAt_3: now,
	})
	if err != nil {
		return false, fmt.Errorf("count tracker intake capacity for %s: %w", projectID, err)
	}
	return used < int64(maxConcurrent), nil
}

// MarkTrackerIntakeSpawnStarted durably closes pure-seed recovery immediately
// before workspace/runtime side effects can begin. Only the exact live owner
// and bound session generation may cross this fence.
func (s *Store) MarkTrackerIntakeSpawnStarted(ctx context.Context, claim ports.TrackerIntakeClaim, sessionID domain.SessionID, startedAt time.Time) (bool, error) {
	if err := validateTrackerIntakeClaim(claim, 1); err != nil || sessionID == "" || startedAt.IsZero() {
		return false, fmt.Errorf("invalid tracker intake spawn start project=%q session=%q", claim.ProjectID, sessionID)
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	rows, err := s.qw.MarkTrackerIntakeClaimSpawning(ctx, gen.MarkTrackerIntakeClaimSpawningParams{
		ProjectID: string(claim.ProjectID), Provider: string(claim.Provider), Repo: claim.Repo,
		IssueID: claim.IssueID, OwnerToken: claim.OwnerToken, SessionID: string(sessionID), LeaseExpiresAt: startedAt,
	})
	if err != nil {
		return false, fmt.Errorf("mark tracker issue %s/%s/%s/%s spawning: %w", claim.ProjectID, claim.Provider, claim.Repo, claim.IssueID, err)
	}
	return rows == 1, nil
}

// ClaimTrackerIntakeIssue is the atomic CAS boundary before spawn. It first
// reconciles any crash-surviving session into the completed ledger, then
// inserts or takes over a pending lease only when project capacity remains.
func (s *Store) ClaimTrackerIntakeIssue(ctx context.Context, claim ports.TrackerIntakeClaim, maxConcurrent int) (ports.TrackerIntakeClaimResult, error) {
	if err := validateTrackerIntakeClaim(claim, maxConcurrent); err != nil {
		return 0, err
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	result := ports.TrackerIntakeClaimAlreadyProcessed
	err := s.inImmediateConnTx(ctx, "claim tracker intake issue", func(conn *sql.Conn, q *gen.Queries) error {
		canonicalIssueID := string(claim.Provider) + ":" + claim.IssueID
		existing, claimErr := q.GetTrackerIntakeClaim(ctx, trackerIntakeClaimKeyParams(claim))
		if claimErr != nil && !errors.Is(claimErr, sql.ErrNoRows) {
			return claimErr
		}
		if errors.Is(claimErr, sql.ErrNoRows) {
			sessionID, err := findLegacyTrackerIntakeSession(ctx, q, claim, domain.IssueID(canonicalIssueID))
			if err == nil {
				return reconcileTrackerIntakeSession(ctx, q, claim, sessionID)
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
		}
		if claimErr == nil {
			if existing.Status == trackerIntakeCompleted {
				return nil
			}
			if (existing.Status == trackerIntakeAdmitted || existing.Status == trackerIntakeSpawning) && existing.SessionID != "" {
				matches, err := trackerIntakeActiveSessionMatches(ctx, q, claim, domain.SessionID(existing.SessionID))
				if err != nil {
					return err
				}
				if matches {
					return reconcileTrackerIntakeSession(ctx, q, claim, domain.SessionID(existing.SessionID))
				}
			}
			switch {
			case (existing.Status == trackerIntakeAdmitted || existing.Status == trackerIntakeSpawning) && !existing.LeaseExpiresAt.After(claim.ClaimedAt):
				safe, deleteSeed, err := expiredTrackerIntakeClaimRecovery(ctx, q, existing)
				if err != nil {
					return err
				}
				if !safe {
					result = ports.TrackerIntakeClaimBusy
					return nil
				}
				if deleteSeed {
					if err := deleteExpiredTrackerIntakeSeed(ctx, conn, q, domain.SessionID(existing.SessionID)); err != nil {
						return err
					}
				}
			case existing.OwnerToken == claim.OwnerToken && existing.LeaseExpiresAt.After(claim.ClaimedAt):
				result = ports.TrackerIntakeClaimAcquired
				return nil
			case existing.LeaseExpiresAt.After(claim.ClaimedAt):
				result = ports.TrackerIntakeClaimBusy
				return nil
			}
		}
		used, err := q.CountTrackerIntakeCapacityUsed(ctx, gen.CountTrackerIntakeCapacityUsedParams{
			ProjectID: claim.ProjectID, ProjectID_2: string(claim.ProjectID),
			LeaseExpiresAt: claim.ClaimedAt, LeaseExpiresAt_2: claim.ClaimedAt, LeaseExpiresAt_3: claim.ClaimedAt,
		})
		if err != nil {
			return err
		}
		if used >= int64(maxConcurrent) {
			result = ports.TrackerIntakeClaimCapacityReached
			return nil
		}

		if errors.Is(claimErr, sql.ErrNoRows) {
			rows, err := q.InsertPendingTrackerIntakeClaim(ctx, gen.InsertPendingTrackerIntakeClaimParams{
				ProjectID: string(claim.ProjectID), Provider: string(claim.Provider), Repo: claim.Repo,
				IssueID: claim.IssueID, OwnerToken: claim.OwnerToken,
				ClaimedAt: claim.ClaimedAt, LeaseExpiresAt: claim.LeaseExpiresAt,
			})
			if err != nil {
				return err
			}
			if rows != 1 {
				return errors.New("tracker intake claim changed while writer lock was held")
			}
		} else {
			rows, err := q.TakeOverExpiredTrackerIntakeClaim(ctx, gen.TakeOverExpiredTrackerIntakeClaimParams{
				OwnerToken: claim.OwnerToken, ClaimedAt: claim.ClaimedAt, LeaseExpiresAt: claim.LeaseExpiresAt,
				ProjectID: string(claim.ProjectID), Provider: string(claim.Provider), Repo: claim.Repo, IssueID: claim.IssueID,
				OwnerToken_2: existing.OwnerToken, LeaseExpiresAt_2: claim.ClaimedAt,
			})
			if err != nil {
				return err
			}
			if rows != 1 {
				return errors.New("expired tracker intake claim changed while writer lock was held")
			}
		}
		result = ports.TrackerIntakeClaimAcquired
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("claim tracker issue %s/%s/%s/%s: %w", claim.ProjectID, claim.Provider, claim.Repo, claim.IssueID, err)
	}
	return result, nil
}

// CompleteTrackerIntakeIssue permanently accounts a successful spawn. The
// session row is verified inside the same writer transaction before the exact
// owner generation may complete the claim.
func (s *Store) CompleteTrackerIntakeIssue(ctx context.Context, claim ports.TrackerIntakeClaim, sessionID domain.SessionID, completedAt time.Time) (bool, error) {
	if err := validateTrackerIntakeClaim(claim, 1); err != nil || sessionID == "" || completedAt.IsZero() {
		return false, fmt.Errorf("invalid tracker intake completion project=%q session=%q", claim.ProjectID, sessionID)
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	completed := false
	err := s.inImmediateTx(ctx, "complete tracker intake issue", func(q *gen.Queries) error {
		matches, err := q.TrackerIntakeCompletionSessionMatches(ctx, gen.TrackerIntakeCompletionSessionMatchesParams{
			ID: sessionID, ProjectID: claim.ProjectID,
			CanonicalIssueID: domain.IssueID(string(claim.Provider) + ":" + claim.IssueID), Provider: string(claim.Provider),
		})
		if err != nil {
			return err
		}
		if !matches {
			return fmt.Errorf("session %s does not match claimed issue", sessionID)
		}
		rows, err := q.CompleteTrackerIntakeClaim(ctx, gen.CompleteTrackerIntakeClaimParams{
			SessionID: string(sessionID), LeaseExpiresAt: completedAt, CompletedAt: sql.NullTime{Time: completedAt, Valid: true},
			ProjectID: string(claim.ProjectID), Provider: string(claim.Provider), Repo: claim.Repo,
			IssueID: claim.IssueID, OwnerToken: claim.OwnerToken, SessionID_2: string(sessionID),
		})
		if err != nil || rows == 1 {
			completed = rows == 1
			return err
		}
		existing, err := q.GetTrackerIntakeClaim(ctx, trackerIntakeClaimKeyParams(claim))
		if err == nil && existing.Status == trackerIntakeCompleted && existing.SessionID == string(sessionID) {
			completed = true
		}
		return err
	})
	if err != nil {
		return false, fmt.Errorf("complete tracker issue %s/%s/%s/%s: %w", claim.ProjectID, claim.Provider, claim.Repo, claim.IssueID, err)
	}
	return completed, nil
}

// ReleaseTrackerIntakeIssue frees a confirmed failed spawn for immediate retry.
// It is token-fenced, and a durable matching session wins over release: that
// session is reconciled to completed so a delayed error cannot open a duplicate
// spawn race.
func (s *Store) ReleaseTrackerIntakeIssue(ctx context.Context, claim ports.TrackerIntakeClaim, releasedAt time.Time) (bool, error) {
	if err := validateTrackerIntakeClaim(claim, 1); err != nil || releasedAt.IsZero() {
		return false, fmt.Errorf("invalid tracker intake release project=%q", claim.ProjectID)
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	released := false
	err := s.inImmediateTx(ctx, "release tracker intake issue", func(q *gen.Queries) error {
		existing, err := q.GetTrackerIntakeClaim(ctx, trackerIntakeClaimKeyParams(claim))
		if errors.Is(err, sql.ErrNoRows) || (err == nil && (existing.Status == trackerIntakeCompleted || existing.OwnerToken != claim.OwnerToken)) {
			return nil
		}
		if err != nil {
			return err
		}
		if existing.SessionID != "" {
			boundID := domain.SessionID(existing.SessionID)
			bound, err := q.GetSession(ctx, boundID)
			if err == nil {
				matches, matchErr := trackerIntakeActiveSessionMatches(ctx, q, claim, boundID)
				if matchErr != nil {
					return matchErr
				}
				if matches && !bound.IsTerminated {
					return reconcileTrackerIntakeSessionAt(ctx, q, claim, boundID, releasedAt)
				}
				if !bound.IsTerminated {
					// An attached admitted seed is still a live fence even when spawn
					// failed before side effects began. Keep it busy until rollback
					// removes the exact row; releasing here could admit a duplicate.
					return nil
				}
			} else if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
		}
		rows, err := q.ReleaseTrackerIntakeClaim(ctx, gen.ReleaseTrackerIntakeClaimParams{
			LeaseExpiresAt: releasedAt, ProjectID: string(claim.ProjectID), Provider: string(claim.Provider), Repo: claim.Repo,
			IssueID: claim.IssueID, OwnerToken: claim.OwnerToken,
		})
		released = rows == 1
		return err
	})
	if err != nil {
		return false, fmt.Errorf("release tracker issue %s/%s/%s/%s: %w", claim.ProjectID, claim.Provider, claim.Repo, claim.IssueID, err)
	}
	return released, nil
}

// RenewTrackerIntakeIssue extends only the current live owner generation.
// A claim already reconciled to completed is also retained: the matching
// admitted session is then the stronger durable fence and the in-flight spawn
// must not be canceled merely because another poll observed it.
func (s *Store) RenewTrackerIntakeIssue(ctx context.Context, claim ports.TrackerIntakeClaim, now, leaseExpiresAt time.Time) (bool, error) {
	if err := validateTrackerIntakeClaim(claim, 1); err != nil || now.IsZero() || !leaseExpiresAt.After(now) {
		return false, fmt.Errorf("invalid tracker intake renewal project=%q", claim.ProjectID)
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	rows, err := s.qw.RenewTrackerIntakeClaim(ctx, gen.RenewTrackerIntakeClaimParams{
		LeaseExpiresAt: leaseExpiresAt, LeaseExpiresAt_2: leaseExpiresAt,
		ProjectID: string(claim.ProjectID), Provider: string(claim.Provider), Repo: claim.Repo,
		IssueID: claim.IssueID, OwnerToken: claim.OwnerToken, LeaseExpiresAt_3: now,
	})
	if err != nil {
		return false, fmt.Errorf("renew tracker issue %s/%s/%s/%s: %w", claim.ProjectID, claim.Provider, claim.Repo, claim.IssueID, err)
	}
	return rows == 1, nil
}

func reconcileTrackerIntakeSession(ctx context.Context, q *gen.Queries, claim ports.TrackerIntakeClaim, sessionID domain.SessionID) error {
	return reconcileTrackerIntakeSessionAt(ctx, q, claim, sessionID, claim.ClaimedAt)
}

func reconcileTrackerIntakeSessionAt(ctx context.Context, q *gen.Queries, claim ports.TrackerIntakeClaim, sessionID domain.SessionID, at time.Time) error {
	params := gen.InsertCompletedTrackerIntakeClaimParams{
		ProjectID: string(claim.ProjectID), Provider: string(claim.Provider), Repo: claim.Repo,
		IssueID: claim.IssueID, OwnerToken: claim.OwnerToken, SessionID: string(sessionID),
		ClaimedAt: at, LeaseExpiresAt: at, CompletedAt: sql.NullTime{Time: at, Valid: true},
	}
	if _, err := q.InsertCompletedTrackerIntakeClaim(ctx, params); err != nil {
		return err
	}
	_, err := q.ReconcileTrackerIntakeClaim(ctx, gen.ReconcileTrackerIntakeClaimParams{
		SessionID: string(sessionID), LeaseExpiresAt: at, CompletedAt: sql.NullTime{Time: at, Valid: true},
		ProjectID: string(claim.ProjectID), Provider: string(claim.Provider), Repo: claim.Repo, IssueID: claim.IssueID,
	})
	return err
}

func findLegacyTrackerIntakeSession(ctx context.Context, q *gen.Queries, claim ports.TrackerIntakeClaim, canonicalIssueID domain.IssueID) (domain.SessionID, error) {
	if claim.Provider == domain.TrackerProviderGitHub {
		return q.FindLegacyGitHubTrackerIntakeSession(ctx, gen.FindLegacyGitHubTrackerIntakeSessionParams{
			ProjectID: claim.ProjectID, IssueID: canonicalIssueID,
		})
	}
	return q.FindLegacyTrackerIntakeSession(ctx, gen.FindLegacyTrackerIntakeSessionParams{
		ProjectID: claim.ProjectID, IssueID: canonicalIssueID,
	})
}

func trackerIntakeActiveSessionMatches(ctx context.Context, q *gen.Queries, claim ports.TrackerIntakeClaim, sessionID domain.SessionID) (bool, error) {
	return q.TrackerIntakeActiveSessionMatches(ctx, gen.TrackerIntakeActiveSessionMatchesParams{
		ID: sessionID, ProjectID: claim.ProjectID,
		CanonicalIssueID: domain.IssueID(string(claim.Provider) + ":" + claim.IssueID), Provider: string(claim.Provider),
	})
}

func expiredTrackerIntakeClaimRecovery(ctx context.Context, q *gen.Queries, claim gen.TrackerIntakeClaim) (safe, deleteSeed bool, err error) {
	if claim.SessionID == "" {
		return false, false, errors.New("admitted tracker intake claim has no bound session")
	}
	id := domain.SessionID(claim.SessionID)
	if _, err := q.GetSession(ctx, id); errors.Is(err, sql.ErrNoRows) {
		// A missing exact bound row proves normal spawn rollback already cleaned
		// the provisional admission. No unknown workspace/runtime can be fenced
		// by this claim anymore, so its expired generation is safe to replace.
		return true, false, nil
	} else if err != nil {
		return false, false, err
	}
	isSeed, err := q.SessionIsSeed(ctx, id)
	if err != nil {
		return false, false, err
	}
	if !isSeed {
		return false, false, fmt.Errorf("expired tracker intake claim references non-seed session %s", id)
	}
	if claim.Status == trackerIntakeAdmitted {
		// The session manager advances admitted -> spawning before the first
		// workspace/runtime side effect, so this exact pure seed is safe to reap.
		return true, true, nil
	}
	// Spawning is fail-closed: side effects may exist even though MarkSpawned has
	// not persisted their handles on the still-pure session row.
	return false, false, nil
}

func deleteExpiredTrackerIntakeSeed(ctx context.Context, conn *sql.Conn, q *gen.Queries, id domain.SessionID) error {
	isSeed, err := q.SessionIsSeed(ctx, id)
	if err != nil {
		return err
	}
	if !isSeed {
		return fmt.Errorf("expired tracker intake seed %s changed before cleanup", id)
	}
	if _, err := conn.ExecContext(ctx, `DELETE FROM change_log WHERE session_id = ?`, id); err != nil {
		return fmt.Errorf("clear expired intake seed change log for %s: %w", id, err)
	}
	res, err := conn.ExecContext(ctx, `
DELETE FROM sessions
WHERE id = ?
  AND is_terminated = 0
  AND workspace_path = ''
  AND runtime_handle_id = ''
  AND agent_session_id = ''
  AND prompt = ''`, id)
	if err != nil {
		return fmt.Errorf("delete expired intake seed %s: %w", id, err)
	}
	rows, err := res.RowsAffected()
	if err != nil || rows != 1 {
		return fmt.Errorf("delete expired intake seed %s affected %d rows: %w", id, rows, err)
	}
	return nil
}

func trackerIntakeClaimKeyParams(claim ports.TrackerIntakeClaim) gen.GetTrackerIntakeClaimParams {
	return gen.GetTrackerIntakeClaimParams{
		ProjectID: string(claim.ProjectID), Provider: string(claim.Provider), Repo: claim.Repo, IssueID: claim.IssueID,
	}
}

func validateTrackerIntakeClaim(claim ports.TrackerIntakeClaim, maxConcurrent int) error {
	if claim.ProjectID == "" || claim.Provider == "" || strings.TrimSpace(claim.Repo) == "" || strings.TrimSpace(claim.IssueID) == "" || claim.OwnerToken == "" || maxConcurrent <= 0 || claim.ClaimedAt.IsZero() || !claim.LeaseExpiresAt.After(claim.ClaimedAt) {
		return fmt.Errorf("invalid tracker intake claim project=%q provider=%q repo=%q issue=%q", claim.ProjectID, claim.Provider, claim.Repo, claim.IssueID)
	}
	if claim.Provider == domain.TrackerProviderGitHub && (claim.Repo != strings.ToLower(strings.TrimSpace(claim.Repo)) || claim.IssueID != strings.ToLower(strings.TrimSpace(claim.IssueID))) {
		return fmt.Errorf("non-canonical GitHub tracker intake claim repo=%q issue=%q", claim.Repo, claim.IssueID)
	}
	return nil
}
