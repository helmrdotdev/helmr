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
),
reactivated_existing_workspace AS (
    UPDATE workspaces
       SET desired_state = 'active',
           updated_at = now()
      FROM existing_runnable
     WHERE workspaces.org_id = existing_runnable.org_id
       AND workspaces.project_id = existing_runnable.project_id
       AND workspaces.environment_id = existing_runnable.environment_id
       AND workspaces.id = existing_runnable.workspace_id
       AND workspaces.desired_state <> 'active'
    RETURNING workspaces.id
),
reactivated_inserted_workspace AS (
    UPDATE workspaces
       SET desired_state = 'active',
           updated_at = now()
      FROM inserted
     WHERE workspaces.org_id = inserted.org_id
       AND workspaces.project_id = inserted.project_id
       AND workspaces.environment_id = inserted.environment_id
       AND workspaces.id = inserted.workspace_id
       AND workspaces.desired_state <> 'active'
    RETURNING workspaces.id
)
SELECT * FROM priority_bumped_existing
UNION ALL
SELECT * FROM unchanged_existing
UNION ALL
SELECT * FROM inserted
LIMIT 1;

-- name: GetWorkspaceMaterialization :one
SELECT *
  FROM workspace_materializations
 WHERE org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND workspace_id = sqlc.arg(workspace_id)
   AND id = sqlc.arg(id);

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
   SET state = CASE
           WHEN workspace_materializations.state IN ('capturing', 'stopping') THEN workspace_materializations.state
           ELSE 'running'::workspace_materialization_state
       END,
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
   AND state IN ('materializing', 'restoring', 'running', 'capturing', 'stopping')
RETURNING *;

-- name: RequestWorkspaceMaterializationStop :one
WITH locked_workspace AS MATERIALIZED (
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
target AS MATERIALIZED (
    SELECT workspace_materializations.*
      FROM locked_workspace
      JOIN workspace_materializations
        ON workspace_materializations.org_id = locked_workspace.org_id
       AND workspace_materializations.project_id = locked_workspace.project_id
       AND workspace_materializations.environment_id = locked_workspace.environment_id
       AND workspace_materializations.workspace_id = locked_workspace.id
     WHERE workspace_materializations.state IN ('requested', 'materializing', 'restoring', 'running', 'pausing', 'paused', 'capturing', 'stopping')
     ORDER BY workspace_materializations.requested_at DESC
     LIMIT 1
     FOR UPDATE OF workspace_materializations
),
requested_without_worker AS (
    UPDATE workspace_materializations
       SET state = 'stopped',
           stopped_at = coalesce(workspace_materializations.stopped_at, now()),
           reservation_expires_at = NULL,
           reserved_cpu_millis = 0,
           reserved_memory_mib = 0,
           reserved_disk_mib = 0,
           reserved_execution_slots = 0,
           capacity_reservation_id = NULL,
           updated_at = now()
      FROM target
     WHERE workspace_materializations.org_id = target.org_id
       AND workspace_materializations.id = target.id
       AND target.state = 'requested'
    RETURNING workspace_materializations.*
),
requested_live_stop AS (
    UPDATE workspace_materializations
       SET state = CASE
               WHEN target.state IN ('capturing', 'stopping') THEN target.state
               WHEN target.dirty_generation > 0 THEN 'capturing'::workspace_materialization_state
               ELSE 'stopping'::workspace_materialization_state
           END,
           updated_at = now()
      FROM target
     WHERE workspace_materializations.org_id = target.org_id
       AND workspace_materializations.id = target.id
       AND target.state <> 'requested'
    RETURNING workspace_materializations.*
),
updated_workspace AS (
    UPDATE workspaces
       SET desired_state = 'stopped',
           dirty_state = CASE
               WHEN coalesce((SELECT dirty_generation FROM requested_live_stop LIMIT 1), 0) > 0
                    AND workspaces.dirty_state = 'dirty' THEN 'capturing'::workspace_dirty_state
               ELSE workspaces.dirty_state
           END,
           updated_at = now()
      FROM locked_workspace
     WHERE workspaces.org_id = locked_workspace.org_id
       AND workspaces.project_id = locked_workspace.project_id
       AND workspaces.environment_id = locked_workspace.environment_id
       AND workspaces.id = locked_workspace.id
    RETURNING workspaces.id
),
cancelled_requested_operations AS (
    UPDATE workspace_materialization_operations
       SET state = 'cancelled',
           error = jsonb_build_object('code', 'workspace_materialization_stopped'),
           completed_at = now(),
           updated_at = now()
      FROM requested_without_worker
     WHERE workspace_materialization_operations.org_id = requested_without_worker.org_id
       AND workspace_materialization_operations.materialization_id = requested_without_worker.id
       AND workspace_materialization_operations.state IN ('queued', 'claimed', 'running')
    RETURNING workspace_materialization_operations.id
),
terminated_requested_execs AS (
    UPDATE workspace_execs
       SET state = 'terminated',
           error = jsonb_build_object('code', 'workspace_materialization_stopped'),
           exited_at = coalesce(workspace_execs.exited_at, now()),
           updated_at = now()
      FROM requested_without_worker
     WHERE workspace_execs.org_id = requested_without_worker.org_id
       AND workspace_execs.project_id = requested_without_worker.project_id
       AND workspace_execs.environment_id = requested_without_worker.environment_id
       AND workspace_execs.workspace_id = requested_without_worker.workspace_id
       AND workspace_execs.materialization_id = requested_without_worker.id
       AND workspace_execs.state IN ('queued', 'materializing', 'running')
    RETURNING workspace_execs.*
),
closed_requested_ptys AS (
    UPDATE workspace_pty_sessions
       SET state = 'closed',
           error = jsonb_build_object('code', 'workspace_materialization_stopped'),
           closed_at = coalesce(workspace_pty_sessions.closed_at, now()),
           updated_at = now()
      FROM requested_without_worker
     WHERE workspace_pty_sessions.org_id = requested_without_worker.org_id
       AND workspace_pty_sessions.project_id = requested_without_worker.project_id
       AND workspace_pty_sessions.environment_id = requested_without_worker.environment_id
       AND workspace_pty_sessions.workspace_id = requested_without_worker.workspace_id
       AND workspace_pty_sessions.materialization_id = requested_without_worker.id
       AND workspace_pty_sessions.state IN ('creating', 'open', 'resizing', 'closing')
    RETURNING workspace_pty_sessions.*
),
closed_requested_ports AS (
    UPDATE workspace_ports
       SET state = 'closed',
           error = jsonb_build_object('code', 'workspace_materialization_stopped'),
           closed_at = coalesce(workspace_ports.closed_at, now()),
           updated_at = now()
      FROM requested_without_worker
     WHERE workspace_ports.org_id = requested_without_worker.org_id
       AND workspace_ports.project_id = requested_without_worker.project_id
       AND workspace_ports.environment_id = requested_without_worker.environment_id
       AND workspace_ports.workspace_id = requested_without_worker.workspace_id
       AND workspace_ports.materialization_id = requested_without_worker.id
       AND workspace_ports.state IN ('exposing', 'open', 'closing')
    RETURNING workspace_ports.id
),
released_requested_leases AS (
    UPDATE workspace_leases
       SET state = 'released',
           released_at = coalesce(workspace_leases.released_at, now()),
           updated_at = now()
      FROM requested_without_worker
     WHERE workspace_leases.org_id = requested_without_worker.org_id
       AND workspace_leases.project_id = requested_without_worker.project_id
       AND workspace_leases.environment_id = requested_without_worker.environment_id
       AND workspace_leases.workspace_id = requested_without_worker.workspace_id
       AND workspace_leases.materialization_id = requested_without_worker.id
       AND workspace_leases.state IN ('active', 'releasing')
    RETURNING workspace_leases.id
),
requested_stream_wakeups AS (
    INSERT INTO workspace_stream_wakeups (org_id, project_id, environment_id, workspace_id, resource_kind, resource_id, stream, cursor_offset, notification_kind)
    SELECT terminated_requested_execs.org_id,
           terminated_requested_execs.project_id,
           terminated_requested_execs.environment_id,
           terminated_requested_execs.workspace_id,
           'workspace_exec',
           terminated_requested_execs.id,
           stream_names.stream,
           stream_names.cursor_offset,
           'terminal'
      FROM terminated_requested_execs
      CROSS JOIN LATERAL (VALUES ('stdout', terminated_requested_execs.stdout_cursor), ('stderr', terminated_requested_execs.stderr_cursor)) AS stream_names(stream, cursor_offset)
    UNION ALL
    SELECT closed_requested_ptys.org_id,
           closed_requested_ptys.project_id,
           closed_requested_ptys.environment_id,
           closed_requested_ptys.workspace_id,
           'workspace_pty',
           closed_requested_ptys.id,
           'output',
           closed_requested_ptys.output_cursor,
           'terminal'
      FROM closed_requested_ptys
    RETURNING id
),
cancelled_live_pending_operations AS (
    UPDATE workspace_materialization_operations
       SET state = 'cancelled',
           error = jsonb_build_object('code', 'workspace_materialization_stopped'),
           completed_at = now(),
           updated_at = now()
      FROM requested_live_stop
     WHERE workspace_materialization_operations.org_id = requested_live_stop.org_id
       AND workspace_materialization_operations.materialization_id = requested_live_stop.id
       AND workspace_materialization_operations.state IN ('queued', 'claimed', 'running')
    RETURNING workspace_materialization_operations.id
),
terminated_live_pending_execs AS (
    UPDATE workspace_execs
       SET state = 'terminated',
           error = jsonb_build_object('code', 'workspace_materialization_stopped'),
           exited_at = coalesce(workspace_execs.exited_at, now()),
           updated_at = now()
      FROM requested_live_stop
     WHERE workspace_execs.org_id = requested_live_stop.org_id
       AND workspace_execs.project_id = requested_live_stop.project_id
       AND workspace_execs.environment_id = requested_live_stop.environment_id
       AND workspace_execs.workspace_id = requested_live_stop.workspace_id
       AND workspace_execs.materialization_id = requested_live_stop.id
       AND workspace_execs.state IN ('queued', 'materializing')
    RETURNING workspace_execs.*
),
closed_live_pending_ptys AS (
    UPDATE workspace_pty_sessions
       SET state = 'closed',
           error = jsonb_build_object('code', 'workspace_materialization_stopped'),
           closed_at = coalesce(workspace_pty_sessions.closed_at, now()),
           updated_at = now()
      FROM requested_live_stop
     WHERE workspace_pty_sessions.org_id = requested_live_stop.org_id
       AND workspace_pty_sessions.project_id = requested_live_stop.project_id
       AND workspace_pty_sessions.environment_id = requested_live_stop.environment_id
       AND workspace_pty_sessions.workspace_id = requested_live_stop.workspace_id
       AND workspace_pty_sessions.materialization_id = requested_live_stop.id
       AND workspace_pty_sessions.state = 'creating'
    RETURNING workspace_pty_sessions.*
),
live_pending_stream_wakeups AS (
    INSERT INTO workspace_stream_wakeups (org_id, project_id, environment_id, workspace_id, resource_kind, resource_id, stream, cursor_offset, notification_kind)
    SELECT terminated_live_pending_execs.org_id,
           terminated_live_pending_execs.project_id,
           terminated_live_pending_execs.environment_id,
           terminated_live_pending_execs.workspace_id,
           'workspace_exec',
           terminated_live_pending_execs.id,
           stream_names.stream,
           stream_names.cursor_offset,
           'terminal'
      FROM terminated_live_pending_execs
      CROSS JOIN LATERAL (VALUES ('stdout', terminated_live_pending_execs.stdout_cursor), ('stderr', terminated_live_pending_execs.stderr_cursor)) AS stream_names(stream, cursor_offset)
    UNION ALL
    SELECT closed_live_pending_ptys.org_id,
           closed_live_pending_ptys.project_id,
           closed_live_pending_ptys.environment_id,
           closed_live_pending_ptys.workspace_id,
           'workspace_pty',
           closed_live_pending_ptys.id,
           'output',
           closed_live_pending_ptys.output_cursor,
           'terminal'
      FROM closed_live_pending_ptys
    RETURNING id
),
requested_cleanup_counts AS (
    SELECT (SELECT count(*) FROM cancelled_requested_operations)
         + (SELECT count(*) FROM closed_requested_ports)
         + (SELECT count(*) FROM released_requested_leases)
         + (SELECT count(*) FROM requested_stream_wakeups)
         + (SELECT count(*) FROM cancelled_live_pending_operations)
         + (SELECT count(*) FROM live_pending_stream_wakeups) AS count
)
SELECT *
  FROM requested_without_worker
 WHERE (SELECT count FROM requested_cleanup_counts) >= 0
UNION ALL
SELECT * FROM requested_live_stop
LIMIT 1;

-- name: PromoteWorkspaceMaterializationStopCapture :one
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
       AND workspace_materializations.workspace_id = sqlc.arg(workspace_id)
       AND workspace_materializations.worker_instance_id = sqlc.arg(worker_instance_id)
       AND workspace_materializations.reservation_token = sqlc.arg(reservation_token)
       AND workspace_materializations.reservation_expires_at > now()
       AND workspace_materializations.state = 'capturing'
       AND workspaces.current_version_id IS NOT DISTINCT FROM workspace_materializations.base_version_id
     FOR UPDATE OF workspaces, workspace_materializations
),
verified_artifact AS (
    SELECT artifacts.id
      FROM artifacts
      JOIN cas_objects
        ON cas_objects.digest = artifacts.digest
     WHERE artifacts.org_id = sqlc.arg(org_id)
       AND artifacts.project_id = sqlc.arg(project_id)
       AND artifacts.environment_id = sqlc.arg(environment_id)
       AND artifacts.id = sqlc.arg(artifact_id)
       AND artifacts.kind = 'workspace_version'
       AND artifacts.size_bytes = sqlc.arg(size_bytes)
       AND artifacts.media_type = 'application/vnd.helmr.workspace.v0.tar'
       AND cas_objects.size_bytes = artifacts.size_bytes
       AND cas_objects.media_type = artifacts.media_type
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
        kind,
        state,
        artifact_id,
        artifact_encoding,
        artifact_entry_count,
        content_digest,
        size_bytes,
        message,
        promoted_at,
        created_by_subject_type
    )
    SELECT sqlc.arg(version_id),
           target.org_id,
           target.project_id,
           target.environment_id,
           target.workspace_id,
           target.base_version_id,
           target.id,
           'system',
           'ready',
           sqlc.arg(artifact_id),
           sqlc.arg(artifact_encoding),
           sqlc.arg(artifact_entry_count),
           sqlc.arg(content_digest),
           sqlc.arg(size_bytes),
           sqlc.arg(message),
           now(),
           'worker'
      FROM target
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
promoted_materialization AS (
    UPDATE workspace_materializations
       SET state = 'stopping',
           dirty_generation = 0,
           updated_at = now()
      FROM created_version
     WHERE workspace_materializations.org_id = created_version.org_id
       AND workspace_materializations.id = created_version.source_materialization_id
       AND workspace_materializations.state = 'capturing'
    RETURNING workspace_materializations.id
)
SELECT created_version.*
  FROM created_version
  JOIN promoted_workspace ON promoted_workspace.id = created_version.workspace_id
  JOIN promoted_materialization ON promoted_materialization.id = created_version.source_materialization_id;

-- name: StopWorkspaceMaterialization :one
WITH stopped AS (
    UPDATE workspace_materializations
       SET state = 'stopped',
           stopped_at = coalesce(workspace_materializations.stopped_at, now()),
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
       AND workspace_materializations.state IN ('materializing', 'restoring', 'running', 'pausing', 'paused', 'stopping')
    RETURNING workspace_materializations.*
),
cancelled_operations AS (
    UPDATE workspace_materialization_operations
       SET state = 'cancelled',
           error = jsonb_build_object('code', 'workspace_materialization_stopped'),
           completed_at = now(),
           updated_at = now()
      FROM stopped
     WHERE workspace_materialization_operations.org_id = stopped.org_id
       AND workspace_materialization_operations.materialization_id = stopped.id
       AND workspace_materialization_operations.state IN ('queued', 'claimed', 'running')
    RETURNING workspace_materialization_operations.id
),
terminated_execs AS (
    UPDATE workspace_execs
       SET state = 'terminated',
           error = jsonb_build_object('code', 'workspace_materialization_stopped'),
           exited_at = coalesce(workspace_execs.exited_at, now()),
           updated_at = now()
      FROM stopped
     WHERE workspace_execs.org_id = stopped.org_id
       AND workspace_execs.project_id = stopped.project_id
       AND workspace_execs.environment_id = stopped.environment_id
       AND workspace_execs.workspace_id = stopped.workspace_id
       AND workspace_execs.materialization_id = stopped.id
       AND workspace_execs.state IN ('queued', 'materializing', 'running')
    RETURNING workspace_execs.*
),
closed_ptys AS (
    UPDATE workspace_pty_sessions
       SET state = 'closed',
           error = jsonb_build_object('code', 'workspace_materialization_stopped'),
           closed_at = coalesce(workspace_pty_sessions.closed_at, now()),
           updated_at = now()
      FROM stopped
     WHERE workspace_pty_sessions.org_id = stopped.org_id
       AND workspace_pty_sessions.project_id = stopped.project_id
       AND workspace_pty_sessions.environment_id = stopped.environment_id
       AND workspace_pty_sessions.workspace_id = stopped.workspace_id
       AND workspace_pty_sessions.materialization_id = stopped.id
       AND workspace_pty_sessions.state IN ('creating', 'open', 'resizing', 'closing')
    RETURNING workspace_pty_sessions.*
),
closed_ports AS (
    UPDATE workspace_ports
       SET state = 'closed',
           error = jsonb_build_object('code', 'workspace_materialization_stopped'),
           closed_at = coalesce(workspace_ports.closed_at, now()),
           updated_at = now()
      FROM stopped
     WHERE workspace_ports.org_id = stopped.org_id
       AND workspace_ports.project_id = stopped.project_id
       AND workspace_ports.environment_id = stopped.environment_id
       AND workspace_ports.workspace_id = stopped.workspace_id
       AND workspace_ports.materialization_id = stopped.id
       AND workspace_ports.state IN ('exposing', 'open', 'closing')
    RETURNING workspace_ports.id
),
released_leases AS (
    UPDATE workspace_leases
       SET state = 'released',
           released_at = coalesce(workspace_leases.released_at, now()),
           updated_at = now()
      FROM stopped
     WHERE workspace_leases.org_id = stopped.org_id
       AND workspace_leases.project_id = stopped.project_id
       AND workspace_leases.environment_id = stopped.environment_id
       AND workspace_leases.workspace_id = stopped.workspace_id
       AND workspace_leases.materialization_id = stopped.id
       AND workspace_leases.state IN ('active', 'releasing')
    RETURNING workspace_leases.id
),
updated_workspace AS (
    UPDATE workspaces
       SET desired_state = 'stopped',
           updated_at = now()
      FROM stopped
     WHERE workspaces.org_id = stopped.org_id
       AND workspaces.project_id = stopped.project_id
       AND workspaces.environment_id = stopped.environment_id
       AND workspaces.id = stopped.workspace_id
    RETURNING workspaces.id
),
stream_wakeups AS (
    INSERT INTO workspace_stream_wakeups (org_id, project_id, environment_id, workspace_id, resource_kind, resource_id, stream, cursor_offset, notification_kind)
    SELECT terminated_execs.org_id,
           terminated_execs.project_id,
           terminated_execs.environment_id,
           terminated_execs.workspace_id,
           'workspace_exec',
           terminated_execs.id,
           stream_names.stream,
           stream_names.cursor_offset,
           'terminal'
      FROM terminated_execs
      CROSS JOIN LATERAL (VALUES ('stdout', terminated_execs.stdout_cursor), ('stderr', terminated_execs.stderr_cursor)) AS stream_names(stream, cursor_offset)
    UNION ALL
    SELECT closed_ptys.org_id,
           closed_ptys.project_id,
           closed_ptys.environment_id,
           closed_ptys.workspace_id,
           'workspace_pty',
           closed_ptys.id,
           'output',
           closed_ptys.output_cursor,
           'terminal'
      FROM closed_ptys
    RETURNING id
)
SELECT *
  FROM stopped
 WHERE (SELECT count(*) FROM stream_wakeups) >= 0;

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
updated_workspace AS (
    UPDATE workspaces
       SET state = CASE
               WHEN target.dirty_generation > 0 AND target.state IN ('capturing', 'stopping') THEN 'recovery_required'::workspace_state
               ELSE workspaces.state
           END,
           dirty_state = CASE
               WHEN target.dirty_generation > 0 AND target.state IN ('capturing', 'stopping') THEN 'capture_failed'::workspace_dirty_state
               ELSE workspaces.dirty_state
           END,
           updated_at = now()
      FROM target
     WHERE workspaces.org_id = target.org_id
       AND workspaces.project_id = target.project_id
       AND workspaces.environment_id = target.environment_id
       AND workspaces.id = target.workspace_id
    RETURNING workspaces.id
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
),
lost_execs AS (
    UPDATE workspace_execs
       SET state = 'lost',
           error = jsonb_build_object('code', 'workspace_materialization_failed'),
           exited_at = coalesce(workspace_execs.exited_at, now()),
           updated_at = now()
      FROM failed
     WHERE workspace_execs.org_id = failed.org_id
       AND workspace_execs.project_id = failed.project_id
       AND workspace_execs.environment_id = failed.environment_id
       AND workspace_execs.workspace_id = failed.workspace_id
       AND workspace_execs.materialization_id = failed.id
       AND workspace_execs.state IN ('queued', 'materializing', 'running')
    RETURNING workspace_execs.*
),
lost_ptys AS (
    UPDATE workspace_pty_sessions
       SET state = 'lost',
           error = jsonb_build_object('code', 'workspace_materialization_failed'),
           closed_at = coalesce(workspace_pty_sessions.closed_at, now()),
           updated_at = now()
      FROM failed
     WHERE workspace_pty_sessions.org_id = failed.org_id
       AND workspace_pty_sessions.project_id = failed.project_id
       AND workspace_pty_sessions.environment_id = failed.environment_id
       AND workspace_pty_sessions.workspace_id = failed.workspace_id
       AND workspace_pty_sessions.materialization_id = failed.id
       AND workspace_pty_sessions.state IN ('creating', 'open', 'resizing', 'closing')
    RETURNING workspace_pty_sessions.*
),
closed_ports AS (
    UPDATE workspace_ports
       SET state = 'closed',
           error = jsonb_build_object('code', 'workspace_materialization_failed'),
           closed_at = coalesce(workspace_ports.closed_at, now()),
           updated_at = now()
      FROM failed
     WHERE workspace_ports.org_id = failed.org_id
       AND workspace_ports.project_id = failed.project_id
       AND workspace_ports.environment_id = failed.environment_id
       AND workspace_ports.workspace_id = failed.workspace_id
       AND workspace_ports.materialization_id = failed.id
       AND workspace_ports.state IN ('exposing', 'open', 'closing')
    RETURNING workspace_ports.id
),
released_leases AS (
    UPDATE workspace_leases
       SET state = 'released',
           released_at = coalesce(workspace_leases.released_at, now()),
           updated_at = now()
      FROM failed
     WHERE workspace_leases.org_id = failed.org_id
       AND workspace_leases.project_id = failed.project_id
       AND workspace_leases.environment_id = failed.environment_id
       AND workspace_leases.workspace_id = failed.workspace_id
       AND workspace_leases.materialization_id = failed.id
       AND workspace_leases.state IN ('active', 'releasing')
    RETURNING workspace_leases.id
),
stream_wakeups AS (
    INSERT INTO workspace_stream_wakeups (org_id, project_id, environment_id, workspace_id, resource_kind, resource_id, stream, cursor_offset, notification_kind)
    SELECT lost_execs.org_id,
           lost_execs.project_id,
           lost_execs.environment_id,
           lost_execs.workspace_id,
           'workspace_exec',
           lost_execs.id,
           stream_names.stream,
           stream_names.cursor_offset,
           'terminal'
      FROM lost_execs
      CROSS JOIN LATERAL (VALUES ('stdout', lost_execs.stdout_cursor), ('stderr', lost_execs.stderr_cursor)) AS stream_names(stream, cursor_offset)
    UNION ALL
    SELECT lost_ptys.org_id,
           lost_ptys.project_id,
           lost_ptys.environment_id,
           lost_ptys.workspace_id,
           'workspace_pty',
           lost_ptys.id,
           'output',
           lost_ptys.output_cursor,
           'terminal'
      FROM lost_ptys
    RETURNING id
)
SELECT *
  FROM failed
 WHERE (SELECT count(*) FROM stream_wakeups) >= 0;

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
),
lost_execs AS (
    UPDATE workspace_execs
       SET state = 'lost',
           error = jsonb_build_object('code', 'workspace_materialization_lost'),
           exited_at = coalesce(workspace_execs.exited_at, now()),
           updated_at = now()
      FROM lost
     WHERE workspace_execs.org_id = lost.org_id
       AND workspace_execs.project_id = lost.project_id
       AND workspace_execs.environment_id = lost.environment_id
       AND workspace_execs.workspace_id = lost.workspace_id
       AND workspace_execs.materialization_id = lost.id
       AND workspace_execs.state IN ('queued', 'materializing', 'running')
    RETURNING workspace_execs.*
),
lost_ptys AS (
    UPDATE workspace_pty_sessions
       SET state = 'lost',
           error = jsonb_build_object('code', 'workspace_materialization_lost'),
           closed_at = coalesce(workspace_pty_sessions.closed_at, now()),
           updated_at = now()
      FROM lost
     WHERE workspace_pty_sessions.org_id = lost.org_id
       AND workspace_pty_sessions.project_id = lost.project_id
       AND workspace_pty_sessions.environment_id = lost.environment_id
       AND workspace_pty_sessions.workspace_id = lost.workspace_id
       AND workspace_pty_sessions.materialization_id = lost.id
       AND workspace_pty_sessions.state IN ('creating', 'open', 'resizing', 'closing')
    RETURNING workspace_pty_sessions.*
),
closed_ports AS (
    UPDATE workspace_ports
       SET state = 'closed',
           error = jsonb_build_object('code', 'workspace_materialization_lost'),
           closed_at = coalesce(workspace_ports.closed_at, now()),
           updated_at = now()
      FROM lost
     WHERE workspace_ports.org_id = lost.org_id
       AND workspace_ports.project_id = lost.project_id
       AND workspace_ports.environment_id = lost.environment_id
       AND workspace_ports.workspace_id = lost.workspace_id
       AND workspace_ports.materialization_id = lost.id
       AND workspace_ports.state IN ('exposing', 'open', 'closing')
    RETURNING workspace_ports.id
),
released_leases AS (
    UPDATE workspace_leases
       SET state = 'released',
           released_at = coalesce(workspace_leases.released_at, now()),
           updated_at = now()
      FROM lost
     WHERE workspace_leases.org_id = lost.org_id
       AND workspace_leases.project_id = lost.project_id
       AND workspace_leases.environment_id = lost.environment_id
       AND workspace_leases.workspace_id = lost.workspace_id
       AND workspace_leases.materialization_id = lost.id
       AND workspace_leases.state IN ('active', 'releasing')
    RETURNING workspace_leases.id
),
stream_wakeups AS (
    INSERT INTO workspace_stream_wakeups (org_id, project_id, environment_id, workspace_id, resource_kind, resource_id, stream, cursor_offset, notification_kind)
    SELECT lost_execs.org_id,
           lost_execs.project_id,
           lost_execs.environment_id,
           lost_execs.workspace_id,
           'workspace_exec',
           lost_execs.id,
           stream_names.stream,
           stream_names.cursor_offset,
           'terminal'
      FROM lost_execs
      CROSS JOIN LATERAL (VALUES ('stdout', lost_execs.stdout_cursor), ('stderr', lost_execs.stderr_cursor)) AS stream_names(stream, cursor_offset)
    UNION ALL
    SELECT lost_ptys.org_id,
           lost_ptys.project_id,
           lost_ptys.environment_id,
           lost_ptys.workspace_id,
           'workspace_pty',
           lost_ptys.id,
           'output',
           lost_ptys.output_cursor,
           'terminal'
      FROM lost_ptys
    RETURNING id
)
SELECT *
  FROM lost
 WHERE (SELECT count(*) FROM stream_wakeups) >= 0;
