-- name: CreateRunStream :one
INSERT INTO run_streams (
    id,
    public_id,
    org_id,
    project_id,
    environment_id,
    run_id,
    deployment_id,
    deployment_definition_id,
    declaration_kind,
    stream_declared_id
)
SELECT sqlc.arg(id),
       sqlc.arg(public_id),
       runs.org_id,
       runs.project_id,
       runs.environment_id,
       runs.id,
       runs.deployment_id,
       deployment_definitions.id,
       deployment_definitions.kind,
       deployment_definitions.declared_id
  FROM runs
  JOIN deployment_definitions
    ON deployment_definitions.environment_id = runs.environment_id
   AND deployment_definitions.deployment_id = runs.deployment_id
   AND deployment_definitions.id = sqlc.arg(deployment_definition_id)
   AND deployment_definitions.kind = 'run_stream'
   AND deployment_definitions.declared_id = sqlc.arg(stream_declared_id)
 WHERE runs.org_id = sqlc.arg(org_id)
   AND runs.project_id = sqlc.arg(project_id)
   AND runs.environment_id = sqlc.arg(environment_id)
   AND runs.id = sqlc.arg(run_id)
ON CONFLICT (run_id, deployment_definition_id)
DO UPDATE SET updated_at = run_streams.updated_at
RETURNING *;

-- name: GetRunStream :one
SELECT *
  FROM run_streams
 WHERE org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND run_id = sqlc.arg(run_id)
   AND id = sqlc.arg(id);

-- name: ListRunStreams :many
SELECT *
  FROM run_streams
 WHERE org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND run_id = sqlc.arg(run_id)
 ORDER BY stream_declared_id, id;
