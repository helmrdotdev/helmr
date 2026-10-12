-- name: CreateLoginSession :one
INSERT INTO login_sessions (id, org_id, user_id, token_hash, expires_at)
VALUES (sqlc.arg(id), sqlc.narg(org_id), sqlc.arg(user_id), sqlc.arg(token_hash), sqlc.arg(expires_at))
RETURNING *;

-- name: GetLoginSessionByTokenHash :one
SELECT
    login_sessions.id,
    login_sessions.user_id,
    users.display_name,
    users.profile_image_url,
    users.admin,
    selected_member.org_id,
    COALESCE(selected_member.role::text, '')::text AS role,
    COALESCE(selected_member.display_name, users.display_name) AS member_display_name,
    login_sessions.expires_at
  FROM login_sessions
  JOIN users ON users.id = login_sessions.user_id
  LEFT JOIN LATERAL (
      SELECT org_members.org_id,
             org_members.role,
             org_members.display_name
        FROM org_members
       WHERE org_members.user_id = login_sessions.user_id
         AND (login_sessions.org_id IS NULL OR org_members.org_id = login_sessions.org_id)
         AND org_members.disabled_at IS NULL
       ORDER BY (org_members.org_id = login_sessions.org_id) DESC, org_members.created_at ASC
       LIMIT 1
  ) AS selected_member ON true
 WHERE login_sessions.token_hash = sqlc.arg(token_hash)
   AND login_sessions.revoked_at IS NULL
   AND login_sessions.expires_at > now()
   AND users.disabled_at IS NULL
   AND (login_sessions.org_id IS NULL OR selected_member.org_id IS NOT NULL);

-- name: RefreshLoginSession :exec
UPDATE login_sessions
   SET last_seen_at = now(),
       expires_at = sqlc.arg(expires_at)
 WHERE id = sqlc.arg(id)
   AND revoked_at IS NULL;

-- name: RevokeLoginSessionByTokenHash :execrows
UPDATE login_sessions
   SET revoked_at = now()
 WHERE token_hash = sqlc.arg(token_hash)
   AND revoked_at IS NULL;

-- name: RevokeLoginSessionsForUser :execrows
UPDATE login_sessions
   SET revoked_at = now()
 WHERE user_id = sqlc.arg(user_id)
   AND revoked_at IS NULL;
