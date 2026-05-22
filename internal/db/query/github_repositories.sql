-- name: ListGitHubInstallationRepositories :many
SELECT i.id AS installation_row_id,
       i.installation_id,
       i.account_login,
       i.account_type,
       i.repository_selection,
       i.html_url AS installation_html_url,
       i.suspended_at,
       i.deleted_at AS installation_deleted_at,
       i.created_at AS installation_created_at,
       i.updated_at AS installation_updated_at,
       r.id AS repository_row_id,
       r.github_repository_id,
       r.owner_login,
       r.name AS repository_name,
       r.full_name,
       r.private,
       r.archived,
       r.default_branch,
       r.html_url AS repository_html_url,
       r.deleted_at AS repository_deleted_at,
       r.created_at AS repository_created_at,
       r.updated_at AS repository_updated_at,
       pgr.id AS project_github_repository_id,
       pgr.project_id AS project_github_repository_project_id
  FROM github_app_installations i
  JOIN github_repositories r
    ON r.org_id = i.org_id
   AND r.installation_id = i.installation_id
   AND r.deleted_at IS NULL
  LEFT JOIN project_github_repositories pgr
    ON pgr.org_id = r.org_id
   AND pgr.github_repository_id = r.github_repository_id
   AND pgr.project_id = sqlc.arg(project_id)
 WHERE i.org_id = sqlc.arg(org_id)
   AND i.installation_id = sqlc.arg(installation_id)
   AND i.suspended_at IS NULL
   AND i.deleted_at IS NULL
 ORDER BY lower(r.full_name);

-- name: GetActiveGitHubRepositoryTarget :one
SELECT i.id AS installation_row_id,
       i.installation_id,
       i.account_login,
       i.account_type,
       i.repository_selection,
       i.html_url AS installation_html_url,
       i.suspended_at,
       i.deleted_at AS installation_deleted_at,
       i.created_at AS installation_created_at,
       i.updated_at AS installation_updated_at,
       r.id AS repository_row_id,
       r.github_repository_id,
       r.owner_login,
       r.name AS repository_name,
       r.full_name,
       r.private,
       r.archived,
       r.default_branch,
       r.html_url AS repository_html_url,
       r.deleted_at AS repository_deleted_at,
       r.created_at AS repository_created_at,
       r.updated_at AS repository_updated_at,
       NULL::uuid AS project_github_repository_id,
       NULL::uuid AS project_github_repository_project_id
  FROM github_app_installations i
  JOIN github_repositories r
    ON r.org_id = i.org_id
   AND r.installation_id = i.installation_id
   AND r.deleted_at IS NULL
 WHERE i.org_id = sqlc.arg(org_id)
   AND i.suspended_at IS NULL
   AND i.deleted_at IS NULL
   AND r.github_repository_id = sqlc.arg(github_repository_id)
 LIMIT 1;

-- name: UpsertGitHubRepository :one
INSERT INTO github_repositories (
    id,
    org_id,
    installation_id,
    github_repository_id,
    owner_login,
    name,
    full_name,
    private,
    archived,
    default_branch,
    html_url
) VALUES (
    sqlc.arg(id),
    sqlc.arg(org_id),
    sqlc.arg(installation_id),
    sqlc.arg(github_repository_id),
    sqlc.arg(owner_login),
    sqlc.arg(name),
    sqlc.arg(full_name),
    sqlc.arg(private),
    sqlc.arg(archived),
    sqlc.arg(default_branch),
    sqlc.arg(html_url)
)
ON CONFLICT (org_id, github_repository_id) DO UPDATE
   SET installation_id = EXCLUDED.installation_id,
       owner_login = EXCLUDED.owner_login,
       name = EXCLUDED.name,
       full_name = EXCLUDED.full_name,
       private = EXCLUDED.private,
       archived = EXCLUDED.archived,
       default_branch = EXCLUDED.default_branch,
       html_url = EXCLUDED.html_url,
       deleted_at = NULL,
       updated_at = now()
RETURNING *;

-- name: MarkGitHubRepositoryDeleted :one
WITH deleted_repository AS (
    UPDATE github_repositories r
       SET deleted_at = now(),
           updated_at = now()
     WHERE r.org_id = sqlc.arg(org_id)
       AND r.installation_id = sqlc.arg(installation_id)
       AND r.github_repository_id = sqlc.arg(github_repository_id)
    RETURNING r.*
),
deleted_project_github_repositories AS (
    DELETE FROM project_github_repositories pgr
     USING deleted_repository r
     WHERE pgr.org_id = r.org_id
       AND pgr.github_repository_id = r.github_repository_id
    RETURNING pgr.id
)
SELECT *
  FROM deleted_repository;

-- name: MarkGitHubRepositoriesDeletedByInstallationID :many
WITH deleted_repositories AS (
    UPDATE github_repositories r
       SET deleted_at = now(),
           updated_at = now()
      FROM github_app_installations i
     WHERE r.org_id = i.org_id
       AND r.installation_id = i.installation_id
       AND i.installation_id = sqlc.arg(installation_id)
       AND r.deleted_at IS NULL
    RETURNING r.*
),
deleted_project_github_repositories AS (
    DELETE FROM project_github_repositories pgr
     USING deleted_repositories r
     WHERE pgr.org_id = r.org_id
       AND pgr.github_repository_id = r.github_repository_id
    RETURNING pgr.id
)
SELECT *
  FROM deleted_repositories;
