-- Widen the durable session activity CHECK for the provider-neutral
-- rate_limited parked state. SQLite cannot ALTER a CHECK, so follow the same
-- out-of-transaction writable-schema pattern used by migration 0007; RESET
-- reparses the schema immediately on this connection.

-- +goose NO TRANSACTION
-- +goose Up
-- +goose StatementBegin
PRAGMA writable_schema = ON;
-- +goose StatementEnd
-- +goose StatementBegin
UPDATE sqlite_master
SET sql = replace(
    sql,
    'CHECK (activity_state IN (''active'', ''idle'', ''waiting_input'', ''blocked'', ''exited''))',
    'CHECK (activity_state IN (''active'', ''idle'', ''waiting_input'', ''blocked'', ''rate_limited'', ''exited''))'
)
WHERE type = 'table' AND name = 'sessions';
-- +goose StatementEnd
-- +goose StatementBegin
PRAGMA writable_schema = RESET;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
UPDATE sessions SET activity_state = 'idle' WHERE activity_state = 'rate_limited';
-- +goose StatementEnd
-- +goose StatementBegin
PRAGMA writable_schema = ON;
-- +goose StatementEnd
-- +goose StatementBegin
UPDATE sqlite_master
SET sql = replace(
    sql,
    'CHECK (activity_state IN (''active'', ''idle'', ''waiting_input'', ''blocked'', ''rate_limited'', ''exited''))',
    'CHECK (activity_state IN (''active'', ''idle'', ''waiting_input'', ''blocked'', ''exited''))'
)
WHERE type = 'table' AND name = 'sessions';
-- +goose StatementEnd
-- +goose StatementBegin
PRAGMA writable_schema = RESET;
-- +goose StatementEnd
