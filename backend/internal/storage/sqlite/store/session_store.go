package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/gen"
)

// ---- sessions ----

// CreateSession assigns the per-project identity ("{project}-{num}") and inserts
// the record, returning it with ID populated. The next-num read and the insert
// run on the writer connection under writeMu, so two concurrent creates in the
// same project can't collide on num.
func (s *Store) CreateSession(ctx context.Context, rec domain.SessionRecord) (domain.SessionRecord, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	rawDeps, err := domain.DecodeSessionDependencyIDs(rec.DependencyIDs)
	if err != nil {
		return domain.SessionRecord{}, fmt.Errorf("create session dependencies: %w", err)
	}
	deps, err := normalizeDependencies(rawDeps)
	if err != nil {
		return domain.SessionRecord{}, err
	}
	tx, err := s.writeDB.BeginTx(ctx, nil)
	if err != nil {
		return domain.SessionRecord{}, fmt.Errorf("begin create session: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	q := s.qw.WithTx(tx)
	num, err := q.NextSessionNum(ctx, rec.ProjectID)
	if err != nil {
		return domain.SessionRecord{}, fmt.Errorf("next session num for %s: %w", rec.ProjectID, err)
	}
	rec.ID = domain.SessionID(fmt.Sprintf("%s-%d", rec.ProjectID, num))
	if !rec.DependencyPreparedAt.IsZero() && rec.Metadata.Branch == "" && rec.DependencyBranchPrefix != "" {
		rec.Metadata.Branch = rec.DependencyBranchPrefix + string(rec.ID) + rec.DependencyBranchSuffix
	}
	if err := validateDependencies(ctx, tx, rec.ID, rec.ProjectID, deps); err != nil {
		return domain.SessionRecord{}, err
	}
	if err := q.InsertSession(ctx, recordToInsert(rec, num)); err != nil {
		return domain.SessionRecord{}, fmt.Errorf("insert session %s: %w", rec.ID, err)
	}
	for _, dependencyID := range deps {
		if err := q.InsertSessionDependency(ctx, gen.InsertSessionDependencyParams{SessionID: rec.ID, DependsOnSessionID: dependencyID}); err != nil {
			return domain.SessionRecord{}, fmt.Errorf("insert dependency %s -> %s: %w", rec.ID, dependencyID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return domain.SessionRecord{}, fmt.Errorf("commit create session: %w", err)
	}
	rec.DependencyIDs = domain.EncodeSessionDependencyIDs(deps)
	return rec, nil
}

// CreateClaimedSession atomically fences tracker-intake admission against the
// exact live claim generation before inserting the provisional session seed.
// A lease takeover committed first makes this a no-op. The session manager must
// durably advance the attached seed to spawning before any external side effect;
// only a seed that never crossed that fence may be reaped after lease expiry.
func (s *Store) CreateClaimedSession(ctx context.Context, rec domain.SessionRecord, claim ports.TrackerIntakeClaim, admittedAt time.Time) (domain.SessionRecord, error) {
	if rec.ProjectID != claim.ProjectID || rec.IssueID != domain.IssueID(string(claim.Provider)+":"+claim.IssueID) || admittedAt.IsZero() {
		return domain.SessionRecord{}, fmt.Errorf("create claimed session: %w", ports.ErrTrackerIntakeClaimLost)
	}
	rawDeps, err := domain.DecodeSessionDependencyIDs(rec.DependencyIDs)
	if err != nil {
		return domain.SessionRecord{}, fmt.Errorf("create session dependencies: %w", err)
	}
	deps, err := normalizeDependencies(rawDeps)
	if err != nil {
		return domain.SessionRecord{}, err
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	err = s.inImmediateConnTx(ctx, "create claimed session", func(conn *sql.Conn, q *gen.Queries) error {
		owned, err := q.TrackerIntakeClaimOwned(ctx, gen.TrackerIntakeClaimOwnedParams{
			ProjectID: string(claim.ProjectID), Provider: string(claim.Provider), Repo: claim.Repo,
			IssueID: claim.IssueID, OwnerToken: claim.OwnerToken, LeaseExpiresAt: admittedAt,
		})
		if err != nil {
			return err
		}
		if !owned {
			return ports.ErrTrackerIntakeClaimLost
		}
		num, err := q.NextSessionNum(ctx, rec.ProjectID)
		if err != nil {
			return fmt.Errorf("next session num for %s: %w", rec.ProjectID, err)
		}
		rec.ID = domain.SessionID(fmt.Sprintf("%s-%d", rec.ProjectID, num))
		if !rec.DependencyPreparedAt.IsZero() && rec.Metadata.Branch == "" && rec.DependencyBranchPrefix != "" {
			rec.Metadata.Branch = rec.DependencyBranchPrefix + string(rec.ID) + rec.DependencyBranchSuffix
		}
		if err := validateDependencies(ctx, conn, rec.ID, rec.ProjectID, deps); err != nil {
			return err
		}
		if err := q.InsertSession(ctx, recordToInsert(rec, num)); err != nil {
			return fmt.Errorf("insert session %s: %w", rec.ID, err)
		}
		attached, err := q.AttachTrackerIntakeClaimSeed(ctx, gen.AttachTrackerIntakeClaimSeedParams{
			SessionID: string(rec.ID), ProjectID: string(claim.ProjectID), Provider: string(claim.Provider), Repo: claim.Repo,
			IssueID: claim.IssueID, OwnerToken: claim.OwnerToken, LeaseExpiresAt: admittedAt,
		})
		if err != nil {
			return err
		}
		if attached != 1 {
			return ports.ErrTrackerIntakeClaimLost
		}
		for _, dependencyID := range deps {
			if err := q.InsertSessionDependency(ctx, gen.InsertSessionDependencyParams{SessionID: rec.ID, DependsOnSessionID: dependencyID}); err != nil {
				return fmt.Errorf("insert dependency %s -> %s: %w", rec.ID, dependencyID, err)
			}
		}
		return nil
	})
	if err != nil {
		return domain.SessionRecord{}, fmt.Errorf("create claimed session: %w", err)
	}
	rec.DependencyIDs = domain.EncodeSessionDependencyIDs(deps)
	return rec, nil
}

type dependencyQueryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func validateDependencies(ctx context.Context, db dependencyQueryer, id domain.SessionID, projectID domain.ProjectID, deps []domain.SessionID) error {
	for _, dependencyID := range deps {
		if dependencyID == id {
			return fmt.Errorf("%w: session %s", ports.ErrDependencySelf, id)
		}
		var dependencyProject domain.ProjectID
		if err := db.QueryRowContext(ctx, `SELECT project_id FROM sessions WHERE id = ?`, dependencyID).Scan(&dependencyProject); errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: session %s depends on unknown session %s", ports.ErrDependencyNotFound, id, dependencyID)
		} else if err != nil {
			return fmt.Errorf("check dependency %s: %w", dependencyID, err)
		}
		if dependencyProject != projectID {
			return fmt.Errorf("%w: session %s in project %s depends on %s in project %s", ports.ErrDependencyProject, id, projectID, dependencyID, dependencyProject)
		}
		var createsCycle bool
		if err := db.QueryRowContext(ctx, `
WITH RECURSIVE ancestors(ancestor_id) AS (
    SELECT depends_on_session_id FROM session_dependencies WHERE session_id = ?
    UNION
    SELECT dependency.depends_on_session_id
    FROM session_dependencies dependency
    JOIN ancestors ON dependency.session_id = ancestors.ancestor_id
)
SELECT EXISTS(SELECT 1 FROM ancestors WHERE ancestor_id = ?)`, dependencyID, id).Scan(&createsCycle); err != nil {
			return fmt.Errorf("check dependency cycle %s -> %s: %w", id, dependencyID, err)
		}
		if createsCycle {
			return fmt.Errorf("%w: adding %s -> %s", ports.ErrDependencyCycle, id, dependencyID)
		}
	}
	return nil
}

func normalizeDependencies(deps []domain.SessionID) ([]domain.SessionID, error) {
	if len(deps) > domain.MaxSessionDependencies {
		return nil, fmt.Errorf("%w: got %d, maximum is %d", ports.ErrDependencyLimit, len(deps), domain.MaxSessionDependencies)
	}
	seen := make(map[domain.SessionID]struct{}, len(deps))
	out := make([]domain.SessionID, 0, len(deps))
	for _, id := range deps {
		id = domain.SessionID(strings.TrimSpace(string(id)))
		if id == "" || strings.ContainsRune(string(id), '\x00') || strings.IndexFunc(string(id), unicode.IsSpace) >= 0 {
			return nil, fmt.Errorf("%w: %q", ports.ErrDependencyInvalid, id)
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out, nil
}

// UpdateSession writes the full mutable state of an existing session. The
// id/project/num/created_at are immutable and not touched here.
func (s *Store) UpdateSession(ctx context.Context, rec domain.SessionRecord) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.qw.UpdateSession(ctx, recordToUpdate(rec))
}

// UpdateSessionLifecycle persists one explicit reducer transition. The core
// lifecycle facts are merged against the current writer snapshot; auxiliary
// durable metadata is compare-and-set only when before/after proves the
// reducer intentionally changed it. Generic hook/runtime writes therefore
// cannot replay stale pending-submit or merged-cleanup latches.
func (s *Store) UpdateSessionLifecycle(ctx context.Context, before, after domain.SessionRecord) error {
	if before.ID == "" || before.ID != after.ID {
		return fmt.Errorf("lifecycle update session id mismatch: before=%q after=%q", before.ID, after.ID)
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.inTx(ctx, "update session lifecycle", func(q *gen.Queries) error {
		row, err := q.GetSession(ctx, after.ID)
		if err != nil {
			return fmt.Errorf("get session %s: %w", after.ID, err)
		}
		current := rowToRecord(row)
		core := current
		if lifecycleActivityEqual(current.Activity, before.Activity) && !lifecycleActivityEqual(before.Activity, after.Activity) {
			core.Activity = after.Activity
		}
		if current.FirstSignalAt.Equal(before.FirstSignalAt) && !before.FirstSignalAt.Equal(after.FirstSignalAt) {
			core.FirstSignalAt = after.FirstSignalAt
		}
		if current.IsTerminated == before.IsTerminated && before.IsTerminated != after.IsTerminated {
			core.IsTerminated = after.IsTerminated
		}
		if lifecycleDiagnosticEqual(current.Diagnostic, before.Diagnostic) && !lifecycleDiagnosticEqual(before.Diagnostic, after.Diagnostic) {
			core.Diagnostic = after.Diagnostic
		}
		core.UpdatedAt = after.UpdatedAt
		if current.UpdatedAt.After(core.UpdatedAt) {
			core.UpdatedAt = current.UpdatedAt
		}
		activity := normalActivity(core.Activity, core.UpdatedAt)
		diagnosticTrigger, diagnosticTail, diagnosticErrorType, diagnosticAt := diagnosticFields(core.Diagnostic)
		if err := q.UpdateSessionLifecycle(ctx, gen.UpdateSessionLifecycleParams{
			ActivityState:           activity.State,
			ActivityLastAt:          activity.LastActivityAt,
			FirstSignalAt:           timeToNullTime(core.FirstSignalAt),
			IsTerminated:            core.IsTerminated,
			DiagnosticTrigger:       diagnosticTrigger,
			DiagnosticTerminalTail:  diagnosticTail,
			DiagnosticHookErrorType: diagnosticErrorType,
			DiagnosticCapturedAt:    diagnosticAt,
			UpdatedAt:               core.UpdatedAt,
			ID:                      core.ID,
		}); err != nil {
			return err
		}

		if before.Metadata.AgentSessionID != after.Metadata.AgentSessionID {
			if _, err := q.UpdateSessionLifecycleAgentID(ctx, gen.UpdateSessionLifecycleAgentIDParams{
				AgentSessionID:   after.Metadata.AgentSessionID,
				ID:               after.ID,
				AgentSessionID_2: before.Metadata.AgentSessionID,
			}); err != nil {
				return err
			}
		}
		if !lifecyclePendingSubmitEqual(before.Metadata, after.Metadata) {
			if _, err := q.UpdateSessionLifecyclePendingSubmit(ctx, gen.UpdateSessionLifecyclePendingSubmitParams{
				PendingSubmitFingerprint:         after.Metadata.PendingSubmitFingerprint,
				PendingSubmitRecoveryAttempted:   after.Metadata.PendingSubmitRecoveryAttempted,
				ID:                               after.ID,
				PendingSubmitFingerprint_2:       before.Metadata.PendingSubmitFingerprint,
				PendingSubmitRecoveryAttempted_2: before.Metadata.PendingSubmitRecoveryAttempted,
			}); err != nil {
				return err
			}
		}
		if !lifecycleMergedCleanupEqual(before.Metadata, after.Metadata) {
			if _, err := q.UpdateSessionLifecycleMergedCleanup(ctx, gen.UpdateSessionLifecycleMergedCleanupParams{
				MergedCleanupPending:   after.Metadata.MergedCleanupPending,
				MergedCleanupPRURL:     after.Metadata.MergedCleanupPRURL,
				ID:                     after.ID,
				MergedCleanupPending_2: before.Metadata.MergedCleanupPending,
				MergedCleanupPRURL_2:   before.Metadata.MergedCleanupPRURL,
			}); err != nil {
				return err
			}
		}
		return nil
	})
}

func lifecycleActivityEqual(a, b domain.Activity) bool {
	return a.State == b.State && a.LastActivityAt.Equal(b.LastActivityAt)
}

func lifecycleDiagnosticEqual(a, b *domain.LifecycleDiagnostic) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Trigger == b.Trigger && a.TerminalTail == b.TerminalTail && a.HookErrorType == b.HookErrorType && a.CapturedAt.Equal(b.CapturedAt)
}

func lifecyclePendingSubmitEqual(a, b domain.SessionMetadata) bool {
	return a.PendingSubmitFingerprint == b.PendingSubmitFingerprint && a.PendingSubmitRecoveryAttempted == b.PendingSubmitRecoveryAttempted
}

func lifecycleMergedCleanupEqual(a, b domain.SessionMetadata) bool {
	return a.MergedCleanupPending == b.MergedCleanupPending && a.MergedCleanupPRURL == b.MergedCleanupPRURL
}

// RenameSession updates only the user-facing display name for an existing
// session. It returns ok=false when the session id does not exist.
func (s *Store) RenameSession(ctx context.Context, id domain.SessionID, displayName string, updatedAt time.Time) (bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	rows, err := s.qw.RenameSession(ctx, gen.RenameSessionParams{
		ID:          id,
		DisplayName: displayName,
		UpdatedAt:   updatedAt,
	})
	if err != nil {
		return false, fmt.Errorf("rename session %s: %w", id, err)
	}
	return rows > 0, nil
}

// SetSessionPreviewURL updates only the browser preview URL for an existing
// session. It returns ok=false when the session id does not exist. The
// sessions_cdc_update trigger fans out a session_updated CDC event when the
// preview URL actually changes.
func (s *Store) SetSessionPreviewURL(ctx context.Context, id domain.SessionID, previewURL string, updatedAt time.Time) (bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	rows, err := s.qw.SetSessionPreviewURL(ctx, gen.SetSessionPreviewURLParams{
		ID:         id,
		PreviewURL: previewURL,
		UpdatedAt:  updatedAt,
	})
	if err != nil {
		return false, fmt.Errorf("set preview url for session %s: %w", id, err)
	}
	return rows > 0, nil
}

// SetPendingSubmit latches a delivered prompt fingerprint before any
// Enter-only recovery is attempted. It returns ok=false for a missing or
// already-terminated session.
func (s *Store) SetPendingSubmit(ctx context.Context, id domain.SessionID, fingerprint string, updatedAt time.Time) (bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	rows, err := s.qw.SetPendingSubmit(ctx, gen.SetPendingSubmitParams{
		PendingSubmitFingerprint: fingerprint,
		UpdatedAt:                updatedAt,
		ID:                       id,
	})
	if err != nil {
		return false, fmt.Errorf("set pending submit for session %s: %w", id, err)
	}
	return rows > 0, nil
}

// ClaimPendingSubmitRecovery atomically records the one permitted Enter-only
// recovery before the pane write. A concurrent caller, daemon restart, or a
// decision/termination transition therefore cannot claim the same recovery.
func (s *Store) ClaimPendingSubmitRecovery(ctx context.Context, id domain.SessionID, fingerprint string, updatedAt time.Time) (bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	rows, err := s.qw.ClaimPendingSubmitRecovery(ctx, gen.ClaimPendingSubmitRecoveryParams{
		UpdatedAt:                updatedAt,
		ID:                       id,
		PendingSubmitFingerprint: fingerprint,
	})
	if err != nil {
		return false, fmt.Errorf("claim pending submit recovery for session %s: %w", id, err)
	}
	return rows > 0, nil
}

// ClearPendingSubmit consumes only the matching latch, so delayed confirmation
// from an older send cannot clear a newer prompt's delivery boundary.
func (s *Store) ClearPendingSubmit(ctx context.Context, id domain.SessionID, fingerprint string, updatedAt time.Time) (bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	rows, err := s.qw.ClearPendingSubmit(ctx, gen.ClearPendingSubmitParams{
		UpdatedAt:                updatedAt,
		ID:                       id,
		PendingSubmitFingerprint: fingerprint,
	})
	if err != nil {
		return false, fmt.Errorf("clear pending submit for session %s: %w", id, err)
	}
	return rows > 0, nil
}

// DeleteSession removes a session row, but only if it is still in seed state
// (no workspace, no runtime handle, no agent session id, no prompt, and not
// already terminated). Rows that have observable spawn output are immutable
// to preserve the no-resurrection guarantee — for those, callers fall back to
// MarkTerminated (lifecycle.Manager) instead.
//
// The deletion runs in a transaction. It first probes seed state with
// SessionIsSeed; only if that returns true does it clear the session's
// change_log rows (required because change_log FKs sessions(id) without
// ON DELETE CASCADE) and then delete the session row. For live or absent
// sessions the transaction commits with no rows touched — critically, the
// session_created / session_updated CDC events for live sessions are NOT
// destroyed when callers (e.g. RollbackSpawn's delete-then-kill fallback)
// invoke DeleteSession on a fully-spawned row.
//
// A seed referenced as another session's dependency parent is protected by the
// parent-side RESTRICT FK. That delete returns an error so spawn rollback parks
// the parent terminal, preserving both the parent fact and the committed edge.
// Returns deleted=true when an unreferenced seed row was removed; deleted=false
// when the session id did not match a seed row (either it never existed, or it
// had already progressed past seed state). The latter case is benign — the
// caller should fall back to MarkTerminated.
func (s *Store) DeleteSession(ctx context.Context, id domain.SessionID) (bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.writeDB.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin delete seed session: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	q := s.qw.WithTx(tx)

	isSeed, err := q.SessionIsSeed(ctx, id)
	if err != nil {
		return false, fmt.Errorf("delete seed session: probe seed state for %s: %w", id, err)
	}
	if !isSeed {
		// Commit the empty tx so we don't leak a transaction. Critically, do
		// NOT touch change_log here — for a live session that contains real
		// session_created / session_updated CDC events.
		if err := tx.Commit(); err != nil {
			return false, fmt.Errorf("delete seed session: commit no-op: %w", err)
		}
		return false, nil
	}

	// Drop change_log rows for this session id first so the FK doesn't reject
	// the session DELETE. We do not touch project-level events (session_id IS
	// NULL) — those belong to the project, not this session. Both this DELETE
	// and the session DELETE below run via raw ExecContext to sidestep sqlc
	// 1.31's SQLite-parser bug, which strips trailing `?` placeholders and
	// string literals from DELETE statements (see queries/changelog.sql and
	// queries/sessions.sql for the documented workaround context).
	if _, err := tx.ExecContext(ctx, `DELETE FROM change_log WHERE session_id = ?`, id); err != nil {
		return false, fmt.Errorf("delete seed session: clear change log for %s: %w", id, err)
	}
	res, err := tx.ExecContext(ctx, `
DELETE FROM sessions
WHERE id = ?
  AND is_terminated = 0
  AND workspace_path = ''
  AND runtime_handle_id = ''
  AND agent_session_id = ''
  AND prompt = ''`, id)
	if err != nil {
		return false, fmt.Errorf("delete seed session %s: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("delete seed session %s: rows affected: %w", id, err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("delete seed session: commit: %w", err)
	}
	return n > 0, nil
}

// GetSession returns the full record for a session, or ok=false if absent.
func (s *Store) GetSession(ctx context.Context, id domain.SessionID) (domain.SessionRecord, bool, error) {
	row, err := s.qr.GetSession(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.SessionRecord{}, false, nil
	}
	if err != nil {
		return domain.SessionRecord{}, false, fmt.Errorf("get session %s: %w", id, err)
	}
	rec := rowToRecord(row)
	deps, err := s.qr.ListSessionDependencies(ctx, id)
	if err != nil {
		return domain.SessionRecord{}, false, fmt.Errorf("list dependencies for %s: %w", id, err)
	}
	rec.DependencyIDs = domain.EncodeSessionDependencyIDs(deps)
	return rec, true, nil
}

// ListSessions returns every session in a project, ordered by num.
func (s *Store) ListSessions(ctx context.Context, project domain.ProjectID) ([]domain.SessionRecord, error) {
	rows, err := s.qr.ListSessionsByProject(ctx, project)
	if err != nil {
		return nil, fmt.Errorf("list sessions for %s: %w", project, err)
	}
	return s.withDependencies(ctx, mapSessionRows(rows))
}

// ListAllSessions returns every session across all projects.
func (s *Store) ListAllSessions(ctx context.Context) ([]domain.SessionRecord, error) {
	rows, err := s.qr.ListAllSessions(ctx)
	if err != nil {
		return nil, fmt.Errorf("list all sessions: %w", err)
	}
	return s.withDependencies(ctx, mapSessionRows(rows))
}

func (s *Store) withDependencies(ctx context.Context, records []domain.SessionRecord) ([]domain.SessionRecord, error) {
	edges, err := s.qr.ListAllSessionDependencies(ctx)
	if err != nil {
		return nil, fmt.Errorf("list session dependencies: %w", err)
	}
	bySession := make(map[domain.SessionID][]domain.SessionID)
	for _, edge := range edges {
		bySession[edge.SessionID] = append(bySession[edge.SessionID], edge.DependsOnSessionID)
	}
	for i := range records {
		records[i].DependencyIDs = domain.EncodeSessionDependencyIDs(bySession[records[i].ID])
	}
	return records, nil
}

// ListReadyDependencySessions returns unclaimed children for which every
// parent has an explicit handoff or terminal merged-PR completion fact.
func (s *Store) ListReadyDependencySessions(ctx context.Context) ([]domain.SessionID, error) {
	ids, err := s.qr.ListReadyDependencySessions(ctx)
	if err != nil {
		return nil, fmt.Errorf("list ready dependency sessions: %w", err)
	}
	return ids, nil
}

// ReserveDependencyPromotion atomically fences one launch attempt with token.
func (s *Store) ReserveDependencyPromotion(ctx context.Context, id domain.SessionID, token string, claimedAt time.Time) (bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	rows, err := s.qw.ReserveDependencyPromotion(ctx, gen.ReserveDependencyPromotionParams{
		DependencyPromotionToken:     token,
		DependencyPromotionClaimedAt: timeToNullTime(claimedAt),
		UpdatedAt:                    claimedAt,
		ID:                           id,
	})
	if err != nil {
		return false, fmt.Errorf("reserve dependency promotion for %s: %w", id, err)
	}
	return rows > 0, nil
}

// CompleteDependencyPromotion consumes only the matching reservation. The DB
// trigger emits one activity event atomically with this successful completion.
func (s *Store) CompleteDependencyPromotion(ctx context.Context, id domain.SessionID, token string, promotedAt time.Time) (bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	rows, err := s.qw.CompleteDependencyPromotion(ctx, gen.CompleteDependencyPromotionParams{
		DependencyPromotedAt:     timeToNullTime(promotedAt),
		UpdatedAt:                promotedAt,
		ID:                       id,
		DependencyPromotionToken: token,
	})
	if err != nil {
		return false, fmt.Errorf("complete dependency promotion for %s: %w", id, err)
	}
	return rows > 0, nil
}

// ReleaseDependencyPromotion makes a failed, fenced launch retryable without
// accepting a stale poller's token.
func (s *Store) ReleaseDependencyPromotion(ctx context.Context, id domain.SessionID, token string, updatedAt time.Time) (bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	rows, err := s.qw.ReleaseDependencyPromotion(ctx, gen.ReleaseDependencyPromotionParams{UpdatedAt: updatedAt, ID: id, DependencyPromotionToken: token})
	if err != nil {
		return false, fmt.Errorf("release dependency promotion for %s: %w", id, err)
	}
	return rows > 0, nil
}

// RecoverDependencyPromotions clears abandoned reservations. The daemon calls
// this only after acquiring the process-wide exclusive SQLite coordination
// lease, so no prior owner can still be launching the fenced children.
func (s *Store) RecoverDependencyPromotions(ctx context.Context, updatedAt time.Time) (int64, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	rows, err := s.qw.RecoverDependencyPromotions(ctx, updatedAt)
	if err != nil {
		return 0, fmt.Errorf("recover dependency promotions: %w", err)
	}
	return rows, nil
}

// RecoverStaleDependencyPromotions releases expired reservations that have not crossed the runtime boundary.
func (s *Store) RecoverStaleDependencyPromotions(ctx context.Context, updatedAt, staleBefore time.Time) (int64, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	rows, err := s.qw.RecoverStaleDependencyPromotions(ctx, gen.RecoverStaleDependencyPromotionsParams{
		UpdatedAt:                    updatedAt,
		DependencyPromotionClaimedAt: timeToNullTime(staleBefore),
	})
	if err != nil {
		return 0, fmt.Errorf("recover stale dependency promotions: %w", err)
	}
	return rows, nil
}

// MarkReservedDependencySpawned is the narrow persistence CAS used only by
// lifecycle.Manager.MarkDependencySpawned. It cannot clear termination or
// overwrite unrelated lifecycle metadata from a stale session snapshot.
func (s *Store) MarkReservedDependencySpawned(ctx context.Context, id domain.SessionID, token string, metadata domain.SessionMetadata, updatedAt time.Time) (bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	rows, err := s.qw.MarkReservedDependencySpawned(ctx, gen.MarkReservedDependencySpawnedParams{
		WorkspaceKind:            string(metadata.WorkspaceKind.WithDefault()),
		Branch:                   metadata.Branch,
		WorkspacePath:            metadata.WorkspacePath,
		RuntimeHandleID:          metadata.RuntimeHandleID,
		Prompt:                   metadata.Prompt,
		ActivityLastAt:           updatedAt,
		UpdatedAt:                updatedAt,
		ID:                       id,
		DependencyPromotionToken: token,
	})
	if err != nil {
		return false, fmt.Errorf("mark reserved dependency %s spawned: %w", id, err)
	}
	return rows > 0, nil
}

// PrepareReservedDependencyWorkspace atomically persists the deterministic
// workspace/runtime ownership claim and every multi-repo cleanup row before
// any external workspace creation begins.
func (s *Store) PrepareReservedDependencyWorkspace(ctx context.Context, id domain.SessionID, token string, metadata domain.SessionMetadata, worktrees []domain.SessionWorktreeRecord, updatedAt time.Time) (bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.writeDB.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin prepare dependency workspace %s: %w", id, err)
	}
	defer func() { _ = tx.Rollback() }()
	q := s.qw.WithTx(tx)
	rows, err := q.MarkReservedDependencySpawned(ctx, gen.MarkReservedDependencySpawnedParams{
		WorkspaceKind:            string(metadata.WorkspaceKind.WithDefault()),
		Branch:                   metadata.Branch,
		WorkspacePath:            metadata.WorkspacePath,
		RuntimeHandleID:          metadata.RuntimeHandleID,
		Prompt:                   metadata.Prompt,
		ActivityLastAt:           updatedAt,
		UpdatedAt:                updatedAt,
		ID:                       id,
		DependencyPromotionToken: token,
	})
	if err != nil {
		return false, fmt.Errorf("prepare reserved dependency workspace %s: %w", id, err)
	}
	if rows != 1 {
		return false, nil
	}
	for _, row := range worktrees {
		state := row.State
		if state == "" {
			state = "active"
		}
		if err := q.UpsertSessionWorktree(ctx, gen.UpsertSessionWorktreeParams{SessionID: id, RepoName: row.RepoName, Branch: row.Branch, BaseSha: row.BaseSHA, WorktreePath: row.WorktreePath, PreservedRef: row.PreservedRef, State: state}); err != nil {
			return false, fmt.Errorf("prepare dependency workspace %s repo %s: %w", id, row.RepoName, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit prepare dependency workspace %s: %w", id, err)
	}
	return true, nil
}

// MarkReservedDependencyLaunchSucceeded commits the external-launch boundary
// only after runtime creation and prompt delivery have both succeeded. Recovery
// never adopts a runtime-backed reservation without this marker.
func (s *Store) MarkReservedDependencyLaunchSucceeded(ctx context.Context, id domain.SessionID, token string, succeededAt time.Time) (bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	rows, err := s.qw.MarkReservedDependencyLaunchSucceeded(ctx, gen.MarkReservedDependencyLaunchSucceededParams{
		DependencyLaunchSucceededAt: timeToNullTime(succeededAt),
		UpdatedAt:                   succeededAt,
		ID:                          id,
		DependencyPromotionToken:    token,
	})
	if err != nil {
		return false, fmt.Errorf("mark reserved dependency %s launch succeeded: %w", id, err)
	}
	return rows > 0, nil
}

// ResetReservedDependencyLaunch clears launch-owned state while retaining the prepared child for retry.
func (s *Store) ResetReservedDependencyLaunch(ctx context.Context, id domain.SessionID, token string, updatedAt time.Time) (bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	rows, err := s.qw.ResetReservedDependencyLaunch(ctx, gen.ResetReservedDependencyLaunchParams{
		ActivityLastAt:           updatedAt,
		UpdatedAt:                updatedAt,
		ID:                       id,
		DependencyPromotionToken: token,
	})
	if err != nil {
		return false, fmt.Errorf("reset reserved dependency launch for %s: %w", id, err)
	}
	return rows > 0, nil
}

// ListDependencyHandoffs returns ordered prerequisite completion context for a
// child. Missing payloads are preserved as nil (merged-PR completion).
func (s *Store) ListDependencyHandoffs(ctx context.Context, id domain.SessionID) ([]domain.DependencyHandoff, error) {
	rows, err := s.qr.ListDependencyHandoffPayloads(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("list dependency handoffs for %s: %w", id, err)
	}
	out := make([]domain.DependencyHandoff, 0, len(rows))
	for _, row := range rows {
		item := domain.DependencyHandoff{SessionID: row.DependsOnSessionID}
		if row.Payload != "" {
			handoff, err := domain.DecodeAgentHandoff(row.Payload)
			if err != nil {
				return nil, fmt.Errorf("decode dependency handoff for %s: %w", row.DependsOnSessionID, err)
			}
			item.Handoff = &handoff
		}
		out = append(out, item)
	}
	return out, nil
}

func mapSessionRows(rows []gen.Session) []domain.SessionRecord {
	out := make([]domain.SessionRecord, 0, len(rows))
	for _, r := range rows {
		out = append(out, rowToRecord(r))
	}
	return out
}

func rowToRecord(row gen.Session) domain.SessionRecord {
	diagnostic := rowDiagnostic(row.DiagnosticTrigger, row.DiagnosticTerminalTail, row.DiagnosticHookErrorType, row.DiagnosticCapturedAt)
	return domain.SessionRecord{
		ID:          row.ID,
		ProjectID:   row.ProjectID,
		IssueID:     row.IssueID,
		Kind:        row.Kind,
		Harness:     row.Harness,
		DisplayName: row.DisplayName,
		Activity: domain.Activity{
			State:          row.ActivityState,
			LastActivityAt: row.ActivityLastAt,
		},
		FirstSignalAt:                nullTimeToTime(row.FirstSignalAt),
		IsTerminated:                 row.IsTerminated,
		DependencyPromotedAt:         nullTimeToTime(row.DependencyPromotedAt),
		DependencyPreparedAt:         nullTimeToTime(row.DependencyPreparedAt),
		DependencyBasePrompt:         row.DependencyBasePrompt,
		DependencyPromotionToken:     row.DependencyPromotionToken,
		DependencyPromotionClaimedAt: nullTimeToTime(row.DependencyPromotionClaimedAt),
		DependencyLaunchSucceededAt:  nullTimeToTime(row.DependencyLaunchSucceededAt),
		Diagnostic:                   diagnostic,
		Metadata: domain.SessionMetadata{
			WorkspaceKind:                  domain.WorkspaceKind(row.WorkspaceKind),
			Branch:                         row.Branch,
			WorkspacePath:                  row.WorkspacePath,
			RuntimeHandleID:                row.RuntimeHandleID,
			AgentSessionID:                 row.AgentSessionID,
			Prompt:                         row.Prompt,
			PreviewURL:                     row.PreviewURL,
			PreviewRevision:                row.PreviewRevision,
			PendingSubmitFingerprint:       row.PendingSubmitFingerprint,
			PendingSubmitRecoveryAttempted: row.PendingSubmitRecoveryAttempted,
			MergedCleanupPending:           row.MergedCleanupPending,
			MergedCleanupPRURL:             row.MergedCleanupPRURL,
		},
		CreatedAt: row.CreatedAt,
		UpdatedAt: row.UpdatedAt,
	}
}

func recordToInsert(rec domain.SessionRecord, num int64) gen.InsertSessionParams {
	activity := normalActivity(rec.Activity, rec.CreatedAt)
	pendingFingerprint, pendingAttempted := pendingSubmitFields(rec, activity.State)
	diagnosticTrigger, diagnosticTail, diagnosticErrorType, diagnosticAt := diagnosticFields(rec.Diagnostic)
	return gen.InsertSessionParams{
		ID:                             rec.ID,
		ProjectID:                      rec.ProjectID,
		Num:                            num,
		IssueID:                        rec.IssueID,
		Kind:                           rec.Kind,
		Harness:                        rec.Harness,
		DisplayName:                    rec.DisplayName,
		ActivityState:                  activity.State,
		ActivityLastAt:                 activity.LastActivityAt,
		FirstSignalAt:                  timeToNullTime(rec.FirstSignalAt),
		IsTerminated:                   rec.IsTerminated,
		WorkspaceKind:                  string(rec.Metadata.WorkspaceKind.WithDefault()),
		Branch:                         rec.Metadata.Branch,
		WorkspacePath:                  rec.Metadata.WorkspacePath,
		RuntimeHandleID:                rec.Metadata.RuntimeHandleID,
		AgentSessionID:                 rec.Metadata.AgentSessionID,
		Prompt:                         rec.Metadata.Prompt,
		PreviewURL:                     rec.Metadata.PreviewURL,
		PreviewRevision:                rec.Metadata.PreviewRevision,
		PendingSubmitFingerprint:       pendingFingerprint,
		PendingSubmitRecoveryAttempted: pendingAttempted,
		MergedCleanupPending:           rec.Metadata.MergedCleanupPending,
		MergedCleanupPRURL:             rec.Metadata.MergedCleanupPRURL,
		DependencyPreparedAt:           timeToNullTime(rec.DependencyPreparedAt),
		DependencyBasePrompt:           rec.DependencyBasePrompt,
		DiagnosticTrigger:              diagnosticTrigger,
		DiagnosticTerminalTail:         diagnosticTail,
		DiagnosticHookErrorType:        diagnosticErrorType,
		DiagnosticCapturedAt:           diagnosticAt,
		CreatedAt:                      rec.CreatedAt,
		UpdatedAt:                      rec.UpdatedAt,
	}
}

func recordToUpdate(rec domain.SessionRecord) gen.UpdateSessionParams {
	activity := normalActivity(rec.Activity, rec.UpdatedAt)
	pendingFingerprint, pendingAttempted := pendingSubmitFields(rec, activity.State)
	diagnosticTrigger, diagnosticTail, diagnosticErrorType, diagnosticAt := diagnosticFields(rec.Diagnostic)
	return gen.UpdateSessionParams{
		ID:                             rec.ID,
		IssueID:                        rec.IssueID,
		Kind:                           rec.Kind,
		Harness:                        rec.Harness,
		DisplayName:                    rec.DisplayName,
		ActivityState:                  activity.State,
		ActivityLastAt:                 activity.LastActivityAt,
		FirstSignalAt:                  timeToNullTime(rec.FirstSignalAt),
		IsTerminated:                   rec.IsTerminated,
		WorkspaceKind:                  string(rec.Metadata.WorkspaceKind.WithDefault()),
		Branch:                         rec.Metadata.Branch,
		WorkspacePath:                  rec.Metadata.WorkspacePath,
		RuntimeHandleID:                rec.Metadata.RuntimeHandleID,
		AgentSessionID:                 rec.Metadata.AgentSessionID,
		Prompt:                         rec.Metadata.Prompt,
		PreviewURL:                     rec.Metadata.PreviewURL,
		PreviewRevision:                rec.Metadata.PreviewRevision,
		PendingSubmitFingerprint:       pendingFingerprint,
		PendingSubmitRecoveryAttempted: pendingAttempted,
		MergedCleanupPending:           rec.Metadata.MergedCleanupPending,
		MergedCleanupPRURL:             rec.Metadata.MergedCleanupPRURL,
		DiagnosticTrigger:              diagnosticTrigger,
		DiagnosticTerminalTail:         diagnosticTail,
		DiagnosticHookErrorType:        diagnosticErrorType,
		DiagnosticCapturedAt:           diagnosticAt,
		UpdatedAt:                      rec.UpdatedAt,
	}
}

func rowDiagnostic(trigger, tail, errorType string, capturedAt sql.NullTime) *domain.LifecycleDiagnostic {
	if trigger == "" || !capturedAt.Valid {
		return nil
	}
	return &domain.LifecycleDiagnostic{
		Trigger:       domain.DiagnosticTrigger(trigger),
		TerminalTail:  tail,
		HookErrorType: errorType,
		CapturedAt:    capturedAt.Time,
	}
}

func diagnosticFields(diagnostic *domain.LifecycleDiagnostic) (string, string, string, sql.NullTime) {
	if diagnostic == nil || diagnostic.Trigger == "" || diagnostic.CapturedAt.IsZero() {
		return "", "", "", sql.NullTime{}
	}
	return string(diagnostic.Trigger), diagnostic.TerminalTail, diagnostic.HookErrorType, timeToNullTime(diagnostic.CapturedAt)
}

// A terminal or delivery-blocked session is definitive evidence that an editor draft
// can no longer be safely submitted. Clear the latch on every full-row write
// of either fact, regardless of which lifecycle path produced it.
func pendingSubmitFields(rec domain.SessionRecord, activity domain.ActivityState) (string, bool) {
	if rec.IsTerminated || activity.BlocksAutomatedDelivery() {
		return "", false
	}
	return rec.Metadata.PendingSubmitFingerprint, rec.Metadata.PendingSubmitRecoveryAttempted
}

// nullTimeToTime / timeToNullTime bridge the nullable first_signal_at column
// to the domain's zero-time convention (zero = no signal received yet).
func nullTimeToTime(t sql.NullTime) time.Time {
	if !t.Valid {
		return time.Time{}
	}
	return t.Time
}

func timeToNullTime(t time.Time) sql.NullTime {
	if t.IsZero() {
		return sql.NullTime{}
	}
	return sql.NullTime{Time: t, Valid: true}
}

func normalActivity(a domain.Activity, fallback time.Time) domain.Activity {
	if a.State == "" {
		a.State = domain.ActivityIdle
	}
	if a.LastActivityAt.IsZero() {
		a.LastActivityAt = fallback
	}
	if a.LastActivityAt.IsZero() {
		a.LastActivityAt = time.Now().UTC()
	}
	return a
}
