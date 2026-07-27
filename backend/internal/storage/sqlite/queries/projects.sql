-- name: UpsertProject :exec
INSERT INTO projects (id, path, repo_origin_url, display_name, registered_at, archived_at, config, kind)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (id) DO UPDATE SET
    path = excluded.path,
    repo_origin_url = excluded.repo_origin_url,
    display_name = excluded.display_name,
    registered_at = excluded.registered_at,
    archived_at = excluded.archived_at,
    config = excluded.config,
    kind = excluded.kind;

-- name: GetProject :one
SELECT id, path, repo_origin_url, display_name, registered_at, archived_at, config, kind
FROM projects WHERE id = ?;

-- name: UpdateProjectConfig :execrows
UPDATE projects
SET config = ?
WHERE id = ?
  -- Older driver writes may include Go's transient " m=+..." monotonic suffix.
  -- Strip it so a timestamp read back from SQLite still identifies that row.
  AND substr(
      CAST(registered_at AS TEXT),
      1,
      instr(CAST(registered_at AS TEXT) || ' m=', ' m=') - 1
  ) = CAST(sqlc.arg(registered_at_text) AS TEXT)
  AND archived_at IS NULL;

-- name: UpdateProjectOriginURL :execrows
UPDATE projects
SET repo_origin_url = ?
WHERE id = ?
  AND substr(
      CAST(registered_at AS TEXT),
      1,
      instr(CAST(registered_at AS TEXT) || ' m=', ' m=') - 1
  ) = CAST(sqlc.arg(registered_at_text) AS TEXT)
  AND archived_at IS NULL;

-- name: ListProjects :many
SELECT id, path, repo_origin_url, display_name, registered_at, archived_at, config, kind
FROM projects WHERE archived_at IS NULL ORDER BY id;

-- name: FindProjectByPath :one
SELECT id, path, repo_origin_url, display_name, registered_at, archived_at, config, kind
FROM projects WHERE path = ? AND archived_at IS NULL;

-- name: ArchiveProject :execrows
UPDATE projects SET archived_at = ? WHERE id = ? AND archived_at IS NULL;
