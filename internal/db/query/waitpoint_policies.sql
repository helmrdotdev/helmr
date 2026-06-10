-- name: CreateWaitpointPolicy :one
INSERT INTO waitpoint_policies (
    id,
    org_id,
    project_id,
    environment_id,
    name,
    label,
    config
) VALUES (
    sqlc.arg(id),
    sqlc.arg(org_id),
    sqlc.arg(project_id),
    sqlc.arg(environment_id),
    sqlc.arg(name),
    sqlc.arg(label),
    sqlc.arg(config)
)
RETURNING *;

-- name: UpdateWaitpointPolicy :one
UPDATE waitpoint_policies
   SET label = sqlc.arg(label),
       config = sqlc.arg(config)
 WHERE org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND name = sqlc.arg(name)
RETURNING *;

-- name: DeleteWaitpointPolicy :execrows
DELETE FROM waitpoint_policies
 WHERE org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND name = sqlc.arg(name);

-- name: GetWaitpointPolicyByName :one
SELECT *
 FROM waitpoint_policies
 WHERE org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND name = sqlc.arg(name);

-- name: ListWaitpointPolicies :many
SELECT *
 FROM waitpoint_policies
 WHERE org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
 ORDER BY lower(name), created_at ASC;
