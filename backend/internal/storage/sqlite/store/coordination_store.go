package store

import (
	"context"
	"fmt"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/gen"
)

// CoordinationClaim is the current durable owner of one internal
// control-plane claim.
type CoordinationClaim struct {
	Key       string
	OwnerPID  int
	ClaimedAt time.Time
}

// TryAcquireCoordinationClaim atomically creates key for ownerPID. Exactly one
// competing INSERT can win across Store instances and processes. Re-acquiring
// a claim already owned by the same PID is idempotent.
func (s *Store) TryAcquireCoordinationClaim(ctx context.Context, key string, ownerPID int, claimedAt time.Time) (CoordinationClaim, bool, error) {
	if key == "" || ownerPID <= 0 {
		return CoordinationClaim{}, false, fmt.Errorf("invalid coordination claim key=%q owner_pid=%d", key, ownerPID)
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	rows, err := s.qw.InsertCoordinationClaim(ctx, gen.InsertCoordinationClaimParams{
		ClaimKey:  key,
		OwnerPid:  int64(ownerPID),
		ClaimedAt: claimedAt,
	})
	if err != nil {
		return CoordinationClaim{}, false, fmt.Errorf("claim coordination key %q: %w", key, err)
	}
	claim, err := s.qw.GetCoordinationClaim(ctx, key)
	if err != nil {
		return CoordinationClaim{}, false, fmt.Errorf("read coordination claim %q: %w", key, err)
	}
	current := CoordinationClaim{Key: claim.ClaimKey, OwnerPID: int(claim.OwnerPid), ClaimedAt: claim.ClaimedAt}
	return current, rows == 1 || current.OwnerPID == ownerPID, nil
}

// TakeOverCoordinationClaim compare-and-swaps key from expectedOwnerPID to
// ownerPID. Callers decide whether the observed owner is stale; the guarded
// UPDATE ensures only one successor can replace that exact owner.
func (s *Store) TakeOverCoordinationClaim(ctx context.Context, key string, expectedOwnerPID, ownerPID int, claimedAt time.Time) (bool, error) {
	if key == "" || expectedOwnerPID <= 0 || ownerPID <= 0 {
		return false, fmt.Errorf("invalid coordination takeover key=%q expected_owner_pid=%d owner_pid=%d", key, expectedOwnerPID, ownerPID)
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	rows, err := s.qw.TakeOverCoordinationClaim(ctx, gen.TakeOverCoordinationClaimParams{
		OwnerPid:   int64(ownerPID),
		ClaimedAt:  claimedAt,
		ClaimKey:   key,
		OwnerPid_2: int64(expectedOwnerPID),
	})
	if err != nil {
		return false, fmt.Errorf("take over coordination claim %q: %w", key, err)
	}
	return rows == 1, nil
}

// ReleaseCoordinationClaim removes key only when ownerPID is still its owner.
// A delayed shutdown therefore cannot release a successor's claim.
func (s *Store) ReleaseCoordinationClaim(ctx context.Context, key string, ownerPID int) (bool, error) {
	if key == "" || ownerPID <= 0 {
		return false, fmt.Errorf("invalid coordination release key=%q owner_pid=%d", key, ownerPID)
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	rows, err := s.qw.ReleaseCoordinationClaim(ctx, gen.ReleaseCoordinationClaimParams{ClaimKey: key, OwnerPid: int64(ownerPID)})
	if err != nil {
		return false, fmt.Errorf("release coordination claim %q: %w", key, err)
	}
	return rows == 1, nil
}
