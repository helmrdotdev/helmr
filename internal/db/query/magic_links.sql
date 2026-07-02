-- name: LockMagicLinkRecipient :exec
SELECT pg_advisory_xact_lock(sqlc.arg(lock_key)::bigint);

-- name: CreateMagicLink :one
INSERT INTO magic_links (
    id,
    purpose,
    token_hash,
    email,
    org_id,
    invitation_id,
    redirect_after,
    expires_at
)
VALUES (
    sqlc.arg(id),
    sqlc.arg(purpose)::magic_link_purpose,
    sqlc.arg(token_hash),
    sqlc.arg(email),
    sqlc.narg(org_id),
    sqlc.narg(invitation_id),
    sqlc.narg(redirect_after),
    sqlc.arg(expires_at)
)
RETURNING *;

-- name: MarkMagicLinkSent :execrows
UPDATE magic_links AS current_link
   SET sent_at = now(),
       revoked_at = CASE
           WHEN EXISTS (
               SELECT 1
                 FROM magic_links AS newer_link
                WHERE newer_link.purpose = current_link.purpose
                  AND newer_link.email = current_link.email
                  AND newer_link.org_id IS NOT DISTINCT FROM current_link.org_id
                  AND newer_link.invitation_id IS NOT DISTINCT FROM current_link.invitation_id
                  AND newer_link.created_at > current_link.created_at
                  AND newer_link.sent_at IS NOT NULL
           )
           THEN now()
           ELSE current_link.revoked_at
       END
 WHERE current_link.id = sqlc.arg(id)
   AND current_link.sent_at IS NULL
   AND current_link.delivery_failed_at IS NULL
   AND current_link.consumed_at IS NULL
   AND current_link.revoked_at IS NULL;

-- name: MarkMagicLinkDeliveryFailed :execrows
UPDATE magic_links
   SET delivery_failed_at = now(),
       revoked_at = now()
 WHERE id = sqlc.arg(id)
   AND sent_at IS NULL
   AND delivery_failed_at IS NULL
   AND consumed_at IS NULL
   AND revoked_at IS NULL;

-- name: GetActiveMagicLinkByTokenHash :one
SELECT id,
       purpose,
       email,
       org_id,
       invitation_id,
       redirect_after,
       expires_at
  FROM magic_links
 WHERE token_hash = sqlc.arg(token_hash)
   AND sent_at IS NOT NULL
   AND consumed_at IS NULL
   AND revoked_at IS NULL
   AND expires_at > now()
 FOR UPDATE;

-- name: ConsumeMagicLink :execrows
UPDATE magic_links
   SET consumed_at = now(),
       consumed_by_user_id = sqlc.arg(consumed_by_user_id)
 WHERE id = sqlc.arg(id)
   AND sent_at IS NOT NULL
   AND consumed_at IS NULL
   AND revoked_at IS NULL
   AND expires_at > now();

-- name: RevokeOpenMagicLinksForRecipient :execrows
UPDATE magic_links
   SET revoked_at = now()
 WHERE magic_links.purpose = sqlc.arg(purpose)::magic_link_purpose
   AND magic_links.email = sqlc.arg(email)
   AND magic_links.org_id IS NOT DISTINCT FROM sqlc.narg(org_id)
   AND magic_links.invitation_id IS NOT DISTINCT FROM sqlc.narg(invitation_id)
   AND magic_links.id <> sqlc.arg(except_id)
   AND magic_links.created_at < (SELECT created_at FROM magic_links AS current_link WHERE current_link.id = sqlc.arg(except_id))
   AND magic_links.sent_at IS NOT NULL
   AND magic_links.consumed_at IS NULL
   AND magic_links.revoked_at IS NULL;

-- name: CountRecentMagicLinks :one
SELECT count(*)
 FROM magic_links
 WHERE purpose = sqlc.arg(purpose)::magic_link_purpose
   AND email = sqlc.arg(email)
   AND delivery_failed_at IS NULL
   AND created_at >= sqlc.arg(since);

-- name: GetMagicLinkLoginUser :one
SELECT users.*
  FROM users
 WHERE lower(users.primary_email) = sqlc.arg(email)
   AND users.disabled_at IS NULL
 LIMIT 1;

-- name: UpsertMagicLinkAuthIdentity :one
WITH upserted_user AS (
    INSERT INTO users (id, display_name, profile_image_url, primary_email)
    SELECT
        sqlc.arg(user_id) AS id,
        sqlc.arg(display_name) AS display_name,
        sqlc.narg(profile_image_url) AS profile_image_url,
        sqlc.arg(email) AS primary_email
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
updated_user AS (
    UPDATE users
       SET display_name = sqlc.arg(display_name),
           profile_image_url = COALESCE(sqlc.narg(profile_image_url), users.profile_image_url),
           primary_email = sqlc.arg(email),
           updated_at = now()
     WHERE id IN (SELECT user_id FROM upserted_identity)
    RETURNING id, display_name, profile_image_url, primary_email, disabled_at, created_at, updated_at
),
selected_user AS (
    SELECT id, display_name, profile_image_url, primary_email, disabled_at, created_at, updated_at FROM updated_user
    UNION ALL
    SELECT id, display_name, profile_image_url, primary_email, disabled_at, created_at, updated_at
      FROM upserted_user
     WHERE NOT EXISTS (SELECT 1 FROM updated_user)
)
SELECT id, display_name, profile_image_url, primary_email, disabled_at, created_at, updated_at FROM selected_user;
