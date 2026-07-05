-- name: CreatePublicAccessToken :one
INSERT INTO public_access_tokens (
    id,
    org_id,
    cell_id,
    project_id,
    environment_id,
    token_hash,
    expires_at,
    max_uses,
    metadata,
    created_by
)
VALUES (
    sqlc.arg(id),
    sqlc.arg(org_id),
    sqlc.arg(cell_id),
    sqlc.arg(project_id),
    sqlc.arg(environment_id),
    sqlc.arg(token_hash),
    sqlc.arg(expires_at),
    sqlc.narg(max_uses)::integer,
    COALESCE(sqlc.arg(metadata)::jsonb, '{}'::jsonb),
    COALESCE(sqlc.arg(created_by)::jsonb, '{}'::jsonb)
)
RETURNING *;

-- name: CreatePublicAccessTokenScope :one
INSERT INTO public_access_token_scopes (
    id,
    org_id,
    cell_id,
    project_id,
    environment_id,
    public_access_token_id,
    scope_type,
    token_id,
    stream_id,
    correlation_id
)
SELECT sqlc.arg(id),
       sqlc.arg(org_id),
       sqlc.arg(cell_id),
       sqlc.arg(project_id),
       sqlc.arg(environment_id),
       public_access_tokens.id,
       sqlc.arg(scope_type)::public_access_token_scope_type,
       sqlc.narg(token_id)::uuid,
       sqlc.narg(stream_id)::uuid,
       CASE
           WHEN sqlc.arg(scope_type)::public_access_token_scope_type = 'token.complete' THEN ''
           ELSE COALESCE(sqlc.narg(correlation_id)::text, '')
       END
 FROM public_access_tokens
 WHERE public_access_tokens.org_id = sqlc.arg(org_id)
   AND public_access_tokens.cell_id = sqlc.arg(cell_id)
   AND public_access_tokens.project_id = sqlc.arg(project_id)
   AND public_access_tokens.environment_id = sqlc.arg(environment_id)
   AND public_access_tokens.id = sqlc.arg(public_access_token_id)
   AND (
       (
           sqlc.arg(scope_type)::public_access_token_scope_type = 'token.complete'
           AND sqlc.narg(token_id)::uuid IS NOT NULL
           AND sqlc.narg(stream_id)::uuid IS NULL
           AND EXISTS (
               SELECT 1
                 FROM tokens
                WHERE tokens.org_id = sqlc.arg(org_id)
                  AND tokens.cell_id = sqlc.arg(cell_id)
                  AND tokens.project_id = sqlc.arg(project_id)
                  AND tokens.environment_id = sqlc.arg(environment_id)
                  AND tokens.id = sqlc.narg(token_id)::uuid
           )
       )
       OR (
           sqlc.arg(scope_type)::public_access_token_scope_type = 'session.input.send'
           AND sqlc.narg(token_id)::uuid IS NULL
           AND sqlc.narg(stream_id)::uuid IS NOT NULL
           AND EXISTS (
               SELECT 1
                 FROM streams
                WHERE streams.org_id = sqlc.arg(org_id)
                  AND streams.cell_id = sqlc.arg(cell_id)
                  AND streams.project_id = sqlc.arg(project_id)
                  AND streams.environment_id = sqlc.arg(environment_id)
                  AND streams.id = sqlc.narg(stream_id)::uuid
                  AND streams.direction = 'input'
           )
       )
       OR (
           sqlc.arg(scope_type)::public_access_token_scope_type = 'session.output.read'
           AND sqlc.narg(token_id)::uuid IS NULL
           AND sqlc.narg(stream_id)::uuid IS NOT NULL
           AND EXISTS (
               SELECT 1
                 FROM streams
                WHERE streams.org_id = sqlc.arg(org_id)
                  AND streams.cell_id = sqlc.arg(cell_id)
                  AND streams.project_id = sqlc.arg(project_id)
                  AND streams.environment_id = sqlc.arg(environment_id)
                  AND streams.id = sqlc.narg(stream_id)::uuid
                  AND streams.direction = 'output'
           )
       )
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
       last_used_at = now(),
       updated_at = now()
 WHERE org_id = sqlc.arg(org_id)
   AND cell_id = sqlc.arg(cell_id)
   AND id = sqlc.arg(id)
   AND state = 'active'
   AND expires_at > now()
   AND (max_uses IS NULL OR used_count < max_uses)
RETURNING *;

-- name: GetPublicAccessToken :one
SELECT *
 FROM public_access_tokens
 WHERE org_id = sqlc.arg(org_id)
   AND cell_id = sqlc.arg(cell_id)
   AND id = sqlc.arg(id);

-- name: ListPublicAccessTokenScopes :many
SELECT *
 FROM public_access_token_scopes
 WHERE org_id = sqlc.arg(org_id)
   AND cell_id = sqlc.arg(cell_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND public_access_token_id = sqlc.arg(public_access_token_id)
 ORDER BY created_at ASC, id ASC;

-- name: GetPublicAccessTokenTokenScope :one
SELECT public_access_token_scopes.*
  FROM public_access_token_scopes
  JOIN public_access_tokens
    ON public_access_tokens.org_id = public_access_token_scopes.org_id
   AND public_access_tokens.cell_id = public_access_token_scopes.cell_id
   AND public_access_tokens.project_id = public_access_token_scopes.project_id
   AND public_access_tokens.environment_id = public_access_token_scopes.environment_id
   AND public_access_tokens.id = public_access_token_scopes.public_access_token_id
 WHERE public_access_token_scopes.org_id = sqlc.arg(org_id)
   AND public_access_token_scopes.cell_id = sqlc.arg(cell_id)
   AND public_access_token_scopes.project_id = sqlc.arg(project_id)
   AND public_access_token_scopes.environment_id = sqlc.arg(environment_id)
   AND public_access_token_scopes.public_access_token_id = sqlc.arg(public_access_token_id)
   AND public_access_token_scopes.scope_type = 'token.complete'
   AND public_access_token_scopes.token_id = sqlc.arg(token_id)
   AND public_access_tokens.state = 'active'
   AND public_access_tokens.expires_at > now();

-- name: GetPublicAccessTokenStreamScope :one
SELECT public_access_token_scopes.*
  FROM public_access_token_scopes
  JOIN public_access_tokens
    ON public_access_tokens.org_id = public_access_token_scopes.org_id
   AND public_access_tokens.cell_id = public_access_token_scopes.cell_id
   AND public_access_tokens.project_id = public_access_token_scopes.project_id
   AND public_access_tokens.environment_id = public_access_token_scopes.environment_id
   AND public_access_tokens.id = public_access_token_scopes.public_access_token_id
 WHERE public_access_token_scopes.org_id = sqlc.arg(org_id)
   AND public_access_token_scopes.cell_id = sqlc.arg(cell_id)
   AND public_access_token_scopes.project_id = sqlc.arg(project_id)
   AND public_access_token_scopes.environment_id = sqlc.arg(environment_id)
   AND public_access_token_scopes.public_access_token_id = sqlc.arg(public_access_token_id)
   AND public_access_token_scopes.scope_type = sqlc.arg(scope_type)::public_access_token_scope_type
   AND public_access_token_scopes.stream_id = sqlc.arg(stream_id)
   AND (
       public_access_token_scopes.correlation_id = ''
       OR public_access_token_scopes.correlation_id = COALESCE(sqlc.narg(correlation_id)::text, '')
   )
   AND public_access_tokens.state = 'active'
   AND public_access_tokens.expires_at > now();

-- name: RevokePublicAccessToken :one
UPDATE public_access_tokens
   SET state = 'revoked',
       revoked_at = now(),
       updated_at = now()
 WHERE org_id = sqlc.arg(org_id)
   AND cell_id = sqlc.arg(cell_id)
   AND id = sqlc.arg(id)
   AND state = 'active'
RETURNING *;
