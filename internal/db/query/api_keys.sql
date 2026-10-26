-- name: IssueAPIKey :one
INSERT INTO api_keys (id,org_id,project_id,environment_id,created_by_member_id,name,key_prefix,token_hash,expires_at)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
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
    matched.credential_version,
    matched.org_id,
    matched.project_id,
    matched.environment_id,
    matched.created_by_member_id,
    matched.name,
    matched.key_prefix,
    matched.created_at,
    matched.last_used_at,
    matched.expires_at
  FROM matched;

-- name: ListAPIKeys :many
SELECT k.id, k.org_id, k.project_id, k.environment_id, k.created_by_member_id, k.name, k.key_prefix, k.created_at, k.last_used_at, k.expires_at, k.revoked_at,
 ARRAY(SELECT r.name FROM role_grants g JOIN roles r ON (r.org_id,r.id)=(g.org_id,g.role_id) WHERE g.api_key_id=k.id ORDER BY r.name)::text[] AS role_names
  FROM api_keys k
 WHERE k.org_id = sqlc.arg(org_id)
   AND k.project_id = sqlc.arg(project_id)
   AND k.environment_id = sqlc.arg(environment_id)
   AND (
       sqlc.arg(status_filter)::text = 'all'
       OR (
           sqlc.arg(status_filter)::text = 'active'
           AND k.revoked_at IS NULL
           AND (k.expires_at IS NULL OR k.expires_at > now())
       )
       OR (
           sqlc.arg(status_filter)::text = 'expired'
           AND k.revoked_at IS NULL
           AND k.expires_at IS NOT NULL
           AND k.expires_at <= now()
       )
       OR (
           sqlc.arg(status_filter)::text = 'revoked'
           AND k.revoked_at IS NOT NULL
       )
   )
   AND (
       sqlc.narg(after_created_at)::timestamptz IS NULL
       OR (k.created_at, k.id) < (sqlc.narg(after_created_at)::timestamptz, sqlc.narg(after_id)::uuid)
   )
 ORDER BY k.created_at DESC, k.id DESC
 LIMIT sqlc.arg(row_limit);

-- name: RevokeAPIKey :execrows
UPDATE api_keys
   SET revoked_at = now()
 WHERE org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND id = sqlc.arg(id)
   AND revoked_at IS NULL;

-- name: LockActiveAPIKey :one
SELECT * FROM api_keys WHERE org_id=$1 AND project_id=$2 AND environment_id=$3 AND id=$4
 AND revoked_at IS NULL AND (expires_at IS NULL OR expires_at>clock_timestamp()) FOR UPDATE;

-- name: UpdateActiveAPIKey :one
UPDATE api_keys SET name=$5,expires_at=$6
WHERE org_id=$1 AND project_id=$2 AND environment_id=$3 AND id=$4
 AND revoked_at IS NULL AND (expires_at IS NULL OR expires_at>clock_timestamp()) RETURNING *;

-- name: RotateActiveAPIKey :one
UPDATE api_keys SET token_hash=$5,key_prefix=$6,credential_version=credential_version+1
WHERE org_id=$1 AND project_id=$2 AND environment_id=$3 AND id=$4
 AND revoked_at IS NULL AND (expires_at IS NULL OR expires_at>clock_timestamp()) RETURNING *;

-- name: ListAPIKeyRoleNames :many
SELECT r.name FROM role_grants g JOIN roles r ON (r.org_id,r.id)=(g.org_id,g.role_id)
WHERE g.org_id=$1 AND g.api_key_id=$2 ORDER BY r.name;
