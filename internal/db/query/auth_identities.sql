-- name: GetAuthIdentity :one
SELECT * FROM auth_identities WHERE provider=$1 AND subject=$2 ORDER BY created_at DESC LIMIT 1;

-- name: CreateAuthIdentity :one
INSERT INTO auth_identities(id,user_id,provider,subject,last_authenticated_at)
VALUES ($1,$2,$3,$4,clock_timestamp()) RETURNING *;

-- name: GetAccount :one
SELECT * FROM users WHERE id=$1;

-- name: GetAccountByEmail :one
SELECT * FROM users WHERE lower(primary_email)=lower($1);

-- name: CreateAccount :one
INSERT INTO users(id,primary_email,display_name,profile_image_url,webauthn_user_handle)
VALUES ($1,$2,$3,$4,$5) RETURNING *;
