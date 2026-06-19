-- name: CreatePublicAccessToken :one
INSERT INTO public_access_tokens (
    id,
    org_id,
    project_id,
    environment_id,
    token_hash,
    allowed_scopes,
    expires_at,
    max_uses,
    metadata,
    created_by
)
VALUES (
    sqlc.arg(id),
    sqlc.arg(org_id),
    sqlc.arg(project_id),
    sqlc.arg(environment_id),
    sqlc.arg(token_hash),
    sqlc.arg(allowed_scopes)::jsonb,
    sqlc.arg(expires_at),
    sqlc.narg(max_uses)::integer,
    COALESCE(sqlc.arg(metadata)::jsonb, '{}'::jsonb),
    COALESCE(sqlc.arg(created_by)::jsonb, '{}'::jsonb)
)
RETURNING *;

-- name: LockPublicAccessTokenByHash :one
SELECT *
  FROM public_access_tokens
 WHERE token_hash = sqlc.arg(token_hash)
 FOR UPDATE;

-- name: ConsumePublicAccessToken :one
UPDATE public_access_tokens
   SET used_count = used_count + 1,
       last_used_at = now()
 WHERE org_id = sqlc.arg(org_id)
   AND id = sqlc.arg(id)
   AND revoked_at IS NULL
   AND expires_at > now()
   AND (max_uses IS NULL OR used_count < max_uses)
RETURNING *;

-- name: GetPublicAccessToken :one
SELECT *
  FROM public_access_tokens
 WHERE org_id = sqlc.arg(org_id)
   AND id = sqlc.arg(id);
