package telemetry

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// RemoteDedupeSink keeps repeated CLI activity out of the remote sink without
// changing the local SQLite history. Its in-memory set avoids duplicate sends
// during one daemon run; deterministic event IDs let PostHog deduplicate the
// same daily key if the daemon restarts.
type RemoteDedupeSink struct {
	next      ports.EventSink
	namespace string

	mu   sync.Mutex
	seen map[string]struct{}
}

// NewRemoteDedupeSink wraps the billed remote branch only. namespace must be
// stable and installation-specific so deterministic provider IDs cannot
// collide across AO installs.
func NewRemoteDedupeSink(next ports.EventSink, namespace string) *RemoteDedupeSink {
	return &RemoteDedupeSink{next: next, namespace: namespace, seen: make(map[string]struct{})}
}

// Emit forwards non-CLI events unchanged and reserves one remote CLI event per
// UTC day (per command path for ao.cli.invoked).
func (s *RemoteDedupeSink) Emit(ctx context.Context, ev ports.TelemetryEvent) {
	key, ok := remoteDailyDedupeKey(ev)
	if !ok {
		s.next.Emit(ctx, ev)
		return
	}

	s.mu.Lock()
	if _, exists := s.seen[key]; exists {
		s.mu.Unlock()
		return
	}
	s.seen[key] = struct{}{}
	s.mu.Unlock()

	ev.ID = deterministicRemoteID(s.namespace, key)
	s.next.Emit(ctx, ev)
}

func deterministicRemoteID(namespace, key string) string {
	sum := sha256.Sum256([]byte("ao-remote-dedupe\x00" + namespace + "\x00" + key))
	return "ded_" + hex.EncodeToString(sum[:])
}

func remoteDailyDedupeKey(ev ports.TelemetryEvent) (string, bool) {
	day := ev.OccurredAt.UTC().Format(time.DateOnly)
	if ev.OccurredAt.IsZero() {
		day = time.Now().UTC().Format(time.DateOnly)
	}
	switch ev.Name {
	case "ao.cli.invoked":
		commandPath, ok := ev.Payload["command_path"].(string)
		if !ok || commandPath == "" {
			return "", false
		}
		return day + "\x00ao.cli.invoked\x00" + commandPath, true
	case "ao.app.active":
		if ev.Source != "cli" && ev.Payload["channel"] != "cli" {
			return "", false
		}
		return day + "\x00ao.app.active\x00cli", true
	default:
		return "", false
	}
}

// Close closes the wrapped sink.
func (s *RemoteDedupeSink) Close(ctx context.Context) error { return s.next.Close(ctx) }
