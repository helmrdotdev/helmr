-- name: CreateSession :one
INSERT INTO sessions (id, org_id, user_id, token_hash, expires_at)
VALUES (sqlc.arg(id), sqlc.arg(org_id), sqlc.arg(user_id), sqlc.arg(token_hash), sqlc.arg(expires_at))
RETURNING *;

-- name: GetSessionByTokenHash :one
SELECT
    sessions.id,
    sessions.org_id,
    sessions.user_id,
    org_members.role,
    COALESCE(org_members.display_name, users.display_name) AS display_name,
    sessions.expires_at
  FROM sessions
  JOIN org_members
    ON org_members.org_id = sessions.org_id
   AND org_members.user_id = sessions.user_id
  JOIN users ON users.id = sessions.user_id
 WHERE sessions.token_hash = sqlc.arg(token_hash)
   AND sessions.revoked_at IS NULL
   AND sessions.expires_at > now()
   AND org_members.disabled_at IS NULL
   AND users.disabled_at IS NULL;

-- name: RefreshSession :exec
UPDATE sessions
   SET last_seen_at = now(),
       expires_at = sqlc.arg(expires_at)
 WHERE id = sqlc.arg(id)
   AND revoked_at IS NULL;

-- name: RevokeSessionByTokenHash :execrows
UPDATE sessions
   SET revoked_at = now()
 WHERE token_hash = sqlc.arg(token_hash)
   AND revoked_at IS NULL;

-- name: RevokeSessionsForUser :execrows
UPDATE sessions
   SET revoked_at = now()
 WHERE org_id = sqlc.arg(org_id)
   AND user_id = sqlc.arg(user_id)
   AND revoked_at IS NULL;
