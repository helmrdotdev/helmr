-- name: GetWorkspaceVersion :one
SELECT *
  FROM workspace_versions
 WHERE org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND workspace_id = sqlc.arg(workspace_id)
   AND id = sqlc.arg(id)
   AND state = 'ready';

-- name: ListWorkspaceVersions :many
SELECT *
  FROM workspace_versions
 WHERE org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND workspace_id = sqlc.arg(workspace_id)
   AND state = 'ready'
   AND (sqlc.narg(kind)::workspace_version_kind IS NULL OR kind = sqlc.narg(kind)::workspace_version_kind)
 ORDER BY created_at DESC, id DESC
 LIMIT sqlc.arg(limit_count);
