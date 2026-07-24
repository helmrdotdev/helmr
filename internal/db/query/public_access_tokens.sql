-- name: CreatePublicAccessToken :one
INSERT INTO public_access_tokens (
    id,
    public_id,
    org_id,
    project_id,
    environment_id,
    token_hash,
    credential_key_id,
    expires_at,
    max_uses,
    metadata,
    created_by
)
VALUES (
    sqlc.arg(id),
    sqlc.arg(public_id),
    sqlc.arg(org_id),
    sqlc.arg(project_id),
    sqlc.arg(environment_id),
    sqlc.arg(token_hash),
    sqlc.arg(credential_key_id),
    sqlc.arg(expires_at)::timestamptz,
    sqlc.narg(max_uses)::integer,
    COALESCE(sqlc.arg(metadata)::jsonb, '{}'::jsonb),
    COALESCE(sqlc.arg(created_by)::jsonb, '{}'::jsonb)
)
RETURNING *;

-- name: CreatePublicAccessTokenScope :one
INSERT INTO public_access_token_scopes (
    id,
    org_id,
    project_id,
    environment_id,
    public_access_token_id,
    scope_type,
    token_id
)
SELECT sqlc.arg(id),
       sqlc.arg(org_id),
       sqlc.arg(project_id),
       sqlc.arg(environment_id),
       public_access_tokens.id,
       'token.complete',
       tokens.id
 FROM public_access_tokens
 JOIN tokens
   ON tokens.org_id = public_access_tokens.org_id
  AND tokens.project_id = public_access_tokens.project_id
  AND tokens.environment_id = public_access_tokens.environment_id
  AND tokens.id = sqlc.arg(token_id)
 WHERE public_access_tokens.org_id = sqlc.arg(org_id)
   AND public_access_tokens.project_id = sqlc.arg(project_id)
   AND public_access_tokens.environment_id = sqlc.arg(environment_id)
   AND public_access_tokens.id = sqlc.arg(public_access_token_id)
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
 WHERE org_id = sqlc.arg(org_id)
   AND id = sqlc.arg(id)
   AND state = 'active'
   AND expires_at > now()
   AND (max_uses IS NULL OR used_count < max_uses)
RETURNING *;

-- name: GetPublicAccessToken :one
SELECT *
 FROM public_access_tokens
 WHERE org_id = sqlc.arg(org_id)
   AND id = sqlc.arg(id);

-- name: GetPublicAccessTokenForToken :one
SELECT public_access_tokens.*
  FROM public_access_tokens
  JOIN public_access_token_scopes
    ON public_access_token_scopes.public_access_token_id = public_access_tokens.id
   AND public_access_token_scopes.org_id = public_access_tokens.org_id
   AND public_access_token_scopes.project_id = public_access_tokens.project_id
   AND public_access_token_scopes.environment_id = public_access_tokens.environment_id
 WHERE public_access_token_scopes.token_id = sqlc.arg(token_id)
   AND public_access_token_scopes.scope_type = 'token.complete';

-- name: ListPublicAccessTokenScopes :many
SELECT *
 FROM public_access_token_scopes
 WHERE org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND public_access_token_id = sqlc.arg(public_access_token_id)
 ORDER BY created_at ASC, id ASC;

-- name: GetPublicAccessTokenTokenScope :one
SELECT public_access_token_scopes.*
  FROM public_access_token_scopes
  JOIN public_access_tokens
    ON public_access_tokens.org_id = public_access_token_scopes.org_id
   AND public_access_tokens.project_id = public_access_token_scopes.project_id
   AND public_access_tokens.environment_id = public_access_token_scopes.environment_id
   AND public_access_tokens.id = public_access_token_scopes.public_access_token_id
 WHERE public_access_token_scopes.org_id = sqlc.arg(org_id)
   AND public_access_token_scopes.project_id = sqlc.arg(project_id)
   AND public_access_token_scopes.environment_id = sqlc.arg(environment_id)
   AND public_access_token_scopes.public_access_token_id = sqlc.arg(public_access_token_id)
   AND public_access_token_scopes.scope_type = 'token.complete'
   AND public_access_token_scopes.token_id = sqlc.arg(token_id)
   AND public_access_tokens.state = 'active'
   AND public_access_tokens.expires_at > now();

-- name: RevokePublicAccessToken :one
UPDATE public_access_tokens
   SET state = 'revoked',
       revoked_at = now(),
       updated_at = now()
 WHERE org_id = sqlc.arg(org_id)
   AND id = sqlc.arg(id)
   AND state = 'active'
RETURNING *;

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
