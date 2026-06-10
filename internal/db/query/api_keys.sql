-- name: IssueAPIKey :one
WITH revoked AS (
    UPDATE api_keys
       SET revoked_at = now()
     WHERE api_keys.org_id = sqlc.arg(org_id)
       AND api_keys.project_id = sqlc.arg(project_id)
       AND api_keys.environment_id = sqlc.arg(environment_id)
       AND api_keys.name = sqlc.arg(name)
       AND api_keys.token_hash <> sqlc.arg(token_hash)
       AND api_keys.revoked_at IS NULL
     RETURNING 1
),
input AS (
    SELECT
        sqlc.arg(id)::uuid AS id,
        sqlc.arg(org_id)::uuid AS org_id,
        sqlc.arg(project_id)::uuid AS project_id,
        sqlc.arg(environment_id)::uuid AS environment_id,
        sqlc.arg(created_by_user_id)::uuid AS created_by_user_id,
        sqlc.arg(role)::org_member_role AS role,
        sqlc.arg(name)::text AS name,
        sqlc.arg(key_prefix)::text AS key_prefix,
        sqlc.arg(token_hash)::bytea AS token_hash,
        sqlc.arg(expires_at)::timestamptz AS expires_at
)
INSERT INTO api_keys (id, org_id, project_id, environment_id, created_by_user_id, role, name, key_prefix, token_hash, expires_at)
SELECT input.id,
       input.org_id,
       input.project_id,
       input.environment_id,
       input.created_by_user_id,
       input.role,
       input.name,
       input.key_prefix,
       input.token_hash,
       input.expires_at
  FROM input
 -- Force same-scope revocation before insert so the active-name partial unique index cannot race the replacement key.
 CROSS JOIN (SELECT count(*) FROM revoked) AS revoked_count
ON CONFLICT (token_hash) DO UPDATE SET
    role = EXCLUDED.role,
    name = EXCLUDED.name,
    key_prefix = EXCLUDED.key_prefix,
    project_id = EXCLUDED.project_id,
    environment_id = EXCLUDED.environment_id,
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
    matched.project_id,
    matched.environment_id,
    matched.created_by_user_id,
    matched.name,
    matched.key_prefix,
    matched.created_at,
    matched.last_used_at,
    matched.expires_at,
    matched.role::text AS role,
    convert_to(COALESCE(
        jsonb_agg(
            jsonb_build_object(
                'id', api_key_grants.id,
                'permission', api_key_grants.permission
            )
            ORDER BY api_key_grants.created_at, api_key_grants.id
        ) FILTER (WHERE api_key_grants.id IS NOT NULL),
        '[]'::jsonb
    )::text, 'UTF8') AS grants
  FROM matched
  LEFT JOIN api_key_grants
    ON api_key_grants.org_id = matched.org_id
   AND api_key_grants.api_key_id = matched.id
 GROUP BY matched.id,
          matched.org_id,
          matched.project_id,
          matched.environment_id,
          matched.created_by_user_id,
          matched.name,
          matched.key_prefix,
          matched.created_at,
          matched.last_used_at,
          matched.expires_at,
          matched.role;

-- name: ListAPIKeys :many
SELECT id, org_id, project_id, environment_id, created_by_user_id, name, key_prefix, created_at, last_used_at, expires_at, revoked_at
  FROM api_keys
 WHERE org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
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
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND id = sqlc.arg(id)
   AND revoked_at IS NULL;

-- name: CreateAPIKeyGrant :one
INSERT INTO api_key_grants (
    id,
    org_id,
    api_key_id,
    permission,
    created_by_user_id
) VALUES (
    sqlc.arg(id),
    sqlc.arg(org_id),
    sqlc.arg(api_key_id),
    sqlc.arg(permission),
    sqlc.narg(created_by_user_id)
)
RETURNING *;

-- name: ListAPIKeyGrants :many
SELECT *
  FROM api_key_grants
 WHERE org_id = sqlc.arg(org_id)
   AND api_key_id = sqlc.arg(api_key_id)
 ORDER BY permission, created_at ASC;

-- name: DeleteAPIKeyGrant :execrows
DELETE FROM api_key_grants
 WHERE org_id = sqlc.arg(org_id)
   AND api_key_id = sqlc.arg(api_key_id)
   AND id = sqlc.arg(id);

-- name: DeleteAPIKeyGrantsForKey :execrows
DELETE FROM api_key_grants
 WHERE org_id = sqlc.arg(org_id)
   AND api_key_id = sqlc.arg(api_key_id);
