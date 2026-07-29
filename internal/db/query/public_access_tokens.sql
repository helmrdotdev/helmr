-- name: CreatePublicAccessToken :one
INSERT INTO public_access_tokens (
    id,
    public_id,
    token_id,
    token_hash,
    expires_at,
    max_uses,
    metadata,
    created_by
)
VALUES (
    sqlc.arg(id),
    sqlc.arg(public_id),
    sqlc.arg(token_id),
    sqlc.arg(token_hash),
    sqlc.arg(expires_at)::timestamptz,
    sqlc.narg(max_uses)::integer,
    COALESCE(sqlc.arg(metadata)::jsonb, '{}'::jsonb),
    COALESCE(sqlc.arg(created_by)::jsonb, '{}'::jsonb)
)
RETURNING *;

-- name: LockPublicAccessTokenByHash :one
SELECT *
  FROM public_access_tokens
 WHERE token_hash = sqlc.arg(token_hash)
   AND state = 'active'
   AND expires_at > transaction_timestamp()
 FOR UPDATE;

-- name: MarkPublicAccessTokenUsed :one
UPDATE public_access_tokens
   SET used_count = used_count + 1,
       last_used_at = now(),
       updated_at = now()
 WHERE id = sqlc.arg(id)
   AND state = 'active'
   AND expires_at > now()
   AND (max_uses IS NULL OR used_count < max_uses)
RETURNING *;

-- name: GetPublicAccessTokenForToken :one
SELECT public_access_tokens.*
  FROM public_access_tokens
 WHERE public_access_tokens.token_id = sqlc.arg(token_id);

-- name: ExpireDuePublicAccessTokens :many
WITH candidates AS MATERIALIZED (
    SELECT id
      FROM public_access_tokens
     WHERE state = 'active'
       AND expires_at <= transaction_timestamp()
     ORDER BY expires_at, id
     FOR UPDATE SKIP LOCKED
     LIMIT sqlc.arg(limit_count)
)
UPDATE public_access_tokens
   SET state = 'expired',
       expired_at = transaction_timestamp(),
       updated_at = transaction_timestamp()
  FROM candidates
 WHERE public_access_tokens.id = candidates.id
   AND public_access_tokens.state = 'active'
RETURNING public_access_tokens.*;
