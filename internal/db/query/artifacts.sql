-- name: CreateArtifact :one
INSERT INTO artifacts (
    id,
    org_id,
    cell_id,
    project_id,
    environment_id,
    digest,
    kind,
    size_bytes,
    media_type,
    created_by_worker_instance_id
) VALUES (
    sqlc.arg(id),
    sqlc.arg(org_id),
    sqlc.arg(cell_id),
    sqlc.arg(project_id),
    sqlc.arg(environment_id),
    sqlc.arg(digest),
    sqlc.arg(kind),
    sqlc.arg(size_bytes),
    sqlc.arg(media_type),
    sqlc.narg(created_by_worker_instance_id)
)
RETURNING *;

-- name: UpsertRuntimeSubstrateArtifactBlob :one
INSERT INTO artifacts (
    id,
    org_id,
    cell_id,
    project_id,
    environment_id,
    digest,
    kind,
    size_bytes,
    media_type,
    created_by_worker_instance_id
) VALUES (
    sqlc.arg(id),
    sqlc.arg(org_id),
    sqlc.arg(cell_id),
    sqlc.arg(project_id),
    sqlc.arg(environment_id),
    sqlc.arg(digest),
    'runtime_substrate',
    sqlc.arg(size_bytes),
    sqlc.arg(media_type),
    sqlc.narg(created_by_worker_instance_id)
)
ON CONFLICT (org_id, project_id, environment_id, digest, kind)
WHERE kind = 'runtime_substrate'
DO UPDATE
   SET created_by_worker_instance_id = COALESCE(artifacts.created_by_worker_instance_id, excluded.created_by_worker_instance_id)
RETURNING *;

-- name: GetArtifact :one
SELECT *
  FROM artifacts
 WHERE org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND id = sqlc.arg(id);

-- name: ListArtifactsByIDs :many
SELECT *
  FROM artifacts
 WHERE org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND id = ANY(sqlc.arg(ids)::uuid[]);
