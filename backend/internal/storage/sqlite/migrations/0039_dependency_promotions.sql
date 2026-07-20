-- +goose Up
-- A NULL marker identifies a dependency-declared session whose launch is still
-- gated. Existing dependency sessions predate scheduling and were already
-- launched, so backfill them before the scheduler begins reconciling.
-- +goose StatementBegin
ALTER TABLE sessions ADD COLUMN dependency_promoted_at TIMESTAMP;
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE sessions ADD COLUMN dependency_prepared_at TIMESTAMP;
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE sessions ADD COLUMN dependency_base_prompt TEXT NOT NULL DEFAULT '';
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE sessions ADD COLUMN dependency_promotion_token TEXT NOT NULL DEFAULT '';
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE sessions ADD COLUMN dependency_promotion_claimed_at TIMESTAMP;
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE sessions ADD COLUMN dependency_launch_succeeded_at TIMESTAMP;
-- +goose StatementEnd

-- +goose StatementBegin
UPDATE sessions
SET dependency_promoted_at = updated_at,
    dependency_prepared_at = updated_at,
    dependency_base_prompt = prompt
WHERE EXISTS (
    SELECT 1 FROM session_dependencies dependency
    WHERE dependency.session_id = sessions.id
);
-- +goose StatementEnd

-- Promotion is a durable session activity event. Keep capture in a DB trigger,
-- alongside the rest of AO's CDC, so the claim and event are atomic and an
-- exact replay cannot emit twice.
-- +goose StatementBegin
CREATE TRIGGER session_dependency_promotion_cdc
AFTER UPDATE OF dependency_promoted_at ON sessions
WHEN OLD.dependency_promoted_at IS NULL
 AND NEW.dependency_promoted_at IS NOT NULL
BEGIN
    INSERT INTO change_log (project_id, session_id, event_type, payload, created_at)
    VALUES (
        NEW.project_id,
        NEW.id,
        'session_updated',
        json_object('id', NEW.id, 'dependencyPromoted', json('true')),
        NEW.dependency_promoted_at
    );
END;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TRIGGER IF EXISTS session_dependency_promotion_cdc;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE sessions DROP COLUMN dependency_launch_succeeded_at;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE sessions DROP COLUMN dependency_promotion_claimed_at;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE sessions DROP COLUMN dependency_promotion_token;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE sessions DROP COLUMN dependency_base_prompt;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE sessions DROP COLUMN dependency_prepared_at;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE sessions DROP COLUMN dependency_promoted_at;
-- +goose StatementEnd
