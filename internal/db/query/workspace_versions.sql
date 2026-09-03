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
  JOIN workspaces
    ON workspaces.environment_id = workspace_versions.environment_id
   AND workspaces.id = workspace_versions.workspace_id
  JOIN environments ON environments.id = workspaces.environment_id
  LEFT JOIN artifacts ON artifacts.environment_id = workspace_versions.environment_id
                     AND artifacts.id = workspace_versions.artifact_id
 WHERE environments.org_id = sqlc.arg(org_id)
   AND environments.project_id = sqlc.arg(project_id)
   AND workspace_versions.environment_id = sqlc.arg(environment_id)
   AND workspace_versions.workspace_id = sqlc.arg(workspace_id)
   AND workspace_versions.id = sqlc.arg(version_id)
   AND workspace_versions.state IN ('committed', 'private');

-- name: GetCheckpointWorkspaceBaseAuthority :one
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
  JOIN workspaces
    ON workspaces.environment_id = workspace_versions.environment_id
   AND workspaces.id = workspace_versions.workspace_id
  JOIN environments ON environments.id = workspaces.environment_id
  LEFT JOIN artifacts ON artifacts.environment_id = workspace_versions.environment_id
                     AND artifacts.id = workspace_versions.artifact_id
 WHERE environments.org_id = sqlc.arg(org_id)
   AND environments.project_id = sqlc.arg(project_id)
   AND workspace_versions.environment_id = sqlc.arg(environment_id)
   AND workspace_versions.workspace_id = sqlc.arg(workspace_id)
   AND workspace_versions.id = sqlc.arg(version_id)
   AND workspace_versions.state IN ('committed', 'private');
