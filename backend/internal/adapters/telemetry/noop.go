package telemetry

import (
	"context"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// NoopSink discards every event.
type NoopSink struct{}

// DurableLocalTelemetry reports that telemetry is disabled.
func (NoopSink) DurableLocalTelemetry() bool { return false }

// Emit discards the event.
func (NoopSink) Emit(context.Context, ports.TelemetryEvent) {}

// Close is a no-op.
func (NoopSink) Close(context.Context) error { return nil }
