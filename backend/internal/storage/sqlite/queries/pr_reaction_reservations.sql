-- name: GetPRReactionReservation :one
SELECT pr_url, reaction_key, owner_token, phase, signature,
       expected_fences,
       previous_signature_present, previous_signature,
       previous_attempts_present, previous_attempts,
       reserved_attempts, reserved_at, lease_expires_at
FROM pr_reaction_reservations
WHERE pr_url = ? AND reaction_key = ?;

-- name: InsertPRReactionReservation :execrows
INSERT INTO pr_reaction_reservations (
    pr_url, reaction_key, owner_token, phase, signature,
    expected_fences,
    previous_signature_present, previous_signature,
    previous_attempts_present, previous_attempts,
    reserved_attempts, reserved_at, lease_expires_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (pr_url, reaction_key) DO NOTHING;

-- name: ReplacePRReactionReservation :execrows
UPDATE pr_reaction_reservations
SET owner_token = ?, phase = ?, signature = ?,
    expected_fences = ?,
    previous_signature_present = ?, previous_signature = ?,
    previous_attempts_present = ?, previous_attempts = ?,
    reserved_attempts = ?, reserved_at = ?, lease_expires_at = ?
WHERE pr_url = ? AND reaction_key = ? AND owner_token = ?
  AND phase = 'reserved' AND lease_expires_at <= ?;

-- name: StartPRReactionReservation :execrows
UPDATE pr_reaction_reservations
SET phase = 'started', reserved_attempts = ?, lease_expires_at = ?
WHERE pr_url = ? AND reaction_key = ? AND owner_token = ?
  AND phase = 'reserved' AND lease_expires_at > ?;

-- name: DeletePRReactionReservation :execrows
DELETE FROM pr_reaction_reservations
WHERE pr_url = ? AND reaction_key = ? AND owner_token = ?;
