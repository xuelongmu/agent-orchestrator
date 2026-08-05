-- +goose Up
-- head_repo records the full name of the repository the PR head branch lives
-- in. It differs from repo for fork PRs; stack-parent matching uses it to keep
-- a fork's branch from being mistaken for an AO-owned stack parent. Empty
-- identifies legacy rows observed before this column existed.
-- +goose StatementBegin
ALTER TABLE pr ADD COLUMN head_repo TEXT NOT NULL DEFAULT '';
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE pr DROP COLUMN head_repo;
-- +goose StatementEnd
