-- name: ListGitHubInstallations :many
SELECT *
  FROM github_app_installations
 WHERE org_id = $1
 ORDER BY lower(account_login), updated_at DESC;

-- name: GetKnownGitHubInstallationByInstallationID :one
SELECT *
  FROM github_app_installations
 WHERE installation_id = $1
   AND deleted_at IS NULL
 ORDER BY updated_at DESC
 LIMIT 1;

-- name: UpsertGitHubInstallation :one
INSERT INTO github_app_installations (
    id,
    org_id,
    installation_id,
    account_login,
    account_type,
    repository_selection,
    html_url
) VALUES (
    $1, $2, $3, $4, $5, $6, $7
)
ON CONFLICT (org_id, installation_id) DO UPDATE
   SET account_login = EXCLUDED.account_login,
       account_type = EXCLUDED.account_type,
       repository_selection = EXCLUDED.repository_selection,
       html_url = EXCLUDED.html_url,
       suspended_at = NULL,
       deleted_at = NULL,
       updated_at = now()
RETURNING *;

-- name: SuspendGitHubInstallation :one
UPDATE github_app_installations
   SET suspended_at = now(),
       updated_at = now()
 WHERE org_id = $1
   AND installation_id = $2
RETURNING *;

-- name: SuspendGitHubInstallationByInstallationID :many
UPDATE github_app_installations
   SET suspended_at = now(),
       updated_at = now()
 WHERE installation_id = $1
   AND deleted_at IS NULL
RETURNING *;

-- name: DeleteGitHubInstallationByInstallationID :many
UPDATE github_app_installations
   SET deleted_at = now(),
       suspended_at = NULL,
       updated_at = now()
 WHERE installation_id = $1
RETURNING *;

-- name: DeleteGitHubInstallation :one
UPDATE github_app_installations
   SET deleted_at = now(),
       suspended_at = NULL,
       updated_at = now()
 WHERE org_id = $1
   AND installation_id = $2
RETURNING *;
