-- +goose Up
-- +goose StatementBegin
-- coordination_claims is the daemon control-plane mutex. The row survives a
-- crash so a successor can compare-and-swap the exact prior owner, while the
-- primary key makes the uncontended claim a single atomic INSERT.
--
-- This table intentionally has no change_log triggers: claims coordinate
-- internal side effects and are not user-visible durable facts.
CREATE TABLE coordination_claims (
    claim_key   TEXT PRIMARY KEY,
    owner_pid   INTEGER NOT NULL CHECK (owner_pid > 0),
    claimed_at  TIMESTAMP NOT NULL
);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE coordination_claims;
-- +goose StatementEnd
