-- name: EnsureWorkspaceMaterializationRequested :one
WITH request_params AS MATERIALIZED (
    SELECT sqlc.arg(priority)::integer AS priority
),
locked_workspace AS MATERIALIZED (
    SELECT workspaces.*
      FROM workspaces
     WHERE workspaces.org_id = sqlc.arg(org_id)
       AND workspaces.project_id = sqlc.arg(project_id)
       AND workspaces.environment_id = sqlc.arg(environment_id)
       AND workspaces.id = sqlc.arg(workspace_id)
       AND workspaces.state = 'active'
       AND workspaces.archived_at IS NULL
       AND workspaces.deleted_at IS NULL
     FOR UPDATE
),
existing_runnable AS MATERIALIZED (
    SELECT workspace_materializations.*
      FROM locked_workspace
      JOIN workspace_materializations
        ON workspace_materializations.org_id = locked_workspace.org_id
       AND workspace_materializations.project_id = locked_workspace.project_id
       AND workspace_materializations.environment_id = locked_workspace.environment_id
       AND workspace_materializations.workspace_id = locked_workspace.id
     WHERE workspace_materializations.state IN ('requested', 'materializing', 'restoring', 'running')
),
existing_active_non_runnable AS MATERIALIZED (
    SELECT workspace_materializations.*
      FROM locked_workspace
      JOIN workspace_materializations
        ON workspace_materializations.org_id = locked_workspace.org_id
       AND workspace_materializations.project_id = locked_workspace.project_id
       AND workspace_materializations.environment_id = locked_workspace.environment_id
       AND workspace_materializations.workspace_id = locked_workspace.id
     WHERE workspace_materializations.state IN ('pausing', 'paused', 'capturing', 'stopping')
),
priority_bumped_existing AS (
    UPDATE workspace_materializations
       SET priority = greatest(workspace_materializations.priority, request_params.priority),
           updated_at = now()
      FROM existing_runnable, request_params
     WHERE workspace_materializations.org_id = existing_runnable.org_id
       AND workspace_materializations.project_id = existing_runnable.project_id
       AND workspace_materializations.environment_id = existing_runnable.environment_id
       AND workspace_materializations.workspace_id = existing_runnable.workspace_id
       AND workspace_materializations.id = existing_runnable.id
       AND existing_runnable.state IN ('requested', 'materializing', 'restoring')
       AND workspace_materializations.priority < request_params.priority
    RETURNING workspace_materializations.*, false::boolean AS inserted
),
unchanged_existing AS MATERIALIZED (
    SELECT existing_runnable.*, false::boolean AS inserted
      FROM existing_runnable, request_params
     WHERE existing_runnable.state = 'running'
        OR existing_runnable.priority >= request_params.priority
),
inserted AS (
    INSERT INTO workspace_materializations (
        id,
        org_id,
        project_id,
        environment_id,
        workspace_id,
        deployment_sandbox_id,
        sandbox_fingerprint,
        base_version_id,
        priority,
        requested_cpu_millis,
        requested_memory_mib,
        requested_disk_mib,
        requested_execution_slots,
        image_artifact_id,
        image_artifact_format,
        rootfs_digest,
        image_digest,
        image_format,
        workspace_artifact_id,
        workspace_artifact_encoding,
        workspace_artifact_entry_count,
        workspace_artifact_digest,
        workspace_artifact_size_bytes,
        workspace_artifact_media_type,
        workspace_mount_path,
        runtime_abi,
        guestd_abi,
        adapter_abi,
        state,
        request
    )
    SELECT sqlc.arg(id),
           workspaces.org_id,
           workspaces.project_id,
           workspaces.environment_id,
           workspaces.id,
           workspaces.deployment_sandbox_id,
           workspaces.sandbox_fingerprint,
           workspaces.current_version_id,
           request_params.priority,
           coalesce((deployment_sandboxes.resource_floor->>'milli_cpu')::integer, 1000),
           coalesce((deployment_sandboxes.resource_floor->>'memory_mib')::integer, 1024),
           deployment_sandboxes.disk_floor_mib,
           1,
           deployment_sandboxes.image_artifact_id,
           deployment_sandboxes.image_artifact_format,
           deployment_sandboxes.rootfs_digest,
           deployment_sandboxes.image_digest,
           deployment_sandboxes.image_format,
           current_workspace_version.artifact_id,
           current_workspace_version.artifact_encoding,
           current_workspace_version.artifact_entry_count,
           workspace_artifact.digest,
           workspace_artifact.size_bytes,
           workspace_artifact.media_type,
           deployment_sandboxes.workspace_mount_path,
           deployment_sandboxes.runtime_abi,
           deployment_sandboxes.guestd_abi,
           deployment_sandboxes.adapter_abi,
           'requested',
           coalesce(sqlc.arg(request)::jsonb, '{}'::jsonb)
      FROM locked_workspace AS workspaces
      CROSS JOIN request_params
      JOIN deployment_sandboxes
        ON deployment_sandboxes.org_id = workspaces.org_id
       AND deployment_sandboxes.project_id = workspaces.project_id
       AND deployment_sandboxes.environment_id = workspaces.environment_id
       AND deployment_sandboxes.id = workspaces.deployment_sandbox_id
      JOIN artifacts AS image_artifact
        ON image_artifact.org_id = deployment_sandboxes.org_id
       AND image_artifact.project_id = deployment_sandboxes.project_id
       AND image_artifact.environment_id = deployment_sandboxes.environment_id
       AND image_artifact.id = deployment_sandboxes.image_artifact_id
       AND image_artifact.kind = 'sandbox_image'
       AND image_artifact.media_type = 'application/vnd.helmr.sandbox-image.v0.oci-tar'
      JOIN workspace_versions AS current_workspace_version
        ON current_workspace_version.org_id = workspaces.org_id
       AND current_workspace_version.project_id = workspaces.project_id
       AND current_workspace_version.environment_id = workspaces.environment_id
       AND current_workspace_version.workspace_id = workspaces.id
       AND current_workspace_version.id = workspaces.current_version_id
       AND current_workspace_version.state = 'ready'
      JOIN artifacts AS workspace_artifact
        ON workspace_artifact.org_id = current_workspace_version.org_id
       AND workspace_artifact.project_id = current_workspace_version.project_id
       AND workspace_artifact.environment_id = current_workspace_version.environment_id
       AND workspace_artifact.id = current_workspace_version.artifact_id
       AND workspace_artifact.kind = 'workspace_version'
       AND workspace_artifact.media_type = 'application/vnd.helmr.workspace.v0.tar'
     WHERE workspaces.org_id = sqlc.arg(org_id)
       AND workspaces.project_id = sqlc.arg(project_id)
       AND workspaces.environment_id = sqlc.arg(environment_id)
       AND workspaces.id = sqlc.arg(workspace_id)
       AND workspaces.state = 'active'
       AND workspaces.archived_at IS NULL
       AND workspaces.deleted_at IS NULL
       AND NOT EXISTS (SELECT 1 FROM existing_runnable)
       AND NOT EXISTS (SELECT 1 FROM existing_active_non_runnable)
    ON CONFLICT (workspace_id) WHERE state IN ('requested', 'materializing', 'restoring', 'running', 'pausing', 'paused', 'capturing', 'stopping')
    DO UPDATE SET updated_at = workspace_materializations.updated_at
    WHERE workspace_materializations.state IN ('requested', 'materializing', 'restoring', 'running')
    RETURNING workspace_materializations.*, (workspace_materializations.xmax = 0)::boolean AS inserted
)
SELECT * FROM priority_bumped_existing
UNION ALL
SELECT * FROM unchanged_existing
UNION ALL
SELECT * FROM inserted
LIMIT 1;

-- name: GetWorkspaceMaterializationPrerequisites :one
SELECT workspaces.id AS workspace_id,
       workspaces.current_version_id,
       current_workspace_version.id AS current_workspace_version_id,
       current_workspace_version.state AS current_workspace_version_state,
       current_workspace_version.artifact_id AS current_workspace_artifact_id,
       workspace_artifact.id AS workspace_artifact_id,
       workspace_artifact.digest AS workspace_artifact_digest,
       workspace_artifact.size_bytes AS workspace_artifact_size_bytes,
       workspace_artifact.media_type AS workspace_artifact_media_type,
       deployment_sandboxes.id AS deployment_sandbox_id,
       deployment_sandboxes.image_artifact_id AS sandbox_image_artifact_id,
       image_artifact.id AS image_artifact_id,
       image_artifact.digest AS image_artifact_digest,
       image_artifact.size_bytes AS image_artifact_size_bytes,
       image_artifact.media_type AS image_artifact_media_type,
       active_materialization.state AS active_materialization_state
  FROM workspaces
  JOIN deployment_sandboxes
    ON deployment_sandboxes.org_id = workspaces.org_id
   AND deployment_sandboxes.project_id = workspaces.project_id
   AND deployment_sandboxes.environment_id = workspaces.environment_id
   AND deployment_sandboxes.id = workspaces.deployment_sandbox_id
  LEFT JOIN artifacts AS image_artifact
    ON image_artifact.org_id = deployment_sandboxes.org_id
   AND image_artifact.project_id = deployment_sandboxes.project_id
   AND image_artifact.environment_id = deployment_sandboxes.environment_id
   AND image_artifact.id = deployment_sandboxes.image_artifact_id
  LEFT JOIN workspace_versions AS current_workspace_version
    ON current_workspace_version.org_id = workspaces.org_id
   AND current_workspace_version.project_id = workspaces.project_id
   AND current_workspace_version.environment_id = workspaces.environment_id
   AND current_workspace_version.workspace_id = workspaces.id
   AND current_workspace_version.id = workspaces.current_version_id
  LEFT JOIN artifacts AS workspace_artifact
    ON workspace_artifact.org_id = current_workspace_version.org_id
   AND workspace_artifact.project_id = current_workspace_version.project_id
   AND workspace_artifact.environment_id = current_workspace_version.environment_id
   AND workspace_artifact.id = current_workspace_version.artifact_id
  LEFT JOIN workspace_materializations AS active_materialization
    ON active_materialization.org_id = workspaces.org_id
   AND active_materialization.project_id = workspaces.project_id
   AND active_materialization.environment_id = workspaces.environment_id
   AND active_materialization.workspace_id = workspaces.id
   AND active_materialization.state IN ('requested', 'materializing', 'restoring', 'running', 'pausing', 'paused', 'capturing', 'stopping')
 WHERE workspaces.org_id = sqlc.arg(org_id)
   AND workspaces.project_id = sqlc.arg(project_id)
   AND workspaces.environment_id = sqlc.arg(environment_id)
   AND workspaces.id = sqlc.arg(workspace_id)
   AND workspaces.state = 'active'
   AND workspaces.archived_at IS NULL
   AND workspaces.deleted_at IS NULL;

-- name: ClaimWorkspaceMaterialization :one
WITH candidate AS (
    SELECT workspace_materializations.id,
           workspace_materializations.org_id
      FROM workspace_materializations
      JOIN deployment_sandboxes
        ON deployment_sandboxes.org_id = workspace_materializations.org_id
       AND deployment_sandboxes.project_id = workspace_materializations.project_id
       AND deployment_sandboxes.environment_id = workspace_materializations.environment_id
       AND deployment_sandboxes.id = workspace_materializations.deployment_sandbox_id
       AND deployment_sandboxes.fingerprint = workspace_materializations.sandbox_fingerprint
      JOIN deployments
        ON deployments.org_id = deployment_sandboxes.org_id
       AND deployments.project_id = deployment_sandboxes.project_id
       AND deployments.environment_id = deployment_sandboxes.environment_id
       AND deployments.id = deployment_sandboxes.deployment_id
      JOIN worker_instances
        ON worker_instances.id = sqlc.arg(worker_instance_id)
       AND worker_instances.worker_group_id = deployments.worker_group_id
     WHERE workspace_materializations.state IN ('requested', 'materializing', 'restoring')
       AND workspace_materializations.dead_lettered_at IS NULL
       AND (
           workspace_materializations.state = 'requested'
           OR workspace_materializations.reservation_expires_at <= now()
       )
       AND NOT EXISTS (
           SELECT 1
             FROM workspace_leases
            WHERE workspace_leases.org_id = workspace_materializations.org_id
              AND workspace_leases.workspace_id = workspace_materializations.workspace_id
              AND workspace_leases.materialization_id = workspace_materializations.id
              AND workspace_leases.lease_kind = 'write'
              AND workspace_leases.state IN ('active', 'releasing')
              AND workspace_leases.expires_at > now()
       )
       AND workspace_materializations.requested_cpu_millis <= sqlc.arg(available_cpu_millis)
       AND workspace_materializations.requested_memory_mib <= sqlc.arg(available_memory_mib)
       AND workspace_materializations.requested_disk_mib <= sqlc.arg(available_disk_mib)
       AND workspace_materializations.requested_execution_slots <= sqlc.arg(available_execution_slots)
       AND workspace_materializations.rootfs_digest = sqlc.arg(rootfs_digest)
       AND workspace_materializations.runtime_abi = sqlc.arg(runtime_abi)
       AND workspace_materializations.guestd_abi = sqlc.arg(guestd_abi)
       AND workspace_materializations.adapter_abi = sqlc.arg(adapter_abi)
     ORDER BY workspace_materializations.priority DESC,
              workspace_materializations.requested_at ASC,
              workspace_materializations.claim_attempt ASC
     LIMIT 1
     FOR UPDATE SKIP LOCKED
),
claimed AS (
    UPDATE workspace_materializations
       SET state = 'materializing',
           worker_instance_id = sqlc.arg(worker_instance_id),
           reservation_token = sqlc.arg(reservation_token),
           reservation_expires_at = sqlc.arg(reservation_expires_at),
           guestd_channel_token_hash = sqlc.arg(guestd_channel_token_hash),
           guestd_channel_token_expires_at = sqlc.arg(reservation_expires_at),
           claim_attempt = workspace_materializations.claim_attempt + 1,
           reserved_cpu_millis = workspace_materializations.requested_cpu_millis,
           reserved_memory_mib = workspace_materializations.requested_memory_mib,
           reserved_disk_mib = workspace_materializations.requested_disk_mib,
           reserved_execution_slots = workspace_materializations.requested_execution_slots,
           runtime_id = sqlc.arg(runtime_id),
           last_heartbeat_at = now(),
           updated_at = now()
      FROM candidate
     WHERE workspace_materializations.org_id = candidate.org_id
       AND workspace_materializations.id = candidate.id
    RETURNING workspace_materializations.*
)
SELECT claimed.*,
       image_artifact.digest AS image_artifact_digest,
       image_artifact.size_bytes AS image_artifact_size_bytes,
       image_artifact.media_type AS image_artifact_media_type
  FROM claimed
  JOIN artifacts AS image_artifact
    ON image_artifact.org_id = claimed.org_id
   AND image_artifact.project_id = claimed.project_id
   AND image_artifact.environment_id = claimed.environment_id
   AND image_artifact.id = claimed.image_artifact_id
   AND image_artifact.kind = 'sandbox_image'
   AND image_artifact.media_type = 'application/vnd.helmr.sandbox-image.v0.oci-tar';

-- name: RenewWorkspaceMaterialization :one
UPDATE workspace_materializations
   SET reservation_expires_at = sqlc.arg(reservation_expires_at),
       guestd_channel_token_expires_at = sqlc.arg(reservation_expires_at),
       last_heartbeat_at = now(),
       updated_at = now()
 WHERE org_id = sqlc.arg(org_id)
   AND id = sqlc.arg(id)
   AND worker_instance_id = sqlc.arg(worker_instance_id)
   AND reservation_token = sqlc.arg(reservation_token)
   AND reservation_expires_at > now()
   AND state IN ('materializing', 'restoring', 'running', 'pausing', 'paused', 'capturing', 'stopping')
RETURNING *;

-- name: MarkWorkspaceMaterializationRunning :one
UPDATE workspace_materializations
   SET state = 'running',
       materialized_at = coalesce(materialized_at, now()),
       reservation_expires_at = sqlc.arg(reservation_expires_at),
       guestd_channel_token_expires_at = sqlc.arg(reservation_expires_at),
       last_heartbeat_at = now(),
       updated_at = now()
 WHERE org_id = sqlc.arg(org_id)
   AND id = sqlc.arg(id)
   AND worker_instance_id = sqlc.arg(worker_instance_id)
   AND reservation_token = sqlc.arg(reservation_token)
   AND reservation_expires_at > now()
   AND state IN ('materializing', 'restoring', 'running')
RETURNING *;

-- name: StopWorkspaceMaterialization :one
WITH stopped AS (
    UPDATE workspace_materializations
       SET state = 'stopped',
           stopped_at = now(),
           reservation_expires_at = NULL,
           reserved_cpu_millis = 0,
           reserved_memory_mib = 0,
           reserved_disk_mib = 0,
           reserved_execution_slots = 0,
           capacity_reservation_id = NULL,
           updated_at = now()
     WHERE workspace_materializations.org_id = sqlc.arg(org_id)
       AND workspace_materializations.id = sqlc.arg(id)
       AND workspace_materializations.worker_instance_id = sqlc.arg(worker_instance_id)
       AND workspace_materializations.reservation_token = sqlc.arg(reservation_token)
       AND workspace_materializations.state IN ('materializing', 'restoring', 'running', 'pausing', 'paused')
    RETURNING workspace_materializations.*
),
lost_operations AS (
    UPDATE workspace_materialization_operations
       SET state = 'lost',
           error = jsonb_build_object('code', 'workspace_materialization_stopped'),
           completed_at = now(),
           updated_at = now()
      FROM stopped
     WHERE workspace_materialization_operations.org_id = stopped.org_id
       AND workspace_materialization_operations.materialization_id = stopped.id
       AND workspace_materialization_operations.state IN ('queued', 'claimed', 'running')
    RETURNING workspace_materialization_operations.id
)
SELECT * FROM stopped;

-- name: FailWorkspaceMaterialization :one
WITH target AS MATERIALIZED (
    SELECT workspace_materializations.*
      FROM workspace_materializations
      JOIN workspaces
        ON workspaces.org_id = workspace_materializations.org_id
       AND workspaces.project_id = workspace_materializations.project_id
       AND workspaces.environment_id = workspace_materializations.environment_id
       AND workspaces.id = workspace_materializations.workspace_id
     WHERE workspace_materializations.org_id = sqlc.arg(org_id)
       AND workspace_materializations.id = sqlc.arg(id)
       AND workspace_materializations.worker_instance_id = sqlc.arg(worker_instance_id)
       AND workspace_materializations.reservation_token = sqlc.arg(reservation_token)
       AND workspace_materializations.state IN ('materializing', 'restoring', 'running', 'pausing', 'paused', 'capturing', 'stopping')
     FOR UPDATE OF workspaces, workspace_materializations
),
failed AS (
    UPDATE workspace_materializations
       SET state = 'failed',
           failed_at = now(),
           reservation_expires_at = NULL,
           reserved_cpu_millis = 0,
           reserved_memory_mib = 0,
           reserved_disk_mib = 0,
           reserved_execution_slots = 0,
           capacity_reservation_id = NULL,
           error = coalesce(sqlc.arg(error)::jsonb, '{}'::jsonb),
           updated_at = now()
      FROM target
     WHERE workspace_materializations.org_id = target.org_id
       AND workspace_materializations.id = target.id
    RETURNING workspace_materializations.*
),
lost_operations AS (
    UPDATE workspace_materialization_operations
       SET state = 'lost',
           error = jsonb_build_object('code', 'workspace_materialization_failed'),
           completed_at = now(),
           updated_at = now()
      FROM failed
     WHERE workspace_materialization_operations.org_id = failed.org_id
       AND workspace_materialization_operations.materialization_id = failed.id
       AND workspace_materialization_operations.state IN ('queued', 'claimed', 'running')
    RETURNING workspace_materialization_operations.id
)
SELECT * FROM failed;

-- name: MarkStaleWorkspaceMaterializationsLost :many
WITH lost AS (
    UPDATE workspace_materializations
       SET state = 'lost',
           lost_at = now(),
           reservation_expires_at = NULL,
           reserved_cpu_millis = 0,
           reserved_memory_mib = 0,
           reserved_disk_mib = 0,
           reserved_execution_slots = 0,
           capacity_reservation_id = NULL,
           updated_at = now()
     WHERE workspace_materializations.state IN ('materializing', 'restoring', 'running', 'pausing', 'paused', 'capturing', 'stopping')
       AND workspace_materializations.last_heartbeat_at < sqlc.arg(stale_before)
       AND NOT EXISTS (
           SELECT 1
             FROM runs
             JOIN run_leases ON run_leases.org_id = runs.org_id
                            AND run_leases.run_id = runs.id
            WHERE runs.org_id = workspace_materializations.org_id
              AND runs.workspace_materialization_id = workspace_materializations.id
              AND run_leases.status IN ('leased', 'running')
       )
    RETURNING *
),
lost_operations AS (
    UPDATE workspace_materialization_operations
       SET state = 'lost',
           error = jsonb_build_object('code', 'workspace_materialization_lost'),
           completed_at = now(),
           updated_at = now()
      FROM lost
     WHERE workspace_materialization_operations.org_id = lost.org_id
       AND workspace_materialization_operations.materialization_id = lost.id
       AND workspace_materialization_operations.state IN ('queued', 'claimed', 'running')
    RETURNING workspace_materialization_operations.id
)
SELECT * FROM lost;
