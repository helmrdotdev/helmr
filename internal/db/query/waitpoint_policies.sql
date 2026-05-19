-- name: CreateWaitpointPolicy :one
INSERT INTO waitpoint_policies (
    id,
    org_id,
    name,
    label,
    mode,
    config
) VALUES (
    sqlc.arg(id),
    sqlc.arg(org_id),
    sqlc.arg(name),
    sqlc.arg(label),
    sqlc.arg(mode),
    sqlc.arg(config)
)
RETURNING *;

-- name: UpdateWaitpointPolicy :one
UPDATE waitpoint_policies
   SET label = sqlc.arg(label),
       mode = sqlc.arg(mode),
       config = sqlc.arg(config)
 WHERE org_id = sqlc.arg(org_id)
   AND name = sqlc.arg(name)
   AND disabled_at IS NULL
RETURNING *;

-- name: DisableWaitpointPolicy :execrows
UPDATE waitpoint_policies
   SET disabled_at = now()
 WHERE org_id = sqlc.arg(org_id)
   AND name = sqlc.arg(name)
   AND disabled_at IS NULL;

-- name: GetWaitpointPolicyByName :one
SELECT *
  FROM waitpoint_policies
 WHERE org_id = sqlc.arg(org_id)
   AND name = sqlc.arg(name)
   AND disabled_at IS NULL;

-- name: ListWaitpointPolicies :many
SELECT *
  FROM waitpoint_policies
 WHERE org_id = sqlc.arg(org_id)
   AND disabled_at IS NULL
 ORDER BY lower(name), created_at ASC;
