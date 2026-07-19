-- Durable, token-fenced claims for tracker issue intake. A pending claim is a
-- short lease around session spawn; completed claims are the permanent dedup
-- ledger. Claims are scoped by the configured project/provider/repo as well as
-- the provider-native issue id so independently configured intake lanes cannot
-- collide.

-- +goose Up
CREATE TABLE tracker_intake_claims (
    project_id       TEXT NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    provider         TEXT NOT NULL CHECK (provider <> ''),
    repo             TEXT NOT NULL CHECK (repo <> ''),
    issue_id         TEXT NOT NULL CHECK (issue_id <> ''),
    owner_token      TEXT NOT NULL CHECK (owner_token <> ''),
    status           TEXT NOT NULL CHECK (status IN ('pending', 'completed')),
    -- Pending: exact provisional seed owned by this generation (empty before
    -- admission). Completed: the successfully spawned session ledger entry.
    session_id       TEXT NOT NULL DEFAULT '',
    claimed_at       TIMESTAMP NOT NULL,
    lease_expires_at TIMESTAMP NOT NULL,
    completed_at     TIMESTAMP,
    PRIMARY KEY (project_id, provider, repo, issue_id)
);

CREATE INDEX idx_tracker_intake_claims_capacity
    ON tracker_intake_claims (project_id, status, lease_expires_at);

-- Intake claims are internal control-plane state and intentionally do not emit
-- change_log events.

-- +goose Down
DROP TABLE tracker_intake_claims;
