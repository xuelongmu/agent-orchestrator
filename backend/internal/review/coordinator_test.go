package review

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

type fakeRoundCapHandoff struct {
	calls int
	err   error
	id    domain.SessionID
	obs   ports.SCMObservation
	round int
}

func (f *fakeRoundCapHandoff) ApplyReviewRoundCapHandoff(_ context.Context, id domain.SessionID, obs ports.SCMObservation, round int) error {
	f.calls++
	f.id = id
	f.obs = obs
	f.round = round
	return f.err
}

func reviewObservation(head string) ports.SCMObservation {
	return ports.SCMObservation{
		Fetched: true,
		PR: ports.SCMPRObservation{
			URL:     "https://github.com/o/r/pull/1",
			Number:  1,
			HeadSHA: head,
		},
		CI:     ports.SCMCIObservation{Summary: string(domain.CIPassing), HeadSHA: head},
		Review: ports.SCMReviewObservation{HeadSHA: head},
	}
}

func reviewRun(id, head string, status domain.ReviewRunStatus, verdict domain.ReviewVerdict, body string) domain.ReviewRun {
	return domain.ReviewRun{
		ID: id, SessionID: "mer-1", PRURL: "https://github.com/o/r/pull/1",
		TargetSHA: head, Status: status, Verdict: verdict, Body: body,
		CreatedAt: time.Unix(int64(len(id)), 0).UTC(),
	}
}

func TestCoordinateStartsFirstPassingHeadOnce(t *testing.T) {
	store := &fakeStore{}
	launcher := &fakeLauncher{handle: "review-mer-1"}
	eng := newEngineForTest(store, fakeSessions{rec: liveWorker(), ok: true}, prAt("sha1"), fakeProjects{}, launcher)

	got, err := eng.Coordinate(context.Background(), "mer-1", reviewObservation("sha1"))
	if err != nil {
		t.Fatalf("Coordinate: %v", err)
	}
	if got.Outcome != CoordinateStarted || got.Round != 1 || !launcher.spawned || len(store.runs) != 1 {
		t.Fatalf("first coordinate = %+v launcher=%+v runs=%+v", got, launcher, store.runs)
	}

	// The durable current-head run is the idempotency key after a restart too;
	// a new Engine over the same store must not spend the round again.
	restarted := newEngineForTest(store, fakeSessions{rec: liveWorker(), ok: true}, prAt("sha1"), fakeProjects{}, launcher)
	got, err = restarted.Coordinate(context.Background(), "mer-1", reviewObservation("sha1"))
	if err != nil {
		t.Fatalf("Coordinate after restart: %v", err)
	}
	if got.Outcome != CoordinateWaiting || len(store.runs) != 1 || launcher.spawnCount != 1 {
		t.Fatalf("repeat coordinate = %+v spawnCount=%d runs=%+v", got, launcher.spawnCount, store.runs)
	}
}

func TestCoordinateWaitsForPassingCurrentHead(t *testing.T) {
	store := &fakeStore{}
	launcher := &fakeLauncher{handle: "review-mer-1"}
	eng := newEngineForTest(store, fakeSessions{rec: liveWorker(), ok: true}, prAt("sha1"), fakeProjects{}, launcher)
	for _, ci := range []domain.CIState{domain.CIUnknown, domain.CIPending, domain.CIFailing} {
		obs := reviewObservation("sha1")
		obs.CI.Summary = string(ci)
		got, err := eng.Coordinate(context.Background(), "mer-1", obs)
		if err != nil {
			t.Fatalf("Coordinate(%s): %v", ci, err)
		}
		if got.Outcome != CoordinateIneligible {
			t.Fatalf("Coordinate(%s) = %+v, want ineligible", ci, got)
		}
	}
	if len(store.runs) != 0 || launcher.spawnCount != 0 {
		t.Fatalf("non-passing heads launched review: runs=%+v launcher=%+v", store.runs, launcher)
	}
}

func TestCoordinateWaitsForFixThenReviewsNewHead(t *testing.T) {
	store := &fakeStore{runs: []domain.ReviewRun{
		reviewRun("run-1", "sha1", domain.ReviewRunDelivered, domain.VerdictChangesRequested, "[P1] lost update"),
	}}
	launcher := &fakeLauncher{handle: "review-mer-1"}
	eng := newEngineForTest(store, fakeSessions{rec: liveWorker(), ok: true}, prAt("sha1"), fakeProjects{}, launcher)

	got, err := eng.Coordinate(context.Background(), "mer-1", reviewObservation("sha1"))
	if err != nil {
		t.Fatalf("Coordinate same head: %v", err)
	}
	if got.Outcome != CoordinateWaiting || len(store.runs) != 1 {
		t.Fatalf("same head = %+v runs=%+v", got, store.runs)
	}

	eng.prs = prAt("sha2")
	fix := reviewObservation("sha2")
	fix.PR.HeadCommitMessage = "fix body with structured trailer"
	got, err = eng.Coordinate(context.Background(), "mer-1", fix)
	if err != nil {
		t.Fatalf("Coordinate new head: %v", err)
	}
	if got.Outcome != CoordinateStarted || got.Round != 2 || len(store.runs) != 2 {
		t.Fatalf("new head = %+v runs=%+v", got, store.runs)
	}
	if store.acceptCalls != 2 || store.acceptedPR != fix.PR.URL || store.acceptedHead != "sha2" || store.acceptedMessage != fix.PR.HeadCommitMessage {
		t.Fatalf("review-fix acceptance call = %d %q %q %q", store.acceptCalls, store.acceptedPR, store.acceptedHead, store.acceptedMessage)
	}
}

func TestCoordinateManualCurrentRunCannotBypassReviewFixAcceptance(t *testing.T) {
	store := &fakeStore{
		runs: []domain.ReviewRun{
			reviewRun("old", "sha1", domain.ReviewRunDelivered, domain.VerdictChangesRequested, "[P1] lost update"),
			reviewRun("manual", "sha2", domain.ReviewRunRunning, domain.VerdictNone, ""),
		},
		acceptRequired: true,
		acceptErr:      errors.New("review-fix invariant declaration is missing"),
	}
	launcher := &fakeLauncher{handle: "review-mer-1"}
	eng := newEngineForTest(store, fakeSessions{rec: liveWorker(), ok: true}, prAt("sha2"), fakeProjects{}, launcher)
	if _, err := eng.Coordinate(context.Background(), "mer-1", reviewObservation("sha2")); !errors.Is(err, store.acceptErr) {
		t.Fatalf("missing trailer error = %v", err)
	}
	if launcher.spawnCount != 0 || len(store.runs) != 2 {
		t.Fatalf("failed acceptance changed manual run: spawns=%d runs=%+v", launcher.spawnCount, store.runs)
	}
	store.acceptErr = nil
	store.acceptBound = 1
	obs := reviewObservation("sha2")
	obs.PR.HeadCommitMessage = "valid structured trailer"
	got, err := eng.Coordinate(context.Background(), "mer-1", obs)
	if err != nil || got.Outcome != CoordinateWaiting || got.Round != 2 {
		t.Fatalf("valid acceptance = %+v, %v", got, err)
	}
}

func TestCoordinateReplacementUsesPRGlobalHistoryForCurrentHeadRoundAndCap(t *testing.T) {
	prURL := "https://github.com/o/r/pull/1"
	predecessor := reviewRun("predecessor", "sha1", domain.ReviewRunDelivered, domain.VerdictChangesRequested, "[P1] fix")
	predecessor.SessionID = "old-worker"
	store := &fakeStore{runs: []domain.ReviewRun{predecessor}}
	launcher := &fakeLauncher{handle: "review-mer-1"}
	eng := newEngineForTest(store, fakeSessions{rec: liveWorker(), ok: true}, prAt("sha1"), fakeProjects{}, launcher)
	got, err := eng.Coordinate(context.Background(), "mer-1", reviewObservation("sha1"))
	if err != nil || got.Outcome != CoordinateWaiting || got.Round != 1 || launcher.spawnCount != 0 {
		t.Fatalf("replacement same head = %+v spawns=%d err=%v", got, launcher.spawnCount, err)
	}

	for i := 2; i <= MaxAutomaticReviewRounds; i++ {
		run := reviewRun(fmt.Sprintf("old-%d", i), fmt.Sprintf("sha%d", i), domain.ReviewRunDelivered, domain.VerdictChangesRequested, "[P1] fix")
		run.SessionID = "old-worker"
		store.runs = append(store.runs, run)
	}
	eng.prs = prAt("sha7")
	obs := reviewObservation("sha7")
	obs.PR.URL = prURL
	got, err = eng.Coordinate(context.Background(), "mer-1", obs)
	if err != nil || got.Outcome != CoordinateExhausted || got.Round != MaxAutomaticReviewRounds || launcher.spawnCount != 0 {
		t.Fatalf("replacement round cap = %+v spawns=%d err=%v", got, launcher.spawnCount, err)
	}
}

func TestCoordinateReplacementSatisfiesPredecessorCurrentRunWhenAllFindingsDeflected(t *testing.T) {
	run := reviewRun("predecessor", "sha1", domain.ReviewRunDelivered, domain.VerdictChangesRequested, "[P1] deferred")
	run.SessionID = "old-worker"
	store := &fakeStore{
		runs: []domain.ReviewRun{run},
		findings: []domain.ReviewFinding{{
			ID: "predecessor:1", RunID: run.ID, SessionID: "old-worker", PRURL: run.PRURL,
			OutOfScope: true, DeferredIssueURL: "https://github.com/o/r/issues/9", ThreadID: "thread-1", ThreadResolved: true,
		}},
	}
	launcher := &fakeLauncher{handle: "review-mer-1"}
	eng := newEngineForTest(store, fakeSessions{rec: liveWorker(), ok: true}, prAt("sha1"), fakeProjects{}, launcher)
	got, err := eng.Coordinate(context.Background(), "mer-1", reviewObservation("sha1"))
	if err != nil || got.Outcome != CoordinateSatisfied || got.Round != 1 || launcher.spawnCount != 0 {
		t.Fatalf("replacement deflected run = %+v spawns=%d err=%v", got, launcher.spawnCount, err)
	}
}

func TestCoordinateFailsBeforeReviewLaunchWhenReviewFixAcceptanceFails(t *testing.T) {
	store := &fakeStore{
		runs:           []domain.ReviewRun{reviewRun("run-1", "sha1", domain.ReviewRunDelivered, domain.VerdictChangesRequested, "[P1] lost update")},
		acceptRequired: true,
		acceptErr:      errors.New("review-fix invariant declaration is missing"),
	}
	launcher := &fakeLauncher{handle: "review-mer-1"}
	eng := newEngineForTest(store, fakeSessions{rec: liveWorker(), ok: true}, prAt("sha2"), fakeProjects{}, launcher)
	if _, err := eng.Coordinate(context.Background(), "mer-1", reviewObservation("sha2")); !errors.Is(err, store.acceptErr) {
		t.Fatalf("Coordinate error = %v, want %v", err, store.acceptErr)
	}
	if store.acceptCalls != 1 || launcher.spawnCount != 0 || len(store.runs) != 1 {
		t.Fatalf("failed boundary mutated round: accepts=%d spawns=%d runs=%+v", store.acceptCalls, launcher.spawnCount, store.runs)
	}
}

func TestCoordinateRequiresTrailerForHistoricalBlockingRunWithoutFindingRows(t *testing.T) {
	store := &fakeStore{
		runs:      []domain.ReviewRun{reviewRun("legacy", "sha1", domain.ReviewRunDelivered, domain.VerdictChangesRequested, "historical untagged blocker")},
		acceptErr: errors.New("review-fix invariant declaration is missing"),
	}
	launcher := &fakeLauncher{handle: "review-mer-1"}
	eng := newEngineForTest(store, fakeSessions{rec: liveWorker(), ok: true}, prAt("sha2"), fakeProjects{}, launcher)
	if _, err := eng.Coordinate(context.Background(), "mer-1", reviewObservation("sha2")); !errors.Is(err, store.acceptErr) {
		t.Fatalf("legacy missing trailer error = %v", err)
	}
	if !store.acceptedRequire || launcher.spawnCount != 0 || len(store.runs) != 1 {
		t.Fatalf("legacy gate = require %v spawns %d runs %+v", store.acceptedRequire, launcher.spawnCount, store.runs)
	}
	store.acceptErr = nil
	obs := reviewObservation("sha2")
	obs.PR.HeadCommitMessage = "valid trailer"
	got, err := eng.Coordinate(context.Background(), "mer-1", obs)
	if err != nil || got.Outcome != CoordinateStarted || got.Round != 2 || launcher.spawnCount != 1 {
		t.Fatalf("legacy valid trailer = %+v spawns=%d err=%v", got, launcher.spawnCount, err)
	}
}

func TestCoordinateStopsOnHeadBoundCleanVerdict(t *testing.T) {
	tests := []struct {
		name    string
		verdict domain.ReviewVerdict
		body    string
	}{
		{name: "approved", verdict: domain.VerdictApproved},
		{name: "only lower-priority finding", verdict: domain.VerdictChangesRequested, body: "[P2] clearer name"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &fakeStore{runs: []domain.ReviewRun{
				reviewRun("run-1", "sha1", domain.ReviewRunDelivered, tt.verdict, tt.body),
			}}
			launcher := &fakeLauncher{handle: "review-mer-1"}
			eng := newEngineForTest(store, fakeSessions{rec: liveWorker(), ok: true}, prAt("sha1"), fakeProjects{}, launcher)

			got, err := eng.Coordinate(context.Background(), "mer-1", reviewObservation("sha1"))
			if err != nil {
				t.Fatalf("Coordinate: %v", err)
			}
			if got.Outcome != CoordinateSatisfied || launcher.spawnCount != 0 || len(store.runs) != 1 {
				t.Fatalf("coordinate = %+v launcher=%+v runs=%+v", got, launcher, store.runs)
			}
		})
	}
}

func TestCoordinateStopsOnCurrentHeadHumanApproval(t *testing.T) {
	store := &fakeStore{}
	launcher := &fakeLauncher{handle: "review-mer-1"}
	eng := newEngineForTest(store, fakeSessions{rec: liveWorker(), ok: true}, prAt("sha1"), fakeProjects{}, launcher)
	obs := reviewObservation("sha1")
	obs.Review.Decision = string(domain.ReviewApproved)
	obs.Review.Reviews = []ports.SCMReviewSummaryObservation{{
		Author: "alice", State: string(domain.ReviewApproved), CommitSHA: "sha1",
	}}

	got, err := eng.Coordinate(context.Background(), "mer-1", obs)
	if err != nil {
		t.Fatalf("Coordinate: %v", err)
	}
	if got.Outcome != CoordinateSatisfied || launcher.spawnCount != 0 || len(store.runs) != 0 {
		t.Fatalf("coordinate = %+v launcher=%+v runs=%+v", got, launcher, store.runs)
	}
}

func TestCoordinateRetriesFailedOrCancelledLaunchAfterDurableBackoff(t *testing.T) {
	for _, status := range []domain.ReviewRunStatus{domain.ReviewRunFailed, domain.ReviewRunCancelled} {
		t.Run(string(status), func(t *testing.T) {
			failedAt := time.Unix(100, 0).UTC()
			store := &fakeStore{runs: []domain.ReviewRun{
				reviewRun("run-1", "sha1", status, domain.VerdictNone, "reviewer launch stopped"),
			}}
			store.runs[0].CreatedAt = failedAt
			launcher := &fakeLauncher{handle: "review-mer-1"}
			now := failedAt.Add(AutomaticReviewRetryBaseDelay - time.Second)
			newEngine := func() *Engine {
				ids := 0
				return New(Deps{
					Store: store, Sessions: fakeSessions{rec: liveWorker(), ok: true}, PRs: prAt("sha1"),
					Projects: fakeProjects{}, Launcher: launcher,
					Clock: func() time.Time { return now },
					NewID: func() string { ids++; return fmt.Sprintf("retry-%d", ids) },
				})
			}

			got, err := newEngine().Coordinate(context.Background(), "mer-1", reviewObservation("sha1"))
			if err != nil {
				t.Fatalf("Coordinate before retry window: %v", err)
			}
			if got.Outcome != CoordinateWaiting || got.Round != 1 || launcher.spawnCount != 0 || len(store.runs) != 1 {
				t.Fatalf("before retry window = %+v launcher=%+v runs=%+v", got, launcher, store.runs)
			}

			// The failed run's timestamp is the durable retry cursor. A fresh engine
			// over the same store retries once the window has elapsed.
			now = failedAt.Add(AutomaticReviewRetryBaseDelay)
			got, err = newEngine().Coordinate(context.Background(), "mer-1", reviewObservation("sha1"))
			if err != nil {
				t.Fatalf("Coordinate after retry window: %v", err)
			}
			if got.Outcome != CoordinateStarted || got.Round != 1 || launcher.spawnCount != 1 || len(store.runs) != 2 {
				t.Fatalf("after retry window = %+v launcher=%+v runs=%+v", got, launcher, store.runs)
			}
		})
	}
}

func TestAutomaticReviewRetryBackoffIsBounded(t *testing.T) {
	if got := automaticReviewRetryDelay(100); got != AutomaticReviewRetryMaxDelay {
		t.Fatalf("retry delay = %s, want bounded maximum %s", got, AutomaticReviewRetryMaxDelay)
	}
}

func TestCoordinateDoesNotTrustStaleApprovalOrReviewSnapshot(t *testing.T) {
	t.Run("stale approval does not stop current head review", func(t *testing.T) {
		store := &fakeStore{}
		launcher := &fakeLauncher{handle: "review-mer-1"}
		eng := newEngineForTest(store, fakeSessions{rec: liveWorker(), ok: true}, prAt("sha2"), fakeProjects{}, launcher)
		obs := reviewObservation("sha2")
		obs.Review.Decision = string(domain.ReviewApproved)
		obs.Review.Reviews = []ports.SCMReviewSummaryObservation{{
			Author: "alice", State: string(domain.ReviewApproved), CommitSHA: "sha1",
		}}

		got, err := eng.Coordinate(context.Background(), "mer-1", obs)
		if err != nil {
			t.Fatalf("Coordinate: %v", err)
		}
		if got.Outcome != CoordinateStarted || launcher.spawnCount != 1 {
			t.Fatalf("coordinate = %+v launcher=%+v", got, launcher)
		}
	})

	for _, mutate := range []func(*ports.SCMObservation){
		func(obs *ports.SCMObservation) { obs.Review.HeadSHA = "sha1" },
		func(obs *ports.SCMObservation) { obs.Review.Partial = true },
	} {
		store := &fakeStore{}
		launcher := &fakeLauncher{handle: "review-mer-1"}
		eng := newEngineForTest(store, fakeSessions{rec: liveWorker(), ok: true}, prAt("sha2"), fakeProjects{}, launcher)
		obs := reviewObservation("sha2")
		mutate(&obs)
		got, err := eng.Coordinate(context.Background(), "mer-1", obs)
		if err != nil {
			t.Fatalf("Coordinate: %v", err)
		}
		if got.Outcome != CoordinateIneligible || launcher.spawnCount != 0 {
			t.Fatalf("coordinate = %+v launcher=%+v", got, launcher)
		}
	}
}

func TestCoordinateDoesNotDeclareCleanWhileCodexP1ThreadRemains(t *testing.T) {
	store := &fakeStore{runs: []domain.ReviewRun{
		reviewRun("run-1", "sha1", domain.ReviewRunDelivered, domain.VerdictApproved, ""),
	}}
	eng := newEngineForTest(store, fakeSessions{rec: liveWorker(), ok: true}, prAt("sha1"), fakeProjects{}, &fakeLauncher{})
	obs := reviewObservation("sha1")
	obs.Review.Threads = []ports.SCMReviewThreadObservation{{
		Comments: []ports.SCMReviewCommentObservation{{
			Author: "chatgpt-codex-connector[bot]", IsBot: true, Body: "[P1] lost update",
		}},
	}}

	got, err := eng.Coordinate(context.Background(), "mer-1", obs)
	if err != nil {
		t.Fatalf("Coordinate: %v", err)
	}
	if got.Outcome != CoordinateWaiting {
		t.Fatalf("coordinate = %+v, want waiting", got)
	}
}

func TestCoordinateFailsClosedOnUntaggedChangesRequested(t *testing.T) {
	store := &fakeStore{runs: []domain.ReviewRun{
		reviewRun("run-1", "sha1", domain.ReviewRunComplete, domain.VerdictChangesRequested, "This can lose updates"),
	}}
	eng := newEngineForTest(store, fakeSessions{rec: liveWorker(), ok: true}, prAt("sha1"), fakeProjects{}, &fakeLauncher{})

	got, err := eng.Coordinate(context.Background(), "mer-1", reviewObservation("sha1"))
	if err != nil {
		t.Fatalf("Coordinate: %v", err)
	}
	if got.Outcome != CoordinateWaiting {
		t.Fatalf("coordinate = %+v, want waiting", got)
	}
}

func TestBodyHasBlockingFindingsRequiresEveryFindingLineTagged(t *testing.T) {
	tests := []struct {
		name string
		body string
		want bool
	}{
		{name: "p2 only", body: "[P2] clearer name", want: false},
		{name: "p3 under heading", body: "## Suggestions\n[P3] minor cleanup", want: false},
		{name: "indented continuation", body: "[P2] clearer name\n  This preserves the same behavior.", want: false},
		{name: "mixed untagged", body: "[P2] clearer name\nThis can lose updates", want: true},
		{name: "p1", body: "[P1] lost update", want: true},
		{name: "untagged", body: "This can lose updates", want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := BodyHasBlockingFindings(tt.body); got != tt.want {
				t.Fatalf("BodyHasBlockingFindings(%q) = %v, want %v", tt.body, got, tt.want)
			}
		})
	}
}

func TestCoordinateStopsAfterSixDistinctHeadRounds(t *testing.T) {
	runs := make([]domain.ReviewRun, 0, MaxAutomaticReviewRounds)
	for i := 1; i <= MaxAutomaticReviewRounds; i++ {
		runs = append(runs, reviewRun(
			fmt.Sprintf("run-%d", i), fmt.Sprintf("sha%d", i),
			domain.ReviewRunDelivered, domain.VerdictChangesRequested, "[P1] still broken",
		))
	}
	store := &fakeStore{runs: runs}
	launcher := &fakeLauncher{handle: "review-mer-1"}
	eng := newEngineForTest(store, fakeSessions{rec: liveWorker(), ok: true}, prAt("sha7"), fakeProjects{}, launcher)

	got, err := eng.Coordinate(context.Background(), "mer-1", reviewObservation("sha7"))
	if err != nil {
		t.Fatalf("Coordinate: %v", err)
	}
	if got.Outcome != CoordinateExhausted || got.Round != MaxAutomaticReviewRounds || launcher.spawnCount != 0 || len(store.runs) != MaxAutomaticReviewRounds {
		t.Fatalf("coordinate = %+v launcher=%+v runs=%+v", got, launcher, store.runs)
	}
}

func TestCoordinateRoundCapHandoffFailureRemainsRetryable(t *testing.T) {
	runs := make([]domain.ReviewRun, 0, MaxAutomaticReviewRounds)
	for i := 1; i <= MaxAutomaticReviewRounds; i++ {
		runs = append(runs, reviewRun(
			fmt.Sprintf("run-%d", i), fmt.Sprintf("sha%d", i),
			domain.ReviewRunDelivered, domain.VerdictChangesRequested, "[P1] still broken",
		))
	}
	store := &fakeStore{runs: runs}
	eng := newEngineForTest(store, fakeSessions{rec: liveWorker(), ok: true}, prAt("sha7"), fakeProjects{}, &fakeLauncher{})
	handoff := &fakeRoundCapHandoff{err: errors.New("notification unavailable")}
	eng.roundCapHandoff = handoff
	obs := reviewObservation("sha7")

	if _, err := eng.Coordinate(context.Background(), "mer-1", obs); !errors.Is(err, handoff.err) {
		t.Fatalf("Coordinate error = %v, want handoff failure", err)
	}
	if _, err := eng.Coordinate(context.Background(), "mer-1", obs); !errors.Is(err, handoff.err) {
		t.Fatalf("Coordinate retry error = %v, want handoff failure", err)
	}
	if handoff.calls != 2 || handoff.id != "mer-1" || handoff.obs.PR.HeadSHA != "sha7" || handoff.round != MaxAutomaticReviewRounds {
		t.Fatalf("handoff calls = %+v, want two exact-head attempts", handoff)
	}
}

func TestCoordinateDoesNotBatchAnotherPRPastItsRoundCap(t *testing.T) {
	const pr1 = "https://github.com/o/r/pull/1"
	const pr2 = "https://github.com/o/r/pull/2"
	runs := make([]domain.ReviewRun, 0, MaxAutomaticReviewRounds)
	for i := 1; i <= MaxAutomaticReviewRounds; i++ {
		r := reviewRun(fmt.Sprintf("other-%d", i), fmt.Sprintf("other-sha%d", i), domain.ReviewRunDelivered, domain.VerdictChangesRequested, "[P1] still broken")
		r.PRURL = pr2
		runs = append(runs, r)
	}
	store := &fakeStore{runs: runs}
	launcher := &fakeLauncher{handle: "review-mer-1"}
	prs := fakePRs{prs: []domain.PullRequest{
		{URL: pr1, Number: 1, HeadSHA: "sha1"},
		{URL: pr2, Number: 2, HeadSHA: "other-sha7"},
	}}
	eng := newEngineForTest(store, fakeSessions{rec: liveWorker(), ok: true}, prs, fakeProjects{}, launcher)

	got, err := eng.Coordinate(context.Background(), "mer-1", reviewObservation("sha1"))
	if err != nil {
		t.Fatalf("Coordinate: %v", err)
	}
	if got.Outcome != CoordinateStarted || len(store.runs) != MaxAutomaticReviewRounds+1 {
		t.Fatalf("coordinate = %+v runs=%+v", got, store.runs)
	}
	if created := store.runs[len(store.runs)-1]; created.PRURL != pr1 {
		t.Fatalf("automatic trigger created run for %q, want only %q", created.PRURL, pr1)
	}
}
