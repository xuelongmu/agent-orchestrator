package store

import (
	"context"
	"fmt"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/gen"
)

// CoordinationClaim is the current durable lease for one internal
// control-plane claim. OwnerToken is the fencing generation; OwnerPID is
// diagnostic only and is never used to authorize mutation.
type CoordinationClaim struct {
	Key            string
	OwnerToken     string
	OwnerPID       int
	ClaimedAt      time.Time
	LeaseExpiresAt time.Time
}

// TryAcquireCoordinationClaim atomically creates key for ownerToken. Exactly
// one competing INSERT can win across Store instances and processes.
// Re-acquiring a claim with the same token is idempotent.
func (s *Store) TryAcquireCoordinationClaim(ctx context.Context, claim CoordinationClaim) (CoordinationClaim, bool, error) {
	if err := validateCoordinationClaim(claim); err != nil {
		return CoordinationClaim{}, false, err
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	var rows int64
	var row gen.CoordinationClaim
	err := s.inTx(ctx, "claim coordination key", func(q *gen.Queries) error {
		var err error
		rows, err = q.InsertCoordinationClaim(ctx, gen.InsertCoordinationClaimParams{
			ClaimKey:       claim.Key,
			OwnerToken:     claim.OwnerToken,
			OwnerPid:       int64(claim.OwnerPID),
			ClaimedAt:      claim.ClaimedAt,
			LeaseExpiresAt: claim.LeaseExpiresAt,
		})
		if err != nil {
			return err
		}
		row, err = q.GetCoordinationClaim(ctx, claim.Key)
		return err
	})
	if err != nil {
		return CoordinationClaim{}, false, fmt.Errorf("claim coordination key %q: %w", claim.Key, err)
	}
	current := coordinationClaimFromGen(row)
	return current, rows == 1 || current.OwnerToken == claim.OwnerToken, nil
}

// TakeOverCoordinationClaim compare-and-swaps an expired key from the exact
// expected fencing token to claim. A concurrent renewal changes the expiry and
// makes the guarded update fail; a concurrent takeover changes the token.
func (s *Store) TakeOverCoordinationClaim(ctx context.Context, expectedOwnerToken string, now time.Time, claim CoordinationClaim) (bool, error) {
	if expectedOwnerToken == "" {
		return false, fmt.Errorf("invalid coordination takeover: expected owner token is empty")
	}
	if err := validateCoordinationClaim(claim); err != nil {
		return false, err
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	rows, err := s.qw.TakeOverCoordinationClaim(ctx, gen.TakeOverCoordinationClaimParams{
		OwnerToken:       claim.OwnerToken,
		OwnerPid:         int64(claim.OwnerPID),
		ClaimedAt:        claim.ClaimedAt,
		LeaseExpiresAt:   claim.LeaseExpiresAt,
		ClaimKey:         claim.Key,
		OwnerToken_2:     expectedOwnerToken,
		LeaseExpiresAt_2: now,
	})
	if err != nil {
		return false, fmt.Errorf("take over coordination claim %q: %w", claim.Key, err)
	}
	return rows == 1, nil
}

// RenewCoordinationClaim extends only the unexpired lease held by ownerToken.
// A stale generation cannot renew a successor, and an expired holder cannot
// resurrect itself after another process becomes eligible to take over.
func (s *Store) RenewCoordinationClaim(ctx context.Context, key, ownerToken string, now, leaseExpiresAt time.Time) (bool, error) {
	if key == "" || ownerToken == "" || !leaseExpiresAt.After(now) {
		return false, fmt.Errorf("invalid coordination renewal key=%q", key)
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	rows, err := s.qw.RenewCoordinationClaim(ctx, gen.RenewCoordinationClaimParams{
		LeaseExpiresAt:   leaseExpiresAt,
		LeaseExpiresAt_2: leaseExpiresAt,
		ClaimKey:         key,
		OwnerToken:       ownerToken,
		LeaseExpiresAt_3: now,
	})
	if err != nil {
		return false, fmt.Errorf("renew coordination claim %q: %w", key, err)
	}
	return rows == 1, nil
}

// ReleaseCoordinationClaim removes key only when ownerToken is still its
// fencing generation. A delayed shutdown cannot release a successor's claim.
func (s *Store) ReleaseCoordinationClaim(ctx context.Context, key, ownerToken string) (bool, error) {
	if key == "" || ownerToken == "" {
		return false, fmt.Errorf("invalid coordination release key=%q", key)
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	rows, err := s.qw.ReleaseCoordinationClaim(ctx, gen.ReleaseCoordinationClaimParams{ClaimKey: key, OwnerToken: ownerToken})
	if err != nil {
		return false, fmt.Errorf("release coordination claim %q: %w", key, err)
	}
	return rows == 1, nil
}

func validateCoordinationClaim(claim CoordinationClaim) error {
	if claim.Key == "" || claim.OwnerToken == "" || claim.OwnerPID <= 0 || claim.ClaimedAt.IsZero() || !claim.LeaseExpiresAt.After(claim.ClaimedAt) {
		return fmt.Errorf("invalid coordination claim key=%q owner_pid=%d", claim.Key, claim.OwnerPID)
	}
	return nil
}

func coordinationClaimFromGen(row gen.CoordinationClaim) CoordinationClaim {
	return CoordinationClaim{
		Key:            row.ClaimKey,
		OwnerToken:     row.OwnerToken,
		OwnerPID:       int(row.OwnerPid),
		ClaimedAt:      row.ClaimedAt,
		LeaseExpiresAt: row.LeaseExpiresAt,
	}
}
