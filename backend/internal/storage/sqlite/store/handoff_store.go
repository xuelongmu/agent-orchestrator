package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/gen"
)

// PutSessionHandoff atomically creates a session's immutable structured
// completion summary. The explicit local ao handoff call is the sealing
// boundary. The exact typed payload may be replayed; any changed payload
// conflicts. It intentionally writes no lifecycle or activity facts.
func (s *Store) PutSessionHandoff(ctx context.Context, id domain.SessionID, handoff domain.AgentHandoff, createdAt time.Time) (bool, error) {
	payload, err := domain.EncodeAgentHandoff(handoff)
	if err != nil {
		return false, fmt.Errorf("validate session handoff: %w", err)
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	var created bool
	err = s.inTx(ctx, "put session handoff", func(q *gen.Queries) error {
		if _, err := q.GetSession(ctx, id); errors.Is(err, sql.ErrNoRows) {
			return ports.ErrSessionNotFound
		} else if err != nil {
			return fmt.Errorf("get session %s: %w", id, err)
		}
		rows, err := q.InsertSessionHandoff(ctx, gen.InsertSessionHandoffParams{SessionID: id, Payload: payload, CreatedAt: createdAt})
		if err != nil {
			return fmt.Errorf("insert handoff for %s: %w", id, err)
		}
		if rows > 0 {
			created = true
			return nil
		}
		existing, err := q.GetSessionHandoffPayload(ctx, id)
		if err != nil {
			return fmt.Errorf("read existing handoff for %s: %w", id, err)
		}
		existingHandoff, err := domain.DecodeAgentHandoff(existing)
		if err != nil {
			return fmt.Errorf("decode stored handoff for %s: %w", id, err)
		}
		if !existingHandoff.Equal(handoff) {
			return ports.ErrHandoffConflict
		}
		return nil
	})
	if err != nil {
		return false, err
	}
	return created, nil
}

// GetSessionHandoff reads and validates a session's durable handoff. Invalid
// persisted JSON or UTF-8 is surfaced as an error rather than normalized.
func (s *Store) GetSessionHandoff(ctx context.Context, id domain.SessionID) (domain.AgentHandoff, bool, error) {
	payload, err := s.qr.GetSessionHandoffPayload(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.AgentHandoff{}, false, nil
	}
	if err != nil {
		return domain.AgentHandoff{}, false, fmt.Errorf("get handoff for %s: %w", id, err)
	}
	handoff, err := domain.DecodeAgentHandoff(payload)
	if err != nil {
		return domain.AgentHandoff{}, false, fmt.Errorf("decode handoff for %s: %w", id, err)
	}
	return handoff, true, nil
}
