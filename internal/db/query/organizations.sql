-- name: LockOrganizationsForSelfHostedSetup :exec
LOCK TABLE organizations IN EXCLUSIVE MODE;

-- name: CountOrganizations :one
SELECT count(*) FROM organizations;

-- name: CreateOrganization :one
INSERT INTO organizations (id, name, slug)
VALUES (
    sqlc.arg(id),
    sqlc.arg(name),
    sqlc.arg(slug)
)
RETURNING *;

-- name: GetUserOnboardingState :one
SELECT
    users.id AS user_id,
    users.display_name,
    users.profile_image_url,
    users.admin,
    first_member.org_id,
    organizations.name AS org_name,
    organizations.slug AS org_slug,
    COALESCE(first_member.role::text, '')::text AS role,
    EXISTS (
        SELECT 1
          FROM projects
         WHERE projects.org_id = first_member.org_id
    ) AS has_projects
  FROM users
  LEFT JOIN LATERAL (
      SELECT org_members.org_id,
             org_members.role
        FROM org_members
       WHERE org_members.user_id = users.id
         AND org_members.disabled_at IS NULL
       ORDER BY org_members.created_at ASC
       LIMIT 1
  ) AS first_member ON true
  LEFT JOIN organizations ON organizations.id = first_member.org_id
 WHERE users.id = sqlc.arg(user_id)
   AND users.disabled_at IS NULL;

-- name: GrantUserAdmin :exec
UPDATE users SET admin = true, updated_at = now() WHERE id = sqlc.arg(user_id);

-- name: ListOrganizationIDs :many
SELECT id
  FROM organizations
 ORDER BY id ASC
 LIMIT sqlc.arg(row_limit);
