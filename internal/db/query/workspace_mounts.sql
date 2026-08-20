-- name: EnsureRunWorkspaceMountRequested :one
WITH same_workspace_child_authority AS MATERIALIZED (
    SELECT child.id AS run_id
      FROM runs AS child
      JOIN run_attempts AS child_attempt
        ON child_attempt.run_id = child.id
       AND child_attempt.number = child.current_attempt_number
       AND child_attempt.workspace_id = child.workspace_id
       AND child_attempt.base_workspace_version_id = child.base_workspace_version_id
      JOIN run_waits AS edge
        ON edge.child_run_id = child.id
       AND edge.workspace_id = child.workspace_id
       AND edge.child_parent_owned IS TRUE
       AND edge.condition_state = 'pending'
       AND edge.suspension_state = 'parked'
       AND edge.base_workspace_version_id = child.base_workspace_version_id
       AND edge.ownership_generation IS NOT NULL
       AND edge.parent_writer_generation IS NOT NULL
      JOIN runs AS parent
        ON parent.id = edge.run_id
       AND parent.environment_id = edge.environment_id
       AND parent.workspace_id = edge.workspace_id
       AND parent.id = child.parent_run_id
       AND parent.status = 'waiting'
       AND parent.current_run_lease_id IS NULL
      JOIN run_checkpoints AS checkpoint
        ON checkpoint.id = edge.suspend_checkpoint_id
       AND checkpoint.run_id = edge.run_id
       AND checkpoint.attempt_number = edge.attempt_number
       AND checkpoint.run_wait_id = edge.id
       AND checkpoint.workspace_id = edge.workspace_id
       AND checkpoint.private_workspace_version_id = edge.base_workspace_version_id
       AND checkpoint.state = 'ready'
       AND (checkpoint.expires_at IS NULL
            OR checkpoint.expires_at > transaction_timestamp())
      JOIN workspaces
        ON workspaces.id = edge.workspace_id
       AND workspaces.environment_id = edge.environment_id
       AND workspaces.ownership_generation = edge.ownership_generation
       AND (
           workspaces.writer_generation = coalesce(
               edge.child_writer_generation,
               edge.parent_writer_generation
           )
           OR EXISTS (
               SELECT 1
                 FROM run_waits AS resume_edge
                WHERE resume_edge.run_id = child.id
                  AND resume_edge.attempt_number = child.current_attempt_number
                  AND resume_edge.workspace_id = child.workspace_id
                  AND resume_edge.suspension_state = 'resume_pending'
                  AND resume_edge.ownership_generation = edge.ownership_generation
                  AND resume_edge.parent_writer_generation = edge.child_writer_generation
                  AND resume_edge.child_writer_generation = workspaces.writer_generation
                  AND resume_edge.resume_writer_generation IS NULL
                  AND resume_edge.resume_workspace_version_id = sqlc.arg(workspace_version_id)
           )
       )
      LEFT JOIN LATERAL (
          SELECT child_workspace_lease.id
            FROM run_leases AS child_lease
            JOIN workspace_leases AS child_workspace_lease
              ON child_workspace_lease.owner_run_lease_id = child_lease.id
             AND child_workspace_lease.workspace_id = child_lease.workspace_id
             AND (
                 child_workspace_lease.base_version_id = edge.base_workspace_version_id
                 OR EXISTS (
                     SELECT 1
                       FROM run_waits AS prior_resume_edge
                      WHERE prior_resume_edge.run_id = child.id
                        AND prior_resume_edge.workspace_id = child.workspace_id
                        AND prior_resume_edge.suspension_state = 'resume_pending'
                        AND prior_resume_edge.ownership_generation = edge.ownership_generation
                        AND prior_resume_edge.resume_writer_generation IS NULL
                        AND prior_resume_edge.resume_workspace_version_id =
                            child_workspace_lease.base_version_id
                 )
             )
             AND child_workspace_lease.ownership_generation = edge.ownership_generation
             AND child_workspace_lease.writer_generation = edge.child_writer_generation
             AND child_workspace_lease.state IN ('released', 'fenced', 'expired', 'lost')
           WHERE child_lease.run_id = child.id
             AND child_lease.workspace_id = child.workspace_id
             AND (
                 child_lease.state IN ('failed', 'expired', 'lost', 'rejected')
                 OR (
                     child_lease.state = 'checkpointed'
                     AND EXISTS (
                         SELECT 1
                          FROM run_waits AS resume_edge
                          WHERE resume_edge.run_id = child.id
                            AND resume_edge.attempt_number = child_lease.attempt_number
                            AND resume_edge.workspace_id = child.workspace_id
                            AND resume_edge.suspension_state = 'resume_pending'
                            AND resume_edge.prior_run_lease_id = child_lease.id
                            AND resume_edge.ownership_generation = edge.ownership_generation
                            AND resume_edge.parent_writer_generation =
                                child_workspace_lease.writer_generation
                            AND resume_edge.resume_writer_generation IS NULL
                     )
                 )
             )
           ORDER BY child_lease.lease_sequence DESC
           LIMIT 1
      ) AS prior_child ON edge.child_writer_generation IS NOT NULL
     WHERE child.org_id = sqlc.arg(org_id)
       AND child.id = sqlc.arg(run_id)
       AND child.workspace_id = sqlc.arg(workspace_id)
       AND child.base_workspace_version_id = sqlc.arg(workspace_version_id)
       AND child.status = 'queued'
       AND child.current_run_lease_id IS NULL
       AND child.cause_kind = 'child'
       AND child.parent_owns_lifecycle IS TRUE
       AND (edge.child_writer_generation IS NULL OR prior_child.id IS NOT NULL)
)
INSERT INTO workspace_mounts (
    id, org_id, project_id, environment_id, region_id, worker_group_id,
    worker_instance_id, worker_epoch, workspace_id,
    materialized_version_id, runtime_instance_id, fencing_generation, request
)
SELECT sqlc.arg(id), runtime_instances.org_id, runtime_instances.project_id,
       runtime_instances.environment_id, runtime_instances.region_id,
       runtime_instances.worker_group_id, runtime_instances.worker_instance_id,
       runtime_instances.worker_epoch, runtime_instances.workspace_id,
       runtime_instances.reserved_workspace_version_id, runtime_instances.id,
       sqlc.arg(fencing_generation), sqlc.arg(request)
  FROM runtime_instances
  JOIN runs ON runs.environment_id = runtime_instances.environment_id
           AND runs.id = runtime_instances.reserved_run_id
           AND runs.deployment_id = runtime_instances.program_deployment_id
  JOIN run_attempts ON run_attempts.run_id = runtime_instances.reserved_run_id
                   AND run_attempts.number = runtime_instances.reserved_attempt_number
                   AND run_attempts.workspace_id = runtime_instances.workspace_id
  JOIN workspace_versions ON workspace_versions.workspace_id = runtime_instances.workspace_id
                         AND workspace_versions.id = runtime_instances.reserved_workspace_version_id
                         AND (
                             (runtime_instances.restore_checkpoint_id IS NOT NULL
                              AND workspace_versions.state = 'private')
                             OR
                             (runtime_instances.restore_checkpoint_id IS NULL
                              AND workspace_versions.state = 'committed')
                             OR
                             (runtime_instances.restore_checkpoint_id IS NULL
                              AND workspace_versions.state = 'private'
                              AND EXISTS (
                                  SELECT 1
                                    FROM same_workspace_child_authority
                                   WHERE same_workspace_child_authority.run_id = runs.id
                              ))
                         )
 WHERE runtime_instances.org_id = sqlc.arg(org_id)
   AND runtime_instances.workspace_id = sqlc.arg(workspace_id)
   AND runtime_instances.id = sqlc.arg(runtime_instance_id)
   AND runtime_instances.reserved_run_id = sqlc.arg(run_id)
   AND runtime_instances.reserved_attempt_number = sqlc.arg(attempt_number)
   AND runtime_instances.reserved_workspace_version_id = sqlc.arg(workspace_version_id)
   AND runtime_instances.observed_state = 'ready'
   AND runtime_instances.reclaimed_at IS NULL
   AND runtime_instances.reservation_expires_at > transaction_timestamp()
ON CONFLICT (workspace_id) WHERE state IN ('mounting','mounted','unmounting')
DO UPDATE SET updated_at = workspace_mounts.updated_at
WHERE workspace_mounts.runtime_instance_id = excluded.runtime_instance_id
  AND workspace_mounts.materialized_version_id = excluded.materialized_version_id
  AND workspace_mounts.fencing_generation = excluded.fencing_generation
RETURNING workspace_mounts.*, (xmax = 0) AS inserted,
          CASE WHEN xmax = 0 THEN 'created'::text ELSE 'replayed'::text END AS decision;

-- name: EnsureProcessWorkspaceMountRequested :one
INSERT INTO workspace_mounts (
    id, org_id, project_id, environment_id, region_id, worker_group_id,
    worker_instance_id, worker_epoch, workspace_id,
    materialized_version_id, runtime_instance_id, request
)
SELECT sqlc.arg(id), runtime_instances.org_id, runtime_instances.project_id,
       runtime_instances.environment_id, runtime_instances.region_id,
       runtime_instances.worker_group_id, runtime_instances.worker_instance_id,
       runtime_instances.worker_epoch, runtime_instances.workspace_id,
       runtime_instances.reserved_workspace_version_id, runtime_instances.id,
       sqlc.arg(request)
  FROM runtime_instances
  JOIN workspace_processes
    ON workspace_processes.id = runtime_instances.reserved_process_id
   AND workspace_processes.workspace_id = runtime_instances.workspace_id
   AND workspace_processes.state = 'pending'
  JOIN workspace_versions
    ON workspace_versions.workspace_id = runtime_instances.workspace_id
   AND workspace_versions.id = runtime_instances.reserved_workspace_version_id
   AND workspace_versions.state = 'committed'
 WHERE runtime_instances.org_id = sqlc.arg(org_id)
   AND runtime_instances.workspace_id = sqlc.arg(workspace_id)
   AND runtime_instances.id = sqlc.arg(runtime_instance_id)
   AND runtime_instances.reserved_process_id = sqlc.arg(process_id)
   AND runtime_instances.reserved_workspace_version_id = sqlc.arg(workspace_version_id)
   AND runtime_instances.observed_state = 'ready'
   AND runtime_instances.reclaimed_at IS NULL
   AND runtime_instances.reservation_expires_at > transaction_timestamp()
ON CONFLICT (workspace_id) WHERE state IN ('mounting','mounted','unmounting')
DO UPDATE SET updated_at = workspace_mounts.updated_at
WHERE workspace_mounts.runtime_instance_id = excluded.runtime_instance_id
  AND workspace_mounts.materialized_version_id = excluded.materialized_version_id
RETURNING workspace_mounts.*, (xmax = 0) AS inserted,
          CASE WHEN xmax = 0 THEN 'created'::text ELSE 'replayed'::text END AS decision;

-- name: ClassifyRunWorkspaceReuse :one
SELECT workspaces.id AS workspace_id, workspace_mounts.id AS workspace_mount_id,
       workspace_mounts.runtime_instance_id, workspace_mounts.state,
       workspace_mounts.fencing_generation
  FROM workspaces
  JOIN environments ON environments.id = workspaces.environment_id
  LEFT JOIN workspace_mounts ON workspace_mounts.workspace_id = workspaces.id
                            AND workspace_mounts.state IN ('mounting','mounted','unmounting')
 WHERE environments.org_id = sqlc.arg(org_id) AND workspaces.id = sqlc.arg(workspace_id);

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
SELECT workspaces.id AS workspace_id, workspaces.head_version_id,
       workspace_versions.id AS current_workspace_version_id,
       workspace_versions.state AS current_workspace_version_state,
       workspace_versions.artifact_id AS current_workspace_artifact_id,
       workspace_artifacts.id AS workspace_artifact_id,
       workspace_artifacts.media_type AS workspace_artifact_media_type,
       deployment_definitions.artifact_id AS workspace_image_artifact_id,
       image_artifacts.id AS image_artifact_id,
       image_artifacts.media_type AS image_artifact_media_type,
       active_mount.state AS active_mount_state
  FROM workspaces
  LEFT JOIN workspace_versions ON workspace_versions.environment_id = workspaces.environment_id
                              AND workspace_versions.workspace_id = workspaces.id
                              AND workspace_versions.id = workspaces.head_version_id
  LEFT JOIN artifacts AS workspace_artifacts ON workspace_artifacts.environment_id = workspace_versions.environment_id
                                             AND workspace_artifacts.id = workspace_versions.artifact_id
  LEFT JOIN deployment_definitions
    ON deployment_definitions.environment_id = workspaces.environment_id
   AND deployment_definitions.id = workspaces.deployment_definition_id
   AND deployment_definitions.kind = 'sandbox'
  LEFT JOIN artifacts AS image_artifacts
    ON image_artifacts.environment_id = deployment_definitions.environment_id
   AND image_artifacts.id = deployment_definitions.artifact_id
  JOIN environments ON environments.id = workspaces.environment_id
  LEFT JOIN workspace_mounts AS active_mount ON active_mount.workspace_id = workspaces.id
                                             AND active_mount.state IN ('mounting','mounted','unmounting')
 WHERE environments.org_id = sqlc.arg(org_id)
   AND environments.project_id = sqlc.arg(project_id)
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
       AND runtime_instances.reserved_workspace_version_id = workspace_mounts.materialized_version_id
       AND num_nonnulls(runtime_instances.reserved_run_id, runtime_instances.reserved_process_id) = 1
       AND runtime_instances.reservation_expires_at > transaction_timestamp()
     ORDER BY workspace_mounts.requested_at, workspace_mounts.id
     LIMIT 1
     FOR UPDATE OF workspace_mounts SKIP LOCKED
), claimed AS (
    UPDATE workspace_mounts
       SET claim_attempt = claim_attempt + 1,
           guest_channel_token_hash = sqlc.arg(guest_channel_token_hash),
           guest_channel_token_expires_at = sqlc.arg(guest_channel_token_expires_at),
           updated_at = now()
      FROM candidate
     WHERE workspace_mounts.id = candidate.id
    RETURNING workspace_mounts.*
)
SELECT claimed.*, runtime_instances.runtime_identity_id AS runtime_id,
	   runtime_instances.restore_checkpoint_id,
	   restore_checkpoint.base_workspace_version_id AS restore_source_version_id,
       runtime_instances.deployment_definition_id,
       runtime_identities.rootfs_digest,
       runtime_identities.vm_runtime_contract,
       runtime_instances.reserved_cpu_millis,
       runtime_instances.reserved_memory_bytes,
       runtime_instances.reserved_guest_ephemeral_disk_bytes,
       runtime_instances.reserved_execution_slots,
       image_artifacts.id AS image_artifact_id,
       image_artifacts.digest AS image_artifact_digest,
       image_artifacts.size_bytes AS image_artifact_size_bytes,
       image_artifacts.media_type AS image_artifact_media_type,
       workspace_versions.artifact_id AS workspace_artifact_id,
       COALESCE(workspace_artifacts.digest, '') AS workspace_artifact_digest,
       COALESCE(workspace_artifacts.size_bytes, 0) AS workspace_artifact_size_bytes,
       COALESCE(workspace_artifacts.media_type, '') AS workspace_artifact_media_type,
	   workspace_versions.content_digest AS workspace_content_digest,
	   workspace_versions.size_bytes AS workspace_logical_size_bytes,
       workspace_versions.entry_count AS workspace_entry_count
  FROM claimed
  JOIN runtime_instances ON runtime_instances.org_id = claimed.org_id
                        AND runtime_instances.id = claimed.runtime_instance_id
  JOIN runtime_identities ON runtime_identities.id = runtime_instances.runtime_identity_id
	LEFT JOIN run_checkpoints AS restore_checkpoint
	  ON restore_checkpoint.id = runtime_instances.restore_checkpoint_id
	 AND restore_checkpoint.workspace_id = claimed.workspace_id
	 AND restore_checkpoint.state = 'ready'
  JOIN deployment_definitions
    ON deployment_definitions.environment_id = runtime_instances.environment_id
   AND deployment_definitions.id = runtime_instances.deployment_definition_id
   AND deployment_definitions.kind = 'sandbox'
  JOIN workspace_versions
    ON workspace_versions.workspace_id = claimed.workspace_id
   AND workspace_versions.id = claimed.materialized_version_id
  LEFT JOIN artifacts AS workspace_artifacts
    ON workspace_artifacts.environment_id = workspace_versions.environment_id
   AND workspace_artifacts.id = workspace_versions.artifact_id
  JOIN artifacts AS image_artifacts
    ON image_artifacts.environment_id = deployment_definitions.environment_id
   AND image_artifacts.id = deployment_definitions.artifact_id;

-- name: RenewWorkspaceMount :one
UPDATE workspace_mounts
   SET guest_channel_token_expires_at = sqlc.arg(guest_channel_token_expires_at),
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

-- name: RequestWorkspaceDeleteMountStop :one
WITH mount AS (
    UPDATE workspace_mounts
       SET state = 'unmounting',
           finalization_kind = 'discard',
           finalization_reason_code = 'workspace_deleted',
           finalization_error = NULL,
           stopped_at = COALESCE(stopped_at, transaction_timestamp()),
           updated_at = transaction_timestamp()
     WHERE workspace_mounts.org_id = sqlc.arg(org_id)
       AND workspace_mounts.project_id = sqlc.arg(project_id)
       AND workspace_mounts.environment_id = sqlc.arg(environment_id)
       AND workspace_mounts.workspace_id = sqlc.arg(workspace_id)
       AND (
           workspace_mounts.state IN ('mounting','mounted')
           OR (
               workspace_mounts.state = 'unmounting'
               AND (
                   workspace_mounts.finalization_kind IS NULL
                   OR (
                       workspace_mounts.finalization_kind = 'discard'
                       AND workspace_mounts.finalization_reason_code = 'workspace_deleted'
                       AND workspace_mounts.finalization_error IS NULL
                   )
               )
           )
       )
    RETURNING *
)
UPDATE runtime_instances
   SET desired_state = 'closed',
       desired_version = CASE
           WHEN runtime_instances.desired_state = 'closed'
               THEN runtime_instances.desired_version
           ELSE runtime_instances.desired_version + 1
       END,
       desired_at = transaction_timestamp(),
       desired_reason = 'workspace_deleted',
       updated_at = transaction_timestamp()
  FROM mount
 WHERE runtime_instances.id = mount.runtime_instance_id
RETURNING mount.*;

-- name: PromoteWorkspaceMountStopCapture :one
WITH target AS (
    SELECT workspace_mounts.*, workspaces.head_version_id,
           workspaces.ownership_generation, workspaces.writer_generation,
           workspace_leases.id AS source_workspace_lease_id
      FROM workspace_mounts
      JOIN workspaces ON workspaces.environment_id = workspace_mounts.environment_id
                     AND workspaces.id = workspace_mounts.workspace_id
      JOIN workspace_leases
        ON workspace_leases.workspace_id = workspace_mounts.workspace_id
       AND workspace_leases.workspace_mount_id = workspace_mounts.id
       AND workspace_leases.state IN ('active', 'releasing')
       AND workspace_leases.expires_at > now()
       AND workspace_leases.ownership_generation = workspaces.ownership_generation
       AND workspace_leases.writer_generation = workspaces.writer_generation
       AND workspace_leases.mount_fencing_generation = workspace_mounts.fencing_generation
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
       AND workspaces.ownership_generation = sqlc.arg(ownership_generation)
       AND workspaces.writer_generation = sqlc.arg(writer_generation)
     FOR UPDATE OF workspace_mounts, workspaces
), created AS (
    INSERT INTO workspace_versions (
        id, environment_id, workspace_id,
        parent_version_id, artifact_id, artifact_kind, kind, content_digest, size_bytes,
        entry_count, state, source_workspace_lease_id, ownership_generation,
        writer_generation, published_at
    )
    SELECT sqlc.arg(workspace_version_id),
           target.environment_id, target.workspace_id,
           target.head_version_id, sqlc.arg(artifact_id), 'workspace_version', 'system',
           sqlc.arg(content_digest), sqlc.arg(size_bytes),
           sqlc.arg(entry_count), 'committed', target.source_workspace_lease_id,
           target.ownership_generation, target.writer_generation, now()
      FROM target
    RETURNING *
), updated_workspace AS (
    UPDATE workspaces
       SET head_version_id = created.id, dirty_state = 'clean', updated_at = now()
      FROM created
     WHERE workspaces.environment_id = created.environment_id
       AND workspaces.id = created.workspace_id
    RETURNING workspaces.id
), updated_mount AS (
    UPDATE workspace_mounts
       SET materialized_version_id = created.id, dirty_generation = dirty_generation + 1,
           updated_at = now()
      FROM created, updated_workspace, target
     WHERE workspace_mounts.environment_id = created.environment_id
       AND workspace_mounts.id = target.id
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
           terminal_error = NULL, reclaimed_at = now(),
           reclaim_evidence = sqlc.arg(cleanup_proof)::jsonb,
           reserved_run_id = NULL, reserved_attempt_number = NULL,
           reserved_process_id = NULL, reserved_workspace_version_id = NULL,
           reservation_expires_at = NULL, updated_at = now()
      FROM stopped
     WHERE runtime_instances.org_id = stopped.org_id
       AND runtime_instances.id = stopped.runtime_instance_id
       AND runtime_instances.worker_instance_id = stopped.worker_instance_id
       AND runtime_instances.worker_epoch = stopped.worker_epoch
       AND runtime_instances.observed_state IN ('allocated','preparing','ready','closing')
    RETURNING runtime_instances.*
)
SELECT stopped.* FROM stopped
  JOIN closed_runtime ON closed_runtime.id = stopped.runtime_instance_id;

-- name: RequestCapacityPressureIdleWorkspaceMountStopsForWorker :many
WITH candidates AS (
    SELECT workspace_mounts.id
      FROM workspace_mounts
      JOIN workspaces
        ON workspaces.environment_id = workspace_mounts.environment_id
       AND workspaces.id = workspace_mounts.workspace_id
     WHERE workspace_mounts.worker_instance_id = sqlc.arg(worker_instance_id)
       AND workspace_mounts.worker_epoch = sqlc.arg(worker_epoch) AND workspace_mounts.state = 'mounted'
       AND workspaces.state = 'active'
       AND workspaces.desired_state = 'active'
       AND workspaces.dirty_state = 'clean'
       AND workspaces.head_version_id = workspace_mounts.materialized_version_id
       AND NOT EXISTS (SELECT 1 FROM workspace_leases
                        WHERE workspace_mount_id = workspace_mounts.id AND state IN ('active','releasing'))
       AND NOT EXISTS (SELECT 1 FROM workspace_processes
                        WHERE workspace_id = workspace_mounts.workspace_id
                          AND state IN ('pending','starting','running','exit_requested'))
     ORDER BY workspace_mounts.updated_at, workspace_mounts.id
     LIMIT sqlc.arg(limit_count)
     FOR UPDATE OF workspace_mounts SKIP LOCKED
)
UPDATE workspace_mounts
   SET state = 'unmounting',
       finalization_kind = 'discard',
       finalization_reason_code = 'capacity_pressure',
       finalization_error = NULL,
       stopped_at = now(),
       updated_at = now()
  FROM candidates WHERE workspace_mounts.id = candidates.id
RETURNING workspace_mounts.*;

-- name: FailWorkspaceMount :one
WITH target AS (
    SELECT workspace_mounts.*
      FROM workspace_mounts
      JOIN runtime_instances
        ON runtime_instances.org_id = workspace_mounts.org_id
       AND runtime_instances.id = workspace_mounts.runtime_instance_id
       AND runtime_instances.worker_instance_id = workspace_mounts.worker_instance_id
       AND runtime_instances.worker_epoch = workspace_mounts.worker_epoch
       AND runtime_instances.observed_state IN ('allocated','preparing','ready','closing')
       AND runtime_instances.reclaimed_at IS NULL
     WHERE workspace_mounts.org_id = sqlc.arg(org_id)
       AND workspace_mounts.id = sqlc.arg(id)
       AND workspace_mounts.worker_instance_id = sqlc.arg(worker_instance_id)
       AND workspace_mounts.worker_epoch = sqlc.arg(worker_epoch)
       AND workspace_mounts.runtime_instance_id = sqlc.arg(runtime_instance_id)
       AND workspace_mounts.fencing_generation = sqlc.arg(fencing_generation)
       AND workspace_mounts.state IN ('mounting','mounted','unmounting')
     FOR UPDATE OF workspace_mounts, runtime_instances
), failed_runtime AS (
    UPDATE runtime_instances
       SET observed_state = 'failed', observed_version = observed_version + 1,
           observed_at = now(), failed_at = now(), terminal_at = now(),
           terminal_reason_code = 'workspace_mount_failed',
           terminal_error = sqlc.narg(error),
           reserved_run_id = NULL, reserved_attempt_number = NULL,
           reserved_process_id = NULL, reserved_workspace_version_id = NULL,
           reservation_expires_at = NULL, updated_at = now()
      FROM target
     WHERE runtime_instances.org_id = target.org_id
       AND runtime_instances.id = target.runtime_instance_id
       AND runtime_instances.worker_instance_id = target.worker_instance_id
       AND runtime_instances.worker_epoch = target.worker_epoch
    RETURNING runtime_instances.*
)
UPDATE workspace_mounts
   SET state = 'failed', failed_at = now(), terminal_at = now(),
       terminal_reason_code = sqlc.arg(reason_code), terminal_error = sqlc.narg(error),
       updated_at = now()
  FROM target, failed_runtime
 WHERE workspace_mounts.org_id = target.org_id
   AND workspace_mounts.id = target.id
   AND failed_runtime.id = target.runtime_instance_id
RETURNING workspace_mounts.*;
