-- +goose Up
-- +goose StatementBegin
ALTER TABLE notifications RENAME TO notifications_legacy;

DROP INDEX idx_notifications_status;
DROP INDEX idx_notifications_unread_dedupe;

CREATE TABLE notifications (
    id TEXT PRIMARY KEY,
    session_id TEXT REFERENCES sessions(id) ON DELETE CASCADE,
    project_id TEXT REFERENCES projects(id) ON DELETE CASCADE,
    pr_url TEXT NOT NULL DEFAULT '',
    type TEXT NOT NULL CHECK (
        type IN (
            'needs_input',
            'ready_to_merge',
            'pr_merged',
            'pr_closed_unmerged',
            'control_plane_failed',
            'control_plane_escalated',
            'control_plane_recovered'
        )
    ),
    title TEXT NOT NULL,
    body TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL DEFAULT 'unread' CHECK (status IN ('read', 'unread')),
    created_at TIMESTAMP NOT NULL,
    CHECK (
        (
            type IN ('control_plane_failed', 'control_plane_escalated', 'control_plane_recovered')
            AND session_id IS NULL
            AND project_id IS NULL
            AND pr_url = ''
        ) OR (
            type NOT IN ('control_plane_failed', 'control_plane_escalated', 'control_plane_recovered')
            AND session_id IS NOT NULL
            AND project_id IS NOT NULL
        )
    )
);

INSERT INTO notifications (id, session_id, project_id, pr_url, type, title, body, status, created_at)
SELECT id, session_id, project_id, pr_url, type, title, body, status, created_at
FROM notifications_legacy;

DROP TABLE notifications_legacy;

CREATE INDEX idx_notifications_status
    ON notifications(status, created_at DESC);

CREATE UNIQUE INDEX idx_notifications_unread_dedupe
    ON notifications(session_id, type, pr_url)
    WHERE status = 'unread'
      AND type NOT IN ('control_plane_failed', 'control_plane_escalated', 'control_plane_recovered');
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE notifications RENAME TO notifications_control_plane;

DROP INDEX idx_notifications_status;
DROP INDEX idx_notifications_unread_dedupe;

CREATE TABLE notifications (
    id TEXT PRIMARY KEY,
    session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    pr_url TEXT NOT NULL DEFAULT '',
    type TEXT NOT NULL CHECK (
        type IN (
            'needs_input',
            'ready_to_merge',
            'pr_merged',
            'pr_closed_unmerged'
        )
    ),
    title TEXT NOT NULL,
    body TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL DEFAULT 'unread' CHECK (status IN ('read', 'unread')),
    created_at TIMESTAMP NOT NULL
);

INSERT INTO notifications (id, session_id, project_id, pr_url, type, title, body, status, created_at)
SELECT id, session_id, project_id, pr_url, type, title, body, status, created_at
FROM notifications_control_plane
WHERE type NOT IN ('control_plane_failed', 'control_plane_escalated', 'control_plane_recovered');

DROP TABLE notifications_control_plane;

CREATE INDEX idx_notifications_status
    ON notifications(status, created_at DESC);

CREATE UNIQUE INDEX idx_notifications_unread_dedupe
    ON notifications(session_id, type, pr_url)
    WHERE status = 'unread';
-- +goose StatementEnd
