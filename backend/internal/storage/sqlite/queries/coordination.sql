-- name: InsertCoordinationClaim :execrows
INSERT INTO coordination_claims (claim_key, owner_pid, claimed_at)
VALUES (?, ?, ?)
ON CONFLICT (claim_key) DO NOTHING;

-- name: GetCoordinationClaim :one
SELECT claim_key, owner_pid, claimed_at
FROM coordination_claims
WHERE claim_key = ?;

-- name: TakeOverCoordinationClaim :execrows
UPDATE coordination_claims
SET owner_pid = ?, claimed_at = ?
WHERE claim_key = ? AND owner_pid = ?;

-- name: ReleaseCoordinationClaim :execrows
DELETE FROM coordination_claims
WHERE claim_key = ? AND owner_pid = ?;
