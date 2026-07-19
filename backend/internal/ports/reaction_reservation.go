package ports

import "github.com/aoagents/agent-orchestrator/backend/internal/domain"

// PRReactionFence pins a delivery to every PR snapshot represented by its
// message. HeadSHA must be nonempty: head-specific automation never silently
// downgrades to session-only fencing.
type PRReactionFence struct {
	PRURL     string           `json:"prUrl"`
	SessionID domain.SessionID `json:"sessionId"`
	HeadSHA   string           `json:"headSha"`
}

// PRReactionReservationStatus classifies an atomic pre-send claim.
type PRReactionReservationStatus string

// PRReactionReserved and related values classify the durable pre-send decision.
const (
	PRReactionReserved  PRReactionReservationStatus = "reserved"
	PRReactionAccounted PRReactionReservationStatus = "accounted"
	PRReactionExhausted PRReactionReservationStatus = "exhausted"
	PRReactionBusy      PRReactionReservationStatus = "busy"
	// PRReactionUncertain means a prior owner crossed the durable send
	// boundary but did not commit or release. Callers must not resend or
	// auto-recover: whether the pane write landed is unknowable. Lifecycle
	// accounts the reaction and durably alerts an operator so unrelated work can
	// continue.
	PRReactionUncertain PRReactionReservationStatus = "uncertain"
	// PRReactionStale means PR ownership or exact head no longer matches the
	// observation that authorized this reaction.
	PRReactionStale PRReactionReservationStatus = "stale"
)

// PRReactionReservation is the durable result of checking or reserving one
// exact PR lifecycle reaction.
type PRReactionReservation struct {
	Status    PRReactionReservationStatus
	Signature string
	Attempts  int
}
