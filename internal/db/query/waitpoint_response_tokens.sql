-- name: CreateWaitpointResponseToken :one
WITH target_waitpoint AS (
    SELECT waitpoints.*
      FROM waitpoints
      JOIN runs ON runs.org_id = waitpoints.org_id
               AND runs.id = waitpoints.run_id
     WHERE waitpoints.org_id = sqlc.arg(org_id)
       AND waitpoints.run_id = sqlc.arg(run_id)
       AND waitpoints.id = sqlc.arg(waitpoint_id)
       AND waitpoints.status = 'waiting'
       AND runs.status = 'waiting'
       AND runs.current_execution_id IS NULL
)
INSERT INTO waitpoint_response_tokens (
    id,
    org_id,
    run_id,
    waitpoint_id,
    token_hash,
    allowed_actions,
    expires_at,
    external_subject,
    metadata
)
SELECT
    sqlc.arg(id),
    target_waitpoint.org_id,
    target_waitpoint.run_id,
    target_waitpoint.id,
    sqlc.arg(token_hash),
    sqlc.arg(allowed_actions)::text[],
    sqlc.arg(expires_at),
    sqlc.narg(external_subject),
    sqlc.arg(metadata)
  FROM target_waitpoint
RETURNING *;

-- name: GetActiveWaitpointResponseToken :one
SELECT
    waitpoint_response_tokens.*,
    waitpoints.kind AS waitpoint_kind,
    waitpoints.display_text AS waitpoint_display_text
  FROM waitpoint_response_tokens
  JOIN waitpoints ON waitpoints.org_id = waitpoint_response_tokens.org_id
                 AND waitpoints.run_id = waitpoint_response_tokens.run_id
                 AND waitpoints.id = waitpoint_response_tokens.waitpoint_id
  JOIN runs ON runs.org_id = waitpoint_response_tokens.org_id
           AND runs.id = waitpoint_response_tokens.run_id
 WHERE waitpoint_response_tokens.id = sqlc.arg(id)
   AND waitpoint_response_tokens.token_hash = sqlc.arg(token_hash)
   AND waitpoint_response_tokens.status = 'pending'
   AND (waitpoint_response_tokens.expires_at IS NULL OR waitpoint_response_tokens.expires_at > now())
   AND waitpoints.status = 'waiting'
   AND runs.status = 'waiting'
   AND runs.current_execution_id IS NULL;

-- name: CompleteWaitpointResponseToken :one
WITH current_token AS (
    SELECT waitpoint_response_tokens.*
      FROM waitpoint_response_tokens
      JOIN waitpoints ON waitpoints.org_id = waitpoint_response_tokens.org_id
                     AND waitpoints.run_id = waitpoint_response_tokens.run_id
                     AND waitpoints.id = waitpoint_response_tokens.waitpoint_id
      JOIN runs ON runs.org_id = waitpoint_response_tokens.org_id
               AND runs.id = waitpoint_response_tokens.run_id
     WHERE waitpoint_response_tokens.id = sqlc.arg(id)
       AND waitpoint_response_tokens.token_hash = sqlc.arg(token_hash)
       AND waitpoint_response_tokens.status = 'pending'
       AND (waitpoint_response_tokens.expires_at IS NULL OR waitpoint_response_tokens.expires_at > now())
       AND (
           sqlc.arg(action)::text = ANY(waitpoint_response_tokens.allowed_actions)
           OR (
               waitpoints.kind = 'message'
               AND sqlc.arg(action)::text IN ('message', 'reply')
               AND waitpoint_response_tokens.allowed_actions && ARRAY['message', 'reply']::text[]
           )
       )
       AND waitpoints.kind = sqlc.arg(kind)
       AND waitpoints.status = 'waiting'
       AND runs.status = 'waiting'
       AND runs.current_execution_id IS NULL
     FOR UPDATE OF waitpoint_response_tokens, waitpoints, runs
),
completed_token AS (
    UPDATE waitpoint_response_tokens
       SET status = 'completed',
           completed_at = now(),
           completed_by_principal = sqlc.arg(completed_by_principal),
           completed_via = sqlc.arg(completed_via),
           external_subject = COALESCE(sqlc.narg(external_subject), waitpoint_response_tokens.external_subject),
           metadata = waitpoint_response_tokens.metadata || sqlc.arg(metadata)::jsonb
      FROM current_token
     WHERE waitpoint_response_tokens.id = current_token.id
       AND waitpoint_response_tokens.token_hash = current_token.token_hash
       AND waitpoint_response_tokens.status = 'pending'
    RETURNING waitpoint_response_tokens.*
)
SELECT completed_token.*
  FROM completed_token;

-- name: RevokeWaitpointResponseToken :execrows
UPDATE waitpoint_response_tokens
   SET status = 'revoked'
 WHERE org_id = sqlc.arg(org_id)
   AND id = sqlc.arg(id)
   AND status = 'pending';
