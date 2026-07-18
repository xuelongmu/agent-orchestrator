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
	Resolved bool
}

// SCMReviewThreadResolver resolves provider-owned review-thread node IDs.
// Implementations must be idempotent: resolving an already-resolved thread is
// a successful confirmation, not an error.
type SCMReviewThreadResolver interface {
	ResolveReviewThread(ctx context.Context, threadID string) (SCMReviewThreadResolution, error)
}
