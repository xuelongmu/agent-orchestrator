package ports

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// PRWriter records the PR facts a PR observation carries. The pr table's own DB
// triggers emit the CDC; this just writes the rows.
type PRWriter interface {
	// WritePR persists a full PR observation — scalar facts, check runs, and the
	// replacement comment set — in one transaction, so the rows and the CDC
	// events they emit are all-or-nothing.
	WritePR(ctx context.Context, pr domain.PullRequest, checks []domain.PullRequestCheck, comments []domain.PullRequestComment) error
}

// ReviewWriteMode describes how an SCM observation should update normalized
// review-thread/comment rows.
type ReviewWriteMode int

// Actionable PR checkout failure kinds.
const (
	// ReviewWritePreserve leaves stored review rows untouched. Metadata/CI-only
	// refreshes and failed review fetches use this mode.
	ReviewWritePreserve ReviewWriteMode = iota
	// ReviewWriteReplace treats the fetched review rows as a complete snapshot
	// and replaces all stored review rows for the PR.
	ReviewWriteReplace
	// ReviewWriteMerge treats the fetched review rows as a partial window:
	// fetched threads/comments are updated while older unseen rows are preserved.
	ReviewWriteMerge
)

// SCMWriter records provider-neutral SCM observations. reviewMode decides
// whether review facts are preserved, replaced with a complete snapshot, or
// merged as a bounded partial window.
type SCMWriter interface {
	WriteSCMObservation(ctx context.Context, pr domain.PullRequest, checks []domain.PullRequestCheck, reviews []domain.PullRequestReview, threads []domain.PullRequestReviewThread, comments []domain.PullRequestComment, reviewMode ReviewWriteMode) error
}

// PRClaimer atomically moves (or creates) a PR row for a target session and
// persists the live SCM facts observed for that PR in the same transaction.
type PRClaimer interface {
	// CheckPRClaim performs the same active-owner guard as ClaimPR without
	// changing ownership. Callers use it before mutating a claimant workspace.
	CheckPRClaim(ctx context.Context, prURL string, claimant domain.SessionID, allowActiveTakeover bool) (ClaimOutcome, error)
	ClaimPR(ctx context.Context, pr domain.PullRequest, checks []domain.PullRequestCheck, reviews []domain.PullRequestReview, threads []domain.PullRequestReviewThread, comments []domain.PullRequestComment, reviewMode ReviewWriteMode, allowActiveTakeover bool, workspaceBranch string) (ClaimOutcome, error)
}

// ErrPRClaimedByActiveSession is returned by PRClaimer.ClaimPR when takeover is
// explicitly disallowed and the existing owner is still alive.
var ErrPRClaimedByActiveSession = errors.New("pr claimed by active session")

// PRClaimedByActiveSessionError carries the active owner that blocked a claim.
type PRClaimedByActiveSessionError struct {
	Owner domain.SessionID
}

func (e PRClaimedByActiveSessionError) Error() string {
	return fmt.Sprintf("%s: %s", ErrPRClaimedByActiveSession, e.Owner)
}

func (e PRClaimedByActiveSessionError) Unwrap() error { return ErrPRClaimedByActiveSession }

// ClaimOutcome describes what owner, if any, a successful claim replaced.
type ClaimOutcome struct {
	PreviousOwner   domain.SessionID
	OwnerTerminated bool
}

// PRCheckoutErrorKind identifies a claim checkout failure that the caller can
// correct or retry. These errors are intentionally typed so the HTTP boundary
// does not turn local Git state into an opaque 500 response.
type PRCheckoutErrorKind string

const (
	// PRCheckoutWorkspaceDirty means local changes prevent a safe checkout.
	PRCheckoutWorkspaceDirty PRCheckoutErrorKind = "workspace_dirty"
	// PRCheckoutFailed means Git or GitHub CLI could not prepare the branch.
	PRCheckoutFailed PRCheckoutErrorKind = "checkout_failed"
	// PRCheckoutHeadMismatch means checkout succeeded at a stale or unexpected commit.
	PRCheckoutHeadMismatch PRCheckoutErrorKind = "head_mismatch"
	// PRCheckoutBranchDiverged means the private claim branch has local commits to preserve.
	PRCheckoutBranchDiverged PRCheckoutErrorKind = "branch_diverged"
)

// PRCheckoutError carries user-actionable checkout context without coupling
// SCM adapters to HTTP response types.
type PRCheckoutError struct {
	Kind         PRCheckoutErrorKind
	Branch       string
	ExpectedHead string
	ActualHead   string
	Err          error
}

func (e PRCheckoutError) Error() string {
	switch e.Kind {
	case PRCheckoutWorkspaceDirty:
		return fmt.Sprintf("claim workspace has uncommitted changes; cannot switch to branch %q safely", e.Branch)
	case PRCheckoutHeadMismatch:
		return fmt.Sprintf("claim workspace HEAD %s does not match PR head %s", e.ActualHead, e.ExpectedHead)
	case PRCheckoutBranchDiverged:
		return fmt.Sprintf("claim branch %q is at %s, not PR head %s; refusing to discard local commits", e.Branch, e.ActualHead, e.ExpectedHead)
	default:
		if e.Err != nil {
			return fmt.Sprintf("claim checkout failed: %v", e.Err)
		}
		return "claim checkout failed"
	}
}

func (e PRCheckoutError) Unwrap() error { return e.Err }

// AgentMessenger injects a message into a running agent. An empty message
// sends only the submit keystroke (Enter) — callers use it to nudge a pasted
// prompt that was not submitted; every runtime must honor this contract.
type AgentMessenger interface {
	Send(ctx context.Context, id domain.SessionID, message string) error
}

// ---- runtime / agent / workspace plugin ports ----

// Runtime is the full runtime adapter contract: session creation/teardown plus
// liveness probing for reapers and terminal attachment.
type Runtime interface {
	Create(ctx context.Context, cfg RuntimeConfig) (RuntimeHandle, error)
	Destroy(ctx context.Context, handle RuntimeHandle) error
	GetOutput(ctx context.Context, handle RuntimeHandle, lines int) (string, error)
	IsAlive(ctx context.Context, handle RuntimeHandle) (bool, error)
}

// RuntimeConfig is the spec for launching a session's process in a Runtime.
// Argv is the agent's launch command as discrete arguments; each Runtime
// shell-quotes it for its own shell, so the command survives args with spaces
// (e.g. a prompt) without the caller guessing the target shell's quoting.
type RuntimeConfig struct {
	SessionID     domain.SessionID
	WorkspacePath string
	Argv          []string
	Env           map[string]string
}

// RuntimeHandle identifies a live runtime instance. Its ID is opaque outside
// the concrete runtime adapter.
type RuntimeHandle struct {
	ID string
}

// Stream is one live terminal attach: PTY-like bytes plus resize. Returned
// already-open by a Runtime's Attach. tmux backs it with a local PTY around
// their attach CLI; conpty backs it with a loopback connection to the pty-host.
type Stream interface {
	io.ReadWriteCloser
	Resize(rows, cols uint16) error
}

// Attacher opens a fresh attach Stream for a session handle, sized rows x cols from
// birth (0 means size not yet known). ctx cancellation must terminate the stream.
type Attacher interface {
	Attach(ctx context.Context, handle RuntimeHandle, rows, cols uint16) (Stream, error)
}

// The Agent port and its supporting types live in agent.go.

// Workspace provisions the filesystem location an agent works in. Implementations
// may be git-backed worktrees, ephemeral scratch directories, or shared dirs.
type Workspace interface {
	Create(ctx context.Context, cfg WorkspaceConfig) (WorkspaceInfo, error)
	Destroy(ctx context.Context, info WorkspaceInfo) error
	Restore(ctx context.Context, cfg WorkspaceConfig) (WorkspaceInfo, error)
	// ForceDestroy removes an isolated workspace unconditionally, bypassing the
	// dirty-worktree refusal that the git adapter enforces. Shared-directory
	// adapters must leave their directory untouched.
	ForceDestroy(ctx context.Context, info WorkspaceInfo) error
	// StashUncommitted captures git worktree state before forced teardown.
	// Non-git adapters return an empty ref because their lifecycle never uses
	// git preservation markers.
	StashUncommitted(ctx context.Context, info WorkspaceInfo) (ref string, err error)
	// ApplyPreserved replays a git capture created by StashUncommitted. Non-git
	// adapters accept only an empty ref.
	ApplyPreserved(ctx context.Context, info WorkspaceInfo, ref string) error
}

// WorkspaceProject is an optional extension for projects composed from a
// root-as-repo parent plus child repositories. It materialises the parent
// worktree at the session root and each child repo at its registered relative
// path inside that root.
type WorkspaceProject interface {
	CreateWorkspaceProject(ctx context.Context, cfg WorkspaceProjectConfig) (WorkspaceProjectInfo, error)
	DestroyWorkspaceProject(ctx context.Context, info WorkspaceProjectInfo) error
}

// Workspace-level sentinels surfaced through Create/Restore/Destroy so callers
// can map them to typed errors rather than collapsing every adapter failure
// into an opaque 500. Adapters wrap these via fmt.Errorf("...: %w", sentinel).
var (
	// ErrWorkspaceBranchCheckedOutElsewhere reports the requested branch is
	// already checked out in another worktree of the same repo.
	ErrWorkspaceBranchCheckedOutElsewhere = errors.New("workspace: branch is already checked out in another worktree")
	// ErrWorkspaceBranchNotFetched reports the requested branch exists nowhere
	// reachable (no local head, no remote-tracking branch, no tag).
	ErrWorkspaceBranchNotFetched = errors.New("workspace: branch is not fetched")
	// ErrWorkspaceBranchInvalid reports the requested branch name is not a valid
	// git ref (rejected by `git check-ref-format`).
	ErrWorkspaceBranchInvalid = errors.New("workspace: invalid branch name")
	// ErrWorkspaceDirty reports Destroy refused to remove a workspace because
	// it holds uncommitted changes or untracked files. Teardown is never
	// forced; callers treat the workspace as intentionally preserved.
	ErrWorkspaceDirty = errors.New("workspace: uncommitted changes present")
	// ErrWorkspaceStale reports an AO-managed workspace path that can no longer
	// be restored. Replacement paths may skip preservation for this state after
	// path-safety checks, while real preserve failures remain fatal.
	ErrWorkspaceStale = errors.New("workspace: stale managed worktree")
	// ErrPreservedConflict is returned by ApplyPreserved when replaying a
	// preserved ref onto the worktree produces merge conflicts. The ref is
	// kept intact (never deleted on conflict); the working tree is left with
	// conflict markers for manual resolution. Adapters wrap this sentinel via
	// fmt.Errorf so callers can match it with errors.Is.
	ErrPreservedConflict = errors.New("workspace: preserved apply produced conflicts")
	// ErrRuntimePrerequisite reports a missing host prerequisite for the selected
	// runtime before a session can be created.
	ErrRuntimePrerequisite = errors.New("runtime: prerequisite missing")
)

// WorkspaceConfig is the spec for creating or restoring a session's workspace.
type WorkspaceConfig struct {
	ProjectID     domain.ProjectID
	SessionID     domain.SessionID
	Kind          domain.SessionKind
	WorkspaceKind domain.WorkspaceKind
	// SessionPrefix is the human-readable project prefix used to name the
	// orchestrator worktree. Defaults to a truncation of ProjectID when empty.
	SessionPrefix string
	Branch        string
	// BaseBranch is the per-project default branch new session branches are
	// created from. Empty falls back to the workspace adapter's own default.
	BaseBranch string
	// RepoPath optionally overrides ProjectID-based repo resolution.
	RepoPath string
	// Path optionally supplies an existing workspace path for restore.
	Path string
}

// WorkspaceInfo describes a created workspace. Branch is empty for non-git kinds.
type WorkspaceInfo struct {
	Path          string
	Branch        string
	WorkspaceKind domain.WorkspaceKind
	SessionID     domain.SessionID
	ProjectID     domain.ProjectID
	// RepoPath optionally overrides ProjectID-based repo resolution. It is used
	// when the normal workspace lifecycle primitives operate on one child repo
	// inside a workspace project.
	RepoPath string
}

// WorkspaceProjectConfig describes a multi-repo workspace session. RootRepoPath
// and child RepoPath values are absolute paths to the canonical repositories.
type WorkspaceProjectConfig struct {
	ProjectID     domain.ProjectID
	SessionID     domain.SessionID
	Kind          domain.SessionKind
	SessionPrefix string
	Branch        string
	RootRepoPath  string
	BaseBranch    string
	Repos         []WorkspaceProjectRepoConfig
}

// WorkspaceProjectRepoConfig describes one registered child repo in a
// workspace project session.
type WorkspaceProjectRepoConfig struct {
	Name         string
	RelativePath string
	RepoPath     string
	BaseBranch   string
}

// WorkspaceProjectInfo returns the root worktree plus every child worktree.
// Worktrees are ordered root first, then children in creation order.
type WorkspaceProjectInfo struct {
	Root      WorkspaceInfo
	Worktrees []WorkspaceRepoInfo
}

// WorkspaceRepoInfo describes one materialized repo worktree in a workspace
// project session.
type WorkspaceRepoInfo struct {
	RepoName     string
	RepoPath     string
	Path         string
	Branch       string
	BaseSHA      string
	SessionID    domain.SessionID
	ProjectID    domain.ProjectID
	RelativePath string
}
