-- name: AcquireWorkspaceInstanceLease :one
INSERT INTO workspace_leases (
    id, org_id, project_id, environment_id, region_id, worker_group_id,
    worker_instance_id, worker_epoch, runtime_instance_id, workspace_id,
    workspace_mount_id, lease_kind, owner_run_id, owner_process_id,
    base_version_id, acquired_version_id, acquired_fencing_generation,
    fencing_token, expires_at
)
SELECT sqlc.arg(id), workspace_mounts.org_id, workspace_mounts.project_id,
       workspace_mounts.environment_id, workspace_mounts.region_id,
       workspace_mounts.worker_group_id, workspace_mounts.worker_instance_id,
       workspace_mounts.worker_epoch, workspace_mounts.runtime_instance_id,
       workspace_mounts.workspace_id, workspace_mounts.id, 'instance',
       sqlc.narg(owner_run_id), sqlc.narg(owner_process_id),
       workspace_mounts.base_version_id, sqlc.narg(acquired_version_id),
       workspace_mounts.fencing_generation, sqlc.arg(fencing_token), sqlc.arg(expires_at)
  FROM workspace_mounts
 WHERE workspace_mounts.org_id = sqlc.arg(org_id)
   AND workspace_mounts.workspace_id = sqlc.arg(workspace_id)
   AND workspace_mounts.id = sqlc.arg(workspace_mount_id)
   AND workspace_mounts.state = 'mounted'
RETURNING *;

-- name: AcquireWorkspaceWriteLease :one
WITH locked AS (
    SELECT * FROM workspace_mounts
     WHERE workspace_mounts.org_id = sqlc.arg(org_id) AND workspace_mounts.workspace_id = sqlc.arg(workspace_id)
       AND workspace_mounts.id = sqlc.arg(workspace_mount_id) AND workspace_mounts.state = 'mounted'
     FOR UPDATE
), fenced AS (
    UPDATE workspace_mounts
       SET fencing_generation = workspace_mounts.fencing_generation + 1, updated_at = now()
      FROM locked WHERE workspace_mounts.id = locked.id
    RETURNING workspace_mounts.*
)
INSERT INTO workspace_leases (
    id, org_id, project_id, environment_id, region_id, worker_group_id,
    worker_instance_id, worker_epoch, runtime_instance_id, workspace_id,
    workspace_mount_id, lease_kind, owner_run_id, owner_process_id,
    base_version_id, acquired_version_id, acquired_fencing_generation,
    fencing_token, expires_at
)
SELECT sqlc.arg(id), fenced.org_id, fenced.project_id, fenced.environment_id,
       fenced.region_id, fenced.worker_group_id, fenced.worker_instance_id,
       fenced.worker_epoch, fenced.runtime_instance_id, fenced.workspace_id,
       fenced.id, 'write', sqlc.narg(owner_run_id), sqlc.narg(owner_process_id),
       fenced.base_version_id, sqlc.narg(acquired_version_id), fenced.fencing_generation,
       sqlc.arg(fencing_token), sqlc.arg(expires_at)
  FROM fenced
RETURNING *;

-- name: GetWorkspaceLease :one
SELECT * FROM workspace_leases
 WHERE org_id = sqlc.arg(org_id) AND workspace_id = sqlc.arg(workspace_id)
   AND id = sqlc.arg(id);

-- name: MarkWorkspaceWriteLeaseDirty :one
WITH valid AS (
    SELECT workspace_leases.* FROM workspace_leases
     WHERE workspace_leases.org_id = sqlc.arg(org_id) AND workspace_leases.workspace_id = sqlc.arg(workspace_id)
       AND workspace_leases.id = sqlc.arg(id) AND workspace_leases.lease_kind = 'write' AND workspace_leases.state = 'active'
       AND workspace_leases.fencing_token = sqlc.arg(fencing_token)
       AND workspace_leases.acquired_fencing_generation = sqlc.arg(fencing_generation)
       AND workspace_leases.expires_at > now() FOR UPDATE
)
UPDATE workspaces SET dirty_state = 'dirty', updated_at = now()
  FROM valid WHERE workspaces.org_id = valid.org_id AND workspaces.id = valid.workspace_id
RETURNING valid.*;

-- name: PromoteWorkspaceCapture :one
WITH valid AS (
    UPDATE workspace_leases SET acquired_version_id = sqlc.arg(workspace_version_id),
                                renewed_at = now(), updated_at = now()
     WHERE workspace_leases.org_id = sqlc.arg(org_id) AND workspace_leases.workspace_id = sqlc.arg(workspace_id)
       AND workspace_leases.id = sqlc.arg(id) AND workspace_leases.lease_kind = 'write' AND workspace_leases.state = 'active'
       AND workspace_leases.fencing_token = sqlc.arg(fencing_token)
       AND workspace_leases.acquired_fencing_generation = sqlc.arg(fencing_generation)
    RETURNING *
)
UPDATE workspaces SET current_version_id = sqlc.arg(workspace_version_id),
                      dirty_state = 'clean', updated_at = now()
  FROM valid WHERE workspaces.org_id = valid.org_id AND workspaces.id = valid.workspace_id
RETURNING valid.*;

-- name: CreateAndPromoteWorkspaceCapture :one
WITH existing AS (
    SELECT workspace_versions.*
      FROM run_waits
      JOIN workspace_versions
        ON workspace_versions.org_id = run_waits.org_id
       AND workspace_versions.workspace_id = run_waits.reserved_workspace_id
       AND workspace_versions.id = run_waits.reserved_workspace_version_id
     WHERE run_waits.org_id = sqlc.arg(org_id)
       AND run_waits.run_id = sqlc.arg(run_id)
       AND run_waits.id = sqlc.arg(run_wait_id)
       AND run_waits.checkpoint_request_version = sqlc.arg(checkpoint_request_version)
       AND run_waits.checkpoint_attempt_id = sqlc.arg(checkpoint_attempt_id)
       AND workspace_versions.content_digest = sqlc.arg(content_digest)
       AND workspace_versions.size_bytes = sqlc.arg(size_bytes)
       AND workspace_versions.artifact_encoding = sqlc.arg(artifact_encoding)
), valid AS (
    SELECT workspace_leases.*
      FROM workspace_leases
      JOIN run_waits ON run_waits.org_id = workspace_leases.org_id
                    AND run_waits.run_id = workspace_leases.owner_run_id
                    AND run_waits.id = sqlc.arg(run_wait_id)
     WHERE workspace_leases.org_id = sqlc.arg(org_id)
       AND workspace_leases.workspace_id = sqlc.arg(workspace_id)
       AND workspace_leases.id = sqlc.arg(write_lease_id)
       AND workspace_leases.workspace_mount_id = sqlc.arg(workspace_mount_id)
       AND workspace_leases.owner_run_id = sqlc.arg(run_id)
       AND workspace_leases.worker_instance_id = sqlc.arg(worker_instance_id)
       AND workspace_leases.worker_epoch = sqlc.arg(worker_epoch)
       AND workspace_leases.lease_kind = 'write'
       AND workspace_leases.state = 'active'
       AND workspace_leases.fencing_token = sqlc.arg(fencing_token)
       AND workspace_leases.acquired_fencing_generation = sqlc.arg(fencing_generation)
       AND workspace_leases.expires_at > now()
       AND run_waits.state = 'checkpointing'
       AND run_waits.checkpoint_request_version = sqlc.arg(checkpoint_request_version)
       AND run_waits.checkpoint_attempt_id = sqlc.arg(checkpoint_attempt_id)
       AND run_waits.reserved_workspace_id IS NULL
     FOR UPDATE OF workspace_leases, run_waits
), created AS (
    INSERT INTO workspace_versions (
        id, public_id, org_id, project_id, environment_id, workspace_id,
        parent_version_id, source_workspace_mount_id, source_write_lease_id,
        produced_by_run_id, kind, state, artifact_id, artifact_encoding,
        artifact_entry_count, content_digest, size_bytes, message, promoted_at
    )
    SELECT sqlc.arg(workspace_version_id), sqlc.arg(workspace_version_public_id),
           valid.org_id, valid.project_id, valid.environment_id, valid.workspace_id,
           COALESCE(valid.acquired_version_id, valid.base_version_id), valid.workspace_mount_id,
           valid.id, valid.owner_run_id, 'system', 'ready', sqlc.arg(artifact_id),
           sqlc.arg(artifact_encoding), sqlc.arg(artifact_entry_count),
           sqlc.arg(content_digest), sqlc.arg(size_bytes),
           'system capture before parked wait', now()
      FROM valid
    RETURNING *
), updated_lease AS (
    UPDATE workspace_leases
       SET acquired_version_id = created.id, renewed_at = now(), updated_at = now()
      FROM created
     WHERE workspace_leases.org_id = created.org_id
       AND workspace_leases.id = created.source_write_lease_id
    RETURNING workspace_leases.id
), updated_workspace AS (
    UPDATE workspaces
       SET current_version_id = created.id, dirty_state = 'clean', updated_at = now()
      FROM created, updated_lease
     WHERE workspaces.org_id = created.org_id AND workspaces.id = created.workspace_id
    RETURNING workspaces.id
), updated_wait AS (
    UPDATE run_waits
       SET reserved_workspace_id = created.workspace_id,
           reserved_workspace_version_id = created.id, updated_at = now()
      FROM created, updated_workspace
     WHERE run_waits.org_id = created.org_id
       AND run_waits.run_id = created.produced_by_run_id
       AND run_waits.id = sqlc.arg(run_wait_id)
       AND run_waits.checkpoint_request_version = sqlc.arg(checkpoint_request_version)
       AND run_waits.checkpoint_attempt_id = sqlc.arg(checkpoint_attempt_id)
    RETURNING run_waits.id
)
SELECT created.* FROM created JOIN updated_wait ON true
UNION ALL
SELECT existing.* FROM existing
LIMIT 1;

-- name: ReleaseWorkspaceLease :one
UPDATE workspace_leases
   SET state = 'released', released_at = now(), terminal_at = now(),
       terminal_reason_code = sqlc.arg(reason_code), terminal_error = NULL,
       updated_at = now()
 WHERE org_id = sqlc.arg(org_id) AND workspace_id = sqlc.arg(workspace_id)
   AND id = sqlc.arg(id) AND state IN ('active','releasing')
   AND fencing_token = sqlc.arg(fencing_token)
   AND acquired_fencing_generation = sqlc.arg(fencing_generation)
RETURNING *;

-- name: ExpireWorkspaceLeases :many
WITH candidates AS (
    SELECT workspace_leases.id
      FROM workspace_leases
     WHERE workspace_leases.state IN ('active','releasing')
       AND workspace_leases.expires_at <= now()
     ORDER BY workspace_leases.expires_at, workspace_leases.id
     LIMIT sqlc.arg(limit_count)
     FOR UPDATE SKIP LOCKED
), expired AS (
    UPDATE workspace_leases
       SET state = 'expired', terminal_at = now(), terminal_reason_code = 'lease_expired',
           terminal_error = NULL, updated_at = now()
      FROM candidates
     WHERE workspace_leases.id = candidates.id
    RETURNING workspace_leases.*
), requested_mount_stop AS (
    UPDATE workspace_mounts
       SET state = 'unmounting', stopped_at = COALESCE(workspace_mounts.stopped_at, now()),
           fencing_generation = workspace_mounts.fencing_generation + 1, updated_at = now()
      FROM expired
     WHERE expired.lease_kind = 'write'
       AND workspace_mounts.id = expired.workspace_mount_id
       AND workspace_mounts.state IN ('mounting','mounted')
    RETURNING workspace_mounts.runtime_instance_id
), requested_runtime_close AS (
    UPDATE runtime_instances
       SET desired_state = 'closed', desired_version = runtime_instances.desired_version + 1,
           desired_at = now(), desired_reason = 'workspace_lease_expired', updated_at = now()
      FROM requested_mount_stop
     WHERE runtime_instances.id = requested_mount_stop.runtime_instance_id
       AND runtime_instances.desired_state <> 'closed'
       AND runtime_instances.observed_state IN ('allocated','preparing','ready')
    RETURNING runtime_instances.id
)
SELECT expired.* FROM expired;
