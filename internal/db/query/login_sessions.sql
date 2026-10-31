-- name: GetLoginSessionByTokenHash :one
SELECT * FROM login_sessions WHERE token_hash=$1 AND revoked_at IS NULL AND expires_at>clock_timestamp();

-- name: GetLoginSession :one
SELECT * FROM login_sessions WHERE id=$1 AND revoked_at IS NULL AND expires_at>clock_timestamp();
