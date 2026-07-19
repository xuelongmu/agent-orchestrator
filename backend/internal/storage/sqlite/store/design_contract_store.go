package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/designcontract"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/gen"
)

// SaveSessionDesignContractSeed durably records tracker-derived seed knowledge
// before a worker is launched. ClaimPR later consumes it inside the ownership
// transaction; deleting a failed spawn cascades the seed row.
func (s *Store) SaveSessionDesignContractSeed(ctx context.Context, sessionID domain.SessionID, markdown string, updatedAt time.Time) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.qw.UpsertSessionDesignContractSeed(ctx, gen.UpsertSessionDesignContractSeedParams{
		SessionID: string(sessionID), Markdown: markdown, UpdatedAt: updatedAt,
	})
}

// AddPRDesignContractInvariant atomically verifies exact PR ownership and
// appends one validated fixer/human-declared invariant.
func (s *Store) AddPRDesignContractInvariant(ctx context.Context, sessionID domain.SessionID, prURL, invariant string, updatedAt time.Time) (string, error) {
	invariant, err := designcontract.NormalizeInvariant(invariant)
	if err != nil {
		return "", err
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	contract := ""
	err = s.inTx(ctx, "add PR design contract invariant", func(q *gen.Queries) error {
		pr, err := q.GetPR(ctx, prURL)
		if err != nil {
			return err
		}
		if pr.SessionID != sessionID {
			return fmt.Errorf("PR %s is owned by session %s", prURL, pr.SessionID)
		}
		if err := q.EnsurePRDesignContract(ctx, gen.EnsurePRDesignContractParams{
			PRURL: prURL, SessionID: string(sessionID), FallbackMarkdown: designcontract.BuildSeed("", ""), UpdatedAt: updatedAt,
		}); err != nil {
			return err
		}
		contract, err = q.GetPRDesignContract(ctx, prURL)
		if err != nil {
			return err
		}
		if strings.Contains(contract, invariant) {
			return nil
		}
		addition := designcontract.AppendInvariant("", invariant)
		if len(contract)+len(addition) > designcontract.MaxCanonicalBytes {
			return errors.New("canonical design contract capacity exceeded")
		}
		n, err := q.AppendPRDesignContractInvariant(ctx, gen.AppendPRDesignContractInvariantParams{
			Addition: addition, UpdatedAt: updatedAt, PRURL: prURL, Invariant: invariant,
		})
		if err != nil {
			return err
		}
		if n != 1 {
			return fmt.Errorf("updated %d contracts, want 1", n)
		}
		contract += addition
		return nil
	})
	return contract, err
}

// GetPRDesignContract returns canonical contract bytes for one normalized PR.
func (s *Store) GetPRDesignContract(ctx context.Context, prURL string) (string, bool, error) {
	markdown, err := s.qr.GetPRDesignContract(ctx, prURL)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("get PR design contract %s: %w", prURL, err)
	}
	return markdown, true, nil
}
