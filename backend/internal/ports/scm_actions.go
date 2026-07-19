package ports

import (
	"context"
	"errors"
)

// Provider-neutral merge failures that callers can map without depending on
// provider HTTP status codes or response bodies.
var (
	ErrSCMHeadChanged      = errors.New("scm: pull request head changed")
	ErrSCMNotMergeable     = errors.New("scm: pull request not mergeable")
	ErrSCMPermissionDenied = errors.New("scm: permission denied")
)

// SCMMergeMethod is the provider-neutral merge strategy.
type SCMMergeMethod string

const (
	// SCMMergeSquash combines the pull request commits into one base-branch commit.
	SCMMergeSquash SCMMergeMethod = "squash"
)

// SCMMergeRequest identifies one pull request and pins the mutation to the
// exact head that was reviewed. Providers must reject a different live head.
type SCMMergeRequest struct {
	PR              SCMPRRef
	ExpectedHeadSHA string
	Method          SCMMergeMethod
}

// SCMMergeResult is the provider-neutral successful merge outcome.
type SCMMergeResult struct {
	MergeCommitSHA string
}

// SCMMerger executes guarded pull-request merge mutations.
type SCMMerger interface {
	MergePullRequest(ctx context.Context, request SCMMergeRequest) (SCMMergeResult, error)
}

// SCMReviewThreadResolution is the provider-neutral outcome of ensuring one
// review thread is resolved. Resolved must only be true when the provider's
// response confirms the thread's resolved state.
type SCMReviewThreadResolution struct {
	ThreadID string
	ReplyID  string
	Resolved bool
}

// SCMReviewThreadResolver resolves provider-owned review-thread node IDs.
// Implementations must be idempotent: resolving an already-resolved thread is
// a successful confirmation, not an error.
type SCMReviewThreadResolver interface {
	ResolveReviewThread(ctx context.Context, threadID string) (SCMReviewThreadResolution, error)
}

// SCMDeferredIssueRequest describes a review finding that belongs in the
// backlog instead of the current PR.
type SCMDeferredIssueRequest struct {
	PRURL string
	Title string
	Body  string
	// ActionKey is a stable, provider-visible idempotency marker for the finding.
	ActionKey string
}

// SCMDeferredIssue is the provider-confirmed backlog item.
type SCMDeferredIssue struct {
	URL string
}

// SCMIssueFiler ensures one provider issue exists in the repository owning a
// PR. Implementations must search for ActionKey before creating a new issue.
type SCMIssueFiler interface {
	FileDeferredIssue(ctx context.Context, request SCMDeferredIssueRequest) (SCMDeferredIssue, error)
}

// SCMReviewDismissalRequest identifies the provider review that must no longer
// block mergeability after all of its findings have been deferred.
type SCMReviewDismissalRequest struct {
	PRURL    string
	ReviewID string
	Message  string
}

// SCMReviewDismissal is the provider-confirmed review clearing outcome.
type SCMReviewDismissal struct {
	Cleared bool
}

// SCMReviewDismisser idempotently clears a provider changes-requested review.
type SCMReviewDismisser interface {
	DismissReview(ctx context.Context, request SCMReviewDismissalRequest) (SCMReviewDismissal, error)
}

// SCMReviewThreadBinding pins a provider thread mutation to the originating
// PR, submitted review, and finding location.
type SCMReviewThreadBinding struct {
	PRURL     string
	ReviewID  string
	ThreadID  string
	File      string
	Body      string
	ActionKey string
	IssueURL  string
}

// SCMFindingThreadDeflector links a review thread to its backlog issue and
// resolves it. Binding validation is read-only and must happen before any
// mutation. Deflection must be idempotent by ActionKey and must not report
// success unless resolution is provider-confirmed.
type SCMFindingThreadDeflector interface {
	ReviewThreadBound(ctx context.Context, binding SCMReviewThreadBinding) (bool, error)
	DeflectReviewThread(ctx context.Context, binding SCMReviewThreadBinding) (SCMReviewThreadResolution, error)
}
