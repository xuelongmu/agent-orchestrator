package daemon

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"testing"
	"time"

	telemetryadapter "github.com/aoagents/agent-orchestrator/backend/internal/adapters/telemetry"
	"github.com/aoagents/agent-orchestrator/backend/internal/config"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
)

func TestNewTelemetrySink_DefaultsToNoopWhenDisabled(t *testing.T) {
	sink := newTelemetrySink(config.Config{}, nil, slog.Default())
	if _, ok := sink.(telemetryadapter.NoopSink); !ok {
		t.Fatalf("sink type = %T, want telemetry.NoopSink", sink)
	}
}

type wiringTestRoundTripper func(*http.Request) (*http.Response, error)

func (f wiringTestRoundTripper) Do(req *http.Request) (*http.Response, error) { return f(req) }

func TestTelemetryWiringPreservesAggregateFieldsOnTheWire(t *testing.T) {
	requests := make(chan map[string]any, 1)
	remote, err := telemetryadapter.NewPostHogSink(t.TempDir(), "phc_test", "https://us.i.posthog.com",
		wiringTestRoundTripper(func(req *http.Request) (*http.Response, error) {
			defer req.Body.Close()
			var body map[string]any
			if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
				return nil, err
			}
			requests <- body
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: http.NoBody}, nil
		}), slog.Default())
	if err != nil {
		t.Fatalf("NewPostHogSink: %v", err)
	}

	rateLimited := telemetryadapter.NewRateLimitedSink(remote, aggregatedEventNames)
	pollsSampled := telemetryadapter.NewLifecyclePollSamplingSink(rateLimited, remote.InstallID())
	aggregated := telemetryadapter.NewAggregatingSink(pollsSampled, aggregatedEventNames, time.Hour)
	for i := 0; i < 3; i++ {
		aggregated.Emit(context.Background(), ports.TelemetryEvent{
			Name: "ao.cli.usage_errors", Source: "cli", Level: ports.TelemetryLevelWarn,
			Payload: map[string]any{"component": "cli", "operation": "command_parse", "error_kind": "usage"},
		})
	}
	if err := aggregated.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}

	select {
	case req := <-requests:
		props := req["properties"].(map[string]any)
		if props["count"] != float64(3) || props["window_start"] == nil || props["window_end"] == nil {
			t.Fatalf("aggregate properties = %#v, want count and window bounds", props)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("PostHog sink did not send aggregate")
	}
}

func TestNewTelemetrySink_MetricsOnlyDoesNotEnableEvents(t *testing.T) {
	sink := newTelemetrySink(config.Config{Telemetry: config.TelemetryConfig{Metrics: true}}, nil, slog.Default())
	if _, ok := sink.(telemetryadapter.NoopSink); !ok {
		t.Fatalf("sink type = %T, want telemetry.NoopSink when only metrics are enabled", sink)
	}
}

func TestNewTelemetrySink_UsesLocalSQLiteWhenEnabled(t *testing.T) {
	dataDir := t.TempDir()
	store, err := sqlite.Open(dataDir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	sink := newTelemetrySink(config.Config{Telemetry: config.TelemetryConfig{Events: true}, DataDir: dataDir}, store, slog.Default())
	local, ok := sink.(*telemetryadapter.LocalSQLiteSink)
	if !ok {
		t.Fatalf("sink type = %T, want *telemetry.LocalSQLiteSink", sink)
	}
	t.Cleanup(func() { _ = local.Close(t.Context()) })
}

func TestNewTelemetrySink_FanoutIncludesPostHogWhenConfigured(t *testing.T) {
	dataDir := t.TempDir()
	store, err := sqlite.Open(dataDir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	sink := newTelemetrySink(config.Config{
		DataDir: dataDir,
		Telemetry: config.TelemetryConfig{
			Events:      true,
			Remote:      config.TelemetryRemotePostHog,
			PostHogKey:  "phc_test",
			PostHogHost: "https://us.i.posthog.com",
		},
	}, store, slog.Default())
	fanout, ok := sink.(*telemetryadapter.FanoutSink)
	if !ok {
		t.Fatalf("sink type = %T, want *telemetry.FanoutSink", sink)
	}
	t.Cleanup(func() { _ = fanout.Close(t.Context()) })
}
