-- +goose Up
-- +goose StatementBegin
CREATE TABLE session_dependencies (
    -- Deleting a child removes its outgoing graph edges. A parent is RESTRICTed:
    -- an in-flight spawn rollback must park a referenced parent terminal rather
    -- than silently erasing a child edge that has already committed.
    session_id            TEXT NOT NULL REFERENCES sessions (id) ON DELETE CASCADE,
    depends_on_session_id TEXT NOT NULL REFERENCES sessions (id) ON DELETE RESTRICT,
    PRIMARY KEY (session_id, depends_on_session_id),
    CHECK (session_id <> depends_on_session_id)
);
CREATE INDEX idx_session_dependencies_parent
    ON session_dependencies (depends_on_session_id, session_id);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE session_dependencies;
-- +goose StatementEnd
