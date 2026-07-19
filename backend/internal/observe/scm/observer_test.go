package scm

// This file tests the SCM observer orchestration contract with fake provider,
// store, and lifecycle collaborators so ETag decisions, batching, log fetching,
// review cadence, semantic hashes, and notification behavior stay provider-neutral.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

var (
	testRepo    = ports.SCMRepo{Provider: "github", Host: "github.com", Owner: "o", Name: "r", Repo: "o/r"}
	testAPIRepo = ports.SCMRepo{Provider: "github", Host: "github.com", Owner: "o", Name: "api", Repo: "o/api"}
)

type fakeStore struct {
	mu sync.Mutex

	sessions         []domain.SessionRecord
	projects         map[string]domain.ProjectRecord
	workspaceRepos   map[string][]domain.WorkspaceRepoRecord
	sessionWorktrees map[domain.SessionID][]domain.SessionWorktreeRecord
	prs              map[domain.SessionID][]domain.PullRequest
	checks           map[string][]domain.PullRequestCheck
	ciRerunAttempts  map[string]ports.SCMCIRerunAttempt
	writeErr         error

	writes []fakeWrite

	listEntered chan struct{}
	listRelease chan struct{}
	listCalls   int
}

type fakeWrite struct {
	pr         domain.PullRequest
	checks     []domain.PullRequestCheck
	reviews    []domain.PullRequestReview
	comments   []domain.PullRequestComment
	reviewMode ports.ReviewWriteMode
}

type captureTelemetrySink struct {
	events []ports.TelemetryEvent
}

func (s *captureTelemetrySink) Emit(_ context.Context, ev ports.TelemetryEvent) {
	s.events = append(s.events, ev)
}

func (s *captureTelemetrySink) Close(context.Context) error { return nil }

func (s *fakeStore) ListAllSessions(ctx context.Context) ([]domain.SessionRecord, error) {
	if s.listEntered != nil {
		select {
		case <-s.listEntered:
		default:
			close(s.listEntered)
		}
	}
	if s.listRelease != nil {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-s.listRelease:
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.listCalls++
	return append([]domain.SessionRecord(nil), s.sessions...), nil
}

func (s *fakeStore) getListCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.listCalls
}

func (s *fakeStore) GetProject(_ context.Context, id string) (domain.ProjectRecord, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.projects[id]
	return p, ok, nil
}

func (s *fakeStore) UpsertProject(_ context.Context, row domain.ProjectRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.projects == nil {
		s.projects = map[string]domain.ProjectRecord{}
	}
	s.projects[row.ID] = row
	return nil
}

func (s *fakeStore) ListWorkspaceRepos(_ context.Context, projectID string) ([]domain.WorkspaceRepoRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]domain.WorkspaceRepoRecord(nil), s.workspaceRepos[projectID]...), nil
}

func (s *fakeStore) ListSessionWorktrees(_ context.Context, sessionID domain.SessionID) ([]domain.SessionWorktreeRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]domain.SessionWorktreeRecord(nil), s.sessionWorktrees[sessionID]...), nil
}

func (s *fakeStore) ListPRsBySession(_ context.Context, id domain.SessionID) ([]domain.PullRequest, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]domain.PullRequest(nil), s.prs[id]...), nil
}

func (s *fakeStore) ListChecks(_ context.Context, prURL string) ([]domain.PullRequestCheck, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]domain.PullRequestCheck(nil), s.checks[prURL]...), nil
}

func (s *fakeStore) WriteSCMObservation(_ context.Context, pr domain.PullRequest, checks []domain.PullRequestCheck, reviews []domain.PullRequestReview, threads []domain.PullRequestReviewThread, comments []domain.PullRequestComment, reviewMode ports.ReviewWriteMode) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.writeErr != nil {
		return s.writeErr
	}
	s.writes = append(s.writes, fakeWrite{pr: pr, checks: append([]domain.PullRequestCheck(nil), checks...), reviews: append([]domain.PullRequestReview(nil), reviews...), comments: append([]domain.PullRequestComment(nil), comments...), reviewMode: reviewMode})
	return nil
}

func (s *fakeStore) GetCIRerunAttempt(_ context.Context, prURL, headSHA, checkName string) (ports.SCMCIRerunAttempt, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	attempt, ok := s.ciRerunAttempts[prURL+"\x00"+headSHA+"\x00"+checkName]
	return attempt, ok, nil
}

func (s *fakeStore) ReserveCIRerunAttempt(_ context.Context, attempt ports.SCMCIRerunAttempt) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ciRerunAttempts == nil {
		s.ciRerunAttempts = map[string]ports.SCMCIRerunAttempt{}
	}
	key := attempt.PRURL + "\x00" + attempt.HeadSHA + "\x00" + attempt.CheckName
	if _, ok := s.ciRerunAttempts[key]; ok {
		return false, nil
	}
	s.ciRerunAttempts[key] = attempt
	return true, nil
}

func (s *fakeStore) UpdateCIRerunAttempt(_ context.Context, attempt ports.SCMCIRerunAttempt) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ciRerunAttempts[attempt.PRURL+"\x00"+attempt.HeadSHA+"\x00"+attempt.CheckName] = attempt
	return nil
}

type fakeProvider struct {
	mu           sync.Mutex
	repoGuards   map[string]ports.SCMGuardResult
	checkGuards  map[string]ports.SCMGuardResult
	openPRs      map[string][]ports.SCMPRObservation
	listErr      error
	observations map[string]ports.SCMObservation
	reviews      map[string]ports.SCMReviewObservation
	logTails     map[string]string
	changedFiles map[string][]string
	rerunErr     error
	fetchErr     error
	reviewErr    error

	credentialGate   bool
	credentialOK     bool
	credentialErr    error
	credentialChecks int
	repoGuardCalls   int
	listCalls        int
	fetchBatches     [][]ports.SCMPRRef
	logCalls         int
	reviewCalls      int
	changedFileCalls int
	rerunCalls       int
	pollDelay        time.Duration
}

func (p *fakeProvider) RecommendedPollDelay(_ time.Time, base time.Duration) time.Duration {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.pollDelay > base {
		return p.pollDelay
	}
	return base
}

func (p *fakeProvider) SCMCredentialsAvailable(context.Context) (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.credentialChecks++
	if !p.credentialGate {
		return true, nil
	}
	return p.credentialOK, p.credentialErr
}

func (p *fakeProvider) ParseRepository(remote string) (ports.SCMRepo, bool) {
	remote = strings.TrimSpace(remote)
	if remote == "" {
		return ports.SCMRepo{}, false
	}
	s := strings.TrimSuffix(remote, ".git")
	s = strings.TrimPrefix(s, "https://github.com/")
	s = strings.TrimPrefix(s, "git@github.com:")
	parts := strings.Split(strings.Trim(s, "/"), "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return ports.SCMRepo{}, false
	}
	return ports.SCMRepo{Provider: "github", Host: "github.com", Owner: parts[0], Name: parts[1], Repo: parts[0] + "/" + parts[1]}, true
}
func (p *fakeProvider) RepoPRListGuard(_ context.Context, repo ports.SCMRepo, _ string) (ports.SCMGuardResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.repoGuardCalls++
	return p.repoGuards[prKey(repo, 0)], nil
}
func (p *fakeProvider) ListOpenPRsByRepo(_ context.Context, repo ports.SCMRepo) ([]ports.SCMPRObservation, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.listCalls++
	if p.listErr != nil {
		return nil, p.listErr
	}
	return p.openPRs[prKey(repo, 0)], nil
}
func (p *fakeProvider) CommitChecksGuard(_ context.Context, repo ports.SCMRepo, sha, _ string) (ports.SCMGuardResult, error) {
	return p.checkGuards[commitKey(repo, sha)], nil
}
func (p *fakeProvider) FetchPullRequests(_ context.Context, refs []ports.SCMPRRef) ([]ports.SCMObservation, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.fetchBatches = append(p.fetchBatches, append([]ports.SCMPRRef(nil), refs...))
	if p.fetchErr != nil {
		return nil, p.fetchErr
	}
	out := make([]ports.SCMObservation, 0, len(refs))
	for _, ref := range refs {
		if obs, ok := p.observations[prKey(ref.Repo, ref.Number)]; ok {
			out = append(out, obs)
		}
	}
	return out, nil
}
func (p *fakeProvider) FetchFailedCheckLogTail(_ context.Context, _ ports.SCMRepo, check ports.SCMCheckObservation) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.logCalls++
	return p.logTails[check.Name], nil
}
func (p *fakeProvider) FetchReviewThreads(_ context.Context, ref ports.SCMPRRef) (ports.SCMReviewObservation, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.reviewCalls++
	if p.reviewErr != nil {
		return ports.SCMReviewObservation{}, p.reviewErr
	}
	return p.reviews[prKey(ref.Repo, ref.Number)], nil
}

func (p *fakeProvider) FetchPullRequestFiles(_ context.Context, ref ports.SCMPRRef) ([]string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.changedFileCalls++
	return append([]string(nil), p.changedFiles[prKey(ref.Repo, ref.Number)]...), nil
}

func (p *fakeProvider) RerunFailedCheck(_ context.Context, _ ports.SCMRepo, _ ports.SCMCheckObservation) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.rerunCalls++
	return p.rerunErr
}

type fakeLifecycle struct {
	observed            []ports.SCMObservation
	idleReviewSnapshots []ports.SCMObservation
	reviewFetchFailures []ports.SCMObservation
	retried             []domain.SessionID
	err                 error
}

type fakeReviewCoordinator struct {
	observed []ports.SCMObservation
	err      error
}

func (c *fakeReviewCoordinator) CoordinateReview(_ context.Context, _ domain.SessionID, obs ports.SCMObservation) error {
	c.observed = append(c.observed, obs)
	return c.err
}

func (l *fakeLifecycle) ApplySCMObservation(_ context.Context, _ domain.SessionID, obs ports.SCMObservation) error {
	if l.err != nil {
		return l.err
	}
	l.observed = append(l.observed, obs)
	return nil
}

func (l *fakeLifecycle) ApplySCMReviewFetchFailure(_ context.Context, _ domain.SessionID, obs ports.SCMObservation) error {
	if l.err != nil {
		return l.err
	}
	l.reviewFetchFailures = append(l.reviewFetchFailures, obs)
	return nil
}

func (l *fakeLifecycle) ApplyIdleReviewSnapshot(_ context.Context, _ domain.SessionID, obs ports.SCMObservation) error {
	if l.err != nil {
		return l.err
	}
	l.idleReviewSnapshots = append(l.idleReviewSnapshots, obs)
	return nil
}

func (l *fakeLifecycle) RetryMergedCleanup(_ context.Context, id domain.SessionID) error {
	l.retried = append(l.retried, id)
	return l.err
}

func newTestObserver(store *fakeStore, provider *fakeProvider, lc Lifecycle, now time.Time) *Observer {
	return New(provider, store, lc, Config{Clock: func() time.Time { return now }, Tick: time.Hour, Logger: quietSlog(), CacheMax: 128})
}

func TestStartUsesProviderRecommendedPollDelay(t *testing.T) {
	store := &fakeStore{}
	provider := &fakeProvider{pollDelay: 100 * time.Millisecond}
	observer := New(provider, store, nil, Config{Tick: 10 * time.Millisecond, Logger: quietSlog()})
	ctx, cancel := context.WithCancel(context.Background())
	done := observer.Start(ctx)
	defer func() {
		cancel()
		<-done
	}()

	deadline := time.Now().Add(time.Second)
	for store.getListCalls() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := store.getListCalls(); got != 1 {
		t.Fatalf("initial poll calls = %d, want 1", got)
	}

	time.Sleep(30 * time.Millisecond)
	if got := store.getListCalls(); got != 1 {
		t.Fatalf("poll ran at base cadence despite provider backoff: calls = %d", got)
	}

	deadline = time.Now().Add(time.Second)
	for store.getListCalls() < 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := store.getListCalls(); got < 2 {
		t.Fatalf("poll did not resume after provider backoff: calls = %d", got)
	}
}

func TestPollRecordsUnscopedCompletion(t *testing.T) {
	startedAt := time.Unix(1_000, 0).UTC()
	completedAt := startedAt.Add(12*time.Second + 345*time.Millisecond)
	times := []time.Time{startedAt, startedAt, completedAt}
	clockCall := 0
	sink := &captureTelemetrySink{}
	observer := New(nil, &fakeStore{}, nil, Config{
		Tick:      30 * time.Second,
		Clock:     func() time.Time { at := times[clockCall]; clockCall++; return at },
		Logger:    quietSlog(),
		Telemetry: sink,
	})
	if err := observer.Poll(context.Background()); err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if len(sink.events) != 1 {
		t.Fatalf("telemetry events = %d, want 1", len(sink.events))
	}
	event := sink.events[0]
	if event.Name != "ao.lifecycle.poll" || event.Source != "scm_observer" || event.Level != ports.TelemetryLevelInfo {
		t.Fatalf("event identity = name:%q source:%q level:%q", event.Name, event.Source, event.Level)
	}
	if !event.OccurredAt.Equal(completedAt) {
		t.Fatalf("OccurredAt = %v, want %v", event.OccurredAt, completedAt)
	}
	if event.ProjectID != nil || event.SessionID != nil {
		t.Fatalf("all-project poll must remain unscoped: project=%v session=%v", event.ProjectID, event.SessionID)
	}
	wantPayload := map[string]any{
		"operation":     "lifecycle.poll",
		"outcome":       "success",
		"duration_ms":   int64(12_345),
		"interval_ms":   int64(30_000),
		"health_status": "ok",
	}
	if len(event.Payload) != len(wantPayload) {
		t.Fatalf("payload = %#v, want %#v", event.Payload, wantPayload)
	}
	for key, want := range wantPayload {
		if got := event.Payload[key]; got != want {
			t.Errorf("payload[%q] = %#v, want %#v", key, got, want)
		}
	}
}

func TestPollEmitsOverrunAndDegradedHealthWithoutChangingObserverState(t *testing.T) {
	startedAt := time.Unix(2_000, 0).UTC()
	completedAt := startedAt.Add(35*time.Second + 250*time.Millisecond)
	times := []time.Time{startedAt, startedAt, completedAt}
	clockCall := 0
	sink := &captureTelemetrySink{}
	buf := &syncBuffer{}
	logger := slog.New(slog.NewTextHandler(buf, nil))
	observer := New(nil, &fakeStore{}, nil, Config{
		Tick:      30 * time.Second,
		Clock:     func() time.Time { at := times[clockCall]; clockCall++; return at },
		Logger:    logger,
		Telemetry: sink,
	})
	observer.credentialsChecked = true
	observer.disabled = true
	observer.Cache.RepoPRListETag["github:github.com:o/r"] = "v1"

	if err := observer.Poll(context.Background()); err != nil {
		t.Fatalf("Poll: %v", err)
	}

	if len(sink.events) != 1 {
		t.Fatalf("telemetry events = %d, want 1", len(sink.events))
	}
	event := sink.events[0]
	if event.Level != ports.TelemetryLevelWarn {
		t.Fatalf("event level = %q, want warn", event.Level)
	}
	for key, want := range map[string]any{
		"operation":     "lifecycle.poll",
		"outcome":       "success",
		"duration_ms":   int64(35_250),
		"interval_ms":   int64(30_000),
		"overrun_ms":    int64(5_250),
		"reason":        "poll_overrun",
		"health_status": "warn",
	} {
		if got := event.Payload[key]; got != want {
			t.Errorf("payload[%q] = %#v, want %#v", key, got, want)
		}
	}
	logged := buf.String()
	for _, want := range []string{"poll overrun", "lifecycle health degraded", "reason=poll_overrun", "overrun_ms=5250"} {
		if !strings.Contains(logged, want) {
			t.Errorf("log missing %q:\n%s", want, logged)
		}
	}
	if !observer.credentialsChecked || !observer.disabled || observer.Cache.RepoPRListETag["github:github.com:o/r"] != "v1" {
		t.Fatalf("observability changed observer state: checked=%v disabled=%v cache=%v", observer.credentialsChecked, observer.disabled, observer.Cache.RepoPRListETag)
	}
}

func TestPollDoesNotDegradeHealthForShutdownCancellation(t *testing.T) {
	startedAt := time.Unix(3_000, 0).UTC()
	times := []time.Time{startedAt, startedAt, startedAt.Add(time.Minute)}
	clockCall := 0
	sink := &captureTelemetrySink{}
	buf := &syncBuffer{}
	observer := New(nil, &fakeStore{}, nil, Config{
		Tick:      30 * time.Second,
		Clock:     func() time.Time { at := times[clockCall]; clockCall++; return at },
		Logger:    slog.New(slog.NewTextHandler(buf, nil)),
		Telemetry: sink,
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := observer.Poll(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Poll error = %v, want context.Canceled", err)
	}
	if len(sink.events) != 0 {
		t.Fatalf("shutdown cancellation emitted telemetry: %#v", sink.events)
	}
	if logged := buf.String(); logged != "" {
		t.Fatalf("shutdown cancellation emitted health warning:\n%s", logged)
	}
}

func TestPollRetriesDurableMergedCleanupBeforeProviderRefresh(t *testing.T) {
	store := testStoreWithSession()
	store.sessions[0].Metadata.MergedCleanupPending = true
	provider := &fakeProvider{}
	lifecycle := &fakeLifecycle{}
	observer := newTestObserver(store, provider, lifecycle, time.Now())

	if err := observer.Poll(context.Background()); err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if len(lifecycle.retried) != 1 || lifecycle.retried[0] != "p-1" {
		t.Fatalf("merged cleanup retries = %v, want [p-1]", lifecycle.retried)
	}
	if provider.repoGuardCalls != 0 || provider.listCalls != 0 || len(provider.fetchBatches) != 0 {
		t.Fatalf("provider calls during cleanup retry = guards:%d lists:%d fetches:%d", provider.repoGuardCalls, provider.listCalls, len(provider.fetchBatches))
	}
}

func TestPollRetriesDurableMergedCleanupWhenObserverDisabled(t *testing.T) {
	store := testStoreWithSession()
	provider := &fakeProvider{credentialGate: true, credentialOK: false}
	lifecycle := &fakeLifecycle{}
	observer := newTestObserver(store, provider, lifecycle, time.Now())

	if err := observer.Poll(context.Background()); err != nil {
		t.Fatalf("first Poll: %v", err)
	}
	if !observer.disabled {
		t.Fatal("credential gate did not disable observer")
	}
	store.sessions[0].Metadata.MergedCleanupPending = true
	if err := observer.Poll(context.Background()); err != nil {
		t.Fatalf("disabled Poll: %v", err)
	}
	if len(lifecycle.retried) != 1 {
		t.Fatalf("cleanup retries = %v, want one after disable", lifecycle.retried)
	}
	if provider.credentialChecks != 1 || provider.repoGuardCalls != 0 || provider.listCalls != 0 || len(provider.fetchBatches) != 0 {
		t.Fatalf("provider use while disabled: credentials=%d guards=%d lists=%d fetches=%d", provider.credentialChecks, provider.repoGuardCalls, provider.listCalls, len(provider.fetchBatches))
	}
}

func TestPoll_DisabledSCMStillFeedsIdleOverlayFallback(t *testing.T) {
	store := testStoreWithSession()
	now := time.Unix(120, 0).UTC()
	store.sessions[0].Activity = domain.Activity{State: domain.ActivityIdle, LastActivityAt: now.Add(-2 * time.Minute)}
	local := knownPR(1)
	local.Review = domain.ReviewChangesRequest
	store.prs["p-1"] = []domain.PullRequest{local}
	provider := &fakeProvider{credentialGate: true, credentialOK: false}
	lifecycle := &fakeLifecycle{}
	observer := newTestObserver(store, provider, lifecycle, now)

	if err := observer.Poll(context.Background()); err != nil {
		t.Fatalf("first Poll: %v", err)
	}
	if err := observer.Poll(context.Background()); err != nil {
		t.Fatalf("disabled retry Poll: %v", err)
	}
	if len(lifecycle.reviewFetchFailures) != 2 {
		t.Fatalf("disabled SCM fallback attempts = %d, want one per poll for durable lifecycle dedup/retry", len(lifecycle.reviewFetchFailures))
	}
	if provider.repoGuardCalls != 0 || provider.listCalls != 0 || len(provider.fetchBatches) != 0 {
		t.Fatalf("disabled fallback made provider calls: guards=%d lists=%d batches=%d", provider.repoGuardCalls, provider.listCalls, len(provider.fetchBatches))
	}
}

func TestDispatchOrderIsDeterministic(t *testing.T) {
	obs := map[string]ports.SCMObservation{
		"a#9": {PR: ports.SCMPRObservation{Number: 9}},
		"a#7": {PR: ports.SCMPRObservation{Number: 7}},
		"b#3": {PR: ports.SCMPRObservation{Number: 3}},
	}
	subjects := map[string]*subject{
		"a#9": {session: domain.SessionRecord{ID: "sess-a"}},
		"a#7": {session: domain.SessionRecord{ID: "sess-a"}},
		"b#3": {session: domain.SessionRecord{ID: "sess-b"}},
	}
	want := []string{"a#7", "a#9", "b#3"}
	for run := 0; run < 8; run++ {
		got := dispatchOrder(obs, subjects)
		if len(got) != len(want) {
			t.Fatalf("run %d: got %d keys, want %d (%v)", run, len(got), len(want), got)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("run %d: order[%d]=%q, want %q (full %v)", run, i, got[i], want[i], got)
			}
		}
	}
}
func quietSlog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func testStoreWithSession() *fakeStore {
	return &fakeStore{
		sessions:        []domain.SessionRecord{{ID: "p-1", ProjectID: "p", Metadata: domain.SessionMetadata{Branch: "feat"}}},
		projects:        map[string]domain.ProjectRecord{"p": {ID: "p", RepoOriginURL: "https://github.com/o/r.git"}},
		prs:             map[domain.SessionID][]domain.PullRequest{},
		checks:          map[string][]domain.PullRequestCheck{},
		ciRerunAttempts: map[string]ports.SCMCIRerunAttempt{},
	}
}

func testObs(num int) ports.SCMObservation {
	return ports.SCMObservation{
		Fetched: true, Provider: "github", Host: "github.com", Repo: "o/r",
		PR:           ports.SCMPRObservation{URL: "https://github.com/o/r/pull/" + fmt.Sprint(num), Number: num, State: "open", SourceBranch: "feat", TargetBranch: "main", HeadSHA: "sha" + fmt.Sprint(num), Title: "PR"},
		CI:           ports.SCMCIObservation{Summary: string(domain.CIPassing), HeadSHA: "sha" + fmt.Sprint(num), Checks: []ports.SCMCheckObservation{{Name: "build", Status: string(domain.PRCheckPassed), Conclusion: "success", URL: "ci"}}},
		Review:       ports.SCMReviewObservation{Decision: string(domain.ReviewNone)},
		Mergeability: ports.SCMMergeabilityObservation{State: string(domain.MergeMergeable), Mergeable: true},
	}
}

func knownPR(num int) domain.PullRequest {
	obs := testObs(num)
	pr, _, _, _, _ := domainFromObservation("p-1", obs, domain.PullRequest{}, persistenceOptions{}, time.Unix(1, 0).UTC())
	return pr
}

func TestRepoForTrackedPRMatchesLegacyRepoOnlyRows(t *testing.T) {
	pr := knownPR(1)
	pr.Provider = ""
	pr.Host = ""
	pr.Repo = "o/r"
	repo, ok := repoForTrackedPR(pr, []ports.SCMRepo{testRepo})
	if !ok {
		t.Fatal("legacy repo-only row should match candidate repo")
	}
	if repoFullName(repo) != "o/r" {
		t.Fatalf("matched repo = %q, want o/r", repoFullName(repo))
	}
}

func TestRepoForTrackedPRUsesPersistedRepoWhenCurrentScanDropsUpstream(t *testing.T) {
	pr := knownPR(42)
	pr.Provider = "github"
	pr.Host = "github.com"
	pr.Repo = "upstream/api"
	repo, ok := repoForTrackedPR(pr, []ports.SCMRepo{testAPIRepo})
	if !ok {
		t.Fatal("persisted tracked PR repo should refresh even when current remotes no longer include it")
	}
	if repo.Provider != "github" || repo.Host != "github.com" || repo.Repo != "upstream/api" {
		t.Fatalf("repo = %#v, want persisted upstream/api tuple", repo)
	}
	if repo.Owner != "upstream" || repo.Name != "api" {
		t.Fatalf("repo owner/name = %q/%q, want upstream/api", repo.Owner, repo.Name)
	}
}

func TestStartAsyncPerformsImmediatePollAndStopsOnCancel(t *testing.T) {
	store := testStoreWithSession()
	store.listEntered = make(chan struct{})
	store.listRelease = make(chan struct{})
	provider := &fakeProvider{repoGuards: map[string]ports.SCMGuardResult{}, observations: map[string]ports.SCMObservation{}}
	obs := newTestObserver(store, provider, &fakeLifecycle{}, time.Unix(1, 0).UTC())
	ctx, cancel := context.WithCancel(context.Background())
	done := obs.Start(ctx)
	select {
	case <-store.listEntered:
	case <-time.After(time.Second):
		t.Fatal("initial poll did not start asynchronously")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("observer did not exit after context cancellation")
	}
}

func TestPoll_DisablesOnceWhenCredentialsUnavailable(t *testing.T) {
	store := testStoreWithSession()
	provider := &fakeProvider{
		credentialGate: true,
		credentialOK:   false,
		repoGuards:     map[string]ports.SCMGuardResult{prKey(testRepo, 0): {ETag: "v1"}},
		observations:   map[string]ports.SCMObservation{},
	}
	obs := newTestObserver(store, provider, &fakeLifecycle{}, time.Unix(1, 0).UTC())
	if err := obs.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := obs.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if provider.credentialChecks != 1 {
		t.Fatalf("credential checks = %d, want one lazy check", provider.credentialChecks)
	}
	if provider.repoGuardCalls != 0 || provider.listCalls != 0 || len(provider.fetchBatches) != 0 {
		t.Fatalf("provider API calls should be skipped without credentials: guards=%d lists=%d batches=%d",
			provider.repoGuardCalls, provider.listCalls, len(provider.fetchBatches))
	}
}

func TestPoll_RetriesTransientCredentialErrors(t *testing.T) {
	store := testStoreWithSession()
	provider := &fakeProvider{
		credentialGate: true,
		credentialErr:  errors.New("gh auth token failed transiently"),
		repoGuards:     map[string]ports.SCMGuardResult{prKey(testRepo, 0): {ETag: "v1"}},
		observations:   map[string]ports.SCMObservation{},
	}
	obs := newTestObserver(store, provider, &fakeLifecycle{}, time.Unix(1, 0).UTC())
	if err := obs.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if obs.credentialsChecked || obs.disabled {
		t.Fatalf("transient credential error should not commit checked/disabled: checked=%v disabled=%v", obs.credentialsChecked, obs.disabled)
	}
	if provider.credentialChecks != 1 || provider.repoGuardCalls != 0 {
		t.Fatalf("first poll should check credentials only: checks=%d repoGuards=%d", provider.credentialChecks, provider.repoGuardCalls)
	}

	provider.mu.Lock()
	provider.credentialErr = nil
	provider.credentialOK = true
	provider.mu.Unlock()
	if err := obs.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !obs.credentialsChecked || obs.disabled {
		t.Fatalf("successful retry should commit checked without disabling: checked=%v disabled=%v", obs.credentialsChecked, obs.disabled)
	}
	if provider.credentialChecks != 2 || provider.repoGuardCalls != 1 {
		t.Fatalf("second poll should retry credentials and continue: checks=%d repoGuards=%d", provider.credentialChecks, provider.repoGuardCalls)
	}
}

// syncBuffer is a goroutine-safe wrapper around bytes.Buffer for capturing
// slog output emitted from the observer's background goroutine.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// TestStart_LogsDisabledWarningWhenNoTokenAndNoSubjects exercises the bug-7
// regression: on a fresh daemon with no tracked sessions/PRs, discoverSubjects
// returns empty and Poll short-circuits before reaching the credential gate.
// The "scm observer disabled: provider credentials unavailable" warn line must
// still fire exactly once from the observer loop's pre-Poll credential check.
func TestStart_LogsDisabledWarningWhenNoTokenAndNoSubjects(t *testing.T) {
	store := &fakeStore{
		sessions: nil, // no sessions → discoverSubjects returns empty
		projects: map[string]domain.ProjectRecord{},
		prs:      map[domain.SessionID][]domain.PullRequest{},
		checks:   map[string][]domain.PullRequestCheck{},
	}
	provider := &fakeProvider{
		credentialGate: true,
		credentialOK:   false,
		repoGuards:     map[string]ports.SCMGuardResult{},
		observations:   map[string]ports.SCMObservation{},
	}

	buf := &syncBuffer{}
	logger := slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	obs := New(provider, store, &fakeLifecycle{}, Config{
		Clock:    func() time.Time { return time.Unix(1, 0).UTC() },
		Tick:     time.Hour,
		Logger:   logger,
		CacheMax: 128,
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := obs.Start(ctx)
	// Wait until the loop has emitted the expected warn line, or fail on timeout.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(buf.String(), "scm observer disabled: provider credentials unavailable") {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("observer did not exit after context cancellation")
	}

	logged := buf.String()
	if !strings.Contains(logged, "scm observer disabled: provider credentials unavailable") {
		t.Fatalf("expected disabled-credentials warn line in logs; got:\n%s", logged)
	}
	if got := strings.Count(logged, "scm observer disabled: provider credentials unavailable"); got != 1 {
		t.Fatalf("warn line should fire exactly once, got %d occurrences:\n%s", got, logged)
	}
	if !obs.credentialsChecked || !obs.disabled {
		t.Fatalf("observer state after pre-poll credential check: checked=%v disabled=%v", obs.credentialsChecked, obs.disabled)
	}
	if provider.credentialChecks != 1 {
		t.Fatalf("credential checks = %d, want exactly one pre-poll check", provider.credentialChecks)
	}
	if provider.repoGuardCalls != 0 || provider.listCalls != 0 || len(provider.fetchBatches) != 0 {
		t.Fatalf("no provider API calls expected when disabled: guards=%d lists=%d batches=%d",
			provider.repoGuardCalls, provider.listCalls, len(provider.fetchBatches))
	}
}

func TestPoll_RepoETag304SkipsListPRs(t *testing.T) {
	store := testStoreWithSession()
	provider := &fakeProvider{repoGuards: map[string]ports.SCMGuardResult{prKey(testRepo, 0): {ETag: "v1", NotModified: true}}, observations: map[string]ports.SCMObservation{}}
	obs := newTestObserver(store, provider, &fakeLifecycle{}, time.Unix(1, 0).UTC())
	obs.Cache.RepoPRListETag[prKey(testRepo, 0)] = "v1"
	if err := obs.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if provider.listCalls != 0 {
		t.Fatalf("ListOpenPRsByRepo called on 304: %d", provider.listCalls)
	}
}

func TestPoll_NoOpenPRsCommitsRepoETag(t *testing.T) {
	store := testStoreWithSession()
	provider := &fakeProvider{
		repoGuards:   map[string]ports.SCMGuardResult{prKey(testRepo, 0): {ETag: "v2"}},
		openPRs:      map[string][]ports.SCMPRObservation{},
		observations: map[string]ports.SCMObservation{},
	}
	obs := newTestObserver(store, provider, &fakeLifecycle{}, time.Unix(1, 0).UTC())
	obs.Cache.RepoPRListETag[prKey(testRepo, 0)] = "v1"
	if err := obs.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if provider.listCalls != 1 {
		t.Fatalf("ListOpenPRsByRepo calls = %d, want 1", provider.listCalls)
	}
	if got := obs.Cache.RepoPRListETag[prKey(testRepo, 0)]; got != "v2" {
		t.Fatalf("repo ETag after empty listing = %q, want v2", got)
	}
	if len(provider.fetchBatches) != 0 {
		t.Fatalf("empty listing should not fetch PR batch: %#v", provider.fetchBatches)
	}
}

func TestPoll_RepoETag200DiscoversPRAndRefreshesSamePoll(t *testing.T) {
	store := testStoreWithSession()
	provider := &fakeProvider{
		repoGuards:   map[string]ports.SCMGuardResult{prKey(testRepo, 0): {ETag: "v2"}},
		openPRs:      map[string][]ports.SCMPRObservation{prKey(testRepo, 0): {{URL: "https://github.com/o/r/pull/1", Number: 1, SourceBranch: "feat", HeadRepo: "o/r", TargetBranch: "main", HeadSHA: "sha1"}}},
		observations: map[string]ports.SCMObservation{prKey(testRepo, 1): testObs(1)},
	}
	lc := &fakeLifecycle{}
	obs := newTestObserver(store, provider, lc, time.Unix(1, 0).UTC())
	if err := obs.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if provider.listCalls != 1 {
		t.Fatalf("ListOpenPRsByRepo calls = %d, want 1", provider.listCalls)
	}
	if len(provider.fetchBatches) != 1 || len(provider.fetchBatches[0]) != 1 || provider.fetchBatches[0][0].Number != 1 {
		t.Fatalf("new PR not refreshed in same poll: %#v", provider.fetchBatches)
	}
	if len(store.writes) < 1 || len(lc.observed) != 1 {
		t.Fatalf("write/lifecycle missing: writes=%d lifecycle=%d", len(store.writes), len(lc.observed))
	}
}

// A session whose branch is the prefix of two open PRs (its root plus a stacked
// child on branch "feat/child") picks up both PRs in a single poll.
func TestPoll_DiscoversStackedChildByBranchPrefix(t *testing.T) {
	store := testStoreWithSession()
	childObs := testObs(2)
	childObs.PR.SourceBranch = "feat/child"
	childObs.PR.TargetBranch = "feat"
	provider := &fakeProvider{
		repoGuards: map[string]ports.SCMGuardResult{prKey(testRepo, 0): {ETag: "v2"}},
		openPRs: map[string][]ports.SCMPRObservation{prKey(testRepo, 0): {
			{URL: "https://github.com/o/r/pull/1", Number: 1, SourceBranch: "feat", HeadRepo: "o/r", TargetBranch: "main", HeadSHA: "sha1"},
			{URL: "https://github.com/o/r/pull/2", Number: 2, SourceBranch: "feat/child", HeadRepo: "o/r", TargetBranch: "feat", HeadSHA: "sha2"},
		}},
		observations: map[string]ports.SCMObservation{prKey(testRepo, 1): testObs(1), prKey(testRepo, 2): childObs},
	}
	lc := &fakeLifecycle{}
	obs := newTestObserver(store, provider, lc, time.Unix(1, 0).UTC())
	if err := obs.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	fetched := map[int]bool{}
	for _, batch := range provider.fetchBatches {
		for _, ref := range batch {
			fetched[ref.Number] = true
		}
	}
	if !fetched[1] || !fetched[2] {
		t.Fatalf("expected both root and stacked child fetched, got %#v", fetched)
	}
}

func TestPoll_DiscoversSiblingUnderRootSessionNamespace(t *testing.T) {
	store := testStoreWithSession()
	store.sessions[0].Metadata.Branch = "ao/p-1/root"
	prObs := testObs(1)
	prObs.PR.SourceBranch = "ao/p-1/fix"
	prObs.PR.TargetBranch = "main"
	provider := &fakeProvider{
		repoGuards: map[string]ports.SCMGuardResult{prKey(testRepo, 0): {ETag: "v2"}},
		openPRs: map[string][]ports.SCMPRObservation{prKey(testRepo, 0): {
			{URL: "https://github.com/o/r/pull/1", Number: 1, SourceBranch: "ao/p-1/fix", HeadRepo: "o/r", TargetBranch: "main", HeadSHA: "sha1"},
		}},
		observations: map[string]ports.SCMObservation{prKey(testRepo, 1): prObs},
	}
	lc := &fakeLifecycle{}
	obs := newTestObserver(store, provider, lc, time.Unix(1, 0).UTC())
	if err := obs.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(store.writes) == 0 {
		t.Fatal("expected discovered PR write")
	}
	if got := store.writes[0].pr.SourceBranch; got != "ao/p-1/fix" {
		t.Fatalf("source branch = %q, want ao/p-1/fix", got)
	}
	if got := store.writes[0].pr.SessionID; got != "p-1" {
		t.Fatalf("session id = %q, want p-1", got)
	}
	if len(lc.observed) != 1 {
		t.Fatalf("lifecycle observations = %d, want 1", len(lc.observed))
	}
}

func TestPoll_DiscoversWorkspaceChildRepoPR(t *testing.T) {
	store := testStoreWithSession()
	store.sessions[0].Metadata.Branch = "ao/p-1/root"
	store.projects["p"] = domain.ProjectRecord{
		ID:            "p",
		RepoOriginURL: "https://github.com/o/r.git",
		Kind:          domain.ProjectKindWorkspace,
	}
	store.workspaceRepos = map[string][]domain.WorkspaceRepoRecord{
		"p": {{ProjectID: "p", Name: "api", RelativePath: "api", RepoOriginURL: "https://github.com/o/api.git"}},
	}
	prObs := testObs(12)
	prObs.Repo = "o/api"
	prObs.PR.URL = "https://github.com/o/api/pull/12"
	prObs.PR.HTMLURL = "https://github.com/o/api/pull/12"
	prObs.PR.Number = 12
	prObs.PR.SourceBranch = "ao/p-1/api-billing"
	prObs.PR.TargetBranch = "main"
	prObs.PR.HeadSHA = "api-sha"
	prObs.CI.HeadSHA = "api-sha"
	provider := &fakeProvider{
		repoGuards: map[string]ports.SCMGuardResult{
			prKey(testRepo, 0):    {ETag: "root-v2"},
			prKey(testAPIRepo, 0): {ETag: "api-v2"},
		},
		openPRs: map[string][]ports.SCMPRObservation{
			prKey(testAPIRepo, 0): {
				{URL: "https://github.com/o/api/pull/12", Number: 12, SourceBranch: "ao/p-1/api-billing", HeadRepo: "o/api", TargetBranch: "main", HeadSHA: "api-sha"},
			},
		},
		observations: map[string]ports.SCMObservation{prKey(testAPIRepo, 12): prObs},
	}
	lc := &fakeLifecycle{}
	obs := newTestObserver(store, provider, lc, time.Unix(1, 0).UTC())
	if err := obs.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(provider.fetchBatches) != 1 || len(provider.fetchBatches[0]) != 1 {
		t.Fatalf("child repo PR not refreshed: %#v", provider.fetchBatches)
	}
	ref := provider.fetchBatches[0][0]
	if ref.Repo.Repo != "o/api" || ref.Number != 12 {
		t.Fatalf("fetched ref = %#v, want o/api#12", ref)
	}
	if len(store.writes) == 0 {
		t.Fatal("expected child repo PR write")
	}
	if got := store.writes[0].pr.Repo; got != "o/api" {
		t.Fatalf("persisted repo = %q, want o/api", got)
	}
	if got := store.writes[0].pr.SessionID; got != "p-1" {
		t.Fatalf("session id = %q, want p-1", got)
	}
	if len(lc.observed) != 1 {
		t.Fatalf("lifecycle observations = %d, want 1", len(lc.observed))
	}
}

func TestPoll_RootPRClaimPreservesChildRepoBranchAttribution(t *testing.T) {
	store := testStoreWithSession()
	store.sessions[0].Metadata.Branch = "ao/claim/p-1/pr-42/root"
	store.projects["p"] = domain.ProjectRecord{
		ID:            "p",
		RepoOriginURL: "https://github.com/o/r.git",
		Kind:          domain.ProjectKindWorkspace,
	}
	store.workspaceRepos = map[string][]domain.WorkspaceRepoRecord{
		"p": {{ProjectID: "p", Name: "api", RelativePath: "api", RepoOriginURL: "https://github.com/o/api.git"}},
	}
	store.sessionWorktrees = map[domain.SessionID][]domain.SessionWorktreeRecord{
		"p-1": {
			{SessionID: "p-1", RepoName: domain.RootWorkspaceRepoName, Branch: "ao/claim/p-1/pr-42/root"},
			{SessionID: "p-1", RepoName: "api", Branch: "ao/p-1/api-billing"},
		},
	}
	prObs := testObs(12)
	prObs.Repo = "o/api"
	prObs.PR.URL = "https://github.com/o/api/pull/12"
	prObs.PR.HTMLURL = prObs.PR.URL
	prObs.PR.Number = 12
	prObs.PR.SourceBranch = "ao/p-1/api-billing"
	prObs.PR.HeadRepo = "o/api"
	prObs.PR.HeadSHA = "api-sha"
	prObs.CI.HeadSHA = "api-sha"
	provider := &fakeProvider{
		repoGuards: map[string]ports.SCMGuardResult{
			prKey(testRepo, 0):    {ETag: "root-v2"},
			prKey(testAPIRepo, 0): {ETag: "api-v2"},
		},
		openPRs: map[string][]ports.SCMPRObservation{
			prKey(testAPIRepo, 0): {{URL: prObs.PR.URL, Number: 12, SourceBranch: "ao/p-1/api-billing", HeadRepo: "o/api", TargetBranch: "main", HeadSHA: "api-sha"}},
		},
		observations: map[string]ports.SCMObservation{prKey(testAPIRepo, 12): prObs},
	}
	obs := newTestObserver(store, provider, &fakeLifecycle{}, time.Unix(1, 0).UTC())
	if err := obs.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(provider.fetchBatches) != 1 || len(provider.fetchBatches[0]) != 1 {
		t.Fatalf("child repo PR not attributed after root claim: %#v", provider.fetchBatches)
	}
	if got := provider.fetchBatches[0][0]; got.Repo.Repo != "o/api" || got.Number != 12 {
		t.Fatalf("fetched ref = %#v, want o/api#12", got)
	}
}

func TestPoll_DiscoversWorkspaceChildRepoUpstreamPR(t *testing.T) {
	oldRemoteURLs := gitRemoteURLsFunc
	gitRemoteURLsFunc = func(path string) []string {
		if strings.HasSuffix(filepath.ToSlash(path), "/api") {
			return []string{"https://github.com/o/api.git", "https://github.com/upstream/api.git"}
		}
		return nil
	}
	defer func() { gitRemoteURLsFunc = oldRemoteURLs }()

	store := testStoreWithSession()
	store.sessions[0].Metadata.Branch = "ao/p-1/api-billing"
	store.projects["p"] = domain.ProjectRecord{
		ID:            "p",
		Path:          "/repo/workspace",
		RepoOriginURL: "https://github.com/o/root.git",
		Kind:          domain.ProjectKindWorkspace,
	}
	store.workspaceRepos = map[string][]domain.WorkspaceRepoRecord{
		"p": {{ProjectID: "p", Name: "api", RelativePath: "api", RepoOriginURL: "https://github.com/o/api.git"}},
	}
	upstreamRepo := ports.SCMRepo{Provider: "github", Host: "github.com", Owner: "upstream", Name: "api", Repo: "upstream/api"}
	prObs := testObs(44)
	prObs.Repo = "upstream/api"
	prObs.PR.URL = "https://github.com/upstream/api/pull/44"
	prObs.PR.HTMLURL = "https://github.com/upstream/api/pull/44"
	prObs.PR.Number = 44
	prObs.PR.SourceBranch = "ao/p-1/api-billing"
	prObs.PR.HeadRepo = "o/api"
	prObs.PR.TargetBranch = "main"
	prObs.PR.HeadSHA = "api-sha"
	prObs.CI.HeadSHA = "api-sha"
	provider := &fakeProvider{
		repoGuards: map[string]ports.SCMGuardResult{
			prKey(upstreamRepo, 0): {ETag: "upstream-v1"},
		},
		openPRs: map[string][]ports.SCMPRObservation{
			prKey(upstreamRepo, 0): {
				{URL: "https://github.com/upstream/api/pull/44", Number: 44, SourceBranch: "ao/p-1/api-billing", HeadRepo: "o/api", TargetBranch: "main", HeadSHA: "api-sha"},
			},
		},
		observations: map[string]ports.SCMObservation{prKey(upstreamRepo, 44): prObs},
	}
	lc := &fakeLifecycle{}
	obs := newTestObserver(store, provider, lc, time.Unix(1, 0).UTC())
	if err := obs.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(store.writes) == 0 {
		t.Fatal("expected discovered upstream child PR write")
	}
	if got := store.writes[0].pr.Repo; got != "upstream/api" {
		t.Fatalf("written repo = %q, want upstream/api", got)
	}
	if got := store.writes[0].pr.SessionID; got != "p-1" {
		t.Fatalf("session id = %q, want p-1", got)
	}
}

// A PR whose head branch matches a session branch but lives in a fork (its head
// repo differs from the project repo) must not be auto-attributed: its commits
// are not the session's work. It is neither fetched nor persisted.
func TestPoll_IgnoresForkPRWithMatchingBranch(t *testing.T) {
	store := testStoreWithSession()
	provider := &fakeProvider{
		repoGuards:   map[string]ports.SCMGuardResult{prKey(testRepo, 0): {ETag: "v2"}},
		openPRs:      map[string][]ports.SCMPRObservation{prKey(testRepo, 0): {{URL: "https://github.com/forker/r/pull/1", Number: 1, SourceBranch: "feat", HeadRepo: "forker/r", TargetBranch: "main", HeadSHA: "sha1"}}},
		observations: map[string]ports.SCMObservation{prKey(testRepo, 1): testObs(1)},
	}
	obs := newTestObserver(store, provider, &fakeLifecycle{}, time.Unix(1, 0).UTC())
	if err := obs.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(provider.fetchBatches) != 0 {
		t.Fatalf("fork PR must not be fetched, got %#v", provider.fetchBatches)
	}
	if len(store.writes) != 0 {
		t.Fatalf("fork PR must not be persisted, got %d writes", len(store.writes))
	}
}

func mustGit(t *testing.T, args ...string) {
	t.Helper()
	if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v (%s)", args, err, out)
	}
}

// A PR opened from the fork push origin against an upstream base repo (the
// standard fork -> upstream contribution flow) must be discovered and attributed
// by scanning every remote in the project checkout, not just origin. Its head
// lives in origin (o/r) while its base lives in upstream (up/r); the persisted
// row records the upstream base repo so refresh refetches against it.
func TestPoll_DiscoversCrossForkPRFromUpstreamRemote(t *testing.T) {
	dir := t.TempDir()
	mustGit(t, "init", dir)
	mustGit(t, "-C", dir, "remote", "add", "origin", "https://github.com/o/r.git")
	mustGit(t, "-C", dir, "remote", "add", "upstream", "https://github.com/up/r.git")

	upstream := ports.SCMRepo{Provider: "github", Host: "github.com", Owner: "up", Name: "r", Repo: "up/r"}
	store := &fakeStore{
		sessions: []domain.SessionRecord{{ID: "p-1", ProjectID: "p", Metadata: domain.SessionMetadata{Branch: "ao/p-1/root"}}},
		projects: map[string]domain.ProjectRecord{"p": {ID: "p", Path: dir, RepoOriginURL: "https://github.com/o/r.git"}},
		prs:      map[domain.SessionID][]domain.PullRequest{},
		checks:   map[string][]domain.PullRequestCheck{},
	}
	crossObs := ports.SCMObservation{
		Fetched: true, Provider: "github", Host: "github.com", Repo: "up/r",
		PR:           ports.SCMPRObservation{URL: "https://github.com/up/r/pull/1", Number: 1, State: "open", SourceBranch: "ao/p-1/feat", HeadRepo: "o/r", TargetBranch: "main", HeadSHA: "sha1", Title: "PR"},
		CI:           ports.SCMCIObservation{Summary: string(domain.CIPassing), HeadSHA: "sha1"},
		Review:       ports.SCMReviewObservation{Decision: string(domain.ReviewNone)},
		Mergeability: ports.SCMMergeabilityObservation{State: string(domain.MergeMergeable), Mergeable: true},
	}
	provider := &fakeProvider{
		repoGuards: map[string]ports.SCMGuardResult{prKey(testRepo, 0): {ETag: "origin"}, prKey(upstream, 0): {ETag: "up"}},
		openPRs: map[string][]ports.SCMPRObservation{
			prKey(upstream, 0): {{URL: "https://github.com/up/r/pull/1", Number: 1, SourceBranch: "ao/p-1/feat", HeadRepo: "o/r", TargetBranch: "main", HeadSHA: "sha1"}},
		},
		observations: map[string]ports.SCMObservation{prKey(upstream, 1): crossObs},
	}
	lc := &fakeLifecycle{}
	obs := newTestObserver(store, provider, lc, time.Unix(1, 0).UTC())
	if err := obs.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(store.writes) == 0 {
		t.Fatal("expected cross-fork PR write")
	}
	got := store.writes[0].pr
	if got.SessionID != "p-1" {
		t.Fatalf("session id = %q, want p-1", got.SessionID)
	}
	if got.Repo != "up/r" {
		t.Fatalf("pr repo = %q, want up/r (upstream base)", got.Repo)
	}
	if got.SourceBranch != "ao/p-1/feat" {
		t.Fatalf("source branch = %q, want ao/p-1/feat", got.SourceBranch)
	}
	fetched := false
	for _, batch := range provider.fetchBatches {
		for _, ref := range batch {
			if ref.Repo.Repo == "up/r" && ref.Number == 1 {
				fetched = true
			}
		}
	}
	if !fetched {
		t.Fatalf("cross-fork PR must be refreshed against upstream, batches=%#v", provider.fetchBatches)
	}
}

// A PR on a scanned upstream remote whose head lives in some third-party fork
// (not this project's origin) must never be attributed, even though its branch
// name matches a session. Scanning extra remotes stays safe.
func TestPoll_IgnoresUpstreamPRFromForeignHead(t *testing.T) {
	dir := t.TempDir()
	mustGit(t, "init", dir)
	mustGit(t, "-C", dir, "remote", "add", "origin", "https://github.com/o/r.git")
	mustGit(t, "-C", dir, "remote", "add", "upstream", "https://github.com/up/r.git")

	upstream := ports.SCMRepo{Provider: "github", Host: "github.com", Owner: "up", Name: "r", Repo: "up/r"}
	store := &fakeStore{
		sessions: []domain.SessionRecord{{ID: "p-1", ProjectID: "p", Metadata: domain.SessionMetadata{Branch: "ao/p-1/root"}}},
		projects: map[string]domain.ProjectRecord{"p": {ID: "p", Path: dir, RepoOriginURL: "https://github.com/o/r.git"}},
		prs:      map[domain.SessionID][]domain.PullRequest{},
		checks:   map[string][]domain.PullRequestCheck{},
	}
	provider := &fakeProvider{
		repoGuards: map[string]ports.SCMGuardResult{prKey(testRepo, 0): {ETag: "origin"}, prKey(upstream, 0): {ETag: "up"}},
		openPRs: map[string][]ports.SCMPRObservation{
			prKey(upstream, 0): {{URL: "https://github.com/up/r/pull/9", Number: 9, SourceBranch: "ao/p-1/feat", HeadRepo: "stranger/r", TargetBranch: "main", HeadSHA: "sha9"}},
		},
		observations: map[string]ports.SCMObservation{},
	}
	obs := newTestObserver(store, provider, &fakeLifecycle{}, time.Unix(1, 0).UTC())
	if err := obs.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(provider.fetchBatches) != 0 {
		t.Fatalf("foreign-head upstream PR must not be fetched, got %#v", provider.fetchBatches)
	}
	if len(store.writes) != 0 {
		t.Fatalf("foreign-head upstream PR must not be persisted, got %d writes", len(store.writes))
	}
}

// A newly discovered open PR is persisted as a baseline row during discovery,
// before the refresh/lifecycle pass. This is what lets a same-poll terminal
// observation for a sibling PR see the open PR in the store and avoid completing
// the session prematurely. The persist holds even when the refresh fetch yields
// no observation for the new PR.
func TestPoll_DiscoveredPRPersistedAsBaselineBeforeRefresh(t *testing.T) {
	store := testStoreWithSession()
	provider := &fakeProvider{
		repoGuards:   map[string]ports.SCMGuardResult{prKey(testRepo, 0): {ETag: "v2"}},
		openPRs:      map[string][]ports.SCMPRObservation{prKey(testRepo, 0): {{URL: "https://github.com/o/r/pull/1", Number: 1, SourceBranch: "feat", HeadRepo: "o/r", TargetBranch: "main", HeadSHA: "sha1"}}},
		observations: map[string]ports.SCMObservation{}, // refresh returns nothing
	}
	obs := newTestObserver(store, provider, &fakeLifecycle{}, time.Unix(1, 0).UTC())
	if err := obs.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	var baseline *domain.PullRequest
	for i := range store.writes {
		if store.writes[i].pr.Number == 1 {
			baseline = &store.writes[i].pr
			break
		}
	}
	if baseline == nil {
		t.Fatalf("discovered PR #1 not persisted as a baseline row; writes=%#v", store.writes)
		return
	}
	if baseline.Merged || baseline.Closed {
		t.Fatalf("baseline row must be open, got merged=%v closed=%v", baseline.Merged, baseline.Closed)
	}
}

func TestPoll_CIETagChangeRefreshesWhenRepoUnchanged(t *testing.T) {
	store := testStoreWithSession()
	store.prs["p-1"] = []domain.PullRequest{knownPR(1)}
	provider := &fakeProvider{
		repoGuards:   map[string]ports.SCMGuardResult{prKey(testRepo, 0): {ETag: "repo", NotModified: true}},
		checkGuards:  map[string]ports.SCMGuardResult{commitKey(testRepo, "sha1"): {ETag: "ci2"}},
		observations: map[string]ports.SCMObservation{prKey(testRepo, 1): testObs(1)},
	}
	obs := newTestObserver(store, provider, &fakeLifecycle{}, time.Unix(2, 0).UTC())
	obs.Cache.RepoPRListETag[prKey(testRepo, 0)] = "repo"
	obs.Cache.CommitChecksETag[commitKey(testRepo, "sha1")] = "ci1"
	if err := obs.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(provider.fetchBatches) != 1 {
		t.Fatalf("CI ETag 200 should trigger batch fetch, got %d", len(provider.fetchBatches))
	}
}

func TestPoll_GraphQLBatchChunksAt25(t *testing.T) {
	store := &fakeStore{projects: map[string]domain.ProjectRecord{"p": {ID: "p", RepoOriginURL: "https://github.com/o/r.git"}}, prs: map[domain.SessionID][]domain.PullRequest{}, checks: map[string][]domain.PullRequestCheck{}}
	provider := &fakeProvider{repoGuards: map[string]ports.SCMGuardResult{prKey(testRepo, 0): {ETag: "repo"}}, observations: map[string]ports.SCMObservation{}}
	for i := 1; i <= 26; i++ {
		id := domain.SessionID("p-" + fmt.Sprint(i))
		store.sessions = append(store.sessions, domain.SessionRecord{ID: id, ProjectID: "p", Metadata: domain.SessionMetadata{Branch: "b" + fmt.Sprint(i)}})
		pr := knownPR(i)
		pr.SessionID = id
		pr.MetadataHash = "" // force candidate
		store.prs[id] = []domain.PullRequest{pr}
		provider.observations[prKey(testRepo, i)] = testObs(i)
	}
	obs := newTestObserver(store, provider, &fakeLifecycle{}, time.Unix(1, 0).UTC())
	if err := obs.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(provider.fetchBatches) != 2 || len(provider.fetchBatches[0]) != 25 || len(provider.fetchBatches[1]) != 1 {
		t.Fatalf("batch sizes = %#v", provider.fetchBatches)
	}
}

func TestPoll_FailingCIFetchesLogTailOnlyWhenFingerprintChanged(t *testing.T) {
	failing := testObs(1)
	failing.CI.Summary = string(domain.CIFailing)
	failing.CI.Checks = []ports.SCMCheckObservation{{Name: "build", Status: string(domain.PRCheckFailed), Conclusion: "failure", ProviderID: "99"}}
	failing.CI.FailedChecks = failing.CI.Checks
	failing.CI.FailedFingerprint = "fp"

	store := testStoreWithSession()
	local := knownPR(1)
	local.CIHash = "old"
	store.prs["p-1"] = []domain.PullRequest{local}
	provider := &fakeProvider{repoGuards: map[string]ports.SCMGuardResult{prKey(testRepo, 0): {ETag: "repo"}}, checkGuards: map[string]ports.SCMGuardResult{commitKey(testRepo, "sha1"): {ETag: "ci2"}}, observations: map[string]ports.SCMObservation{prKey(testRepo, 1): failing}, logTails: map[string]string{"build": "tail"}}
	obs := newTestObserver(store, provider, &fakeLifecycle{}, time.Unix(1, 0).UTC())
	if err := obs.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if provider.logCalls != 1 {
		t.Fatalf("log calls = %d, want 1", provider.logCalls)
	}

	provider.logCalls = 0
	store.writes = nil
	withTail := failing
	withTail.CI.Checks[0].LogTail = "tail"
	withTail.CI.FailedChecks[0].LogTail = "tail"
	withTail.CI.FailureLogTail = "tail"
	local.CIHash = ciSemanticHash(withTail.CI)
	store.prs["p-1"] = []domain.PullRequest{local}
	store.checks[local.URL] = []domain.PullRequestCheck{{Name: "build", Status: domain.PRCheckFailed, LogTail: "tail"}}
	if err := obs.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if provider.logCalls != 0 {
		t.Fatalf("unchanged fingerprint fetched logs again: %d", provider.logCalls)
	}
	if len(store.writes) != 0 {
		t.Fatalf("unchanged failed fingerprint with stored log tail should not write, got %d writes", len(store.writes))
	}
}

func ciRerunObservation(providerID, logTail string) ports.SCMObservation {
	obs := testObs(1)
	failed := ports.SCMCheckObservation{
		Name: "frontend test", Status: string(domain.PRCheckFailed), Conclusion: "failure",
		ProviderID: providerID, LogTail: logTail,
	}
	obs.CI = ports.SCMCIObservation{
		Summary: "failing", HeadSHA: obs.PR.HeadSHA, FailedFingerprint: "failed-fingerprint-" + providerID,
		Checks: []ports.SCMCheckObservation{failed}, FailedChecks: []ports.SCMCheckObservation{failed},
		FailureLogTail: logTail,
	}
	obs.Mergeability = ports.SCMMergeabilityObservation{State: string(domain.MergeBlocked), Blockers: []string{"ci_failing"}}
	return obs
}

func ciRerunPollFixture(changedFiles []string, providerID, logTail string) (*fakeStore, *fakeProvider, *fakeLifecycle, *Observer) {
	store := testStoreWithSession()
	local := knownPR(1)
	local.CIHash = "previous-ci"
	store.prs["p-1"] = []domain.PullRequest{local}
	provider := &fakeProvider{
		repoGuards:   map[string]ports.SCMGuardResult{prKey(testRepo, 0): {ETag: "repo-v2"}},
		checkGuards:  map[string]ports.SCMGuardResult{commitKey(testRepo, "sha1"): {ETag: "ci-v2"}},
		observations: map[string]ports.SCMObservation{prKey(testRepo, 1): ciRerunObservation(providerID, logTail)},
		changedFiles: map[string][]string{prKey(testRepo, 1): changedFiles},
	}
	lifecycle := &fakeLifecycle{}
	observer := newTestObserver(store, provider, lifecycle, time.Unix(1_000, 0).UTC())
	return store, provider, lifecycle, observer
}

func TestPoll_CIRerunRelatedFailureDispatchesWithoutRerun(t *testing.T) {
	_, provider, lifecycle, observer := ciRerunPollFixture(
		[]string{"frontend/src/renderer/components/CenterPane.tsx"},
		"101", "FAIL frontend/src/renderer/components/CenterPane.test.tsx > renders",
	)
	if err := observer.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if provider.rerunCalls != 0 {
		t.Fatalf("rerun calls = %d, want 0 for a diff-related failure", provider.rerunCalls)
	}
	if len(lifecycle.observed) != 1 || lifecycle.observed[0].CI.RerunRequested {
		t.Fatalf("related failure must take normal lifecycle path: %#v", lifecycle.observed)
	}
}

func TestPoll_CIRerunPackageRelativeFailurePathDispatchesWithoutRerun(t *testing.T) {
	_, provider, lifecycle, observer := ciRerunPollFixture(
		[]string{"frontend/src/renderer/components/CenterPane.tsx"},
		"101", "FAIL src/renderer/components/CenterPane.test.tsx > renders",
	)
	if err := observer.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if provider.rerunCalls != 0 {
		t.Fatalf("rerun calls = %d, want 0 for an ambiguous package-relative failure path", provider.rerunCalls)
	}
	if len(lifecycle.observed) != 1 || lifecycle.observed[0].CI.RerunRequested {
		t.Fatalf("package-relative failure must take normal lifecycle path: %#v", lifecycle.observed)
	}
}

func TestPoll_CIRerunUnrelatedUnitFailureRerunsOnceAndSuppressesDispatch(t *testing.T) {
	store, provider, lifecycle, observer := ciRerunPollFixture(
		[]string{"backend/internal/lifecycle/reactions.go"},
		"101", "FAIL frontend/src/renderer/components/CenterPane.test.tsx > renders",
	)
	if err := observer.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if provider.rerunCalls != 1 {
		t.Fatalf("rerun calls = %d, want 1", provider.rerunCalls)
	}
	if len(lifecycle.observed) != 1 || !lifecycle.observed[0].CI.RerunRequested {
		t.Fatalf("accepted rerun must suppress the stale CI dispatch: %#v", lifecycle.observed)
	}
	if len(store.ciRerunAttempts) != 1 {
		t.Fatalf("durable attempts = %#v, want one", store.ciRerunAttempts)
	}
}

func TestPoll_CIRerunReproducedFailureDispatches(t *testing.T) {
	store, provider, lifecycle, observer := ciRerunPollFixture(
		[]string{"backend/internal/lifecycle/reactions.go"},
		"202", "FAIL frontend/src/renderer/components/CenterPane.test.tsx > renders",
	)
	key := "https://github.com/o/r/pull/1\x00sha1\x00frontend test"
	store.ciRerunAttempts[key] = ports.SCMCIRerunAttempt{
		PRURL: "https://github.com/o/r/pull/1", HeadSHA: "sha1", CheckName: "frontend test",
		ProviderID: "101", Status: ports.SCMCIRerunRequested, RequestedAt: time.Unix(900, 0).UTC(),
	}
	if err := observer.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if provider.rerunCalls != 0 {
		t.Fatalf("reproduced failure rerun calls = %d, want 0", provider.rerunCalls)
	}
	if len(lifecycle.observed) != 1 || lifecycle.observed[0].CI.RerunRequested {
		t.Fatalf("reproduced failure must resume normal dispatch: %#v", lifecycle.observed)
	}
}

func TestPoll_CIRerunRestartDeduplicatesPendingAttempt(t *testing.T) {
	store, provider, lifecycle, observer := ciRerunPollFixture(
		[]string{"backend/internal/lifecycle/reactions.go"},
		"101", "FAIL frontend/src/renderer/components/CenterPane.test.tsx > renders",
	)
	if err := observer.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	store.writes = nil
	lifecycle.observed = nil
	restarted := newTestObserver(store, provider, lifecycle, time.Unix(1_001, 0).UTC())
	if err := restarted.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if provider.rerunCalls != 1 {
		t.Fatalf("rerun calls across restart = %d, want exactly 1", provider.rerunCalls)
	}
	if len(lifecycle.observed) != 1 || !lifecycle.observed[0].CI.RerunRequested {
		t.Fatalf("same pending provider job must stay suppressed after restart: %#v", lifecycle.observed)
	}
}

func TestPoll_CIRerunUnchangedFailureDispatchesAfterPendingTimeout(t *testing.T) {
	store, provider, lifecycle, observer := ciRerunPollFixture(
		[]string{"backend/internal/lifecycle/reactions.go"},
		"101", "FAIL frontend/src/renderer/components/CenterPane.test.tsx > renders",
	)
	if err := observer.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if provider.rerunCalls != 1 || len(store.writes) < 2 {
		t.Fatalf("first poll reruns=%d writes=%d, want 1 and persisted acknowledgement", provider.rerunCalls, len(store.writes))
	}
	// Reconstruct the durable post-ack state a real SQLite store exposes after
	// restart. The provider still reports the exact same failed job/check id.
	store.prs["p-1"] = []domain.PullRequest{store.writes[len(store.writes)-1].pr}
	store.checks["https://github.com/o/r/pull/1"] = append([]domain.PullRequestCheck(nil), store.writes[len(store.writes)-1].checks...)
	store.writes = nil
	lifecycle.observed = nil
	restarted := newTestObserver(store, provider, lifecycle, time.Unix(1_000, 0).UTC().Add(ciRerunPendingTimeout+time.Second))
	if err := restarted.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if provider.rerunCalls != 1 {
		t.Fatalf("rerun calls after timeout = %d, want still 1", provider.rerunCalls)
	}
	if len(lifecycle.observed) != 1 || lifecycle.observed[0].CI.RerunRequested {
		t.Fatalf("expired unchanged failure must resume normal lifecycle dispatch: %#v", lifecycle.observed)
	}
}

func TestPoll_CIRerunUnchangedPreFeatureFailureEntersPendingOnce(t *testing.T) {
	store, provider, lifecycle, observer := ciRerunPollFixture(
		[]string{"backend/internal/lifecycle/reactions.go"},
		"101", "FAIL frontend/src/renderer/components/CenterPane.test.tsx > renders",
	)
	observed := provider.observations[prKey(testRepo, 1)]
	local := store.prs["p-1"][0]
	local.MetadataHash = metadataSemanticHash(observed)
	local.CIHash = ciSemanticHash(observed.CI)
	local.ReviewHash = reviewSemanticHash(observed.Review)
	store.prs["p-1"] = []domain.PullRequest{local}

	if err := observer.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if provider.rerunCalls != 1 {
		t.Fatalf("rerun calls = %d, want 1 for the first post-upgrade observation", provider.rerunCalls)
	}
	if len(lifecycle.observed) != 1 || !lifecycle.observed[0].CI.RerunRequested {
		t.Fatalf("new rerun from unchanged facts must enter CI pending: %#v", lifecycle.observed)
	}
	lifecycle.observed = nil
	if err := observer.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if provider.rerunCalls != 1 || len(lifecycle.observed) != 0 {
		t.Fatalf("pending unchanged poll must be quiet: reruns=%d lifecycle=%#v", provider.rerunCalls, lifecycle.observed)
	}
}

func TestPoll_CIRerunProviderFailureResumesNormalDispatch(t *testing.T) {
	store, provider, lifecycle, observer := ciRerunPollFixture(
		[]string{"backend/internal/lifecycle/reactions.go"},
		"101", "FAIL frontend/src/renderer/components/CenterPane.test.tsx > renders",
	)
	provider.rerunErr = errors.New("actions API unavailable")
	if err := observer.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if provider.rerunCalls != 1 {
		t.Fatalf("rerun calls = %d, want 1", provider.rerunCalls)
	}
	if len(lifecycle.observed) != 1 || lifecycle.observed[0].CI.RerunRequested {
		t.Fatalf("provider failure must resume normal lifecycle dispatch: %#v", lifecycle.observed)
	}
	for _, attempt := range store.ciRerunAttempts {
		if attempt.Status != ports.SCMCIRerunFailed {
			t.Fatalf("attempt status = %q, want failed", attempt.Status)
		}
	}
}

func TestPoll_CIRerunKnownRunnerFlakeDoesNotRequireDiffFacts(t *testing.T) {
	_, provider, lifecycle, observer := ciRerunPollFixture(
		nil, "101", "The runner has received a shutdown signal. This can happen when the runner service is stopped.",
	)
	if err := observer.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if provider.changedFileCalls != 0 || provider.rerunCalls != 1 {
		t.Fatalf("changed-file calls=%d rerun calls=%d, want 0/1", provider.changedFileCalls, provider.rerunCalls)
	}
	if len(lifecycle.observed) != 1 || !lifecycle.observed[0].CI.RerunRequested {
		t.Fatalf("known runner flake must suppress stale failure: %#v", lifecycle.observed)
	}
}

func TestEnrichFailureLogsDoesNotRefetchExistingTailOrMissingProviderID(t *testing.T) {
	obsValue := testObs(1)
	obsValue.CI.Summary = string(domain.CIFailing)
	obsValue.CI.FailedFingerprint = "fp"
	obsValue.CI.Checks = []ports.SCMCheckObservation{
		{Name: "build", Status: string(domain.PRCheckFailed), Conclusion: "failure", ProviderID: "99", LogTail: "provider supplied tail"},
		{Name: "lint", Status: string(domain.PRCheckFailed), Conclusion: "failure"},
	}
	obsValue.CI.FailedChecks = append([]ports.SCMCheckObservation(nil), obsValue.CI.Checks...)

	provider := &fakeProvider{logTails: map[string]string{"build": "fetched tail", "lint": "should not fetch"}}
	obs := newTestObserver(testStoreWithSession(), provider, &fakeLifecycle{}, time.Unix(1, 0).UTC())
	obs.enrichFailureLogs(context.Background(), &obsValue, domain.PullRequest{})

	if provider.logCalls != 0 {
		t.Fatalf("log calls = %d, want 0 when tail already exists or provider id is missing", provider.logCalls)
	}
	if got := obsValue.CI.FailedChecks[0].LogTail; got != "provider supplied tail" {
		t.Fatalf("existing tail changed: got %q", got)
	}
	if got := obsValue.CI.FailedChecks[1].LogTail; got != "" {
		t.Fatalf("tail without provider id = %q, want empty", got)
	}
	if got := obsValue.CI.FailureLogTail; got != "provider supplied tail" {
		t.Fatalf("FailureLogTail = %q, want only existing tail", got)
	}
}

func TestPoll_ReviewPollingRespectsInterval(t *testing.T) {
	store := testStoreWithSession()
	local := knownPR(1)
	local.Review = domain.ReviewChangesRequest
	local.ReviewHash = "old-review"
	store.prs["p-1"] = []domain.PullRequest{local}
	provider := &fakeProvider{repoGuards: map[string]ports.SCMGuardResult{prKey(testRepo, 0): {ETag: "repo", NotModified: true}}, observations: map[string]ports.SCMObservation{}, reviews: map[string]ports.SCMReviewObservation{prKey(testRepo, 1): {Decision: string(domain.ReviewChangesRequest), Threads: []ports.SCMReviewThreadObservation{{ID: "t1", Path: "f.go", Line: 1, Comments: []ports.SCMReviewCommentObservation{{ID: "c1", Body: "fix"}}}}}}}
	obs := newTestObserver(store, provider, &fakeLifecycle{}, time.Unix(120, 0).UTC())
	obs.Cache.RepoPRListETag[prKey(testRepo, 0)] = "repo"
	obs.Cache.LastReviewPollAt[prKey(testRepo, 1)] = time.Unix(90, 0).UTC()
	if err := obs.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if provider.reviewCalls != 0 {
		t.Fatalf("review fetched before interval: %d", provider.reviewCalls)
	}
	obs.Cache.LastReviewPollAt[prKey(testRepo, 1)] = time.Unix(0, 0).UTC()
	if err := obs.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if provider.reviewCalls != 1 {
		t.Fatalf("review not fetched after interval: %d", provider.reviewCalls)
	}
	if len(store.writes) == 0 || store.writes[0].reviewMode != ports.ReviewWriteReplace {
		t.Fatalf("review refresh not persisted with replace mode: %#v", store.writes)
	}
}

func TestPoll_IdleSessionForcesFreshBacklogDecisionInsideReviewThrottle(t *testing.T) {
	store := testStoreWithSession()
	now := time.Unix(120, 0).UTC()
	store.sessions[0].Activity = domain.Activity{State: domain.ActivityIdle, LastActivityAt: now.Add(-2 * time.Minute)}
	local := knownPR(1)
	review := ports.SCMReviewObservation{
		Decision: string(domain.ReviewChangesRequest),
		HeadSHA:  local.HeadSHA,
		Threads: []ports.SCMReviewThreadObservation{{
			ID: "t1", Comments: []ports.SCMReviewCommentObservation{{ID: "c1", Author: "alice", Body: "fix"}},
		}},
	}
	local.Review = domain.ReviewChangesRequest
	local.ReviewHash = reviewSemanticHash(review)
	store.prs["p-1"] = []domain.PullRequest{local}
	provider := &fakeProvider{
		repoGuards: map[string]ports.SCMGuardResult{prKey(testRepo, 0): {ETag: "repo", NotModified: true}},
		reviews:    map[string]ports.SCMReviewObservation{prKey(testRepo, 1): review},
	}
	lifecycle := &fakeLifecycle{}
	obs := newTestObserver(store, provider, lifecycle, now)
	obs.Cache.RepoPRListETag[prKey(testRepo, 0)] = "repo"
	obs.Cache.LastReviewPollAt[prKey(testRepo, 1)] = now.Add(-30 * time.Second)

	if err := obs.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if provider.reviewCalls != 1 {
		t.Fatalf("idle backlog refresh calls = %d, want 1 inside throttle window", provider.reviewCalls)
	}
	if len(lifecycle.idleReviewSnapshots) != 1 {
		t.Fatalf("central idle decisions = %d, want 1: %#v", len(lifecycle.idleReviewSnapshots), lifecycle.idleReviewSnapshots)
	}
	if len(lifecycle.observed) != 0 {
		t.Fatalf("unchanged snapshot also fired transition lifecycle: %#v", lifecycle.observed)
	}
}

func TestPoll_HeadChangeForcesExactHeadReviewRefresh(t *testing.T) {
	store := testStoreWithSession()
	local := knownPR(1)
	local.ReviewHash = "reviewed-sha1"
	store.prs["p-1"] = []domain.PullRequest{local}

	current := testObs(1)
	current.PR.HeadSHA = "sha2"
	current.CI.HeadSHA = "sha2"
	review := ports.SCMReviewObservation{Decision: string(domain.ReviewNone), HeadSHA: "sha2"}
	provider := &fakeProvider{
		repoGuards:   map[string]ports.SCMGuardResult{prKey(testRepo, 0): {ETag: "repo"}},
		observations: map[string]ports.SCMObservation{prKey(testRepo, 1): current},
		reviews:      map[string]ports.SCMReviewObservation{prKey(testRepo, 1): review},
	}
	lc := &fakeLifecycle{}
	obs := newTestObserver(store, provider, lc, time.Unix(200, 0).UTC())
	if err := obs.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if provider.reviewCalls != 1 {
		t.Fatalf("review calls = %d, want 1 after head change", provider.reviewCalls)
	}
	if len(lc.observed) != 1 || lc.observed[0].Review.HeadSHA != "sha2" {
		t.Fatalf("lifecycle review snapshot = %#v, want exact head sha2", lc.observed)
	}
}

func TestPoll_CIPassingForcesExactHeadReviewRefreshBeforeReady(t *testing.T) {
	store := testStoreWithSession()
	local := knownPR(1)
	local.CI = domain.CIPending
	local.CIHash = "pending-ci"
	local.Review = domain.ReviewApproved
	local.ReviewHash = "reviewed-sha1"
	store.prs["p-1"] = []domain.PullRequest{local}

	current := testObs(1)
	current.Review.Decision = string(domain.ReviewApproved)
	review := ports.SCMReviewObservation{
		Decision: string(domain.ReviewApproved),
		HeadSHA:  "sha1",
		Reviews: []ports.SCMReviewSummaryObservation{{
			ID: "review-1", Author: "alice", State: string(domain.ReviewApproved), CommitSHA: "sha1",
		}},
	}
	provider := &fakeProvider{
		repoGuards:   map[string]ports.SCMGuardResult{prKey(testRepo, 0): {ETag: "repo"}},
		observations: map[string]ports.SCMObservation{prKey(testRepo, 1): current},
		reviews:      map[string]ports.SCMReviewObservation{prKey(testRepo, 1): review},
	}
	lc := &fakeLifecycle{}
	obs := newTestObserver(store, provider, lc, time.Unix(200, 0).UTC())
	if err := obs.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if provider.reviewCalls != 1 {
		t.Fatalf("review calls = %d, want 1 before CI-only observation becomes ready", provider.reviewCalls)
	}
	if len(lc.observed) != 1 ||
		lc.observed[0].Review.HeadSHA != "sha1" ||
		len(lc.observed[0].Review.Reviews) != 1 {
		t.Fatalf("lifecycle review snapshot = %#v, want exact-head approval facts", lc.observed)
	}
}

func TestPoll_CoordinationRefreshesPassingBlockedHeadWithCachedReviewHashAfterRestart(t *testing.T) {
	store := testStoreWithSession()
	review := ports.SCMReviewObservation{
		Decision: string(domain.ReviewRequired),
		HeadSHA:  "sha1",
	}
	local := knownPR(1)
	local.CI = domain.CIPassing
	local.Review = domain.ReviewRequired
	local.Mergeability = domain.MergeBlocked
	local.ReviewHash = reviewSemanticHash(review)
	store.prs["p-1"] = []domain.PullRequest{local}

	provider := &fakeProvider{
		repoGuards: map[string]ports.SCMGuardResult{prKey(testRepo, 0): {ETag: "repo", NotModified: true}},
		reviews:    map[string]ports.SCMReviewObservation{prKey(testRepo, 1): review},
	}
	coordinator := &fakeReviewCoordinator{}
	obs := New(provider, store, &fakeLifecycle{}, Config{
		Clock: func() time.Time { return time.Unix(200, 0).UTC() }, Tick: time.Hour,
		Logger: quietSlog(), CacheMax: 128, ReviewCoordinator: coordinator,
	})

	if err := obs.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if provider.reviewCalls != 1 {
		t.Fatalf("review calls = %d, want exact-head refresh despite cached hash", provider.reviewCalls)
	}
	if len(coordinator.observed) != 1 {
		t.Fatalf("coordinator observations = %#v, want one complete snapshot", coordinator.observed)
	}
	if len(store.writes) != 0 {
		t.Fatalf("unchanged cached snapshot wrote durable facts again: %#v", store.writes)
	}
	got := coordinator.observed[0]
	if got.Review.HeadSHA != got.PR.HeadSHA || got.Review.HeadSHA != "sha1" || got.Review.Partial {
		t.Fatalf("coordinator snapshot = %#v, want complete exact-head review facts", got)
	}
}

func TestPoll_CoordinationHandoffFailureDoesNotThrottleRetry(t *testing.T) {
	store := testStoreWithSession()
	review := ports.SCMReviewObservation{
		Decision: string(domain.ReviewRequired),
		HeadSHA:  "sha1",
	}
	local := knownPR(1)
	local.CI = domain.CIPassing
	local.Review = domain.ReviewRequired
	local.Mergeability = domain.MergeBlocked
	local.ReviewHash = reviewSemanticHash(review)
	store.prs["p-1"] = []domain.PullRequest{local}
	provider := &fakeProvider{
		repoGuards: map[string]ports.SCMGuardResult{prKey(testRepo, 0): {ETag: "repo", NotModified: true}},
		reviews:    map[string]ports.SCMReviewObservation{prKey(testRepo, 1): review},
	}
	coordinator := &fakeReviewCoordinator{err: errors.New("handoff undelivered")}
	now := time.Unix(200, 0).UTC()
	obs := New(provider, store, &fakeLifecycle{}, Config{
		Clock: func() time.Time { return now }, Tick: time.Hour,
		Logger: quietSlog(), CacheMax: 128, ReviewCoordinator: coordinator,
	})

	if err := obs.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := obs.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	key := prKey(testRepo, 1)
	if provider.reviewCalls != 2 || len(coordinator.observed) != 2 {
		t.Fatalf("undelivered handoff was throttled: review calls=%d coordinator=%d", provider.reviewCalls, len(coordinator.observed))
	}
	if _, ok := obs.Cache.LastReviewPollAt[key]; ok {
		t.Fatalf("undelivered handoff advanced review throttle: %#v", obs.Cache.LastReviewPollAt)
	}
}

func TestCoordinationObservationNeedsReviewSnapshotWhileProviderBlocksOnReview(t *testing.T) {
	obs := testObs(1)
	obs.Review.Decision = string(domain.ReviewRequired)
	obs.Mergeability = ports.SCMMergeabilityObservation{State: string(domain.MergeBlocked)}
	if !coordinationObservationNeedsReviewSnapshot(obs) {
		t.Fatal("passing exact head must fetch review facts even when provider mergeability is review-blocked")
	}
	obs.CI.Summary = string(domain.CIFailing)
	if coordinationObservationNeedsReviewSnapshot(obs) {
		t.Fatal("failing head must not trigger automatic-review snapshot fetch")
	}
}

func TestPoll_UnchangedHashesDoNotWriteOrNotify(t *testing.T) {
	store := testStoreWithSession()
	obsValue := testObs(1)
	local := knownPR(1)
	local.MetadataHash = metadataSemanticHash(obsValue)
	local.CIHash = ciSemanticHash(obsValue.CI)
	local.ReviewHash = reviewSemanticHash(obsValue.Review)
	store.prs["p-1"] = []domain.PullRequest{local}
	provider := &fakeProvider{repoGuards: map[string]ports.SCMGuardResult{prKey(testRepo, 0): {ETag: "repo"}}, observations: map[string]ports.SCMObservation{prKey(testRepo, 1): obsValue}}
	lc := &fakeLifecycle{}
	coordinator := &fakeReviewCoordinator{}
	obs := New(provider, store, lc, Config{
		Clock: func() time.Time { return time.Unix(1, 0).UTC() }, Tick: time.Hour,
		Logger: quietSlog(), CacheMax: 128, ReviewCoordinator: coordinator,
	})
	if err := obs.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(store.writes) != 0 || len(lc.observed) != 0 {
		t.Fatalf("unchanged hashes wrote/notified: writes=%d observed=%d", len(store.writes), len(lc.observed))
	}
	if len(coordinator.observed) != 1 || coordinator.observed[0].PR.HeadSHA != obsValue.PR.HeadSHA {
		t.Fatalf("unchanged durable snapshot was not coordinated: %#v", coordinator.observed)
	}
}

func TestPoll_ReviewHashDrivesPersistenceAndLifecycle(t *testing.T) {
	store := testStoreWithSession()
	local := knownPR(1)
	local.ReviewHash = "old"
	local.Review = domain.ReviewChangesRequest
	store.prs["p-1"] = []domain.PullRequest{local}
	review := ports.SCMReviewObservation{
		Decision: string(domain.ReviewChangesRequest),
		Reviews:  []ports.SCMReviewSummaryObservation{{ID: "review-1", Author: "ann", State: string(domain.ReviewChangesRequest), URL: "https://github.com/o/r/pull/1#pullrequestreview-1", SubmittedAt: time.Unix(199, 0).UTC()}},
		Threads:  []ports.SCMReviewThreadObservation{{ID: "t1", Path: "f.go", Line: 2, Comments: []ports.SCMReviewCommentObservation{{ID: "c1", Author: "ann", Body: "fix this"}}}},
	}
	provider := &fakeProvider{repoGuards: map[string]ports.SCMGuardResult{prKey(testRepo, 0): {ETag: "repo", NotModified: true}}, observations: map[string]ports.SCMObservation{}, reviews: map[string]ports.SCMReviewObservation{prKey(testRepo, 1): review}}
	lc := &fakeLifecycle{}
	obs := newTestObserver(store, provider, lc, time.Unix(200, 0).UTC())
	obs.Cache.RepoPRListETag[prKey(testRepo, 0)] = "repo"
	if err := obs.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(store.writes) == 0 || len(store.writes[0].comments) != 1 {
		t.Fatalf("review change not persisted: %#v", store.writes)
	}
	if len(store.writes[0].reviews) != 1 || store.writes[0].reviews[0].URL != "https://github.com/o/r/pull/1#pullrequestreview-1" {
		t.Fatalf("review summaries not persisted: %#v", store.writes[0].reviews)
	}
	if len(store.writes) != 2 {
		t.Fatalf("review change with lifecycle should write held-back facts then acknowledgement, got %d writes", len(store.writes))
	}
	if store.writes[0].reviewMode != ports.ReviewWriteReplace {
		t.Fatalf("initial review write mode = %v, want replace", store.writes[0].reviewMode)
	}
	if store.writes[1].reviewMode != ports.ReviewWritePreserve || len(store.writes[1].comments) != 0 {
		t.Fatalf("lifecycle acknowledgement should preserve review rows, got mode=%v comments=%d", store.writes[1].reviewMode, len(store.writes[1].comments))
	}
	if len(lc.observed) != 1 || !lc.observed[0].Changed.Review {
		t.Fatalf("review change not notified: %#v", lc.observed)
	}
}

func TestPoll_PartialReviewRefreshUsesMergeMode(t *testing.T) {
	store := testStoreWithSession()
	local := knownPR(1)
	local.ReviewHash = "old"
	local.Review = domain.ReviewChangesRequest
	store.prs["p-1"] = []domain.PullRequest{local}
	review := ports.SCMReviewObservation{
		Decision: string(domain.ReviewChangesRequest),
		Partial:  true,
		Threads:  []ports.SCMReviewThreadObservation{{ID: "t1", Path: "f.go", Line: 2, Comments: []ports.SCMReviewCommentObservation{{ID: "c1", Author: "ann", Body: "fix this"}}}},
	}
	provider := &fakeProvider{
		repoGuards: map[string]ports.SCMGuardResult{prKey(testRepo, 0): {ETag: "repo", NotModified: true}},
		reviews:    map[string]ports.SCMReviewObservation{prKey(testRepo, 1): review},
	}
	obs := newTestObserver(store, provider, nil, time.Unix(210, 0).UTC())
	obs.Cache.RepoPRListETag[prKey(testRepo, 0)] = "repo"
	if err := obs.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(store.writes) != 1 {
		t.Fatalf("writes = %#v, want one partial review merge", store.writes)
	}
	if store.writes[0].reviewMode != ports.ReviewWriteMerge {
		t.Fatalf("review mode = %v, want merge", store.writes[0].reviewMode)
	}
	if store.writes[0].pr.ReviewHash != reviewSemanticHash(review) {
		t.Fatalf("review hash = %q, want partial hash %q", store.writes[0].pr.ReviewHash, reviewSemanticHash(review))
	}
}

func TestPoll_ReviewOnlyRefreshPreservesLocalCIAndMetadata(t *testing.T) {
	store := testStoreWithSession()
	localObs := testObs(1)
	local := knownPR(1)
	local.CI = domain.CIPassing
	local.Review = domain.ReviewChangesRequest
	local.ReviewHash = "old-review"
	local.MetadataHash = metadataSemanticHash(localObs)
	local.CIHash = ciSemanticHash(localObs.CI)
	local.ObservedAt = time.Unix(10, 0).UTC()
	local.CIObservedAt = time.Unix(11, 0).UTC()
	local.ReviewObservedAt = time.Unix(12, 0).UTC()
	store.prs["p-1"] = []domain.PullRequest{local}
	store.checks[local.URL] = []domain.PullRequestCheck{
		{Name: "build", CommitHash: "sha1", Status: domain.PRCheckPassed, Conclusion: "success", URL: "ci"},
		{Name: "stale", CommitHash: "old-sha", Status: domain.PRCheckFailed, Conclusion: "failure", URL: "old-ci", LogTail: "old tail"},
	}
	review := ports.SCMReviewObservation{
		Decision: string(domain.ReviewChangesRequest),
		Threads:  []ports.SCMReviewThreadObservation{{ID: "t1", Path: "f.go", Line: 2, Comments: []ports.SCMReviewCommentObservation{{ID: "c1", Author: "ann", Body: "fix"}}}},
	}
	provider := &fakeProvider{
		repoGuards: map[string]ports.SCMGuardResult{prKey(testRepo, 0): {ETag: "repo", NotModified: true}},
		reviews:    map[string]ports.SCMReviewObservation{prKey(testRepo, 1): review},
	}
	now := time.Unix(200, 0).UTC()
	obs := newTestObserver(store, provider, &fakeLifecycle{}, now)
	obs.Cache.RepoPRListETag[prKey(testRepo, 0)] = "repo"
	if err := obs.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(store.writes) == 0 {
		t.Fatalf("writes = %d, want review-only write", len(store.writes))
	}
	write := store.writes[len(store.writes)-1]
	if write.pr.MetadataHash != local.MetadataHash {
		t.Fatalf("metadata hash changed on review-only refresh: got %q want %q", write.pr.MetadataHash, local.MetadataHash)
	}
	if write.pr.CIHash != local.CIHash {
		t.Fatalf("CI hash changed on review-only refresh: got %q want %q", write.pr.CIHash, local.CIHash)
	}
	if !write.pr.ObservedAt.Equal(local.ObservedAt) {
		t.Fatalf("ObservedAt changed on review-only refresh: got %s want %s", write.pr.ObservedAt, local.ObservedAt)
	}
	if !write.pr.CIObservedAt.Equal(local.CIObservedAt) {
		t.Fatalf("CIObservedAt changed on review-only refresh: got %s want %s", write.pr.CIObservedAt, local.CIObservedAt)
	}
	if !write.pr.ReviewObservedAt.Equal(now) {
		t.Fatalf("ReviewObservedAt = %s, want %s", write.pr.ReviewObservedAt, now)
	}
	if len(write.checks) != 1 || write.checks[0].Name != "build" {
		t.Fatalf("review-only local reconstruction should include current-head checks only: %#v", write.checks)
	}
}

func TestPoll_ReviewFetchFailureDoesNotUpdateReviewDecision(t *testing.T) {
	store := testStoreWithSession()
	local := knownPR(1)
	local.Review = domain.ReviewChangesRequest
	local.ReviewHash = reviewSemanticHash(ports.SCMReviewObservation{Decision: string(domain.ReviewChangesRequest), Threads: []ports.SCMReviewThreadObservation{{ID: "old", Comments: []ports.SCMReviewCommentObservation{{ID: "c-old", Body: "old"}}}}})
	obsValue := testObs(1)
	obsValue.Review.Decision = string(domain.ReviewApproved)
	local.MetadataHash = metadataSemanticHash(obsValue)
	local.CIHash = ciSemanticHash(obsValue.CI)
	store.prs["p-1"] = []domain.PullRequest{local}
	provider := &fakeProvider{
		repoGuards:   map[string]ports.SCMGuardResult{prKey(testRepo, 0): {ETag: "repo2"}},
		observations: map[string]ports.SCMObservation{prKey(testRepo, 1): obsValue},
		reviewErr:    errors.New("review API down"),
	}
	lc := &fakeLifecycle{}
	obs := newTestObserver(store, provider, lc, time.Unix(300, 0).UTC())
	if err := obs.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if provider.reviewCalls != 1 {
		t.Fatalf("reviewCalls = %d, want 1", provider.reviewCalls)
	}
	if len(store.writes) != 0 || len(lc.observed) != 0 {
		t.Fatalf("review fetch failure should not persist/notify stale review data: writes=%#v lifecycle=%#v", store.writes, lc.observed)
	}
	if len(lc.reviewFetchFailures) != 1 || lc.reviewFetchFailures[0].PR.URL != local.URL {
		t.Fatalf("review fetch failure fallback = %#v, want one lifecycle signal for %s", lc.reviewFetchFailures, local.URL)
	}
	if !obs.Cache.ReviewRefreshFailed[prKey(testRepo, 1)] {
		t.Fatalf("review fetch failure was not marked for retry")
	}
}

func TestPoll_SuccessfulReviewRefreshClearsRetryCacheSlot(t *testing.T) {
	store := testStoreWithSession()
	local := knownPR(1)
	local.Review = domain.ReviewChangesRequest
	local.ReviewHash = "old-review"
	store.prs["p-1"] = []domain.PullRequest{local}
	review := ports.SCMReviewObservation{
		Decision: string(domain.ReviewChangesRequest),
		Threads:  []ports.SCMReviewThreadObservation{{ID: "t1", Path: "f.go", Line: 2, Comments: []ports.SCMReviewCommentObservation{{ID: "c1", Body: "fix"}}}},
	}
	provider := &fakeProvider{
		repoGuards: map[string]ports.SCMGuardResult{prKey(testRepo, 0): {ETag: "repo", NotModified: true}},
		reviews:    map[string]ports.SCMReviewObservation{prKey(testRepo, 1): review},
	}
	obs := newTestObserver(store, provider, nil, time.Unix(350, 0).UTC())
	obs.Cache.RepoPRListETag[prKey(testRepo, 0)] = "repo"
	obs.cacheSetBool(obs.Cache.ReviewRefreshFailed, &obs.Cache.reviewFailedOrder, prKey(testRepo, 1), true)

	if err := obs.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := obs.Cache.ReviewRefreshFailed[prKey(testRepo, 1)]; ok {
		t.Fatalf("successful review refresh should delete retry map entry, got %#v", obs.Cache.ReviewRefreshFailed)
	}
	for _, key := range obs.Cache.reviewFailedOrder {
		if key == prKey(testRepo, 1) {
			t.Fatalf("successful review refresh should remove retry order slot, got %#v", obs.Cache.reviewFailedOrder)
		}
	}
}

func TestPoll_DoesNotCommitCommitETagWhenFetchFails(t *testing.T) {
	store := testStoreWithSession()
	local := knownPR(1)
	store.prs["p-1"] = []domain.PullRequest{local}
	provider := &fakeProvider{
		repoGuards:  map[string]ports.SCMGuardResult{prKey(testRepo, 0): {ETag: "repo", NotModified: true}},
		checkGuards: map[string]ports.SCMGuardResult{commitKey(testRepo, "sha1"): {ETag: "ci2"}},
		fetchErr:    errors.New("graphql down"),
	}
	obs := newTestObserver(store, provider, &fakeLifecycle{}, time.Unix(400, 0).UTC())
	obs.Cache.RepoPRListETag[prKey(testRepo, 0)] = "repo"
	obs.Cache.CommitChecksETag[commitKey(testRepo, "sha1")] = "ci1"
	if err := obs.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := obs.Cache.CommitChecksETag[commitKey(testRepo, "sha1")]; got != "ci1" {
		t.Fatalf("commit ETag advanced after failed fetch: got %q want ci1", got)
	}
}

func TestPoll_LifecycleFailureHoldsBackHashesForDurableRetry(t *testing.T) {
	store := testStoreWithSession()
	local := knownPR(1)
	local.MetadataHash = "old-metadata"
	local.CIHash = "old-ci"
	store.prs["p-1"] = []domain.PullRequest{local}
	changed := testObs(1)
	changed.PR.Title = "changed title"
	provider := &fakeProvider{
		repoGuards:   map[string]ports.SCMGuardResult{prKey(testRepo, 0): {ETag: "repo2"}},
		checkGuards:  map[string]ports.SCMGuardResult{commitKey(testRepo, "sha1"): {ETag: "ci2"}},
		observations: map[string]ports.SCMObservation{prKey(testRepo, 1): changed},
	}
	lc := &fakeLifecycle{err: errors.New("lifecycle down")}
	obs := newTestObserver(store, provider, lc, time.Unix(500, 0).UTC())
	obs.Cache.RepoPRListETag[prKey(testRepo, 0)] = "repo1"
	obs.Cache.CommitChecksETag[commitKey(testRepo, "sha1")] = "ci1"
	if err := obs.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(store.writes) != 1 {
		t.Fatalf("DB write should succeed before lifecycle retry is queued, got writes=%#v", store.writes)
	}
	if store.writes[0].pr.Title != "changed title" {
		t.Fatalf("DB write did not persist the observed PR state: %#v", store.writes[0].pr)
	}
	if store.writes[0].pr.MetadataHash != local.MetadataHash {
		t.Fatalf("metadata hash advanced before lifecycle acknowledgement: got %q want %q", store.writes[0].pr.MetadataHash, local.MetadataHash)
	}
	if store.writes[0].pr.CIHash != local.CIHash {
		t.Fatalf("CI hash advanced before lifecycle acknowledgement: got %q want %q", store.writes[0].pr.CIHash, local.CIHash)
	}
	if got := obs.Cache.RepoPRListETag[prKey(testRepo, 0)]; got != "repo1" {
		t.Fatalf("repo ETag advanced after lifecycle failure: got %q want repo1", got)
	}
	if got := obs.Cache.CommitChecksETag[commitKey(testRepo, "sha1")]; got != "ci1" {
		t.Fatalf("commit ETag advanced after lifecycle failure: got %q want ci1", got)
	}

	lc.err = nil
	store.prs["p-1"] = []domain.PullRequest{store.writes[0].pr}
	restarted := newTestObserver(store, provider, lc, time.Unix(501, 0).UTC())
	if err := restarted.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(lc.observed) != 1 {
		t.Fatalf("held-back semantic hashes did not force lifecycle retry after restart: %#v", lc.observed)
	}
	if len(store.writes) != 3 {
		t.Fatalf("retry should write held-back facts then acknowledge hashes, got writes=%d", len(store.writes))
	}
	last := store.writes[len(store.writes)-1].pr
	if last.MetadataHash != metadataSemanticHash(changed) {
		t.Fatalf("metadata hash not acknowledged after lifecycle success: got %q want %q", last.MetadataHash, metadataSemanticHash(changed))
	}
	if last.CIHash != ciSemanticHash(changed.CI) {
		t.Fatalf("CI hash not acknowledged after lifecycle success: got %q want %q", last.CIHash, ciSemanticHash(changed.CI))
	}
}

func TestPoll_WriteFailureDoesNotAdvanceGuardETags(t *testing.T) {
	store := testStoreWithSession()
	store.writeErr = errors.New("db down")
	local := knownPR(1)
	local.MetadataHash = "old-metadata"
	local.CIHash = "old-ci"
	store.prs["p-1"] = []domain.PullRequest{local}
	changed := testObs(1)
	changed.PR.Title = "changed title"
	provider := &fakeProvider{
		repoGuards:   map[string]ports.SCMGuardResult{prKey(testRepo, 0): {ETag: "repo2"}},
		checkGuards:  map[string]ports.SCMGuardResult{commitKey(testRepo, "sha1"): {ETag: "ci2"}},
		observations: map[string]ports.SCMObservation{prKey(testRepo, 1): changed},
	}
	obs := newTestObserver(store, provider, &fakeLifecycle{}, time.Unix(550, 0).UTC())
	obs.Cache.RepoPRListETag[prKey(testRepo, 0)] = "repo1"
	obs.Cache.CommitChecksETag[commitKey(testRepo, "sha1")] = "ci1"
	if err := obs.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := obs.Cache.RepoPRListETag[prKey(testRepo, 0)]; got != "repo1" {
		t.Fatalf("repo ETag advanced after write failure: got %q want repo1", got)
	}
	if got := obs.Cache.CommitChecksETag[commitKey(testRepo, "sha1")]; got != "ci1" {
		t.Fatalf("commit ETag advanced after write failure: got %q want ci1", got)
	}
}

func TestPoll_DuplicateTrackedPRKeepsFirstSession(t *testing.T) {
	store := &fakeStore{
		sessions: []domain.SessionRecord{
			{ID: "p-1", ProjectID: "p", Metadata: domain.SessionMetadata{Branch: "feat"}},
			{ID: "p-2", ProjectID: "p", Metadata: domain.SessionMetadata{Branch: "feat"}},
		},
		projects: map[string]domain.ProjectRecord{"p": {ID: "p", RepoOriginURL: "https://github.com/o/r.git"}},
		prs:      map[domain.SessionID][]domain.PullRequest{},
		checks:   map[string][]domain.PullRequestCheck{},
	}
	pr1 := knownPR(1)
	pr1.MetadataHash = "old-1"
	pr2 := pr1
	pr2.SessionID = "p-2"
	store.prs["p-1"] = []domain.PullRequest{pr1}
	store.prs["p-2"] = []domain.PullRequest{pr2}
	provider := &fakeProvider{
		repoGuards:   map[string]ports.SCMGuardResult{prKey(testRepo, 0): {ETag: "repo2"}},
		observations: map[string]ports.SCMObservation{prKey(testRepo, 1): testObs(1)},
	}
	obs := newTestObserver(store, provider, &fakeLifecycle{}, time.Unix(600, 0).UTC())
	if err := obs.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(store.writes) == 0 {
		t.Fatalf("writes = %d, want exactly one owner write", len(store.writes))
	}
	if store.writes[0].pr.SessionID != "p-1" {
		t.Fatalf("duplicate owner overwrote first session: wrote session %s", store.writes[0].pr.SessionID)
	}
}

// TestDiscoverSubjects_BackfillsRepoOriginURL asserts that a project row with
// an empty RepoOriginURL but a real on-disk repo gets its origin filled in
// during discovery and persisted, so the same project becomes observable
// without re-running project add.
func TestDiscoverSubjects_BackfillsRepoOriginURL(t *testing.T) {
	dir := t.TempDir()
	if out, err := exec.Command("git", "init", dir).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v (%s)", err, out)
	}
	if out, err := exec.Command("git", "-C", dir, "remote", "add", "origin", "https://github.com/o/r.git").CombinedOutput(); err != nil {
		t.Fatalf("git remote add: %v (%s)", err, out)
	}

	store := &fakeStore{
		sessions: []domain.SessionRecord{{ID: "p-1", ProjectID: "p", Metadata: domain.SessionMetadata{Branch: "feat"}}},
		projects: map[string]domain.ProjectRecord{"p": {ID: "p", Path: dir}}, // empty RepoOriginURL
		prs:      map[domain.SessionID][]domain.PullRequest{},
		checks:   map[string][]domain.PullRequestCheck{},
	}
	provider := &fakeProvider{}
	obs := newTestObserver(store, provider, &fakeLifecycle{}, time.Unix(0, 0).UTC())

	if _, _, err := obs.discoverSubjects(context.Background()); err != nil {
		t.Fatalf("discoverSubjects: %v", err)
	}
	if got := store.projects["p"].RepoOriginURL; got != "https://github.com/o/r.git" {
		t.Fatalf("RepoOriginURL after backfill = %q, want https://github.com/o/r.git", got)
	}
}

// TestDiscoverSubjects_NonGitPathDoesNotBackfill confirms the backfill is
// best-effort: a non-git project path leaves RepoOriginURL empty without
// erroring or persisting a stub, so the project is simply skipped.
func TestDiscoverSubjects_NonGitPathDoesNotBackfill(t *testing.T) {
	dir := t.TempDir()
	store := &fakeStore{
		sessions: []domain.SessionRecord{{ID: "p-1", ProjectID: "p", Metadata: domain.SessionMetadata{Branch: "feat"}}},
		projects: map[string]domain.ProjectRecord{"p": {ID: "p", Path: dir}},
		prs:      map[domain.SessionID][]domain.PullRequest{},
		checks:   map[string][]domain.PullRequestCheck{},
	}
	obs := newTestObserver(store, &fakeProvider{}, &fakeLifecycle{}, time.Unix(0, 0).UTC())
	subjects, sessionRepos, err := obs.discoverSubjects(context.Background())
	if err != nil {
		t.Fatalf("discoverSubjects: %v", err)
	}
	if len(subjects) != 0 || len(sessionRepos) != 0 {
		t.Fatalf("non-git project should be skipped, got %d subjects %d sessionRepos", len(subjects), len(sessionRepos))
	}
	if got := store.projects["p"].RepoOriginURL; got != "" {
		t.Fatalf("RepoOriginURL = %q, want empty (no persist on failed backfill)", got)
	}
}
