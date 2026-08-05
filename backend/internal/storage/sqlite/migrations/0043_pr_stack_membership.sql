-- +goose Up
-- Native stacked-PR membership (GitHub public preview). stack_number is the
-- provider's per-repository stack number; zero means the provider reported no
-- stack (or does not support the feature) and branch-topology inference
-- applies alone. Position/size are informational context for display.
-- +goose StatementBegin
ALTER TABLE pr ADD COLUMN stack_number INTEGER NOT NULL DEFAULT 0;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE pr ADD COLUMN stack_position INTEGER NOT NULL DEFAULT 0;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE pr ADD COLUMN stack_size INTEGER NOT NULL DEFAULT 0;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE pr DROP COLUMN stack_size;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE pr DROP COLUMN stack_position;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE pr DROP COLUMN stack_number;
-- +goose StatementEnd
