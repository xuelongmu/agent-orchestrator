package store_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func TestSessionHandoffExactReplayIsIdempotentAndChangedPayloadConflicts(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedProject(t, s, "mer")
	session, err := s.CreateSession(ctx, sampleRecord("mer"))
	if err != nil {
		t.Fatal(err)
	}
	handoff := domain.AgentHandoff{
		ChangedFiles:         []string{"backend/internal/domain/handoff.go"},
		VerificationCommands: []string{"go test ./internal/domain"},
		ResidualRisk:         "Full CI pending.",
	}
	created, err := s.PutSessionHandoff(ctx, session.ID, handoff, time.Now().UTC())
	if err != nil || !created {
		t.Fatalf("first submit: created=%v err=%v", created, err)
	}
	created, err = s.PutSessionHandoff(ctx, session.ID, handoff, time.Now().UTC().Add(time.Minute))
	if err != nil || created {
		t.Fatalf("exact replay: created=%v err=%v", created, err)
	}
	changed := handoff
	changed.ResidualRisk = "Different risk."
	if _, err := s.PutSessionHandoff(ctx, session.ID, changed, time.Now().UTC()); !errors.Is(err, ports.ErrHandoffConflict) {
		t.Fatalf("changed replay error = %v, want ErrHandoffConflict", err)
	}
	got, ok, err := s.GetSessionHandoff(ctx, session.ID)
	if err != nil || !ok || !got.Equal(handoff) {
		t.Fatalf("read: got=%#v ok=%v err=%v", got, ok, err)
	}

	events, err := s.EventsAfter(ctx, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[1].Type != "session_updated" {
		t.Fatalf("events = %#v, want create plus one handoff update", events)
	}
	if len(events[1].Payload) > 128 || strings.Contains(string(events[1].Payload), "handoff.go") || strings.Contains(string(events[1].Payload), "Full CI") {
		t.Fatalf("handoff CDC payload copied completion blob: %s", events[1].Payload)
	}
}

func TestSessionHandoffRejectsUnknownSessionAndOversizePayload(t *testing.T) {
	s := newTestStore(t)
	handoff := domain.AgentHandoff{ChangedFiles: []string{}, VerificationCommands: []string{}}
	if _, err := s.PutSessionHandoff(context.Background(), "missing", handoff, time.Now().UTC()); !errors.Is(err, ports.ErrSessionNotFound) {
		t.Fatalf("missing session error = %v", err)
	}
	handoff.ResidualRisk = string(make([]byte, domain.MaxHandoffResidualRiskBytes+1))
	if _, err := s.PutSessionHandoff(context.Background(), "missing", handoff, time.Now().UTC()); err == nil {
		t.Fatal("oversize handoff succeeded")
	}
}
