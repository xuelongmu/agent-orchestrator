package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/gen"
)

// GetCIRerunAttempt returns the durable automation claim for one PR head/check.
func (s *Store) GetCIRerunAttempt(ctx context.Context, prURL, headSHA, checkName string) (ports.SCMCIRerunAttempt, bool, error) {
	row, err := s.qr.GetCIRerunAttempt(ctx, gen.GetCIRerunAttemptParams{PRURL: prURL, HeadSha: headSHA, CheckName: checkName})
	if errors.Is(err, sql.ErrNoRows) {
		return ports.SCMCIRerunAttempt{}, false, nil
	}
	if err != nil {
		return ports.SCMCIRerunAttempt{}, false, fmt.Errorf("get CI rerun attempt: %w", err)
	}
	attempt := ports.SCMCIRerunAttempt{
		PRURL: row.PRURL, HeadSHA: row.HeadSha, CheckName: row.CheckName,
		ProviderID: row.ProviderID, Status: row.Status, RequestedAt: row.RequestedAt,
	}
	return attempt, true, nil
}

// ReserveCIRerunAttempt atomically claims one PR head/check. A false result
// means another poll or daemon process already owns the bounded attempt.
func (s *Store) ReserveCIRerunAttempt(ctx context.Context, attempt ports.SCMCIRerunAttempt) (bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	rows, err := s.qw.ReserveCIRerunAttempt(ctx, gen.ReserveCIRerunAttemptParams{
		PRURL: attempt.PRURL, HeadSha: attempt.HeadSHA, CheckName: attempt.CheckName,
		ProviderID: attempt.ProviderID, Status: attempt.Status, RequestedAt: attempt.RequestedAt,
	})
	if err != nil {
		return false, fmt.Errorf("reserve CI rerun attempt: %w", err)
	}
	return rows == 1, nil
}

// UpdateCIRerunAttempt records whether the provider accepted or rejected a
// previously-reserved mutation.
func (s *Store) UpdateCIRerunAttempt(ctx context.Context, attempt ports.SCMCIRerunAttempt) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	rows, err := s.qw.UpdateCIRerunAttempt(ctx, gen.UpdateCIRerunAttemptParams{
		ProviderID: attempt.ProviderID, Status: attempt.Status, RequestedAt: attempt.RequestedAt,
		PRURL: attempt.PRURL, HeadSha: attempt.HeadSHA, CheckName: attempt.CheckName,
	})
	if err != nil {
		return fmt.Errorf("update CI rerun attempt: %w", err)
	}
	if rows != 1 {
		return fmt.Errorf("update CI rerun attempt: reservation not found")
	}
	return nil
}
