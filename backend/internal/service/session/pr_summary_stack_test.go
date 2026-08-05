package session

import (
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// #142 (root → main), #143 stacked on #142, #144 stacked on #143. Only open
// parents annotate their children; a merged parent no longer marks the child
// as stacked.
func TestAnnotateStacksMarksChildrenOfOpenParents(t *testing.T) {
	prs := []domain.PullRequest{
		{URL: "p142", HTMLURL: "h142", Number: 142, SourceBranch: "s/root", TargetBranch: "main"},
		{URL: "p143", HTMLURL: "h143", Number: 143, SourceBranch: "s/root/a", TargetBranch: "s/root"},
		{URL: "p144", HTMLURL: "h144", Number: 144, SourceBranch: "s/root/a/b", TargetBranch: "s/root/a"},
	}
	out := []PRSummary{
		{URL: "p142", State: domain.PRStateOpen, TargetBranch: "main"},
		{URL: "p143", State: domain.PRStateOpen, TargetBranch: "s/root"},
		{URL: "p144", State: domain.PRStateOpen, TargetBranch: "s/root/a"},
	}
	annotateStacks(prs, out)
	if out[0].StackedOnURL != "" || out[0].StackedOnNumber != 0 {
		t.Fatalf("root PR should not be stacked: %+v", out[0])
	}
	if out[1].StackedOnURL != "h142" || out[1].StackedOnNumber != 142 {
		t.Fatalf("child should be stacked on #142: %+v", out[1])
	}
	if out[2].StackedOnURL != "h143" || out[2].StackedOnNumber != 143 {
		t.Fatalf("grandchild should be stacked on #143: %+v", out[2])
	}

	prs[0].Merged = true
	out[1].StackedOnURL, out[1].StackedOnNumber = "", 0
	annotateStacks(prs, out)
	if out[1].StackedOnURL != "" {
		t.Fatalf("merged parent should stop annotating the child: %+v", out[1])
	}
}

// A parent owned by another session still annotates the child when it appears
// in the candidate set, and a same-named branch in a different repository does
// not.
func TestAnnotateStacksMatchesAcrossSessionsWithinOneRepo(t *testing.T) {
	candidates := []domain.PullRequest{
		{URL: "parent", HTMLURL: "hparent", Number: 7, Repo: "acme/app", SourceBranch: "s/root", TargetBranch: "main"},
		{URL: "decoy", HTMLURL: "hdecoy", Number: 9, Repo: "acme/other", SourceBranch: "s/other", TargetBranch: "main"},
		{URL: "child", Number: 8, Repo: "acme/app", SourceBranch: "s/root/a", TargetBranch: "s/root"},
	}
	out := []PRSummary{
		{URL: "child", State: domain.PRStateOpen, Repo: "acme/app", TargetBranch: "s/root"},
		{URL: "lost", State: domain.PRStateOpen, Repo: "acme/app", TargetBranch: "s/other"},
	}
	annotateStacks(candidates, out)
	if out[0].StackedOnURL != "hparent" || out[0].StackedOnNumber != 7 {
		t.Fatalf("cross-session parent should annotate the child: %+v", out[0])
	}
	if out[1].StackedOnURL != "" {
		t.Fatalf("branch match in another repo must not annotate: %+v", out[1])
	}
}
