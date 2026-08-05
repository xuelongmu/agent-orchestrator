package domain

import (
	"strings"
	"time"
)

// ---- PR read model ----

// PRFacts is the per-session PR snapshot the status derivation reads from the
// pr table.
type PRFacts struct {
	URL            string
	Number         int
	HeadSHA        string
	Draft          bool
	Merged         bool
	Closed         bool
	CI             CIState
	Review         ReviewDecision
	Mergeability   Mergeability
	ReviewComments bool // has unresolved review comments (any author) to address
	// Repo/HeadRepo mirror PullRequest: a differing head repo marks a fork PR,
	// whose source branch is not an AO-owned stack parent. Empty HeadRepo means
	// unobserved (legacy rows) and is treated as same-repo.
	Repo     string
	HeadRepo string
	// StackNumber mirrors PullRequest.StackNumber: the provider's native stack
	// membership, zero when none reported.
	StackNumber  int
	SourceBranch string
	TargetBranch string
	UpdatedAt    time.Time
}

// HeadInBaseRepo mirrors PullRequest.HeadInBaseRepo for the facts snapshot.
func (p PRFacts) HeadInBaseRepo() bool {
	return p.HeadRepo == "" || strings.EqualFold(p.HeadRepo, p.Repo)
}

// NativeStackAllowsParent reports whether native stacked-PR membership permits
// a parent edge. A child inside a native stack only accepts parents from the
// same stack — the provider's stack is authoritative for the chain — while a
// child outside any native stack (zero) accepts branch-inferred parents.
func NativeStackAllowsParent(childStack, parentStack int) bool {
	return childStack <= 0 || childStack == parentStack
}

// PullRequest is the app-level representation of one tracked pull request as
// persisted by the PR store. It is intentionally separate from the sqlc
// generated sqlite row type so storage details do not leak outside sqlite.
type PullRequest struct {
	URL          string
	SessionID    SessionID
	Number       int
	Draft        bool
	Merged       bool
	Closed       bool
	CI           CIState
	Review       ReviewDecision
	Mergeability Mergeability
	UpdatedAt    time.Time

	Provider string
	Host     string
	Repo     string
	// HeadRepo is the full name of the repository the head branch lives in.
	// It differs from Repo for fork PRs; empty means unobserved (legacy rows).
	HeadRepo string

	// StackNumber is the provider's native stack number when the PR belongs to
	// a native stack (GitHub public preview); zero means none reported and
	// branch-topology inference applies alone. Position/size are context.
	StackNumber   int
	StackPosition int
	StackSize     int

	SourceBranch   string
	TargetBranch   string
	HeadSHA        string
	Title          string
	Additions      int
	Deletions      int
	ChangedFiles   int
	Author         string
	BaseSHA        string
	MergeCommitSHA string

	ProviderState            string
	ProviderMergeable        string
	ProviderMergeStateStatus string
	HTMLURL                  string

	CreatedAtProvider time.Time
	UpdatedAtProvider time.Time
	MergedAtProvider  time.Time
	ClosedAtProvider  time.Time

	MetadataHash string
	CIHash       string
	ReviewHash   string

	ObservedAt       time.Time
	CIObservedAt     time.Time
	ReviewObservedAt time.Time
}

// HeadInBaseRepo reports whether the PR's head branch lives in its base
// repository. A fork-headed PR's source branch is not an AO-owned branch, so
// it can never be a stack parent. Empty means unobserved (legacy rows) and is
// treated as same-repo for backwards compatibility.
func (p PullRequest) HeadInBaseRepo() bool {
	return p.HeadRepo == "" || strings.EqualFold(p.HeadRepo, p.Repo)
}

// PullRequestCheck is one normalized CI check run for a pull request.
type PullRequestCheck struct {
	Name       string
	CommitHash string
	Status     PRCheckStatus
	Conclusion string
	URL        string
	Details    string
	LogTail    string
	CreatedAt  time.Time
}

// PullRequestComment is one normalized review comment for a pull request.
type PullRequestComment struct {
	ThreadID  string
	ID        string
	Author    string
	File      string
	Line      int
	Body      string
	URL       string
	Resolved  bool
	IsBot     bool
	CreatedAt time.Time
}

// PullRequestReviewThread is one normalized review thread for a pull request.
type PullRequestReviewThread struct {
	ThreadID     string
	Path         string
	Line         int
	Resolved     bool
	IsBot        bool
	SemanticHash string
	UpdatedAt    time.Time
}

// PullRequestReview is one submitted provider review for a pull request.
type PullRequestReview struct {
	ID          string
	Author      string
	State       ReviewDecision
	URL         string
	IsBot       bool
	SubmittedAt time.Time
}

// CIState is the aggregate CI status of a PR.
type CIState string

// CI states.
const (
	CIUnknown CIState = "unknown"
	CIPending CIState = "pending"
	CIPassing CIState = "passing"
	CIFailing CIState = "failing"
)

// ReviewDecision is the aggregate human-review verdict on a PR.
type ReviewDecision string

// Review decisions.
const (
	ReviewNone           ReviewDecision = "none"
	ReviewApproved       ReviewDecision = "approved"
	ReviewChangesRequest ReviewDecision = "changes_requested"
	ReviewRequired       ReviewDecision = "review_required"
)

// Mergeability is whether a PR can currently be merged.
type Mergeability string

// Mergeability states.
const (
	MergeUnknown     Mergeability = "unknown"
	MergeMergeable   Mergeability = "mergeable"
	MergeConflicting Mergeability = "conflicting"
	MergeBlocked     Mergeability = "blocked"
	MergeUnstable    Mergeability = "unstable"
)

// PRState is the normalized lifecycle of one tracked pull request as stored in
// the pr table.
type PRState string

// PR states.
const (
	PRStateDraft  PRState = "draft"
	PRStateOpen   PRState = "open"
	PRStateMerged PRState = "merged"
	PRStateClosed PRState = "closed"
)

// PRCheckStatus is one CI check run's normalized status.
type PRCheckStatus string

// PR check statuses.
const (
	PRCheckUnknown    PRCheckStatus = "unknown"
	PRCheckQueued     PRCheckStatus = "queued"
	PRCheckInProgress PRCheckStatus = "in_progress"
	PRCheckPassed     PRCheckStatus = "passed"
	PRCheckFailed     PRCheckStatus = "failed"
	PRCheckSkipped    PRCheckStatus = "skipped"
	PRCheckCancelled  PRCheckStatus = "cancelled"
)
