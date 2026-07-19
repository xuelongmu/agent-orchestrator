package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
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
	unlockDelivery := designcontract.LockDelivery(prURL)
	defer unlockDelivery()
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
		if designcontract.HasInvariant(contract, invariant) {
			return nil
		}
		addition := designcontract.AppendInvariant("", invariant)
		if len(contract)+len(addition) > designcontract.MaxCanonicalBytes {
			return errors.New("canonical design contract capacity exceeded")
		}
		n, err := q.AppendPRDesignContractInvariant(ctx, gen.AppendPRDesignContractInvariantParams{
			Addition: addition, UpdatedAt: updatedAt, PRURL: prURL,
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

// GetPendingPRDesignContractDelivery returns the canonical bytes only when the
// exact PR currently has a durable claim barrier for sessionID.
func (s *Store) GetPendingPRDesignContractDelivery(ctx context.Context, sessionID domain.SessionID, prURL string) (designcontract.PendingDelivery, bool, error) {
	row, err := s.qr.GetPendingPRDesignContractDelivery(ctx, gen.GetPendingPRDesignContractDeliveryParams{
		PRURL: prURL, SessionID: sql.NullString{String: string(sessionID), Valid: true},
	})
	if errors.Is(err, sql.ErrNoRows) {
		return designcontract.PendingDelivery{}, false, nil
	}
	if err != nil {
		return designcontract.PendingDelivery{}, false, fmt.Errorf("get pending PR design contract delivery %s: %w", prURL, err)
	}
	return designcontract.PendingDelivery{Contract: row.Markdown, TaskPrompt: row.TaskPrompt, Token: row.DeliveryToken, Revision: row.ContractRevision}, true, nil
}

// CompletePRDesignContractDelivery clears the claim barrier with an exact
// PR/session compare-and-set, so a stale sender cannot acknowledge a newer
// takeover's obligation.
func (s *Store) CompletePRDesignContractDelivery(ctx context.Context, sessionID domain.SessionID, prURL, deliveryToken string, contractRevision int64) (bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	n, err := s.qw.CompletePRDesignContractDelivery(ctx, gen.CompletePRDesignContractDeliveryParams{
		PRURL: prURL, SessionID: sql.NullString{String: string(sessionID), Valid: true}, DeliveryToken: deliveryToken, ContractRevision: contractRevision,
	})
	if err != nil {
		return false, fmt.Errorf("complete PR design contract delivery %s: %w", prURL, err)
	}
	return n == 1, nil
}
