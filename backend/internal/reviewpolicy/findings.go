// Package reviewpolicy contains the provider-neutral, exact-head review gate
// vocabulary shared by merge readiness and automatic review coordination.
package reviewpolicy

import (
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// HasCurrentHeadHumanApproval reports whether a provider approval is both human
// and attached to the exact current PR head.
func HasCurrentHeadHumanApproval(reviews []ports.SCMReviewSummaryObservation, headSHA string) bool {
	for _, review := range reviews {
		if !review.IsBot && domain.ReviewDecision(review.State) == domain.ReviewApproved && review.CommitSHA == headSHA {
			return true
		}
	}
	return false
}

// HasUnresolvedRequiredComments is the merge-gate rule: every unresolved human
// comment blocks, while automated comments block only for Codex P0/P1 findings.
func HasUnresolvedRequiredComments(threads []ports.SCMReviewThreadObservation) bool {
	for _, thread := range threads {
		if thread.Resolved {
			continue
		}
		for _, comment := range thread.Comments {
			if !comment.IsBot || IsCodexReviewer(comment.Author) && HasP0OrP1(comment.Body) {
				return true
			}
		}
	}
	return false
}

// HasUnresolvedCodexP0P1 is the narrower automatic-review stop rule.
func HasUnresolvedCodexP0P1(threads []ports.SCMReviewThreadObservation) bool {
	for _, thread := range threads {
		if thread.Resolved {
			continue
		}
		for _, comment := range thread.Comments {
			if IsCodexReviewer(comment.Author) && HasP0OrP1(comment.Body) {
				return true
			}
		}
	}
	return false
}

// IsCodexReviewer recognizes the provider identities currently emitted for
// Codex reviews, with or without the conventional [bot] suffix.
func IsCodexReviewer(author string) bool {
	author = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(author)), "[bot]")
	return author == "chatgpt-codex-connector" || author == "codex"
}

// HasP0OrP1 recognizes the priority tags required by AO's reviewer prompt.
func HasP0OrP1(body string) bool {
	body = strings.ToLower(body)
	return strings.Contains(body, "[p0]") || strings.Contains(body, "[p1]")
}
