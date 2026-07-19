package ports

import (
	"context"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// NotificationSink is the shared write-side boundary for user-facing
// notifications. Producers submit intents without depending on persistence or
// live-delivery details; the daemon wires them to the notify pipeline.
type NotificationSink interface {
	Notify(ctx context.Context, intent NotificationIntent) error
}

// NotificationIntent is the producer-to-notification-pipeline contract. It is
// not an HTTP DTO. Session lifecycle producers fill the session/project fields;
// daemon-wide control-plane producers deliberately leave them empty.
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
