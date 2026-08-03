-- name: CreateDeletionJob :one
INSERT INTO deletion_jobs (
    id,
    org_id,
    target_type,
    target_id,
    target_project_id,
    target_slug,
    target_name,
    requested_by_principal
) VALUES (
    sqlc.arg(id),
    sqlc.arg(org_id),
    sqlc.arg(target_type),
    sqlc.arg(target_id),
    sqlc.narg(target_project_id),
    sqlc.arg(target_slug),
    sqlc.arg(target_name),
    sqlc.arg(requested_by_principal)
)
RETURNING *;

-- name: MarkDeletionJobRunning :one
UPDATE deletion_jobs
   SET status = 'running',
       started_at = COALESCE(started_at, now()),
       failure = '',
       updated_at = now()
 WHERE org_id = sqlc.arg(org_id)
   AND id = sqlc.arg(id)
RETURNING *;

-- name: CompleteDeletionJob :one
UPDATE deletion_jobs
   SET status = 'completed',
       completed_at = now(),
       failure = '',
       deleted_counts = sqlc.arg(deleted_counts),
       updated_at = now()
 WHERE org_id = sqlc.arg(org_id)
   AND id = sqlc.arg(id)
RETURNING *;

-- name: FailDeletionJob :one
UPDATE deletion_jobs
   SET status = 'failed',
       failure = sqlc.arg(failure),
       updated_at = now()
 WHERE org_id = sqlc.arg(org_id)
   AND id = sqlc.arg(id)
RETURNING *;

-- name: ListDueEnvironmentImageCacheRetirements :many
SELECT id, target_id
  FROM deletion_jobs
 WHERE target_type = 'environment'
   AND status = 'completed'
   AND completed_at IS NOT NULL
   AND completed_at <= transaction_timestamp() - INTERVAL '7 days'
   AND deleted_counts -> 'image_cache_repositories' IS DISTINCT FROM '1'::jsonb
 ORDER BY completed_at, id
 LIMIT sqlc.arg(result_limit);

-- name: MarkEnvironmentImageCacheRetired :execrows
UPDATE deletion_jobs
   SET deleted_counts = jsonb_set(
           deleted_counts,
           '{image_cache_repositories}',
           '1'::jsonb,
           true
       ),
       updated_at = now()
 WHERE id = sqlc.arg(id)
   AND target_type = 'environment'
   AND target_id = sqlc.arg(environment_id)
   AND status = 'completed'
   AND deleted_counts -> 'image_cache_repositories' IS DISTINCT FROM '1'::jsonb;
