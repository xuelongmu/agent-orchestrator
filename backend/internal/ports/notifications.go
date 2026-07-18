package ports

import (
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// NotificationIntent is the lifecycle-to-notification-producer contract. It is
// not an HTTP DTO; lifecycle fills it from facts it already has after the
// underlying session/PR state write succeeds.
type NotificationIntent struct {
	Type      domain.NotificationType
	SessionID domain.SessionID
	ProjectID domain.ProjectID
	PRURL     string
	CreatedAt time.Time
	// TitleOverride and BodyOverride let a centralized human handoff explain
	// the condition that automation could not deliver. Normal lifecycle
	// notifications leave them empty and use the standard type-based copy.
	TitleOverride string
	BodyOverride  string

	// Enrichment hints. These avoid storage reads on the hot path.
	SessionDisplayName string
	PRNumber           int
	PRTitle            string
	PRSourceBranch     string
	PRTargetBranch     string
	Provider           string
	Repo               string
}
