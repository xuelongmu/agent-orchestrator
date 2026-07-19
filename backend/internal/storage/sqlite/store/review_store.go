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

// UpsertReview inserts the per-worker review row, or reuses the existing one
// (session_id is unique) by refreshing its harness/pr_url/updated_at.
func (s *Store) UpsertReview(ctx context.Context, r domain.Review) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.qw.UpsertReview(ctx, gen.UpsertReviewParams{
		ID:               r.ID,
		SessionID:        r.SessionID,
		ProjectID:        r.ProjectID,
		Harness:          r.Harness,
		PRURL:            r.PRURL,
		ReviewerHandleID: r.ReviewerHandleID,
		CreatedAt:        r.CreatedAt,
		UpdatedAt:        r.UpdatedAt,
	})
}

// GetReviewBySession returns the review row for a worker session, ok=false if none.
func (s *Store) GetReviewBySession(ctx context.Context, id domain.SessionID) (domain.Review, bool, error) {
	row, err := s.qr.GetReviewBySession(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Review{}, false, nil
	}
	if err != nil {
		return domain.Review{}, false, fmt.Errorf("get review by session %s: %w", id, err)
	}
	return reviewFromRow(row), true, nil
}

// InsertReviewRun records a new review pass. A unique-constraint hit on the
// (session_id, pr_url, target_sha) index (migration 0020) is surfaced as the sentinel
// domain.ErrDuplicateReviewRun so the engine can fall back to the existing run.
func (s *Store) InsertReviewRun(ctx context.Context, r domain.ReviewRun) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	err := s.qw.InsertReviewRun(ctx, gen.InsertReviewRunParams{
		ID:             r.ID,
		ReviewID:       r.ReviewID,
		SessionID:      r.SessionID,
		BatchID:        r.BatchID,
		Harness:        r.Harness,
		PRURL:          r.PRURL,
		TargetSha:      r.TargetSHA,
		Status:         r.Status,
		Verdict:        r.Verdict,
		Body:           r.Body,
		GithubReviewID: r.GithubReviewID,
		CreatedAt:      r.CreatedAt,
	})
	if isSQLiteUnique(err) {
		return fmt.Errorf("insert review run for session %s pr %s sha %s: %w", r.SessionID, r.PRURL, r.TargetSHA, domain.ErrDuplicateReviewRun)
	}
	return err
}

// UpdateReviewRunResult sets the status/verdict/body and the GitHub review id of
// a running review pass.
func (s *Store) UpdateReviewRunResult(ctx context.Context, id string, status domain.ReviewRunStatus, verdict domain.ReviewVerdict, body, githubReviewID string) (bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	n, err := s.qw.UpdateReviewRunResult(ctx, gen.UpdateReviewRunResultParams{
		Status:         status,
		Verdict:        verdict,
		Body:           body,
		GithubReviewID: githubReviewID,
		ID:             id,
	})
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// SupersedeStaleRunningReviewRuns marks older running unverdicted passes for a
// worker failed before starting a review for a newer commit.
func (s *Store) SupersedeStaleRunningReviewRuns(ctx context.Context, sessionID domain.SessionID, prURL, targetSHA, body string) (int64, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.qw.SupersedeStaleRunningReviewRuns(ctx, gen.SupersedeStaleRunningReviewRunsParams{
		Body:      body,
		SessionID: sessionID,
		PRURL:     prURL,
		TargetSha: targetSHA,
	})
}

// CancelRunningReviewRunsBySession marks all currently running review passes
// for a worker cancelled.
func (s *Store) CancelRunningReviewRunsBySession(ctx context.Context, sessionID domain.SessionID, body string) (int64, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.qw.CancelRunningReviewRunsBySession(ctx, gen.CancelRunningReviewRunsBySessionParams{
		Body:      body,
		SessionID: sessionID,
	})
}

// MarkReviewRunDelivered records that lifecycle delivered the worker nudge for
// a completed AO-internal review pass.
func (s *Store) MarkReviewRunDelivered(ctx context.Context, id string, deliveredAt time.Time) (bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	n, err := s.qw.MarkReviewRunDelivered(ctx, gen.MarkReviewRunDeliveredParams{
		DeliveredAt: sql.NullTime{Time: deliveredAt, Valid: true},
		ID:          id,
	})
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// GetReviewRun returns one review pass by id.
func (s *Store) GetReviewRun(ctx context.Context, id string) (domain.ReviewRun, bool, error) {
	row, err := s.qr.GetReviewRun(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.ReviewRun{}, false, nil
	}
	if err != nil {
		return domain.ReviewRun{}, false, fmt.Errorf("get review run %s: %w", id, err)
	}
	return reviewRunFromRow(row), true, nil
}

// GetReviewRunBySessionPRAndSHA returns the most recent review pass for a
// worker session, PR, and commit, ok=false if none. It lets a repeat trigger for
// the same PR head short-circuit to the existing run without colliding with
// another PR that happens to share the same head commit.
func (s *Store) GetReviewRunBySessionPRAndSHA(ctx context.Context, id domain.SessionID, prURL, targetSHA string) (domain.ReviewRun, bool, error) {
	row, err := s.qr.GetReviewRunBySessionPRAndSHA(ctx, gen.GetReviewRunBySessionPRAndSHAParams{SessionID: id, PRURL: prURL, TargetSha: targetSHA})
	if errors.Is(err, sql.ErrNoRows) {
		return domain.ReviewRun{}, false, nil
	}
	if err != nil {
		return domain.ReviewRun{}, false, fmt.Errorf("get review run for session %s pr %s sha %s: %w", id, prURL, targetSHA, err)
	}
	return reviewRunFromRow(row), true, nil
}

// ListReviewRunsBySession returns all review passes for a worker session, newest first.
func (s *Store) ListReviewRunsBySession(ctx context.Context, id domain.SessionID) ([]domain.ReviewRun, error) {
	rows, err := s.qr.ListReviewRunsBySession(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("list review runs for session %s: %w", id, err)
	}
	out := make([]domain.ReviewRun, 0, len(rows))
	for _, row := range rows {
		out = append(out, reviewRunFromRow(row))
	}
	return out, nil
}

// ListRunningReviewRunsBySession returns only currently running unverdicted
// review passes for a worker session, newest first.
func (s *Store) ListRunningReviewRunsBySession(ctx context.Context, id domain.SessionID) ([]domain.ReviewRun, error) {
	rows, err := s.qr.ListRunningReviewRunsBySession(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("list running review runs for session %s: %w", id, err)
	}
	out := make([]domain.ReviewRun, 0, len(rows))
	for _, row := range rows {
		out = append(out, reviewRunFromRow(row))
	}
	return out, nil
}

// ListReviewRunsByBatch returns all passes in one trigger-created batch, oldest first.
func (s *Store) ListReviewRunsByBatch(ctx context.Context, id domain.SessionID, batchID string) ([]domain.ReviewRun, error) {
	rows, err := s.qr.ListReviewRunsByBatch(ctx, gen.ListReviewRunsByBatchParams{SessionID: id, BatchID: batchID})
	if err != nil {
		return nil, fmt.Errorf("list review runs for session %s batch %s: %w", id, batchID, err)
	}
	out := make([]domain.ReviewRun, 0, len(rows))
	for _, row := range rows {
		out = append(out, reviewRunFromRow(row))
	}
	return out, nil
}

// InsertReviewFinding adds one idempotent entry to the durable finding ledger.
func (s *Store) InsertReviewFinding(ctx context.Context, finding domain.ReviewFinding) error {
	return s.qw.InsertReviewFinding(ctx, gen.InsertReviewFindingParams{
		ID: finding.ID, RunID: finding.RunID, SessionID: finding.SessionID,
		PRURL: finding.PRURL, Round: int64(finding.Round), File: finding.File,
		ClassTag: finding.ClassTag, RootCauseNote: finding.RootCauseNote,
		FixCommit: finding.FixCommit, ThreadID: finding.ThreadID, Body: finding.Body,
		OutOfScope: boolInt64(finding.OutOfScope), DeferredIssueURL: finding.DeferredIssueURL,
		ThreadResolved: boolInt64(finding.ThreadResolved), CreatedAt: finding.CreatedAt,
	})
}

// ListReviewFindingsBySession returns the full per-worker ledger oldest first.
func (s *Store) ListReviewFindingsBySession(ctx context.Context, id domain.SessionID) ([]domain.ReviewFinding, error) {
	rows, err := s.qr.ListReviewFindingsBySession(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("list review findings for session %s: %w", id, err)
	}
	out := make([]domain.ReviewFinding, 0, len(rows))
	for _, row := range rows {
		out = append(out, reviewFindingFromRow(row))
	}
	return out, nil
}

// ListReviewFindingsByRun returns the findings recorded by one review pass.
func (s *Store) ListReviewFindingsByRun(ctx context.Context, runID string) ([]domain.ReviewFinding, error) {
	rows, err := s.qr.ListReviewFindingsByRun(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("list review findings for run %s: %w", runID, err)
	}
	out := make([]domain.ReviewFinding, 0, len(rows))
	for _, row := range rows {
		out = append(out, reviewFindingFromRow(row))
	}
	return out, nil
}

// SetPendingReviewFindingFixCommit binds unfixed findings to the next reviewed
// head, documenting which commit attempted their fix.
func (s *Store) SetPendingReviewFindingFixCommit(ctx context.Context, id domain.SessionID, prURL, commit string) (int64, error) {
	return s.qw.SetPendingReviewFindingFixCommit(ctx, gen.SetPendingReviewFindingFixCommitParams{
		FixCommit: commit, SessionID: id, PRURL: prURL,
	})
}

// MarkReviewFindingIssueFiled records the issue filed for a deferred review finding.
func (s *Store) MarkReviewFindingIssueFiled(ctx context.Context, id, issueURL string) (bool, error) {
	n, err := s.qw.MarkReviewFindingIssueFiled(ctx, gen.MarkReviewFindingIssueFiledParams{DeferredIssueURL: issueURL, ID: id})
	return n > 0, err
}

// MarkReviewFindingThreadResolved records that a review finding's thread was resolved.
func (s *Store) MarkReviewFindingThreadResolved(ctx context.Context, id string) (bool, error) {
	n, err := s.qw.MarkReviewFindingThreadResolved(ctx, id)
	return n > 0, err
}

func reviewFromRow(r gen.Review) domain.Review {
	return domain.Review{
		ID:               r.ID,
		SessionID:        r.SessionID,
		ProjectID:        r.ProjectID,
		Harness:          r.Harness,
		PRURL:            r.PRURL,
		ReviewerHandleID: r.ReviewerHandleID,
		CreatedAt:        r.CreatedAt,
		UpdatedAt:        r.UpdatedAt,
	}
}

func reviewRunFromRow(r gen.ReviewRun) domain.ReviewRun {
	var deliveredAt *time.Time
	if r.DeliveredAt.Valid {
		t := r.DeliveredAt.Time
		deliveredAt = &t
	}
	return domain.ReviewRun{
		ID:             r.ID,
		ReviewID:       r.ReviewID,
		SessionID:      r.SessionID,
		BatchID:        r.BatchID,
		Harness:        r.Harness,
		PRURL:          r.PRURL,
		TargetSHA:      r.TargetSha,
		Status:         r.Status,
		Verdict:        r.Verdict,
		Body:           r.Body,
		GithubReviewID: r.GithubReviewID,
		CreatedAt:      r.CreatedAt,
		DeliveredAt:    deliveredAt,
	}
}

func reviewFindingFromRow(r gen.ReviewFinding) domain.ReviewFinding {
	return domain.ReviewFinding{
		ID: r.ID, RunID: r.RunID, SessionID: r.SessionID, PRURL: r.PRURL,
		Round: int(r.Round), File: r.File, ClassTag: r.ClassTag,
		RootCauseNote: r.RootCauseNote, FixCommit: r.FixCommit,
		ThreadID: r.ThreadID, Body: r.Body, OutOfScope: r.OutOfScope != 0,
		DeferredIssueURL: r.DeferredIssueURL, ThreadResolved: r.ThreadResolved != 0,
		CreatedAt: r.CreatedAt,
	}
}

func boolInt64(v bool) int64 {
	if v {
		return 1
	}
	return 0
}
