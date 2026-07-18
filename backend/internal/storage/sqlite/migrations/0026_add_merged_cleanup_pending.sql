-- +goose Up
-- A merge-complete session latches cleanup intent before external teardown.
-- The SCM observer retries this durable marker on later polls and after daemon
-- restart until runtime/workspace teardown succeeds. The triggering PR URL
-- resumes the terminal notification after a transient cleanup failure.
-- +goose StatementBegin
ALTER TABLE sessions ADD COLUMN merged_cleanup_pending BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE sessions ADD COLUMN merged_cleanup_pr_url TEXT NOT NULL DEFAULT '';
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE sessions DROP COLUMN merged_cleanup_pr_url;
ALTER TABLE sessions DROP COLUMN merged_cleanup_pending;
-- +goose StatementEnd
