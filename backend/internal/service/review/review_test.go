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

	updateCalls     int
	markCalls       int
	markedIDs       []string
	findings        []domain.ReviewFinding
	project         domain.ProjectRecord
	completeErr     error
	markErr         error
	telemetryEvents map[string]ports.TelemetryEvent
	issueClaims     map[string]string
	threadClaims    map[string]string
}

func (f *fakeStore) GetSession(_ context.Context, id domain.SessionID) (domain.SessionRecord, bool, error) {
	rec, ok := f.sessions[id]
	return rec, ok, nil
}
func (f *fakeStore) GetProject(context.Context, string) (domain.ProjectRecord, bool, error) {
	return f.project, f.project.ID != "", nil
}
func (f *fakeStore) UpdateSession(_ context.Context, rec domain.SessionRecord) error {
	if f.sessions == nil {
		f.sessions = map[domain.SessionID]domain.SessionRecord{}
	}
	f.sessions[rec.ID] = rec
	return nil
}
func (f *fakeStore) UpdateSessionLifecycle(ctx context.Context, _, after domain.SessionRecord) error {
	return f.UpdateSession(ctx, after)
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

type fakeTelemetrySink struct{ events []ports.TelemetryEvent }

func (f *fakeTelemetrySink) Emit(_ context.Context, event ports.TelemetryEvent) {
	if event.ID != "" {
		for _, existing := range f.events {
			if existing.ID == event.ID {
				return
			}
		}
	}
	f.events = append(f.events, event)
}

func (*fakeTelemetrySink) Close(context.Context) error { return nil }

func (*fakeTelemetrySink) DurableLocalTelemetry() bool { return true }

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

func (f *fakeStore) CompleteReviewRunWithFindings(_ context.Context, id string, verdict domain.ReviewVerdict, body, githubReviewID, simplificationClass string, findings []domain.ReviewFinding) (bool, error) {
	if f.completeErr != nil {
		return false, f.completeErr
	}
	for i := range f.batchRuns {
		if f.batchRuns[i].ID == id {
			if f.batchRuns[i].Status != domain.ReviewRunRunning {
				return false, nil
			}
			f.updateCalls++
			f.batchRuns[i].Status = domain.ReviewRunComplete
			f.batchRuns[i].Verdict = verdict
			f.batchRuns[i].Body = body
			f.batchRuns[i].GithubReviewID = githubReviewID
			f.batchRuns[i].SimplificationClass = simplificationClass
			f.findings = append(f.findings, findings...)
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
	f.run.Status = domain.ReviewRunComplete
	f.run.Verdict = verdict
	f.run.Body = body
	f.run.GithubReviewID = githubReviewID
	f.run.SimplificationClass = simplificationClass
	f.findings = append(f.findings, findings...)
	return true, nil
}

func (f *fakeStore) RefreshReviewRunSimplificationClass(_ context.Context, id string) (bool, error) {
	for i := range f.batchRuns {
		if f.batchRuns[i].ID == id && f.batchRuns[i].Status == domain.ReviewRunComplete && f.batchRuns[i].DeliveredAt == nil {
			f.batchRuns[i].SimplificationClass = reviewcore.SimplificationClassForRun(findingsForPR(f.findings, f.batchRuns[i].PRURL), id)
			if f.run.ID == id {
				f.run = f.batchRuns[i]
			}
			return true, nil
		}
	}
	if f.run.ID == id && f.run.Status == domain.ReviewRunComplete && f.run.DeliveredAt == nil {
		f.run.SimplificationClass = reviewcore.SimplificationClassForRun(findingsForPR(f.findings, f.run.PRURL), id)
		return true, nil
	}
	return false, nil
}

func (f *fakeStore) UpdateReviewRunResult(_ context.Context, id string, status domain.ReviewRunStatus, verdict domain.ReviewVerdict, body, githubReviewID string) (bool, error) {
	for i := range f.batchRuns {
		if f.batchRuns[i].ID == id && f.batchRuns[i].Status == domain.ReviewRunRunning {
			f.batchRuns[i].Status, f.batchRuns[i].Verdict = status, verdict
			f.batchRuns[i].Body, f.batchRuns[i].GithubReviewID = body, githubReviewID
			return true, nil
		}
	}
	if f.run.ID != id || f.run.Status != domain.ReviewRunRunning {
		return false, nil
	}
	f.run.Status, f.run.Verdict = status, verdict
	f.run.Body, f.run.GithubReviewID = body, githubReviewID
	return true, nil
}

func (f *fakeStore) MarkReviewRunDelivered(_ context.Context, id string, deliveredAt time.Time) (bool, error) {
	f.markCalls++
	f.markedIDs = append(f.markedIDs, id)
	if f.markErr != nil {
		err := f.markErr
		f.markErr = nil
		return false, err
	}
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

func (f *fakeStore) EnsureReviewRunSimplificationEvent(_ context.Context, id, targetSHA string, event ports.TelemetryEvent) (ports.TelemetryEvent, bool, error) {
	if f.telemetryEvents == nil {
		f.telemetryEvents = map[string]ports.TelemetryEvent{}
	}
	if f.run.ID == id && f.run.TargetSHA == targetSHA && f.run.Status == domain.ReviewRunComplete && f.run.SimplificationClass != "" && f.run.SimplificationDispatchedAt == nil {
		f.run.SimplificationDispatchedAt = &event.OccurredAt
		f.telemetryEvents[event.ID] = event
		for i := range f.batchRuns {
			if f.batchRuns[i].ID == id {
				f.batchRuns[i].SimplificationDispatchedAt = &event.OccurredAt
			}
		}
		return event, true, nil
	}
	for i := range f.batchRuns {
		if f.batchRuns[i].ID == id && f.batchRuns[i].TargetSHA == targetSHA && f.batchRuns[i].Status == domain.ReviewRunComplete && f.batchRuns[i].SimplificationClass != "" && f.batchRuns[i].SimplificationDispatchedAt == nil {
			f.batchRuns[i].SimplificationDispatchedAt = &event.OccurredAt
			f.telemetryEvents[event.ID] = event
			return event, true, nil
		}
	}
	if persisted, ok := f.telemetryEvents[event.ID]; ok {
		return persisted, false, nil
	}
	return ports.TelemetryEvent{}, false, nil
}

func (f *fakeStore) MarkReviewRunDeflectedReviewCleared(_ context.Context, id string, clearedAt time.Time) (bool, error) {
	if f.run.ID == id && f.run.DeflectedReviewClearedAt == nil {
		f.run.DeflectedReviewClearedAt = &clearedAt
		return true, nil
	}
	for i := range f.batchRuns {
		if f.batchRuns[i].ID == id && f.batchRuns[i].DeflectedReviewClearedAt == nil {
			f.batchRuns[i].DeflectedReviewClearedAt = &clearedAt
			return true, nil
		}
	}
	return false, nil
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
func (f *fakeStore) ListReviewFindingsByRun(_ context.Context, runID string) ([]domain.ReviewFinding, error) {
	var out []domain.ReviewFinding
	for _, finding := range f.findings {
		if finding.RunID == runID {
			out = append(out, finding)
		}
	}
	return out, nil
}
func (f *fakeStore) ListReviewFindingsBySession(_ context.Context, id domain.SessionID) ([]domain.ReviewFinding, error) {
	var out []domain.ReviewFinding
	for _, finding := range f.findings {
		if finding.SessionID == id {
			out = append(out, finding)
		}
	}
	return out, nil
}
func (f *fakeStore) SetPendingReviewFindingFixCommit(context.Context, domain.SessionID, string, string) (int64, error) {
	return 0, nil
}
func (f *fakeStore) ClaimReviewFindingIssueAction(_ context.Context, id, token string, _, _ time.Time) (bool, error) {
	if f.issueClaims == nil {
		f.issueClaims = map[string]string{}
	}
	if f.issueClaims[id] != "" {
		return false, nil
	}
	f.issueClaims[id] = token
	return true, nil
}
func (f *fakeStore) CompleteReviewFindingIssueAction(_ context.Context, id, token, issueURL string) (bool, error) {
	for i := range f.findings {
		if f.findings[i].ID == id && f.findings[i].DeferredIssueURL == "" && f.issueClaims[id] == token {
			f.findings[i].DeferredIssueURL = issueURL
			delete(f.issueClaims, id)
			return true, nil
		}
	}
	return false, nil
}
func (f *fakeStore) ReleaseReviewFindingIssueAction(_ context.Context, id, token string) error {
	if f.issueClaims[id] == token {
		delete(f.issueClaims, id)
	}
	return nil
}
func (f *fakeStore) ClaimReviewFindingThreadAction(_ context.Context, id, token string, _, _ time.Time) (bool, error) {
	if f.threadClaims == nil {
		f.threadClaims = map[string]string{}
	}
	if f.threadClaims[id] != "" {
		return false, nil
	}
	f.threadClaims[id] = token
	return true, nil
}
func (f *fakeStore) CompleteReviewFindingThreadAction(_ context.Context, id, token, replyID string) (bool, error) {
	for i := range f.findings {
		if f.findings[i].ID == id && !f.findings[i].ThreadResolved && f.threadClaims[id] == token {
			f.findings[i].ThreadResolved = true
			f.findings[i].ThreadReplyID = replyID
			delete(f.threadClaims, id)
			return true, nil
		}
	}
	return false, nil
}
func (f *fakeStore) ReleaseReviewFindingThreadAction(_ context.Context, id, token string) error {
	if f.threadClaims[id] == token {
		delete(f.threadClaims, id)
	}
	return nil
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

type fakeDeflector struct {
	filed      []ports.SCMDeferredIssueRequest
	resolved   []string
	unbound    bool
	boundCalls int
	dismissed  []ports.SCMReviewDismissalRequest
}

func (f *fakeDeflector) FileDeferredIssue(_ context.Context, request ports.SCMDeferredIssueRequest) (ports.SCMDeferredIssue, error) {
	f.filed = append(f.filed, request)
	return ports.SCMDeferredIssue{URL: "https://github.com/o/r/issues/60"}, nil
}

func (f *fakeDeflector) ReviewThreadBound(context.Context, ports.SCMReviewThreadBinding) (bool, error) {
	f.boundCalls++
	return !f.unbound, nil
}

func (f *fakeDeflector) DeflectReviewThread(_ context.Context, binding ports.SCMReviewThreadBinding) (ports.SCMReviewThreadResolution, error) {
	f.resolved = append(f.resolved, binding.ThreadID)
	return ports.SCMReviewThreadResolution{ThreadID: binding.ThreadID, ReplyID: "reply-1", Resolved: true}, nil
}

func (f *fakeDeflector) DismissReview(_ context.Context, request ports.SCMReviewDismissalRequest) (ports.SCMReviewDismissal, error) {
	f.dismissed = append(f.dismissed, request)
	return ports.SCMReviewDismissal{Cleared: true}, nil
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

func TestDeliverSubmittedSimplificationReceiptSurvivesDeliveryStampFailureAndRestart(t *testing.T) {
	now := time.Unix(100, 0).UTC()
	run := domain.ReviewRun{
		ID: "run-3", SessionID: "mer-1", PRURL: "https://github.com/o/r/pull/1", TargetSHA: "sha3",
		Status: domain.ReviewRunComplete, Verdict: domain.VerdictChangesRequested, Body: "fix the invariant",
	}
	st := &fakeStore{
		ok: true, run: run, markErr: errors.New("delivery stamp unavailable"),
		prs: []domain.PullRequest{{URL: run.PRURL, HeadSHA: run.TargetSHA}},
		sessions: map[domain.SessionID]domain.SessionRecord{
			"mer-1": {ID: "mer-1", ProjectID: "mer", Activity: domain.Activity{State: domain.ActivityActive}},
		},
		findings: []domain.ReviewFinding{
			{ID: "finding-1", RunID: "run-1", SessionID: "mer-1", PRURL: run.PRURL, Round: 1, ClassTag: "missing-notify"},
			{ID: "finding-2", RunID: "run-2", SessionID: "mer-1", PRURL: run.PRURL, Round: 2, ClassTag: "missing-notify"},
			{ID: "finding-3", RunID: run.ID, SessionID: "mer-1", PRURL: run.PRURL, Round: 3, ClassTag: "missing-notify"},
		},
	}
	messenger := &fakeMessenger{}
	telemetry := &fakeTelemetrySink{}
	manager := lifecycle.New(st, messenger, lifecycle.WithTelemetry(telemetry))
	svc := New(nil, st, WithLifecycleReducer(manager), WithClock(func() time.Time { return now }))

	if _, err := svc.deliverSubmitted(context.Background(), "mer-1", []domain.ReviewRun{run}); err == nil || !strings.Contains(err.Error(), "delivery stamp unavailable") {
		t.Fatalf("first delivery error = %v, want delivery stamp failure", err)
	}
	if len(messenger.msgs) != 1 || len(telemetry.events) != 1 {
		t.Fatalf("first delivery sends/events = %d/%d, want 1/1", len(messenger.msgs), len(telemetry.events))
	}
	if telemetry.events[0].Name != "review_simplification_round" {
		t.Fatalf("first telemetry event = %q", telemetry.events[0].Name)
	}
	if st.run.DeliveredAt != nil || st.run.SimplificationDispatchedAt == nil {
		t.Fatalf("failed delivery stamp lost independent simplification receipt: %+v", st.run)
	}

	// Recreate lifecycle to prove both receipts survive process-local state.
	restartedManager := lifecycle.New(st, messenger, lifecycle.WithTelemetry(telemetry))
	restartedService := New(nil, st, WithLifecycleReducer(restartedManager), WithClock(func() time.Time { return now.Add(time.Minute) }))
	delivered, err := restartedService.deliverSubmitted(context.Background(), "mer-1", []domain.ReviewRun{st.run})
	if err != nil {
		t.Fatalf("retry delivery: %v", err)
	}
	if len(delivered) != 1 || delivered[0].Status != domain.ReviewRunDelivered {
		t.Fatalf("retry delivered = %+v, want one delivered run", delivered)
	}
	if len(messenger.msgs) != 1 || len(telemetry.events) != 1 {
		t.Fatalf("retry sends/events = %d/%d, want no duplicates", len(messenger.msgs), len(telemetry.events))
	}
}

func TestSubmitRejectsFindingsOnApprovedReview(t *testing.T) {
	st := &fakeStore{ok: true, run: domain.ReviewRun{ID: "run-1", SessionID: "mer-1", Status: domain.ReviewRunRunning}}
	svc := New(nil, st)
	_, err := svc.SubmitMany(context.Background(), "mer-1", []SubmittedReview{{
		RunID: "run-1", Verdict: domain.VerdictApproved,
		Findings: []SubmittedFinding{{ClassTag: "should-not-persist"}},
	}})
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid", err)
	}
	if st.updateCalls != 0 || len(st.findings) != 0 {
		t.Fatalf("approved findings mutated state: updates=%d findings=%+v", st.updateCalls, st.findings)
	}
}

func TestSubmitValidatesFindingsBeforeCompletingRun(t *testing.T) {
	st := &fakeStore{ok: true, run: domain.ReviewRun{ID: "run-1", SessionID: "mer-1", Status: domain.ReviewRunRunning}}
	svc := New(nil, st)
	_, err := svc.SubmitMany(context.Background(), "mer-1", []SubmittedReview{{
		RunID: "run-1", Verdict: domain.VerdictChangesRequested, Body: "[P1] malformed",
		Findings: []SubmittedFinding{{ClassTag: " -- "}},
	}})
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid", err)
	}
	if st.run.Status != domain.ReviewRunRunning || st.updateCalls != 0 || len(st.findings) != 0 {
		t.Fatalf("malformed finding mutated state: run=%+v updates=%d findings=%+v", st.run, st.updateCalls, st.findings)
	}
}

func TestSubmitDeflectsOutOfScopeFindingInsteadOfFixDispatch(t *testing.T) {
	st := &fakeStore{
		ok:       true,
		run:      domain.ReviewRun{ID: "run-1", SessionID: "mer-1", PRURL: "https://github.com/o/r/pull/1", TargetSHA: "sha1", Status: domain.ReviewRunRunning},
		prs:      []domain.PullRequest{{URL: "https://github.com/o/r/pull/1", HeadSHA: "sha1"}},
		sessions: map[domain.SessionID]domain.SessionRecord{"mer-1": {ID: "mer-1", ProjectID: "mer"}},
		project:  domain.ProjectRecord{ID: "mer", Config: domain.ProjectConfig{ReviewPolicy: domain.ReviewPolicyConfig{OutOfScopeDeflection: true}}},
	}
	reducer := &fakeReducer{outcome: lifecycle.ReviewDeliverySent}
	deflector := &fakeDeflector{}
	svc := New(nil, st, WithLifecycleReducer(reducer), WithFindingDeflector(deflector))

	runs, err := svc.SubmitMany(context.Background(), "mer-1", []SubmittedReview{{
		RunID: "run-1", Verdict: domain.VerdictChangesRequested, Body: "[P1] persist state", GithubReviewID: "123",
		Findings: []SubmittedFinding{{File: "state.go", ClassTag: "state-persistence", RootCauseNote: "durable state belongs to the persistence subsystem", ThreadID: "PRRT_1", Body: "persist state", OutOfScope: true}},
	}})
	if err != nil {
		t.Fatalf("SubmitMany: %v", err)
	}
	if len(deflector.filed) != 1 || len(deflector.resolved) != 1 || deflector.resolved[0] != "PRRT_1" {
		t.Fatalf("deflection calls filed=%+v resolved=%+v", deflector.filed, deflector.resolved)
	}
	if len(deflector.dismissed) != 1 || deflector.dismissed[0].ReviewID != "123" {
		t.Fatalf("cleared reviews = %+v", deflector.dismissed)
	}
	if deflector.filed[0].ActionKey != "run-1:1" {
		t.Fatalf("issue action key = %q", deflector.filed[0].ActionKey)
	}
	if reducer.calls != 0 || reducer.batchCalls != 0 {
		t.Fatalf("out-of-scope finding entered fix dispatch: reducer=%+v", reducer)
	}
	if len(runs) != 1 || runs[0].Status != domain.ReviewRunComplete {
		t.Fatalf("runs = %+v", runs)
	}
	if len(st.findings) != 1 || st.findings[0].DeferredIssueURL == "" || !st.findings[0].ThreadResolved {
		t.Fatalf("durable finding = %+v", st.findings)
	}
}

func TestSubmitKeepsUnboundOutOfScopeFindingActionable(t *testing.T) {
	for _, tc := range []struct {
		name     string
		threadID string
		unbound  bool
		wantBind int
	}{
		{name: "missing thread"},
		{name: "unrelated thread", threadID: "PRRT_other", unbound: true, wantBind: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := &fakeStore{
				ok:       true,
				run:      domain.ReviewRun{ID: "run-1", SessionID: "mer-1", PRURL: "https://github.com/o/r/pull/1", TargetSHA: "sha1", Status: domain.ReviewRunRunning},
				prs:      []domain.PullRequest{{URL: "https://github.com/o/r/pull/1", HeadSHA: "sha1"}},
				sessions: map[domain.SessionID]domain.SessionRecord{"mer-1": {ID: "mer-1", ProjectID: "mer"}},
				project:  domain.ProjectRecord{ID: "mer", Config: domain.ProjectConfig{ReviewPolicy: domain.ReviewPolicyConfig{OutOfScopeDeflection: true}}},
			}
			reducer := &fakeReducer{outcome: lifecycle.ReviewDeliverySent}
			deflector := &fakeDeflector{unbound: tc.unbound}
			svc := New(nil, st, WithLifecycleReducer(reducer), WithFindingDeflector(deflector))
			_, err := svc.SubmitMany(context.Background(), "mer-1", []SubmittedReview{{
				RunID: "run-1", Verdict: domain.VerdictChangesRequested, Body: "[P1] persist state", GithubReviewID: "123",
				Findings: []SubmittedFinding{{File: "state.go", ClassTag: "state-persistence", ThreadID: tc.threadID, Body: "persist state", OutOfScope: true}},
			}})
			if err != nil {
				t.Fatal(err)
			}
			if len(deflector.filed) != 0 || len(deflector.resolved) != 0 || deflector.boundCalls != tc.wantBind {
				t.Fatalf("unsafe provider calls: bind=%d filed=%d resolved=%d", deflector.boundCalls, len(deflector.filed), len(deflector.resolved))
			}
			if reducer.calls != 1 {
				t.Fatalf("unbound finding was suppressed: reducer calls=%d", reducer.calls)
			}
		})
	}
}

func TestSubmitPersistsSimplificationOnlyForCurrentRepeatedClass(t *testing.T) {
	st := &fakeStore{
		batchRuns: []domain.ReviewRun{
			{ID: "run-1", SessionID: "mer-1", PRURL: "pr1", TargetSHA: "sha1", Status: domain.ReviewRunDelivered},
			{ID: "run-2", SessionID: "mer-1", PRURL: "pr1", TargetSHA: "sha2", Status: domain.ReviewRunDelivered},
			{ID: "run-3", SessionID: "mer-1", PRURL: "pr1", TargetSHA: "sha3", Status: domain.ReviewRunRunning},
		},
		prs: []domain.PullRequest{{URL: "pr1", HeadSHA: "sha3"}},
		findings: []domain.ReviewFinding{
			{ID: "run-1:1", RunID: "run-1", SessionID: "mer-1", PRURL: "pr1", Round: 1, ClassTag: "missing-notify"},
			{ID: "run-2:1", RunID: "run-2", SessionID: "mer-1", PRURL: "pr1", Round: 2, ClassTag: "missing-notify"},
		},
	}
	reducer := &fakeReducer{outcome: lifecycle.ReviewDeliverySent}
	svc := New(nil, st, WithLifecycleReducer(reducer))
	_, err := svc.SubmitMany(context.Background(), "mer-1", []SubmittedReview{{
		RunID: "run-3", Verdict: domain.VerdictChangesRequested, Body: "[P1] notify", Findings: []SubmittedFinding{{ClassTag: "missing-notify", Body: "notify"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if reducer.got.SimplificationClass != "missing-notify" || st.batchRuns[2].SimplificationClass != "missing-notify" {
		t.Fatalf("simplification not durable: result=%q run=%q", reducer.got.SimplificationClass, st.batchRuns[2].SimplificationClass)
	}
	st.batchRuns = append(st.batchRuns, domain.ReviewRun{ID: "run-4", SessionID: "mer-1", PRURL: "pr1", TargetSHA: "sha4", Status: domain.ReviewRunRunning})
	st.prs[0].HeadSHA = "sha4"
	_, err = svc.SubmitMany(context.Background(), "mer-1", []SubmittedReview{{
		RunID: "run-4", Verdict: domain.VerdictChangesRequested, Body: "[P1] nil", Findings: []SubmittedFinding{{ClassTag: "nil-safety", Body: "nil"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if reducer.got.SimplificationClass != "" || st.batchRuns[3].SimplificationClass != "" {
		t.Fatalf("historical class leaked into later run: result=%q run=%q", reducer.got.SimplificationClass, st.batchRuns[3].SimplificationClass)
	}
}

func TestSubmitClearsSimplificationClassAfterThresholdFindingIsDeflected(t *testing.T) {
	st := &fakeStore{
		batchRuns: []domain.ReviewRun{
			{ID: "run-1", SessionID: "mer-1", PRURL: "pr1", TargetSHA: "sha1", Status: domain.ReviewRunDelivered},
			{ID: "run-2", SessionID: "mer-1", PRURL: "pr1", TargetSHA: "sha2", Status: domain.ReviewRunDelivered},
			{ID: "run-3", SessionID: "mer-1", PRURL: "pr1", TargetSHA: "sha3", Status: domain.ReviewRunRunning},
		},
		prs:      []domain.PullRequest{{URL: "pr1", HeadSHA: "sha3"}},
		sessions: map[domain.SessionID]domain.SessionRecord{"mer-1": {ID: "mer-1", ProjectID: "mer"}},
		project:  domain.ProjectRecord{ID: "mer", Config: domain.ProjectConfig{ReviewPolicy: domain.ReviewPolicyConfig{OutOfScopeDeflection: true}}},
		findings: []domain.ReviewFinding{
			{ID: "run-1:1", RunID: "run-1", SessionID: "mer-1", PRURL: "pr1", Round: 1, ClassTag: "missing-notify"},
			{ID: "run-2:1", RunID: "run-2", SessionID: "mer-1", PRURL: "pr1", Round: 2, ClassTag: "missing-notify"},
		},
	}
	reducer := &fakeReducer{outcome: lifecycle.ReviewDeliverySent}
	svc := New(nil, st, WithLifecycleReducer(reducer), WithFindingDeflector(&fakeDeflector{}))
	runs, err := svc.SubmitMany(context.Background(), "mer-1", []SubmittedReview{{
		RunID: "run-3", Verdict: domain.VerdictChangesRequested, Body: "two findings", GithubReviewID: "review-3",
		Findings: []SubmittedFinding{
			{ClassTag: "missing-notify", Body: "unrelated subsystem", ThreadID: "thread-3", OutOfScope: true},
			{ClassTag: "nil-safety", Body: "fix the current nil path"},
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if reducer.calls != 1 || reducer.got.SimplificationClass != "" {
		t.Fatalf("deflected threshold class leaked into dispatch: calls=%d result=%+v", reducer.calls, reducer.got)
	}
	if st.batchRuns[2].SimplificationClass != "" || len(runs) != 1 || runs[0].SimplificationClass != "" {
		t.Fatalf("post-deflection simplification was not cleared durably: store=%+v runs=%+v", st.batchRuns[2], runs)
	}
	if !strings.Contains(reducer.got.Findings[1].Body, "current nil path") {
		t.Fatalf("unrelated actionable finding was not dispatched: %+v", reducer.got.Findings)
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
