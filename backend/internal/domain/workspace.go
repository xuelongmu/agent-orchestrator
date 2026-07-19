package domain

// WorkspaceKind describes the filesystem shape assigned to a session.
// Worktree is the dedicated git-backed default; scratch is an ephemeral empty
// directory; dir runs directly in the registered project's shared path.
type WorkspaceKind string

const (
	// WorkspaceKindWorktree selects a dedicated git worktree.
	WorkspaceKindWorktree WorkspaceKind = "worktree"
	// WorkspaceKindScratch selects a dedicated ephemeral directory without git.
	WorkspaceKindScratch WorkspaceKind = "scratch"
	// WorkspaceKindDir selects the registered project's shared directory.
	WorkspaceKindDir WorkspaceKind = "dir"
)

// WithDefault preserves the historical git-worktree behavior for records and
// requests that predate workspace kinds.
func (k WorkspaceKind) WithDefault() WorkspaceKind {
	if k == "" {
		return WorkspaceKindWorktree
	}
	return k
}

// IsKnown reports whether k is one of the supported workspace shapes. Empty is
// accepted as the backwards-compatible worktree default.
func (k WorkspaceKind) IsKnown() bool {
	switch k.WithDefault() {
	case WorkspaceKindWorktree, WorkspaceKindScratch, WorkspaceKindDir:
		return true
	default:
		return false
	}
}
