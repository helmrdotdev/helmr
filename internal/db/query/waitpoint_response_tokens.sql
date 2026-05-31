-- name: GetWaitpointForResponseTokenCreation :one
SELECT id,
       org_id,
       project_id,
       environment_id,
       kind,
       status,
       display_text
  FROM waitpoints
 WHERE waitpoints.org_id = sqlc.arg(org_id)
       AND waitpoints.id = sqlc.arg(waitpoint_id)
       AND waitpoints.status = 'pending'
       AND (waitpoints.expires_at IS NULL OR waitpoints.expires_at > now());

-- name: CreateWaitpointResponseToken :one
WITH target_waitpoint AS (
    SELECT *
      FROM waitpoints
     WHERE waitpoints.org_id = sqlc.arg(org_id)
       AND waitpoints.id = sqlc.arg(waitpoint_id)
       AND waitpoints.status = 'pending'
       AND (waitpoints.expires_at IS NULL OR waitpoints.expires_at > now())
)
INSERT INTO waitpoint_response_tokens (
    id,
    org_id,
    project_id,
    environment_id,
    waitpoint_id,
    token_hash,
    expires_at,
    external_subject,
    metadata
)
SELECT
    sqlc.arg(id),
    target_waitpoint.org_id,
    target_waitpoint.project_id,
    target_waitpoint.environment_id,
    target_waitpoint.id,
    sqlc.arg(token_hash),
    sqlc.arg(expires_at),
    sqlc.narg(external_subject),
    sqlc.arg(metadata)
  FROM target_waitpoint
RETURNING *;

-- name: GetWaitpointResponseTokenForRespond :one
SELECT
    waitpoint_response_tokens.*,
    waitpoints.kind AS waitpoint_kind,
    waitpoints.display_text AS waitpoint_display_text
  FROM waitpoint_response_tokens
  JOIN waitpoints ON waitpoints.org_id = waitpoint_response_tokens.org_id
                 AND waitpoints.id = waitpoint_response_tokens.waitpoint_id
 WHERE waitpoint_response_tokens.id = sqlc.arg(id)
   AND waitpoint_response_tokens.token_hash = sqlc.arg(token_hash)
   AND waitpoint_response_tokens.status IN ('pending', 'completed')
   AND waitpoint_response_tokens.expires_at > now()
   AND waitpoints.status IN ('pending', 'completed')
   AND (waitpoints.expires_at IS NULL OR waitpoints.expires_at > now())
 FOR UPDATE OF waitpoint_response_tokens, waitpoints;

-- name: MarkWaitpointResponseTokenCompleted :one
UPDATE waitpoint_response_tokens
   SET status = 'completed',
       completed_at = COALESCE(completed_at, now()),
       completed_by_principal = COALESCE(completed_by_principal, sqlc.arg(completed_by_principal)),
       completed_via = COALESCE(completed_via, sqlc.arg(completed_via)),
       external_subject = COALESCE(sqlc.narg(external_subject), external_subject),
       metadata = metadata || sqlc.arg(metadata)::jsonb
 WHERE waitpoint_response_tokens.org_id = sqlc.arg(org_id)
   AND waitpoint_response_tokens.id = sqlc.arg(id)
   AND waitpoint_response_tokens.token_hash = sqlc.arg(token_hash)
   AND waitpoint_response_tokens.status = 'pending'
   AND waitpoint_response_tokens.expires_at > now()
RETURNING *;

-- name: RevokeWaitpointResponseToken :execrows
UPDATE waitpoint_response_tokens
   SET status = 'revoked'
 WHERE waitpoint_response_tokens.org_id = sqlc.arg(org_id)
   AND waitpoint_response_tokens.id = sqlc.arg(id)
   AND waitpoint_response_tokens.status = 'pending';
