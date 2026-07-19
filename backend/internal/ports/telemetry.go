package ports

import (
	"context"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// TelemetryLevel is the severity of a telemetry event.
type TelemetryLevel string

const (
	// TelemetryLevelDebug marks verbose diagnostic events.
	TelemetryLevelDebug TelemetryLevel = "debug"
	// TelemetryLevelInfo marks normal operational events.
	TelemetryLevelInfo TelemetryLevel = "info"
	// TelemetryLevelWarn marks degraded but non-fatal events.
	TelemetryLevelWarn TelemetryLevel = "warn"
	// TelemetryLevelError marks failed operations.
	TelemetryLevelError TelemetryLevel = "error"
)

// TelemetryEvent is a structured operational/product event emitted by the
// daemon. Payload must be allowlisted at the call site; sinks may serialize it
// but must not mutate it.
type TelemetryEvent struct {
	// ID is optional for ordinary best-effort events. A non-empty ID is a
	// durable idempotency key: sinks must reuse it as their local primary key or
	// provider deduplication key when an event is replayed.
	ID         string
	Name       string
	Source     string
	OccurredAt time.Time
	Level      TelemetryLevel
	ProjectID  *domain.ProjectID
	SessionID  *domain.SessionID
	RequestID  string
	Payload    map[string]any
}

// EventSink consumes structured telemetry events. Implementations should be
// best-effort: a slow or failing sink must not break the user action that
// emitted the event.
type EventSink interface {
	Emit(ctx context.Context, ev TelemetryEvent)
	Close(ctx context.Context) error
}

// DurableLocalEventSink reports whether the sink includes durable local event
// storage. Lifecycle uses this capability before creating transactional review
// telemetry; disabled/no-op and remote-only sinks return false.
type DurableLocalEventSink interface {
	EventSink
	DurableLocalTelemetry() bool
}
