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

-- name: GetOwnedPRDesignContract :one
SELECT COALESCE(pr_design_contract.markdown, '') AS markdown,
       pr_design_contract.pr_url IS NOT NULL AS contract_exists
FROM pr
LEFT JOIN pr_design_contract ON pr_design_contract.pr_url = pr.url
WHERE pr.url = sqlc.arg(pr_url)
  AND pr.session_id = sqlc.arg(session_id);

-- name: AppendPRDesignContractInvariant :execrows
UPDATE pr_design_contract
SET markdown = markdown || sqlc.arg(addition),
    contract_revision = contract_revision + 1,
    updated_at = sqlc.arg(updated_at)
WHERE pr_url = sqlc.arg(pr_url);

-- name: RequirePRDesignContractDelivery :execrows
UPDATE pr_design_contract
SET pending_delivery_session_id = sqlc.arg(session_id),
    pending_delivery_task_prompt = sqlc.arg(task_prompt),
    pending_delivery_token = sqlc.arg(delivery_token),
    delivery_required_at = sqlc.arg(required_at)
WHERE pr_url = sqlc.arg(pr_url);

-- name: GetPendingPRDesignContractDelivery :one
SELECT markdown,
       pending_delivery_task_prompt AS task_prompt,
       pending_delivery_token AS delivery_token,
       contract_revision
FROM pr_design_contract
WHERE pr_url = sqlc.arg(pr_url)
  AND pending_delivery_session_id = sqlc.arg(session_id)
  AND EXISTS (
      SELECT 1 FROM pr
      WHERE pr.url = pr_design_contract.pr_url
        AND pr.session_id = sqlc.arg(session_id)
  );

-- name: CompletePRDesignContractDelivery :execrows
UPDATE pr_design_contract
SET pending_delivery_session_id = NULL,
    pending_delivery_task_prompt = '',
    pending_delivery_token = '',
    delivery_required_at = NULL
WHERE pr_url = sqlc.arg(pr_url)
  AND pending_delivery_session_id = sqlc.arg(session_id)
  AND pending_delivery_token = sqlc.arg(delivery_token)
  AND contract_revision = sqlc.arg(contract_revision)
  AND EXISTS (
      SELECT 1 FROM pr
      WHERE pr.url = pr_design_contract.pr_url
        AND pr.session_id = sqlc.arg(session_id)
  );
