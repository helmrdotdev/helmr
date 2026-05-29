-- name: CreateSession :one
INSERT INTO sessions (id, org_id, user_id, token_hash, expires_at)
VALUES (sqlc.arg(id), sqlc.narg(org_id), sqlc.arg(user_id), sqlc.arg(token_hash), sqlc.arg(expires_at))
RETURNING *;

-- name: GetSessionByTokenHash :one
SELECT
    sessions.id,
    sessions.user_id,
    users.display_name,
    users.profile_image_url,
    selected_member.org_id,
    COALESCE(selected_member.role::text, '')::text AS role,
    COALESCE(selected_member.display_name, users.display_name) AS member_display_name,
    sessions.expires_at
  FROM sessions
  JOIN users ON users.id = sessions.user_id
  LEFT JOIN LATERAL (
      SELECT org_members.org_id,
             org_members.role,
             org_members.display_name
        FROM org_members
       WHERE org_members.user_id = sessions.user_id
         AND (sessions.org_id IS NULL OR org_members.org_id = sessions.org_id)
         AND org_members.disabled_at IS NULL
       ORDER BY (org_members.org_id = sessions.org_id) DESC, org_members.created_at ASC
       LIMIT 1
  ) AS selected_member ON true
 WHERE sessions.token_hash = sqlc.arg(token_hash)
   AND sessions.revoked_at IS NULL
   AND sessions.expires_at > now()
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
 WHERE user_id = sqlc.arg(user_id)
   AND revoked_at IS NULL;

-- name: RevokeOrgSessionsForUser :execrows
UPDATE sessions
   SET revoked_at = now()
 WHERE org_id = sqlc.arg(org_id)
   AND user_id = sqlc.arg(user_id)
   AND revoked_at IS NULL;
