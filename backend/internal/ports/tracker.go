package ports

import (
	"context"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// TrackerIntakeClaim is one token-fenced attempt to spawn a session for an
// issue discovered by tracker intake. IssueID is provider-native; Provider and
// Repo are separate parts of the durable claim key.
type TrackerIntakeClaim struct {
	ProjectID      domain.ProjectID
	Provider       domain.TrackerProvider
	Repo           string
	IssueID        string
	OwnerToken     string
	ClaimedAt      time.Time
	LeaseExpiresAt time.Time
}

// TrackerIntakeClaimResult explains why an intake candidate may or may not
// proceed to the external spawn side effect.
type TrackerIntakeClaimResult uint8

const (
	TrackerIntakeClaimAcquired TrackerIntakeClaimResult = iota + 1
	TrackerIntakeClaimAlreadyProcessed
	TrackerIntakeClaimBusy
	TrackerIntakeClaimCapacityReached
)

// Tracker is the outbound read-only port for issue trackers:
//
//   - Get returns a normalized snapshot of one issue, used by spawn-bootstrap
//     to hydrate the agent prompt.
//   - List returns a filtered slice of issues in a repo, used when the SM
//     needs to enumerate work (e.g. backlog view, status sweeps).
//   - Preflight verifies the configured credential is actually valid against
//     the provider so daemons fail fast at startup, not at first request.
//
// Provider differences are absorbed inside each adapter via
// domain.NormalizedIssueState. Richer per-provider metadata belongs behind a
// separate port.
type Tracker interface {
	Get(ctx context.Context, id domain.TrackerID) (domain.Issue, error)
	List(ctx context.Context, repo domain.TrackerRepo, filter domain.ListFilter) ([]domain.Issue, error)
	Preflight(ctx context.Context) error
}
