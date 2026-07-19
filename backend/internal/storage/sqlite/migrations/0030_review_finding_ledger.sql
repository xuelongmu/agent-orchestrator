-- Durable finding taxonomy for the automatic review/fix loop (issue #60).

-- +goose Up
-- +goose StatementBegin
CREATE TABLE review_finding (
    id                  TEXT PRIMARY KEY,
    run_id              TEXT NOT NULL REFERENCES review_run (id) ON DELETE CASCADE,
    session_id          TEXT NOT NULL REFERENCES sessions (id) ON DELETE CASCADE,
    pr_url              TEXT NOT NULL,
    round               INTEGER NOT NULL CHECK (round > 0),
    file                TEXT NOT NULL DEFAULT '',
    class_tag           TEXT NOT NULL,
    root_cause_note     TEXT NOT NULL DEFAULT '',
    fix_commit          TEXT NOT NULL DEFAULT '',
    thread_id           TEXT NOT NULL DEFAULT '',
    body                TEXT NOT NULL DEFAULT '',
    out_of_scope        INTEGER NOT NULL DEFAULT 0 CHECK (out_of_scope IN (0, 1)),
    deferred_issue_url  TEXT NOT NULL DEFAULT '',
    thread_resolved     INTEGER NOT NULL DEFAULT 0 CHECK (thread_resolved IN (0, 1)),
    created_at          TIMESTAMP NOT NULL
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX idx_review_finding_session_pr_round
    ON review_finding (session_id, pr_url, round, created_at);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX idx_review_finding_class
    ON review_finding (session_id, pr_url, class_tag);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE review_finding;
-- +goose StatementEnd
