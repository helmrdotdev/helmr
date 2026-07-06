-- name: GetRunRestorePayload :one
SELECT
    runtime_checkpoints.id AS runtime_checkpoint_id,
    runtime_checkpoints.manifest,
    run_waits.id AS run_wait_id,
    run_waits.correlation_id AS run_wait_correlation_id,
    run_waits.kind AS run_wait_kind,
    run_waits.state AS run_wait_state,
    streams.name AS stream_name,
    matched_stream_record.sequence AS stream_record_sequence,
    matched_stream_record.data AS stream_record_data,
    tokens.state AS token_state,
    tokens.completion_data AS token_completion_data,
    timer_waits.fire_at AS timer_fire_at
  FROM runs
  JOIN run_leases ON run_leases.org_id = runs.org_id
                      AND run_leases.run_id = runs.id
                      AND run_leases.worker_group_id = runs.worker_group_id
                      AND run_leases.id = runs.current_run_lease_id
                      AND run_leases.restore_runtime_checkpoint_id = runs.latest_runtime_checkpoint_id
  JOIN runtime_checkpoints ON runtime_checkpoints.org_id = runs.org_id
                  AND runtime_checkpoints.worker_group_id = runs.worker_group_id
                  AND runtime_checkpoints.run_id = runs.id
                  AND runtime_checkpoints.id = runs.latest_runtime_checkpoint_id
  JOIN worker_instances ON worker_instances.id = run_leases.worker_instance_id
                       AND worker_instances.worker_group_id = runs.worker_group_id
  JOIN (
    SELECT placement_project.org_id,
           placement_project.id AS project_id,
           target_environment.id AS environment_id,
           placement_worker_group.region_id AS region_id,
           placement_worker_group.id AS worker_group_id,
           placement_worker_group.state AS worker_group_state
      FROM projects AS placement_project
      JOIN environments AS target_environment
        ON target_environment.org_id = placement_project.org_id
       AND target_environment.project_id = placement_project.id
      JOIN worker_groups AS placement_worker_group
        ON true
) AS project_worker_group_placement
    ON project_worker_group_placement.org_id = runs.org_id
   AND project_worker_group_placement.project_id = runs.project_id
   AND project_worker_group_placement.environment_id = runs.environment_id
   AND project_worker_group_placement.worker_group_id = runs.worker_group_id
   AND project_worker_group_placement.worker_group_state IN ('active', 'draining')
  JOIN worker_groups ON worker_groups.id = project_worker_group_placement.worker_group_id
            AND worker_groups.state = 'active'
  JOIN runtime_checkpoint_restores ON runtime_checkpoint_restores.org_id = runs.org_id
                                  AND runtime_checkpoint_restores.worker_group_id = runs.worker_group_id
                                  AND runtime_checkpoint_restores.run_id = runs.id
                                  AND runtime_checkpoint_restores.run_lease_id = run_leases.id
                                  AND runtime_checkpoint_restores.runtime_checkpoint_id = runtime_checkpoints.id
                                  AND runtime_checkpoint_restores.status = 'restoring'
  JOIN run_waits ON run_waits.org_id = runs.org_id
                AND run_waits.project_id = runs.project_id
                AND run_waits.environment_id = runs.environment_id
                AND run_waits.worker_group_id = runs.worker_group_id
                AND run_waits.run_id = runs.id
                AND run_waits.runtime_checkpoint_id = runtime_checkpoints.id
                AND run_waits.state = 'resuming'
  LEFT JOIN stream_waits ON stream_waits.org_id = run_waits.org_id
                        AND stream_waits.worker_group_id = run_waits.worker_group_id
                        AND stream_waits.project_id = run_waits.project_id
                        AND stream_waits.environment_id = run_waits.environment_id
                        AND stream_waits.run_wait_id = run_waits.id
  LEFT JOIN streams ON streams.org_id = stream_waits.org_id
                   AND streams.worker_group_id = stream_waits.worker_group_id
                   AND streams.project_id = stream_waits.project_id
                   AND streams.environment_id = stream_waits.environment_id
                   AND streams.id = stream_waits.stream_id
  LEFT JOIN stream_records AS matched_stream_record
         ON matched_stream_record.org_id = stream_waits.org_id
        AND matched_stream_record.worker_group_id = stream_waits.worker_group_id
        AND matched_stream_record.stream_id = stream_waits.stream_id
        AND matched_stream_record.id = stream_waits.matched_record_id
  LEFT JOIN token_waits ON token_waits.org_id = run_waits.org_id
                       AND token_waits.worker_group_id = run_waits.worker_group_id
                       AND token_waits.project_id = run_waits.project_id
                       AND token_waits.environment_id = run_waits.environment_id
                       AND token_waits.run_wait_id = run_waits.id
  LEFT JOIN tokens ON tokens.org_id = token_waits.org_id
                  AND tokens.worker_group_id = token_waits.worker_group_id
                  AND tokens.project_id = token_waits.project_id
                  AND tokens.environment_id = token_waits.environment_id
                  AND tokens.id = token_waits.token_id
  LEFT JOIN timer_waits ON timer_waits.org_id = run_waits.org_id
                       AND timer_waits.worker_group_id = run_waits.worker_group_id
                       AND timer_waits.project_id = run_waits.project_id
                       AND timer_waits.environment_id = run_waits.environment_id
                       AND timer_waits.run_wait_id = run_waits.id
 WHERE runs.org_id = sqlc.arg(org_id)
   AND runs.id = sqlc.arg(run_id)
   AND runs.current_run_lease_id = sqlc.arg(run_lease_id)
   AND run_leases.worker_instance_id = sqlc.arg(worker_instance_id)
   AND run_leases.status IN ('leased', 'running')
   AND run_leases.lease_expires_at > now()
   AND runs.latest_runtime_checkpoint_id IS NOT NULL
   AND runtime_checkpoints.state = 'ready'
   AND (runtime_checkpoints.expires_at IS NULL OR runtime_checkpoints.expires_at > now())
 LIMIT 1;

-- name: GetAcknowledgedReadyRuntimeCheckpointForRunWait :one
SELECT runtime_checkpoints.id AS runtime_checkpoint_id
  FROM runtime_checkpoints
  JOIN run_waits
    ON run_waits.org_id = runtime_checkpoints.org_id
   AND run_waits.worker_group_id = runtime_checkpoints.worker_group_id
   AND run_waits.project_id = runtime_checkpoints.project_id
   AND run_waits.environment_id = runtime_checkpoints.environment_id
   AND run_waits.run_id = runtime_checkpoints.run_id
   AND run_waits.runtime_checkpoint_id = runtime_checkpoints.id
  JOIN worker_commands
    ON worker_commands.org_id = runtime_checkpoints.org_id
   AND worker_commands.worker_group_id = runtime_checkpoints.worker_group_id
   AND worker_commands.project_id = runtime_checkpoints.project_id
   AND worker_commands.environment_id = runtime_checkpoints.environment_id
   AND worker_commands.run_id = runtime_checkpoints.run_id
   AND worker_commands.run_wait_id = run_waits.id
   AND worker_commands.run_lease_id = runtime_checkpoints.owner_run_lease_id
   AND worker_commands.worker_instance_id = runtime_checkpoints.owner_worker_instance_id
   AND worker_commands.runtime_instance_id = runtime_checkpoints.owner_runtime_instance_id
   AND worker_commands.runtime_epoch = runtime_checkpoints.owner_runtime_epoch
   AND worker_commands.kind = 'runtime_checkpoint_wait'
  JOIN worker_instances
    ON worker_instances.id = runtime_checkpoints.owner_worker_instance_id
   AND worker_instances.worker_group_id = runtime_checkpoints.worker_group_id
  JOIN (
    SELECT placement_project.org_id,
           placement_project.id AS project_id,
           target_environment.id AS environment_id,
           placement_worker_group.region_id AS region_id,
           placement_worker_group.id AS worker_group_id,
           placement_worker_group.state AS worker_group_state
      FROM projects AS placement_project
      JOIN environments AS target_environment
        ON target_environment.org_id = placement_project.org_id
       AND target_environment.project_id = placement_project.id
      JOIN worker_groups AS placement_worker_group
        ON true
) AS project_worker_group_placement
    ON project_worker_group_placement.org_id = runtime_checkpoints.org_id
   AND project_worker_group_placement.project_id = runtime_checkpoints.project_id
   AND project_worker_group_placement.environment_id = runtime_checkpoints.environment_id
   AND project_worker_group_placement.worker_group_id = runtime_checkpoints.worker_group_id
   AND project_worker_group_placement.worker_group_state IN ('active', 'draining')
  JOIN worker_groups ON worker_groups.id = project_worker_group_placement.worker_group_id
            AND worker_groups.state = 'active'
 WHERE runtime_checkpoints.org_id = sqlc.arg(org_id)
   AND runtime_checkpoints.run_id = sqlc.arg(run_id)
   AND runtime_checkpoints.id = sqlc.arg(runtime_checkpoint_id)
   AND runtime_checkpoints.state = 'ready'
   AND run_waits.id = sqlc.arg(run_wait_id)
   AND run_waits.owner_run_lease_id = sqlc.arg(run_lease_id)
   AND run_waits.owner_worker_instance_id = sqlc.arg(worker_instance_id)
   AND worker_commands.id = sqlc.arg(worker_command_id)
   AND worker_commands.acknowledged_at IS NOT NULL
 LIMIT 1;

-- name: CreateReadyRuntimeCheckpointForRunWait :one
WITH wait_scope AS (
    SELECT run_waits.*,
           runs.workspace_id,
           runs.current_run_lease_id,
           runs.active_started_at,
           runs.active_elapsed_ms,
           runs.max_active_duration_ms,
           workspace_leases.id AS workspace_lease_id,
           workspace_leases.workspace_mount_id
      FROM run_waits
      JOIN runs ON runs.org_id = run_waits.org_id
               AND runs.worker_group_id = run_waits.worker_group_id
               AND runs.project_id = run_waits.project_id
               AND runs.environment_id = run_waits.environment_id
               AND runs.id = run_waits.run_id
      JOIN run_leases ON run_leases.org_id = runs.org_id
                     AND run_leases.worker_group_id = runs.worker_group_id
                     AND run_leases.run_id = runs.id
                     AND run_leases.id = runs.current_run_lease_id
      JOIN runtime_checkpoints
        ON runtime_checkpoints.org_id = run_waits.org_id
       AND runtime_checkpoints.worker_group_id = run_waits.worker_group_id
       AND runtime_checkpoints.project_id = run_waits.project_id
       AND runtime_checkpoints.environment_id = run_waits.environment_id
       AND runtime_checkpoints.run_id = run_waits.run_id
       AND runtime_checkpoints.id = run_waits.runtime_checkpoint_id
       AND runtime_checkpoints.state = 'creating'
       AND runtime_checkpoints.owner_runtime_instance_id = run_waits.owner_runtime_instance_id
       AND runtime_checkpoints.owner_runtime_epoch = run_waits.owner_runtime_epoch
       AND runtime_checkpoints.owner_run_id = run_waits.run_id
       AND runtime_checkpoints.owner_run_wait_id = run_waits.id
       AND runtime_checkpoints.owner_run_lease_id = run_waits.owner_run_lease_id
       AND runtime_checkpoints.owner_worker_instance_id = run_waits.owner_worker_instance_id
       AND runtime_checkpoints.source_worker_instance_id = run_waits.owner_worker_instance_id
      JOIN worker_commands
        ON worker_commands.org_id = run_waits.org_id
       AND worker_commands.worker_group_id = run_waits.worker_group_id
       AND worker_commands.project_id = run_waits.project_id
       AND worker_commands.environment_id = run_waits.environment_id
       AND worker_commands.run_id = run_waits.run_id
       AND worker_commands.run_wait_id = run_waits.id
       AND worker_commands.run_lease_id = run_waits.owner_run_lease_id
       AND worker_commands.worker_instance_id = run_waits.owner_worker_instance_id
       AND worker_commands.runtime_instance_id = run_waits.owner_runtime_instance_id
       AND worker_commands.runtime_epoch = run_waits.owner_runtime_epoch
       AND worker_commands.run_state_version = run_waits.owner_run_state_version
       AND worker_commands.kind = 'runtime_checkpoint_wait'
       AND worker_commands.id = sqlc.arg(worker_command_id)
       AND worker_commands.accepted_at IS NOT NULL
       AND worker_commands.acknowledged_at IS NULL
      JOIN worker_instances
        ON worker_instances.id = run_waits.owner_worker_instance_id
       AND worker_instances.worker_group_id = run_waits.worker_group_id
      JOIN (
    SELECT placement_project.org_id,
           placement_project.id AS project_id,
           target_environment.id AS environment_id,
           placement_worker_group.region_id AS region_id,
           placement_worker_group.id AS worker_group_id,
           placement_worker_group.state AS worker_group_state
      FROM projects AS placement_project
      JOIN environments AS target_environment
        ON target_environment.org_id = placement_project.org_id
       AND target_environment.project_id = placement_project.id
      JOIN worker_groups AS placement_worker_group
        ON true
) AS project_worker_group_placement
        ON project_worker_group_placement.org_id = run_waits.org_id
       AND project_worker_group_placement.project_id = run_waits.project_id
       AND project_worker_group_placement.environment_id = run_waits.environment_id
       AND project_worker_group_placement.worker_group_id = run_waits.worker_group_id
       AND project_worker_group_placement.worker_group_state IN ('active', 'draining')
      JOIN worker_groups ON worker_groups.id = project_worker_group_placement.worker_group_id
                AND worker_groups.state = 'active'
      JOIN workspaces ON workspaces.org_id = runs.org_id
                     AND workspaces.worker_group_id = runs.worker_group_id
                     AND workspaces.project_id = runs.project_id
                     AND workspaces.environment_id = runs.environment_id
                     AND workspaces.id = runs.workspace_id
      JOIN workspace_versions ON workspace_versions.org_id = workspaces.org_id
                             AND workspace_versions.worker_group_id = workspaces.worker_group_id
                             AND workspace_versions.project_id = workspaces.project_id
                             AND workspace_versions.environment_id = workspaces.environment_id
                             AND workspace_versions.workspace_id = workspaces.id
                             AND workspace_versions.id = run_waits.workspace_version_id
                             AND workspace_versions.state = 'ready'
      JOIN workspace_leases ON workspace_leases.org_id = runs.org_id
                           AND workspace_leases.worker_group_id = runs.worker_group_id
                           AND workspace_leases.project_id = runs.project_id
                           AND workspace_leases.environment_id = runs.environment_id
                           AND workspace_leases.workspace_id = runs.workspace_id
                           AND workspace_leases.owner_run_id = runs.id
                           AND workspace_leases.lease_kind = 'write'
                           AND workspace_leases.state = 'active'
                           AND workspace_leases.released_at IS NULL
                           AND workspace_leases.expires_at > now()
      JOIN workspace_mounts ON workspace_mounts.org_id = workspace_leases.org_id
                           AND workspace_mounts.worker_group_id = workspace_leases.worker_group_id
                           AND workspace_mounts.project_id = workspace_leases.project_id
                           AND workspace_mounts.environment_id = workspace_leases.environment_id
                           AND workspace_mounts.workspace_id = workspace_leases.workspace_id
                           AND workspace_mounts.id = workspace_leases.workspace_mount_id
     WHERE run_waits.org_id = sqlc.arg(org_id)
       AND run_waits.project_id = sqlc.arg(project_id)
       AND run_waits.environment_id = sqlc.arg(environment_id)
       AND run_waits.id = sqlc.arg(run_wait_id)
       AND run_waits.run_id = sqlc.arg(run_id)
       AND run_waits.runtime_checkpoint_id = sqlc.arg(runtime_checkpoint_id)
       AND run_waits.state = 'checkpointing'
       AND runs.status = 'running'
       AND runs.current_run_lease_id = sqlc.arg(run_lease_id)
       AND run_leases.worker_instance_id = sqlc.arg(worker_instance_id)
       AND worker_instances.id = sqlc.arg(worker_instance_id)
       AND run_waits.owner_run_id = sqlc.arg(run_id)
       AND run_waits.owner_run_lease_id = sqlc.arg(run_lease_id)
       AND run_waits.owner_worker_instance_id = sqlc.arg(worker_instance_id)
       AND run_leases.status IN ('leased', 'running')
       AND run_leases.lease_expires_at > now()
       AND run_waits.workspace_version_id IS NOT NULL
       AND workspaces.current_version_id = run_waits.workspace_version_id
       AND workspace_mounts.dirty_generation = 0
       AND workspace_mounts.state = 'mounted'
       AND NOT EXISTS (
           SELECT 1
             FROM workspace_leases AS other_workspace_leases
            WHERE other_workspace_leases.org_id = workspace_mounts.org_id
              AND other_workspace_leases.project_id = workspace_mounts.project_id
              AND other_workspace_leases.environment_id = workspace_mounts.environment_id
              AND other_workspace_leases.workspace_id = workspace_mounts.workspace_id
              AND other_workspace_leases.workspace_mount_id = workspace_mounts.id
              AND other_workspace_leases.id <> workspace_leases.id
              AND other_workspace_leases.state IN ('active', 'releasing')
              AND other_workspace_leases.expires_at > now()
       )
	     FOR UPDATE OF run_waits, runs, runtime_checkpoints, worker_commands, workspace_leases, workspace_mounts
),
created_checkpoint AS (
    UPDATE runtime_checkpoints
       SET state = 'ready',
           base_workspace_version_id = wait_scope.workspace_version_id,
           runtime_backend = sqlc.arg(runtime_backend),
           runtime_id = sqlc.arg(runtime_id),
           runtime_arch = sqlc.arg(runtime_arch),
           runtime_abi = sqlc.arg(runtime_abi),
           kernel_digest = sqlc.arg(kernel_digest),
           initramfs_digest = sqlc.arg(initramfs_digest),
           rootfs_digest = sqlc.arg(rootfs_digest),
           runtime_config_digest = sqlc.arg(runtime_config_digest),
           source_workspace_lease_id = wait_scope.workspace_lease_id,
           workspace_mount_id = wait_scope.workspace_mount_id,
           owner_runtime_instance_id = wait_scope.owner_runtime_instance_id,
           owner_runtime_epoch = wait_scope.owner_runtime_epoch,
           owner_run_id = wait_scope.run_id,
           owner_run_wait_id = wait_scope.id,
           owner_run_lease_id = wait_scope.owner_run_lease_id,
           owner_worker_instance_id = sqlc.arg(worker_instance_id),
           source_worker_instance_id = sqlc.arg(worker_instance_id),
           runtime_substrate_artifact_id = sqlc.narg(runtime_substrate_artifact_id)::uuid,
           runtime_vcpus = sqlc.narg(runtime_vcpus)::int,
           runtime_memory_mib = sqlc.narg(runtime_memory_mib)::int,
           runtime_scratch_disk_mib = sqlc.narg(runtime_scratch_disk_mib)::int,
           cni_profile = sqlc.arg(cni_profile),
           substrate_digest = sqlc.narg(substrate_digest)::text,
           manifest = sqlc.arg(manifest)::jsonb,
           expires_at = CASE
             WHEN wait_scope.timeout_at IS NULL THEN NULL::timestamptz
             ELSE wait_scope.timeout_at + interval '1 day'
           END,
           ready_at = now()
      FROM wait_scope
     WHERE runtime_checkpoints.org_id = wait_scope.org_id
       AND runtime_checkpoints.project_id = wait_scope.project_id
       AND runtime_checkpoints.environment_id = wait_scope.environment_id
       AND runtime_checkpoints.run_id = wait_scope.run_id
       AND runtime_checkpoints.id = sqlc.arg(runtime_checkpoint_id)
       AND runtime_checkpoints.state = 'creating'
       AND runtime_checkpoints.owner_runtime_instance_id = wait_scope.owner_runtime_instance_id
       AND runtime_checkpoints.owner_runtime_epoch = wait_scope.owner_runtime_epoch
       AND runtime_checkpoints.owner_run_id = wait_scope.run_id
       AND runtime_checkpoints.owner_run_wait_id = wait_scope.id
       AND runtime_checkpoints.owner_run_lease_id = wait_scope.owner_run_lease_id
       AND runtime_checkpoints.owner_worker_instance_id = sqlc.arg(worker_instance_id)
       AND runtime_checkpoints.source_worker_instance_id = sqlc.arg(worker_instance_id)
       AND (
           sqlc.narg(runtime_substrate_artifact_id)::uuid IS NULL
           OR EXISTS (
               SELECT 1
                 FROM runtime_substrate_artifacts
                WHERE runtime_substrate_artifacts.org_id = runtime_checkpoints.org_id
                  AND runtime_substrate_artifacts.worker_group_id = runtime_checkpoints.worker_group_id
                  AND runtime_substrate_artifacts.project_id = runtime_checkpoints.project_id
                  AND runtime_substrate_artifacts.environment_id = runtime_checkpoints.environment_id
                  AND runtime_substrate_artifacts.id = sqlc.narg(runtime_substrate_artifact_id)::uuid
                  AND runtime_substrate_artifacts.retired_at IS NULL
           )
       )
    RETURNING runtime_checkpoints.*
),
updated_wait AS (
    UPDATE run_waits
       SET state = 'checkpointed_waiting',
           runtime_checkpoint_id = created_checkpoint.id,
           workspace_version_id = created_checkpoint.base_workspace_version_id,
           active_elapsed_ms_at_park = LEAST(
               wait_scope.max_active_duration_ms,
               wait_scope.active_elapsed_ms
               + CASE
                   WHEN wait_scope.active_started_at IS NULL THEN 0
                   ELSE GREATEST((EXTRACT(EPOCH FROM (now() - wait_scope.active_started_at)) * 1000)::bigint, 0)
                 END
           ),
           updated_at = now()
      FROM wait_scope, created_checkpoint
     WHERE run_waits.org_id = wait_scope.org_id
       AND run_waits.id = wait_scope.id
       AND run_waits.state = 'checkpointing'
    RETURNING run_waits.*
),
released_workspace_lease AS (
    UPDATE workspace_leases
       SET state = 'released',
           released_at = now(),
           updated_at = now()
      FROM wait_scope, updated_wait
     WHERE workspace_leases.org_id = wait_scope.org_id
       AND workspace_leases.id = wait_scope.workspace_lease_id
       AND workspace_leases.state = 'active'
    RETURNING workspace_leases.id
),
unmounted_mount AS (
    UPDATE workspace_mounts
       SET state = 'unmounted',
           unmounted_at = coalesce(workspace_mounts.unmounted_at, now()),
           updated_at = now()
      FROM wait_scope, released_workspace_lease
     WHERE workspace_mounts.org_id = wait_scope.org_id
       AND workspace_mounts.project_id = wait_scope.project_id
       AND workspace_mounts.environment_id = wait_scope.environment_id
       AND workspace_mounts.workspace_id = wait_scope.workspace_id
       AND workspace_mounts.id = wait_scope.workspace_mount_id
       AND workspace_mounts.state = 'mounted'
    RETURNING workspace_mounts.id,
              workspace_mounts.org_id,
              workspace_mounts.runtime_instance_id
),
closed_runtime_instance AS (
    UPDATE runtime_instances
       SET state = 'closed',
           closed_at = COALESCE(runtime_instances.closed_at, now()),
           expires_at = NULL,
           owner_run_id = NULL,
           owner_run_lease_id = NULL,
           owner_run_wait_id = NULL,
           owner_workspace_id = NULL,
           owner_workspace_version_id = NULL,
           owner_run_state_version = NULL,
           updated_at = now()
      FROM unmounted_mount
     WHERE runtime_instances.org_id = unmounted_mount.org_id
       AND runtime_instances.id = unmounted_mount.runtime_instance_id
       AND runtime_instances.worker_instance_id = sqlc.arg(worker_instance_id)
       AND runtime_instances.state = 'checkpointing'
    RETURNING runtime_instances.id
),
detached_run_lease AS (
    UPDATE run_leases
       SET status = 'detached',
           active_duration_ms = updated_wait.active_elapsed_ms_at_park,
           renewed_at = now()
      FROM wait_scope, updated_wait
     WHERE run_leases.org_id = wait_scope.org_id
       AND run_leases.run_id = wait_scope.run_id
       AND run_leases.id = wait_scope.current_run_lease_id
       AND run_leases.worker_instance_id = sqlc.arg(worker_instance_id)
       AND run_leases.status IN ('leased', 'running')
    RETURNING run_leases.id
),
parked_run AS (
    UPDATE runs
       SET status = 'waiting',
           execution_status = 'waiting',
           current_run_lease_id = NULL,
           latest_runtime_checkpoint_id = created_checkpoint.id,
           active_elapsed_ms = updated_wait.active_elapsed_ms_at_park,
           active_started_at = NULL,
           state_version = runs.state_version + 1,
           updated_at = now()
      FROM wait_scope, created_checkpoint, updated_wait
     WHERE runs.org_id = wait_scope.org_id
       AND runs.id = wait_scope.run_id
       AND runs.status = 'running'
    RETURNING runs.id
),
parked_attempt AS (
    UPDATE run_attempts
       SET status = 'waiting',
           updated_at = now()
      FROM wait_scope, parked_run
     WHERE run_attempts.org_id = wait_scope.org_id
       AND run_attempts.run_id = wait_scope.run_id
       AND run_attempts.id = (
           SELECT runs.current_attempt_id
             FROM runs
            WHERE runs.org_id = wait_scope.org_id
              AND runs.id = wait_scope.run_id
       )
       AND run_attempts.status = 'running'
    RETURNING run_attempts.run_id
),
parked_queue AS (
    UPDATE run_queue_items
       SET status = 'parked',
           dispatch_message_id = NULL,
           reserved_by_worker_instance_id = NULL,
           reservation_expires_at = NULL,
           updated_at = now()
      FROM wait_scope, parked_run
     WHERE run_queue_items.org_id = wait_scope.org_id
       AND run_queue_items.run_id = wait_scope.run_id
       AND run_queue_items.status IN ('reserved', 'published')
    RETURNING run_queue_items.run_id
)
SELECT created_checkpoint.*
  FROM created_checkpoint
  JOIN updated_wait ON true
  JOIN released_workspace_lease ON true
  JOIN unmounted_mount ON true
  JOIN closed_runtime_instance ON true
  JOIN detached_run_lease ON true
  JOIN parked_run ON true
  JOIN parked_attempt ON true;

-- name: CreateRuntimeCheckpointArtifact :one
INSERT INTO runtime_checkpoint_artifacts (
    org_id,
    worker_group_id,
    project_id,
    environment_id,
    run_id,
    runtime_checkpoint_id,
    role,
    ordinal,
    artifact_id,
    size_bytes,
    media_type,
    digest,
    encrypt_duration_ms,
    store_duration_ms
)
SELECT runtime_checkpoints.org_id,
       runtime_checkpoints.worker_group_id,
       runtime_checkpoints.project_id,
       runtime_checkpoints.environment_id,
       runtime_checkpoints.run_id,
       runtime_checkpoints.id,
       sqlc.arg(role)::runtime_checkpoint_artifact_role,
       sqlc.arg(ordinal)::int,
       artifacts.id,
       artifacts.size_bytes,
       artifacts.media_type,
       artifacts.digest,
       sqlc.arg(encrypt_duration_ms)::bigint,
       sqlc.arg(store_duration_ms)::bigint
  FROM runtime_checkpoints
  JOIN artifacts ON artifacts.org_id = runtime_checkpoints.org_id
                AND artifacts.project_id = runtime_checkpoints.project_id
                AND artifacts.environment_id = runtime_checkpoints.environment_id
                AND artifacts.id = sqlc.arg(artifact_id)
                AND artifacts.digest = sqlc.arg(digest)
 WHERE runtime_checkpoints.org_id = sqlc.arg(org_id)
   AND runtime_checkpoints.project_id = sqlc.arg(project_id)
   AND runtime_checkpoints.environment_id = sqlc.arg(environment_id)
   AND runtime_checkpoints.run_id = sqlc.arg(run_id)
   AND runtime_checkpoints.id = sqlc.arg(runtime_checkpoint_id)
RETURNING *;

-- name: FailRuntimeCheckpointAttempt :one
WITH wait_scope AS MATERIALIZED (
    SELECT run_waits.*
      FROM run_waits
      JOIN runtime_checkpoints
        ON runtime_checkpoints.org_id = run_waits.org_id
       AND runtime_checkpoints.worker_group_id = run_waits.worker_group_id
       AND runtime_checkpoints.project_id = run_waits.project_id
       AND runtime_checkpoints.environment_id = run_waits.environment_id
       AND runtime_checkpoints.run_id = run_waits.run_id
       AND runtime_checkpoints.id = run_waits.runtime_checkpoint_id
       AND runtime_checkpoints.id = sqlc.arg(runtime_checkpoint_id)
       AND runtime_checkpoints.state = 'creating'
       AND runtime_checkpoints.owner_runtime_instance_id = run_waits.owner_runtime_instance_id
       AND runtime_checkpoints.owner_runtime_epoch = run_waits.owner_runtime_epoch
       AND runtime_checkpoints.owner_run_id = run_waits.run_id
       AND runtime_checkpoints.owner_run_wait_id = run_waits.id
       AND runtime_checkpoints.owner_run_lease_id = run_waits.owner_run_lease_id
       AND runtime_checkpoints.owner_worker_instance_id = run_waits.owner_worker_instance_id
       AND runtime_checkpoints.source_worker_instance_id = run_waits.owner_worker_instance_id
      JOIN runtime_instances
        ON runtime_instances.org_id = run_waits.org_id
       AND runtime_instances.worker_group_id = run_waits.worker_group_id
       AND runtime_instances.id = run_waits.owner_runtime_instance_id
       AND runtime_instances.worker_instance_id = run_waits.owner_worker_instance_id
       AND runtime_instances.runtime_epoch = run_waits.owner_runtime_epoch
       AND runtime_instances.owner_run_id = run_waits.run_id
       AND runtime_instances.owner_run_lease_id = run_waits.owner_run_lease_id
       AND runtime_instances.owner_run_wait_id = run_waits.id
       AND runtime_instances.owner_run_state_version = run_waits.owner_run_state_version
       AND runtime_instances.state = 'checkpointing'
      JOIN worker_commands
        ON worker_commands.org_id = run_waits.org_id
       AND worker_commands.worker_group_id = run_waits.worker_group_id
       AND worker_commands.project_id = run_waits.project_id
       AND worker_commands.environment_id = run_waits.environment_id
       AND worker_commands.run_id = run_waits.run_id
       AND worker_commands.run_wait_id = run_waits.id
       AND worker_commands.run_lease_id = run_waits.owner_run_lease_id
       AND worker_commands.worker_instance_id = run_waits.owner_worker_instance_id
       AND worker_commands.runtime_instance_id = run_waits.owner_runtime_instance_id
       AND worker_commands.runtime_epoch = run_waits.owner_runtime_epoch
       AND worker_commands.run_state_version = run_waits.owner_run_state_version
       AND worker_commands.kind = 'runtime_checkpoint_wait'
       AND worker_commands.id = sqlc.arg(worker_command_id)
       AND worker_commands.accepted_at IS NOT NULL
       AND worker_commands.acknowledged_at IS NULL
      JOIN worker_instances
        ON worker_instances.id = run_waits.owner_worker_instance_id
       AND worker_instances.worker_group_id = run_waits.worker_group_id
      JOIN (
    SELECT placement_project.org_id,
           placement_project.id AS project_id,
           target_environment.id AS environment_id,
           placement_worker_group.region_id AS region_id,
           placement_worker_group.id AS worker_group_id,
           placement_worker_group.state AS worker_group_state
      FROM projects AS placement_project
      JOIN environments AS target_environment
        ON target_environment.org_id = placement_project.org_id
       AND target_environment.project_id = placement_project.id
      JOIN worker_groups AS placement_worker_group
        ON true
) AS project_worker_group_placement
        ON project_worker_group_placement.org_id = run_waits.org_id
       AND project_worker_group_placement.project_id = run_waits.project_id
       AND project_worker_group_placement.environment_id = run_waits.environment_id
       AND project_worker_group_placement.worker_group_id = run_waits.worker_group_id
       AND project_worker_group_placement.worker_group_state IN ('active', 'draining')
      JOIN worker_groups ON worker_groups.id = project_worker_group_placement.worker_group_id
                AND worker_groups.state = 'active'
     WHERE run_waits.org_id = sqlc.arg(org_id)
       AND run_waits.project_id = sqlc.arg(project_id)
       AND run_waits.environment_id = sqlc.arg(environment_id)
       AND run_waits.run_id = sqlc.arg(run_id)
       AND run_waits.id = sqlc.arg(run_wait_id)
       AND run_waits.owner_run_lease_id = sqlc.arg(run_lease_id)
       AND run_waits.owner_worker_instance_id = sqlc.arg(worker_instance_id)
       AND run_waits.state = 'checkpointing'
       AND worker_instances.id = sqlc.arg(worker_instance_id)
     FOR UPDATE OF run_waits, runtime_checkpoints, runtime_instances, worker_commands
),
invalidated_checkpoint AS (
    UPDATE runtime_checkpoints
       SET state = 'invalid',
           error_message = COALESCE(NULLIF(sqlc.arg(error_message)::text, ''), 'runtime checkpoint failed'),
           invalidated_at = COALESCE(runtime_checkpoints.invalidated_at, now())
      FROM wait_scope
     WHERE runtime_checkpoints.org_id = wait_scope.org_id
       AND runtime_checkpoints.project_id = wait_scope.project_id
       AND runtime_checkpoints.environment_id = wait_scope.environment_id
       AND runtime_checkpoints.run_id = wait_scope.run_id
       AND runtime_checkpoints.id = sqlc.arg(runtime_checkpoint_id)
       AND runtime_checkpoints.state = 'creating'
    RETURNING runtime_checkpoints.id
),
waiting_runtime_instance AS (
    UPDATE runtime_instances
       SET state = 'waiting_hot',
           waiting_at = now(),
           updated_at = now()
      FROM wait_scope, invalidated_checkpoint
     WHERE runtime_instances.org_id = wait_scope.org_id
       AND runtime_instances.id = wait_scope.owner_runtime_instance_id
       AND runtime_instances.worker_instance_id = wait_scope.owner_worker_instance_id
       AND runtime_instances.runtime_epoch = wait_scope.owner_runtime_epoch
       AND runtime_instances.owner_run_id = wait_scope.run_id
       AND runtime_instances.owner_run_lease_id = wait_scope.owner_run_lease_id
       AND runtime_instances.owner_run_wait_id = wait_scope.id
       AND runtime_instances.owner_run_state_version = wait_scope.owner_run_state_version
       AND runtime_instances.state = 'checkpointing'
    RETURNING runtime_instances.id
),
updated_wait AS (
    UPDATE run_waits
       SET state = 'live_waiting',
           runtime_checkpoint_id = NULL,
           runtime_checkpoint_started_at = NULL,
           runtime_checkpoint_due_at = now() + interval '5 seconds',
           updated_at = now()
      FROM wait_scope, invalidated_checkpoint, waiting_runtime_instance
     WHERE run_waits.org_id = wait_scope.org_id
       AND run_waits.project_id = wait_scope.project_id
       AND run_waits.environment_id = wait_scope.environment_id
       AND run_waits.run_id = wait_scope.run_id
       AND run_waits.id = wait_scope.id
       AND run_waits.state = 'checkpointing'
    RETURNING run_waits.*
)
SELECT *
  FROM updated_wait;
