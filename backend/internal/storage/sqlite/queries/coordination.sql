-- name: InsertCoordinationClaim :execrows
INSERT INTO coordination_claims (claim_key, owner_token, owner_pid, claimed_at, lease_expires_at)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT (claim_key) DO NOTHING;

-- name: GetCoordinationClaim :one
SELECT claim_key, owner_token, owner_pid, claimed_at, lease_expires_at
FROM coordination_claims
WHERE claim_key = ?;

-- name: TakeOverCoordinationClaim :execrows
UPDATE coordination_claims
SET owner_token = ?, owner_pid = ?, claimed_at = ?, lease_expires_at = ?
WHERE claim_key = ?
  AND owner_token = ?
  AND lease_expires_at <= ?;

-- name: RenewCoordinationClaim :execrows
UPDATE coordination_claims
SET lease_expires_at = sqlc.arg(proposed_expiry)
WHERE claim_key = sqlc.arg(claim_key)
  AND owner_token = sqlc.arg(owner_token)
  AND lease_expires_at > sqlc.arg(now)
  AND lease_expires_at < sqlc.arg(proposed_expiry);

-- name: ReleaseCoordinationClaim :execrows
DELETE FROM coordination_claims
WHERE claim_key = ? AND owner_token = ?;
