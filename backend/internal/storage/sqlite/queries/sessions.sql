-- name: NextSessionNum :one
SELECT COALESCE(MAX(num), 0) + 1 AS next FROM sessions WHERE project_id = ?;

-- name: InsertSession :exec
INSERT INTO sessions (
    id, project_id, num, issue_id, kind, harness, display_name,
    activity_state, activity_last_at, first_signal_at, is_terminated,
    workspace_kind, branch, workspace_path, runtime_handle_id, agent_session_id, prompt,
    preview_url, preview_revision, pending_submit_fingerprint,
    pending_submit_recovery_attempted, diagnostic_trigger,
    diagnostic_terminal_tail, diagnostic_hook_error_type,
    diagnostic_captured_at, merged_cleanup_pending, merged_cleanup_pr_url,
    dependency_prepared_at, dependency_base_prompt, created_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: UpdateSession :exec
UPDATE sessions SET
    issue_id = ?, kind = ?, harness = ?, display_name = ?,
    activity_state = ?, activity_last_at = ?, first_signal_at = ?, is_terminated = ?,
    workspace_kind = ?, branch = ?, workspace_path = ?, runtime_handle_id = ?, agent_session_id = ?, prompt = ?,
    preview_url = ?, preview_revision = ?, pending_submit_fingerprint = ?,
    pending_submit_recovery_attempted = ?, diagnostic_trigger = ?,
    diagnostic_terminal_tail = ?, diagnostic_hook_error_type = ?,
    diagnostic_captured_at = ?, merged_cleanup_pending = ?, merged_cleanup_pr_url = ?, updated_at = ?
WHERE id = ?;

-- name: UpdateSessionLifecycle :exec
-- Lifecycle reads a session snapshot before reducing a hook/runtime signal.
-- This generic write-back is limited to the reducer's core fact columns.
-- Auxiliary durable metadata is updated only by the compare-and-set queries
-- below when the reducer explicitly transitions that field.
UPDATE sessions SET
    activity_state = ?, activity_last_at = ?, first_signal_at = ?, is_terminated = ?,
    diagnostic_trigger = ?,
    diagnostic_terminal_tail = ?, diagnostic_hook_error_type = ?,
    diagnostic_captured_at = ?, updated_at = ?
WHERE id = ?;

-- name: UpdateSessionLifecycleAgentID :execrows
UPDATE sessions SET agent_session_id = ?
WHERE id = ? AND agent_session_id = ?;

-- name: UpdateSessionLifecyclePendingSubmit :execrows
UPDATE sessions SET
    pending_submit_fingerprint = ?, pending_submit_recovery_attempted = ?
WHERE id = ?
  AND pending_submit_fingerprint = ?
  AND pending_submit_recovery_attempted = ?;

-- name: UpdateSessionLifecycleMergedCleanup :execrows
UPDATE sessions SET
    merged_cleanup_pending = ?, merged_cleanup_pr_url = ?
WHERE id = ?
  AND merged_cleanup_pending = ?
  AND merged_cleanup_pr_url = ?;

-- name: GetSession :one
SELECT id, project_id, num, issue_id, kind, harness,
    activity_state, activity_last_at, is_terminated, branch, workspace_path,
    runtime_handle_id, agent_session_id, prompt, created_at, updated_at, display_name, first_signal_at, preview_url, preview_revision,
    pending_submit_fingerprint, pending_submit_recovery_attempted,
    merged_cleanup_pending, merged_cleanup_pr_url,
    diagnostic_trigger, diagnostic_terminal_tail, diagnostic_hook_error_type,
    diagnostic_captured_at, workspace_kind, dependency_promoted_at, dependency_prepared_at, dependency_base_prompt,
    dependency_promotion_token, dependency_promotion_claimed_at, dependency_launch_succeeded_at
FROM sessions WHERE id = ?;

-- name: ListSessionsByProject :many
SELECT id, project_id, num, issue_id, kind, harness,
    activity_state, activity_last_at, is_terminated, branch, workspace_path,
    runtime_handle_id, agent_session_id, prompt, created_at, updated_at, display_name, first_signal_at, preview_url, preview_revision,
    pending_submit_fingerprint, pending_submit_recovery_attempted,
    merged_cleanup_pending, merged_cleanup_pr_url,
    diagnostic_trigger, diagnostic_terminal_tail, diagnostic_hook_error_type,
    diagnostic_captured_at, workspace_kind, dependency_promoted_at, dependency_prepared_at, dependency_base_prompt,
    dependency_promotion_token, dependency_promotion_claimed_at, dependency_launch_succeeded_at
FROM sessions WHERE project_id = ? ORDER BY num;

-- name: ListAllSessions :many
SELECT id, project_id, num, issue_id, kind, harness,
    activity_state, activity_last_at, is_terminated, branch, workspace_path,
    runtime_handle_id, agent_session_id, prompt, created_at, updated_at, display_name, first_signal_at, preview_url, preview_revision,
    pending_submit_fingerprint, pending_submit_recovery_attempted,
    merged_cleanup_pending, merged_cleanup_pr_url,
    diagnostic_trigger, diagnostic_terminal_tail, diagnostic_hook_error_type,
    diagnostic_captured_at, workspace_kind, dependency_promoted_at, dependency_prepared_at, dependency_base_prompt,
    dependency_promotion_token, dependency_promotion_claimed_at, dependency_launch_succeeded_at
FROM sessions ORDER BY project_id, num;

-- name: InsertSessionDependency :exec
INSERT INTO session_dependencies (session_id, depends_on_session_id)
VALUES (?, ?);

-- name: ListSessionDependencies :many
SELECT depends_on_session_id
FROM session_dependencies
WHERE session_id = ?
ORDER BY depends_on_session_id;

-- name: ListAllSessionDependencies :many
SELECT session_id, depends_on_session_id
FROM session_dependencies
ORDER BY session_id, depends_on_session_id;

-- name: ListReadyDependencySessions :many
SELECT child.id
FROM sessions child
WHERE child.dependency_promoted_at IS NULL
  AND child.dependency_prepared_at IS NOT NULL
  AND child.dependency_promotion_token = ''
  AND child.is_terminated = FALSE
  AND EXISTS (
      SELECT 1 FROM session_dependencies edge
      WHERE edge.session_id = child.id
  )
  AND NOT EXISTS (
      SELECT 1
      FROM session_dependencies edge
      JOIN sessions parent ON parent.id = edge.depends_on_session_id
      WHERE edge.session_id = child.id
        AND NOT (
            EXISTS (
                SELECT 1 FROM agent_handoffs handoff
                WHERE handoff.session_id = parent.id
            )
            OR (
              parent.is_terminated = TRUE
              AND EXISTS (
                    SELECT 1 FROM pr merged_pr
                    WHERE merged_pr.session_id = parent.id
                      AND merged_pr.is_merged = TRUE
              )
              AND NOT EXISTS (
                    SELECT 1 FROM pr open_pr
                    WHERE open_pr.session_id = parent.id
                      AND open_pr.is_merged = FALSE
                      AND open_pr.is_closed = FALSE
              )
            )
        )
      )
ORDER BY child.project_id, child.num;

-- name: ReserveDependencyPromotion :execrows
UPDATE sessions
SET dependency_promotion_token = ?, dependency_promotion_claimed_at = ?, updated_at = ?
WHERE sessions.id = ?
  AND sessions.dependency_promoted_at IS NULL
  AND sessions.dependency_prepared_at IS NOT NULL
  AND sessions.dependency_promotion_token = ''
  AND sessions.is_terminated = FALSE
  AND EXISTS (
      SELECT 1 FROM session_dependencies edge
      WHERE edge.session_id = sessions.id
  )
  AND NOT EXISTS (
      SELECT 1
      FROM session_dependencies edge
      JOIN sessions parent ON parent.id = edge.depends_on_session_id
      WHERE edge.session_id = sessions.id
        AND NOT (
            EXISTS (
                SELECT 1 FROM agent_handoffs handoff
                WHERE handoff.session_id = parent.id
            )
            OR (
              parent.is_terminated = TRUE
              AND EXISTS (
                    SELECT 1 FROM pr merged_pr
                    WHERE merged_pr.session_id = parent.id
                      AND merged_pr.is_merged = TRUE
              )
              AND NOT EXISTS (
                    SELECT 1 FROM pr open_pr
                    WHERE open_pr.session_id = parent.id
                      AND open_pr.is_merged = FALSE
                      AND open_pr.is_closed = FALSE
              )
            )
        )
      );

-- name: CompleteDependencyPromotion :execrows
UPDATE sessions
SET dependency_promoted_at = ?, dependency_promotion_token = '',
    dependency_promotion_claimed_at = NULL, updated_at = ?
WHERE id = ?
  AND dependency_promoted_at IS NULL
  AND dependency_promotion_token = ?
  AND is_terminated = FALSE
  AND runtime_handle_id <> ''
  AND dependency_launch_succeeded_at IS NOT NULL;

-- name: ReleaseDependencyPromotion :execrows
UPDATE sessions
SET dependency_promotion_token = '', dependency_promotion_claimed_at = NULL, updated_at = ?
WHERE id = ?
  AND dependency_promoted_at IS NULL
  AND dependency_promotion_token = ?;

-- name: RecoverDependencyPromotions :execrows
UPDATE sessions
SET dependency_promotion_token = '', dependency_promotion_claimed_at = NULL, updated_at = ?
WHERE dependency_promoted_at IS NULL
  AND dependency_promotion_token <> ''
  AND runtime_handle_id = '';

-- name: RecoverStaleDependencyPromotions :execrows
UPDATE sessions
SET dependency_promotion_token = '', dependency_promotion_claimed_at = NULL, updated_at = ?
WHERE dependency_promoted_at IS NULL
  AND dependency_promotion_token <> ''
  AND runtime_handle_id = ''
  AND dependency_promotion_claimed_at < ?;

-- name: MarkReservedDependencySpawned :execrows
UPDATE sessions
SET workspace_kind = ?, branch = ?, workspace_path = ?, runtime_handle_id = ?,
    prompt = ?, activity_state = 'idle', activity_last_at = ?, first_signal_at = NULL,
    diagnostic_trigger = '', diagnostic_terminal_tail = '', diagnostic_hook_error_type = '',
    diagnostic_captured_at = NULL, updated_at = ?
WHERE id = ?
  AND dependency_promoted_at IS NULL
  AND dependency_promotion_token = ?
  AND is_terminated = FALSE;

-- name: MarkReservedDependencyLaunchSucceeded :execrows
UPDATE sessions
SET dependency_launch_succeeded_at = ?, updated_at = ?
WHERE id = ?
  AND dependency_promoted_at IS NULL
  AND dependency_promotion_token = ?
  AND is_terminated = FALSE
  AND runtime_handle_id <> '';

-- name: ResetReservedDependencyLaunch :execrows
UPDATE sessions
SET workspace_path = '', runtime_handle_id = '', agent_session_id = '',
    prompt = dependency_base_prompt,
    dependency_launch_succeeded_at = NULL,
    activity_state = CASE WHEN is_terminated THEN activity_state ELSE 'idle' END,
    activity_last_at = CASE WHEN is_terminated THEN activity_last_at ELSE ? END,
    first_signal_at = CASE WHEN is_terminated THEN first_signal_at ELSE NULL END,
    updated_at = ?
WHERE id = ?
  AND dependency_promoted_at IS NULL
  AND dependency_promotion_token = ?;

-- name: ListDependencyHandoffPayloads :many
SELECT edge.depends_on_session_id, COALESCE(handoff.payload, '') AS payload
FROM session_dependencies edge
LEFT JOIN agent_handoffs handoff ON handoff.session_id = edge.depends_on_session_id
WHERE edge.session_id = ?
ORDER BY edge.depends_on_session_id;

-- name: InsertSessionHandoff :execrows
INSERT OR IGNORE INTO agent_handoffs (session_id, payload, created_at)
VALUES (?, ?, ?);

-- name: GetSessionHandoffPayload :one
SELECT payload FROM agent_handoffs WHERE session_id = ?;

-- name: SetPendingSubmit :execrows
UPDATE sessions SET
    pending_submit_fingerprint = ?, pending_submit_recovery_attempted = FALSE,
    updated_at = ?
WHERE id = ? AND is_terminated = FALSE;

-- name: ClaimPendingSubmitRecovery :execrows
UPDATE sessions SET pending_submit_recovery_attempted = TRUE, updated_at = ?
WHERE id = ?
  AND pending_submit_fingerprint = ?
  AND pending_submit_recovery_attempted = FALSE
  AND is_terminated = FALSE
  AND activity_state NOT IN ('blocked', 'rate_limited');

-- name: ClearPendingSubmit :execrows
UPDATE sessions SET
    pending_submit_fingerprint = '', pending_submit_recovery_attempted = FALSE,
    updated_at = ?
WHERE id = ? AND pending_submit_fingerprint = ?;


-- name: RenameSession :execrows
UPDATE sessions SET display_name = ?, updated_at = ? WHERE id = ?;

-- name: SetSessionPreviewURL :execrows
-- preview_revision is bumped on every call (even when preview_url is unchanged)
-- so a repeated `ao preview <same-url>` still trips the sessions_cdc_update
-- trigger and the desktop browser panel re-navigates / refreshes.
UPDATE sessions SET preview_url = ?, preview_revision = preview_revision + 1, updated_at = ? WHERE id = ?;

-- name: SetClaimedPRBranch :exec
UPDATE sessions SET branch = ?, updated_at = ? WHERE id = ?;

-- name: SessionIsSeed :one
-- SessionIsSeed reports whether the session id matches a row still in seed
-- state (see DeleteSeedSession for the conditions). Callers probe with this
-- before touching change_log so that DeleteSession is a true no-op for live
-- sessions instead of silently destroying their CDC events. Returns 0 when
-- the row does not exist OR has progressed past seed state.
SELECT EXISTS(
    SELECT 1 FROM sessions
    WHERE id = ?
      AND is_terminated = 0
      AND workspace_path = ''
      AND runtime_handle_id = ''
      AND agent_session_id = ''
      AND prompt = ''
) AS is_seed;

-- NOTE: the `DELETE FROM sessions WHERE id = ? AND <seed-state predicates>`
-- statement is intentionally NOT a sqlc query — same sqlc 1.31 SQLite-parser
-- bug as documented in queries/changelog.sql: trailing string literals (and
-- placeholders) on the RHS of `=` in a DELETE get silently stripped, so the
-- generated SQL ends up mid-clause and the row count is meaningless. The
-- store runs that DELETE directly via tx.ExecContext inside
-- Store.DeleteSession, inside the same transaction as the SessionIsSeed
-- probe and the raw change_log cleanup.
