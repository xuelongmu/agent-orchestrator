-- +goose Up
-- A prompt digest is latched only after terminal output proves the complete
-- prompt reached an agent editor but did not submit. The attempted bit is an
-- at-most-once claim for the guarded Enter-only recovery across daemon restarts.
-- +goose StatementBegin
ALTER TABLE sessions ADD COLUMN pending_submit_fingerprint TEXT NOT NULL DEFAULT '';
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE sessions ADD COLUMN pending_submit_recovery_attempted BOOLEAN NOT NULL DEFAULT FALSE;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE sessions DROP COLUMN pending_submit_recovery_attempted;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE sessions DROP COLUMN pending_submit_fingerprint;
-- +goose StatementEnd
