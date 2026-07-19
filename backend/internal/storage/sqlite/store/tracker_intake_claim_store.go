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

// TrackerIntakeHasCapacity is the cheap pre-list guard used to preserve the
// observer's existing behavior of not calling the tracker while a project is
// full. ClaimTrackerIntakeIssue repeats the check under BEGIN IMMEDIATE, so
// this snapshot is only an optimization and never authorizes a spawn.
func (s *Store) TrackerIntakeHasCapacity(ctx context.Context, projectID domain.ProjectID, maxConcurrent int, now time.Time) (bool, error) {
	if projectID == "" || maxConcurrent <= 0 || now.IsZero() {
		return false, fmt.Errorf("invalid tracker intake capacity project=%q max=%d", projectID, maxConcurrent)
	}
	used, err := s.qr.CountTrackerIntakeCapacityUsed(ctx, gen.CountTrackerIntakeCapacityUsedParams{
		ProjectID: projectID, ProjectID_2: string(projectID), LeaseExpiresAt: now, LeaseExpiresAt_2: now,
	})
	if err != nil {
		return false, fmt.Errorf("count tracker intake capacity for %s: %w", projectID, err)
	}
	return used < int64(maxConcurrent), nil
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
	err := s.inImmediateTx(ctx, "claim tracker intake issue", func(q *gen.Queries) error {
		canonicalIssueID := string(claim.Provider) + ":" + claim.IssueID
		sessionID, sessionErr := q.FindAdmittedTrackerIntakeSession(ctx, gen.FindAdmittedTrackerIntakeSessionParams{
			ProjectID: claim.ProjectID, CanonicalIssueID: domain.IssueID(canonicalIssueID), Provider: string(claim.Provider),
		})
		if sessionErr == nil {
			return reconcileTrackerIntakeSession(ctx, q, claim, sessionID)
		}
		if !errors.Is(sessionErr, sql.ErrNoRows) {
			return sessionErr
		}

		existing, claimErr := q.GetTrackerIntakeClaim(ctx, trackerIntakeClaimKeyParams(claim))
		if claimErr != nil && !errors.Is(claimErr, sql.ErrNoRows) {
			return claimErr
		}
		if claimErr == nil {
			switch {
			case existing.Status == trackerIntakeCompleted:
				return nil
			case existing.Status == trackerIntakeAdmitted && !existing.LeaseExpiresAt.After(claim.ClaimedAt):
				safe, err := expiredAdmittedTrackerIntakeClaimIsSafe(ctx, q, existing)
				if err != nil {
					return err
				}
				if !safe {
					result = ports.TrackerIntakeClaimBusy
					return nil
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
			LeaseExpiresAt: claim.ClaimedAt, LeaseExpiresAt_2: claim.ClaimedAt,
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
		matches, err := q.TrackerIntakeSessionMatches(ctx, gen.TrackerIntakeSessionMatchesParams{
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
			IssueID: claim.IssueID, OwnerToken: claim.OwnerToken,
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
		canonicalIssueID := domain.IssueID(string(claim.Provider) + ":" + claim.IssueID)
		sessionID, err := q.FindAdmittedTrackerIntakeSession(ctx, gen.FindAdmittedTrackerIntakeSessionParams{
			ProjectID: claim.ProjectID, CanonicalIssueID: canonicalIssueID, Provider: string(claim.Provider),
		})
		if err == nil {
			return reconcileTrackerIntakeSessionAt(ctx, q, claim, sessionID, releasedAt)
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		rows, err := q.ReleaseTrackerIntakeClaim(ctx, gen.ReleaseTrackerIntakeClaimParams{
			ProjectID: string(claim.ProjectID), Provider: string(claim.Provider), Repo: claim.Repo,
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

func expiredAdmittedTrackerIntakeClaimIsSafe(ctx context.Context, q *gen.Queries, claim gen.TrackerIntakeClaim) (bool, error) {
	if claim.SessionID == "" {
		return false, errors.New("admitted tracker intake claim has no bound session")
	}
	id := domain.SessionID(claim.SessionID)
	if _, err := q.GetSession(ctx, id); errors.Is(err, sql.ErrNoRows) {
		// A missing exact bound row proves normal spawn rollback already cleaned
		// the provisional admission. No unknown workspace/runtime can be fenced
		// by this claim anymore, so its expired generation is safe to replace.
		return true, nil
	} else if err != nil {
		return false, err
	}
	isSeed, err := q.SessionIsSeed(ctx, id)
	if err != nil {
		return false, err
	}
	if !isSeed {
		return false, fmt.Errorf("expired tracker intake claim references non-seed session %s", id)
	}
	// A surviving provisional row is fail-closed: workspace/runtime side effects
	// may already exist even though MarkSpawned has not persisted their handles.
	return false, nil
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
