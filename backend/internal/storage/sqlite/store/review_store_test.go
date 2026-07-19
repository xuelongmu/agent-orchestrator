package store_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func TestInsertReviewRunDuplicatePRSHAMapsToSentinel(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedProject(t, s, "mer")
	rec, err := s.CreateSession(ctx, sampleRecord("mer"))
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	if err := s.UpsertReview(ctx, domain.Review{
		ID: "rev-1", SessionID: rec.ID, ProjectID: rec.ProjectID,
		Harness: domain.ReviewerClaudeCode, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("upsert review: %v", err)
	}
	run := domain.ReviewRun{
		ID: "run-1", ReviewID: "rev-1", SessionID: rec.ID, Harness: domain.ReviewerClaudeCode,
		PRURL: "https://example/pr/1", TargetSHA: "sha1", Status: domain.ReviewRunRunning, Verdict: domain.VerdictNone, CreatedAt: now,
	}
	if err := s.InsertReviewRun(ctx, run); err != nil {
		t.Fatalf("first insert: %v", err)
	}

	// A second run for the same (session_id, pr_url, target_sha) hits the
	// partial unique index (migration 0020) and must surface as the sentinel so
	// the engine can fall back to the existing run.
	dup := run
	dup.ID = "run-2"
	if err := s.InsertReviewRun(ctx, dup); !errors.Is(err, domain.ErrDuplicateReviewRun) {
		t.Fatalf("duplicate insert err = %v, want ErrDuplicateReviewRun", err)
	}

	otherPR := run
	otherPR.ID = "run-other-pr"
	otherPR.PRURL = "https://example/pr/2"
	if err := s.InsertReviewRun(ctx, otherPR); err != nil {
		t.Fatalf("same sha on different PR should insert: %v", err)
	}

	if ok, err := s.UpdateReviewRunResult(ctx, "run-1", domain.ReviewRunFailed, domain.VerdictNone, "claude: not found", ""); err != nil {
		t.Fatalf("mark failed: %v", err)
	} else if !ok {
		t.Fatal("mark failed: got ok=false")
	}
	if err := s.InsertReviewRun(ctx, dup); err != nil {
		t.Fatalf("retry after failed insert: %v", err)
	}

	// An empty target_sha is excluded from the index, so two are allowed.
	for _, id := range []string{"run-empty-1", "run-empty-2"} {
		r := run
		r.ID, r.TargetSHA = id, ""
		if err := s.InsertReviewRun(ctx, r); err != nil {
			t.Fatalf("empty-sha insert %s: %v", id, err)
		}
	}
}

func TestInsertReviewRunAllowsRerunAfterChangesRequested(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedProject(t, s, "mer")
	rec, err := s.CreateSession(ctx, sampleRecord("mer"))
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	if err := s.UpsertReview(ctx, domain.Review{
		ID: "rev-1", SessionID: rec.ID, ProjectID: rec.ProjectID,
		Harness: domain.ReviewerClaudeCode, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("upsert review: %v", err)
	}
	run := domain.ReviewRun{
		ID: "run-1", ReviewID: "rev-1", SessionID: rec.ID, Harness: domain.ReviewerClaudeCode,
		PRURL: "https://example/pr/1", TargetSHA: "sha1", Status: domain.ReviewRunRunning, Verdict: domain.VerdictNone, CreatedAt: now,
	}
	if err := s.InsertReviewRun(ctx, run); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	if ok, err := s.UpdateReviewRunResult(ctx, "run-1", domain.ReviewRunComplete, domain.VerdictChangesRequested, "please fix", "rev-1"); err != nil {
		t.Fatalf("mark changes requested: %v", err)
	} else if !ok {
		t.Fatal("mark changes requested: got ok=false")
	}

	rerun := run
	rerun.ID = "run-2"
	rerun.CreatedAt = now.Add(time.Second)
	if err := s.InsertReviewRun(ctx, rerun); err != nil {
		t.Fatalf("rerun after changes_requested insert: %v", err)
	}
}

func TestInsertReviewRunAllowsRerunAfterTerminalEmptyVerdict(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedProject(t, s, "mer")
	rec, err := s.CreateSession(ctx, sampleRecord("mer"))
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	if err := s.UpsertReview(ctx, domain.Review{
		ID: "rev-1", SessionID: rec.ID, ProjectID: rec.ProjectID,
		Harness: domain.ReviewerClaudeCode, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("upsert review: %v", err)
	}
	run := domain.ReviewRun{
		ID: "run-1", ReviewID: "rev-1", SessionID: rec.ID, Harness: domain.ReviewerClaudeCode,
		PRURL: "https://example/pr/1", TargetSHA: "sha1", Status: domain.ReviewRunComplete, Verdict: domain.VerdictNone, CreatedAt: now,
	}
	if err := s.InsertReviewRun(ctx, run); err != nil {
		t.Fatalf("first insert: %v", err)
	}

	rerun := run
	rerun.ID = "run-2"
	rerun.Status = domain.ReviewRunRunning
	rerun.CreatedAt = now.Add(time.Second)
	if err := s.InsertReviewRun(ctx, rerun); err != nil {
		t.Fatalf("rerun after terminal empty-verdict insert: %v", err)
	}
}

func TestReviewUpsertReusesRowAndRunRoundTrip(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedProject(t, s, "mer")
	rec, err := s.CreateSession(ctx, sampleRecord("mer"))
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	now := time.Now().UTC().Truncate(time.Second)

	// First upsert creates the review row.
	if err := s.UpsertReview(ctx, domain.Review{
		ID: "rev-1", SessionID: rec.ID, ProjectID: rec.ProjectID,
		Harness: domain.ReviewerClaudeCode, PRURL: "https://example/pr/1",
		ReviewerHandleID: "review-mer-1",
		CreatedAt:        now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("upsert review: %v", err)
	}
	// Second upsert with the same session reuses the row (session_id UNIQUE),
	// refreshing harness/pr_url/reviewer_handle_id but keeping the original id.
	if err := s.UpsertReview(ctx, domain.Review{
		ID: "rev-2", SessionID: rec.ID, ProjectID: rec.ProjectID,
		Harness: domain.ReviewerHarness("greptile"), PRURL: "https://example/pr/2",
		ReviewerHandleID: "review-mer-1b",
		CreatedAt:        now, UpdatedAt: now.Add(time.Second),
	}); err != nil {
		t.Fatalf("upsert review (reuse): %v", err)
	}
	got, ok, err := s.GetReviewBySession(ctx, rec.ID)
	if err != nil || !ok {
		t.Fatalf("get review: ok=%v err=%v", ok, err)
	}
	if got.ID != "rev-1" {
		t.Fatalf("upsert created a new row, want reuse: id=%q", got.ID)
	}
	if got.Harness != domain.ReviewerHarness("greptile") || got.PRURL != "https://example/pr/2" || got.ReviewerHandleID != "review-mer-1b" {
		t.Fatalf("upsert did not refresh fields: %+v", got)
	}

	// A run inserts running and updates to complete/changes_requested.
	if err := s.InsertReviewRun(ctx, domain.ReviewRun{
		ID: "run-1", ReviewID: got.ID, SessionID: rec.ID, BatchID: "batch-1", Harness: domain.ReviewerHarness("greptile"),
		PRURL: got.PRURL, TargetSHA: "sha1", Status: domain.ReviewRunRunning, Verdict: domain.VerdictNone,
		CreatedAt: now,
	}); err != nil {
		t.Fatalf("insert run: %v", err)
	}
	if ok, err := s.UpdateReviewRunResult(ctx, "run-1", domain.ReviewRunComplete, domain.VerdictChangesRequested, "please fix", "rev-987"); err != nil {
		t.Fatalf("update run: %v", err)
	} else if !ok {
		t.Fatal("update run: got ok=false")
	}

	gotRun, ok, err := s.GetReviewRun(ctx, "run-1")
	if err != nil || !ok {
		t.Fatalf("get run: ok=%v err=%v", ok, err)
	}
	if gotRun.ID != "run-1" || gotRun.SessionID != rec.ID || gotRun.BatchID != "batch-1" || gotRun.TargetSHA != "sha1" {
		t.Fatalf("get run = %+v", gotRun)
	}

	bySHA, ok, err := s.GetReviewRunBySessionPRAndSHA(ctx, rec.ID, got.PRURL, "sha1")
	if err != nil || !ok {
		t.Fatalf("by sha: ok=%v err=%v", ok, err)
	}
	if bySHA.Status != domain.ReviewRunComplete || bySHA.Verdict != domain.VerdictChangesRequested || bySHA.Body != "please fix" || bySHA.GithubReviewID != "rev-987" {
		t.Fatalf("run result not persisted: %+v", bySHA)
	}
	if _, ok, _ := s.GetReviewRunBySessionPRAndSHA(ctx, rec.ID, got.PRURL, "other"); ok {
		t.Fatal("unexpected run for a different sha")
	}

	runs, err := s.ListReviewRunsBySession(ctx, rec.ID)
	if err != nil {
		t.Fatalf("list runs: %v", err)
	}
	if len(runs) != 1 || runs[0].ID != "run-1" {
		t.Fatalf("list runs = %+v", runs)
	}
	batchRuns, err := s.ListReviewRunsByBatch(ctx, rec.ID, "batch-1")
	if err != nil {
		t.Fatalf("list batch runs: %v", err)
	}
	if len(batchRuns) != 1 || batchRuns[0].ID != "run-1" || batchRuns[0].BatchID != "batch-1" {
		t.Fatalf("batch runs = %+v", batchRuns)
	}

	if ok, err := s.UpdateReviewRunResult(ctx, "run-1", domain.ReviewRunComplete, domain.VerdictApproved, "again", ""); err != nil {
		t.Fatalf("second update: %v", err)
	} else if ok {
		t.Fatal("second update completed an already-complete run")
	}
}

func TestCancelRunningReviewRunsBySession(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedProject(t, s, "mer")
	rec, err := s.CreateSession(ctx, sampleRecord("mer"))
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	if err := s.UpsertReview(ctx, domain.Review{
		ID: "rev-1", SessionID: rec.ID, ProjectID: rec.ProjectID,
		Harness: domain.ReviewerCodex, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("upsert review: %v", err)
	}
	for _, run := range []domain.ReviewRun{
		{ID: "run-1", ReviewID: "rev-1", SessionID: rec.ID, Harness: domain.ReviewerCodex, PRURL: "https://example/pr/1", TargetSHA: "sha1", Status: domain.ReviewRunRunning, CreatedAt: now},
		{ID: "run-2", ReviewID: "rev-1", SessionID: rec.ID, Harness: domain.ReviewerCodex, PRURL: "https://example/pr/2", TargetSHA: "sha2", Status: domain.ReviewRunRunning, CreatedAt: now.Add(time.Second)},
		{ID: "run-3", ReviewID: "rev-1", SessionID: rec.ID, Harness: domain.ReviewerCodex, PRURL: "https://example/pr/3", TargetSHA: "sha3", Status: domain.ReviewRunComplete, Verdict: domain.VerdictApproved, CreatedAt: now.Add(2 * time.Second)},
	} {
		if err := s.InsertReviewRun(ctx, run); err != nil {
			t.Fatalf("insert %s: %v", run.ID, err)
		}
	}
	running, err := s.ListRunningReviewRunsBySession(ctx, rec.ID)
	if err != nil {
		t.Fatalf("list running: %v", err)
	}
	if len(running) != 2 || running[0].ID != "run-2" || running[1].ID != "run-1" {
		t.Fatalf("running = %+v", running)
	}
	n, err := s.CancelRunningReviewRunsBySession(ctx, rec.ID, "cancelled by user")
	if err != nil {
		t.Fatalf("cancel running: %v", err)
	}
	if n != 2 {
		t.Fatalf("cancelled rows = %d, want 2", n)
	}
	runs, err := s.ListReviewRunsBySession(ctx, rec.ID)
	if err != nil {
		t.Fatalf("list runs: %v", err)
	}
	byID := map[string]domain.ReviewRun{}
	for _, run := range runs {
		byID[run.ID] = run
	}
	if byID["run-1"].Status != domain.ReviewRunCancelled || byID["run-2"].Status != domain.ReviewRunCancelled {
		t.Fatalf("running runs not cancelled: %+v", byID)
	}
	if byID["run-3"].Status != domain.ReviewRunComplete {
		t.Fatalf("complete run changed: %+v", byID["run-3"])
	}
}

func TestReviewGettersMissing(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if _, ok, err := s.GetReviewBySession(ctx, "mer-1"); err != nil || ok {
		t.Fatalf("missing review: ok=%v err=%v", ok, err)
	}
	if _, ok, err := s.GetReviewRunBySessionPRAndSHA(ctx, "mer-1", "pr1", "sha1"); err != nil || ok {
		t.Fatalf("missing run: ok=%v err=%v", ok, err)
	}
	if _, ok, err := s.GetReviewRun(ctx, "run-missing"); err != nil || ok {
		t.Fatalf("missing run by id: ok=%v err=%v", ok, err)
	}
}

func TestReviewFindingLedgerRoundTripsDeflectionReceipts(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedProject(t, s, "mer")
	rec, err := s.CreateSession(ctx, sampleRecord("mer"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	if err := s.UpsertReview(ctx, domain.Review{ID: "rev-1", SessionID: rec.ID, ProjectID: rec.ProjectID, Harness: domain.ReviewerCodex, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertReviewRun(ctx, domain.ReviewRun{ID: "run-1", ReviewID: "rev-1", SessionID: rec.ID, Harness: domain.ReviewerCodex, PRURL: "pr1", TargetSHA: "sha1", Status: domain.ReviewRunRunning, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	finding := domain.ReviewFinding{ID: "run-1:1", RunID: "run-1", SessionID: rec.ID, PRURL: "pr1", Round: 1, File: "notify.go", ClassTag: "missing-notify", RootCauseNote: "every broken path notifies", ThreadID: "PRRT_1", OutOfScope: true, CreatedAt: now}
	before, err := s.LatestSeq(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.InsertReviewFinding(ctx, finding); err != nil {
		t.Fatal(err)
	}
	events, err := s.EventsAfter(ctx, before, 10)
	if err != nil || len(events) != 1 || events[0].Type != "session_updated" || events[0].SessionID != string(rec.ID) {
		t.Fatalf("finding CDC = %+v, %v", events, err)
	}
	updateBefore, err := s.LatestSeq(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := s.SetPendingReviewFindingFixCommit(ctx, rec.ID, "pr1", "sha2"); err != nil || n != 1 {
		t.Fatalf("bind fix commit = %d, %v", n, err)
	}
	if ok, err := s.ClaimReviewFindingIssueAction(ctx, finding.ID, "issue-token", now.Add(time.Minute), now); err != nil || !ok {
		t.Fatalf("claim issue = %v, %v", ok, err)
	}
	if ok, err := s.ClaimReviewFindingIssueAction(ctx, finding.ID, "racing-token", now.Add(time.Minute), now); err != nil || ok {
		t.Fatalf("racing issue claim = %v, %v", ok, err)
	}
	if ok, err := s.ClaimReviewFindingIssueAction(ctx, finding.ID, "recovery-token", now.Add(3*time.Minute), now.Add(2*time.Minute)); err != nil || !ok {
		t.Fatalf("expired issue claim recovery = %v, %v", ok, err)
	}
	if ok, err := s.CompleteReviewFindingIssueAction(ctx, finding.ID, "issue-token", "https://github.com/o/r/issues/wrong"); err != nil || ok {
		t.Fatalf("mismatched issue receipt = %v, %v", ok, err)
	}
	if ok, err := s.CompleteReviewFindingIssueAction(ctx, finding.ID, "recovery-token", "https://github.com/o/r/issues/60"); err != nil || !ok {
		t.Fatalf("mark issue = %v, %v", ok, err)
	}
	if ok, err := s.ClaimReviewFindingThreadAction(ctx, finding.ID, "thread-token", now.Add(time.Minute), now); err != nil || !ok {
		t.Fatalf("claim thread = %v, %v", ok, err)
	}
	if ok, err := s.CompleteReviewFindingThreadAction(ctx, finding.ID, "thread-token", "reply-1"); err != nil || !ok {
		t.Fatalf("mark thread = %v, %v", ok, err)
	}
	events, err = s.EventsAfter(ctx, updateBefore, 20)
	if err != nil || len(events) < 4 {
		t.Fatalf("finding update CDC = %+v, %v", events, err)
	}
	for _, event := range events {
		if event.Type != "session_updated" || event.SessionID != string(rec.ID) {
			t.Fatalf("unexpected finding update event = %+v", event)
		}
	}
	got, err := s.ListReviewFindingsBySession(ctx, rec.ID)
	if err != nil || len(got) != 1 {
		t.Fatalf("findings = %+v, %v", got, err)
	}
	if got[0].FixCommit != "sha2" || got[0].DeferredIssueURL == "" || !got[0].ThreadResolved || got[0].ThreadReplyID != "reply-1" {
		t.Fatalf("finding = %+v", got[0])
	}
}

func TestCompleteReviewRunWithFindingsIsAtomicAndPersistsSimplificationDispatch(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedProject(t, s, "mer")
	rec, err := s.CreateSession(ctx, sampleRecord("mer"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	if err := s.UpsertReview(ctx, domain.Review{ID: "rev-1", SessionID: rec.ID, ProjectID: rec.ProjectID, Harness: domain.ReviewerCodex, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertReviewRun(ctx, domain.ReviewRun{ID: "run-1", ReviewID: "rev-1", SessionID: rec.ID, Harness: domain.ReviewerCodex, PRURL: "pr1", TargetSHA: "sha1", Status: domain.ReviewRunRunning, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	duplicate := domain.ReviewFinding{ID: "run-1:1", RunID: "run-1", SessionID: rec.ID, PRURL: "pr1", Round: 1, ClassTag: "missing-notify", CreatedAt: now}
	if _, err := s.CompleteReviewRunWithFindings(ctx, "run-1", domain.VerdictChangesRequested, "fix", "123", "missing-notify", []domain.ReviewFinding{duplicate, duplicate}); err == nil {
		t.Fatal("duplicate finding insert should fail")
	}
	run, _, err := s.GetReviewRun(ctx, "run-1")
	if err != nil || run.Status != domain.ReviewRunRunning {
		t.Fatalf("failed transaction run = %+v, %v", run, err)
	}
	if findings, err := s.ListReviewFindingsByRun(ctx, "run-1"); err != nil || len(findings) != 0 {
		t.Fatalf("failed transaction findings = %+v, %v", findings, err)
	}
	if ok, err := s.CompleteReviewRunWithFindings(ctx, "run-1", domain.VerdictChangesRequested, "fix", "123", "missing-notify", []domain.ReviewFinding{duplicate}); err != nil || !ok {
		t.Fatalf("complete = %v, %v", ok, err)
	}
	if ok, err := s.RefreshReviewRunSimplificationClass(ctx, "run-1"); err != nil || !ok {
		t.Fatalf("clear simplification = %v, %v", ok, err)
	}
	run, _, err = s.GetReviewRun(ctx, "run-1")
	if err != nil || run.SimplificationClass != "" {
		t.Fatalf("cleared simplification = %+v, %v", run, err)
	}
	for i := 2; i <= 3; i++ {
		if err := s.InsertReviewFinding(ctx, domain.ReviewFinding{ID: fmt.Sprintf("run-1:%d", i), RunID: "run-1", SessionID: rec.ID, PRURL: "pr1", Round: i, ClassTag: "missing-notify", CreatedAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	if ok, err := s.RefreshReviewRunSimplificationClass(ctx, "run-1"); err != nil || !ok {
		t.Fatalf("restore simplification = %v, %v", ok, err)
	}
	if ok, err := s.MarkReviewRunDelivered(ctx, "run-1", now.Add(time.Minute)); err != nil || !ok {
		t.Fatalf("deliver = %v, %v", ok, err)
	}
	if ok, err := s.MarkReviewRunDeflectedReviewCleared(ctx, "run-1", now.Add(2*time.Minute)); err != nil || !ok {
		t.Fatalf("clear review = %v, %v", ok, err)
	}
	run, _, err = s.GetReviewRun(ctx, "run-1")
	if err != nil || run.SimplificationClass != "missing-notify" || run.SimplificationDispatchedAt == nil || run.DeflectedReviewClearedAt == nil {
		t.Fatalf("durable simplification = %+v, %v", run, err)
	}
}

func TestSetPendingReviewFindingFixCommitExcludesOnlyFullyDeflectedFindings(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedProject(t, s, "mer")
	rec, err := s.CreateSession(ctx, sampleRecord("mer"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	if err := s.UpsertReview(ctx, domain.Review{ID: "rev-1", SessionID: rec.ID, ProjectID: rec.ProjectID, Harness: domain.ReviewerCodex, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertReviewRun(ctx, domain.ReviewRun{ID: "run-1", ReviewID: "rev-1", SessionID: rec.ID, Harness: domain.ReviewerCodex, PRURL: "pr1", TargetSHA: "sha1", Status: domain.ReviewRunRunning, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	findings := []domain.ReviewFinding{
		{ID: "run-1:1", RunID: "run-1", SessionID: rec.ID, PRURL: "pr1", Round: 1, ClassTag: "actionable", CreatedAt: now},
		{ID: "run-1:2", RunID: "run-1", SessionID: rec.ID, PRURL: "pr1", Round: 1, ClassTag: "partial", OutOfScope: true, DeferredIssueURL: "issue", ThreadID: "thread", CreatedAt: now},
		{ID: "run-1:3", RunID: "run-1", SessionID: rec.ID, PRURL: "pr1", Round: 1, ClassTag: "deferred", OutOfScope: true, DeferredIssueURL: "issue", ThreadID: "thread", ThreadResolved: true, CreatedAt: now},
	}
	for _, finding := range findings {
		if err := s.InsertReviewFinding(ctx, finding); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := s.SetPendingReviewFindingFixCommit(ctx, rec.ID, "pr1", "sha2"); err != nil || n != 2 {
		t.Fatalf("updated = %d, %v", n, err)
	}
	got, err := s.ListReviewFindingsByRun(ctx, "run-1")
	if err != nil || got[0].FixCommit != "sha2" || got[1].FixCommit != "sha2" || got[2].FixCommit != "" {
		t.Fatalf("findings = %+v, %v", got, err)
	}
}
