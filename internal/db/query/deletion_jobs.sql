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
       failure = ''
 WHERE org_id = sqlc.arg(org_id)
   AND id = sqlc.arg(id)
RETURNING *;

-- name: CompleteDeletionJob :one
UPDATE deletion_jobs
   SET status = 'completed',
       completed_at = now(),
       failure = '',
       deleted_counts = sqlc.arg(deleted_counts)
 WHERE org_id = sqlc.arg(org_id)
   AND id = sqlc.arg(id)
RETURNING *;

-- name: FailDeletionJob :one
UPDATE deletion_jobs
   SET status = 'failed',
       failure = sqlc.arg(failure)
 WHERE org_id = sqlc.arg(org_id)
   AND id = sqlc.arg(id)
RETURNING *;
