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

// AcceptReviewFixCommit is the single review-fix acceptance transaction. When
// the exact PR has no pending actionable findings it normally bypasses
// declaration parsing; requireDeclaration closes the upgrade gap for a legacy
// blocking run that predates structured finding rows. Otherwise it validates
// the exact owner/head and head-bound commit trailer, updates the canonical
// contract if requested, and binds every pending finding to headSHA in one
// transaction.
func (s *Store) AcceptReviewFixCommit(ctx context.Context, sessionID domain.SessionID, prURL, headSHA, commitMessage string, requireDeclaration bool, updatedAt time.Time) (required bool, bound int64, err error) {
	unlockDelivery := designcontract.LockDelivery(prURL)
	defer unlockDelivery()
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	err = s.inTx(ctx, "accept review-fix commit", func(q *gen.Queries) error {
		pr, err := q.GetPR(ctx, prURL)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: PR %s no longer exists", designcontract.ErrPRNotOwned, prURL)
		}
		if err != nil {
			return err
		}
		if pr.SessionID != sessionID {
			return fmt.Errorf("%w: PR %s is owned by session %s", designcontract.ErrPRNotOwned, prURL, pr.SessionID)
		}
		if pr.HeadSha != headSHA {
			return fmt.Errorf("%w: stored head %q does not equal observed head %q", designcontract.ErrReviewFixDeclarationStale, pr.HeadSha, headSHA)
		}
		pending, err := q.CountPendingActionableReviewFindingsByPR(ctx, gen.CountPendingActionableReviewFindingsByPRParams{PRURL: prURL, HeadSha: headSHA})
		if err != nil {
			return err
		}
		if pending == 0 && !requireDeclaration {
			return nil
		}
		required = true
		declaration, err := designcontract.ParseReviewFixInvariantDeclaration(commitMessage)
		if err != nil {
			return err
		}
		if declaration.PR != prURL {
			return fmt.Errorf("%w: declaration PR %q does not equal normalized observed PR %q", designcontract.ErrReviewFixDeclarationStale, declaration.PR, prURL)
		}
		invariant := declaration.Invariant
		switch declaration.Mode {
		case "preserve":
		case "add":
			invariant, err = designcontract.NormalizeInvariant(invariant)
			if err != nil {
				return fmt.Errorf("%w: %w", designcontract.ErrReviewFixDeclarationMalformed, err)
			}
		default:
			return fmt.Errorf("%w: mode must be %q or %q", designcontract.ErrReviewFixDeclarationMalformed, "preserve", "add")
		}
		if err := q.EnsurePRDesignContract(ctx, gen.EnsurePRDesignContractParams{
			PRURL: prURL, SessionID: string(sessionID), FallbackMarkdown: designcontract.BuildSeed("", ""), UpdatedAt: updatedAt,
		}); err != nil {
			return err
		}
		contract, err := q.GetPRDesignContract(ctx, prURL)
		if err != nil {
			return err
		}
		if declaration.Mode == "preserve" {
			if !designcontract.HasExactInvariant(contract, invariant) {
				return fmt.Errorf("%w: %q", designcontract.ErrReviewFixInvariantUnknown, invariant)
			}
		} else if !designcontract.HasInvariant(contract, invariant) {
			addition := designcontract.AppendInvariant("", invariant)
			if len(contract)+len(addition) > designcontract.MaxCanonicalBytes {
				return designcontract.ErrContractCapacityExceeded
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
		}
		bound, err = q.SetPendingReviewFindingFixCommitByPR(ctx, gen.SetPendingReviewFindingFixCommitByPRParams{FixCommit: headSHA, PRURL: prURL, HeadSha: headSHA})
		if err != nil {
			return err
		}
		if bound != pending {
			return fmt.Errorf("bound %d pending review findings, want %d", bound, pending)
		}
		return nil
	})
	if err != nil {
		return false, 0, err
	}
	return required, bound, nil
}

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
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: PR %s no longer exists", designcontract.ErrPRNotOwned, prURL)
		}
		if err != nil {
			return err
		}
		if pr.SessionID != sessionID {
			return fmt.Errorf("%w: PR %s is owned by session %s", designcontract.ErrPRNotOwned, prURL, pr.SessionID)
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
			return designcontract.ErrContractCapacityExceeded
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

// GetOwnedPRDesignContract atomically verifies current PR ownership and reads
// canonical bytes in one SQLite statement. A pre-takeover session cannot read
// after another daemon commits a new owner between service resolution and the
// final store boundary.
func (s *Store) GetOwnedPRDesignContract(ctx context.Context, sessionID domain.SessionID, prURL string) (string, bool, error) {
	row, err := s.qr.GetOwnedPRDesignContract(ctx, gen.GetOwnedPRDesignContractParams{
		PRURL: prURL, SessionID: sessionID,
	})
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, fmt.Errorf("%w: %s", designcontract.ErrPRNotOwned, prURL)
	}
	if err != nil {
		return "", false, fmt.Errorf("get owned PR design contract %s: %w", prURL, err)
	}
	return row.Markdown, row.ContractExists, nil
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
