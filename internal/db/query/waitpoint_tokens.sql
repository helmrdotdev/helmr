-- name: CreateWaitpointToken :one
INSERT INTO waitpoint_tokens (
    id,
    org_id,
    project_id,
    environment_id,
    callback_secret_hash,
    timeout_at,
    tags,
    metadata
)
VALUES (
    sqlc.arg(id),
    sqlc.arg(org_id),
    sqlc.arg(project_id),
    sqlc.arg(environment_id),
    sqlc.arg(callback_secret_hash),
    sqlc.arg(timeout_at),
    COALESCE(sqlc.arg(tags)::text[], '{}'::text[]),
    COALESCE(sqlc.arg(metadata)::jsonb, '{}'::jsonb)
)
RETURNING *;

-- name: GetWaitpointToken :one
SELECT *
  FROM waitpoint_tokens
 WHERE waitpoint_tokens.org_id = sqlc.arg(org_id)
   AND waitpoint_tokens.project_id = sqlc.arg(project_id)
   AND waitpoint_tokens.environment_id = sqlc.arg(environment_id)
   AND waitpoint_tokens.id = sqlc.arg(id);

-- name: ListWaitpointTokens :many
WITH cursor_token AS (
    SELECT created_at, id
      FROM waitpoint_tokens
     WHERE waitpoint_tokens.org_id = sqlc.arg(org_id)
       AND waitpoint_tokens.project_id = sqlc.arg(project_id)
       AND waitpoint_tokens.environment_id = sqlc.arg(environment_id)
       AND waitpoint_tokens.id = sqlc.narg(after_id)::uuid
)
SELECT *
  FROM waitpoint_tokens
 WHERE waitpoint_tokens.org_id = sqlc.arg(org_id)
   AND waitpoint_tokens.project_id = sqlc.arg(project_id)
   AND waitpoint_tokens.environment_id = sqlc.arg(environment_id)
   AND (
       sqlc.narg(status)::text IS NULL
       OR waitpoint_tokens.status = sqlc.narg(status)::waitpoint_token_status
   )
   AND (
       sqlc.narg(after_id)::uuid IS NULL
       OR (waitpoint_tokens.created_at, waitpoint_tokens.id) > (SELECT cursor_token.created_at, cursor_token.id FROM cursor_token)
   )
 ORDER BY waitpoint_tokens.created_at ASC, waitpoint_tokens.id ASC
 LIMIT sqlc.arg(limit_count);

-- name: GetWaitpointTokenForCallbackCompletion :one
SELECT *
  FROM waitpoint_tokens
 WHERE waitpoint_tokens.id = sqlc.arg(id)
   AND waitpoint_tokens.callback_secret_hash = sqlc.arg(callback_secret_hash)
   AND waitpoint_tokens.status IN ('waiting', 'completed')
 FOR UPDATE;

-- name: GetWaitpointTokenForAuthenticatedCompletion :one
SELECT *
  FROM waitpoint_tokens
 WHERE waitpoint_tokens.org_id = sqlc.arg(org_id)
   AND waitpoint_tokens.id = sqlc.arg(id)
   AND waitpoint_tokens.status IN ('waiting', 'completed');

-- name: GetWaitpointTokenForPublicCompletion :one
SELECT *
  FROM waitpoint_tokens
 WHERE waitpoint_tokens.org_id = sqlc.arg(org_id)
   AND waitpoint_tokens.project_id = sqlc.arg(project_id)
   AND waitpoint_tokens.environment_id = sqlc.arg(environment_id)
   AND waitpoint_tokens.id = sqlc.arg(id)
   AND waitpoint_tokens.status IN ('waiting', 'completed')
 FOR UPDATE;

-- name: AttachWaitpointTokenToWaitpoint :one
WITH target_waitpoint AS (
    SELECT waitpoints.*
      FROM waitpoints
     WHERE waitpoints.org_id = sqlc.arg(org_id)
       AND waitpoints.id = sqlc.arg(waitpoint_id)
       AND waitpoints.status = 'pending'
     FOR UPDATE OF waitpoints
),
target_token AS (
    SELECT waitpoint_tokens.*
      FROM waitpoint_tokens
      JOIN target_waitpoint ON target_waitpoint.org_id = waitpoint_tokens.org_id
                           AND target_waitpoint.project_id = waitpoint_tokens.project_id
                           AND target_waitpoint.environment_id = waitpoint_tokens.environment_id
     WHERE waitpoint_tokens.id = sqlc.arg(token_id)
       AND waitpoint_tokens.status IN ('waiting', 'completed', 'timed_out')
       AND (
           target_waitpoint.waitpoint_token_id IS NULL
           OR target_waitpoint.waitpoint_token_id = waitpoint_tokens.id
       )
       AND NOT EXISTS (
           SELECT 1
             FROM waitpoints attached_waitpoints
            WHERE attached_waitpoints.org_id = waitpoint_tokens.org_id
              AND attached_waitpoints.waitpoint_token_id = waitpoint_tokens.id
              AND attached_waitpoints.id <> target_waitpoint.id
       )
     FOR UPDATE OF waitpoint_tokens
),
attached_waitpoint AS (
    UPDATE waitpoints
       SET waitpoint_token_id = target_token.id,
           status = CASE
               WHEN target_token.status = 'completed' THEN 'completed'::waitpoint_status
               WHEN target_token.status = 'timed_out' THEN 'timed_out'::waitpoint_status
               ELSE waitpoints.status
           END,
           data = CASE
               WHEN target_token.status = 'completed' THEN target_token.data
               WHEN target_token.status = 'timed_out' THEN NULL
               ELSE waitpoints.data
           END,
           error = CASE
               WHEN target_token.status = 'timed_out'
                   THEN COALESCE(target_token.error, jsonb_build_object('code', 'timed_out', 'at', to_jsonb(now())))
               WHEN target_token.status = 'completed' THEN NULL
               ELSE waitpoints.error
           END,
           resolved_at = CASE
               WHEN target_token.status IN ('completed', 'timed_out')
                   THEN COALESCE(target_token.completed_at, now())
               ELSE waitpoints.resolved_at
           END,
           updated_at = now()
      FROM target_token
     WHERE waitpoints.org_id = target_token.org_id
       AND waitpoints.id = sqlc.arg(waitpoint_id)
    RETURNING waitpoints.*
)
SELECT target_token.*,
       target_waitpoint.id AS waitpoint_id,
       (target_token.status IN ('completed', 'timed_out'))::boolean AS resolved_waitpoint
  FROM target_token
  JOIN target_waitpoint ON true;

-- name: CompleteWaitpointToken :one
WITH target AS (
    SELECT *
      FROM waitpoint_tokens
     WHERE waitpoint_tokens.org_id = sqlc.arg(org_id)
       AND waitpoint_tokens.id = sqlc.arg(id)
       AND waitpoint_tokens.status IN ('waiting', 'completed')
     FOR UPDATE
),
completed AS (
    UPDATE waitpoint_tokens
       SET status = 'completed',
           data = COALESCE(waitpoint_tokens.data, COALESCE(sqlc.arg(data)::jsonb, 'null'::jsonb)),
           completion_hash = COALESCE(waitpoint_tokens.completion_hash, sqlc.arg(completion_hash)),
           completed_at = COALESCE(waitpoint_tokens.completed_at, now()),
           updated_at = now()
      FROM target
     WHERE waitpoint_tokens.org_id = target.org_id
       AND waitpoint_tokens.id = target.id
       AND (
           target.status = 'waiting'
           OR target.completion_hash = sqlc.arg(completion_hash)
       )
       AND (
           target.status = 'completed'
           OR target.timeout_at > now()
       )
    RETURNING waitpoint_tokens.*
),
completed_waitpoint AS (
    UPDATE waitpoints
       SET status = 'completed',
           data = completed.data,
           error = NULL,
           resolved_at = COALESCE(completed.completed_at, now()),
           updated_at = now()
      FROM completed
     WHERE waitpoints.org_id = completed.org_id
       AND waitpoints.waitpoint_token_id = completed.id
       AND waitpoints.status = 'pending'
    RETURNING waitpoints.*
)
SELECT completed.*,
       (SELECT completed_waitpoint.id FROM completed_waitpoint LIMIT 1) AS waitpoint_id,
       EXISTS(SELECT 1 FROM completed_waitpoint)::boolean AS resolved_waitpoint
  FROM completed;

-- name: CancelWaitpointToken :execrows
UPDATE waitpoint_tokens
   SET status = 'cancelled',
       updated_at = now()
 WHERE waitpoint_tokens.org_id = sqlc.arg(org_id)
   AND waitpoint_tokens.id = sqlc.arg(id)
   AND waitpoint_tokens.status = 'waiting';
