package domain

import (
	"encoding/json"
	"fmt"
	"time"
)

// These ID types are distinct string types so they can't be swapped at a call
// site by accident.
type (
	// SessionID identifies a session.
	SessionID string
	// ProjectID identifies a project.
	ProjectID string
	// IssueID identifies a tracker issue.
	IssueID string
)

// SessionKind distinguishes a worker session from an orchestrator session.
type SessionKind string

// Session kinds.
const (
	KindWorker       SessionKind = "worker"
	KindOrchestrator SessionKind = "orchestrator"
	// MaxSessionDependencies bounds one spawn request and its durable outgoing
	// graph fan-out. Scheduling semantics are intentionally outside Part 1.
	MaxSessionDependencies = 32
)

// SessionMetadata is the typed, off-status metadata for a session: operational
// handles and seed inputs used by Session Manager and reaper.
type SessionMetadata struct {
	WorkspaceKind   WorkspaceKind `json:"workspaceKind,omitempty"`
	Branch          string        `json:"branch,omitempty"`
	WorkspacePath   string        `json:"workspacePath,omitempty"`
	RuntimeHandleID string        `json:"runtimeHandleId,omitempty"`
	AgentSessionID  string        `json:"agentSessionId,omitempty"`
	Prompt          string        `json:"prompt,omitempty"`
	// PreviewURL is the browser preview target the desktop app opens for this
	// session. Set via `ao preview` (POST /sessions/{id}/preview); persisted so
	// it survives a daemon restart. Empty means no preview has been requested.
	PreviewURL string `json:"previewUrl,omitempty"`
	// PreviewRevision is a monotonic counter bumped on every `ao preview` call,
	// even when PreviewURL is unchanged. The desktop browser panel keys
	// navigation on it so a repeated `ao preview <same-url>` still refreshes.
	PreviewRevision int64 `json:"previewRevision,omitempty"`
	// PendingSubmitFingerprint latches the digest of a prompt that reached an
	// agent editor but was observed still waiting for submission. It is an
	// internal delivery fact: while set, callers must never paste that prompt
	// again. PendingSubmitRecoveryAttempted durably claims the single Enter-only
	// recovery attempt so daemon restarts cannot repeat it.
	PendingSubmitFingerprint       string `json:"pendingSubmitFingerprint,omitempty"`
	PendingSubmitRecoveryAttempted bool   `json:"pendingSubmitRecoveryAttempted,omitempty"`
	// MergedCleanupPending is a durable internal retry latch. Lifecycle sets it
	// before delegating merge-complete resource teardown. Ordinary terminal
	// observations preserve it; only successful teardown clears it. The SCM
	// poller retries latched sessions across polls/restarts, including sessions
	// whose runtime exited while cleanup was pending.
	MergedCleanupPending bool `json:"-"`
	// MergedCleanupPRURL identifies the terminal PR observation whose lifecycle
	// notification must resume after a transient teardown failure. The PR row
	// holds the remaining enrichment fields.
	MergedCleanupPRURL string `json:"-"`
}

// SessionRecord is the persistence shape. It intentionally stores only durable
// facts: identity, agent harness, activity_state, is_terminated, and operational
// metadata. The user-facing Status is derived from these facts plus PR facts.
type SessionRecord struct {
	ID          SessionID    `json:"id"`
	ProjectID   ProjectID    `json:"projectId"`
	IssueID     IssueID      `json:"issueId,omitempty"`
	Kind        SessionKind  `json:"kind"`
	Harness     AgentHarness `json:"harness,omitempty"`
	DisplayName string       `json:"displayName,omitempty"`
	// DependencyIDs is the internal, comparable encoding of declared dependency
	// ids. The API read model exposes DependsOn as a slice. Keeping the durable
	// record comparable preserves lifecycle snapshot/CAS invariants.
	DependencyIDs string `json:"-"`
	// DependencyPromotedAt is the durable exactly-once launch claim for a
	// dependency-gated session. It is intentionally not a display status.
	DependencyPromotedAt time.Time `json:"-"`
	DependencyPreparedAt time.Time `json:"-"`
	DependencyBasePrompt string    `json:"-"`
	// DependencyBranchPrefix/Suffix are create-time-only inputs that let the
	// store resolve an id-derived default branch inside the same transaction
	// that inserts the child, its launch prompt, and its dependency edges.
	DependencyBranchPrefix string `json:"-"`
	DependencyBranchSuffix string `json:"-"`
	// DependencyPromotionToken fences the in-flight external launch. A new
	// daemon holding the exclusive store lease clears an abandoned token before
	// retrying; completion accepts only the reserving token.
	DependencyPromotionToken     string    `json:"-"`
	DependencyPromotionClaimedAt time.Time `json:"-"`
	// DependencyLaunchSucceededAt is the token-fenced external-launch commit
	// point. A runtime handle without this marker is an interrupted attempt that
	// recovery must tear down rather than adopt as successfully launched.
	DependencyLaunchSucceededAt time.Time `json:"-"`
	Activity                    Activity  `json:"activity"`
	// FirstSignalAt is when the FIRST agent hook callback arrived for the
	// current spawn/restore: raw signal receipt, independent of the derived
	// activity state. Zero means no hook has ever reported, which deriveStatus
	// surfaces as StatusNoSignal after a grace period. Internal fact, not part
	// of the API read model.
	FirstSignalAt time.Time `json:"-"`
	IsTerminated  bool      `json:"isTerminated"`
	// Diagnostic is the latest bounded, privacy-scrubbed evidence captured at
	// an abnormal or terminal lifecycle boundary. Nil means no capture has
	// succeeded; capture failure never fabricates evidence.
	Diagnostic *LifecycleDiagnostic `json:"diagnostic,omitempty"`
	Metadata   SessionMetadata      `json:"-"`
	CreatedAt  time.Time            `json:"createdAt"`
	UpdatedAt  time.Time            `json:"updatedAt"`
}

// DependencyPending reports whether this session has declared prerequisites
// but has not yet consumed its durable promotion claim.
func (s SessionRecord) DependencyPending() bool {
	return s.DependencyIDs != "" && s.DependencyPromotedAt.IsZero() && !s.IsTerminated
}

// Session is the read-model returned across the API boundary: a SessionRecord
// plus the derived display Status.
type Session struct {
	SessionRecord
	Status           SessionStatus `json:"status" enum:"working,pr_open,draft,ci_failed,review_pending,changes_requested,approved,mergeable,merged,needs_input,rate_limited,idle,terminated,no_signal"`
	TerminalHandleID string        `json:"terminalHandleId,omitempty"`
	// DependencyPending is the durable launch-gate fact after synchronous
	// dependency reconciliation. Clients must use this instead of inferring
	// waiting state from the presence of DependsOn edges.
	DependencyPending bool `json:"dependencyPending,omitempty"`
	// Handoff is the immutable structured completion summary explicitly
	// submitted by this session's agent. Sealing it can satisfy dependency
	// scheduling, but it never terminates or otherwise mutates this session's
	// lifecycle facts.
	Handoff *AgentHandoff `json:"handoff,omitempty"`
	// DependsOn is the deduplicated, declared prerequisite graph for this
	// session. The graph is read-only over the API; the daemon scheduler gates
	// and promotes the child from durable completion facts.
	DependsOn []SessionID `json:"dependsOn,omitempty"`
	// PRs are the session's attributed pull requests (one session can own many).
	// They feed status derivation and are surfaced on the API read model. Not
	// serialized here: the HTTP boundary maps them to the curated wire shape.
	PRs []PRFacts `json:"-"`
}

// EncodeSessionDependencyIDs stores dependency ids in a comparable record
// field using collision-free JSON. String-only arrays cannot fail to marshal.
func EncodeSessionDependencyIDs(ids []SessionID) string {
	if len(ids) == 0 {
		return ""
	}
	encoded, _ := json.Marshal(ids)
	return string(encoded)
}

// DecodeSessionDependencyIDs returns the API/store slice for an encoded record.
func DecodeSessionDependencyIDs(encoded string) ([]SessionID, error) {
	if encoded == "" {
		return nil, nil
	}
	var ids []SessionID
	if err := json.Unmarshal([]byte(encoded), &ids); err != nil {
		return nil, fmt.Errorf("decode session dependency ids: %w", err)
	}
	if ids == nil {
		return nil, fmt.Errorf("decode session dependency ids: expected JSON array")
	}
	return ids, nil
}
