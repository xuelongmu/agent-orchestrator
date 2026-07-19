package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/gen"
)

// TelemetryEventRecord is the store-facing representation of a telemetry row.
type TelemetryEventRecord struct {
	ID          string
	OccurredAt  time.Time
	Name        string
	Source      string
	Level       string
	ProjectID   *domain.ProjectID
	SessionID   *domain.SessionID
	RequestID   string
	PayloadJSON string
}

// CreateTelemetryEvent persists one telemetry event row.
func (s *Store) CreateTelemetryEvent(ctx context.Context, rec TelemetryEventRecord) error {
	if err := s.qw.CreateTelemetryEvent(ctx, createTelemetryEventParams(rec)); err != nil {
		return fmt.Errorf("create telemetry event %s: %w", rec.ID, err)
	}
	return nil
}

func createTelemetryEventParams(rec TelemetryEventRecord) gen.CreateTelemetryEventParams {
	arg := gen.CreateTelemetryEventParams{
		ID:          rec.ID,
		OccurredAt:  rec.OccurredAt.UTC(),
		Name:        rec.Name,
		Source:      rec.Source,
		Level:       rec.Level,
		RequestID:   rec.RequestID,
		PayloadJson: rec.PayloadJSON,
	}
	if rec.ProjectID != nil {
		arg.ProjectID = sql.NullString{String: string(*rec.ProjectID), Valid: true}
	}
	if rec.SessionID != nil {
		arg.SessionID = sql.NullString{String: string(*rec.SessionID), Valid: true}
	}
	return arg
}

func telemetryEventRecord(event ports.TelemetryEvent) (TelemetryEventRecord, error) {
	payloadJSON, err := json.Marshal(event.Payload)
	if err != nil {
		return TelemetryEventRecord{}, fmt.Errorf("marshal telemetry event %s payload: %w", event.ID, err)
	}
	return TelemetryEventRecord{
		ID: event.ID, OccurredAt: event.OccurredAt.UTC(), Name: event.Name,
		Source: event.Source, Level: string(event.Level), ProjectID: event.ProjectID,
		SessionID: event.SessionID, RequestID: event.RequestID, PayloadJSON: string(payloadJSON),
	}, nil
}

func telemetryEventFromRow(row gen.TelemetryEvent) (ports.TelemetryEvent, error) {
	var payload map[string]any
	if err := json.Unmarshal([]byte(row.PayloadJson), &payload); err != nil {
		return ports.TelemetryEvent{}, fmt.Errorf("unmarshal telemetry event %s payload: %w", row.ID, err)
	}
	event := ports.TelemetryEvent{
		ID: row.ID, Name: row.Name, Source: row.Source, OccurredAt: row.OccurredAt.UTC(),
		Level: ports.TelemetryLevel(row.Level), RequestID: row.RequestID, Payload: payload,
	}
	if row.ProjectID.Valid {
		projectID := domain.ProjectID(row.ProjectID.String)
		event.ProjectID = &projectID
	}
	if row.SessionID.Valid {
		sessionID := domain.SessionID(row.SessionID.String)
		event.SessionID = &sessionID
	}
	return event, nil
}

// ListTelemetryEventsSince returns telemetry rows oldest-first from a time
// boundary, capped by limit.
func (s *Store) ListTelemetryEventsSince(ctx context.Context, since time.Time, limit int64) ([]gen.TelemetryEvent, error) {
	rows, err := s.qr.ListTelemetryEventsSince(ctx, gen.ListTelemetryEventsSinceParams{
		OccurredAt: since.UTC(),
		Limit:      limit,
	})
	if err != nil {
		return nil, fmt.Errorf("list telemetry events since %s: %w", since.UTC().Format(time.RFC3339), err)
	}
	return rows, nil
}

// PruneTelemetryEventsBefore deletes at most limit rows older than before and
// returns how many rows were removed.
func (s *Store) PruneTelemetryEventsBefore(ctx context.Context, before time.Time, limit int64) (int64, error) {
	n, err := s.qw.PruneTelemetryEventsBefore(ctx, gen.PruneTelemetryEventsBeforeParams{
		OccurredAt: before.UTC(),
		Limit:      limit,
	})
	if err != nil {
		return 0, fmt.Errorf("prune telemetry events before %s: %w", before.UTC().Format(time.RFC3339), err)
	}
	return n, nil
}
