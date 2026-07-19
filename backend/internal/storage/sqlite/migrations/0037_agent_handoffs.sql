-- +goose Up
-- One immutable, canonical JSON completion handoff per session. The separate
-- table keeps this agent-submitted summary outside lifecycle fact columns.
-- The explicit local ao handoff API call is the sealing boundary; lifecycle
-- and dependency promotion semantics are intentionally deferred.
-- +goose StatementBegin
CREATE TABLE agent_handoffs (
    session_id TEXT PRIMARY KEY REFERENCES sessions (id) ON DELETE CASCADE,
    payload    TEXT NOT NULL CHECK (json_valid(payload)),
    created_at TIMESTAMP NOT NULL
);
-- +goose StatementEnd

-- Handoff creation is a session read-model change. Emit only a small
-- invalidation; detail consumers fetch the bounded payload lazily. Exact
-- replays do not insert and therefore do not emit duplicate events.
-- +goose StatementBegin
CREATE TRIGGER agent_handoffs_cdc_insert
AFTER INSERT ON agent_handoffs
BEGIN
    INSERT INTO change_log (project_id, session_id, event_type, payload, created_at)
    VALUES (
        (SELECT project_id FROM sessions WHERE id = NEW.session_id),
        NEW.session_id,
        'session_updated',
        json_object('id', NEW.session_id, 'handoffChanged', json('true')),
        NEW.created_at
    );
END;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TRIGGER IF EXISTS agent_handoffs_cdc_insert;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE agent_handoffs;
-- +goose StatementEnd
