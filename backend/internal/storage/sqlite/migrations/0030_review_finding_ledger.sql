-- Durable finding taxonomy for the automatic review/fix loop (issue #60).

-- +goose Up
-- +goose StatementBegin
ALTER TABLE review_run ADD COLUMN simplification_class TEXT NOT NULL DEFAULT '';
ALTER TABLE review_run ADD COLUMN simplification_dispatched_at TIMESTAMP;
ALTER TABLE review_run ADD COLUMN deflected_review_cleared_at TIMESTAMP;
-- +goose StatementEnd

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
    thread_reply_id     TEXT NOT NULL DEFAULT '',
    issue_action_token  TEXT NOT NULL DEFAULT '',
    issue_action_lease_until TIMESTAMP,
    thread_action_token TEXT NOT NULL DEFAULT '',
    thread_action_lease_until TIMESTAMP,
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

-- Reuse session_updated so existing SSE clients receive a compatible event;
-- the frontend invalidates the session-reviews query on every CDC refresh.
-- +goose StatementBegin
CREATE TRIGGER review_finding_cdc_insert
AFTER INSERT ON review_finding
BEGIN
    INSERT INTO change_log (project_id, session_id, event_type, payload, created_at)
    VALUES (
        (SELECT project_id FROM sessions WHERE id = NEW.session_id),
        NEW.session_id,
        'session_updated',
        json_object('id', NEW.session_id, 'reviewFinding', NEW.id),
        NEW.created_at);
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER review_finding_cdc_update
AFTER UPDATE ON review_finding
BEGIN
    INSERT INTO change_log (project_id, session_id, event_type, payload, created_at)
    VALUES (
        (SELECT project_id FROM sessions WHERE id = NEW.session_id),
        NEW.session_id,
        'session_updated',
        json_object('id', NEW.session_id, 'reviewFinding', NEW.id),
        datetime('now'));
END;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TRIGGER IF EXISTS review_finding_cdc_update;
DROP TRIGGER IF EXISTS review_finding_cdc_insert;
DROP TABLE review_finding;
ALTER TABLE review_run DROP COLUMN deflected_review_cleared_at;
ALTER TABLE review_run DROP COLUMN simplification_dispatched_at;
ALTER TABLE review_run DROP COLUMN simplification_class;
-- +goose StatementEnd
