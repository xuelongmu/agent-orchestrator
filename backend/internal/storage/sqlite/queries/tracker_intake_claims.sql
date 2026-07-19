-- name: GetTrackerIntakeClaim :one
SELECT project_id, provider, repo, issue_id, owner_token, status, session_id,
       claimed_at, lease_expires_at, completed_at
FROM tracker_intake_claims
WHERE project_id = ? AND provider = ? AND repo = ? AND issue_id = ?;

-- name: FindLegacyTrackerIntakeSession :one
SELECT id
FROM sessions
WHERE project_id = ? AND issue_id = ?
ORDER BY num
LIMIT 1;

-- name: FindLegacyGitHubTrackerIntakeSession :one
SELECT id
FROM sessions
WHERE project_id = ? AND issue_id = ? COLLATE NOCASE
ORDER BY num
LIMIT 1;

-- name: TrackerIntakeActiveSessionMatches :one
SELECT EXISTS(
    SELECT 1 FROM sessions
    WHERE id = sqlc.arg(id) AND project_id = sqlc.arg(project_id)
      AND (
          issue_id = sqlc.arg(canonical_issue_id)
          OR (CAST(sqlc.arg(provider) AS TEXT) = 'github' AND issue_id = sqlc.arg(canonical_issue_id) COLLATE NOCASE)
      )
      AND is_terminated = FALSE
      AND (workspace_path <> '' OR runtime_handle_id <> '' OR agent_session_id <> '' OR prompt <> '')
) AS matches;

-- name: TrackerIntakeCompletionSessionMatches :one
SELECT EXISTS(
    SELECT 1 FROM sessions
    WHERE id = sqlc.arg(id) AND project_id = sqlc.arg(project_id)
      AND (
          issue_id = sqlc.arg(canonical_issue_id)
          OR (CAST(sqlc.arg(provider) AS TEXT) = 'github' AND issue_id = sqlc.arg(canonical_issue_id) COLLATE NOCASE)
      )
      AND (workspace_path <> '' OR runtime_handle_id <> '' OR agent_session_id <> '' OR prompt <> '')
) AS matches;

-- name: TrackerIntakeClaimOwned :one
SELECT EXISTS(
    SELECT 1 FROM tracker_intake_claims
    WHERE project_id = ? AND provider = ? AND repo = ? AND issue_id = ?
      AND status = 'pending' AND owner_token = ? AND lease_expires_at > ?
) AS owned;

-- name: CountTrackerIntakeCapacityUsed :one
SELECT
    (SELECT COUNT(*)
       FROM sessions AS active_session
      WHERE active_session.project_id = ?
        AND active_session.kind = 'worker'
        AND active_session.is_terminated = FALSE
        AND (
            active_session.workspace_path <> ''
            OR active_session.runtime_handle_id <> ''
            OR active_session.agent_session_id <> ''
            OR active_session.prompt <> ''
            OR active_session.issue_id = ''
            OR NOT EXISTS (
                SELECT 1 FROM tracker_intake_claims AS seed_claim
                WHERE seed_claim.project_id = active_session.project_id
                  AND seed_claim.status IN ('admitted', 'spawning')
                  AND seed_claim.session_id = active_session.id
            )
        ))
    +
    (SELECT COUNT(*)
       FROM tracker_intake_claims AS claim
      WHERE claim.project_id = ?
        AND (
            (claim.status = 'pending' AND claim.lease_expires_at > ?)
            OR (claim.status = 'admitted' AND claim.lease_expires_at > ?)
            OR (
                claim.status = 'spawning'
                AND (
                    claim.lease_expires_at > ?
                    OR EXISTS (SELECT 1 FROM sessions AS bound_session WHERE bound_session.id = claim.session_id)
                )
            )
        )
        AND NOT EXISTS (
            SELECT 1
              FROM sessions
             WHERE sessions.project_id = claim.project_id
               AND (
                   (claim.provider <> 'github' AND sessions.issue_id = claim.provider || ':' || claim.issue_id)
                   OR (
                       claim.provider = 'github'
                       AND sessions.issue_id = claim.provider || ':' || claim.issue_id COLLATE NOCASE
                   )
               )
               AND sessions.is_terminated = FALSE
               AND (sessions.workspace_path <> '' OR sessions.runtime_handle_id <> '' OR sessions.agent_session_id <> '' OR sessions.prompt <> '')
        )) AS used;

-- name: InsertPendingTrackerIntakeClaim :execrows
INSERT INTO tracker_intake_claims (
    project_id, provider, repo, issue_id, owner_token, status, session_id,
    claimed_at, lease_expires_at, completed_at
) VALUES (?, ?, ?, ?, ?, 'pending', '', ?, ?, NULL)
ON CONFLICT (project_id, provider, repo, issue_id) DO NOTHING;

-- name: TakeOverExpiredTrackerIntakeClaim :execrows
UPDATE tracker_intake_claims
SET owner_token = ?, status = 'pending', session_id = '', claimed_at = ?,
    lease_expires_at = ?, completed_at = NULL
WHERE project_id = ? AND provider = ? AND repo = ? AND issue_id = ?
  AND status IN ('pending', 'admitted', 'spawning', 'retryable')
  AND owner_token = ?
  AND lease_expires_at <= ?;

-- name: AttachTrackerIntakeClaimSeed :execrows
UPDATE tracker_intake_claims
SET status = 'admitted', session_id = ?
WHERE project_id = ? AND provider = ? AND repo = ? AND issue_id = ?
  AND status = 'pending' AND owner_token = ? AND lease_expires_at > ?
  AND session_id = '';

-- name: MarkTrackerIntakeClaimSpawning :execrows
UPDATE tracker_intake_claims
SET status = 'spawning'
WHERE project_id = ? AND provider = ? AND repo = ? AND issue_id = ?
  AND status = 'admitted' AND owner_token = ? AND session_id = ?
  AND lease_expires_at > ?;

-- name: InsertCompletedTrackerIntakeClaim :execrows
INSERT INTO tracker_intake_claims (
    project_id, provider, repo, issue_id, owner_token, status, session_id,
    claimed_at, lease_expires_at, completed_at
) VALUES (?, ?, ?, ?, ?, 'completed', ?, ?, ?, ?)
ON CONFLICT (project_id, provider, repo, issue_id) DO NOTHING;

-- name: ReconcileTrackerIntakeClaim :execrows
UPDATE tracker_intake_claims
SET status = 'completed', session_id = ?, lease_expires_at = ?, completed_at = ?
WHERE project_id = ? AND provider = ? AND repo = ? AND issue_id = ?;

-- name: CompleteTrackerIntakeClaim :execrows
UPDATE tracker_intake_claims
SET status = 'completed', session_id = ?, lease_expires_at = ?, completed_at = ?
WHERE project_id = ? AND provider = ? AND repo = ? AND issue_id = ?
  AND status IN ('admitted', 'spawning') AND owner_token = ? AND session_id = ?;

-- name: RenewTrackerIntakeClaim :execrows
UPDATE tracker_intake_claims
SET lease_expires_at = CASE
    WHEN status IN ('pending', 'admitted', 'spawning') AND lease_expires_at < ? THEN ?
    ELSE lease_expires_at
END
WHERE project_id = ? AND provider = ? AND repo = ? AND issue_id = ?
  AND (
      status = 'completed'
      OR (status IN ('pending', 'admitted', 'spawning') AND owner_token = ? AND lease_expires_at > ?)
  );

-- name: ReleaseTrackerIntakeClaim :execrows
UPDATE tracker_intake_claims
SET status = 'retryable', lease_expires_at = ?, completed_at = NULL
WHERE project_id = ? AND provider = ? AND repo = ? AND issue_id = ?
  AND status IN ('pending', 'admitted', 'spawning') AND owner_token = ?;
