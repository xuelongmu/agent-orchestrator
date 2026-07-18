// Package scmready owns the provider-neutral definition of done for one exact
// pull-request head. Both lifecycle notifications and destructive merge
// actions use this gate so readiness cannot drift between observation and act.
package scmready

import (
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/reviewpolicy"
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
		reviewpolicy.HasUnresolvedRequiredComments(o.Review.Threads) {
		return false
	}
	if reviewDecision == domain.ReviewApproved && !reviewpolicy.HasCurrentHeadHumanApproval(o.Review.Reviews, o.PR.HeadSHA) {
		return false
	}
	return domain.Mergeability(o.Mergeability.State) == domain.MergeMergeable
}
