-- +goose Up
ALTER TABLE sessions ADD COLUMN workspace_kind TEXT NOT NULL DEFAULT 'worktree'
    CHECK (workspace_kind IN ('worktree', 'scratch', 'dir'));

-- +goose Down
ALTER TABLE sessions DROP COLUMN workspace_kind;
