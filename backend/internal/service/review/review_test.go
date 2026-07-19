package review

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/lifecycle"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	reviewcore "github.com/aoagents/agent-orchestrator/backend/internal/review"
)

type fakeStore struct {
	run        domain.ReviewRun
	ok         bool
	batchRuns  []domain.ReviewRun
	prs        []domain.PullRequest
	sessions   map[domain.SessionID]domain.SessionRecord
	signatures map[string]string

	updateCalls int
	markCalls   int
	markedIDs   []string
}

func (f *fakeStore) GetSession(_ context.Context, id domain.SessionID) (domain.SessionRecord, bool, error) {
	rec, ok := f.sessions[id]
	return rec, ok, nil
}
func (f *fakeStore) UpdateSession(_ context.Context, rec domain.SessionRecord) error {
	if f.sessions == nil {
		f.sessions = map[domain.SessionID]domain.SessionRecord{}
	}
	f.sessions[rec.ID] = rec
	return nil
}
func (f *fakeStore) GetPRLastNudgeSignature(_ context.Context, prURL string) (string, error) {
	return f.signatures[prURL], nil
}
func (f *fakeStore) UpdatePRLastNudgeSignature(_ context.Context, prURL, payload string) error {
	if f.signatures == nil {
		f.signatures = map[string]string{}
	}
	f.signatures[prURL] = payload
	return nil
}

type fakeMessenger struct{ msgs []string }

func (f *fakeMessenger) Send(_ context.Context, _ domain.SessionID, msg string) error {
	f.msgs = append(f.msgs, msg)
	return nil
}

func (f *fakeStore) GetReviewRun(_ context.Context, id string) (domain.ReviewRun, bool, error) {
	for _, run := range f.batchRuns {
		if run.ID == id {
			return run, true, nil
		}
	}
	if f.ok && f.run.ID == id {
		return f.run, true, nil
	}
	return domain.ReviewRun{}, false, nil
}

func (f *fakeStore) UpdateReviewRunResult(_ context.Context, id string, status domain.ReviewRunStatus, verdict domain.ReviewVerdict, body, githubReviewID string) (bool, error) {
	for i := range f.batchRuns {
		if f.batchRuns[i].ID == id {
			if f.batchRuns[i].Status != domain.ReviewRunRunning {
				return false, nil
			}
			f.updateCalls++
			f.batchRuns[i].Status = status
			f.batchRuns[i].Verdict = verdict
			f.batchRuns[i].Body = body
			f.batchRuns[i].GithubReviewID = githubReviewID
			if f.run.ID == id {
				f.run = f.batchRuns[i]
			}
			return true, nil
		}
	}
	if f.run.Status != domain.ReviewRunRunning {
		return false, nil
	}
	f.updateCalls++
	f.run.Status = status
	f.run.Verdict = verdict
	f.run.Body = body
	f.run.GithubReviewID = githubReviewID
	return true, nil
}

func (f *fakeStore) MarkReviewRunDelivered(_ context.Context, id string, deliveredAt time.Time) (bool, error) {
	f.markCalls++
	f.markedIDs = append(f.markedIDs, id)
	if f.run.ID == id && f.run.Status == domain.ReviewRunComplete && f.run.DeliveredAt == nil {
		f.run.Status = domain.ReviewRunDelivered
		f.run.DeliveredAt = &deliveredAt
	}
	for i := range f.batchRuns {
		if f.batchRuns[i].ID == id && f.batchRuns[i].Status == domain.ReviewRunComplete && f.batchRuns[i].DeliveredAt == nil {
			f.batchRuns[i].Status = domain.ReviewRunDelivered
			f.batchRuns[i].DeliveredAt = &deliveredAt
			return true, nil
		}
	}
	if f.run.ID != id || f.run.Status != domain.ReviewRunDelivered {
		return false, nil
	}
	return true, nil
}

func (f *fakeStore) ListReviewRunsByBatch(context.Context, domain.SessionID, string) ([]domain.ReviewRun, error) {
	out := append([]domain.ReviewRun(nil), f.batchRuns...)
	return out, nil
}

func (f *fakeStore) ListPRsBySession(context.Context, domain.SessionID) ([]domain.PullRequest, error) {
	out := append([]domain.PullRequest(nil), f.prs...)
	return out, nil
}

func (f *fakeStore) UpsertReview(context.Context, domain.Review) error { return nil }
func (f *fakeStore) GetReviewBySession(context.Context, domain.SessionID) (domain.Review, bool, error) {
	return domain.Review{}, false, nil
}
func (f *fakeStore) InsertReviewRun(_ context.Context, run domain.ReviewRun) error {
	f.batchRuns = append(f.batchRuns, run)
	return nil
}
func (f *fakeStore) SupersedeStaleRunningReviewRuns(context.Context, domain.SessionID, string, string, string) (int64, error) {
	return 0, nil
}
func (f *fakeStore) CancelRunningReviewRunsBySession(context.Context, domain.SessionID, string) (int64, error) {
	return 0, nil
}
func (f *fakeStore) GetReviewRunBySessionPRAndSHA(_ context.Context, id domain.SessionID, prURL, targetSHA string) (domain.ReviewRun, bool, error) {
	for _, run := range f.reviewRuns() {
		if run.SessionID == id && run.PRURL == prURL && run.TargetSHA == targetSHA {
			return run, true, nil
		}
	}
	return domain.ReviewRun{}, false, nil
}
func (f *fakeStore) ListReviewRunsBySession(context.Context, domain.SessionID) ([]domain.ReviewRun, error) {
	return f.reviewRuns(), nil
}
func (f *fakeStore) ListRunningReviewRunsBySession(context.Context, domain.SessionID) ([]domain.ReviewRun, error) {
	var runs []domain.ReviewRun
	for _, run := range f.reviewRuns() {
		if run.Status == domain.ReviewRunRunning {
			runs = append(runs, run)
		}
	}
	return runs, nil
}
func (f *fakeStore) reviewRuns() []domain.ReviewRun {
	if len(f.batchRuns) > 0 {
		return append([]domain.ReviewRun(nil), f.batchRuns...)
	}
	if f.ok || f.run.ID != "" {
		return []domain.ReviewRun{f.run}
	}
	return nil
}

type fakeReducer struct {
	outcome    lifecycle.ReviewDeliveryOutcome
	err        error
	calls      int
	batchCalls int
	got        lifecycle.ReviewResult
	gotBatchID string
	gotBatch   []lifecycle.ReviewResult
}

func (f *fakeReducer) ApplyReviewResult(_ context.Context, _ domain.SessionID, result lifecycle.ReviewResult) (lifecycle.ReviewDeliveryOutcome, error) {
	f.calls++
	f.got = result
	return f.outcome, f.err
}

func (f *fakeReducer) ApplyReviewBatch(_ context.Context, _ domain.SessionID, batchID string, results []lifecycle.ReviewResult) (lifecycle.ReviewDeliveryOutcome, error) {
	f.batchCalls++
	f.gotBatchID = batchID
	f.gotBatch = append([]lifecycle.ReviewResult(nil), results...)
	return f.outcome, f.err
}

func TestSubmitPersistsThenAppliesThenStampsDelivered(t *testing.T) {
	now := time.Unix(100, 0).UTC()
	st := &fakeStore{ok: true, run: domain.ReviewRun{ID: "run-1", SessionID: "mer-1", PRURL: "pr1", TargetSHA: "sha1", Status: domain.ReviewRunRunning}}
	reducer := &fakeReducer{outcome: lifecycle.ReviewDeliverySent}
	svc := New(nil, st, WithLifecycleReducer(reducer), WithClock(func() time.Time { return now }))

	run, err := svc.Submit(context.Background(), "mer-1", "run-1", domain.VerdictChangesRequested, "fix it", "987")
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if st.updateCalls != 1 || reducer.calls != 1 || st.markCalls != 1 {
		t.Fatalf("calls update/reducer/mark = %d/%d/%d", st.updateCalls, reducer.calls, st.markCalls)
	}
	if reducer.got.Verdict != domain.VerdictChangesRequested || reducer.got.Body != "fix it" || reducer.got.GithubReviewID != "987" {
		t.Fatalf("reducer saw wrong result: %+v", reducer.got)
	}
	if run.Status != domain.ReviewRunDelivered || run.DeliveredAt == nil || !run.DeliveredAt.Equal(now) {
		t.Fatalf("run not stamped delivered: %+v", run)
	}
}

func TestCoordinateReplaysParkedCompletedReviewAfterRecovery(t *testing.T) {
	for _, tc := range []struct {
		name     string
		batchID  string
		wantRuns int
	}{
		{name: "single run", wantRuns: 1},
		{name: "full batch", batchID: "batch-1", wantRuns: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run1 := domain.ReviewRun{
				ID: "run-1", SessionID: "mer-1", BatchID: tc.batchID,
				PRURL: "pr1", TargetSHA: "sha1", Status: domain.ReviewRunComplete,
				Verdict: domain.VerdictChangesRequested, Body: "fix pr1", CreatedAt: time.Unix(1, 0).UTC(),
			}
			st := &fakeStore{
				ok: true, run: run1,
				prs: []domain.PullRequest{{URL: "pr1", HeadSHA: "sha1"}},
				sessions: map[domain.SessionID]domain.SessionRecord{
					"mer-1": {ID: "mer-1", Activity: domain.Activity{State: domain.ActivityRateLimited}},
				},
			}
			if tc.batchID != "" {
				run2 := domain.ReviewRun{
					ID: "run-2", SessionID: "mer-1", BatchID: tc.batchID,
					PRURL: "pr2", TargetSHA: "sha2", Status: domain.ReviewRunComplete,
					Verdict: domain.VerdictChangesRequested, Body: "fix pr2", CreatedAt: time.Unix(1, 0).UTC(),
				}
				st.batchRuns = []domain.ReviewRun{run1, run2}
				st.prs = append(st.prs, domain.PullRequest{URL: "pr2", HeadSHA: "sha2"})
			}
			messenger := &fakeMessenger{}
			reducer := lifecycle.New(st, messenger)
			engine := reviewcore.New(reviewcore.Deps{Store: st})
			now := time.Unix(100, 0).UTC()
			svc := New(engine, st, WithLifecycleReducer(reducer), WithClock(func() time.Time { return now }))
			obs := ports.SCMObservation{
				Fetched: true,
				PR:      ports.SCMPRObservation{URL: "pr1", HeadSHA: "sha1"},
				CI:      ports.SCMCIObservation{Summary: string(domain.CIPassing), HeadSHA: "sha1"},
				Review:  ports.SCMReviewObservation{HeadSHA: "sha1"},
			}

			first, err := svc.Coordinate(context.Background(), "mer-1", obs)
			if err != nil {
				t.Fatal(err)
			}
			if first.Run.Status != domain.ReviewRunComplete || st.markCalls != 0 || len(messenger.msgs) != 0 {
				t.Fatalf("parked poll delivered feedback: result=%+v marks=%d messages=%v", first, st.markCalls, messenger.msgs)
			}

			recovered := st.sessions["mer-1"]
			recovered.Activity.State = domain.ActivityActive
			st.sessions["mer-1"] = recovered
			second, err := svc.Coordinate(context.Background(), "mer-1", obs)
			if err != nil {
				t.Fatal(err)
			}
			if st.markCalls != tc.wantRuns || len(messenger.msgs) != 1 || second.Run.Status != domain.ReviewRunDelivered || second.Run.DeliveredAt == nil {
				t.Fatalf("recovered poll did not deliver once: result=%+v marks=%d/%d messages=%v", second, st.markCalls, tc.wantRuns, messenger.msgs)
			}
			if tc.batchID != "" && (!strings.Contains(messenger.msgs[0], "PR: pr1") || !strings.Contains(messenger.msgs[0], "PR: pr2")) {
				t.Fatalf("batch replay did not preserve full group: %q", messenger.msgs[0])
			}
		})
	}
}

func TestSubmitBatchRunDoesNotWaitForOtherRunningRuns(t *testing.T) {
	now := time.Unix(100, 0).UTC()
	st := &fakeStore{
		ok:  true,
		run: domain.ReviewRun{ID: "run-1", SessionID: "mer-1", BatchID: "batch-1", PRURL: "pr1", TargetSHA: "sha1", Status: domain.ReviewRunRunning},
		batchRuns: []domain.ReviewRun{
			{ID: "run-1", SessionID: "mer-1", BatchID: "batch-1", PRURL: "pr1", TargetSHA: "sha1", Status: domain.ReviewRunRunning},
			{ID: "run-2", SessionID: "mer-1", BatchID: "batch-1", PRURL: "pr2", TargetSHA: "sha2", Status: domain.ReviewRunRunning},
		},
		prs: []domain.PullRequest{{URL: "pr1", HeadSHA: "sha1"}, {URL: "pr2", HeadSHA: "sha2"}},
	}
	reducer := &fakeReducer{outcome: lifecycle.ReviewDeliverySent}
	svc := New(nil, st, WithLifecycleReducer(reducer), WithClock(func() time.Time { return now }))

	run, err := svc.Submit(context.Background(), "mer-1", "run-1", domain.VerdictChangesRequested, "fix pr1", "101")
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if run.Status != domain.ReviewRunDelivered || run.DeliveredAt == nil || !run.DeliveredAt.Equal(now) {
		t.Fatalf("first submit status = %+v, want delivered", run)
	}
	if reducer.batchCalls != 1 || len(reducer.gotBatch) != 1 || reducer.gotBatch[0].RunID != "run-1" || st.markCalls != 1 {
		t.Fatalf("submitted run should deliver independently: batchCalls=%d got=%+v markCalls=%d", reducer.batchCalls, reducer.gotBatch, st.markCalls)
	}
}

func TestSubmitManySendsCombinedChangesRequested(t *testing.T) {
	now := time.Unix(100, 0).UTC()
	st := &fakeStore{
		ok: true,
		batchRuns: []domain.ReviewRun{
			{ID: "run-1", SessionID: "mer-1", BatchID: "batch-1", PRURL: "pr1", TargetSHA: "sha1", Status: domain.ReviewRunRunning},
			{ID: "run-2", SessionID: "mer-1", BatchID: "batch-1", PRURL: "pr2", TargetSHA: "sha2", Status: domain.ReviewRunRunning},
			{ID: "run-3", SessionID: "mer-1", BatchID: "batch-1", PRURL: "pr3", TargetSHA: "sha3", Status: domain.ReviewRunComplete, Verdict: domain.VerdictApproved},
			{ID: "run-4", SessionID: "mer-1", BatchID: "batch-1", PRURL: "pr4", TargetSHA: "old", Status: domain.ReviewRunComplete, Verdict: domain.VerdictChangesRequested, Body: "stale"},
			{ID: "run-5", SessionID: "mer-1", BatchID: "batch-1", PRURL: "pr5", TargetSHA: "sha5", Status: domain.ReviewRunFailed},
		},
		prs: []domain.PullRequest{
			{URL: "pr1", HeadSHA: "sha1"},
			{URL: "pr2", HeadSHA: "sha2"},
			{URL: "pr3", HeadSHA: "sha3"},
			{URL: "pr4", HeadSHA: "new"},
			{URL: "pr5", HeadSHA: "sha5"},
		},
	}
	reducer := &fakeReducer{outcome: lifecycle.ReviewDeliverySent}
	svc := New(nil, st, WithLifecycleReducer(reducer), WithClock(func() time.Time { return now }))

	runs, err := svc.SubmitMany(context.Background(), "mer-1", []SubmittedReview{
		{RunID: "run-1", Verdict: domain.VerdictChangesRequested, Body: "fix pr1", GithubReviewID: "101"},
		{RunID: "run-2", Verdict: domain.VerdictChangesRequested, Body: "fix pr2", GithubReviewID: "102"},
		{RunID: "run-3", Verdict: domain.VerdictApproved},
	})
	if err != nil {
		t.Fatalf("SubmitMany: %v", err)
	}
	if reducer.batchCalls != 1 || reducer.gotBatchID != "batch-1" {
		t.Fatalf("batch delivery calls/id = %d/%q", reducer.batchCalls, reducer.gotBatchID)
	}
	if len(reducer.gotBatch) != 2 || reducer.gotBatch[0].RunID != "run-1" || reducer.gotBatch[1].RunID != "run-2" {
		t.Fatalf("delivered batch = %+v, want run-1 and run-2 only", reducer.gotBatch)
	}
	if st.markCalls != 2 {
		t.Fatalf("markCalls = %d, want 2", st.markCalls)
	}
	if runs[0].Status != domain.ReviewRunDelivered || runs[0].DeliveredAt == nil || !runs[0].DeliveredAt.Equal(now) ||
		runs[1].Status != domain.ReviewRunDelivered || runs[1].DeliveredAt == nil || !runs[1].DeliveredAt.Equal(now) {
		t.Fatalf("submitted runs not stamped delivered: %+v", runs)
	}
}

func TestSubmitBatchApprovedOnlySendsNothing(t *testing.T) {
	st := &fakeStore{
		ok:  true,
		run: domain.ReviewRun{ID: "run-2", SessionID: "mer-1", BatchID: "batch-1", PRURL: "pr2", TargetSHA: "sha2", Status: domain.ReviewRunRunning},
		batchRuns: []domain.ReviewRun{
			{ID: "run-1", SessionID: "mer-1", BatchID: "batch-1", PRURL: "pr1", TargetSHA: "sha1", Status: domain.ReviewRunComplete, Verdict: domain.VerdictApproved},
			{ID: "run-2", SessionID: "mer-1", BatchID: "batch-1", PRURL: "pr2", TargetSHA: "sha2", Status: domain.ReviewRunRunning},
		},
		prs: []domain.PullRequest{{URL: "pr1", HeadSHA: "sha1"}, {URL: "pr2", HeadSHA: "sha2"}},
	}
	reducer := &fakeReducer{outcome: lifecycle.ReviewDeliverySent}
	svc := New(nil, st, WithLifecycleReducer(reducer))

	if _, err := svc.Submit(context.Background(), "mer-1", "run-2", domain.VerdictApproved, "", "102"); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if reducer.batchCalls != 0 || st.markCalls != 0 {
		t.Fatalf("approved-only batch should not deliver: batchCalls=%d markCalls=%d", reducer.batchCalls, st.markCalls)
	}
}

func TestSubmitDeliveryFailureLeavesCompletedUndeliveredForRetry(t *testing.T) {
	sendErr := errors.New("dead pane")
	st := &fakeStore{ok: true, run: domain.ReviewRun{ID: "run-1", SessionID: "mer-1", PRURL: "pr1", TargetSHA: "sha1", Status: domain.ReviewRunRunning}}
	reducer := &fakeReducer{err: sendErr}
	svc := New(nil, st, WithLifecycleReducer(reducer))

	if _, err := svc.Submit(context.Background(), "mer-1", "run-1", domain.VerdictChangesRequested, "fix it", "987"); !errors.Is(err, sendErr) {
		t.Fatalf("err = %v, want sendErr", err)
	}
	if st.run.Status != domain.ReviewRunComplete || st.run.DeliveredAt != nil || st.markCalls != 0 {
		t.Fatalf("failed delivery should leave completed/undelivered without stamp: %+v markCalls=%d", st.run, st.markCalls)
	}

	reducer.err = nil
	reducer.outcome = lifecycle.ReviewDeliverySent
	if _, err := svc.Submit(context.Background(), "mer-1", "run-1", domain.VerdictChangesRequested, "fix it", "987"); err != nil {
		t.Fatalf("retry Submit: %v", err)
	}
	if st.updateCalls != 1 || reducer.calls != 2 || st.run.Status != domain.ReviewRunDelivered || st.run.DeliveredAt == nil {
		t.Fatalf("retry should not rewrite result and should stamp delivery: update=%d reducer=%d run=%+v", st.updateCalls, reducer.calls, st.run)
	}
}

func TestSubmitCompletedRetryRejectsDifferentRecordedFields(t *testing.T) {
	tests := []struct {
		name           string
		body           string
		githubReviewID string
	}{
		{name: "different body", body: "different", githubReviewID: "987"},
		{name: "different review id", body: "fix it", githubReviewID: "654"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := &fakeStore{ok: true, run: domain.ReviewRun{
				ID: "run-1", SessionID: "mer-1", PRURL: "pr1", TargetSHA: "sha1",
				Status: domain.ReviewRunComplete, Verdict: domain.VerdictChangesRequested,
				Body: "fix it", GithubReviewID: "987",
			}}
			reducer := &fakeReducer{outcome: lifecycle.ReviewDeliverySent}
			svc := New(nil, st, WithLifecycleReducer(reducer))

			if _, err := svc.Submit(context.Background(), "mer-1", "run-1", domain.VerdictChangesRequested, tt.body, tt.githubReviewID); !errors.Is(err, ErrInvalid) {
				t.Fatalf("err = %v, want ErrInvalid", err)
			}
			if st.updateCalls != 0 || st.markCalls != 0 || reducer.calls != 0 {
				t.Fatalf("mismatched retry should not rewrite or deliver: update=%d mark=%d reducer=%d", st.updateCalls, st.markCalls, reducer.calls)
			}
		})
	}
}
