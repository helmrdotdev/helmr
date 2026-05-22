-- name: ConnectProjectGitHubRepository :one
WITH access_target AS (
    SELECT r.org_id,
           r.github_repository_id
      FROM github_repositories r
      JOIN github_app_installations i
        ON i.org_id = r.org_id
       AND i.installation_id = r.installation_id
     WHERE r.org_id = sqlc.arg(org_id)
       AND r.github_repository_id = sqlc.arg(github_repository_id)
       AND r.deleted_at IS NULL
       AND i.suspended_at IS NULL
       AND i.deleted_at IS NULL
     FOR UPDATE OF r, i
)
INSERT INTO project_github_repositories (
    id,
    org_id,
    project_id,
    github_repository_id,
    connected_by_user_id
)
SELECT sqlc.arg(id),
       access_target.org_id,
       sqlc.arg(project_id),
       access_target.github_repository_id,
       sqlc.narg(connected_by_user_id)
  FROM access_target
ON CONFLICT (org_id, project_id, github_repository_id) DO UPDATE
   SET connected_by_user_id = EXCLUDED.connected_by_user_id,
       updated_at = now()
RETURNING *;

-- name: DisconnectProjectGitHubRepository :one
DELETE FROM project_github_repositories pgr
 USING github_repositories r
  JOIN github_app_installations i
    ON i.org_id = r.org_id
   AND i.installation_id = r.installation_id
 WHERE pgr.org_id = sqlc.arg(org_id)
   AND pgr.project_id = sqlc.arg(project_id)
   AND pgr.github_repository_id = sqlc.arg(github_repository_id)
   AND r.org_id = pgr.org_id
   AND r.github_repository_id = pgr.github_repository_id
   AND r.deleted_at IS NULL
   AND i.suspended_at IS NULL
   AND i.deleted_at IS NULL
RETURNING pgr.*;

-- name: GetActiveProjectGitHubRepository :one
SELECT pgr.id AS project_github_repository_id,
       r.installation_id,
       r.github_repository_id,
       r.full_name,
       r.name AS repository_name
  FROM project_github_repositories pgr
  JOIN github_repositories r
    ON r.org_id = pgr.org_id
   AND r.github_repository_id = pgr.github_repository_id
  JOIN github_app_installations i
    ON i.org_id = r.org_id
   AND i.installation_id = r.installation_id
 WHERE pgr.org_id = sqlc.arg(org_id)
   AND pgr.project_id = sqlc.arg(project_id)
   AND pgr.github_repository_id = sqlc.arg(github_repository_id)
   AND r.deleted_at IS NULL
   AND i.suspended_at IS NULL
   AND i.deleted_at IS NULL
 LIMIT 1;

-- name: GetActiveProjectGitHubRepositoryByFullName :one
SELECT pgr.id AS project_github_repository_id,
       r.installation_id,
       r.github_repository_id,
       r.full_name,
       r.name AS repository_name
  FROM project_github_repositories pgr
  JOIN github_repositories r
    ON r.org_id = pgr.org_id
   AND r.github_repository_id = pgr.github_repository_id
  JOIN github_app_installations i
    ON i.org_id = r.org_id
   AND i.installation_id = r.installation_id
 WHERE pgr.org_id = sqlc.arg(org_id)
   AND pgr.project_id = sqlc.arg(project_id)
   AND lower(r.full_name) = lower(sqlc.arg(full_name))
   AND r.deleted_at IS NULL
   AND i.suspended_at IS NULL
   AND i.deleted_at IS NULL
 LIMIT 1;

-- name: ListActiveProjectGitHubRepositories :many
SELECT pgr.id AS project_github_repository_id,
       r.installation_id,
       r.github_repository_id,
       r.full_name,
       r.name AS repository_name
  FROM project_github_repositories pgr
  JOIN github_repositories r
    ON r.org_id = pgr.org_id
   AND r.github_repository_id = pgr.github_repository_id
  JOIN github_app_installations i
    ON i.org_id = r.org_id
   AND i.installation_id = r.installation_id
 WHERE pgr.org_id = sqlc.arg(org_id)
   AND pgr.project_id = sqlc.arg(project_id)
   AND r.deleted_at IS NULL
   AND i.suspended_at IS NULL
   AND i.deleted_at IS NULL
 ORDER BY lower(r.full_name);
