-- name: LockOrganizationsForSelfHostedSetup :exec
LOCK TABLE organizations IN EXCLUSIVE MODE;

-- name: CountOrganizations :one
SELECT count(*) FROM organizations;

-- name: LockOrganizationForProjectDefaults :one
SELECT id
  FROM organizations
 WHERE id = sqlc.arg(id)
 FOR NO KEY UPDATE;

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
    organizations.name AS org_name,
    organizations.slug AS org_slug,
    EXISTS (
        SELECT 1 FROM projects
         WHERE projects.org_id = sqlc.narg(org_id)
    ) AS has_projects
  FROM users
  LEFT JOIN organizations ON organizations.id = sqlc.narg(org_id)
 WHERE users.id = sqlc.arg(user_id)
   AND users.disabled_at IS NULL;

-- name: GrantUserAdmin :exec
UPDATE users SET admin = true, updated_at = now() WHERE id = sqlc.arg(user_id);

-- name: ListOrganizationIDs :many
SELECT id
  FROM organizations
 ORDER BY id ASC
 LIMIT sqlc.arg(row_limit);
