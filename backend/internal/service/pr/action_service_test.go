package pr

import (
	"context"
	"errors"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

type fakeActionStore struct {
	pr  domain.PullRequest
	ok  bool
	err error
}

func (f *fakeActionStore) GetPR(_ context.Context, _ string) (domain.PullRequest, bool, error) {
	return f.pr, f.ok, f.err
}

type fakeSCMAction struct {
	request      ports.SCMMergeRequest
	result       ports.SCMMergeResult
	mergeErr     error
	observations []ports.SCMObservation
	fetchErr     error
	review       ports.SCMReviewObservation
	reviewErr    error
	mergeCalls   int
}

func (f *fakeSCMAction) MergePullRequest(_ context.Context, request ports.SCMMergeRequest) (ports.SCMMergeResult, error) {
	f.mergeCalls++
	f.request = request
	return f.result, f.mergeErr
}

func (f *fakeSCMAction) FetchPullRequests(_ context.Context, _ []ports.SCMPRRef) ([]ports.SCMObservation, error) {
	return f.observations, f.fetchErr
}

func (f *fakeSCMAction) FetchReviewThreads(_ context.Context, _ ports.SCMPRRef) (ports.SCMReviewObservation, error) {
	return f.review, f.reviewErr
}

func mergeablePR() domain.PullRequest {
	return domain.PullRequest{
		URL:          "https://github.com/acme/widgets/pull/42",
		Number:       42,
		Provider:     "github",
		Host:         "github.com",
		Repo:         "acme/widgets",
		HeadSHA:      "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Mergeability: domain.MergeMergeable,
	}
}

func readySCM(pr domain.PullRequest) *fakeSCMAction {
	return &fakeSCMAction{
		observations: []ports.SCMObservation{{
			Fetched:      true,
			PR:           ports.SCMPRObservation{URL: pr.URL, Number: pr.Number, HeadSHA: pr.HeadSHA},
			CI:           ports.SCMCIObservation{Summary: string(domain.CIPassing), HeadSHA: pr.HeadSHA},
			Mergeability: ports.SCMMergeabilityObservation{State: string(domain.MergeMergeable), Mergeable: true},
		}},
		review: ports.SCMReviewObservation{Decision: string(domain.ReviewNone), HeadSHA: pr.HeadSHA},
	}
}

func TestMerge_SquashMergesTrackedPRAtExactHead(t *testing.T) {
	store := &fakeActionStore{pr: mergeablePR(), ok: true}
	scm := readySCM(store.pr)
	scm.result = ports.SCMMergeResult{MergeCommitSHA: "merge-sha"}
	svc := NewActionService(ActionDeps{Store: store, Merger: scm, Reader: scm})

	res, err := svc.Merge(context.Background(), MergeRequest{
		PRID:            "42",
		PRURL:           store.pr.URL,
		ExpectedHeadSHA: store.pr.HeadSHA,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Method != "squash" || res.PRNumber != 42 || res.MergeCommitSHA != "merge-sha" {
		t.Fatalf("result = %#v", res)
	}
	if scm.mergeCalls != 1 || scm.request.ExpectedHeadSHA != store.pr.HeadSHA || scm.request.Method != ports.SCMMergeSquash {
		t.Fatalf("merge request = %#v, calls=%d", scm.request, scm.mergeCalls)
	}
	if scm.request.PR.Number != 42 || scm.request.PR.Repo.Repo != "acme/widgets" {
		t.Fatalf("PR ref = %#v", scm.request.PR)
	}
}

func TestMerge_RequiresCallerExpectedHead(t *testing.T) {
	store := &fakeActionStore{pr: mergeablePR(), ok: true}
	scm := readySCM(store.pr)
	svc := NewActionService(ActionDeps{Store: store, Merger: scm, Reader: scm})

	_, err := svc.Merge(context.Background(), MergeRequest{PRID: "42", PRURL: store.pr.URL})
	if !errors.Is(err, ErrPRPreconditions) {
		t.Fatalf("error = %v, want ErrPRPreconditions", err)
	}
	if scm.mergeCalls != 0 {
		t.Fatalf("merge called %d times", scm.mergeCalls)
	}
}

func TestMerge_RejectsInvalidPRIDs(t *testing.T) {
	for _, id := range []string{"", "0", "01", "-1", "+1", " 1", "1 ", "1.0", "abc"} {
		t.Run(id, func(t *testing.T) {
			scm := &fakeSCMAction{}
			svc := NewActionService(ActionDeps{Store: &fakeActionStore{}, Merger: scm, Reader: scm})
			_, err := svc.Merge(context.Background(), MergeRequest{PRID: id, PRURL: "https://github.com/acme/widgets/pull/1"})
			if !errors.Is(err, ErrInvalidPR) {
				t.Fatalf("Merge(%q) error = %v, want ErrInvalidPR", id, err)
			}
		})
	}
}

func TestMerge_RejectsMissingOrMismatchedPR(t *testing.T) {
	tests := []struct {
		name  string
		store *fakeActionStore
	}{
		{name: "not found", store: &fakeActionStore{}},
		{name: "number mismatch", store: &fakeActionStore{pr: mergeablePR(), ok: true}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			scm := &fakeSCMAction{}
			svc := NewActionService(ActionDeps{Store: tc.store, Merger: scm, Reader: scm})
			id := "42"
			if tc.name == "number mismatch" {
				id = "43"
			}
			_, err := svc.Merge(context.Background(), MergeRequest{
				PRID:            id,
				PRURL:           "https://github.com/acme/widgets/pull/42",
				ExpectedHeadSHA: mergeablePR().HeadSHA,
			})
			if !errors.Is(err, ErrPRNotFound) {
				t.Fatalf("error = %v, want ErrPRNotFound", err)
			}
		})
	}
}

func TestMerge_RejectsStaleOrMissingHead(t *testing.T) {
	pr := mergeablePR()
	tests := []struct {
		name     string
		pr       domain.PullRequest
		expected string
		want     error
	}{
		{name: "stale request", pr: pr, expected: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", want: ErrPRHeadChanged},
		{name: "missing observed head", pr: func() domain.PullRequest { p := pr; p.HeadSHA = ""; return p }(), want: ErrPRPreconditions},
		{name: "invalid expected head", pr: pr, expected: "not-a-sha", want: ErrInvalidPR},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			scm := readySCM(tc.pr)
			svc := NewActionService(ActionDeps{Store: &fakeActionStore{pr: tc.pr, ok: true}, Merger: scm, Reader: scm})
			_, err := svc.Merge(context.Background(), MergeRequest{PRID: "42", PRURL: pr.URL, ExpectedHeadSHA: tc.expected})
			if !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
			if scm.mergeCalls != 0 {
				t.Fatalf("merger called %d times", scm.mergeCalls)
			}
		})
	}
}

func TestMerge_RejectsTerminalOrDraftPR(t *testing.T) {
	for _, mutate := range []func(*domain.PullRequest){
		func(p *domain.PullRequest) { p.Draft = true },
		func(p *domain.PullRequest) { p.Merged = true },
		func(p *domain.PullRequest) { p.Closed = true },
	} {
		pr := mergeablePR()
		mutate(&pr)
		scm := readySCM(pr)
		svc := NewActionService(ActionDeps{Store: &fakeActionStore{pr: pr, ok: true}, Merger: scm, Reader: scm})
		_, err := svc.Merge(context.Background(), MergeRequest{PRID: "42", PRURL: pr.URL, ExpectedHeadSHA: pr.HeadSHA})
		if !errors.Is(err, ErrPRNotMergeable) {
			t.Fatalf("PR %#v error = %v, want ErrPRNotMergeable", pr, err)
		}
	}
}

func TestMerge_MapsProviderErrors(t *testing.T) {
	tests := []struct {
		provider error
		want     error
	}{
		{provider: ports.ErrSCMNotFound, want: ErrPRNotFound},
		{provider: ports.ErrSCMHeadChanged, want: ErrPRHeadChanged},
		{provider: ports.ErrSCMNotMergeable, want: ErrPRNotMergeable},
	}
	for _, tc := range tests {
		pr := mergeablePR()
		scm := readySCM(pr)
		scm.mergeErr = tc.provider
		svc := NewActionService(ActionDeps{Store: &fakeActionStore{pr: pr, ok: true}, Merger: scm, Reader: scm})
		_, err := svc.Merge(context.Background(), MergeRequest{PRID: "42", PRURL: pr.URL, ExpectedHeadSHA: pr.HeadSHA})
		if !errors.Is(err, tc.want) {
			t.Fatalf("provider %v mapped to %v, want %v", tc.provider, err, tc.want)
		}
	}
}

func TestMerge_FailsClosedWhenFreshDefinitionOfDoneIsUnmet(t *testing.T) {
	pr := mergeablePR()
	tests := []struct {
		name   string
		mutate func(*ports.SCMObservation, *ports.SCMReviewObservation)
		want   error
	}{
		{name: "head advanced", want: ErrPRHeadChanged, mutate: func(o *ports.SCMObservation, r *ports.SCMReviewObservation) {
			o.PR.HeadSHA = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
			r.HeadSHA = o.PR.HeadSHA
		}},
		{name: "CI failing", want: ErrPRPreconditions, mutate: func(o *ports.SCMObservation, _ *ports.SCMReviewObservation) { o.CI.Summary = string(domain.CIFailing) }},
		{name: "CI pending", want: ErrPRPreconditions, mutate: func(o *ports.SCMObservation, _ *ports.SCMReviewObservation) { o.CI.Summary = string(domain.CIPending) }},
		{name: "stale CI snapshot", want: ErrPRPreconditions, mutate: func(o *ports.SCMObservation, _ *ports.SCMReviewObservation) {
			o.CI.HeadSHA = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
		}},
		{name: "incomplete PR observation", want: ErrPRNotFound, mutate: func(o *ports.SCMObservation, _ *ports.SCMReviewObservation) { o.Fetched = false }},
		{name: "merge conflict", want: ErrPRPreconditions, mutate: func(o *ports.SCMObservation, _ *ports.SCMReviewObservation) {
			o.Mergeability.State = string(domain.MergeConflicting)
		}},
		{name: "review required", want: ErrPRPreconditions, mutate: func(_ *ports.SCMObservation, r *ports.SCMReviewObservation) {
			r.Decision = string(domain.ReviewRequired)
		}},
		{name: "changes requested", want: ErrPRPreconditions, mutate: func(_ *ports.SCMObservation, r *ports.SCMReviewObservation) {
			r.Decision = string(domain.ReviewChangesRequest)
		}},
		{name: "partial review window", want: ErrPRPreconditions, mutate: func(_ *ports.SCMObservation, r *ports.SCMReviewObservation) { r.Partial = true }},
		{name: "stale review snapshot", want: ErrPRPreconditions, mutate: func(_ *ports.SCMObservation, r *ports.SCMReviewObservation) {
			r.HeadSHA = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
		}},
		{name: "unresolved human", want: ErrPRPreconditions, mutate: func(_ *ports.SCMObservation, r *ports.SCMReviewObservation) {
			r.Threads = []ports.SCMReviewThreadObservation{{ID: "human", Comments: []ports.SCMReviewCommentObservation{{Author: "alice", Body: "fix this"}}}}
		}},
		{name: "unresolved Codex P1", want: ErrPRPreconditions, mutate: func(_ *ports.SCMObservation, r *ports.SCMReviewObservation) {
			r.Threads = []ports.SCMReviewThreadObservation{{ID: "codex", IsBot: true, Comments: []ports.SCMReviewCommentObservation{{Author: "chatgpt-codex-connector[bot]", IsBot: true, Body: "[P1] unsafe merge"}}}}
		}},
		{name: "stale approval", want: ErrPRPreconditions, mutate: func(_ *ports.SCMObservation, r *ports.SCMReviewObservation) {
			r.Decision = string(domain.ReviewApproved)
			r.Reviews = []ports.SCMReviewSummaryObservation{{Author: "alice", State: string(domain.ReviewApproved), CommitSHA: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}}
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			scm := readySCM(pr)
			tc.mutate(&scm.observations[0], &scm.review)
			svc := NewActionService(ActionDeps{Store: &fakeActionStore{pr: pr, ok: true}, Merger: scm, Reader: scm})
			_, err := svc.Merge(context.Background(), MergeRequest{PRID: "42", PRURL: pr.URL, ExpectedHeadSHA: pr.HeadSHA})
			if !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
			if scm.mergeCalls != 0 {
				t.Fatalf("merge called %d times", scm.mergeCalls)
			}
		})
	}
}

func TestResolveComments_ReturnsOK(t *testing.T) {
	svc := NewActionService(ActionDeps{})
	_, err := svc.ResolveComments(context.Background(), "1", nil)
	if err != nil {
		t.Fatal(err)
	}
}
