package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
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

	num, err := s.qw.NextSessionNum(ctx, rec.ProjectID)
	if err != nil {
		return domain.SessionRecord{}, fmt.Errorf("next session num for %s: %w", rec.ProjectID, err)
	}
	rec.ID = domain.SessionID(fmt.Sprintf("%s-%d", rec.ProjectID, num))
	if err := s.qw.InsertSession(ctx, recordToInsert(rec, num)); err != nil {
		return domain.SessionRecord{}, fmt.Errorf("insert session %s: %w", rec.ID, err)
	}
	return rec, nil
}

// UpdateSession writes the full mutable state of an existing session. The
// id/project/num/created_at are immutable and not touched here.
func (s *Store) UpdateSession(ctx context.Context, rec domain.SessionRecord) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.qw.UpdateSession(ctx, recordToUpdate(rec))
}

// UpdateSessionLifecycle writes only the facts owned by lifecycle. Lifecycle
// reducers read a snapshot before applying agent-hook and runtime signals; a
// full-row write here could replay stale operational metadata over a targeted
// concurrent update such as SetSessionPreviewURL or SetClaimedPRBranch.
func (s *Store) UpdateSessionLifecycle(ctx context.Context, rec domain.SessionRecord) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	current, err := s.qw.GetSession(ctx, rec.ID)
	if err != nil {
		return fmt.Errorf("get session %s before lifecycle update: %w", rec.ID, err)
	}
	if current.UpdatedAt.After(rec.UpdatedAt) {
		rec.UpdatedAt = current.UpdatedAt
	}
	activity := normalActivity(rec.Activity, rec.UpdatedAt)
	pendingFingerprint, pendingAttempted := pendingSubmitFields(rec, activity.State)
	diagnosticTrigger, diagnosticTail, diagnosticErrorType, diagnosticAt := diagnosticFields(rec.Diagnostic)
	return s.qw.UpdateSessionLifecycle(ctx, gen.UpdateSessionLifecycleParams{
		ActivityState:                  activity.State,
		ActivityLastAt:                 activity.LastActivityAt,
		FirstSignalAt:                  timeToNullTime(rec.FirstSignalAt),
		IsTerminated:                   rec.IsTerminated,
		AgentSessionID:                 rec.Metadata.AgentSessionID,
		PendingSubmitFingerprint:       pendingFingerprint,
		PendingSubmitRecoveryAttempted: pendingAttempted,
		DiagnosticTrigger:              diagnosticTrigger,
		DiagnosticTerminalTail:         diagnosticTail,
		DiagnosticHookErrorType:        diagnosticErrorType,
		DiagnosticCapturedAt:           diagnosticAt,
		MergedCleanupPending:           rec.Metadata.MergedCleanupPending,
		MergedCleanupPRURL:             rec.Metadata.MergedCleanupPRURL,
		UpdatedAt:                      rec.UpdatedAt,
		ID:                             rec.ID,
	})
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
// Returns deleted=true when a seed row was removed; deleted=false when the
// session id did not match a seed row (either it never existed, or it had
// already progressed past seed state). The latter case is benign — the caller
// should fall back to MarkTerminated.
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
	return rowToRecord(row), true, nil
}

// ListSessions returns every session in a project, ordered by num.
func (s *Store) ListSessions(ctx context.Context, project domain.ProjectID) ([]domain.SessionRecord, error) {
	rows, err := s.qr.ListSessionsByProject(ctx, project)
	if err != nil {
		return nil, fmt.Errorf("list sessions for %s: %w", project, err)
	}
	return mapSessionRows(rows), nil
}

// ListAllSessions returns every session across all projects.
func (s *Store) ListAllSessions(ctx context.Context) ([]domain.SessionRecord, error) {
	rows, err := s.qr.ListAllSessions(ctx)
	if err != nil {
		return nil, fmt.Errorf("list all sessions: %w", err)
	}
	return mapSessionRows(rows), nil
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
		FirstSignalAt: nullTimeToTime(row.FirstSignalAt),
		IsTerminated:  row.IsTerminated,
		Diagnostic:    diagnostic,
		Metadata: domain.SessionMetadata{
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
