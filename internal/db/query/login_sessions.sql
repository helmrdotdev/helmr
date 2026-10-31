-- name: GetLoginSessionByTokenHash :one
SELECT * FROM login_sessions WHERE token_hash=$1 AND revoked_at IS NULL AND expires_at>clock_timestamp();

-- name: GetLoginSession :one
SELECT * FROM login_sessions WHERE id=$1 AND revoked_at IS NULL AND expires_at>clock_timestamp();

-- An operator session holds authority only while the session and the key that
-- signed it in stay live. Inside a transaction, the shared locks hold that
-- authority until commit, so key removal, repair and sign-out wait for it.
-- Outside a transaction the locks end with the statement.
-- name: GetOperatorAuthority :one
SELECT s.id,s.operator_id,s.passkey_id,s.authenticated_at,s.expires_at
FROM login_sessions s
JOIN platform_operators o ON o.id=s.operator_id
JOIN passkey_credentials k ON k.id=s.passkey_id AND k.operator_id=o.id
WHERE s.id=$1 AND s.channel='operator' AND s.revoked_at IS NULL AND s.expires_at>clock_timestamp() AND k.revoked_at IS NULL
FOR SHARE OF o,s,k;
