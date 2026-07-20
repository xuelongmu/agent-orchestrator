package daemon

import (
	"log/slog"
	"time"

	telemetryadapter "github.com/aoagents/agent-orchestrator/backend/internal/adapters/telemetry"
	"github.com/aoagents/agent-orchestrator/backend/internal/config"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
)

// aggregatedEventNames are bursty remote-only rollups. Lifecycle polls use a
// separate fixed-window sampling policy so failures and overruns never wait for
// a rollup and every healthy day has a small, predictable sample.
var aggregatedEventNames = []string{
	"ao.http.5xx",
	"ao.daemon.panic",
	"ao.cli.usage_errors",
}

func newTelemetrySink(cfg config.Config, store *sqlite.Store, log *slog.Logger) ports.EventSink {
	if !cfg.Telemetry.Events {
		return telemetryadapter.NoopSink{}
	}
	local := telemetryadapter.NewLocalSQLiteSink(store, log)
	if cfg.Telemetry.Remote != config.TelemetryRemotePostHog {
		return local
	}
	remote, err := telemetryadapter.NewPostHogSink(cfg.DataDir, cfg.Telemetry.PostHogKey, cfg.Telemetry.PostHogHost, nil, log)
	if err != nil {
		log.Warn("telemetry remote sink disabled", "remote", cfg.Telemetry.Remote, "error", err)
		return local
	}
	// Volume policy wraps only the billed remote branch. It never filters the
	// local SQLite branch's raw command, poll, or failure stream.
	rateLimited := telemetryadapter.NewRateLimitedSink(remote, aggregatedEventNames)
	pollsSampled := telemetryadapter.NewLifecyclePollSamplingSink(rateLimited, remote.InstallID())
	aggregated := telemetryadapter.NewAggregatingSink(pollsSampled, aggregatedEventNames, time.Minute)
	deduped := telemetryadapter.NewRemoteDedupeSink(aggregated, remote.InstallID())
	return telemetryadapter.NewFanoutSink(local, deduped)
}
