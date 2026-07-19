package ports

import (
	"errors"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// ErrSessionNotFound reports an observation for an unknown session id.
var ErrSessionNotFound = errors.New("session not found")

// ErrHandoffConflict means a session already has a different immutable
// completion handoff. An exact replay is not an error.
var ErrHandoffConflict = errors.New("session handoff already submitted with a different payload")

// Dependency graph validation errors. Persistence owns the atomic graph check;
// service layers map these sentinels to stable API errors.
var (
	ErrDependencySelf     = errors.New("session dependency cannot reference itself")
	ErrDependencyCycle    = errors.New("session dependency would create a cycle")
	ErrDependencyNotFound = errors.New("session dependency not found")
	ErrDependencyProject  = errors.New("session dependency must belong to the same project")
	ErrDependencyInvalid  = errors.New("invalid session dependency id")
	ErrDependencyLimit    = errors.New("too many session dependencies")
)

// SpawnConfig is the request to start a new session: which project/issue, which
// agent harness, and the branch/prompt the agent launches with.
type SpawnConfig struct {
	ProjectID domain.ProjectID
	IssueID   domain.IssueID
	// IssueContext is optional pre-fetched tracker context for the task prompt.
	// Standing rules stay in SystemPrompt; issue facts belong to the user task.
	IssueContext  string
	Kind          domain.SessionKind
	Harness       domain.AgentHarness
	WorkspaceKind domain.WorkspaceKind
	Branch        string
	Prompt        string
	DependsOn     []domain.SessionID

	// DisplayName is the user-facing sidebar label. Empty falls back to the
	// session id in the read model (e.g. orchestrator sessions).
	DisplayName string
}
