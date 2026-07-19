package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	sqlitestore "github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

func TestTelemetryStore_CreateListAndPrune(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	projectID := domain.ProjectID("mer")
	sessionID := domain.SessionID("mer-1")
	seedProject(t, s, string(projectID))

	oldAt := time.Now().UTC().Add(-31 * 24 * time.Hour).Truncate(time.Second)
	newAt := time.Now().UTC().Truncate(time.Second)

	if err := s.CreateTelemetryEvent(ctx, telemetryRecord("tev_old", oldAt, &projectID, &sessionID)); err != nil {
		t.Fatalf("CreateTelemetryEvent old: %v", err)
	}
	if err := s.CreateTelemetryEvent(ctx, telemetryRecord("tev_new", newAt, &projectID, &sessionID)); err != nil {
		t.Fatalf("CreateTelemetryEvent new: %v", err)
	}
	duplicate := telemetryRecord("tev_new", newAt.Add(time.Minute), &projectID, &sessionID)
	duplicate.Name = "should-not-replace-stable-event"
	if err := s.CreateTelemetryEvent(ctx, duplicate); err != nil {
		t.Fatalf("CreateTelemetryEvent duplicate stable id: %v", err)
	}

	rows, err := s.ListTelemetryEventsSince(ctx, oldAt.Add(-time.Second), 10)
	if err != nil {
		t.Fatalf("ListTelemetryEventsSince: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	if rows[0].ID != "tev_old" || rows[1].ID != "tev_new" {
		t.Fatalf("ids = %q, %q", rows[0].ID, rows[1].ID)
	}

	n, err := s.PruneTelemetryEventsBefore(ctx, newAt.Add(-24*time.Hour), 100)
	if err != nil {
		t.Fatalf("PruneTelemetryEventsBefore: %v", err)
	}
	if n != 1 {
		t.Fatalf("pruned = %d, want 1", n)
	}

	rows, err = s.ListTelemetryEventsSince(ctx, oldAt.Add(-time.Second), 10)
	if err != nil {
		t.Fatalf("ListTelemetryEventsSince after prune: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != "tev_new" {
		t.Fatalf("remaining rows = %+v", rows)
	}
}

func TestTelemetryPrunePreservesUndeliveredSimplificationIntentAcrossRestart(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Unix(2_000_000_000, 0).UTC()
	eventAt := now.Add(-31 * 24 * time.Hour)
	cutoff := now.Add(-30 * 24 * time.Hour)

	seedProject(t, s, "mer")
	session, err := s.CreateSession(ctx, sampleRecord("mer"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertReview(ctx, domain.Review{
		ID: "review-1", SessionID: session.ID, ProjectID: session.ProjectID,
		Harness: domain.ReviewerCodex, CreatedAt: eventAt, UpdatedAt: eventAt,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertReviewRun(ctx, domain.ReviewRun{
		ID: "run-1", ReviewID: "review-1", SessionID: session.ID,
		Harness: domain.ReviewerCodex, PRURL: "pr1", TargetSHA: "sha1",
		Status: domain.ReviewRunRunning, CreatedAt: eventAt,
	}); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.CompleteReviewRunWithFindings(ctx, "run-1", domain.VerdictChangesRequested, "fix", "", "missing-notify", nil); err != nil || !ok {
		t.Fatalf("complete review = %v, %v", ok, err)
	}
	projectID, sessionID := session.ProjectID, session.ID
	event := ports.TelemetryEvent{
		ID: "tev_review_simplification_retention", Name: "review_simplification_round", Source: "lifecycle",
		OccurredAt: eventAt, Level: ports.TelemetryLevelInfo, ProjectID: &projectID, SessionID: &sessionID,
		Payload: map[string]any{"class_tag": "missing-notify"},
	}
	if _, created, err := s.EnsureReviewRunSimplificationEvent(ctx, "run-1", "sha1", event); err != nil || !created {
		t.Fatalf("ensure simplification intent = %v, %v", created, err)
	}

	if pruned, err := s.PruneTelemetryEventsBefore(ctx, cutoff, 100); err != nil || pruned != 0 {
		t.Fatalf("prune undelivered intent = %d, %v, want 0", pruned, err)
	}
	rows, err := s.ListTelemetryEventsSince(ctx, eventAt.Add(-time.Second), 10)
	if err != nil || len(rows) != 1 || rows[0].ID != event.ID {
		t.Fatalf("protected telemetry rows = %+v, %v", rows, err)
	}

	// A restarted delivery rebuilds a candidate with a new clock value, but
	// drains the original durable intent instead of failing or replacing it.
	replay, created, err := s.EnsureReviewRunSimplificationEvent(ctx, "run-1", "sha1", ports.TelemetryEvent{
		ID: event.ID, OccurredAt: now,
	})
	if err != nil || created || !replay.OccurredAt.Equal(eventAt) {
		t.Fatalf("restart replay = %+v, created=%v, err=%v", replay, created, err)
	}

	if ok, err := s.MarkReviewRunDelivered(ctx, "run-1", now); err != nil || !ok {
		t.Fatalf("mark delivered = %v, %v", ok, err)
	}
	if pruned, err := s.PruneTelemetryEventsBefore(ctx, cutoff, 100); err != nil || pruned != 1 {
		t.Fatalf("prune delivered intent = %d, %v, want 1", pruned, err)
	}
	rows, err = s.ListTelemetryEventsSince(ctx, eventAt.Add(-time.Second), 10)
	if err != nil || len(rows) != 0 {
		t.Fatalf("telemetry rows after delivered retention = %+v, %v", rows, err)
	}
}

func telemetryRecord(id string, at time.Time, projectID *domain.ProjectID, sessionID *domain.SessionID) sqlitestore.TelemetryEventRecord {
	return sqlitestore.TelemetryEventRecord{
		ID:          id,
		OccurredAt:  at,
		Name:        "ao.daemon.started",
		Source:      "daemon",
		Level:       "info",
		ProjectID:   projectID,
		SessionID:   sessionID,
		RequestID:   "req_123",
		PayloadJSON: `{"port":3001}`,
	}
}
