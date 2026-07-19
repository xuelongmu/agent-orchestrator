package trackerintake

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func TestPollSpawnsWorkerForEligibleIssue(t *testing.T) {
	store := &fakeStore{
		projects: []domain.ProjectRecord{{
			ID:            "demo",
			RepoOriginURL: "https://github.com/acme/demo.git",
			Config: domain.ProjectConfig{TrackerIntake: domain.TrackerIntakeConfig{
				Enabled:  true,
				Assignee: "alice",
			}},
		}},
	}
	tracker := &fakeTracker{issues: []domain.Issue{{
		ID:        domain.TrackerID{Provider: domain.TrackerProviderGitHub, Native: "acme/demo#12"},
		Title:     "Fix login",
		Body:      "The login form submits twice.",
		State:     domain.IssueOpen,
		URL:       "https://github.com/acme/demo/issues/12",
		Labels:    []string{"agent-ready"},
		Assignees: []string{"alice"},
	}}}
	spawner := &fakeSpawner{}

	if err := New(singleResolver(tracker), store, spawner, Config{Logger: discardLogger()}).Poll(context.Background()); err != nil {
		t.Fatalf("Poll() error = %v", err)
	}
	if len(spawner.calls) != 1 {
		t.Fatalf("spawn calls = %d, want 1", len(spawner.calls))
	}
	call := spawner.calls[0]
	if call.ProjectID != "demo" || call.Kind != domain.KindWorker {
		t.Fatalf("spawn config = %+v", call)
	}
	if call.IssueID != "github:acme/demo#12" {
		t.Fatalf("IssueID = %q, want canonical github id", call.IssueID)
	}
	if !strings.Contains(call.Prompt, "Fix login") || !strings.Contains(call.Prompt, "The login form submits twice.") {
		t.Fatalf("prompt missing issue context:\n%s", call.Prompt)
	}
	if len(tracker.filters) != 1 {
		t.Fatalf("tracker filters = %d, want 1", len(tracker.filters))
	}
	if got := tracker.filters[0]; got.State != domain.ListOpen || got.Assignee != "alice" || len(got.Labels) != 0 {
		t.Fatalf("tracker filter = %+v", got)
	}
	if len(store.claimCalls) != 1 || store.claimCalls[0].Provider != domain.TrackerProviderGitHub || store.claimCalls[0].Repo != "acme/demo" || store.claimCalls[0].IssueID != "acme/demo#12" {
		t.Fatalf("claim calls = %+v, want project/provider/repo/issue scoped claim", store.claimCalls)
	}
	if len(store.completeCalls) != 1 {
		t.Fatalf("completed claims = %d, want 1", len(store.completeCalls))
	}
}

func TestPollCompletedClaimPreventsRespawnAcrossObserverRecreate(t *testing.T) {
	store := &fakeStore{projects: []domain.ProjectRecord{{
		ID: "demo", RepoOriginURL: "https://github.com/acme/demo.git",
		Config: domain.ProjectConfig{TrackerIntake: domain.TrackerIntakeConfig{Enabled: true, Assignee: "alice", Repo: "AcMe/DeMo"}},
	}}}
	tracker := &fakeTracker{issues: []domain.Issue{{
		ID:    domain.TrackerID{Provider: domain.TrackerProviderGitHub, Native: "ACME/DEMO#12"},
		State: domain.IssueOpen, Assignees: []string{"alice"},
	}}}
	spawner := &fakeSpawner{}
	cfg := Config{Logger: discardLogger(), Token: func() string { return "owner-a" }}
	if err := New(singleResolver(tracker), store, spawner, cfg).Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	store.sessions[0].IsTerminated = true // leave capacity so dedup, not fullness, decides the second poll
	store.projects[0].Config.TrackerIntake.Repo = "aCmE/dEmO"
	tracker.issues[0].ID.Native = "Acme/Demo#12"
	cfg.Token = func() string { return "owner-b" }
	if err := New(singleResolver(tracker), store, spawner, cfg).Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(spawner.calls) != 1 {
		t.Fatalf("spawn calls across observer recreate = %d, want 1", len(spawner.calls))
	}
	if len(store.claimCalls) != 2 {
		t.Fatalf("claim calls across observer recreate = %d, want durable second claim check", len(store.claimCalls))
	}
	for _, claim := range store.claimCalls {
		if claim.Repo != "acme/demo" || claim.IssueID != "acme/demo#12" {
			t.Fatalf("non-canonical GitHub claim = %+v", claim)
		}
	}
	if spawner.calls[0].IssueID != "github:acme/demo#12" {
		t.Fatalf("persisted issue id = %q, want case-normalized GitHub identity", spawner.calls[0].IssueID)
	}
}

func TestPollFailedSpawnReleasesClaimForRetry(t *testing.T) {
	store := &fakeStore{projects: []domain.ProjectRecord{{
		ID: "demo", RepoOriginURL: "https://github.com/acme/demo.git",
		Config: domain.ProjectConfig{TrackerIntake: domain.TrackerIntakeConfig{Enabled: true, Assignee: "alice"}},
	}}}
	tracker := &fakeTracker{issues: []domain.Issue{{
		ID:    domain.TrackerID{Provider: domain.TrackerProviderGitHub, Native: "acme/demo#12"},
		State: domain.IssueOpen, Assignees: []string{"alice"},
	}}}
	spawner := &fakeSpawner{failIssue: "github:acme/demo#12"}
	if err := New(singleResolver(tracker), store, spawner, Config{Logger: discardLogger(), Token: func() string { return "owner-a" }}).Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	spawner.failIssue = ""
	if err := New(singleResolver(tracker), store, spawner, Config{Logger: discardLogger(), Token: func() string { return "owner-b" }}).Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(spawner.calls) != 2 {
		t.Fatalf("spawn attempts = %d, want failed attempt plus retry", len(spawner.calls))
	}
	if len(store.releaseCalls) != 1 || len(store.completeCalls) != 1 {
		t.Fatalf("release calls=%d complete calls=%d, want one each", len(store.releaseCalls), len(store.completeCalls))
	}
}

func TestPollRejectsLateSpawnSuccessAfterTakeoverBeforeFirstRenewal(t *testing.T) {
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	store := &fakeStore{projects: []domain.ProjectRecord{{
		ID: "demo", RepoOriginURL: "https://github.com/acme/demo.git",
		Config: domain.ProjectConfig{TrackerIntake: domain.TrackerIntakeConfig{Enabled: true, Assignee: "alice"}},
	}}}
	tracker := &fakeTracker{issues: []domain.Issue{{
		ID:    domain.TrackerID{Provider: domain.TrackerProviderGitHub, Native: "acme/demo#12"},
		State: domain.IssueOpen, Assignees: []string{"alice"},
	}}}
	spawner := &fakeSpawner{}
	spawner.hook = func(ports.SpawnConfig) {
		now = now.Add(6 * time.Minute)
		successor := ports.TrackerIntakeClaim{
			ProjectID: "demo", Provider: domain.TrackerProviderGitHub, Repo: "acme/demo",
			IssueID: "acme/demo#12", OwnerToken: "owner-b",
			ClaimedAt: now, LeaseExpiresAt: now.Add(5 * time.Minute),
		}
		if result, err := store.ClaimTrackerIntakeIssue(context.Background(), successor, 3); err != nil || result != ports.TrackerIntakeClaimAcquired {
			t.Fatalf("takeover during spawn result=%v err=%v", result, err)
		}
	}
	observer := New(singleResolver(tracker), store, spawner, Config{
		Clock: func() time.Time { return now }, ClaimLease: 5 * time.Minute,
		Token: func() string { return "owner-a" }, Logger: discardLogger(),
	})
	if err := observer.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(spawner.calls) != 1 {
		t.Fatalf("spawn calls=%d, want late success attempt", len(spawner.calls))
	}
	if len(store.completeCalls) != 0 {
		t.Fatalf("stale completion calls=%d, want final fence to reject before completion", len(store.completeCalls))
	}
	key := fakeClaimKey(store.claimCalls[0])
	if got := store.claims[key].OwnerToken; got != "owner-b" {
		t.Fatalf("claim owner after delayed release=%q, want successor owner-b", got)
	}
}

func TestPollRespectsPerProjectConcurrencyLimit(t *testing.T) {
	store := &fakeStore{
		projects: []domain.ProjectRecord{{
			ID:            "demo",
			RepoOriginURL: "https://github.com/acme/demo.git",
			Config: domain.ProjectConfig{TrackerIntake: domain.TrackerIntakeConfig{
				Enabled:       true,
				Assignee:      "alice",
				MaxConcurrent: 3,
			}},
		}},
		sessions: []domain.SessionRecord{
			{ID: "demo-1", ProjectID: "demo", Kind: domain.KindWorker},
			{ID: "demo-2", ProjectID: "demo", Kind: domain.KindWorker},
			{ID: "demo-orch", ProjectID: "demo", Kind: domain.KindOrchestrator},
			{ID: "demo-old", ProjectID: "demo", Kind: domain.KindWorker, IsTerminated: true},
			{ID: "other-1", ProjectID: "other", Kind: domain.KindWorker},
		},
	}
	tracker := &fakeTracker{issues: []domain.Issue{
		{ID: domain.TrackerID{Provider: domain.TrackerProviderGitHub, Native: "acme/demo#1"}, State: domain.IssueOpen, Assignees: []string{"alice"}},
		{ID: domain.TrackerID{Provider: domain.TrackerProviderGitHub, Native: "acme/demo#2"}, State: domain.IssueOpen, Assignees: []string{"alice"}},
	}}
	spawner := &fakeSpawner{}

	if err := New(singleResolver(tracker), store, spawner, Config{Logger: discardLogger()}).Poll(context.Background()); err != nil {
		t.Fatalf("Poll() error = %v", err)
	}
	if len(spawner.calls) != 1 {
		t.Fatalf("spawn calls = %d, want 1 available slot", len(spawner.calls))
	}
	if len(tracker.filters) != 1 {
		t.Fatalf("tracker filters = %+v, want one query", tracker.filters)
	}
}

func TestPollSkipsTrackerWhenProjectAtConcurrencyLimit(t *testing.T) {
	store := &fakeStore{
		projects: []domain.ProjectRecord{{
			ID:            "demo",
			RepoOriginURL: "https://github.com/acme/demo.git",
			Config: domain.ProjectConfig{TrackerIntake: domain.TrackerIntakeConfig{
				Enabled:       true,
				Assignee:      "alice",
				MaxConcurrent: 1,
			}},
		}},
		sessions: []domain.SessionRecord{{ID: "demo-1", ProjectID: "demo", Kind: domain.KindWorker}},
	}
	tracker := &fakeTracker{}

	if err := New(singleResolver(tracker), store, &fakeSpawner{}, Config{Logger: discardLogger()}).Poll(context.Background()); err != nil {
		t.Fatalf("Poll() error = %v", err)
	}
	if len(tracker.repos) != 0 {
		t.Fatalf("tracker calls = %d, want 0 at capacity", len(tracker.repos))
	}
}

func TestPollReconcilesExistingIssueSessionsAfterRestart(t *testing.T) {
	store := &fakeStore{
		projects: []domain.ProjectRecord{{
			ID:            "demo",
			RepoOriginURL: "https://github.com/acme/demo.git",
			Config:        domain.ProjectConfig{TrackerIntake: domain.TrackerIntakeConfig{Enabled: true, Assignee: "alice"}},
		}},
		sessions: []domain.SessionRecord{{ID: "demo-1", ProjectID: "demo", IssueID: "github:acme/demo#12"}},
	}
	tracker := &fakeTracker{issues: []domain.Issue{{
		ID:        domain.TrackerID{Provider: domain.TrackerProviderGitHub, Native: "acme/demo#12"},
		Title:     "Already running",
		State:     domain.IssueOpen,
		Assignees: []string{"alice"},
	}}}
	spawner := &fakeSpawner{}

	if err := New(singleResolver(tracker), store, spawner, Config{Logger: discardLogger()}).Poll(context.Background()); err != nil {
		t.Fatalf("Poll() error = %v", err)
	}
	if len(spawner.calls) != 0 {
		t.Fatalf("spawn calls = %d, want 0", len(spawner.calls))
	}
}

func TestPollSkipsCapacityReadWhenIntakeDisabled(t *testing.T) {
	store := &fakeStore{
		projects:    []domain.ProjectRecord{{ID: "demo"}},
		capacityErr: errors.New("capacity read should not run"),
	}

	if err := New(singleResolver(&fakeTracker{}), store, &fakeSpawner{}, Config{Logger: discardLogger()}).Poll(context.Background()); err != nil {
		t.Fatalf("Poll() error = %v, want nil", err)
	}
}

func TestPollSkipsIneligibleAndInvalidProjects(t *testing.T) {
	store := &fakeStore{
		projects: []domain.ProjectRecord{
			{ID: "off", RepoOriginURL: "https://github.com/acme/off.git"},
			{ID: "broad", RepoOriginURL: "https://github.com/acme/broad.git", Config: domain.ProjectConfig{TrackerIntake: domain.TrackerIntakeConfig{Enabled: true}}},
			{ID: "missing-origin", Config: domain.ProjectConfig{TrackerIntake: domain.TrackerIntakeConfig{Enabled: true, Assignee: "alice"}}},
		},
	}
	tracker := &fakeTracker{issues: []domain.Issue{{
		ID:    domain.TrackerID{Provider: domain.TrackerProviderGitHub, Native: "acme/off#1"},
		Title: "ignored",
		State: domain.IssueOpen,
	}}}
	spawner := &fakeSpawner{}

	if err := New(singleResolver(tracker), store, spawner, Config{Logger: discardLogger()}).Poll(context.Background()); err != nil {
		t.Fatalf("Poll() error = %v", err)
	}
	if len(tracker.repos) != 0 {
		t.Fatalf("tracker was called for invalid/off projects: %+v", tracker.repos)
	}
	if len(spawner.calls) != 0 {
		t.Fatalf("spawn calls = %d, want 0", len(spawner.calls))
	}
}

func TestPollContinuesAfterTrackerAndSpawnFailures(t *testing.T) {
	store := &fakeStore{projects: []domain.ProjectRecord{
		{ID: "bad", RepoOriginURL: "https://github.com/acme/bad.git", Config: domain.ProjectConfig{TrackerIntake: domain.TrackerIntakeConfig{Enabled: true, Assignee: "alice"}}},
		{ID: "good", RepoOriginURL: "https://github.com/acme/good.git", Config: domain.ProjectConfig{TrackerIntake: domain.TrackerIntakeConfig{Enabled: true, Assignee: "alice"}}},
	}}
	tracker := &fakeTracker{
		failRepos: map[string]error{"acme/bad": errors.New("rate limited")},
		issuesByRepo: map[string][]domain.Issue{
			"acme/good": {
				{ID: domain.TrackerID{Provider: domain.TrackerProviderGitHub, Native: "acme/good#1"}, Title: "first", State: domain.IssueOpen, Assignees: []string{"alice"}},
				{ID: domain.TrackerID{Provider: domain.TrackerProviderGitHub, Native: "acme/good#2"}, Title: "second", State: domain.IssueOpen, Assignees: []string{"alice"}},
			},
		},
	}
	spawner := &fakeSpawner{failIssue: domain.IssueID("github:acme/good#1")}

	if err := New(singleResolver(tracker), store, spawner, Config{Logger: discardLogger()}).Poll(context.Background()); err != nil {
		t.Fatalf("Poll() error = %v", err)
	}
	if len(spawner.calls) != 2 {
		t.Fatalf("spawn attempts = %d, want 2", len(spawner.calls))
	}
	if spawner.calls[1].IssueID != "github:acme/good#2" {
		t.Fatalf("second spawn issue = %q", spawner.calls[1].IssueID)
	}
}

func TestPollBacksOffProjectAfterFailure(t *testing.T) {
	now := time.Date(2026, 6, 27, 10, 0, 0, 0, time.UTC)
	store := &fakeStore{projects: []domain.ProjectRecord{{
		ID:            "demo",
		RepoOriginURL: "https://github.com/acme/demo.git",
		Config:        domain.ProjectConfig{TrackerIntake: domain.TrackerIntakeConfig{Enabled: true, Assignee: "alice"}},
	}}}
	tracker := &fakeTracker{failRepos: map[string]error{"acme/demo": errors.New("rate limited")}}
	observer := New(singleResolver(tracker), store, &fakeSpawner{}, Config{
		Clock:          func() time.Time { return now },
		FailureBackoff: time.Minute,
		Logger:         discardLogger(),
	})

	if err := observer.Poll(context.Background()); err != nil {
		t.Fatalf("first Poll() error = %v", err)
	}
	if len(tracker.repos) != 1 {
		t.Fatalf("tracker calls after first poll = %d, want 1", len(tracker.repos))
	}

	if err := observer.Poll(context.Background()); err != nil {
		t.Fatalf("second Poll() error = %v", err)
	}
	if len(tracker.repos) != 1 {
		t.Fatalf("tracker calls during backoff = %d, want still 1", len(tracker.repos))
	}

	now = now.Add(time.Minute + time.Nanosecond)
	if err := observer.Poll(context.Background()); err != nil {
		t.Fatalf("third Poll() error = %v", err)
	}
	if len(tracker.repos) != 2 {
		t.Fatalf("tracker calls after backoff = %d, want 2", len(tracker.repos))
	}
}

func TestPollSamplesClaimClockAfterSlowTrackerList(t *testing.T) {
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	store := &fakeStore{projects: []domain.ProjectRecord{{
		ID: "demo", RepoOriginURL: "https://github.com/acme/demo.git",
		Config: domain.ProjectConfig{TrackerIntake: domain.TrackerIntakeConfig{Enabled: true, Assignee: "alice"}},
	}}}
	tracker := &fakeTracker{
		issues: []domain.Issue{{
			ID:    domain.TrackerID{Provider: domain.TrackerProviderGitHub, Native: "acme/demo#28"},
			State: domain.IssueOpen, Assignees: []string{"alice"},
		}},
		listHook: func() { now = now.Add(10 * time.Minute) },
	}
	observer := New(singleResolver(tracker), store, &fakeSpawner{}, Config{
		Clock: func() time.Time { return now }, ClaimLease: 5 * time.Minute,
		Token: func() string { return "owner-a" }, Logger: discardLogger(),
	})
	if err := observer.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(store.claimCalls) != 1 {
		t.Fatalf("claim calls=%d, want 1", len(store.claimCalls))
	}
	claim := store.claimCalls[0]
	if !claim.ClaimedAt.Equal(now) || !claim.LeaseExpiresAt.Equal(now.Add(5*time.Minute)) {
		t.Fatalf("claim times after slow list = %s..%s, want %s..%s", claim.ClaimedAt, claim.LeaseExpiresAt, now, now.Add(5*time.Minute))
	}
}

func TestPollSkipsNonOpenIssueStates(t *testing.T) {
	store := &fakeStore{projects: []domain.ProjectRecord{{
		ID:            "demo",
		RepoOriginURL: "https://github.com/acme/demo.git",
		Config:        domain.ProjectConfig{TrackerIntake: domain.TrackerIntakeConfig{Enabled: true, Assignee: "alice"}},
	}}}
	tracker := &fakeTracker{issues: []domain.Issue{
		{ID: domain.TrackerID{Provider: domain.TrackerProviderGitHub, Native: "acme/demo#1"}, Title: "already active", State: domain.IssueInProgress, Assignees: []string{"alice"}},
		{ID: domain.TrackerID{Provider: domain.TrackerProviderGitHub, Native: "acme/demo#2"}, Title: "ready", State: domain.IssueOpen, Assignees: []string{"alice"}},
	}}
	spawner := &fakeSpawner{}

	if err := New(singleResolver(tracker), store, spawner, Config{Logger: discardLogger()}).Poll(context.Background()); err != nil {
		t.Fatalf("Poll() error = %v", err)
	}
	if len(spawner.calls) != 1 || spawner.calls[0].IssueID != "github:acme/demo#2" {
		t.Fatalf("spawn calls = %+v, want only open issue #2", spawner.calls)
	}
}

func TestPollAppliesLocalEligibilityFilter(t *testing.T) {
	store := &fakeStore{projects: []domain.ProjectRecord{{
		ID:            "demo",
		RepoOriginURL: "https://github.com/acme/demo.git",
		Config:        domain.ProjectConfig{TrackerIntake: domain.TrackerIntakeConfig{Enabled: true, Assignee: "alice"}},
	}}}
	tracker := &fakeTracker{issues: []domain.Issue{
		{ID: domain.TrackerID{Provider: domain.TrackerProviderGitHub, Native: "acme/demo#1"}, Title: "unassigned", State: domain.IssueOpen},
		{ID: domain.TrackerID{Provider: domain.TrackerProviderGitHub, Native: "acme/demo#2"}, Title: "wrong assignee", State: domain.IssueOpen, Assignees: []string{"bob"}},
		{ID: domain.TrackerID{Provider: domain.TrackerProviderGitHub, Native: "acme/demo#3"}, Title: "eligible", State: domain.IssueOpen, Labels: []string{"Agent-Ready"}, Assignees: []string{"Alice"}},
	}}
	spawner := &fakeSpawner{}

	if err := New(singleResolver(tracker), store, spawner, Config{Logger: discardLogger()}).Poll(context.Background()); err != nil {
		t.Fatalf("Poll() error = %v", err)
	}
	if len(spawner.calls) != 1 || spawner.calls[0].IssueID != "github:acme/demo#3" {
		t.Fatalf("spawn calls = %+v, want only eligible issue #3", spawner.calls)
	}
}

func TestIssueMatchesConfigAssigneeSpecialValues(t *testing.T) {
	assigned := domain.Issue{Assignees: []string{"alice"}}
	unassigned := domain.Issue{}
	if !issueMatchesConfig(assigned, domain.TrackerIntakeConfig{Assignee: "*"}) {
		t.Fatal("assigned issue should match assignee=*")
	}
	if issueMatchesConfig(unassigned, domain.TrackerIntakeConfig{Assignee: "*"}) {
		t.Fatal("unassigned issue should not match assignee=*")
	}
	if !issueMatchesConfig(unassigned, domain.TrackerIntakeConfig{Assignee: "none"}) {
		t.Fatal("unassigned issue should match assignee=none")
	}
	if issueMatchesConfig(assigned, domain.TrackerIntakeConfig{Assignee: "none"}) {
		t.Fatal("assigned issue should not match assignee=none")
	}
}

func TestBuildIssuePromptCapsLargeIssueBody(t *testing.T) {
	prompt := BuildIssuePrompt(domain.Issue{
		ID:    domain.TrackerID{Provider: domain.TrackerProviderGitHub, Native: "acme/demo#99"},
		Title: "Large issue",
		URL:   "https://github.com/acme/demo/issues/99",
		Body:  strings.Repeat("body ", 2000),
	})
	if len(prompt) > maxIntakePromptLen {
		t.Fatalf("prompt length = %d, want <= %d", len(prompt), maxIntakePromptLen)
	}
	if !strings.Contains(prompt, "Issue content truncated") {
		t.Fatalf("prompt missing truncation notice:\n%s", prompt)
	}
	if !strings.Contains(prompt, "https://github.com/acme/demo/issues/99") {
		t.Fatalf("prompt missing issue URL:\n%s", prompt)
	}
	if !strings.HasSuffix(prompt, intakePromptFooter) {
		t.Fatalf("prompt missing footer:\n%s", prompt)
	}
}

func TestTrackerRepoUsesConfiguredRepo(t *testing.T) {
	project := domain.ProjectRecord{
		ID:            "demo",
		RepoOriginURL: "https://github.com/wrong/repo.git",
		Config: domain.ProjectConfig{TrackerIntake: domain.TrackerIntakeConfig{
			Enabled:  true,
			Repo:     "AcMe/DeMo",
			Assignee: "alice",
		}},
	}
	repo, ok := trackerRepo(project, project.Config.TrackerIntake.WithDefaults())
	if !ok {
		t.Fatal("trackerRepo ok = false")
	}
	if repo.Native != "acme/demo" {
		t.Fatalf("repo.Native = %q, want acme/demo", repo.Native)
	}
}

func singleResolver(tracker ports.Tracker) TrackerResolver {
	return SingleTrackerResolver{Provider: domain.TrackerProviderGitHub, Adapter: tracker}
}

type fakeStore struct {
	mu            sync.Mutex
	projects      []domain.ProjectRecord
	sessions      []domain.SessionRecord
	capacityErr   error
	claimErr      error
	completeErr   error
	releaseErr    error
	claims        map[string]ports.TrackerIntakeClaim
	completed     map[string]domain.SessionID
	claimCalls    []ports.TrackerIntakeClaim
	completeCalls []ports.TrackerIntakeClaim
	renewCalls    []ports.TrackerIntakeClaim
	releaseCalls  []ports.TrackerIntakeClaim
	capacityReads int
}

func (f *fakeStore) ListProjects(context.Context) ([]domain.ProjectRecord, error) {
	return append([]domain.ProjectRecord(nil), f.projects...), nil
}

func (f *fakeStore) TrackerIntakeHasCapacity(_ context.Context, projectID domain.ProjectID, maxConcurrent int, now time.Time) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.capacityReads++
	if f.capacityErr != nil {
		return false, f.capacityErr
	}
	return f.capacityUsed(projectID, now) < maxConcurrent, nil
}

func (f *fakeStore) ClaimTrackerIntakeIssue(_ context.Context, claim ports.TrackerIntakeClaim, maxConcurrent int) (ports.TrackerIntakeClaimResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.claimCalls = append(f.claimCalls, claim)
	if f.claimErr != nil {
		return 0, f.claimErr
	}
	key := fakeClaimKey(claim)
	canonical := domain.IssueID(string(claim.Provider) + ":" + claim.IssueID)
	for _, session := range f.sessions {
		if session.ProjectID == claim.ProjectID && session.IssueID == canonical {
			if f.completed == nil {
				f.completed = map[string]domain.SessionID{}
			}
			f.completed[key] = session.ID
			return ports.TrackerIntakeClaimAlreadyProcessed, nil
		}
	}
	if _, ok := f.completed[key]; ok {
		return ports.TrackerIntakeClaimAlreadyProcessed, nil
	}
	if existing, ok := f.claims[key]; ok && existing.LeaseExpiresAt.After(claim.ClaimedAt) {
		if existing.OwnerToken == claim.OwnerToken {
			return ports.TrackerIntakeClaimAcquired, nil
		}
		return ports.TrackerIntakeClaimBusy, nil
	}
	if f.capacityUsed(claim.ProjectID, claim.ClaimedAt) >= maxConcurrent {
		return ports.TrackerIntakeClaimCapacityReached, nil
	}
	if f.claims == nil {
		f.claims = map[string]ports.TrackerIntakeClaim{}
	}
	f.claims[key] = claim
	return ports.TrackerIntakeClaimAcquired, nil
}

func (f *fakeStore) CompleteTrackerIntakeIssue(_ context.Context, claim ports.TrackerIntakeClaim, sessionID domain.SessionID, _ time.Time) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.completeCalls = append(f.completeCalls, claim)
	if f.completeErr != nil {
		return false, f.completeErr
	}
	key := fakeClaimKey(claim)
	existing, ok := f.claims[key]
	if !ok || existing.OwnerToken != claim.OwnerToken {
		return false, nil
	}
	delete(f.claims, key)
	if f.completed == nil {
		f.completed = map[string]domain.SessionID{}
	}
	f.completed[key] = sessionID
	f.sessions = append(f.sessions, domain.SessionRecord{
		ID: sessionID, ProjectID: claim.ProjectID, Kind: domain.KindWorker,
		IssueID: domain.IssueID(string(claim.Provider) + ":" + claim.IssueID),
	})
	return true, nil
}

func (f *fakeStore) RenewTrackerIntakeIssue(_ context.Context, claim ports.TrackerIntakeClaim, _ time.Time, leaseExpiresAt time.Time) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.renewCalls = append(f.renewCalls, claim)
	key := fakeClaimKey(claim)
	if _, ok := f.completed[key]; ok {
		return true, nil
	}
	existing, ok := f.claims[key]
	if !ok || existing.OwnerToken != claim.OwnerToken {
		return false, nil
	}
	existing.LeaseExpiresAt = leaseExpiresAt
	f.claims[key] = existing
	return true, nil
}

func (f *fakeStore) ReleaseTrackerIntakeIssue(_ context.Context, claim ports.TrackerIntakeClaim, _ time.Time) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.releaseCalls = append(f.releaseCalls, claim)
	if f.releaseErr != nil {
		return false, f.releaseErr
	}
	key := fakeClaimKey(claim)
	existing, ok := f.claims[key]
	if !ok || existing.OwnerToken != claim.OwnerToken {
		return false, nil
	}
	delete(f.claims, key)
	return true, nil
}

func (f *fakeStore) capacityUsed(projectID domain.ProjectID, now time.Time) int {
	used := 0
	for _, session := range f.sessions {
		if session.ProjectID == projectID && session.Kind == domain.KindWorker && !session.IsTerminated {
			used++
		}
	}
	for _, claim := range f.claims {
		if claim.ProjectID == projectID && claim.LeaseExpiresAt.After(now) {
			used++
		}
	}
	return used
}

func fakeClaimKey(claim ports.TrackerIntakeClaim) string {
	return string(claim.ProjectID) + "\x00" + string(claim.Provider) + "\x00" + claim.Repo + "\x00" + claim.IssueID
}

type fakeTracker struct {
	issues       []domain.Issue
	issuesByRepo map[string][]domain.Issue
	failRepos    map[string]error
	repos        []domain.TrackerRepo
	filters      []domain.ListFilter
	listHook     func()
}

func (f *fakeTracker) Get(context.Context, domain.TrackerID) (domain.Issue, error) {
	return domain.Issue{}, nil
}

func (f *fakeTracker) List(_ context.Context, repo domain.TrackerRepo, filter domain.ListFilter) ([]domain.Issue, error) {
	if f.listHook != nil {
		f.listHook()
	}
	f.repos = append(f.repos, repo)
	f.filters = append(f.filters, filter)
	if err := f.failRepos[repo.Native]; err != nil {
		return nil, err
	}
	if f.issuesByRepo != nil {
		return append([]domain.Issue(nil), f.issuesByRepo[repo.Native]...), nil
	}
	return append([]domain.Issue(nil), f.issues...), nil
}

func (f *fakeTracker) Preflight(context.Context) error { return nil }

type fakeSpawner struct {
	calls     []ports.SpawnConfig
	failIssue domain.IssueID
	hook      func(ports.SpawnConfig)
}

func (f *fakeSpawner) Spawn(_ context.Context, cfg ports.SpawnConfig) (domain.Session, error) {
	f.calls = append(f.calls, cfg)
	if f.hook != nil {
		f.hook(cfg)
	}
	if cfg.IssueID == f.failIssue {
		return domain.Session{}, errors.New("spawn failed")
	}
	return domain.Session{SessionRecord: domain.SessionRecord{ID: domain.SessionID(string(cfg.ProjectID) + "-1"), ProjectID: cfg.ProjectID, IssueID: cfg.IssueID, Kind: cfg.Kind}}, nil
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
