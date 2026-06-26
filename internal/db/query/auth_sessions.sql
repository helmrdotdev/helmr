-- name: CreateAuthSession :one
INSERT INTO auth_sessions (id, org_id, user_id, token_hash, expires_at)
VALUES (sqlc.arg(id), sqlc.narg(org_id), sqlc.arg(user_id), sqlc.arg(token_hash), sqlc.arg(expires_at))
RETURNING *;

-- name: GetAuthSessionByTokenHash :one
SELECT
    auth_sessions.id,
    auth_sessions.user_id,
    users.display_name,
    users.profile_image_url,
    selected_member.org_id,
    COALESCE(selected_member.role::text, '')::text AS role,
    COALESCE(selected_member.display_name, users.display_name) AS member_display_name,
    auth_sessions.expires_at
  FROM auth_sessions
  JOIN users ON users.id = auth_sessions.user_id
  LEFT JOIN LATERAL (
      SELECT org_members.org_id,
             org_members.role,
             org_members.display_name
        FROM org_members
       WHERE org_members.user_id = auth_sessions.user_id
         AND (auth_sessions.org_id IS NULL OR org_members.org_id = auth_sessions.org_id)
         AND org_members.disabled_at IS NULL
       ORDER BY (org_members.org_id = auth_sessions.org_id) DESC, org_members.created_at ASC
       LIMIT 1
  ) AS selected_member ON true
 WHERE auth_sessions.token_hash = sqlc.arg(token_hash)
   AND auth_sessions.revoked_at IS NULL
   AND auth_sessions.expires_at > now()
   AND users.disabled_at IS NULL;

-- name: RefreshAuthSession :exec
UPDATE auth_sessions
   SET last_seen_at = now(),
       expires_at = sqlc.arg(expires_at)
 WHERE id = sqlc.arg(id)
   AND revoked_at IS NULL;

-- name: RevokeAuthSessionByTokenHash :execrows
UPDATE auth_sessions
   SET revoked_at = now()
 WHERE token_hash = sqlc.arg(token_hash)
   AND revoked_at IS NULL;

-- name: RevokeAuthSessionsForUser :execrows
UPDATE auth_sessions
   SET revoked_at = now()
 WHERE user_id = sqlc.arg(user_id)
   AND revoked_at IS NULL;

-- name: RevokeOrgAuthSessionsForUser :execrows
UPDATE auth_sessions
   SET revoked_at = now()
 WHERE user_id = sqlc.arg(user_id)
   AND (org_id = sqlc.arg(org_id) OR org_id IS NULL)
   AND revoked_at IS NULL;
