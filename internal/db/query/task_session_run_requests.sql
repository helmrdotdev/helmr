-- name: EnsureTaskSessionRunRequestForStreamRecord :one
INSERT INTO task_session_run_requests (
    id,
    org_id,
    project_id,
    environment_id,
    task_session_id,
    stream_record_id,
    stream_id,
    cause_kind
) VALUES (
    sqlc.arg(id),
    sqlc.arg(org_id),
    sqlc.arg(project_id),
    sqlc.arg(environment_id),
    sqlc.arg(task_session_id),
    sqlc.arg(stream_record_id),
    sqlc.arg(stream_id),
    'stream_record'
)
ON CONFLICT (org_id, project_id, environment_id, stream_record_id)
DO UPDATE SET updated_at = task_session_run_requests.updated_at
RETURNING *;

-- name: ClaimDueTaskSessionRunRequests :many
WITH eligible AS (
    SELECT id
      FROM task_session_run_requests
     WHERE status IN ('accepted', 'claimed')
       AND (
           status = 'accepted'
           OR claim_expires_at IS NULL
           OR claim_expires_at <= now()
       )
       AND next_attempt_at <= now()
       AND (
           sqlc.narg(org_id)::uuid IS NULL
           OR org_id = sqlc.narg(org_id)
       )
       AND (
           sqlc.narg(project_id)::uuid IS NULL
           OR project_id = sqlc.narg(project_id)
       )
       AND (
           sqlc.narg(environment_id)::uuid IS NULL
           OR environment_id = sqlc.narg(environment_id)
       )
       AND (
           sqlc.narg(task_session_id)::uuid IS NULL
           OR task_session_id = sqlc.narg(task_session_id)
       )
     ORDER BY next_attempt_at ASC, created_at ASC, id ASC
     LIMIT sqlc.arg(limit_count)
     FOR UPDATE SKIP LOCKED
)
UPDATE task_session_run_requests
   SET status = 'claimed',
       attempts = attempts + 1,
       claimed_at = now(),
       claim_expires_at = now() + sqlc.arg(claim_ttl)::interval,
       claim_owner = sqlc.arg(claim_owner),
       updated_at = now()
  FROM eligible
 WHERE task_session_run_requests.id = eligible.id
RETURNING task_session_run_requests.*;

-- name: ReleaseTaskSessionRunRequestForRetry :one
UPDATE task_session_run_requests
   SET status = 'accepted',
       next_attempt_at = now() + sqlc.arg(retry_after)::interval,
       last_error = sqlc.arg(last_error),
       error_message = sqlc.arg(last_error),
       claimed_at = NULL,
       claim_expires_at = NULL,
       claim_owner = '',
       updated_at = now()
 WHERE org_id = sqlc.arg(org_id)
	   AND project_id = sqlc.arg(project_id)
	   AND environment_id = sqlc.arg(environment_id)
	   AND id = sqlc.arg(id)
	   AND status = 'claimed'
	   AND claim_owner = sqlc.arg(claim_owner)
	RETURNING *;

-- name: MarkTaskSessionRunRequestCreated :one
UPDATE task_session_run_requests
   SET status = 'created',
       run_id = sqlc.arg(run_id),
       last_error = '',
       error_message = '',
       claimed_at = NULL,
       claim_expires_at = NULL,
       claim_owner = '',
       updated_at = now()
 WHERE org_id = sqlc.arg(org_id)
	   AND project_id = sqlc.arg(project_id)
	   AND environment_id = sqlc.arg(environment_id)
	   AND id = sqlc.arg(id)
	   AND status = 'claimed'
	   AND claim_owner = sqlc.arg(claim_owner)
	RETURNING *;

-- name: MarkTaskSessionRunRequestSkipped :one
UPDATE task_session_run_requests
   SET status = 'skipped',
       last_error = sqlc.arg(reason),
       error_message = sqlc.arg(reason),
       claimed_at = NULL,
       claim_expires_at = NULL,
       claim_owner = '',
       updated_at = now()
 WHERE org_id = sqlc.arg(org_id)
	   AND project_id = sqlc.arg(project_id)
	   AND environment_id = sqlc.arg(environment_id)
	   AND id = sqlc.arg(id)
	   AND status = 'claimed'
	   AND claim_owner = sqlc.arg(claim_owner)
	RETURNING *;

-- name: MarkTaskSessionRunRequestFailed :one
UPDATE task_session_run_requests
   SET status = 'failed',
       last_error = sqlc.arg(reason),
       error_message = sqlc.arg(reason),
       claimed_at = NULL,
       claim_expires_at = NULL,
       claim_owner = '',
       updated_at = now()
 WHERE org_id = sqlc.arg(org_id)
	   AND project_id = sqlc.arg(project_id)
	   AND environment_id = sqlc.arg(environment_id)
	   AND id = sqlc.arg(id)
	   AND status = 'claimed'
	   AND claim_owner = sqlc.arg(claim_owner)
	RETURNING *;
