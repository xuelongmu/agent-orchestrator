-- Atomically reserve lifecycle nudges before their external pane write. The
-- committed dedup payload remains in pr.last_nudge_signature; this table holds
-- only the rollback data needed while a delivery attempt is in flight.

-- +goose Up
CREATE TABLE pr_reaction_reservations (
    pr_url                     TEXT NOT NULL REFERENCES pr (url) ON DELETE CASCADE,
    reaction_key               TEXT NOT NULL,
    owner_token                TEXT NOT NULL CHECK (owner_token <> ''),
    phase                      TEXT NOT NULL CHECK (phase IN ('reserved', 'started')),
    signature                  TEXT NOT NULL,
    expected_fences            TEXT NOT NULL CHECK (json_valid(expected_fences)),
    previous_signature_present BOOLEAN NOT NULL,
    previous_signature         TEXT NOT NULL DEFAULT '',
    previous_attempts_present  BOOLEAN NOT NULL,
    previous_attempts          INTEGER NOT NULL DEFAULT 0 CHECK (previous_attempts >= 0),
    reserved_attempts          INTEGER NOT NULL CHECK (reserved_attempts > 0),
    reserved_at                TIMESTAMP NOT NULL,
    lease_expires_at           TIMESTAMP NOT NULL,
    PRIMARY KEY (pr_url, reaction_key)
);

-- Reservations are internal control-plane state and intentionally do not emit
-- change_log events.

-- +goose Down
DROP TABLE pr_reaction_reservations;
