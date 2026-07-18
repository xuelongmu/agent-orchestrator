package reviewpolicy

import (
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func TestCurrentHeadApprovalIsHumanAndHeadBound(t *testing.T) {
	reviews := []ports.SCMReviewSummaryObservation{
		{Author: "codex[bot]", IsBot: true, State: string(domain.ReviewApproved), CommitSHA: "head-2"},
		{Author: "alice", State: string(domain.ReviewApproved), CommitSHA: "head-1"},
		{Author: "bob", State: string(domain.ReviewApproved), CommitSHA: "head-2"},
	}
	if !HasCurrentHeadHumanApproval(reviews, "head-2") {
		t.Fatal("expected current-head human approval")
	}
	if HasCurrentHeadHumanApproval(reviews[:2], "head-2") {
		t.Fatal("bot or stale approvals must not satisfy the gate")
	}
}

func TestUnresolvedRequiredCommentsMatchesMergeGate(t *testing.T) {
	tests := []struct {
		name string
		body string
		bot  bool
		want bool
	}{
		{name: "human suggestion", body: "please rename", want: true},
		{name: "codex p1", body: "[P1] lost update", bot: true, want: true},
		{name: "codex p2", body: "[P2] clearer name", bot: true, want: false},
		{name: "unrelated bot p1", body: "[P1] coverage", bot: true, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			author := "alice"
			if tt.bot {
				author = "chatgpt-codex-connector[bot]"
				if tt.name == "unrelated bot p1" {
					author = "coverage[bot]"
				}
			}
			threads := []ports.SCMReviewThreadObservation{{Comments: []ports.SCMReviewCommentObservation{{Author: author, Body: tt.body, IsBot: tt.bot}}}}
			if got := HasUnresolvedRequiredComments(threads); got != tt.want {
				t.Fatalf("HasUnresolvedRequiredComments() = %v, want %v", got, tt.want)
			}
		})
	}
}
