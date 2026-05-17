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
       c.id AS connection_id,
       c.disabled_at AS connection_disabled_at,
       w.id AS project_workspace_repository_id,
       w.project_id AS project_workspace_repository_project_id,
       w.disabled_at AS project_workspace_repository_disabled_at
  FROM github_app_installations i
  JOIN github_repositories r
    ON r.org_id = i.org_id
   AND r.installation_id = i.installation_id
   AND r.deleted_at IS NULL
  LEFT JOIN github_repository_connections c
    ON c.org_id = r.org_id
   AND c.github_repository_id = r.github_repository_id
  LEFT JOIN project_workspace_repositories w
    ON w.org_id = r.org_id
   AND w.github_repository_id = r.github_repository_id
   AND w.project_id = sqlc.arg(project_id)
   AND w.disabled_at IS NULL
 WHERE i.org_id = sqlc.arg(org_id)
   AND i.installation_id = sqlc.arg(installation_id)
   AND i.suspended_at IS NULL
   AND i.deleted_at IS NULL
 ORDER BY lower(r.full_name);

-- name: GetActiveGitHubRepositoryAccessTarget :one
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
       c.id AS connection_id,
       c.disabled_at AS connection_disabled_at,
       NULL::uuid AS project_workspace_repository_id,
       NULL::uuid AS project_workspace_repository_project_id,
       NULL::timestamptz AS project_workspace_repository_disabled_at
  FROM github_app_installations i
  JOIN github_repositories r
    ON r.org_id = i.org_id
   AND r.installation_id = i.installation_id
   AND r.deleted_at IS NULL
  LEFT JOIN github_repository_connections c
    ON c.org_id = r.org_id
   AND c.github_repository_id = r.github_repository_id
 WHERE i.org_id = sqlc.arg(org_id)
   AND i.installation_id = sqlc.arg(installation_id)
   AND i.suspended_at IS NULL
   AND i.deleted_at IS NULL
   AND r.github_repository_id = sqlc.arg(github_repository_id)
   AND (
       NOT sqlc.arg(require_access_enabled)::boolean
       OR (c.id IS NOT NULL AND c.disabled_at IS NULL)
   )
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
disabled_connections AS (
    UPDATE github_repository_connections c
       SET disabled_at = now(),
           updated_at = now()
      FROM deleted_repository r
     WHERE c.org_id = r.org_id
       AND c.github_repository_id = r.github_repository_id
       AND c.disabled_at IS NULL
    RETURNING c.id
),
disabled_project_workspace_repositories AS (
    UPDATE project_workspace_repositories w
       SET disabled_at = now(),
           updated_at = now()
      FROM deleted_repository r
     WHERE w.org_id = r.org_id
       AND w.github_repository_id = r.github_repository_id
       AND w.disabled_at IS NULL
    RETURNING w.id
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
disabled_connections AS (
    UPDATE github_repository_connections c
       SET disabled_at = now(),
           updated_at = now()
      FROM deleted_repositories r
     WHERE c.org_id = r.org_id
       AND c.github_repository_id = r.github_repository_id
       AND c.disabled_at IS NULL
    RETURNING c.id
),
disabled_project_workspace_repositories AS (
    UPDATE project_workspace_repositories w
       SET disabled_at = now(),
           updated_at = now()
      FROM deleted_repositories r
     WHERE w.org_id = r.org_id
       AND w.github_repository_id = r.github_repository_id
       AND w.disabled_at IS NULL
    RETURNING w.id
)
SELECT *
  FROM deleted_repositories;

-- name: EnableGitHubRepositoryConnection :one
INSERT INTO github_repository_connections (
    id,
    org_id,
    github_repository_id,
    enabled_by_user_id
)
SELECT sqlc.arg(id),
       r.org_id,
       r.github_repository_id,
       sqlc.arg(enabled_by_user_id)
  FROM github_repositories r
  JOIN github_app_installations i
    ON i.org_id = r.org_id
   AND i.installation_id = r.installation_id
 WHERE r.org_id = sqlc.arg(org_id)
   AND r.github_repository_id = sqlc.arg(github_repository_id)
   AND r.deleted_at IS NULL
   AND i.suspended_at IS NULL
   AND i.deleted_at IS NULL
ON CONFLICT (org_id, github_repository_id) DO UPDATE
   SET enabled_by_user_id = EXCLUDED.enabled_by_user_id,
       disabled_at = NULL,
       updated_at = now()
RETURNING *;

-- name: DisableGitHubRepositoryConnection :one
WITH disabled_connection AS (
    UPDATE github_repository_connections c
       SET disabled_at = now(),
           updated_at = now()
      FROM github_repositories r
      JOIN github_app_installations i
        ON i.org_id = r.org_id
       AND i.installation_id = r.installation_id
     WHERE c.org_id = sqlc.arg(org_id)
       AND c.github_repository_id = sqlc.arg(github_repository_id)
       AND c.disabled_at IS NULL
       AND r.org_id = c.org_id
       AND r.github_repository_id = c.github_repository_id
       AND r.deleted_at IS NULL
       AND i.suspended_at IS NULL
       AND i.deleted_at IS NULL
    RETURNING c.*
),
disabled_project_workspace_repositories AS (
    UPDATE project_workspace_repositories w
       SET disabled_at = now(),
           updated_at = now()
      FROM disabled_connection c
     WHERE w.org_id = c.org_id
       AND w.github_repository_id = c.github_repository_id
       AND w.disabled_at IS NULL
    RETURNING w.id
)
SELECT *
  FROM disabled_connection;
