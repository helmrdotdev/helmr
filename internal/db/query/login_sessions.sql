-- name: GetLoginSessionByTokenHash :one
SELECT * FROM login_sessions WHERE token_hash=$1 AND revoked_at IS NULL AND expires_at>clock_timestamp();

-- name: GetLoginSession :one
SELECT * FROM login_sessions WHERE id=$1 AND revoked_at IS NULL AND expires_at>clock_timestamp();

-- name: RefreshLoginSession :execrows
UPDATE login_sessions SET last_human_activity_at=clock_timestamp(), expires_at=$2
WHERE id=$1 AND revoked_at IS NULL AND expires_at>clock_timestamp() AND channel IN ('browser','cli');

-- name: RevokeLoginSessionByTokenHash :execrows
UPDATE login_sessions SET revoked_at=clock_timestamp() WHERE token_hash=$1 AND revoked_at IS NULL;

-- name: RevokeLoginSessionsForUser :execrows
UPDATE login_sessions SET revoked_at=clock_timestamp() WHERE user_id=$1 AND revoked_at IS NULL;

-- name: RevokeLoginSessionsForMembership :exec
UPDATE login_sessions SET revoked_at=clock_timestamp()
WHERE member_id=$1 AND channel='cli' AND revoked_at IS NULL;
