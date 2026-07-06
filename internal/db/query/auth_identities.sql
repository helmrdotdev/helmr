-- name: UpsertAuthIdentity :one
WITH upserted_user AS (
    INSERT INTO users (id, public_id, display_name, profile_image_url, primary_email)
    SELECT
        sqlc.arg(user_id) AS id,
        sqlc.arg(user_public_id) AS public_id,
        sqlc.arg(display_name) AS display_name,
        sqlc.narg(profile_image_url) AS profile_image_url,
        CASE WHEN sqlc.arg(email_verified)::bool THEN sqlc.arg(email) ELSE NULL END AS primary_email
     WHERE NOT EXISTS (
         SELECT 1
           FROM auth_identities AS auth_identity
          WHERE auth_identity.provider = sqlc.arg(identity_provider)
            AND auth_identity.subject = sqlc.arg(identity_subject)
     )
    ON CONFLICT (lower(primary_email)) WHERE primary_email IS NOT NULL AND disabled_at IS NULL DO UPDATE
       SET primary_email = users.primary_email
     WHERE users.disabled_at IS NULL
    RETURNING id, display_name, profile_image_url, primary_email, disabled_at, created_at, updated_at
),
target_user AS (
    SELECT auth_identity.user_id AS id
      FROM auth_identities AS auth_identity
     WHERE auth_identity.provider = sqlc.arg(identity_provider)
       AND auth_identity.subject = sqlc.arg(identity_subject)
    UNION ALL
    SELECT id FROM upserted_user
),
upserted_identity AS (
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
        sqlc.arg(identity_id) AS id,
        target_user.id AS user_id,
        sqlc.arg(identity_provider) AS provider,
        sqlc.arg(identity_subject) AS subject,
        sqlc.arg(email) AS email,
        sqlc.arg(claims) AS claims,
        now() AS last_login_at
      FROM target_user
    ON CONFLICT (provider, subject) DO UPDATE
       SET email = EXCLUDED.email,
           claims = EXCLUDED.claims,
           updated_at = now(),
           last_login_at = now()
    RETURNING user_id
),
updated_existing_user AS (
    UPDATE users
       SET display_name = sqlc.arg(display_name),
           profile_image_url = COALESCE(sqlc.narg(profile_image_url), users.profile_image_url),
           primary_email = CASE WHEN sqlc.arg(email_verified)::bool THEN sqlc.arg(email) ELSE users.primary_email END,
           updated_at = now()
     WHERE id IN (SELECT user_id FROM upserted_identity)
    RETURNING id, display_name, profile_image_url, primary_email, disabled_at, created_at, updated_at
),
selected_user AS (
    SELECT id, display_name, profile_image_url, primary_email, disabled_at, created_at, updated_at FROM updated_existing_user
    UNION ALL
    SELECT id, display_name, profile_image_url, primary_email, disabled_at, created_at, updated_at
      FROM upserted_user
     WHERE NOT EXISTS (SELECT 1 FROM updated_existing_user)
)
SELECT id, display_name, profile_image_url, primary_email, disabled_at, created_at, updated_at FROM selected_user;
