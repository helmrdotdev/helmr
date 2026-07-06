-- name: CreatePreparedRuntimeInstanceForWorkspaceMountSource :one
WITH source_mount AS MATERIALIZED (
    SELECT workspace_mounts.*,
           image_artifact.digest AS image_artifact_digest,
           coalesce((deployment_sandboxes.resource_floor->>'milli_cpu')::integer, 1000) AS sandbox_floor_cpu_millis,
           coalesce((deployment_sandboxes.resource_floor->>'memory_mib')::integer, 1024) AS sandbox_floor_memory_mib,
           deployment_sandboxes.disk_floor_mib AS sandbox_floor_disk_mib,
           1::integer AS sandbox_floor_execution_slots
      FROM workspace_mounts
      JOIN deployment_sandboxes
        ON deployment_sandboxes.org_id = workspace_mounts.org_id
       AND deployment_sandboxes.project_id = workspace_mounts.project_id
       AND deployment_sandboxes.environment_id = workspace_mounts.environment_id
       AND deployment_sandboxes.id = workspace_mounts.deployment_sandbox_id
      JOIN artifacts AS image_artifact
        ON image_artifact.org_id = workspace_mounts.org_id
       AND image_artifact.project_id = workspace_mounts.project_id
       AND image_artifact.environment_id = workspace_mounts.environment_id
       AND image_artifact.id = workspace_mounts.image_artifact_id
       AND image_artifact.kind = 'sandbox_image'
       AND image_artifact.media_type = 'application/vnd.helmr.sandbox-image.v0.oci-tar'
      JOIN worker_groups
        ON worker_groups.id = workspace_mounts.worker_group_id
       AND worker_groups.state IN ('active', 'draining')
     WHERE workspace_mounts.id = sqlc.arg(workspace_mount_id)
       AND workspace_mounts.state IN ('mounting', 'mounted')
       AND workspace_mounts.runtime_instance_id IS NULL
       AND workspace_mounts.guestd_channel_token_hash = sqlc.arg(guestd_channel_token_hash)
       AND workspace_mounts.guestd_channel_token_expires_at > now()
),
worker_scope AS MATERIALIZED (
    SELECT worker_instances.*
      FROM worker_instances
     WHERE worker_instances.id = sqlc.arg(worker_instance_id)
       AND worker_instances.status = 'active'
     FOR UPDATE OF worker_instances
),
active_run_usage AS MATERIALIZED (
    SELECT COALESCE(sum(run_runtime_requirements.requested_milli_cpu), 0)::bigint AS used_milli_cpu,
           COALESCE(sum(run_runtime_requirements.requested_memory_mib), 0)::bigint AS used_memory_mib,
           COALESCE(sum(run_runtime_requirements.requested_disk_mib), 0)::bigint AS used_disk_mib,
           COALESCE(sum(run_runtime_requirements.requested_execution_slots), 0)::int AS used_slots
      FROM worker_scope
      JOIN run_leases ON run_leases.worker_instance_id = worker_scope.id
                     AND run_leases.status IN ('leased', 'running')
      JOIN runs ON runs.org_id = run_leases.org_id
               AND runs.id = run_leases.run_id
               AND runs.workspace_mount_id IS NULL
      JOIN run_runtime_requirements ON run_runtime_requirements.org_id = run_leases.org_id
                                   AND run_runtime_requirements.run_id = run_leases.run_id
),
active_runtime_instance_usage AS MATERIALIZED (
    SELECT COALESCE(sum(runtime_instances.reserved_cpu_millis), 0)::bigint AS used_milli_cpu,
           COALESCE(sum(runtime_instances.reserved_memory_mib), 0)::bigint AS used_memory_mib,
           COALESCE(sum(runtime_instances.reserved_disk_mib), 0)::bigint AS used_disk_mib,
           COALESCE(sum(runtime_instances.reserved_execution_slots), 0)::int AS used_slots
      FROM worker_scope
      JOIN runtime_instances
        ON runtime_instances.worker_instance_id = worker_scope.id
       AND runtime_instances.state IN ('preparing', 'ready', 'binding', 'running', 'waiting_hot', 'checkpointing', 'stopping')
       AND (
           runtime_instances.expires_at IS NULL
           OR runtime_instances.expires_at > now()
       )
),
candidate AS MATERIALIZED (
    SELECT source_mount.*
      FROM source_mount, worker_scope, active_run_usage, active_runtime_instance_usage
     WHERE source_mount.worker_group_id = worker_scope.worker_group_id
       AND source_mount.sandbox_floor_cpu_millis <= GREATEST(worker_scope.available_milli_cpu - active_run_usage.used_milli_cpu - active_runtime_instance_usage.used_milli_cpu, 0)
       AND source_mount.sandbox_floor_memory_mib <= GREATEST(worker_scope.available_memory_mib - active_run_usage.used_memory_mib - active_runtime_instance_usage.used_memory_mib, 0)
       AND source_mount.sandbox_floor_disk_mib <= GREATEST(worker_scope.available_disk_mib - active_run_usage.used_disk_mib - active_runtime_instance_usage.used_disk_mib, 0)
       AND source_mount.sandbox_floor_execution_slots <= GREATEST(worker_scope.available_execution_slots - active_run_usage.used_slots - active_runtime_instance_usage.used_slots, 0)
)
INSERT INTO runtime_instances (
    id,
    org_id,
    worker_group_id,
    project_id,
    environment_id,
    worker_instance_id,
    runtime_release_id,
    deployment_sandbox_id,
    runtime_key_hash,
    runtime_key,
    sandbox_fingerprint,
    rootfs_digest,
    image_digest,
    image_format,
    sandbox_image_artifact_id,
    sandbox_image_artifact_digest,
    sandbox_image_artifact_format,
    workspace_mount_path,
    runtime_abi,
    guestd_abi,
    adapter_abi,
    network_policy,
    reserved_cpu_millis,
    reserved_memory_mib,
    reserved_disk_mib,
    reserved_execution_slots,
    instance_token,
    last_heartbeat_at,
    expires_at
)
SELECT sqlc.arg(id),
       candidate.org_id,
       candidate.worker_group_id,
       candidate.project_id,
       candidate.environment_id,
       sqlc.arg(worker_instance_id),
       sqlc.arg(runtime_release_id),
       candidate.deployment_sandbox_id,
       sqlc.arg(runtime_key_hash),
       COALESCE(sqlc.arg(runtime_key)::jsonb, '{}'::jsonb),
       candidate.sandbox_fingerprint,
       candidate.rootfs_digest,
       candidate.image_digest,
       candidate.image_format,
       candidate.image_artifact_id,
       candidate.image_artifact_digest,
       candidate.image_artifact_format,
       candidate.workspace_mount_path,
       candidate.runtime_abi,
       candidate.guestd_abi,
       candidate.adapter_abi,
       COALESCE(sqlc.arg(network_policy)::jsonb, '{}'::jsonb),
       candidate.sandbox_floor_cpu_millis,
       candidate.sandbox_floor_memory_mib,
       candidate.sandbox_floor_disk_mib,
       candidate.sandbox_floor_execution_slots,
       sqlc.arg(instance_token),
       now(),
       sqlc.arg(expires_at)
  FROM candidate
RETURNING runtime_instances.*;

-- name: CreateRuntimeInstanceForDeploymentSandbox :one
WITH worker_scope AS MATERIALIZED (
    SELECT worker_instances.*
      FROM worker_instances
     WHERE worker_instances.id = sqlc.arg(worker_instance_id)
       AND worker_instances.status = 'active'
       AND worker_instances.runtime_id = sqlc.arg(runtime_release_id)
       AND worker_instances.rootfs_digest = sqlc.arg(rootfs_digest)
       AND worker_instances.runtime_abi = sqlc.arg(runtime_abi)
     FOR UPDATE OF worker_instances
),
source_sandbox AS MATERIALIZED (
    SELECT deployment_sandboxes.*,
           worker_scope.worker_group_id,
           image_artifact.digest AS image_artifact_digest,
           image_artifact.media_type AS image_artifact_media_type,
           image_artifact.size_bytes AS image_artifact_size_bytes,
           coalesce((deployment_sandboxes.resource_floor->>'milli_cpu')::integer, 1000) AS requested_cpu_millis,
           coalesce((deployment_sandboxes.resource_floor->>'memory_mib')::integer, 1024) AS requested_memory_mib,
           deployment_sandboxes.disk_floor_mib AS requested_disk_mib,
           1::integer AS requested_execution_slots
      FROM deployment_sandboxes
      JOIN deployments
        ON deployments.org_id = deployment_sandboxes.org_id
       AND deployments.project_id = deployment_sandboxes.project_id
       AND deployments.environment_id = deployment_sandboxes.environment_id
       AND deployments.id = deployment_sandboxes.deployment_id
      JOIN environments
        ON environments.org_id = deployment_sandboxes.org_id
       AND environments.project_id = deployment_sandboxes.project_id
       AND environments.id = deployment_sandboxes.environment_id
       AND environments.current_deployment_id = deployment_sandboxes.deployment_id
      JOIN artifacts AS image_artifact
        ON image_artifact.org_id = deployment_sandboxes.org_id
       AND image_artifact.project_id = deployment_sandboxes.project_id
       AND image_artifact.environment_id = deployment_sandboxes.environment_id
       AND image_artifact.id = deployment_sandboxes.image_artifact_id
       AND image_artifact.digest = deployment_sandboxes.image_digest
       AND image_artifact.kind = 'sandbox_image'
       AND image_artifact.media_type = 'application/vnd.helmr.sandbox-image.v0.oci-tar'
      JOIN worker_scope ON true
      JOIN projects
        ON projects.org_id = deployment_sandboxes.org_id
       AND projects.id = deployment_sandboxes.project_id
      JOIN worker_groups
        ON worker_groups.id = worker_scope.worker_group_id
       AND worker_groups.region_id = projects.default_region_id
       AND worker_groups.state = 'active'
       AND worker_groups.health_state IN ('healthy', 'degraded')
       AND worker_groups.routing_fresh_until > now()
     WHERE deployment_sandboxes.id = sqlc.arg(deployment_sandbox_id)
       AND deployment_sandboxes.rootfs_digest = sqlc.arg(rootfs_digest)
       AND deployment_sandboxes.runtime_abi = sqlc.arg(runtime_abi)
),
active_run_usage AS MATERIALIZED (
    SELECT COALESCE(sum(run_runtime_requirements.requested_milli_cpu), 0)::bigint AS used_milli_cpu,
           COALESCE(sum(run_runtime_requirements.requested_memory_mib), 0)::bigint AS used_memory_mib,
           COALESCE(sum(run_runtime_requirements.requested_disk_mib), 0)::bigint AS used_disk_mib,
           COALESCE(sum(run_runtime_requirements.requested_execution_slots), 0)::int AS used_slots
      FROM worker_scope
      JOIN run_leases ON run_leases.worker_instance_id = worker_scope.id
                     AND run_leases.status IN ('leased', 'running')
      JOIN runs ON runs.org_id = run_leases.org_id
               AND runs.id = run_leases.run_id
               AND runs.workspace_mount_id IS NULL
      JOIN run_runtime_requirements ON run_runtime_requirements.org_id = run_leases.org_id
                                   AND run_runtime_requirements.run_id = run_leases.run_id
),
active_runtime_instance_usage AS MATERIALIZED (
    SELECT COALESCE(sum(runtime_instances.reserved_cpu_millis), 0)::bigint AS used_milli_cpu,
           COALESCE(sum(runtime_instances.reserved_memory_mib), 0)::bigint AS used_memory_mib,
           COALESCE(sum(runtime_instances.reserved_disk_mib), 0)::bigint AS used_disk_mib,
           COALESCE(sum(runtime_instances.reserved_execution_slots), 0)::int AS used_slots
      FROM worker_scope
      JOIN runtime_instances
        ON runtime_instances.worker_instance_id = worker_scope.id
       AND runtime_instances.state IN ('preparing', 'ready', 'binding', 'running', 'waiting_hot', 'checkpointing', 'stopping')
       AND (
           runtime_instances.expires_at IS NULL
           OR runtime_instances.expires_at > now()
       )
),
candidate AS MATERIALIZED (
    SELECT source_sandbox.*
      FROM source_sandbox, worker_scope, active_run_usage, active_runtime_instance_usage
     WHERE source_sandbox.requested_cpu_millis <= GREATEST(worker_scope.available_milli_cpu - active_run_usage.used_milli_cpu - active_runtime_instance_usage.used_milli_cpu, 0)
       AND source_sandbox.requested_memory_mib <= GREATEST(worker_scope.available_memory_mib - active_run_usage.used_memory_mib - active_runtime_instance_usage.used_memory_mib, 0)
       AND source_sandbox.requested_disk_mib <= GREATEST(worker_scope.available_disk_mib - active_run_usage.used_disk_mib - active_runtime_instance_usage.used_disk_mib, 0)
       AND source_sandbox.requested_execution_slots <= GREATEST(worker_scope.available_execution_slots - active_run_usage.used_slots - active_runtime_instance_usage.used_slots, 0)
),
inserted AS (
    INSERT INTO runtime_instances (
        id,
        org_id,
        worker_group_id,
        project_id,
        environment_id,
        worker_instance_id,
        runtime_release_id,
        deployment_sandbox_id,
        runtime_key_hash,
        runtime_key,
        sandbox_fingerprint,
        rootfs_digest,
        image_digest,
        image_format,
        sandbox_image_artifact_id,
        sandbox_image_artifact_digest,
        sandbox_image_artifact_format,
        workspace_mount_path,
        runtime_abi,
        guestd_abi,
        adapter_abi,
        network_policy,
        reserved_cpu_millis,
        reserved_memory_mib,
        reserved_disk_mib,
        reserved_execution_slots,
        instance_token,
        last_heartbeat_at,
        expires_at
    )
    SELECT sqlc.arg(id),
           candidate.org_id,
           candidate.worker_group_id,
           candidate.project_id,
           candidate.environment_id,
           sqlc.arg(worker_instance_id),
           sqlc.arg(runtime_release_id),
           candidate.id,
           sqlc.arg(runtime_key_hash),
           COALESCE(sqlc.arg(runtime_key)::jsonb, '{}'::jsonb),
           candidate.fingerprint,
           candidate.rootfs_digest,
           candidate.image_digest,
           candidate.image_format,
           candidate.image_artifact_id,
           candidate.image_artifact_digest,
           candidate.image_artifact_format,
           candidate.workspace_mount_path,
           candidate.runtime_abi,
           candidate.guestd_abi,
           candidate.adapter_abi,
           candidate.network_policy,
           candidate.requested_cpu_millis,
           candidate.requested_memory_mib,
           candidate.requested_disk_mib,
           candidate.requested_execution_slots,
           sqlc.arg(instance_token),
           now(),
           sqlc.arg(expires_at)
      FROM candidate
    RETURNING *
)
SELECT inserted.*,
       artifacts.media_type AS sandbox_image_artifact_media_type,
       artifacts.size_bytes AS sandbox_image_artifact_size_bytes
  FROM inserted
  JOIN artifacts
    ON artifacts.org_id = inserted.org_id
   AND artifacts.project_id = inserted.project_id
   AND artifacts.environment_id = inserted.environment_id
   AND artifacts.id = inserted.sandbox_image_artifact_id
   AND artifacts.digest = inserted.sandbox_image_artifact_digest;

-- name: RenewRuntimeInstance :one
UPDATE runtime_instances
   SET last_heartbeat_at = now(),
       expires_at = sqlc.arg(expires_at),
       updated_at = now()
  FROM worker_instances
  JOIN worker_groups
    ON worker_groups.id = worker_instances.worker_group_id
   AND worker_groups.state IN ('active', 'draining')
 WHERE runtime_instances.id = sqlc.arg(id)
   AND runtime_instances.worker_instance_id = sqlc.arg(worker_instance_id)
   AND worker_instances.id = runtime_instances.worker_instance_id
   AND worker_instances.worker_group_id = runtime_instances.worker_group_id
   AND runtime_instances.instance_token = sqlc.arg(instance_token)
   AND runtime_instances.state IN ('preparing', 'ready')
   AND (
       runtime_instances.expires_at IS NULL
       OR runtime_instances.expires_at > now()
   )
   AND (
       sqlc.narg(runtime_substrate_artifact_id)::uuid IS NULL
       OR EXISTS (
           SELECT 1
             FROM runtime_substrate_artifacts
             JOIN artifacts
               ON artifacts.org_id = runtime_substrate_artifacts.org_id
              AND artifacts.project_id = runtime_substrate_artifacts.project_id
              AND artifacts.environment_id = runtime_substrate_artifacts.environment_id
              AND artifacts.id = runtime_substrate_artifacts.artifact_id
             JOIN deployment_sandboxes
               ON deployment_sandboxes.org_id = runtime_substrate_artifacts.org_id
              AND deployment_sandboxes.project_id = runtime_substrate_artifacts.project_id
              AND deployment_sandboxes.environment_id = runtime_substrate_artifacts.environment_id
              AND deployment_sandboxes.id = runtime_substrate_artifacts.deployment_sandbox_id
            WHERE runtime_substrate_artifacts.org_id = runtime_instances.org_id
              AND runtime_substrate_artifacts.worker_group_id = runtime_instances.worker_group_id
              AND runtime_substrate_artifacts.project_id = runtime_instances.project_id
              AND runtime_substrate_artifacts.environment_id = runtime_instances.environment_id
              AND runtime_substrate_artifacts.deployment_sandbox_id = runtime_instances.deployment_sandbox_id
              AND runtime_substrate_artifacts.id = sqlc.narg(runtime_substrate_artifact_id)::uuid
       )
   )
RETURNING runtime_instances.*;

-- name: MarkRuntimeInstanceReady :one
UPDATE runtime_instances
   SET state = 'ready',
       prepared_at = coalesce(prepared_at, now()),
       runtime_substrate_artifact_id = COALESCE(sqlc.narg(runtime_substrate_artifact_id), runtime_instances.runtime_substrate_artifact_id),
       last_heartbeat_at = now(),
       expires_at = sqlc.arg(expires_at),
       updated_at = now()
  FROM worker_instances
  JOIN worker_groups
    ON worker_groups.id = worker_instances.worker_group_id
   AND worker_groups.state IN ('active', 'draining')
 WHERE runtime_instances.id = sqlc.arg(id)
   AND runtime_instances.worker_instance_id = sqlc.arg(worker_instance_id)
   AND worker_instances.id = runtime_instances.worker_instance_id
   AND worker_instances.worker_group_id = runtime_instances.worker_group_id
   AND runtime_instances.instance_token = sqlc.arg(instance_token)
   AND runtime_instances.state = 'preparing'
   AND (
       runtime_instances.expires_at IS NULL
       OR runtime_instances.expires_at > now()
   )
   AND (
       sqlc.narg(runtime_substrate_artifact_id)::uuid IS NULL
       OR EXISTS (
           SELECT 1
             FROM runtime_substrate_artifacts
             JOIN artifacts
               ON artifacts.org_id = runtime_substrate_artifacts.org_id
              AND artifacts.project_id = runtime_substrate_artifacts.project_id
              AND artifacts.environment_id = runtime_substrate_artifacts.environment_id
              AND artifacts.id = runtime_substrate_artifacts.artifact_id
             JOIN deployment_sandboxes
               ON deployment_sandboxes.org_id = runtime_substrate_artifacts.org_id
              AND deployment_sandboxes.project_id = runtime_substrate_artifacts.project_id
              AND deployment_sandboxes.environment_id = runtime_substrate_artifacts.environment_id
              AND deployment_sandboxes.id = runtime_substrate_artifacts.deployment_sandbox_id
            WHERE runtime_substrate_artifacts.org_id = runtime_instances.org_id
              AND runtime_substrate_artifacts.worker_group_id = runtime_instances.worker_group_id
              AND runtime_substrate_artifacts.project_id = runtime_instances.project_id
              AND runtime_substrate_artifacts.environment_id = runtime_instances.environment_id
              AND runtime_substrate_artifacts.deployment_sandbox_id = runtime_instances.deployment_sandbox_id
              AND runtime_substrate_artifacts.id = sqlc.narg(runtime_substrate_artifact_id)::uuid
       )
   )
RETURNING runtime_instances.*;

-- name: MarkRuntimeInstanceClosed :one
WITH target AS MATERIALIZED (
    SELECT runtime_instances.*
      FROM runtime_instances
      JOIN worker_instances ON worker_instances.id = runtime_instances.worker_instance_id
                           AND worker_instances.worker_group_id = runtime_instances.worker_group_id
      JOIN worker_groups ON worker_groups.id = runtime_instances.worker_group_id
                        AND worker_groups.state IN ('active', 'draining')
     WHERE runtime_instances.id = sqlc.arg(id)
       AND runtime_instances.worker_instance_id = sqlc.arg(worker_instance_id)
       AND runtime_instances.instance_token = sqlc.arg(instance_token)
       AND runtime_instances.state IN ('preparing', 'ready', 'binding', 'running', 'waiting_hot', 'checkpointing', 'stopping', 'lost')
     FOR UPDATE OF runtime_instances
),
closed_runtime_instance AS (
    UPDATE runtime_instances
       SET state = CASE WHEN runtime_instances.state = 'lost' THEN runtime_instances.state ELSE 'closed'::runtime_instance_state END,
           closed_at = CASE WHEN runtime_instances.state = 'lost' THEN runtime_instances.closed_at ELSE coalesce(runtime_instances.closed_at, now()) END,
           expires_at = NULL,
           adopting_workspace_mount_id = NULL,
           owner_workspace_id = NULL,
           owner_workspace_version_id = NULL,
           owner_run_id = NULL,
           owner_run_lease_id = NULL,
           owner_run_wait_id = NULL,
           owner_run_state_version = NULL,
           updated_at = now()
      FROM target
     WHERE runtime_instances.id = target.id
    RETURNING runtime_instances.*
)
SELECT *
  FROM closed_runtime_instance;

-- name: MarkRuntimeInstanceFailed :one
WITH target AS MATERIALIZED (
    SELECT runtime_instances.*
      FROM runtime_instances
      JOIN worker_instances ON worker_instances.id = runtime_instances.worker_instance_id
                           AND worker_instances.worker_group_id = runtime_instances.worker_group_id
      JOIN worker_groups ON worker_groups.id = runtime_instances.worker_group_id
                        AND worker_groups.state IN ('active', 'draining')
     WHERE runtime_instances.id = sqlc.arg(id)
       AND runtime_instances.worker_instance_id = sqlc.arg(worker_instance_id)
       AND runtime_instances.instance_token = sqlc.arg(instance_token)
       AND runtime_instances.state IN ('preparing', 'ready', 'binding', 'running', 'waiting_hot', 'checkpointing', 'stopping')
     FOR UPDATE OF runtime_instances
),
failed_runtime_instance AS (
    UPDATE runtime_instances
       SET state = 'failed',
           failed_at = coalesce(runtime_instances.failed_at, now()),
           expires_at = NULL,
           adopting_workspace_mount_id = NULL,
           owner_workspace_id = NULL,
           owner_workspace_version_id = NULL,
           owner_run_id = NULL,
           owner_run_lease_id = NULL,
           owner_run_wait_id = NULL,
           owner_run_state_version = NULL,
           error = COALESCE(sqlc.arg(error)::jsonb, '{}'::jsonb),
           updated_at = now()
      FROM target
     WHERE runtime_instances.id = target.id
    RETURNING runtime_instances.*
)
SELECT runtime_instances.*
  FROM runtime_instances
  JOIN failed_runtime_instance ON failed_runtime_instance.id = runtime_instances.id;

-- name: CreateExpiredRuntimeStopCommands :many
WITH target AS MATERIALIZED (
    SELECT runtime_instances.*
      FROM runtime_instances
	     WHERE runtime_instances.state IN ('preparing', 'ready')
	       AND runtime_instances.worker_group_id = sqlc.arg(worker_group_id)
	       AND runtime_instances.expires_at IS NOT NULL
       AND runtime_instances.expires_at <= sqlc.arg(expired_before)
       AND runtime_instances.workspace_mount_id IS NULL
       AND runtime_instances.adopting_workspace_mount_id IS NULL
       AND NOT EXISTS (
           SELECT 1
             FROM worker_commands
            WHERE worker_commands.org_id = runtime_instances.org_id
              AND worker_commands.runtime_instance_id = runtime_instances.id
              AND worker_commands.runtime_epoch = runtime_instances.runtime_epoch
              AND worker_commands.kind = 'runtime_stop'
              AND worker_commands.acknowledged_at IS NULL
       )
     FOR UPDATE OF runtime_instances SKIP LOCKED
),
stopping_runtime_instances AS (
    UPDATE runtime_instances
       SET state = 'stopping',
           stopping_requested_at = COALESCE(runtime_instances.stopping_requested_at, now()),
           expires_at = now() + interval '5 minutes',
           adopting_workspace_mount_id = NULL,
           updated_at = now()
      FROM target
     WHERE runtime_instances.id = target.id
    RETURNING runtime_instances.*
)
INSERT INTO worker_commands (
    org_id,
    worker_group_id,
    project_id,
    environment_id,
    worker_instance_id,
    runtime_instance_id,
    runtime_epoch,
    kind,
    payload
)
SELECT stopping_runtime_instances.org_id,
       stopping_runtime_instances.worker_group_id,
       stopping_runtime_instances.project_id,
       stopping_runtime_instances.environment_id,
       stopping_runtime_instances.worker_instance_id,
       stopping_runtime_instances.id,
       stopping_runtime_instances.runtime_epoch,
       'runtime_stop',
       jsonb_build_object('reason', 'expired_runtime_instance')
  FROM stopping_runtime_instances
RETURNING *;

-- name: MarkExpiredRuntimeInstancesLost :many
WITH target AS MATERIALIZED (
    SELECT runtime_instances.*
      FROM runtime_instances
	     WHERE runtime_instances.expires_at IS NOT NULL
	       AND runtime_instances.worker_group_id = sqlc.arg(worker_group_id)
	       AND runtime_instances.expires_at <= sqlc.arg(expired_before)
       AND (
           runtime_instances.state = 'stopping'
           OR runtime_instances.state IN ('binding', 'running', 'waiting_hot', 'checkpointing')
       )
     FOR UPDATE OF runtime_instances SKIP LOCKED
),
lost_runtime_instances AS (
    UPDATE runtime_instances
       SET state = 'lost',
           lost_at = coalesce(runtime_instances.lost_at, now()),
           expires_at = NULL,
           adopting_workspace_mount_id = NULL,
           owner_workspace_id = NULL,
           owner_workspace_version_id = NULL,
           owner_run_id = NULL,
           owner_run_lease_id = NULL,
           owner_run_wait_id = NULL,
           owner_run_state_version = NULL,
           updated_at = now()
      FROM target
     WHERE runtime_instances.id = target.id
    RETURNING runtime_instances.*
)
SELECT runtime_instances.*
  FROM runtime_instances
  JOIN lost_runtime_instances ON lost_runtime_instances.id = runtime_instances.id;

-- name: CreateSupersededPreparedRuntimeStopCommands :many
WITH candidates AS (
    SELECT runtime_instances.*
      FROM runtime_instances
      JOIN deployment_sandboxes
        ON deployment_sandboxes.org_id = runtime_instances.org_id
       AND deployment_sandboxes.project_id = runtime_instances.project_id
       AND deployment_sandboxes.environment_id = runtime_instances.environment_id
       AND deployment_sandboxes.id = runtime_instances.deployment_sandbox_id
      JOIN environments
        ON environments.org_id = deployment_sandboxes.org_id
       AND environments.project_id = deployment_sandboxes.project_id
       AND environments.id = deployment_sandboxes.environment_id
     WHERE runtime_instances.state IN ('preparing', 'ready')
       AND runtime_instances.workspace_mount_id IS NULL
       AND runtime_instances.adopting_workspace_mount_id IS NULL
       AND environments.current_deployment_id IS DISTINCT FROM deployment_sandboxes.deployment_id
       AND NOT EXISTS (
           SELECT 1
             FROM worker_commands
            WHERE worker_commands.org_id = runtime_instances.org_id
              AND worker_commands.runtime_instance_id = runtime_instances.id
              AND worker_commands.runtime_epoch = runtime_instances.runtime_epoch
              AND worker_commands.kind = 'runtime_stop'
              AND worker_commands.acknowledged_at IS NULL
       )
     ORDER BY runtime_instances.updated_at ASC, runtime_instances.id ASC
     LIMIT sqlc.arg(row_limit)
     FOR UPDATE OF runtime_instances SKIP LOCKED
),
stopping_runtime_instances AS (
    UPDATE runtime_instances
       SET state = 'stopping',
           stopping_requested_at = COALESCE(runtime_instances.stopping_requested_at, now()),
           expires_at = now() + interval '5 minutes',
           adopting_workspace_mount_id = NULL,
           updated_at = now()
      FROM candidates
     WHERE runtime_instances.id = candidates.id
    RETURNING runtime_instances.*
)
INSERT INTO worker_commands (
    org_id,
    worker_group_id,
    project_id,
    environment_id,
    worker_instance_id,
    runtime_instance_id,
    runtime_epoch,
    kind,
    payload
)
SELECT stopping_runtime_instances.org_id,
       stopping_runtime_instances.worker_group_id,
       stopping_runtime_instances.project_id,
       stopping_runtime_instances.environment_id,
       stopping_runtime_instances.worker_instance_id,
       stopping_runtime_instances.id,
       stopping_runtime_instances.runtime_epoch,
       'runtime_stop',
       jsonb_build_object('reason', 'superseded_deployment')
  FROM stopping_runtime_instances
RETURNING *;

-- name: ListRuntimeInstanceWarmTargets :many
WITH current_sandboxes AS MATERIALIZED (
    SELECT deployment_sandboxes.*,
           worker_groups.id AS worker_group_id,
           image_artifact.digest AS image_artifact_digest,
           image_artifact.media_type AS image_artifact_media_type,
           image_artifact.size_bytes AS image_artifact_size_bytes,
           coalesce((deployment_sandboxes.resource_floor->>'milli_cpu')::integer, 1000) AS requested_cpu_millis,
           coalesce((deployment_sandboxes.resource_floor->>'memory_mib')::integer, 1024) AS requested_memory_mib,
           deployment_sandboxes.disk_floor_mib AS requested_disk_mib,
           1::integer AS requested_execution_slots
      FROM deployment_sandboxes
      JOIN deployments
        ON deployments.org_id = deployment_sandboxes.org_id
       AND deployments.project_id = deployment_sandboxes.project_id
       AND deployments.environment_id = deployment_sandboxes.environment_id
       AND deployments.id = deployment_sandboxes.deployment_id
      JOIN environments
        ON environments.org_id = deployment_sandboxes.org_id
       AND environments.project_id = deployment_sandboxes.project_id
       AND environments.id = deployment_sandboxes.environment_id
       AND environments.current_deployment_id = deployment_sandboxes.deployment_id
      JOIN artifacts AS image_artifact
        ON image_artifact.org_id = deployment_sandboxes.org_id
       AND image_artifact.project_id = deployment_sandboxes.project_id
       AND image_artifact.environment_id = deployment_sandboxes.environment_id
       AND image_artifact.id = deployment_sandboxes.image_artifact_id
       AND image_artifact.digest = deployment_sandboxes.image_digest
       AND image_artifact.kind = 'sandbox_image'
       AND image_artifact.media_type = 'application/vnd.helmr.sandbox-image.v0.oci-tar'
      JOIN projects
        ON projects.org_id = deployment_sandboxes.org_id
       AND projects.id = deployment_sandboxes.project_id
      JOIN worker_groups
        ON worker_groups.region_id = projects.default_region_id
       AND worker_groups.state = 'active'
       AND worker_groups.health_state IN ('healthy', 'degraded')
       AND worker_groups.routing_fresh_until > now()
     WHERE (
           sqlc.narg(deployment_sandbox_id)::uuid IS NULL
           OR deployment_sandboxes.id = sqlc.narg(deployment_sandbox_id)::uuid
       )
),
worker_sandbox_scope AS MATERIALIZED (
    SELECT worker_instances.id AS worker_instance_id,
           worker_instances.worker_group_id,
           worker_instances.available_milli_cpu,
           worker_instances.available_memory_mib,
           worker_instances.available_disk_mib,
           worker_instances.available_execution_slots,
           worker_instances.runtime_id AS runtime_release_id,
           worker_instances.rootfs_digest,
           worker_instances.runtime_abi,
           current_sandboxes.id AS deployment_sandbox_id,
           current_sandboxes.org_id,
           current_sandboxes.project_id,
           current_sandboxes.environment_id,
           current_sandboxes.requested_cpu_millis,
           current_sandboxes.requested_memory_mib,
           current_sandboxes.requested_disk_mib,
           current_sandboxes.requested_execution_slots,
           current_sandboxes.fingerprint,
           current_sandboxes.image_artifact_id,
           current_sandboxes.image_artifact_format,
           current_sandboxes.image_artifact_digest,
           current_sandboxes.image_artifact_media_type,
           current_sandboxes.image_artifact_size_bytes,
           current_sandboxes.image_digest,
           current_sandboxes.image_format,
           current_sandboxes.workspace_mount_path,
           current_sandboxes.guestd_abi,
           current_sandboxes.adapter_abi
      FROM worker_instances
      JOIN current_sandboxes
        ON current_sandboxes.worker_group_id = worker_instances.worker_group_id
       AND current_sandboxes.rootfs_digest = worker_instances.rootfs_digest
       AND current_sandboxes.runtime_abi = worker_instances.runtime_abi
     WHERE worker_instances.status = 'active'
       AND worker_instances.available_execution_slots > 0
),
worker_usage_scope AS MATERIALIZED (
    SELECT DISTINCT worker_sandbox_scope.worker_instance_id,
           worker_sandbox_scope.available_milli_cpu,
           worker_sandbox_scope.available_memory_mib,
           worker_sandbox_scope.available_disk_mib,
           worker_sandbox_scope.available_execution_slots
      FROM worker_sandbox_scope
),
active_run_usage AS MATERIALIZED (
    SELECT worker_usage_scope.worker_instance_id,
           COALESCE(sum(run_runtime_requirements.requested_milli_cpu), 0)::bigint AS used_milli_cpu,
           COALESCE(sum(run_runtime_requirements.requested_memory_mib), 0)::bigint AS used_memory_mib,
           COALESCE(sum(run_runtime_requirements.requested_disk_mib), 0)::bigint AS used_disk_mib,
           COALESCE(sum(run_runtime_requirements.requested_execution_slots), 0)::int AS used_slots
      FROM worker_usage_scope
      LEFT JOIN run_leases ON run_leases.worker_instance_id = worker_usage_scope.worker_instance_id
                           AND run_leases.status IN ('leased', 'running')
      LEFT JOIN runs ON runs.org_id = run_leases.org_id
                    AND runs.id = run_leases.run_id
      LEFT JOIN run_runtime_requirements ON run_runtime_requirements.org_id = run_leases.org_id
                                        AND run_runtime_requirements.run_id = run_leases.run_id
                                        AND runs.workspace_mount_id IS NULL
     GROUP BY worker_usage_scope.worker_instance_id
),
active_runtime_instance_usage AS MATERIALIZED (
    SELECT worker_usage_scope.worker_instance_id,
           COALESCE(sum(runtime_instances.reserved_cpu_millis), 0)::bigint AS used_milli_cpu,
           COALESCE(sum(runtime_instances.reserved_memory_mib), 0)::bigint AS used_memory_mib,
           COALESCE(sum(runtime_instances.reserved_disk_mib), 0)::bigint AS used_disk_mib,
           COALESCE(sum(runtime_instances.reserved_execution_slots), 0)::int AS used_slots
      FROM worker_usage_scope
      LEFT JOIN runtime_instances
        ON runtime_instances.worker_instance_id = worker_usage_scope.worker_instance_id
       AND runtime_instances.state IN ('preparing', 'ready', 'binding', 'running', 'waiting_hot', 'checkpointing', 'stopping')
       AND (
           runtime_instances.expires_at IS NULL
           OR runtime_instances.expires_at > now()
       )
     GROUP BY worker_usage_scope.worker_instance_id
),
prepared_supply AS MATERIALIZED (
    SELECT worker_sandbox_scope.worker_instance_id,
           worker_sandbox_scope.deployment_sandbox_id,
           count(runtime_instances.id)::integer AS supply_count
      FROM worker_sandbox_scope
      LEFT JOIN runtime_instances
        ON runtime_instances.worker_instance_id = worker_sandbox_scope.worker_instance_id
       AND runtime_instances.deployment_sandbox_id = worker_sandbox_scope.deployment_sandbox_id
       AND runtime_instances.runtime_release_id = worker_sandbox_scope.runtime_release_id
       AND runtime_instances.rootfs_digest = worker_sandbox_scope.rootfs_digest
       AND runtime_instances.runtime_abi = worker_sandbox_scope.runtime_abi
       AND runtime_instances.state IN ('preparing', 'ready')
       AND runtime_instances.adopting_workspace_mount_id IS NULL
       AND (
           runtime_instances.expires_at IS NULL
           OR runtime_instances.expires_at > now()
       )
     GROUP BY worker_sandbox_scope.worker_instance_id, worker_sandbox_scope.deployment_sandbox_id
),
worker_prepared_supply AS MATERIALIZED (
    SELECT worker_usage_scope.worker_instance_id,
           count(runtime_instances.id)::integer AS supply_count
      FROM worker_usage_scope
      LEFT JOIN runtime_instances
       ON runtime_instances.worker_instance_id = worker_usage_scope.worker_instance_id
       AND runtime_instances.state IN ('preparing', 'ready')
       AND runtime_instances.adopting_workspace_mount_id IS NULL
       AND (
           runtime_instances.expires_at IS NULL
           OR runtime_instances.expires_at > now()
       )
     GROUP BY worker_usage_scope.worker_instance_id
),
pending_warm_commands AS MATERIALIZED (
    SELECT worker_sandbox_scope.worker_instance_id,
           worker_sandbox_scope.deployment_sandbox_id,
           count(worker_commands.id)::integer AS command_count
      FROM worker_sandbox_scope
      LEFT JOIN worker_commands
        ON worker_commands.worker_instance_id = worker_sandbox_scope.worker_instance_id
       AND worker_commands.kind = 'runtime_prepare'
       AND worker_commands.acknowledged_at IS NULL
       AND worker_commands.payload->>'deployment_sandbox_id' = worker_sandbox_scope.deployment_sandbox_id::text
     GROUP BY worker_sandbox_scope.worker_instance_id, worker_sandbox_scope.deployment_sandbox_id
),
worker_pending_warm_commands AS MATERIALIZED (
    SELECT worker_usage_scope.worker_instance_id,
           count(worker_commands.id)::integer AS command_count
      FROM worker_usage_scope
      LEFT JOIN worker_commands
        ON worker_commands.worker_instance_id = worker_usage_scope.worker_instance_id
       AND worker_commands.kind = 'runtime_prepare'
       AND worker_commands.acknowledged_at IS NULL
     GROUP BY worker_usage_scope.worker_instance_id
),
sandbox_demand AS MATERIALIZED (
    SELECT worker_sandbox_scope.worker_instance_id,
           worker_sandbox_scope.deployment_sandbox_id,
           GREATEST(
               max(coalesce(workspace_mounts.requested_at, workspace_mounts.created_at)),
               max(runs.created_at)
           ) AS last_demand_at,
           (
               count(DISTINCT workspace_mounts.id)
               + count(DISTINCT runs.id)
               + count(DISTINCT deployment_tasks.id)
           )::integer AS demand_count
      FROM worker_sandbox_scope
      LEFT JOIN workspace_mounts
        ON workspace_mounts.org_id = worker_sandbox_scope.org_id
       AND workspace_mounts.project_id = worker_sandbox_scope.project_id
       AND workspace_mounts.environment_id = worker_sandbox_scope.environment_id
       AND workspace_mounts.deployment_sandbox_id = worker_sandbox_scope.deployment_sandbox_id
      LEFT JOIN deployment_tasks
        ON deployment_tasks.org_id = worker_sandbox_scope.org_id
       AND deployment_tasks.project_id = worker_sandbox_scope.project_id
       AND deployment_tasks.environment_id = worker_sandbox_scope.environment_id
       AND deployment_tasks.deployment_sandbox_id = worker_sandbox_scope.deployment_sandbox_id
      LEFT JOIN runs
        ON runs.org_id = deployment_tasks.org_id
       AND runs.project_id = deployment_tasks.project_id
       AND runs.environment_id = deployment_tasks.environment_id
       AND runs.task_id = deployment_tasks.task_id
     GROUP BY worker_sandbox_scope.worker_instance_id, worker_sandbox_scope.deployment_sandbox_id
),
eligible_warm_targets AS MATERIALIZED (
    SELECT worker_sandbox_scope.org_id,
           worker_sandbox_scope.worker_group_id,
           worker_sandbox_scope.project_id,
           worker_sandbox_scope.environment_id,
           worker_sandbox_scope.worker_instance_id,
           worker_sandbox_scope.deployment_sandbox_id,
           worker_sandbox_scope.runtime_release_id,
           worker_sandbox_scope.rootfs_digest,
           worker_sandbox_scope.runtime_abi,
           worker_sandbox_scope.image_artifact_digest AS sandbox_image_artifact_digest,
           worker_sandbox_scope.image_artifact_media_type AS sandbox_image_artifact_media_type,
           worker_sandbox_scope.image_artifact_size_bytes AS sandbox_image_artifact_size_bytes,
           worker_sandbox_scope.image_artifact_format AS sandbox_image_artifact_format,
           worker_sandbox_scope.image_digest,
           worker_sandbox_scope.image_format,
           worker_sandbox_scope.workspace_mount_path,
           worker_sandbox_scope.guestd_abi,
           worker_sandbox_scope.adapter_abi,
           prepared_supply.supply_count,
           pending_warm_commands.command_count,
           sandbox_demand.last_demand_at,
           sandbox_demand.demand_count,
           LEAST(
               sqlc.arg(target_count)::integer - worker_prepared_supply.supply_count - worker_pending_warm_commands.command_count,
               GREATEST(
                   worker_sandbox_scope.available_execution_slots
                   - active_run_usage.used_slots
                   - active_runtime_instance_usage.used_slots
                   - 1,
                   0
               )
           )::integer AS worker_target_budget,
           row_number() OVER (
               PARTITION BY worker_sandbox_scope.worker_instance_id
               ORDER BY sandbox_demand.last_demand_at DESC NULLS LAST,
                        sandbox_demand.demand_count DESC,
                        prepared_supply.supply_count ASC,
                        pending_warm_commands.command_count ASC,
                        worker_sandbox_scope.environment_id,
                        worker_sandbox_scope.deployment_sandbox_id
           )::integer AS worker_target_rank
      FROM worker_sandbox_scope
      JOIN active_run_usage USING (worker_instance_id)
      JOIN active_runtime_instance_usage USING (worker_instance_id)
      JOIN prepared_supply USING (worker_instance_id, deployment_sandbox_id)
      JOIN worker_prepared_supply USING (worker_instance_id)
      JOIN pending_warm_commands USING (worker_instance_id, deployment_sandbox_id)
      JOIN worker_pending_warm_commands USING (worker_instance_id)
      JOIN sandbox_demand USING (worker_instance_id, deployment_sandbox_id)
     WHERE sqlc.arg(target_count)::integer > 0
       AND prepared_supply.supply_count + pending_warm_commands.command_count = 0
       AND worker_prepared_supply.supply_count + worker_pending_warm_commands.command_count < sqlc.arg(target_count)::integer
       AND worker_sandbox_scope.requested_cpu_millis <= GREATEST(worker_sandbox_scope.available_milli_cpu - active_run_usage.used_milli_cpu - active_runtime_instance_usage.used_milli_cpu - worker_sandbox_scope.requested_cpu_millis, 0)
       AND worker_sandbox_scope.requested_memory_mib <= GREATEST(worker_sandbox_scope.available_memory_mib - active_run_usage.used_memory_mib - active_runtime_instance_usage.used_memory_mib - worker_sandbox_scope.requested_memory_mib, 0)
       AND worker_sandbox_scope.requested_disk_mib <= GREATEST(worker_sandbox_scope.available_disk_mib - active_run_usage.used_disk_mib - active_runtime_instance_usage.used_disk_mib - worker_sandbox_scope.requested_disk_mib, 0)
       AND worker_sandbox_scope.requested_execution_slots <= GREATEST(worker_sandbox_scope.available_execution_slots - active_run_usage.used_slots - active_runtime_instance_usage.used_slots - 1, 0)
)
SELECT org_id,
       worker_group_id,
       project_id,
       environment_id,
       worker_instance_id,
       deployment_sandbox_id,
       runtime_release_id,
       rootfs_digest,
       runtime_abi,
       sandbox_image_artifact_digest,
       sandbox_image_artifact_media_type,
       sandbox_image_artifact_size_bytes,
       sandbox_image_artifact_format,
       image_digest,
       image_format,
       workspace_mount_path,
       guestd_abi,
       adapter_abi,
       supply_count,
       command_count,
       last_demand_at,
       demand_count
  FROM eligible_warm_targets
 WHERE worker_target_rank <= worker_target_budget
 ORDER BY last_demand_at DESC NULLS LAST,
          demand_count DESC,
          supply_count ASC,
          command_count ASC,
          environment_id,
          deployment_sandbox_id,
          worker_instance_id
 LIMIT sqlc.arg(row_limit);

-- name: ListRuntimeSubstratePrepareTargets :many
WITH current_sandboxes AS MATERIALIZED (
    SELECT deployment_sandboxes.*,
           worker_groups.id AS worker_group_id,
           image_artifact.digest AS image_artifact_digest,
           image_artifact.media_type AS image_artifact_media_type,
           image_artifact.size_bytes AS image_artifact_size_bytes
      FROM deployment_sandboxes
      JOIN deployments
        ON deployments.org_id = deployment_sandboxes.org_id
       AND deployments.project_id = deployment_sandboxes.project_id
       AND deployments.environment_id = deployment_sandboxes.environment_id
       AND deployments.id = deployment_sandboxes.deployment_id
      JOIN environments
        ON environments.org_id = deployment_sandboxes.org_id
       AND environments.project_id = deployment_sandboxes.project_id
       AND environments.id = deployment_sandboxes.environment_id
       AND environments.current_deployment_id = deployment_sandboxes.deployment_id
      JOIN artifacts AS image_artifact
        ON image_artifact.org_id = deployment_sandboxes.org_id
       AND image_artifact.project_id = deployment_sandboxes.project_id
       AND image_artifact.environment_id = deployment_sandboxes.environment_id
       AND image_artifact.id = deployment_sandboxes.image_artifact_id
       AND image_artifact.digest = deployment_sandboxes.image_digest
       AND image_artifact.kind = 'sandbox_image'
       AND image_artifact.media_type = 'application/vnd.helmr.sandbox-image.v0.oci-tar'
      JOIN projects
        ON projects.org_id = deployment_sandboxes.org_id
       AND projects.id = deployment_sandboxes.project_id
      JOIN worker_groups
        ON worker_groups.region_id = projects.default_region_id
       AND worker_groups.state = 'active'
       AND worker_groups.health_state IN ('healthy', 'degraded')
       AND worker_groups.routing_fresh_until > now()
),
worker_sandbox_scope AS MATERIALIZED (
    SELECT worker_instances.id AS worker_instance_id,
           worker_instances.worker_group_id,
           worker_instances.runtime_id AS runtime_release_id,
           worker_instances.rootfs_digest,
           worker_instances.runtime_abi,
           current_sandboxes.id AS deployment_sandbox_id,
           current_sandboxes.org_id,
           current_sandboxes.project_id,
           current_sandboxes.environment_id,
           current_sandboxes.image_artifact_format,
           current_sandboxes.image_artifact_digest,
           current_sandboxes.image_artifact_media_type,
           current_sandboxes.image_artifact_size_bytes,
           current_sandboxes.image_digest,
           current_sandboxes.image_format,
           current_sandboxes.workspace_mount_path,
           current_sandboxes.guestd_abi,
           current_sandboxes.adapter_abi
      FROM worker_instances
      JOIN current_sandboxes
        ON current_sandboxes.worker_group_id = worker_instances.worker_group_id
       AND current_sandboxes.rootfs_digest = worker_instances.rootfs_digest
       AND current_sandboxes.runtime_abi = worker_instances.runtime_abi
     WHERE worker_instances.status = 'active'
),
sandbox_demand AS MATERIALIZED (
    SELECT worker_sandbox_scope.worker_instance_id,
           worker_sandbox_scope.deployment_sandbox_id,
           max(coalesce(workspace_mounts.requested_at, workspace_mounts.created_at)) AS last_demand_at,
           count(workspace_mounts.id)::integer AS demand_count
      FROM worker_sandbox_scope
      LEFT JOIN workspace_mounts
        ON workspace_mounts.org_id = worker_sandbox_scope.org_id
       AND workspace_mounts.project_id = worker_sandbox_scope.project_id
       AND workspace_mounts.environment_id = worker_sandbox_scope.environment_id
       AND workspace_mounts.deployment_sandbox_id = worker_sandbox_scope.deployment_sandbox_id
     GROUP BY worker_sandbox_scope.worker_instance_id, worker_sandbox_scope.deployment_sandbox_id
)
SELECT worker_sandbox_scope.org_id,
       worker_sandbox_scope.worker_group_id,
       worker_sandbox_scope.project_id,
       worker_sandbox_scope.environment_id,
       worker_sandbox_scope.worker_instance_id,
       worker_sandbox_scope.deployment_sandbox_id,
       worker_sandbox_scope.runtime_release_id,
       worker_sandbox_scope.rootfs_digest,
       worker_sandbox_scope.runtime_abi,
       worker_sandbox_scope.image_artifact_digest AS sandbox_image_artifact_digest,
       worker_sandbox_scope.image_artifact_media_type AS sandbox_image_artifact_media_type,
       worker_sandbox_scope.image_artifact_size_bytes AS sandbox_image_artifact_size_bytes,
       worker_sandbox_scope.image_artifact_format AS sandbox_image_artifact_format,
       worker_sandbox_scope.image_digest,
       worker_sandbox_scope.image_format,
       worker_sandbox_scope.workspace_mount_path,
       worker_sandbox_scope.guestd_abi,
       worker_sandbox_scope.adapter_abi,
       0::integer AS supply_count,
       0::integer AS command_count,
       sandbox_demand.last_demand_at,
       sandbox_demand.demand_count
  FROM worker_sandbox_scope
  JOIN sandbox_demand USING (worker_instance_id, deployment_sandbox_id)
 WHERE sandbox_demand.demand_count > 0
   AND NOT EXISTS (
           SELECT 1
            FROM runtime_substrate_artifacts
            WHERE runtime_substrate_artifacts.org_id = worker_sandbox_scope.org_id
              AND runtime_substrate_artifacts.worker_group_id = worker_sandbox_scope.worker_group_id
              AND runtime_substrate_artifacts.project_id = worker_sandbox_scope.project_id
              AND runtime_substrate_artifacts.environment_id = worker_sandbox_scope.environment_id
              AND runtime_substrate_artifacts.deployment_sandbox_id = worker_sandbox_scope.deployment_sandbox_id
              AND runtime_substrate_artifacts.substrate_format = sqlc.arg(substrate_format)
              AND runtime_substrate_artifacts.builder_abi = sqlc.arg(substrate_builder_abi)
              AND runtime_substrate_artifacts.layout_abi = sqlc.arg(substrate_layout_abi)
              AND runtime_substrate_artifacts.source->'substrate_source'->>'sandbox_artifact_digest' = worker_sandbox_scope.image_artifact_digest
              AND runtime_substrate_artifacts.source->'substrate_source'->>'sandbox_artifact_format' = worker_sandbox_scope.image_artifact_format
              AND runtime_substrate_artifacts.source->'substrate_source'->>'image_digest' = worker_sandbox_scope.image_digest
              AND runtime_substrate_artifacts.source->'substrate_source'->>'rootfs_digest' = worker_sandbox_scope.rootfs_digest
              AND runtime_substrate_artifacts.source->'substrate_source'->>'runtime_abi' = worker_sandbox_scope.runtime_abi
              AND runtime_substrate_artifacts.source->'substrate_source'->>'guestd_abi' = worker_sandbox_scope.guestd_abi
              AND runtime_substrate_artifacts.source->'substrate_source'->>'adapter_abi' = worker_sandbox_scope.adapter_abi
              AND runtime_substrate_artifacts.source->'substrate_source'->>'workspace_mount_path' = worker_sandbox_scope.workspace_mount_path
              AND runtime_substrate_artifacts.retired_at IS NULL
       )
   AND NOT EXISTS (
           SELECT 1
             FROM worker_commands
            WHERE worker_commands.worker_instance_id = worker_sandbox_scope.worker_instance_id
              AND worker_commands.kind = 'runtime_substrate_prepare'
              AND worker_commands.acknowledged_at IS NULL
              AND worker_commands.deployment_sandbox_id = worker_sandbox_scope.deployment_sandbox_id
              AND worker_commands.payload->'source'->>'sandbox_image_artifact_format' = worker_sandbox_scope.image_artifact_format
              AND worker_commands.payload->'source'->'sandbox_image_artifact'->>'digest' = worker_sandbox_scope.image_artifact_digest
              AND worker_commands.payload->'source'->>'image_digest' = worker_sandbox_scope.image_digest
              AND worker_commands.payload->'source'->>'rootfs_digest' = worker_sandbox_scope.rootfs_digest
              AND worker_commands.payload->'source'->>'runtime_abi' = worker_sandbox_scope.runtime_abi
              AND worker_commands.payload->'source'->>'guestd_abi' = worker_sandbox_scope.guestd_abi
              AND worker_commands.payload->'source'->>'adapter_abi' = worker_sandbox_scope.adapter_abi
              AND worker_commands.payload->'source'->>'workspace_mount_path' = worker_sandbox_scope.workspace_mount_path
       )
 ORDER BY sandbox_demand.last_demand_at DESC NULLS LAST,
          sandbox_demand.demand_count DESC,
          worker_sandbox_scope.environment_id,
          worker_sandbox_scope.deployment_sandbox_id,
          worker_sandbox_scope.worker_instance_id
 LIMIT sqlc.arg(row_limit);
