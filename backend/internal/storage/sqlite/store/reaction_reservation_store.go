package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/gen"
)

const (
	reactionPhaseReserved = "reserved"
	reactionPhaseStarted  = "started"
)

// ReservePRReaction claims permission to prepare one exact reaction delivery.
// It does not consume the attempt yet. A live reservation is never replaced;
// only an expired reservation that has not crossed StartPRReaction's durable
// pane-write boundary may be taken over.
func (s *Store) ReservePRReaction(
	ctx context.Context,
	prURL, key, signature string,
	maxAttempts int,
	ownerToken string,
	fences []ports.PRReactionFence,
	now, leaseExpiresAt time.Time,
) (ports.PRReactionReservation, error) {
	fencesJSON, err := encodeReactionFences(prURL, fences)
	if err != nil || prURL == "" || key == "" || signature == "" || ownerToken == "" || maxAttempts < 0 || now.IsZero() || !leaseExpiresAt.After(now) {
		return ports.PRReactionReservation{}, fmt.Errorf("invalid PR reaction reservation url=%q key=%q", prURL, key)
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	var result ports.PRReactionReservation
	err = s.inImmediateTx(ctx, "reserve PR reaction", func(q *gen.Queries) error {
		pr, current, err := fencedAnchorPR(ctx, q, prURL, fences)
		if err != nil {
			return err
		}
		if !current {
			result.Status = ports.PRReactionStale
			return nil
		}

		raw := pr.LastNudgeSignature
		doc := decodeReactionDocument(raw)
		seen := reactionStringMap(doc, "seen")
		attempts := reactionIntMap(doc, "attempts")
		priorSignature, signaturePresent := seen[key]
		priorAttempts, attemptsPresent := attempts[key]

		existing, reservationErr := q.GetPRReactionReservation(ctx, gen.GetPRReactionReservationParams{PRURL: prURL, ReactionKey: key})
		if reservationErr != nil && !errors.Is(reservationErr, sql.ErrNoRows) {
			return reservationErr
		}
		if reservationErr == nil {
			switch {
			case existing.Phase == reactionPhaseStarted:
				// Unknown delivery is fail-closed for every signature/head: replacing
				// it could overlap a slow live send or duplicate a crash-surviving one.
				result = ports.PRReactionReservation{Status: ports.PRReactionUncertain, Signature: existing.Signature, Attempts: int(existing.ReservedAttempts)}
				return nil
			case existing.LeaseExpiresAt.After(now):
				result = ports.PRReactionReservation{Status: ports.PRReactionBusy, Signature: existing.Signature}
				return nil
			case existing.Phase != reactionPhaseReserved:
				return fmt.Errorf("invalid reaction reservation phase %q", existing.Phase)
			}
		}

		if priorSignature == signature {
			result = ports.PRReactionReservation{Status: ports.PRReactionAccounted, Signature: signature, Attempts: priorAttempts}
			return nil
		}
		baseAttempts := priorAttempts
		if signaturePresent && priorSignature != signature {
			baseAttempts = 0
		}
		if maxAttempts > 0 && baseAttempts >= maxAttempts {
			result = ports.PRReactionReservation{Status: ports.PRReactionExhausted, Signature: priorSignature, Attempts: baseAttempts}
			return nil
		}
		nextAttempts := baseAttempts + 1
		params := gen.InsertPRReactionReservationParams{
			PRURL: prURL, ReactionKey: key, OwnerToken: ownerToken, Phase: reactionPhaseReserved,
			Signature: signature, ExpectedFences: fencesJSON,
			PreviousSignaturePresent: signaturePresent, PreviousSignature: priorSignature,
			PreviousAttemptsPresent: attemptsPresent, PreviousAttempts: int64(priorAttempts),
			ReservedAttempts: int64(nextAttempts), ReservedAt: now, LeaseExpiresAt: leaseExpiresAt,
		}
		if errors.Is(reservationErr, sql.ErrNoRows) {
			rows, err := q.InsertPRReactionReservation(ctx, params)
			if err != nil {
				return err
			}
			if rows != 1 {
				return errors.New("reaction reservation changed while writer lock was held")
			}
		} else {
			rows, err := q.ReplacePRReactionReservation(ctx, gen.ReplacePRReactionReservationParams{
				OwnerToken: ownerToken, Phase: reactionPhaseReserved, Signature: signature,
				ExpectedFences:           fencesJSON,
				PreviousSignaturePresent: signaturePresent, PreviousSignature: priorSignature,
				PreviousAttemptsPresent: attemptsPresent, PreviousAttempts: int64(priorAttempts),
				ReservedAttempts: int64(nextAttempts), ReservedAt: now, LeaseExpiresAt: leaseExpiresAt,
				PRURL: prURL, ReactionKey: key, OwnerToken_2: existing.OwnerToken, LeaseExpiresAt_2: now,
			})
			if err != nil {
				return err
			}
			if rows != 1 {
				return errors.New("expired reaction reservation changed while writer lock was held")
			}
		}
		result = ports.PRReactionReservation{Status: ports.PRReactionReserved, Signature: signature, Attempts: nextAttempts}
		return nil
	})
	if err != nil {
		return ports.PRReactionReservation{}, fmt.Errorf("reserve PR reaction %s/%q: %w", prURL, key, err)
	}
	return result, nil
}

// StartPRReaction atomically revalidates PR ownership/exact head, commits the
// dedup signature and attempt, and marks the reservation as having crossed the
// external-send boundary. A crash after this point has an explicit uncertain
// policy: future callers never resend and surface the unknown delivery.
func (s *Store) StartPRReaction(ctx context.Context, prURL, key, ownerToken string, now, leaseExpiresAt time.Time) (ports.PRReactionReservation, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	var result ports.PRReactionReservation
	err := s.inImmediateTx(ctx, "start PR reaction", func(q *gen.Queries) error {
		reservation, err := q.GetPRReactionReservation(ctx, gen.GetPRReactionReservationParams{PRURL: prURL, ReactionKey: key})
		if errors.Is(err, sql.ErrNoRows) || (err == nil && reservation.OwnerToken != ownerToken) {
			result.Status = ports.PRReactionBusy
			return nil
		}
		if err != nil {
			return err
		}
		if reservation.Phase != reactionPhaseReserved || !reservation.LeaseExpiresAt.After(now) {
			result.Status = ports.PRReactionBusy
			return nil
		}
		var fences []ports.PRReactionFence
		if err := json.Unmarshal([]byte(reservation.ExpectedFences), &fences); err != nil {
			return fmt.Errorf("decode reaction fences: %w", err)
		}
		pr, current, err := fencedAnchorPR(ctx, q, prURL, fences)
		if err != nil {
			return err
		}
		if !current {
			_, err := q.DeletePRReactionReservation(ctx, gen.DeletePRReactionReservationParams{PRURL: prURL, ReactionKey: key, OwnerToken: ownerToken})
			result.Status = ports.PRReactionStale
			return err
		}
		doc := decodeReactionDocument(pr.LastNudgeSignature)
		seen := reactionStringMap(doc, "seen")
		attempts := reactionIntMap(doc, "attempts")
		seen[key] = reservation.Signature
		attempts[key] = int(reservation.ReservedAttempts)
		setReactionMap(doc, "seen", seen)
		setReactionMap(doc, "attempts", attempts)
		encoded, err := json.Marshal(doc)
		if err != nil {
			return err
		}
		rows, err := q.StartPRReactionReservation(ctx, gen.StartPRReactionReservationParams{
			ReservedAttempts: reservation.ReservedAttempts, LeaseExpiresAt: leaseExpiresAt,
			PRURL: prURL, ReactionKey: key, OwnerToken: ownerToken, LeaseExpiresAt_2: now,
		})
		if err != nil {
			return err
		}
		if rows != 1 {
			result.Status = ports.PRReactionBusy
			return nil
		}
		if err := q.UpdatePRLastNudgeSignature(ctx, gen.UpdatePRLastNudgeSignatureParams{LastNudgeSignature: string(encoded), URL: prURL}); err != nil {
			return err
		}
		result = ports.PRReactionReservation{Status: ports.PRReactionReserved, Signature: reservation.Signature, Attempts: int(reservation.ReservedAttempts)}
		return nil
	})
	if err != nil {
		return ports.PRReactionReservation{}, fmt.Errorf("start PR reaction %s/%q: %w", prURL, key, err)
	}
	return result, nil
}

// CommitPRReaction finalizes a successful external send by clearing its
// rollback metadata. The dedup state was already durable at StartPRReaction.
func (s *Store) CommitPRReaction(ctx context.Context, prURL, key, ownerToken string) (bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	rows, err := s.qw.DeletePRReactionReservation(ctx, gen.DeletePRReactionReservationParams{PRURL: prURL, ReactionKey: key, OwnerToken: ownerToken})
	if err != nil {
		return false, fmt.Errorf("commit PR reaction %s/%q: %w", prURL, key, err)
	}
	return rows == 1, nil
}

// ReleasePRReaction rolls back the exact owner's started attempt after a guard
// suppression or confirmed send error, or simply frees a not-yet-started claim.
// Newer owner generations and unrelated handoff JSON are never overwritten.
func (s *Store) ReleasePRReaction(ctx context.Context, prURL, key, ownerToken string) (bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	var released bool
	err := s.inImmediateTx(ctx, "release PR reaction", func(q *gen.Queries) error {
		reservation, err := q.GetPRReactionReservation(ctx, gen.GetPRReactionReservationParams{PRURL: prURL, ReactionKey: key})
		if errors.Is(err, sql.ErrNoRows) || (err == nil && reservation.OwnerToken != ownerToken) {
			return nil
		}
		if err != nil {
			return err
		}
		if reservation.Phase == reactionPhaseStarted {
			raw, err := q.GetPRLastNudgeSignature(ctx, prURL)
			if err != nil {
				return err
			}
			doc := decodeReactionDocument(raw)
			seen := reactionStringMap(doc, "seen")
			attempts := reactionIntMap(doc, "attempts")
			if seen[key] == reservation.Signature && attempts[key] == int(reservation.ReservedAttempts) {
				if reservation.PreviousSignaturePresent {
					seen[key] = reservation.PreviousSignature
				} else {
					delete(seen, key)
				}
				if reservation.PreviousAttemptsPresent {
					attempts[key] = int(reservation.PreviousAttempts)
				} else {
					delete(attempts, key)
				}
				setReactionMap(doc, "seen", seen)
				setReactionMap(doc, "attempts", attempts)
				encoded, err := json.Marshal(doc)
				if err != nil {
					return err
				}
				if err := q.UpdatePRLastNudgeSignature(ctx, gen.UpdatePRLastNudgeSignatureParams{LastNudgeSignature: string(encoded), URL: prURL}); err != nil {
					return err
				}
			}
		}
		rows, err := q.DeletePRReactionReservation(ctx, gen.DeletePRReactionReservationParams{PRURL: prURL, ReactionKey: key, OwnerToken: ownerToken})
		released = rows == 1
		return err
	})
	if err != nil {
		return false, fmt.Errorf("release PR reaction %s/%q: %w", prURL, key, err)
	}
	return released, nil
}

func decodeReactionDocument(raw string) map[string]json.RawMessage {
	doc := map[string]json.RawMessage{}
	_ = json.Unmarshal([]byte(raw), &doc)
	if doc == nil {
		doc = map[string]json.RawMessage{}
	}
	return doc
}

func reactionStringMap(doc map[string]json.RawMessage, key string) map[string]string {
	result := map[string]string{}
	_ = json.Unmarshal(doc[key], &result)
	return result
}

func reactionIntMap(doc map[string]json.RawMessage, key string) map[string]int {
	result := map[string]int{}
	_ = json.Unmarshal(doc[key], &result)
	return result
}

func setReactionMap[T any](doc map[string]json.RawMessage, key string, value map[string]T) {
	if len(value) == 0 {
		delete(doc, key)
		return
	}
	doc[key], _ = json.Marshal(value)
}

func encodeReactionFences(anchorPR string, fences []ports.PRReactionFence) (string, error) {
	seen := make(map[string]ports.PRReactionFence, len(fences))
	anchorFound := false
	for _, fence := range fences {
		if fence.PRURL == "" || fence.SessionID == "" || fence.HeadSHA == "" {
			return "", errors.New("reaction fence requires PR URL, session, and exact head")
		}
		if prior, ok := seen[fence.PRURL]; ok && prior != fence {
			return "", fmt.Errorf("conflicting reaction fences for %s", fence.PRURL)
		}
		seen[fence.PRURL] = fence
		anchorFound = anchorFound || fence.PRURL == anchorPR
	}
	if !anchorFound {
		return "", errors.New("reaction fences do not include anchor PR")
	}
	raw, err := json.Marshal(fences)
	return string(raw), err
}

func fencedAnchorPR(ctx context.Context, q *gen.Queries, anchorPR string, fences []ports.PRReactionFence) (gen.PR, bool, error) {
	var anchor gen.PR
	for _, fence := range fences {
		pr, err := q.GetPR(ctx, fence.PRURL)
		if errors.Is(err, sql.ErrNoRows) {
			return gen.PR{}, false, nil
		}
		if err != nil {
			return gen.PR{}, false, err
		}
		if pr.SessionID != fence.SessionID || pr.HeadSha != fence.HeadSHA {
			return gen.PR{}, false, nil
		}
		if fence.PRURL == anchorPR {
			anchor = pr
		}
	}
	if anchor.URL == "" {
		return gen.PR{}, false, nil
	}
	return anchor, true, nil
}
