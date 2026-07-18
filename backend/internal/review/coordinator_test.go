package review

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

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
	got, err = eng.Coordinate(context.Background(), "mer-1", reviewObservation("sha2"))
	if err != nil {
		t.Fatalf("Coordinate new head: %v", err)
	}
	if got.Outcome != CoordinateStarted || got.Round != 2 || len(store.runs) != 2 {
		t.Fatalf("new head = %+v runs=%+v", got, store.runs)
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
