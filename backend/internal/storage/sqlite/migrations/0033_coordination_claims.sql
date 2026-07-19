-- +goose Up
-- +goose StatementBegin
-- coordination_claims is the daemon control-plane lease. The random owner
-- token is the fencing generation: PID reuse cannot make a successor look like
-- the prior holder. The expiry makes a crash before running.json publication
-- reclaimable without guessing from PID liveness.
--
-- This table intentionally has no change_log triggers: claims coordinate
-- internal side effects and are not user-visible durable facts.
CREATE TABLE coordination_claims (
    claim_key        TEXT PRIMARY KEY,
    owner_token      TEXT NOT NULL CHECK (owner_token <> ''),
    owner_pid        INTEGER NOT NULL CHECK (owner_pid > 0),
    claimed_at       TIMESTAMP NOT NULL,
    lease_expires_at TIMESTAMP NOT NULL
);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE coordination_claims;
-- +goose StatementEnd
