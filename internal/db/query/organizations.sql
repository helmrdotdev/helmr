-- name: EnsureDefaultOrganization :exec
INSERT INTO organizations (id, name, slug)
VALUES ($1, 'Default', 'default')
ON CONFLICT (id) DO NOTHING;

-- name: CreateOrganization :one
INSERT INTO organizations (id, name, slug)
VALUES (sqlc.arg(id), sqlc.arg(name), sqlc.arg(slug))
RETURNING *;

-- name: GetOrganization :one
SELECT *
  FROM organizations
 WHERE id = sqlc.arg(id);

-- name: GetUserOnboardingState :one
SELECT
    users.id AS user_id,
    users.display_name,
    users.profile_image_url,
    first_member.org_id,
    organizations.name AS org_name,
    organizations.slug AS org_slug,
    COALESCE(first_member.role::text, '')::text AS role,
    EXISTS (
        SELECT 1
          FROM projects
         WHERE projects.org_id = first_member.org_id
           AND projects.archived_at IS NULL
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

-- name: GetDefaultProjectEnvironment :one
SELECT
    projects.id AS project_id,
    environments.id AS environment_id
  FROM projects
  JOIN environments
    ON environments.org_id = projects.org_id
   AND environments.project_id = projects.id
   AND environments.is_default
   AND environments.archived_at IS NULL
 WHERE projects.org_id = sqlc.arg(org_id)
   AND projects.is_default
   AND projects.archived_at IS NULL
 LIMIT 1;

-- name: ListOrganizationIDs :many
SELECT id
  FROM organizations
 ORDER BY id ASC
 LIMIT sqlc.arg(row_limit);

-- name: ListOrganizationIDsPage :many
SELECT id
  FROM organizations
 WHERE sqlc.narg(after_id)::uuid IS NULL
    OR id > sqlc.narg(after_id)::uuid
 ORDER BY id ASC
 LIMIT sqlc.arg(row_limit);
