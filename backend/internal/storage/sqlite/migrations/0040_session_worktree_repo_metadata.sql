-- +goose Up
-- Historical worktree rows must remain independently operable when the
-- mutable workspace registry changes. NULL identifies legacy rows that still
-- require registry resolution.
-- +goose StatementBegin
ALTER TABLE session_worktrees ADD COLUMN repo_path TEXT;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE session_worktrees ADD COLUMN relative_path TEXT;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE session_worktrees DROP COLUMN relative_path;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE session_worktrees DROP COLUMN repo_path;
-- +goose StatementEnd
