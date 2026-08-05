package session

import (
	"context"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// #142 (root → main), #143 stacked on #142, #144 stacked on #143. Only open
// parents annotate their children; a merged parent no longer marks the child
// as stacked.
func TestStackParentsMarksChildrenOfOpenParents(t *testing.T) {
	svc := &Service{store: &fakeStore{}}
	prs := []domain.PullRequest{
		{URL: "p142", HTMLURL: "h142", Number: 142, Repo: "acme/app", SourceBranch: "s/root", TargetBranch: "main"},
		{URL: "p143", HTMLURL: "h143", Number: 143, Repo: "acme/app", SourceBranch: "s/root/a", TargetBranch: "s/root"},
		{URL: "p144", HTMLURL: "h144", Number: 144, Repo: "acme/app", SourceBranch: "s/root/a/b", TargetBranch: "s/root/a"},
	}
	parents := svc.stackParents(context.Background(), "proj-1", prs)
	if _, ok := parents["p142"]; ok {
		t.Fatalf("root PR should not be stacked: %+v", parents)
	}
	if parents["p143"].Number != 142 || parents["p144"].Number != 143 {
		t.Fatalf("stack parents = %+v", parents)
	}

	out := []PRSummary{{URL: "p143", State: domain.PRStateOpen}}
	annotateStacks(parents, out)
	if out[0].StackedOnURL != "h142" || out[0].StackedOnNumber != 142 {
		t.Fatalf("child should be annotated with #142: %+v", out[0])
	}

	prs[0].Merged = true
	parents = svc.stackParents(context.Background(), "proj-1", prs)
	if _, ok := parents["p143"]; ok {
		t.Fatalf("merged parent should stop blocking the child: %+v", parents)
	}
}

// A parent owned by another session still matches when the repo-wide lookup
// returns it, and a same-named branch on another provider/host/repo does not.
func TestStackParentsMatchesAcrossSessionsWithinOneRepo(t *testing.T) {
	store := &fakeStore{openPRsByRepo: []domain.PullRequest{
		{URL: "parent", HTMLURL: "hparent", Number: 7, SessionID: "other", Provider: "github", Host: "github.com", Repo: "acme/app", SourceBranch: "s/root", TargetBranch: "main"},
		{URL: "decoy", HTMLURL: "hdecoy", Number: 9, SessionID: "other", Provider: "github", Host: "ghe.example.com", Repo: "acme/app", SourceBranch: "s/other", TargetBranch: "main"},
		{URL: "fork", HTMLURL: "hfork", Number: 11, SessionID: "other", Provider: "github", Host: "github.com", Repo: "acme/app", HeadRepo: "outsider/app", SourceBranch: "s/forked", TargetBranch: "main"},
	}}
	svc := &Service{store: store}
	prs := []domain.PullRequest{
		{URL: "child", Number: 8, SessionID: "mine", Provider: "github", Host: "github.com", Repo: "acme/app", SourceBranch: "s/root/a", TargetBranch: "s/root"},
		{URL: "lost", Number: 10, SessionID: "mine", Provider: "github", Host: "github.com", Repo: "acme/app", SourceBranch: "s/lost", TargetBranch: "s/other"},
		{URL: "forkchild", Number: 12, SessionID: "mine", Provider: "github", Host: "github.com", Repo: "acme/app", SourceBranch: "s/fc", TargetBranch: "s/forked"},
	}
	parents := svc.stackParents(context.Background(), "proj-1", prs)
	if parents["child"].URL != "parent" {
		t.Fatalf("cross-session parent should mark the child: %+v", parents)
	}
	if _, ok := parents["lost"]; ok {
		t.Fatalf("branch match on another host must not mark: %+v", parents)
	}
	if _, ok := parents["forkchild"]; ok {
		t.Fatalf("fork-headed PR must not be a stack parent: %+v", parents)
	}
}

// A child inside a native stack only accepts same-stack parents; a child
// outside any native stack still accepts branch-inferred parents.
func TestStackParentsHonorNativeStackMembership(t *testing.T) {
	store := &fakeStore{openPRsByRepo: []domain.PullRequest{
		{URL: "otherstack", HTMLURL: "hother", Number: 5, SessionID: "other", Repo: "acme/app", StackNumber: 9, SourceBranch: "s/root", TargetBranch: "main"},
		// A same-branch decoy from another stack listed before the real
		// parent must not shadow it.
		{URL: "shadow", HTMLURL: "hshadow", Number: 7, SessionID: "other", Repo: "acme/app", StackNumber: 9, SourceBranch: "s/mid", TargetBranch: "main"},
		{URL: "samestack", HTMLURL: "hsame", Number: 6, SessionID: "other", Repo: "acme/app", StackNumber: 4, SourceBranch: "s/mid", TargetBranch: "s/base"},
	}}
	svc := &Service{store: store}
	prs := []domain.PullRequest{
		{URL: "nativechild", Number: 8, SessionID: "mine", Repo: "acme/app", StackNumber: 4, SourceBranch: "s/mid/x", TargetBranch: "s/mid"},
		{URL: "strandedchild", Number: 9, SessionID: "mine", Repo: "acme/app", StackNumber: 4, SourceBranch: "s/y", TargetBranch: "s/root"},
		{URL: "inferredchild", Number: 10, SessionID: "mine", Repo: "acme/app", SourceBranch: "s/z", TargetBranch: "s/root"},
	}
	parents := svc.stackParents(context.Background(), "proj-1", prs)
	if parents["nativechild"].URL != "samestack" {
		t.Fatalf("same-stack parent should match despite the same-branch decoy: %+v", parents)
	}
	if _, ok := parents["strandedchild"]; ok {
		t.Fatalf("native child must not adopt a parent from another stack: %+v", parents)
	}
	if parents["inferredchild"].URL != "otherstack" {
		t.Fatalf("non-native child keeps branch inference: %+v", parents)
	}
}
