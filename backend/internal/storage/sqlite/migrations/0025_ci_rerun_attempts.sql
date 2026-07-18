-- Bound automatic flaky-CI retries to one provider mutation per PR head/check.

-- +goose Up
CREATE TABLE ci_rerun_attempt (
    pr_url       TEXT NOT NULL REFERENCES pr(url) ON DELETE CASCADE,
    head_sha     TEXT NOT NULL,
    check_name   TEXT NOT NULL,
    provider_id  TEXT NOT NULL,
    status       TEXT NOT NULL CHECK (status IN ('reserved', 'requested', 'failed')),
    requested_at TIMESTAMP NOT NULL,
    PRIMARY KEY (pr_url, head_sha, check_name)
);

-- +goose Down
DROP TABLE ci_rerun_attempt;
