package telemetry

import (
	"context"
	"sync"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

const lifecyclePollSampleInterval = 15 * time.Minute

// LifecyclePollSamplingSink forwards every failed or overrun lifecycle poll,
// but samples healthy completions to one event per fixed UTC interval. The
// deterministic remote ID keeps the same interval idempotent at PostHog when a
// daemon restart repeats it. This wrapper belongs on the remote branch only;
// the unfiltered local branch must receive every raw poll.
type LifecyclePollSamplingSink struct {
	next      ports.EventSink
	namespace string
	interval  time.Duration

	mu   sync.Mutex
	seen map[time.Time]struct{}
	now  func() time.Time
}

// NewLifecyclePollSamplingSink applies AO's explicit remote lifecycle-poll
// policy: at most one healthy sample per fixed 15-minute UTC bucket, while all
// failures and overruns pass through immediately.
func NewLifecyclePollSamplingSink(next ports.EventSink, namespace string) *LifecyclePollSamplingSink {
	return &LifecyclePollSamplingSink{
		next:      next,
		namespace: namespace,
		interval:  lifecyclePollSampleInterval,
		seen:      make(map[time.Time]struct{}),
		now:       time.Now,
	}
}

// Emit applies the sampling policy and forwards accepted events.
func (s *LifecyclePollSamplingSink) Emit(ctx context.Context, ev ports.TelemetryEvent) {
	if ev.Name != "ao.lifecycle.poll" || pollNeedsImmediateVisibility(ev) {
		s.next.Emit(ctx, ev)
		return
	}

	occurredAt := ev.OccurredAt
	if occurredAt.IsZero() {
		occurredAt = s.now()
	}
	bucket := occurredAt.UTC().Truncate(s.interval)
	s.mu.Lock()
	if _, exists := s.seen[bucket]; exists {
		s.mu.Unlock()
		return
	}
	s.seen[bucket] = struct{}{}
	s.mu.Unlock()

	ev.ID = deterministicRemoteID(s.namespace, "ao.lifecycle.poll\x00"+bucket.Format(time.RFC3339Nano))
	s.next.Emit(ctx, ev)
}

// Close closes the wrapped sink.
func (s *LifecyclePollSamplingSink) Close(ctx context.Context) error {
	return s.next.Close(ctx)
}

// pollNeedsImmediateVisibility identifies lifecycle health signals that must
// not be hidden by aggregation or a process-local volume budget. Healthy poll
// completions are high-frequency heartbeat data; failures and overruns are the
// signal operators need when the observer is unhealthy.
func pollNeedsImmediateVisibility(ev ports.TelemetryEvent) bool {
	if ev.Name != "ao.lifecycle.poll" {
		return false
	}
	if ev.Level == ports.TelemetryLevelWarn || ev.Level == ports.TelemetryLevelError {
		return true
	}
	if ev.Payload["outcome"] == "failure" || ev.Payload["health_status"] == "warn" || ev.Payload["health_status"] == "error" {
		return true
	}
	return positiveTelemetryNumber(ev.Payload["overrun_ms"])
}

func positiveTelemetryNumber(value any) bool {
	switch value := value.(type) {
	case int:
		return value > 0
	case int8:
		return value > 0
	case int16:
		return value > 0
	case int32:
		return value > 0
	case int64:
		return value > 0
	case uint:
		return value > 0
	case uint8:
		return value > 0
	case uint16:
		return value > 0
	case uint32:
		return value > 0
	case uint64:
		return value > 0
	case float32:
		return value > 0
	case float64:
		return value > 0
	default:
		return false
	}
}
