-- name: AcquireWorkspaceInstanceLease :one
INSERT INTO workspace_leases (
    id,
    org_id,
    project_id,
    environment_id,
    workspace_id,
    materialization_id,
    lease_kind,
    owner_exec_id,
    base_version_id,
    acquired_version_id,
    acquired_fencing_generation,
    fencing_token,
    heartbeat_token,
    expires_at
)
SELECT sqlc.arg(id),
       workspace_materializations.org_id,
       workspace_materializations.project_id,
       workspace_materializations.environment_id,
       workspace_materializations.workspace_id,
       workspace_materializations.id,
       'instance',
       sqlc.arg(owner_exec_id),
       workspace_materializations.base_version_id,
       workspaces.current_version_id,
       workspace_materializations.fencing_generation,
       sqlc.arg(fencing_token),
       sqlc.arg(heartbeat_token),
       sqlc.arg(expires_at)
  FROM workspace_materializations
  JOIN workspaces
    ON workspaces.org_id = workspace_materializations.org_id
   AND workspaces.project_id = workspace_materializations.project_id
   AND workspaces.environment_id = workspace_materializations.environment_id
   AND workspaces.id = workspace_materializations.workspace_id
 WHERE workspace_materializations.org_id = sqlc.arg(org_id)
   AND workspace_materializations.project_id = sqlc.arg(project_id)
   AND workspace_materializations.environment_id = sqlc.arg(environment_id)
   AND workspace_materializations.workspace_id = sqlc.arg(workspace_id)
   AND workspace_materializations.id = sqlc.arg(materialization_id)
   AND workspace_materializations.state IN ('running', 'paused')
RETURNING *;

-- name: AcquireWorkspaceWriteLease :one
WITH fenced_materialization AS (
    UPDATE workspace_materializations
       SET fencing_generation = workspace_materializations.fencing_generation + 1,
           updated_at = now()
     WHERE workspace_materializations.org_id = sqlc.arg(org_id)
       AND workspace_materializations.project_id = sqlc.arg(project_id)
       AND workspace_materializations.environment_id = sqlc.arg(environment_id)
       AND workspace_materializations.workspace_id = sqlc.arg(workspace_id)
       AND workspace_materializations.id = sqlc.arg(materialization_id)
       AND workspace_materializations.state IN ('running', 'paused')
    RETURNING *
)
INSERT INTO workspace_leases (
    id,
    org_id,
    project_id,
    environment_id,
    workspace_id,
    materialization_id,
    lease_kind,
    owner_exec_id,
    owner_pty_session_id,
    base_version_id,
    acquired_version_id,
    acquired_fencing_generation,
    fencing_token,
    heartbeat_token,
    expires_at
)
SELECT sqlc.arg(id),
       fenced_materialization.org_id,
       fenced_materialization.project_id,
       fenced_materialization.environment_id,
       fenced_materialization.workspace_id,
       fenced_materialization.id,
       'write',
       sqlc.arg(owner_exec_id),
       sqlc.arg(owner_pty_session_id),
       fenced_materialization.base_version_id,
       workspaces.current_version_id,
       fenced_materialization.fencing_generation,
       sqlc.arg(fencing_token),
       sqlc.arg(heartbeat_token),
       sqlc.arg(expires_at)
  FROM fenced_materialization
  JOIN workspaces
    ON workspaces.org_id = fenced_materialization.org_id
   AND workspaces.project_id = fenced_materialization.project_id
   AND workspaces.environment_id = fenced_materialization.environment_id
   AND workspaces.id = fenced_materialization.workspace_id
RETURNING *;

-- name: GetWorkspaceLease :one
SELECT *
  FROM workspace_leases
 WHERE org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND workspace_id = sqlc.arg(workspace_id)
   AND id = sqlc.arg(id);

-- name: MarkWorkspaceWriteLeaseDirty :one
WITH active_writer AS (
    SELECT workspace_leases.org_id,
           workspace_leases.project_id,
           workspace_leases.environment_id,
           workspace_leases.workspace_id,
           workspace_leases.materialization_id,
           workspace_leases.acquired_fencing_generation
      FROM workspace_leases
     WHERE workspace_leases.org_id = sqlc.arg(org_id)
       AND workspace_leases.id = sqlc.arg(write_lease_id)
       AND workspace_leases.fencing_token = sqlc.arg(fencing_token)
       AND workspace_leases.lease_kind = 'write'
       AND workspace_leases.state = 'active'
       AND workspace_leases.expires_at > now()
),
updated_materialization AS (
    UPDATE workspace_materializations
       SET dirty_generation = dirty_generation + 1,
           updated_at = now()
      FROM active_writer
     WHERE workspace_materializations.org_id = active_writer.org_id
       AND workspace_materializations.project_id = active_writer.project_id
       AND workspace_materializations.environment_id = active_writer.environment_id
       AND workspace_materializations.workspace_id = active_writer.workspace_id
       AND workspace_materializations.id = active_writer.materialization_id
       AND workspace_materializations.fencing_generation = active_writer.acquired_fencing_generation
    RETURNING workspace_materializations.*
),
updated_workspace AS (
    UPDATE workspaces
       SET dirty_state = 'dirty',
           updated_at = now()
      FROM updated_materialization
     WHERE workspaces.org_id = updated_materialization.org_id
       AND workspaces.project_id = updated_materialization.project_id
       AND workspaces.environment_id = updated_materialization.environment_id
       AND workspaces.id = updated_materialization.workspace_id
    RETURNING workspaces.id
)
SELECT * FROM updated_materialization;

-- name: PromoteWorkspaceCapture :one
WITH active_writer AS (
    SELECT workspace_leases.*
      FROM workspace_leases
     WHERE workspace_leases.org_id = sqlc.arg(org_id)
       AND workspace_leases.id = sqlc.arg(write_lease_id)
       AND workspace_leases.fencing_token = sqlc.arg(fencing_token)
       AND workspace_leases.lease_kind = 'write'
       AND workspace_leases.state = 'active'
       AND workspace_leases.expires_at > now()
),
active_materialization AS (
    SELECT workspace_materializations.*
      FROM workspace_materializations
      JOIN active_writer
        ON active_writer.org_id = workspace_materializations.org_id
       AND active_writer.project_id = workspace_materializations.project_id
       AND active_writer.environment_id = workspace_materializations.environment_id
       AND active_writer.workspace_id = workspace_materializations.workspace_id
       AND active_writer.materialization_id = workspace_materializations.id
       AND active_writer.acquired_fencing_generation = workspace_materializations.fencing_generation
     WHERE workspace_materializations.dirty_generation = sqlc.arg(dirty_generation)
),
verified_artifact AS (
    SELECT artifacts.id
      FROM artifacts
      JOIN cas_objects
        ON cas_objects.digest = artifacts.digest
     WHERE artifacts.org_id = sqlc.arg(org_id)
       AND artifacts.id = sqlc.arg(artifact_id)
       AND artifacts.kind = 'workspace_version'
       AND artifacts.size_bytes = sqlc.arg(size_bytes)
       AND cas_objects.size_bytes = artifacts.size_bytes
       AND btrim(sqlc.arg(artifact_encoding)::text) <> ''
       AND btrim(sqlc.arg(content_digest)::text) <> ''
),
created_version AS (
    INSERT INTO workspace_versions (
        id,
        org_id,
        project_id,
        environment_id,
        workspace_id,
        parent_version_id,
        source_materialization_id,
        source_write_lease_id,
        kind,
        state,
        artifact_id,
        artifact_encoding,
        artifact_entry_count,
        content_digest,
        size_bytes,
        message,
        promoted_at
    )
    SELECT sqlc.arg(version_id),
           active_writer.org_id,
           active_writer.project_id,
           active_writer.environment_id,
           active_writer.workspace_id,
           active_writer.acquired_version_id,
           active_writer.materialization_id,
           active_writer.id,
           sqlc.arg(kind),
           'ready',
           sqlc.arg(artifact_id),
           sqlc.arg(artifact_encoding),
           sqlc.arg(artifact_entry_count),
           sqlc.arg(content_digest),
           sqlc.arg(size_bytes),
           sqlc.arg(message),
           now()
      FROM active_writer
      JOIN active_materialization ON active_materialization.id = active_writer.materialization_id
      JOIN verified_artifact ON verified_artifact.id = sqlc.arg(artifact_id)
    RETURNING *
),
promoted_workspace AS (
    UPDATE workspaces
       SET current_version_id = created_version.id,
           dirty_state = 'clean',
           updated_at = now()
      FROM created_version
     WHERE workspaces.org_id = created_version.org_id
       AND workspaces.project_id = created_version.project_id
       AND workspaces.environment_id = created_version.environment_id
       AND workspaces.id = created_version.workspace_id
       AND workspaces.current_version_id IS NOT DISTINCT FROM created_version.parent_version_id
    RETURNING workspaces.id
),
cleaned_materialization AS (
    UPDATE workspace_materializations
       SET dirty_generation = 0,
           updated_at = now()
      FROM created_version
     WHERE workspace_materializations.org_id = created_version.org_id
       AND workspace_materializations.project_id = created_version.project_id
       AND workspace_materializations.environment_id = created_version.environment_id
       AND workspace_materializations.workspace_id = created_version.workspace_id
       AND workspace_materializations.id = created_version.source_materialization_id
       AND workspace_materializations.dirty_generation = sqlc.arg(dirty_generation)
    RETURNING workspace_materializations.id
)
SELECT created_version.*
  FROM created_version
  JOIN promoted_workspace ON promoted_workspace.id = created_version.workspace_id
  JOIN cleaned_materialization ON cleaned_materialization.id = created_version.source_materialization_id;

-- name: ReleaseWorkspaceLease :one
UPDATE workspace_leases
   SET state = 'released',
       released_at = now(),
       updated_at = now()
 WHERE org_id = sqlc.arg(org_id)
   AND id = sqlc.arg(id)
   AND fencing_token = sqlc.arg(fencing_token)
   AND state = 'active'
RETURNING *;
