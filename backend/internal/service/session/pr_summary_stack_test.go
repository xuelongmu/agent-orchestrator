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
