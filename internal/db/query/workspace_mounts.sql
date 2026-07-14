-- name: EnsureWorkspaceMountRequested :one
INSERT INTO workspace_mounts (
    id, org_id, project_id, environment_id, region_id, worker_group_id,
    worker_instance_id, worker_epoch, workspace_id, deployment_sandbox_id,
    sandbox_fingerprint, base_version_id, runtime_instance_id, priority,
    guestd_channel_token_hash, guestd_channel_token_expires_at, request,
    network_namespace, port_namespace, image_artifact_id, image_artifact_format,
    rootfs_digest, image_digest, image_format, workspace_artifact_id,
    workspace_artifact_encoding, workspace_artifact_entry_count,
    workspace_artifact_digest, workspace_artifact_size_bytes,
    workspace_artifact_media_type, workspace_mount_path, runtime_abi,
    guestd_abi, adapter_abi
)
SELECT sqlc.arg(id), runtime_instances.org_id, runtime_instances.project_id,
       runtime_instances.environment_id, runtime_instances.region_id,
       runtime_instances.worker_group_id, runtime_instances.worker_instance_id,
       runtime_instances.worker_epoch, workspaces.id, workspaces.deployment_sandbox_id,
       workspaces.sandbox_fingerprint, workspaces.current_version_id, runtime_instances.id,
       sqlc.arg(priority), '', NULL, sqlc.arg(request), '', '',
       deployment_sandboxes.image_artifact_id, deployment_sandboxes.image_artifact_format,
       deployment_sandboxes.rootfs_digest, deployment_sandboxes.image_digest,
       deployment_sandboxes.image_format, workspace_versions.artifact_id,
       workspace_versions.artifact_encoding, workspace_versions.artifact_entry_count,
       workspace_versions.content_digest, workspace_versions.size_bytes,
       workspace_artifacts.media_type, deployment_sandboxes.workspace_mount_path,
       runtime_instances.runtime_abi, runtime_instances.guestd_abi, runtime_instances.adapter_abi
  FROM workspaces
  JOIN deployment_sandboxes ON deployment_sandboxes.org_id = workspaces.org_id
                           AND deployment_sandboxes.project_id = workspaces.project_id
                           AND deployment_sandboxes.environment_id = workspaces.environment_id
                           AND deployment_sandboxes.id = workspaces.deployment_sandbox_id
  JOIN workspace_versions ON workspace_versions.org_id = workspaces.org_id
                         AND workspace_versions.workspace_id = workspaces.id
                         AND workspace_versions.id = workspaces.current_version_id
                         AND workspace_versions.state = 'ready'
  JOIN artifacts AS workspace_artifacts ON workspace_artifacts.org_id = workspace_versions.org_id
                                       AND workspace_artifacts.id = workspace_versions.artifact_id
  JOIN runtime_instances ON runtime_instances.org_id = workspaces.org_id
                        AND runtime_instances.project_id = workspaces.project_id
                        AND runtime_instances.environment_id = workspaces.environment_id
                        AND runtime_instances.workspace_id = workspaces.id
                        AND runtime_instances.observed_state = 'ready'
 WHERE workspaces.org_id = sqlc.arg(org_id) AND workspaces.id = sqlc.arg(workspace_id)
ON CONFLICT (workspace_id) WHERE state IN ('mounting','mounted','unmounting')
DO UPDATE SET updated_at = workspace_mounts.updated_at
RETURNING workspace_mounts.*, (xmax = 0) AS inserted,
          CASE WHEN xmax = 0 THEN 'created'::text ELSE 'replayed'::text END AS decision;

-- name: ClassifyRunWorkspaceReuse :one
SELECT workspaces.id AS workspace_id, workspace_mounts.id AS workspace_mount_id,
       workspace_mounts.runtime_instance_id, workspace_mounts.state,
       workspace_mounts.fencing_generation
  FROM workspaces
  LEFT JOIN workspace_mounts ON workspace_mounts.workspace_id = workspaces.id
                            AND workspace_mounts.state IN ('mounting','mounted','unmounting')
 WHERE workspaces.org_id = sqlc.arg(org_id) AND workspaces.id = sqlc.arg(workspace_id);

-- name: GetWorkspaceMount :one
SELECT * FROM workspace_mounts
 WHERE org_id = sqlc.arg(org_id) AND workspace_id = sqlc.arg(workspace_id)
   AND id = sqlc.arg(id);

-- name: GetWorkspaceMountForWorkerPrimitiveScope :one
SELECT * FROM workspace_mounts
 WHERE org_id = sqlc.arg(org_id) AND workspace_id = sqlc.arg(workspace_id)
   AND id = sqlc.arg(id) AND worker_instance_id = sqlc.arg(worker_instance_id)
   AND worker_epoch = sqlc.arg(worker_epoch) AND runtime_instance_id = sqlc.arg(runtime_instance_id);

-- name: GetWorkspaceMountForWorkerTransition :one
SELECT * FROM workspace_mounts
 WHERE org_id = sqlc.arg(org_id) AND id = sqlc.arg(id)
   AND worker_instance_id = sqlc.arg(worker_instance_id)
   AND worker_epoch = sqlc.arg(worker_epoch)
   AND state IN ('mounting','mounted','unmounting');

-- name: GetWorkspaceMountPrerequisites :one
SELECT workspaces.id AS workspace_id, workspaces.current_version_id,
       workspace_versions.id AS current_workspace_version_id,
       workspace_versions.state AS current_workspace_version_state,
       workspace_versions.artifact_id AS current_workspace_artifact_id,
       workspace_artifacts.id AS workspace_artifact_id,
       workspace_artifacts.media_type AS workspace_artifact_media_type,
       deployment_sandboxes.image_artifact_id AS sandbox_image_artifact_id,
       image_artifacts.id AS image_artifact_id,
       image_artifacts.media_type AS image_artifact_media_type,
       active_mount.state AS active_mount_state
  FROM workspaces
  LEFT JOIN workspace_versions ON workspace_versions.org_id = workspaces.org_id
                              AND workspace_versions.workspace_id = workspaces.id
                              AND workspace_versions.id = workspaces.current_version_id
  LEFT JOIN artifacts AS workspace_artifacts ON workspace_artifacts.org_id = workspace_versions.org_id
                                             AND workspace_artifacts.id = workspace_versions.artifact_id
  LEFT JOIN deployment_sandboxes ON deployment_sandboxes.org_id = workspaces.org_id
                                AND deployment_sandboxes.project_id = workspaces.project_id
                                AND deployment_sandboxes.environment_id = workspaces.environment_id
                                AND deployment_sandboxes.id = workspaces.deployment_sandbox_id
  LEFT JOIN artifacts AS image_artifacts ON image_artifacts.org_id = deployment_sandboxes.org_id
                                        AND image_artifacts.id = deployment_sandboxes.image_artifact_id
  LEFT JOIN workspace_mounts AS active_mount ON active_mount.org_id = workspaces.org_id
                                             AND active_mount.workspace_id = workspaces.id
                                             AND active_mount.state IN ('mounting','mounted','unmounting')
 WHERE workspaces.org_id = sqlc.arg(org_id)
   AND workspaces.project_id = sqlc.arg(project_id)
   AND workspaces.environment_id = sqlc.arg(environment_id)
   AND workspaces.id = sqlc.arg(workspace_id);

-- name: ClaimWorkspaceMount :one
WITH candidate AS (
    SELECT workspace_mounts.id
      FROM workspace_mounts
      JOIN runtime_instances ON runtime_instances.org_id = workspace_mounts.org_id
                            AND runtime_instances.id = workspace_mounts.runtime_instance_id
                            AND runtime_instances.worker_instance_id = workspace_mounts.worker_instance_id
                            AND runtime_instances.worker_epoch = workspace_mounts.worker_epoch
     WHERE workspace_mounts.worker_instance_id = sqlc.arg(worker_instance_id)
       AND workspace_mounts.worker_epoch = sqlc.arg(worker_epoch)
       AND workspace_mounts.state = 'mounting'
       AND runtime_instances.observed_state = 'ready'
     ORDER BY workspace_mounts.requested_at, workspace_mounts.id
     LIMIT 1
     FOR UPDATE OF workspace_mounts SKIP LOCKED
), claimed AS (
    UPDATE workspace_mounts
       SET claim_attempt = claim_attempt + 1,
           guestd_channel_token_hash = sqlc.arg(guestd_channel_token_hash),
           guestd_channel_token_expires_at = sqlc.arg(guestd_channel_token_expires_at),
           updated_at = now()
      FROM candidate
     WHERE workspace_mounts.id = candidate.id
    RETURNING workspace_mounts.*
)
SELECT claimed.*, runtime_instances.runtime_identity_id AS runtime_id,
       worker_network_slots.id AS network_slot_id,
       worker_network_slots.generation AS network_slot_generation,
       runtime_instances.reserved_cpu_millis,
       runtime_instances.reserved_memory_bytes,
       runtime_instances.reserved_workload_disk_bytes,
       runtime_instances.reserved_execution_slots,
       image_artifacts.size_bytes AS image_artifact_size_bytes,
       image_artifacts.media_type AS image_artifact_media_type
  FROM claimed
  JOIN runtime_instances ON runtime_instances.org_id = claimed.org_id
                        AND runtime_instances.id = claimed.runtime_instance_id
  JOIN worker_network_slots ON worker_network_slots.worker_instance_id = runtime_instances.worker_instance_id
                    AND worker_network_slots.worker_epoch = runtime_instances.worker_epoch
                    AND worker_network_slots.runtime_instance_id = runtime_instances.id
                    AND worker_network_slots.state = 'bound'
  JOIN artifacts AS image_artifacts ON image_artifacts.org_id = claimed.org_id
                                   AND image_artifacts.id = claimed.image_artifact_id;

-- name: RenewWorkspaceMount :one
UPDATE workspace_mounts
   SET guestd_channel_token_expires_at = sqlc.arg(guestd_channel_token_expires_at),
       updated_at = now()
 WHERE org_id = sqlc.arg(org_id) AND id = sqlc.arg(id)
   AND worker_instance_id = sqlc.arg(worker_instance_id)
   AND worker_epoch = sqlc.arg(worker_epoch) AND runtime_instance_id = sqlc.arg(runtime_instance_id)
   AND state IN ('mounting','mounted','unmounting')
RETURNING *;

-- name: MarkWorkspaceMountMounted :one
UPDATE workspace_mounts
   SET state = 'mounted', mounted_at = COALESCE(mounted_at, now()), updated_at = now()
 WHERE org_id = sqlc.arg(org_id) AND id = sqlc.arg(id)
   AND worker_instance_id = sqlc.arg(worker_instance_id) AND worker_epoch = sqlc.arg(worker_epoch)
   AND runtime_instance_id = sqlc.arg(runtime_instance_id)
   AND fencing_generation = sqlc.arg(fencing_generation) AND state = 'mounting'
RETURNING *;

-- name: RequestWorkspaceMountStop :one
WITH mount AS (
    UPDATE workspace_mounts SET state = 'unmounting', stopped_at = COALESCE(stopped_at, now()),
                                updated_at = now()
     WHERE workspace_mounts.org_id = sqlc.arg(org_id)
       AND workspace_mounts.project_id = sqlc.arg(project_id)
       AND workspace_mounts.environment_id = sqlc.arg(environment_id)
       AND workspace_mounts.workspace_id = sqlc.arg(workspace_id)
       AND workspace_mounts.state IN ('mounting','mounted')
    RETURNING *
)
UPDATE runtime_instances SET desired_state = 'closed', desired_version = desired_version + 1,
                             desired_at = now(), desired_reason = sqlc.arg(reason_code), updated_at = now()
  FROM mount WHERE runtime_instances.id = mount.runtime_instance_id
RETURNING mount.*;

-- name: PromoteWorkspaceMountStopCapture :one
WITH target AS (
    SELECT workspace_mounts.*, workspaces.current_version_id
      FROM workspace_mounts
      JOIN workspaces ON workspaces.org_id = workspace_mounts.org_id
                     AND workspaces.project_id = workspace_mounts.project_id
                     AND workspaces.environment_id = workspace_mounts.environment_id
                     AND workspaces.id = workspace_mounts.workspace_id
     WHERE workspace_mounts.org_id = sqlc.arg(org_id)
       AND workspace_mounts.project_id = sqlc.arg(project_id)
       AND workspace_mounts.environment_id = sqlc.arg(environment_id)
       AND workspace_mounts.workspace_id = sqlc.arg(workspace_id)
       AND workspace_mounts.id = sqlc.arg(id)
       AND workspace_mounts.state = 'unmounting'
       AND workspace_mounts.worker_instance_id = sqlc.arg(worker_instance_id)
       AND workspace_mounts.worker_epoch = sqlc.arg(worker_epoch)
       AND workspace_mounts.runtime_instance_id = sqlc.arg(runtime_instance_id)
       AND workspace_mounts.fencing_generation = sqlc.arg(fencing_generation)
     FOR UPDATE OF workspace_mounts, workspaces
), created AS (
    INSERT INTO workspace_versions (
        id, public_id, org_id, project_id, environment_id, workspace_id,
        parent_version_id, source_workspace_mount_id, kind, state, artifact_id,
        artifact_encoding, artifact_entry_count, content_digest, size_bytes,
        message, promoted_at
    )
    SELECT sqlc.arg(workspace_version_id), sqlc.arg(workspace_version_public_id),
           target.org_id, target.project_id, target.environment_id, target.workspace_id,
           COALESCE(target.current_version_id, target.base_version_id), target.id,
           'system', 'ready', sqlc.arg(artifact_id), sqlc.arg(artifact_encoding),
           sqlc.arg(artifact_entry_count), sqlc.arg(content_digest), sqlc.arg(size_bytes),
           sqlc.arg(message), now()
      FROM target
    RETURNING *
), updated_workspace AS (
    UPDATE workspaces
       SET current_version_id = created.id, dirty_state = 'clean', updated_at = now()
      FROM created
     WHERE workspaces.org_id = created.org_id AND workspaces.id = created.workspace_id
    RETURNING workspaces.id
), updated_mount AS (
    UPDATE workspace_mounts
       SET base_version_id = created.id, dirty_generation = dirty_generation + 1,
           updated_at = now()
      FROM created, updated_workspace
     WHERE workspace_mounts.org_id = created.org_id
       AND workspace_mounts.id = created.source_workspace_mount_id
    RETURNING workspace_mounts.id
)
SELECT created.* FROM created JOIN updated_mount ON true;

-- name: StopWorkspaceMount :one
WITH stopped AS (
    UPDATE workspace_mounts
       SET state = 'unmounted', unmounted_at = now(), terminal_at = now(),
           terminal_reason_code = sqlc.arg(reason_code), terminal_error = NULL, updated_at = now()
     WHERE workspace_mounts.org_id = sqlc.arg(org_id)
       AND workspace_mounts.id = sqlc.arg(id) AND workspace_mounts.state = 'unmounting'
       AND workspace_mounts.worker_instance_id = sqlc.arg(worker_instance_id)
       AND workspace_mounts.worker_epoch = sqlc.arg(worker_epoch)
       AND workspace_mounts.runtime_instance_id = sqlc.arg(runtime_instance_id)
       AND workspace_mounts.fencing_generation = sqlc.arg(fencing_generation)
    RETURNING workspace_mounts.*
), closed_runtime AS (
    UPDATE runtime_instances
       SET desired_state = 'closed',
           desired_version = CASE WHEN runtime_instances.desired_state = 'closed'
                                  THEN runtime_instances.desired_version
                                  ELSE runtime_instances.desired_version + 1 END,
           desired_at = now(), desired_reason = 'workspace_unmounted',
           observed_state = 'closed', observed_version = runtime_instances.observed_version + 1,
           observed_desired_version = CASE WHEN runtime_instances.desired_state = 'closed'
                                           THEN runtime_instances.desired_version
                                           ELSE runtime_instances.desired_version + 1 END,
           observed_at = now(), closing_at = COALESCE(runtime_instances.closing_at, now()),
           closed_at = now(), terminal_at = now(), terminal_reason_code = 'workspace_unmounted',
           terminal_error = NULL, reclaimed_at = now(), updated_at = now()
      FROM stopped
     WHERE runtime_instances.org_id = stopped.org_id
       AND runtime_instances.id = stopped.runtime_instance_id
       AND runtime_instances.worker_instance_id = stopped.worker_instance_id
       AND runtime_instances.worker_epoch = stopped.worker_epoch
       AND runtime_instances.observed_state IN ('allocated','preparing','ready','closing')
    RETURNING runtime_instances.*
), reclaimed_slot AS (
    UPDATE worker_network_slots
       SET state = 'available', generation = worker_network_slots.generation + 1,
           runtime_instance_id = NULL, host_interface_name = NULL, guest_address = NULL,
           gateway_address = NULL, subnet = NULL, tap_name = NULL, netns_name = NULL,
           guest_mac = NULL, reclaiming_at = NULL, quarantined_at = NULL, lost_at = NULL,
           reclaimed_at = now(),
           reclaim_evidence = jsonb_build_object('reason_code', 'workspace_unmounted'),
           state_reason_code = NULL, state_error = NULL, updated_at = now()
      FROM closed_runtime
     WHERE worker_network_slots.worker_instance_id = closed_runtime.worker_instance_id
       AND worker_network_slots.worker_epoch = closed_runtime.worker_epoch
       AND worker_network_slots.runtime_instance_id = closed_runtime.id
       AND worker_network_slots.state IN ('bound','reclaiming')
    RETURNING worker_network_slots.id
)
SELECT stopped.* FROM stopped
  JOIN closed_runtime ON closed_runtime.id = stopped.runtime_instance_id
  JOIN reclaimed_slot ON true;

-- name: RequestCapacityPressureIdleWorkspaceMountStopsForWorker :many
WITH candidates AS (
    SELECT workspace_mounts.id FROM workspace_mounts
     WHERE workspace_mounts.worker_instance_id = sqlc.arg(worker_instance_id)
       AND workspace_mounts.worker_epoch = sqlc.arg(worker_epoch) AND workspace_mounts.state = 'mounted'
       AND NOT EXISTS (SELECT 1 FROM workspace_leases
                        WHERE workspace_mount_id = workspace_mounts.id AND state IN ('active','releasing'))
     ORDER BY workspace_mounts.updated_at, workspace_mounts.id LIMIT sqlc.arg(limit_count) FOR UPDATE SKIP LOCKED
)
UPDATE workspace_mounts SET state = 'unmounting', stopped_at = now(), updated_at = now()
  FROM candidates WHERE workspace_mounts.id = candidates.id
RETURNING workspace_mounts.*;

-- name: FailWorkspaceMount :one
UPDATE workspace_mounts
   SET state = 'failed', failed_at = now(), terminal_at = now(),
       terminal_reason_code = sqlc.arg(reason_code), terminal_error = sqlc.narg(error),
       updated_at = now()
 WHERE org_id = sqlc.arg(org_id) AND id = sqlc.arg(id)
   AND worker_instance_id = sqlc.arg(worker_instance_id) AND worker_epoch = sqlc.arg(worker_epoch)
   AND runtime_instance_id = sqlc.arg(runtime_instance_id)
   AND fencing_generation = sqlc.arg(fencing_generation)
   AND state IN ('mounting','mounted','unmounting')
RETURNING *;

-- name: MarkStaleWorkspaceMountsLost :many
UPDATE workspace_mounts
   SET state = 'lost', lost_at = now(), terminal_at = now(),
       terminal_reason_code = 'worker_epoch_lost', updated_at = now()
 WHERE id IN (
     SELECT workspace_mounts.id FROM workspace_mounts
      JOIN worker_instances ON worker_instances.id = workspace_mounts.worker_instance_id
      WHERE workspace_mounts.state IN ('mounting','mounted','unmounting')
        AND (workspace_mounts.worker_epoch IS DISTINCT FROM worker_instances.current_epoch
             OR worker_instances.state IN ('disabled', 'lost'))
      ORDER BY workspace_mounts.updated_at, workspace_mounts.id
      LIMIT sqlc.arg(limit_count) FOR UPDATE OF workspace_mounts SKIP LOCKED
 )
RETURNING *;
