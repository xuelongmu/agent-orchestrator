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

type fakeSCMMerger struct {
	request ports.SCMMergeRequest
	result  ports.SCMMergeResult
	err     error
	calls   int
}

func (f *fakeSCMMerger) MergePullRequest(_ context.Context, request ports.SCMMergeRequest) (ports.SCMMergeResult, error) {
	f.calls++
	f.request = request
	return f.result, f.err
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

func TestMerge_SquashMergesTrackedPRAtExactHead(t *testing.T) {
	store := &fakeActionStore{pr: mergeablePR(), ok: true}
	merger := &fakeSCMMerger{result: ports.SCMMergeResult{MergeCommitSHA: "merge-sha"}}
	svc := NewActionService(ActionDeps{Store: store, Merger: merger})

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
	if merger.calls != 1 || merger.request.ExpectedHeadSHA != store.pr.HeadSHA || merger.request.Method != ports.SCMMergeSquash {
		t.Fatalf("merge request = %#v, calls=%d", merger.request, merger.calls)
	}
	if merger.request.PR.Number != 42 || merger.request.PR.Repo.Repo != "acme/widgets" {
		t.Fatalf("PR ref = %#v", merger.request.PR)
	}
}

func TestMerge_UsesPersistedHeadForClientsWithoutHeadField(t *testing.T) {
	store := &fakeActionStore{pr: mergeablePR(), ok: true}
	merger := &fakeSCMMerger{}
	svc := NewActionService(ActionDeps{Store: store, Merger: merger})

	_, err := svc.Merge(context.Background(), MergeRequest{PRID: "42", PRURL: store.pr.URL})
	if err != nil {
		t.Fatal(err)
	}
	if merger.request.ExpectedHeadSHA != store.pr.HeadSHA {
		t.Fatalf("expected head = %q, want %q", merger.request.ExpectedHeadSHA, store.pr.HeadSHA)
	}
}

func TestMerge_RejectsInvalidPRIDs(t *testing.T) {
	for _, id := range []string{"", "0", "01", "-1", "+1", " 1", "1 ", "1.0", "abc"} {
		t.Run(id, func(t *testing.T) {
			svc := NewActionService(ActionDeps{Store: &fakeActionStore{}, Merger: &fakeSCMMerger{}})
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
			svc := NewActionService(ActionDeps{Store: tc.store, Merger: &fakeSCMMerger{}})
			id := "42"
			if tc.name == "number mismatch" {
				id = "43"
			}
			_, err := svc.Merge(context.Background(), MergeRequest{PRID: id, PRURL: "https://github.com/acme/widgets/pull/42"})
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
			merger := &fakeSCMMerger{}
			svc := NewActionService(ActionDeps{Store: &fakeActionStore{pr: tc.pr, ok: true}, Merger: merger})
			_, err := svc.Merge(context.Background(), MergeRequest{PRID: "42", PRURL: pr.URL, ExpectedHeadSHA: tc.expected})
			if !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
			if merger.calls != 0 {
				t.Fatalf("merger called %d times", merger.calls)
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
		svc := NewActionService(ActionDeps{Store: &fakeActionStore{pr: pr, ok: true}, Merger: &fakeSCMMerger{}})
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
		svc := NewActionService(ActionDeps{Store: &fakeActionStore{pr: pr, ok: true}, Merger: &fakeSCMMerger{err: tc.provider}})
		_, err := svc.Merge(context.Background(), MergeRequest{PRID: "42", PRURL: pr.URL, ExpectedHeadSHA: pr.HeadSHA})
		if !errors.Is(err, tc.want) {
			t.Fatalf("provider %v mapped to %v, want %v", tc.provider, err, tc.want)
		}
	}
}

func TestResolveComments_ReturnsOK(t *testing.T) {
	svc := NewActionService(ActionDeps{})
	_, err := svc.ResolveComments(context.Background(), "1", nil)
	if err != nil {
		t.Fatal(err)
	}
}
