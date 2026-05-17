-- name: IssueAPIKey :one
WITH revoked AS (
    UPDATE api_keys
       SET revoked_at = now()
     WHERE org_id = sqlc.arg(org_id)
       AND name = sqlc.arg(name)
       AND token_hash <> sqlc.arg(token_hash)
       AND revoked_at IS NULL
)
INSERT INTO api_keys (id, org_id, created_by_user_id, name, key_prefix, token_hash, expires_at)
VALUES (
    sqlc.arg(id),
    sqlc.arg(org_id),
    sqlc.arg(created_by_user_id),
    sqlc.arg(name),
    sqlc.arg(key_prefix),
    sqlc.arg(token_hash),
    sqlc.arg(expires_at)
)
ON CONFLICT (token_hash) DO UPDATE SET
    name = EXCLUDED.name,
    key_prefix = EXCLUDED.key_prefix,
    expires_at = EXCLUDED.expires_at,
    revoked_at = NULL
RETURNING *;

-- name: TouchActiveAPIKeyByTokenHash :one
WITH matched AS (
    UPDATE api_keys
       SET last_used_at = now()
     WHERE token_hash = $1
       AND revoked_at IS NULL
       AND (expires_at IS NULL OR expires_at > now())
     RETURNING *
)
SELECT
    matched.id,
    matched.org_id,
    matched.created_by_user_id,
    matched.name,
    matched.key_prefix,
    matched.created_at,
    matched.last_used_at,
    matched.expires_at,
    org_members.role::text AS role,
    convert_to(COALESCE(
        jsonb_agg(
            jsonb_build_object(
                'id', api_key_grants.id,
                'permission', api_key_grants.permission,
                'project_id', api_key_grants.project_id,
                'environment_id', api_key_grants.environment_id
            )
            ORDER BY api_key_grants.created_at, api_key_grants.id
        ) FILTER (WHERE api_key_grants.id IS NOT NULL),
        '[]'::jsonb
    )::text, 'UTF8') AS grants
  FROM matched
  JOIN org_members
    ON org_members.org_id = matched.org_id
   AND org_members.user_id = matched.created_by_user_id
   AND org_members.disabled_at IS NULL
  JOIN users
    ON users.id = org_members.user_id
   AND users.disabled_at IS NULL
  LEFT JOIN api_key_grants
    ON api_key_grants.org_id = matched.org_id
   AND api_key_grants.api_key_id = matched.id
 GROUP BY matched.id,
          matched.org_id,
          matched.created_by_user_id,
          matched.name,
          matched.key_prefix,
          matched.created_at,
          matched.last_used_at,
          matched.expires_at,
          org_members.role;

-- name: AuthorizeAPIKeyPermission :one
WITH matched AS (
    UPDATE api_keys
       SET last_used_at = now()
     WHERE api_keys.token_hash = sqlc.arg(token_hash)
       AND api_keys.org_id = sqlc.arg(org_id)
       AND api_keys.revoked_at IS NULL
       AND (api_keys.expires_at IS NULL OR api_keys.expires_at > now())
     RETURNING id, org_id
)
SELECT
    matched.id AS api_key_id,
    matched.org_id,
    api_key_grants.id AS grant_id,
    api_key_grants.permission,
    api_key_grants.project_id,
    api_key_grants.environment_id
  FROM matched
  JOIN api_key_grants
    ON api_key_grants.org_id = matched.org_id
   AND api_key_grants.api_key_id = matched.id
 WHERE api_key_grants.permission = sqlc.arg(permission)
   AND (
       (
           api_key_grants.project_id IS NULL
           AND api_key_grants.environment_id IS NULL
           AND sqlc.narg(project_id)::uuid IS NULL
           AND sqlc.narg(environment_id)::uuid IS NULL
       )
       OR (
           api_key_grants.project_id = sqlc.narg(project_id)
           AND api_key_grants.environment_id IS NULL
           AND sqlc.narg(environment_id)::uuid IS NULL
       )
       OR (
           api_key_grants.project_id = sqlc.narg(project_id)
           AND api_key_grants.environment_id = sqlc.narg(environment_id)
       )
   )
 ORDER BY
     api_key_grants.environment_id IS NOT NULL DESC,
     api_key_grants.project_id IS NOT NULL DESC,
     api_key_grants.created_at ASC
 LIMIT 1;

-- name: ListAPIKeys :many
SELECT id, org_id, created_by_user_id, name, key_prefix, created_at, last_used_at, expires_at, revoked_at
  FROM api_keys
 WHERE org_id = sqlc.arg(org_id)
   AND (
       sqlc.arg(status_filter)::text = 'all'
       OR (
           sqlc.arg(status_filter)::text = 'active'
           AND revoked_at IS NULL
           AND (expires_at IS NULL OR expires_at > now())
       )
       OR (
           sqlc.arg(status_filter)::text = 'expired'
           AND revoked_at IS NULL
           AND expires_at IS NOT NULL
           AND expires_at <= now()
       )
       OR (
           sqlc.arg(status_filter)::text = 'revoked'
           AND revoked_at IS NOT NULL
       )
   )
 ORDER BY created_at DESC
 LIMIT sqlc.arg(row_limit);

-- name: RevokeAPIKey :execrows
UPDATE api_keys
   SET revoked_at = now()
 WHERE org_id = sqlc.arg(org_id)
   AND id = sqlc.arg(id)
   AND revoked_at IS NULL;

-- name: CreateAPIKeyGrant :one
INSERT INTO api_key_grants (
    id,
    org_id,
    api_key_id,
    project_id,
    environment_id,
    permission,
    created_by_user_id
) VALUES (
    sqlc.arg(id),
    sqlc.arg(org_id),
    sqlc.arg(api_key_id),
    sqlc.narg(project_id),
    sqlc.narg(environment_id),
    sqlc.arg(permission),
    sqlc.narg(created_by_user_id)
)
RETURNING *;

-- name: ListAPIKeyGrants :many
SELECT *
  FROM api_key_grants
 WHERE org_id = sqlc.arg(org_id)
   AND api_key_id = sqlc.arg(api_key_id)
 ORDER BY permission, project_id NULLS FIRST, environment_id NULLS FIRST, created_at ASC;

-- name: DeleteAPIKeyGrant :execrows
DELETE FROM api_key_grants
 WHERE org_id = sqlc.arg(org_id)
   AND api_key_id = sqlc.arg(api_key_id)
   AND id = sqlc.arg(id);

-- name: DeleteAPIKeyGrantsForKey :execrows
DELETE FROM api_key_grants
 WHERE org_id = sqlc.arg(org_id)
   AND api_key_id = sqlc.arg(api_key_id);
