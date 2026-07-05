-- name: UpsertDeploymentStream :one
INSERT INTO deployment_streams (
    id,
    org_id,
    worker_group_id,
    project_id,
    environment_id,
    deployment_id,
    name,
    direction,
    schema_fingerprint,
    schema_json,
    metadata
)
VALUES (
    sqlc.arg(id),
    sqlc.arg(org_id),
    sqlc.arg(worker_group_id),
    sqlc.arg(project_id),
    sqlc.arg(environment_id),
    sqlc.arg(deployment_id),
    sqlc.arg(name),
    sqlc.arg(direction)::stream_direction,
    COALESCE(sqlc.arg(schema_fingerprint)::text, ''),
    COALESCE(sqlc.arg(schema_json)::jsonb, 'null'::jsonb),
    COALESCE(sqlc.arg(metadata)::jsonb, '{}'::jsonb)
)
ON CONFLICT (org_id, worker_group_id, deployment_id, name, direction)
DO UPDATE SET
    schema_fingerprint = EXCLUDED.schema_fingerprint,
    schema_json = EXCLUDED.schema_json,
    metadata = EXCLUDED.metadata
RETURNING *;

-- name: GetDeploymentStreamByName :one
SELECT *
  FROM deployment_streams
 WHERE org_id = sqlc.arg(org_id)
   AND worker_group_id = sqlc.arg(worker_group_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND deployment_id = sqlc.arg(deployment_id)
   AND name = sqlc.arg(name)
   AND direction = sqlc.arg(direction)::stream_direction;

-- name: ListDeploymentStreamsForDeployment :many
SELECT *
  FROM deployment_streams
 WHERE org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND deployment_id = sqlc.arg(deployment_id)
 ORDER BY name ASC, direction ASC;
