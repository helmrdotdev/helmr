-- name: EnableProjectWorkspaceRepositoryAccess :one
WITH access_target AS (
    SELECT r.org_id,
           r.github_repository_id
      FROM github_repositories r
      JOIN github_repository_connections c
        ON c.org_id = r.org_id
       AND c.github_repository_id = r.github_repository_id
      JOIN github_app_installations i
        ON i.org_id = r.org_id
       AND i.installation_id = r.installation_id
     WHERE r.org_id = sqlc.arg(org_id)
       AND r.github_repository_id = sqlc.arg(github_repository_id)
       AND r.deleted_at IS NULL
       AND c.disabled_at IS NULL
       AND i.suspended_at IS NULL
       AND i.deleted_at IS NULL
     FOR UPDATE OF r, c, i
)
INSERT INTO project_workspace_repositories (
    id,
    org_id,
    project_id,
    github_repository_id,
    enabled_by_user_id
)
SELECT sqlc.arg(id),
       access_target.org_id,
       sqlc.arg(project_id),
       access_target.github_repository_id,
       sqlc.arg(enabled_by_user_id)
  FROM access_target
ON CONFLICT (org_id, project_id, github_repository_id) DO UPDATE
   SET enabled_by_user_id = EXCLUDED.enabled_by_user_id,
       disabled_at = NULL,
       updated_at = now()
RETURNING *;

-- name: DisableProjectWorkspaceRepositoryAccess :one
UPDATE project_workspace_repositories w
   SET disabled_at = now(),
       updated_at = now()
  FROM github_repositories r
  JOIN github_app_installations i
    ON i.org_id = r.org_id
   AND i.installation_id = r.installation_id
 WHERE w.org_id = sqlc.arg(org_id)
   AND w.project_id = sqlc.arg(project_id)
   AND w.github_repository_id = sqlc.arg(github_repository_id)
   AND w.disabled_at IS NULL
   AND r.org_id = w.org_id
   AND r.github_repository_id = w.github_repository_id
   AND r.deleted_at IS NULL
   AND i.suspended_at IS NULL
   AND i.deleted_at IS NULL
RETURNING w.*;

-- name: GetActiveProjectWorkspaceRepositoryAccess :one
SELECT w.id AS workspace_repository_id,
       r.installation_id,
       r.github_repository_id,
       r.full_name,
       r.name AS repository_name
  FROM project_workspace_repositories w
  JOIN github_repository_connections c
    ON c.org_id = w.org_id
   AND c.github_repository_id = w.github_repository_id
  JOIN github_repositories r
    ON r.org_id = w.org_id
   AND r.github_repository_id = w.github_repository_id
  JOIN github_app_installations i
    ON i.org_id = r.org_id
   AND i.installation_id = r.installation_id
 WHERE w.org_id = sqlc.arg(org_id)
   AND w.project_id = sqlc.arg(project_id)
   AND w.github_repository_id = sqlc.arg(github_repository_id)
   AND w.disabled_at IS NULL
   AND c.disabled_at IS NULL
   AND r.deleted_at IS NULL
   AND i.suspended_at IS NULL
   AND i.deleted_at IS NULL
 LIMIT 1;

-- name: GetActiveProjectWorkspaceRepositoryAccessByFullName :one
SELECT w.id AS workspace_repository_id,
       r.installation_id,
       r.github_repository_id,
       r.full_name,
       r.name AS repository_name
  FROM project_workspace_repositories w
  JOIN github_repository_connections c
    ON c.org_id = w.org_id
   AND c.github_repository_id = w.github_repository_id
  JOIN github_repositories r
    ON r.org_id = w.org_id
   AND r.github_repository_id = w.github_repository_id
  JOIN github_app_installations i
    ON i.org_id = r.org_id
   AND i.installation_id = r.installation_id
 WHERE w.org_id = sqlc.arg(org_id)
   AND w.project_id = sqlc.arg(project_id)
   AND lower(r.full_name) = lower(sqlc.arg(full_name))
   AND w.disabled_at IS NULL
   AND c.disabled_at IS NULL
   AND r.deleted_at IS NULL
   AND i.suspended_at IS NULL
   AND i.deleted_at IS NULL
 LIMIT 1;

-- name: ListActiveProjectWorkspaceRepositoryAccess :many
SELECT w.id AS workspace_repository_id,
       r.installation_id,
       r.github_repository_id,
       r.full_name,
       r.name AS repository_name
  FROM project_workspace_repositories w
  JOIN github_repository_connections c
    ON c.org_id = w.org_id
   AND c.github_repository_id = w.github_repository_id
  JOIN github_repositories r
    ON r.org_id = w.org_id
   AND r.github_repository_id = w.github_repository_id
  JOIN github_app_installations i
    ON i.org_id = r.org_id
   AND i.installation_id = r.installation_id
 WHERE w.org_id = sqlc.arg(org_id)
   AND w.project_id = sqlc.arg(project_id)
   AND w.disabled_at IS NULL
   AND c.disabled_at IS NULL
   AND r.deleted_at IS NULL
   AND i.suspended_at IS NULL
   AND i.deleted_at IS NULL
 ORDER BY lower(r.full_name);
