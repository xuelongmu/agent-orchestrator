// Package scmready owns the provider-neutral definition of done for one exact
// pull-request head. Both lifecycle notifications and destructive merge
// actions use this gate so readiness cannot drift between observation and act.
package scmready

import (
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// IsReadyToMerge reports whether a complete SCM observation proves that the
// exact PR head has passing CI, complete current-head review facts, no required
// unresolved feedback, and provider mergeability.
func IsReadyToMerge(o ports.SCMObservation) bool {
	if !o.Fetched || o.PR.Merged || o.PR.Closed || o.PR.Draft {
		return false
	}
	if o.PR.HeadSHA == "" ||
		o.CI.HeadSHA != o.PR.HeadSHA ||
		o.Review.HeadSHA == "" ||
		o.Review.HeadSHA != o.PR.HeadSHA ||
		o.Review.Partial {
		return false
	}
	ci := domain.CIState(o.CI.Summary)
	if ci == "" {
		ci = domain.CIUnknown
	}
	if ci != domain.CIPassing {
		return false
	}
	reviewDecision := domain.ReviewDecision(o.Review.Decision)
	if reviewDecision == domain.ReviewChangesRequest ||
		reviewDecision == domain.ReviewRequired ||
		hasUnresolvedRequiredComments(o.Review.Threads) {
		return false
	}
	if reviewDecision == domain.ReviewApproved && !hasCurrentHeadApproval(o.Review.Reviews, o.PR.HeadSHA) {
		return false
	}
	return domain.Mergeability(o.Mergeability.State) == domain.MergeMergeable
}

func hasCurrentHeadApproval(reviews []ports.SCMReviewSummaryObservation, headSHA string) bool {
	for _, review := range reviews {
		if !review.IsBot && domain.ReviewDecision(review.State) == domain.ReviewApproved && review.CommitSHA == headSHA {
			return true
		}
	}
	return false
}

func hasUnresolvedRequiredComments(threads []ports.SCMReviewThreadObservation) bool {
	for _, thread := range threads {
		if thread.Resolved {
			continue
		}
		for _, comment := range thread.Comments {
			if !comment.IsBot {
				return true
			}
			if isCodexReviewBot(comment.Author) && isP0OrP1Finding(comment.Body) {
				return true
			}
		}
	}
	return false
}

func isCodexReviewBot(author string) bool {
	author = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(author)), "[bot]")
	return author == "chatgpt-codex-connector" || author == "codex"
}

func isP0OrP1Finding(body string) bool {
	body = strings.ToLower(body)
	return strings.Contains(body, "[p0]") || strings.Contains(body, "[p1]")
}
