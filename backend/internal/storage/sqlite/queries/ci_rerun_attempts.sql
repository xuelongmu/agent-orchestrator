-- name: GetCIRerunAttempt :one
SELECT pr_url, head_sha, check_name, provider_id, status, requested_at
FROM ci_rerun_attempt
WHERE pr_url = ? AND head_sha = ? AND check_name = ?;

-- name: ReserveCIRerunAttempt :execrows
INSERT OR IGNORE INTO ci_rerun_attempt
    (pr_url, head_sha, check_name, provider_id, status, requested_at)
VALUES (?, ?, ?, ?, ?, ?);

-- name: UpdateCIRerunAttempt :execrows
UPDATE ci_rerun_attempt
SET provider_id = ?, status = ?, requested_at = ?
WHERE pr_url = ? AND head_sha = ? AND check_name = ?;
