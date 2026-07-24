-- name: GetWorkspaceVersion :one
SELECT *
  FROM workspace_versions
 WHERE org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND workspace_id = sqlc.arg(workspace_id)
   AND id = sqlc.arg(id)
   AND state = 'committed';

-- name: GetWorkspaceVersionByPublicID :one
SELECT *
  FROM workspace_versions
 WHERE org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND workspace_id = sqlc.arg(workspace_id)
   AND public_id = sqlc.arg(public_id)
   AND state = 'committed';

-- name: GetWorkspaceResetTargetAuthority :one
SELECT workspace_versions.id AS version_id,
       workspace_versions.parent_version_id,
       workspace_versions.artifact_id,
       workspace_versions.artifact_kind,
       workspace_versions.kind AS version_kind,
       workspace_versions.content_digest,
       workspace_versions.size_bytes AS logical_size_bytes,
       workspace_versions.entry_count,
       workspace_versions.source_workspace_lease_id,
       workspace_versions.ownership_generation,
       workspace_versions.writer_generation,
       artifacts.kind AS artifact_row_kind,
       artifacts.digest AS artifact_digest,
       artifacts.size_bytes AS artifact_size_bytes,
       artifacts.media_type AS artifact_media_type
  FROM workspace_versions
  LEFT JOIN artifacts ON artifacts.org_id = workspace_versions.org_id
                     AND artifacts.project_id = workspace_versions.project_id
                     AND artifacts.environment_id = workspace_versions.environment_id
                     AND artifacts.id = workspace_versions.artifact_id
 WHERE workspace_versions.org_id = sqlc.arg(org_id)
   AND workspace_versions.project_id = sqlc.arg(project_id)
   AND workspace_versions.environment_id = sqlc.arg(environment_id)
   AND workspace_versions.workspace_id = sqlc.arg(workspace_id)
   AND workspace_versions.id = sqlc.arg(version_id)
   AND workspace_versions.state = 'committed';

-- name: ListWorkspaceVersions :many
SELECT *
  FROM workspace_versions
 WHERE org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND workspace_id = sqlc.arg(workspace_id)
   AND state = 'committed'
   AND (sqlc.narg(kind)::workspace_version_kind IS NULL OR kind = sqlc.narg(kind)::workspace_version_kind)
 ORDER BY created_at DESC, id DESC
 LIMIT sqlc.arg(limit_count);
