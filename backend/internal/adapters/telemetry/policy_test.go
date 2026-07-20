package telemetry

import (
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func TestLifecyclePollSamplingPolicyAcrossFullDayPreservesFailuresAndOverruns(t *testing.T) {
	rec := &recordingSink{}
	limiter := NewRateLimitedSink(rec, nil)
	current := time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC)
	limiter.now = func() time.Time { return current }
	sampler := NewLifecyclePollSamplingSink(limiter, "ins_test")

	const pollsPerDay = 24 * 60 * 2 // the production observer's 30-second cadence
	criticalWant := 0
	for poll := 0; poll < pollsPerDay; poll++ {
		sampler.Emit(t.Context(), healthyPoll(current))
		if poll%720 == 0 {
			criticalWant += 2
			sampler.Emit(t.Context(), ports.TelemetryEvent{
				Name: "ao.lifecycle.poll", Source: "scm_observer", OccurredAt: current,
				Level:   ports.TelemetryLevelError,
				Payload: map[string]any{"outcome": "failure", "health_status": "error", "reason": "github_authentication"},
			})
			sampler.Emit(t.Context(), ports.TelemetryEvent{
				Name: "ao.lifecycle.poll", Source: "scm_observer", OccurredAt: current,
				Level:   ports.TelemetryLevelWarn,
				Payload: map[string]any{"outcome": "success", "health_status": "warn", "overrun_ms": int64(5000)},
			})
		}
		current = current.Add(30 * time.Second)
	}

	healthyWant := int((24 * time.Hour) / lifecyclePollSampleInterval)
	healthyIDs := make(map[string]struct{}, healthyWant)
	var healthy, critical int
	for _, ev := range rec.events {
		if pollNeedsImmediateVisibility(ev) {
			critical++
			if ev.ID != "" {
				t.Fatalf("critical poll received sampling dedupe ID %q", ev.ID)
			}
			continue
		}
		healthy++
		if ev.ID == "" {
			t.Fatal("healthy poll sample lacks deterministic remote ID")
		}
		healthyIDs[ev.ID] = struct{}{}
	}
	if healthy != healthyWant || len(healthyIDs) != healthyWant {
		t.Fatalf("healthy poll samples/unique IDs = %d/%d, want %d/%d", healthy, len(healthyIDs), healthyWant, healthyWant)
	}
	if critical != criticalWant {
		t.Fatalf("visible failures/overruns = %d, want %d", critical, criticalWant)
	}
}

func TestLifecyclePollSamplingIsRestartIdempotentWithinUTCBucket(t *testing.T) {
	at := time.Date(2026, 7, 20, 12, 7, 0, 0, time.UTC)
	firstRemote := &recordingSink{}
	NewLifecyclePollSamplingSink(firstRemote, "ins_test").Emit(t.Context(), healthyPoll(at))
	restartedRemote := &recordingSink{}
	NewLifecyclePollSamplingSink(restartedRemote, "ins_test").Emit(t.Context(), healthyPoll(at.Add(time.Minute)))

	if len(firstRemote.events) != 1 || len(restartedRemote.events) != 1 {
		t.Fatalf("remote samples before/after restart = %d/%d, want 1/1", len(firstRemote.events), len(restartedRemote.events))
	}
	if got, want := restartedRemote.events[0].ID, firstRemote.events[0].ID; got != want {
		t.Fatalf("sample ID after restart = %q, want stable %q", got, want)
	}
}

func TestLifecyclePollSamplingRecognizesNumericOverruns(t *testing.T) {
	for name, overrun := range map[string]any{
		"int": int(1), "int32": int32(1), "int64": int64(1),
		"uint": uint(1), "uint32": uint32(1), "uint64": uint64(1),
		"float32": float32(1), "float64": float64(1),
	} {
		t.Run(name, func(t *testing.T) {
			ev := healthyPoll(time.Now())
			ev.Payload["overrun_ms"] = overrun
			if !pollNeedsImmediateVisibility(ev) {
				t.Fatalf("overrun type %T was not treated as immediately visible", overrun)
			}
		})
	}
}

func healthyPoll(at time.Time) ports.TelemetryEvent {
	return ports.TelemetryEvent{
		Name: "ao.lifecycle.poll", Source: "scm_observer", OccurredAt: at,
		Level:   ports.TelemetryLevelInfo,
		Payload: map[string]any{"outcome": "success", "health_status": "ok"},
	}
}
