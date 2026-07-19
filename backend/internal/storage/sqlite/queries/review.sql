-- name: UpsertReview :exec
INSERT INTO review (id, session_id, project_id, harness, pr_url, reviewer_handle_id, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (session_id) DO UPDATE SET
    harness = excluded.harness,
    pr_url = excluded.pr_url,
    reviewer_handle_id = excluded.reviewer_handle_id,
    updated_at = excluded.updated_at;

-- name: GetReviewBySession :one
SELECT id, session_id, project_id, harness, pr_url, reviewer_handle_id, created_at, updated_at
FROM review WHERE session_id = ?;

-- name: InsertReviewRun :exec
INSERT INTO review_run (id, review_id, session_id, batch_id, harness, pr_url, target_sha, status, verdict, body, github_review_id, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: UpdateReviewRunResult :execrows
UPDATE review_run SET status = ?, verdict = ?, body = ?, github_review_id = ? WHERE id = ? AND status = 'running';

-- name: CompleteReviewRunResult :execrows
UPDATE review_run
SET status = 'complete', verdict = ?, body = ?, github_review_id = ?, simplification_class = ?
WHERE id = ? AND status = 'running';

-- name: RefreshReviewRunSimplificationClass :execrows
UPDATE review_run
SET simplification_class = COALESCE((
    SELECT TRIM(history.class_tag)
    FROM review_finding AS history
    WHERE history.session_id = review_run.session_id
      AND history.pr_url = review_run.pr_url
      AND NOT (
        history.out_of_scope = 1 AND history.deferred_issue_url != ''
        AND history.thread_id != '' AND history.thread_resolved = 1
      )
      AND EXISTS (
        SELECT 1
        FROM review_finding AS current
        WHERE current.run_id = review_run.id
          AND TRIM(current.class_tag) = TRIM(history.class_tag)
          AND NOT (
            current.out_of_scope = 1 AND current.deferred_issue_url != ''
            AND current.thread_id != '' AND current.thread_resolved = 1
          )
      )
    GROUP BY TRIM(history.class_tag)
    HAVING COUNT(*) >= 3
    ORDER BY COUNT(*) DESC, TRIM(history.class_tag) ASC
    LIMIT 1
), '')
WHERE review_run.id = ? AND review_run.status = 'complete' AND review_run.delivered_at IS NULL;

-- name: SupersedeStaleRunningReviewRuns :execrows
UPDATE review_run SET status = 'failed', body = ? WHERE session_id = ? AND pr_url = ? AND target_sha != ? AND status = 'running' AND verdict = '';

-- name: CancelRunningReviewRunsBySession :execrows
UPDATE review_run SET status = 'cancelled', body = ? WHERE session_id = ? AND status = 'running' AND verdict = '';

-- name: MarkReviewRunDelivered :execrows
UPDATE review_run
SET status = 'delivered', delivered_at = ?
WHERE id = ? AND status = 'complete' AND delivered_at IS NULL;

-- name: ClaimReviewRunSimplificationDispatch :execrows
UPDATE review_run
SET simplification_dispatched_at = ?, simplification_event_id = ?
WHERE id = ? AND target_sha = ? AND status = 'complete'
  AND simplification_class != '' AND simplification_dispatched_at IS NULL;

-- name: MarkReviewRunDeflectedReviewCleared :execrows
UPDATE review_run
SET deflected_review_cleared_at = ?
WHERE id = ? AND deflected_review_cleared_at IS NULL;

-- name: GetReviewRun :one
SELECT id, review_id, session_id, harness, pr_url, target_sha, status, verdict, body, created_at, github_review_id, delivered_at, batch_id, simplification_class, simplification_dispatched_at, deflected_review_cleared_at, simplification_event_id
FROM review_run WHERE id = ?;

-- name: GetReviewRunBySessionPRAndSHA :one
SELECT id, review_id, session_id, harness, pr_url, target_sha, status, verdict, body, created_at, github_review_id, delivered_at, batch_id, simplification_class, simplification_dispatched_at, deflected_review_cleared_at, simplification_event_id
FROM review_run WHERE session_id = ? AND pr_url = ? AND target_sha = ? ORDER BY created_at DESC LIMIT 1;

-- name: ListReviewRunsBySession :many
SELECT id, review_id, session_id, harness, pr_url, target_sha, status, verdict, body, created_at, github_review_id, delivered_at, batch_id, simplification_class, simplification_dispatched_at, deflected_review_cleared_at, simplification_event_id
FROM review_run WHERE session_id = ? ORDER BY created_at DESC;

-- name: ListRunningReviewRunsBySession :many
SELECT id, review_id, session_id, harness, pr_url, target_sha, status, verdict, body, created_at, github_review_id, delivered_at, batch_id, simplification_class, simplification_dispatched_at, deflected_review_cleared_at, simplification_event_id
FROM review_run WHERE session_id = ? AND status = 'running' AND verdict = '' ORDER BY created_at DESC;

-- name: ListReviewRunsByBatch :many
SELECT id, review_id, session_id, harness, pr_url, target_sha, status, verdict, body, created_at, github_review_id, delivered_at, batch_id, simplification_class, simplification_dispatched_at, deflected_review_cleared_at, simplification_event_id
FROM review_run WHERE session_id = ? AND batch_id = ? ORDER BY created_at ASC, id ASC;

-- name: InsertReviewFinding :exec
INSERT INTO review_finding (
    id, run_id, session_id, pr_url, round, file, class_tag, root_cause_note,
    fix_commit, thread_id, body, out_of_scope, deferred_issue_url,
    thread_resolved, created_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (id) DO NOTHING;

-- name: InsertReviewFindingStrict :exec
INSERT INTO review_finding (
    id, run_id, session_id, pr_url, round, file, class_tag, root_cause_note,
    fix_commit, thread_id, body, out_of_scope, deferred_issue_url,
    thread_resolved, thread_reply_id, created_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: ListReviewFindingsBySession :many
SELECT id, run_id, session_id, pr_url, round, file, class_tag, root_cause_note,
       fix_commit, thread_id, body, out_of_scope, deferred_issue_url,
       thread_resolved, thread_reply_id, issue_action_token,
       issue_action_lease_until, thread_action_token, thread_action_lease_until, created_at
FROM review_finding
WHERE session_id = ?
ORDER BY round ASC, created_at ASC, id ASC;

-- name: ListReviewFindingsByRun :many
SELECT id, run_id, session_id, pr_url, round, file, class_tag, root_cause_note,
       fix_commit, thread_id, body, out_of_scope, deferred_issue_url,
       thread_resolved, thread_reply_id, issue_action_token,
       issue_action_lease_until, thread_action_token, thread_action_lease_until, created_at
FROM review_finding
WHERE run_id = ?
ORDER BY created_at ASC, id ASC;

-- name: SetPendingReviewFindingFixCommit :execrows
UPDATE review_finding
SET fix_commit = ?
WHERE session_id = ? AND pr_url = ? AND fix_commit = ''
  AND NOT (
    out_of_scope = 1 AND deferred_issue_url != '' AND thread_id != '' AND thread_resolved = 1
  );

-- name: ClaimReviewFindingIssueAction :execrows
UPDATE review_finding
SET issue_action_token = ?, issue_action_lease_until = ?
WHERE id = ? AND out_of_scope = 1 AND deferred_issue_url = ''
  AND (issue_action_token = '' OR issue_action_lease_until IS NULL OR issue_action_lease_until <= ?);

-- name: CompleteReviewFindingIssueAction :execrows
UPDATE review_finding
SET deferred_issue_url = ?, issue_action_token = '', issue_action_lease_until = NULL
WHERE id = ? AND out_of_scope = 1 AND deferred_issue_url = '' AND issue_action_token = ?;

-- name: ReleaseReviewFindingIssueAction :execrows
UPDATE review_finding
SET issue_action_token = '', issue_action_lease_until = NULL
WHERE id = ? AND out_of_scope = 1 AND deferred_issue_url = '' AND issue_action_token = ?;

-- name: ClaimReviewFindingThreadAction :execrows
UPDATE review_finding
SET thread_action_token = ?, thread_action_lease_until = ?
WHERE id = ? AND out_of_scope = 1 AND deferred_issue_url != '' AND thread_resolved = 0
  AND (thread_action_token = '' OR thread_action_lease_until IS NULL OR thread_action_lease_until <= ?);

-- name: CompleteReviewFindingThreadAction :execrows
UPDATE review_finding
SET thread_resolved = 1, thread_reply_id = ?, thread_action_token = '', thread_action_lease_until = NULL
WHERE id = ? AND out_of_scope = 1 AND thread_resolved = 0 AND thread_action_token = ?;

-- name: ReleaseReviewFindingThreadAction :execrows
UPDATE review_finding
SET thread_action_token = '', thread_action_lease_until = NULL
WHERE id = ? AND out_of_scope = 1 AND thread_resolved = 0 AND thread_action_token = ?;
