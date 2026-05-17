-- name: UpsertAuthIdentity :one
WITH existing_identity AS (
    UPDATE auth_identities AS identity
       SET email = sqlc.arg(email),
           claims = sqlc.arg(claims),
           updated_at = now(),
           last_login_at = now()
     WHERE identity.provider = sqlc.arg(identity_provider)
       AND identity.subject = sqlc.arg(identity_subject)
     RETURNING user_id
),
inserted_user AS (
    INSERT INTO users (id, display_name, profile_image_url, primary_email)
    SELECT sqlc.arg(user_id), sqlc.arg(display_name), sqlc.narg(profile_image_url), sqlc.arg(email)
     WHERE NOT EXISTS (SELECT 1 FROM existing_identity)
    RETURNING id, display_name, profile_image_url, primary_email, disabled_at, created_at, updated_at
),
inserted_identity AS (
    INSERT INTO auth_identities (
        id,
        user_id,
        provider,
        subject,
        email,
        claims,
        last_login_at
    )
    SELECT
        sqlc.arg(identity_id),
        inserted_user.id,
        sqlc.arg(identity_provider),
        sqlc.arg(identity_subject),
        sqlc.arg(email),
        sqlc.arg(claims),
        now()
      FROM inserted_user
    RETURNING user_id
),
updated_existing_user AS (
    UPDATE users
       SET display_name = sqlc.arg(display_name),
           profile_image_url = COALESCE(sqlc.narg(profile_image_url), users.profile_image_url),
           primary_email = sqlc.arg(email),
           updated_at = now()
     WHERE id IN (SELECT user_id FROM existing_identity)
    RETURNING id, display_name, profile_image_url, primary_email, disabled_at, created_at, updated_at
)
SELECT id, display_name, profile_image_url, primary_email, disabled_at, created_at, updated_at FROM updated_existing_user
UNION ALL
SELECT id, display_name, profile_image_url, primary_email, disabled_at, created_at, updated_at FROM inserted_user;
