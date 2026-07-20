package telemetry

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	sqlitestore "github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

type dedupeLocalStore struct {
	mu      sync.Mutex
	records []sqlitestore.TelemetryEventRecord
}

func (s *dedupeLocalStore) CreateTelemetryEvent(_ context.Context, rec sqlitestore.TelemetryEventRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records = append(s.records, rec)
	return nil
}

func (*dedupeLocalStore) PruneTelemetryEventsBefore(context.Context, time.Time, int64) (int64, error) {
	return 0, nil
}

func TestRemoteCLIDedupePreservesLocalHistory(t *testing.T) {
	store := &dedupeLocalStore{}
	local := NewLocalSQLiteSink(store, slog.Default())
	remote := &recordingSink{}
	sink := NewFanoutSink(local, NewRemoteDedupeSink(remote, "ins_test"))
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)

	for i := 0; i < 3; i++ {
		sink.Emit(t.Context(), ports.TelemetryEvent{
			Name:       "ao.cli.invoked",
			Source:     "cli",
			OccurredAt: now,
			Payload:    map[string]any{"command": "status", "command_path": "ao status"},
		})
	}
	if err := sink.Close(t.Context()); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	store.mu.Lock()
	localRecords := append([]sqlitestore.TelemetryEventRecord(nil), store.records...)
	store.mu.Unlock()
	if got := len(localRecords); got != 3 {
		t.Fatalf("local events = %d, want all 3 repeated commands", got)
	}
	if got := len(remote.events); got != 1 {
		t.Fatalf("remote events = %d, want 1 deduplicated command", got)
	}
	if remote.events[0].ID == "" {
		t.Fatal("remote event lacks deterministic provider dedupe ID")
	}

	// A fresh wrapper models a daemon restart. It may attempt the event again,
	// but emits the same provider ID so PostHog can deterministically dedupe it.
	restartedRemote := &recordingSink{}
	restarted := NewRemoteDedupeSink(restartedRemote, "ins_test")
	restarted.Emit(context.Background(), ports.TelemetryEvent{
		Name:       "ao.cli.invoked",
		Source:     "cli",
		OccurredAt: now,
		Payload:    map[string]any{"command": "status", "command_path": "ao status"},
	})
	if got, want := restartedRemote.events[0].ID, remote.events[0].ID; got != want {
		t.Fatalf("ID after restart = %q, want stable %q", got, want)
	}
}

func TestRemoteCLIDedupeSeparatesCommandsAndUTCDates(t *testing.T) {
	remote := &recordingSink{}
	sink := NewRemoteDedupeSink(remote, "ins_test")
	firstDay := time.Date(2026, 7, 20, 23, 59, 0, 0, time.UTC)
	for _, ev := range []ports.TelemetryEvent{
		{Name: "ao.cli.invoked", Source: "cli", OccurredAt: firstDay, Payload: map[string]any{"command_path": "ao status"}},
		{Name: "ao.cli.invoked", Source: "cli", OccurredAt: firstDay, Payload: map[string]any{"command_path": "ao session ls"}},
		{Name: "ao.cli.invoked", Source: "cli", OccurredAt: firstDay.Add(time.Minute), Payload: map[string]any{"command_path": "ao status"}},
	} {
		sink.Emit(t.Context(), ev)
	}
	if got := len(remote.events); got != 3 {
		t.Fatalf("remote events = %d, want different paths/dates retained", got)
	}
}
