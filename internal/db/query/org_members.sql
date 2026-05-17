-- name: EnsureOrgMember :one
INSERT INTO org_members (org_id, user_id, role, display_name)
VALUES (
    sqlc.arg(org_id),
    sqlc.arg(user_id),
    sqlc.arg(role)::org_member_role,
    sqlc.arg(display_name)
)
ON CONFLICT (org_id, user_id) DO UPDATE
   SET role = EXCLUDED.role,
       display_name = EXCLUDED.display_name,
       disabled_at = NULL,
       updated_at = now()
RETURNING *;

-- name: OwnerExists :one
SELECT EXISTS (
    SELECT 1
      FROM org_members
      JOIN users ON users.id = org_members.user_id
     WHERE org_members.org_id = sqlc.arg(org_id)
       AND org_members.role = 'owner'
       AND org_members.disabled_at IS NULL
       AND users.disabled_at IS NULL
);

-- name: GetLoginIdentityMember :one
SELECT
    auth_identities.user_id,
    org_members.org_id,
    org_members.role
  FROM auth_identities
  JOIN org_members ON org_members.user_id = auth_identities.user_id
  JOIN users ON users.id = auth_identities.user_id
 WHERE auth_identities.provider = sqlc.arg(provider)
   AND auth_identities.subject = sqlc.arg(subject)
   AND org_members.disabled_at IS NULL
   AND users.disabled_at IS NULL
 ORDER BY org_members.created_at ASC
 LIMIT 1;

-- name: GetOrgMember :one
SELECT org_members.*, users.display_name AS user_display_name, users.profile_image_url
  FROM org_members
  JOIN users ON users.id = org_members.user_id
 WHERE org_members.org_id = sqlc.arg(org_id)
   AND org_members.user_id = sqlc.arg(user_id)
   AND org_members.disabled_at IS NULL
   AND users.disabled_at IS NULL;

-- name: ListOrgMembers :many
SELECT org_members.org_id,
       org_members.user_id,
       org_members.role,
       COALESCE(org_members.display_name, users.display_name) AS display_name,
       users.primary_email,
       org_members.disabled_at,
       users.disabled_at AS user_disabled_at,
       org_members.created_at,
       org_members.updated_at
  FROM org_members
  JOIN users ON users.id = org_members.user_id
 WHERE org_members.org_id = sqlc.arg(org_id)
 ORDER BY org_members.disabled_at IS NULL DESC,
          org_members.created_at ASC;

-- name: GetOrgMemberForManagement :one
SELECT org_members.org_id,
       org_members.user_id,
       org_members.role,
       org_members.display_name,
       users.primary_email,
       org_members.disabled_at,
       users.disabled_at AS user_disabled_at,
       org_members.created_at,
       org_members.updated_at
  FROM org_members
  JOIN users ON users.id = org_members.user_id
 WHERE org_members.org_id = sqlc.arg(org_id)
   AND org_members.user_id = sqlc.arg(user_id);

-- name: UpdateOrgMemberRole :one
WITH locked_active_owners AS (
    SELECT org_members.user_id
      FROM org_members
      JOIN users ON users.id = org_members.user_id
     WHERE org_members.org_id = sqlc.arg(org_id)
       AND org_members.role = 'owner'
       AND org_members.disabled_at IS NULL
       AND users.disabled_at IS NULL
     FOR UPDATE OF org_members
)
UPDATE org_members
   SET role = sqlc.arg(role)::org_member_role,
       updated_at = now()
 WHERE org_members.org_id = sqlc.arg(org_id)
   AND org_members.user_id = sqlc.arg(user_id)
   AND org_members.role = sqlc.arg(expected_role)::org_member_role
   AND org_members.disabled_at IS NULL
   AND (
       sqlc.arg(actor_is_owner)::boolean
       OR (
           org_members.role <> 'owner'
           AND sqlc.arg(role)::org_member_role <> 'owner'
       )
   )
   AND (
       org_members.role <> 'owner'
       OR sqlc.arg(role)::org_member_role = 'owner'
       OR EXISTS (
           SELECT 1 FROM locked_active_owners
            WHERE locked_active_owners.user_id <> org_members.user_id
       )
   )
RETURNING *;

-- name: DisableOrgMember :one
WITH locked_active_owners AS (
    SELECT org_members.user_id
      FROM org_members
      JOIN users ON users.id = org_members.user_id
     WHERE org_members.org_id = sqlc.arg(org_id)
       AND org_members.role = 'owner'
       AND org_members.disabled_at IS NULL
       AND users.disabled_at IS NULL
     FOR UPDATE OF org_members
)
UPDATE org_members
   SET disabled_at = now(),
       updated_at = now()
 WHERE org_members.org_id = sqlc.arg(org_id)
   AND org_members.user_id = sqlc.arg(user_id)
   AND org_members.role = sqlc.arg(expected_role)::org_member_role
   AND org_members.disabled_at IS NULL
   AND (
       sqlc.arg(actor_is_owner)::boolean
       OR org_members.role <> 'owner'
   )
   AND (
       org_members.role <> 'owner'
       OR EXISTS (
           SELECT 1 FROM locked_active_owners
            WHERE locked_active_owners.user_id <> org_members.user_id
       )
   )
RETURNING *;
