-- +goose Up
-- Durable, privacy-scrubbed evidence captured at abnormal lifecycle boundaries.
-- Separate columns keep the API model structured without persisting raw hook
-- payloads or introducing an unvalidated JSON blob.
-- +goose StatementBegin
ALTER TABLE sessions ADD COLUMN diagnostic_trigger TEXT NOT NULL DEFAULT '';
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE sessions ADD COLUMN diagnostic_terminal_tail TEXT NOT NULL DEFAULT '';
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE sessions ADD COLUMN diagnostic_hook_error_type TEXT NOT NULL DEFAULT '';
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE sessions ADD COLUMN diagnostic_captured_at TIMESTAMP;
-- +goose StatementEnd

-- Diagnostic-only updates must wake session subscribers just like activity,
-- termination, first-signal, and preview changes do.
-- +goose StatementBegin
DROP TRIGGER IF EXISTS sessions_cdc_update;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER sessions_cdc_update
AFTER UPDATE ON sessions
WHEN OLD.activity_state <> NEW.activity_state
    OR OLD.is_terminated <> NEW.is_terminated
    OR (OLD.first_signal_at IS NULL AND NEW.first_signal_at IS NOT NULL)
    OR OLD.preview_url <> NEW.preview_url
    OR OLD.preview_revision <> NEW.preview_revision
    OR OLD.diagnostic_trigger <> NEW.diagnostic_trigger
    OR OLD.diagnostic_terminal_tail <> NEW.diagnostic_terminal_tail
    OR OLD.diagnostic_hook_error_type <> NEW.diagnostic_hook_error_type
    OR OLD.diagnostic_captured_at IS NOT NEW.diagnostic_captured_at
BEGIN
    INSERT INTO change_log (project_id, session_id, event_type, payload, created_at)
    VALUES (NEW.project_id, NEW.id, 'session_updated',
        json_object('id', NEW.id, 'activity', NEW.activity_state, 'isTerminated', json(CASE WHEN NEW.is_terminated THEN 'true' ELSE 'false' END), 'previewUrl', NEW.preview_url, 'previewRevision', NEW.preview_revision, 'diagnosticTrigger', NEW.diagnostic_trigger),
        NEW.updated_at);
END;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TRIGGER IF EXISTS sessions_cdc_update;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER sessions_cdc_update
AFTER UPDATE ON sessions
WHEN OLD.activity_state <> NEW.activity_state
    OR OLD.is_terminated <> NEW.is_terminated
    OR (OLD.first_signal_at IS NULL AND NEW.first_signal_at IS NOT NULL)
    OR OLD.preview_url <> NEW.preview_url
    OR OLD.preview_revision <> NEW.preview_revision
BEGIN
    INSERT INTO change_log (project_id, session_id, event_type, payload, created_at)
    VALUES (NEW.project_id, NEW.id, 'session_updated',
        json_object('id', NEW.id, 'activity', NEW.activity_state, 'isTerminated', json(CASE WHEN NEW.is_terminated THEN 'true' ELSE 'false' END), 'previewUrl', NEW.preview_url, 'previewRevision', NEW.preview_revision),
        NEW.updated_at);
END;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE sessions DROP COLUMN diagnostic_captured_at;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE sessions DROP COLUMN diagnostic_hook_error_type;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE sessions DROP COLUMN diagnostic_terminal_tail;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE sessions DROP COLUMN diagnostic_trigger;
-- +goose StatementEnd
