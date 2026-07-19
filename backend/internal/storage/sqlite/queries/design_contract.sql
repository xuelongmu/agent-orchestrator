-- name: UpsertSessionDesignContractSeed :exec
INSERT INTO session_design_contract_seed (session_id, markdown, updated_at)
VALUES (?, ?, ?)
ON CONFLICT (session_id) DO UPDATE SET
    markdown = excluded.markdown,
    updated_at = excluded.updated_at;

-- name: EnsurePRDesignContract :exec
INSERT INTO pr_design_contract (pr_url, markdown, updated_at)
VALUES (
    sqlc.arg(pr_url),
    COALESCE((SELECT markdown FROM session_design_contract_seed WHERE session_id = sqlc.arg(session_id)), sqlc.arg(fallback_markdown)),
    sqlc.arg(updated_at)
)
ON CONFLICT (pr_url) DO NOTHING;

-- name: GetPRDesignContract :one
SELECT markdown FROM pr_design_contract WHERE pr_url = ?;

-- name: AppendPRDesignContractInvariant :execrows
UPDATE pr_design_contract
SET markdown = markdown || sqlc.arg(addition), updated_at = sqlc.arg(updated_at)
WHERE pr_url = sqlc.arg(pr_url) AND instr(markdown, sqlc.arg(invariant)) = 0;
