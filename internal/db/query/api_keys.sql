-- name: IssueAPIKey :one
INSERT INTO api_keys (id,org_id,project_id,environment_id,created_by_member_id,permissions,name,key_prefix,token_hash,expires_at)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
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
    matched.created_by_member_id,
    matched.name,
    matched.key_prefix,
    matched.created_at,
    matched.last_used_at,
    matched.expires_at,
    matched.permissions
  FROM matched;

-- name: ListAPIKeys :many
SELECT id, org_id, project_id, environment_id, created_by_member_id, permissions, name, key_prefix, created_at, last_used_at, expires_at, revoked_at
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
   AND (
       sqlc.narg(after_created_at)::timestamptz IS NULL
       OR (created_at, id) < (sqlc.narg(after_created_at)::timestamptz, sqlc.narg(after_id)::uuid)
   )
 ORDER BY created_at DESC, id DESC
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
UPDATE api_keys SET name=$5,permissions=$6,expires_at=$7
WHERE org_id=$1 AND project_id=$2 AND environment_id=$3 AND id=$4
 AND revoked_at IS NULL AND (expires_at IS NULL OR expires_at>clock_timestamp()) RETURNING *;

-- name: RotateActiveAPIKey :one
UPDATE api_keys SET token_hash=$5,key_prefix=$6
WHERE org_id=$1 AND project_id=$2 AND environment_id=$3 AND id=$4
 AND revoked_at IS NULL AND (expires_at IS NULL OR expires_at>clock_timestamp()) RETURNING *;
