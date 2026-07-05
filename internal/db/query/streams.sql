-- name: EnsureSessionStream :one
INSERT INTO streams (
    id,
    public_id,
    org_id,
    cell_id,
    project_id,
    environment_id,
    session_id,
    deployment_stream_id,
    name,
    direction,
    schema_fingerprint,
    metadata
)
SELECT sqlc.arg(id),
       sqlc.arg(public_id),
       sessions.org_id,
       sessions.cell_id,
       sessions.project_id,
       sessions.environment_id,
       sessions.id,
       deployment_streams.id,
       deployment_streams.name,
       deployment_streams.direction,
       deployment_streams.schema_fingerprint,
       COALESCE(sqlc.arg(metadata)::jsonb, '{}'::jsonb)
  FROM sessions
  JOIN deployment_streams
    ON deployment_streams.org_id = sessions.org_id
   AND deployment_streams.cell_id = sessions.cell_id
   AND deployment_streams.project_id = sessions.project_id
   AND deployment_streams.environment_id = sessions.environment_id
   AND deployment_streams.id = sqlc.arg(deployment_stream_id)
 WHERE sessions.org_id = sqlc.arg(org_id)
   AND sessions.cell_id = sqlc.arg(cell_id)
   AND sessions.project_id = sqlc.arg(project_id)
   AND sessions.environment_id = sqlc.arg(environment_id)
   AND sessions.id = sqlc.arg(session_id)
ON CONFLICT (org_id, cell_id, session_id, name, direction)
DO UPDATE SET
    deployment_stream_id = streams.deployment_stream_id,
    schema_fingerprint = streams.schema_fingerprint,
    metadata = streams.metadata
RETURNING *;

-- name: GetSessionStreamByName :one
SELECT *
 FROM streams
 WHERE org_id = sqlc.arg(org_id)
   AND cell_id = sqlc.arg(cell_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND session_id = sqlc.arg(session_id)
   AND name = sqlc.arg(name)
   AND direction = sqlc.arg(direction)::stream_direction;

-- name: GetStream :one
SELECT *
 FROM streams
 WHERE org_id = sqlc.arg(org_id)
   AND cell_id = sqlc.arg(cell_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND id = sqlc.arg(id);

-- name: ListSessionStreams :many
SELECT *
 FROM streams
 WHERE org_id = sqlc.arg(org_id)
   AND cell_id = sqlc.arg(cell_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND session_id = sqlc.arg(session_id)
 ORDER BY name ASC, direction ASC;
