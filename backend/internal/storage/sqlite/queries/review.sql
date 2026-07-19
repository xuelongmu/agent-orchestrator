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

-- name: SupersedeStaleRunningReviewRuns :execrows
UPDATE review_run SET status = 'failed', body = ? WHERE session_id = ? AND pr_url = ? AND target_sha != ? AND status = 'running' AND verdict = '';

-- name: CancelRunningReviewRunsBySession :execrows
UPDATE review_run SET status = 'cancelled', body = ? WHERE session_id = ? AND status = 'running' AND verdict = '';

-- name: MarkReviewRunDelivered :execrows
UPDATE review_run SET status = 'delivered', delivered_at = ? WHERE id = ? AND status = 'complete' AND delivered_at IS NULL;

-- name: GetReviewRun :one
SELECT id, review_id, session_id, harness, pr_url, target_sha, status, verdict, body, created_at, github_review_id, delivered_at, batch_id
FROM review_run WHERE id = ?;

-- name: GetReviewRunBySessionPRAndSHA :one
SELECT id, review_id, session_id, harness, pr_url, target_sha, status, verdict, body, created_at, github_review_id, delivered_at, batch_id
FROM review_run WHERE session_id = ? AND pr_url = ? AND target_sha = ? ORDER BY created_at DESC LIMIT 1;

-- name: ListReviewRunsBySession :many
SELECT id, review_id, session_id, harness, pr_url, target_sha, status, verdict, body, created_at, github_review_id, delivered_at, batch_id
FROM review_run WHERE session_id = ? ORDER BY created_at DESC;

-- name: ListRunningReviewRunsBySession :many
SELECT id, review_id, session_id, harness, pr_url, target_sha, status, verdict, body, created_at, github_review_id, delivered_at, batch_id
FROM review_run WHERE session_id = ? AND status = 'running' AND verdict = '' ORDER BY created_at DESC;

-- name: ListReviewRunsByBatch :many
SELECT id, review_id, session_id, harness, pr_url, target_sha, status, verdict, body, created_at, github_review_id, delivered_at, batch_id
FROM review_run WHERE session_id = ? AND batch_id = ? ORDER BY created_at ASC, id ASC;

-- name: InsertReviewFinding :exec
INSERT INTO review_finding (
    id, run_id, session_id, pr_url, round, file, class_tag, root_cause_note,
    fix_commit, thread_id, body, out_of_scope, deferred_issue_url,
    thread_resolved, created_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (id) DO NOTHING;

-- name: ListReviewFindingsBySession :many
SELECT id, run_id, session_id, pr_url, round, file, class_tag, root_cause_note,
       fix_commit, thread_id, body, out_of_scope, deferred_issue_url,
       thread_resolved, created_at
FROM review_finding
WHERE session_id = ?
ORDER BY round ASC, created_at ASC, id ASC;

-- name: ListReviewFindingsByRun :many
SELECT id, run_id, session_id, pr_url, round, file, class_tag, root_cause_note,
       fix_commit, thread_id, body, out_of_scope, deferred_issue_url,
       thread_resolved, created_at
FROM review_finding
WHERE run_id = ?
ORDER BY created_at ASC, id ASC;

-- name: SetPendingReviewFindingFixCommit :execrows
UPDATE review_finding
SET fix_commit = ?
WHERE session_id = ? AND pr_url = ? AND fix_commit = '';

-- name: MarkReviewFindingIssueFiled :execrows
UPDATE review_finding
SET deferred_issue_url = ?
WHERE id = ? AND deferred_issue_url = '';

-- name: MarkReviewFindingThreadResolved :execrows
UPDATE review_finding
SET thread_resolved = 1
WHERE id = ? AND thread_resolved = 0;
