-- name: LockWorkerGroupCreationRegion :exec
SELECT pg_advisory_xact_lock(sqlc.arg(lock_key)::bigint);

-- name: LockWorkerGroupMutation :exec
SELECT pg_advisory_xact_lock(sqlc.arg(lock_key)::bigint);

-- name: GetWorkerGroupByRegionName :one
SELECT *
  FROM worker_groups
 WHERE region_id = sqlc.arg(region_id)
   AND name = sqlc.arg(name);

-- name: CreateWorkerGroup :one
WITH token AS (
    INSERT INTO worker_group_tokens (id, token_hash)
    VALUES (sqlc.arg(token_id), sqlc.arg(token_hash))
    RETURNING id
)
INSERT INTO worker_groups (
    id, token_id, region_id, name, description, state, allows_run, allows_build,
    required_cpu_millis, required_memory_bytes, required_guest_ephemeral_disk_bytes,
    required_build_cache_bytes, required_artifact_cache_bytes,
    required_vm_slots, required_build_executors, observation_ttl_seconds
)
SELECT sqlc.arg(id), token.id, sqlc.arg(region_id), sqlc.arg(name),
       sqlc.arg(description), 'active', sqlc.arg(allows_run), sqlc.arg(allows_build),
       sqlc.arg(required_cpu_millis), sqlc.arg(required_memory_bytes),
       sqlc.arg(required_guest_ephemeral_disk_bytes),
       sqlc.arg(required_build_cache_bytes), sqlc.arg(required_artifact_cache_bytes),
       sqlc.arg(required_vm_slots), sqlc.arg(required_build_executors),
       sqlc.arg(observation_ttl_seconds)
  FROM token
RETURNING *;

-- name: UpdateWorkerGroupDescription :one
UPDATE worker_groups
   SET description = sqlc.arg(description), updated_at = now()
 WHERE id = sqlc.arg(id)
RETURNING *;

-- name: RotateWorkerGroupToken :one
UPDATE worker_group_tokens
   SET token_hash = sqlc.arg(token_hash), updated_at = now()
  FROM worker_groups
 WHERE worker_groups.id = sqlc.arg(worker_group_id)
   AND worker_groups.token_id = worker_group_tokens.id
RETURNING worker_group_tokens.*;

-- name: ListWorkerGroups :many
SELECT *
  FROM worker_groups
 WHERE sqlc.narg(region_id)::text IS NULL OR region_id = sqlc.narg(region_id)
 ORDER BY region_id, name ASC
 LIMIT sqlc.arg(row_limit);

-- name: GetWorkerGroup :one
SELECT * FROM worker_groups WHERE id = sqlc.arg(id);

-- name: GetControlPlaneWorkerGroupReadiness :one
SELECT id AS worker_group_id,
       state,
       state = 'active' AS routable
  FROM worker_groups
 WHERE id = sqlc.arg(worker_group_id);

-- name: GetWorkerGroupState :one
SELECT id, state, claim_version
  FROM worker_groups
 WHERE id = sqlc.arg(worker_group_id);

-- name: TransitionWorkerGroupState :one
WITH transitioned AS (
    UPDATE worker_groups
       SET state = sqlc.arg(target_state),
           claim_version = worker_groups.claim_version + 1,
           updated_at = now()
     WHERE worker_groups.id = sqlc.arg(worker_group_id)
       AND worker_groups.claim_version = sqlc.arg(expected_claim_version)
       AND (
           (worker_groups.state = 'active' AND sqlc.arg(target_state)::text IN ('paused', 'draining'))
           OR (worker_groups.state = 'paused' AND sqlc.arg(target_state)::text IN ('active', 'draining'))
           OR (
               worker_groups.state = 'draining'
               AND sqlc.arg(target_state)::text = 'disabled'
               AND NOT EXISTS (
                   SELECT 1 FROM worker_instances
                    WHERE worker_instances.worker_group_id = worker_groups.id
                      AND worker_instances.state IN ('registering', 'active', 'draining')
               )
               AND NOT EXISTS (
                   SELECT 1 FROM run_leases
                    WHERE run_leases.worker_group_id = worker_groups.id
                      AND run_leases.state IN ('assigned', 'starting', 'running', 'checkpointing', 'finalizing')
               )
               AND NOT EXISTS (
                   SELECT 1 FROM deployment_build_leases
                    WHERE deployment_build_leases.worker_group_id = worker_groups.id
                      AND deployment_build_leases.state IN ('assigned', 'starting', 'running')
               )
               AND NOT EXISTS (
                   SELECT 1 FROM runtime_instances
                    WHERE runtime_instances.worker_group_id = worker_groups.id
                      AND runtime_instances.reclaimed_at IS NULL
               )
               AND NOT EXISTS (
                   SELECT 1 FROM workspace_mounts
                    WHERE workspace_mounts.worker_group_id = worker_groups.id
                      AND workspace_mounts.state IN ('mounting', 'mounted', 'unmounting')
               )
               AND NOT EXISTS (
                   SELECT 1 FROM workspace_leases
                    WHERE workspace_leases.worker_group_id = worker_groups.id
                      AND workspace_leases.state IN ('active', 'releasing')
               )
               AND NOT EXISTS (
                   SELECT 1 FROM workspace_processes
                    WHERE workspace_processes.worker_group_id = worker_groups.id
                      AND workspace_processes.state IN ('starting', 'running', 'exit_requested')
               )
           )
       )
    RETURNING worker_groups.id, worker_groups.state, worker_groups.claim_version
)
SELECT id, state, claim_version, true AS transition_applied
  FROM transitioned
UNION ALL
SELECT worker_groups.id, worker_groups.state, worker_groups.claim_version,
       false AS transition_applied
  FROM worker_groups
 WHERE worker_groups.id = sqlc.arg(worker_group_id)
   AND worker_groups.state = sqlc.arg(target_state)::text
   AND worker_groups.claim_version = sqlc.arg(expected_claim_version) + 1
   AND NOT EXISTS (SELECT 1 FROM transitioned)
LIMIT 1;

-- name: GetWorkerInstanceStateByResource :one
SELECT id, resource_id, worker_group_id, state, claim_version, current_epoch
  FROM worker_instances
 WHERE worker_group_id = sqlc.arg(worker_group_id)
   AND resource_id = sqlc.arg(resource_id)
 ORDER BY (state IN ('registering', 'active', 'draining')) DESC, created_at DESC
 LIMIT 1;

-- name: GetCapacityWorkerInstance :one
SELECT id, resource_id, worker_group_id, state, claim_version, current_epoch,
       supports_run, supports_build, draining_at, termination_ready_at, lost_at,
       created_at, updated_at
  FROM worker_instances
 WHERE id = sqlc.arg(worker_instance_id);

-- name: ListCapacityWorkerInstances :many
WITH current_instances AS (
    SELECT DISTINCT ON (worker_group_id, resource_id)
           id, resource_id, worker_group_id, state, claim_version, current_epoch,
           supports_run, supports_build, draining_at, termination_ready_at, lost_at,
           created_at, updated_at
      FROM worker_instances
     WHERE (sqlc.narg(worker_group_id)::text IS NULL OR worker_group_id = sqlc.narg(worker_group_id))
       AND (
           cardinality(sqlc.arg(resource_ids)::text[]) = 0
           OR resource_id = ANY(sqlc.arg(resource_ids)::text[])
       )
     ORDER BY worker_group_id, resource_id,
              (state IN ('registering', 'active', 'draining')) DESC,
              created_at DESC, id DESC
)
SELECT *
  FROM current_instances
 WHERE (
       cardinality(sqlc.arg(states)::text[]) = 0
       OR state = ANY(sqlc.arg(states)::text[])
   )
 ORDER BY worker_group_id, resource_id
 LIMIT sqlc.arg(row_limit);

-- name: MarkWorkerInstanceLost :one
WITH target AS (
    UPDATE worker_instances
       SET state = 'lost', claim_version = worker_instances.claim_version + 1,
           lost_at = COALESCE(worker_instances.lost_at, now()), updated_at = now()
     WHERE worker_instances.worker_group_id = sqlc.arg(worker_group_id)
       AND worker_instances.resource_id = sqlc.arg(resource_id)
       AND worker_instances.claim_version = sqlc.arg(expected_claim_version)
       AND worker_instances.state IN ('registering', 'active', 'draining')
    RETURNING worker_instances.id, worker_instances.resource_id,
              worker_instances.worker_group_id, worker_instances.state,
              worker_instances.claim_version, worker_instances.current_epoch
), revoked_credentials AS (
    UPDATE worker_instance_credentials
       SET revoked_at = COALESCE(revoked_at, now())
      FROM target
     WHERE worker_instance_credentials.worker_instance_id = target.id
       AND worker_instance_credentials.revoked_at IS NULL
    RETURNING worker_instance_credentials.id
), lost_mounts AS (
    UPDATE workspace_mounts
       SET state = 'lost', lost_at = now(), terminal_at = now(),
           terminal_reason_code = 'external_instance_drift', updated_at = now()
      FROM target
     WHERE workspace_mounts.worker_instance_id = target.id
       AND workspace_mounts.worker_epoch = target.current_epoch
       AND workspace_mounts.state IN ('mounting', 'mounted', 'unmounting')
    RETURNING workspace_mounts.id
), lost_runtimes AS (
    UPDATE runtime_instances
       SET observed_state = 'lost', observed_version = observed_version + 1,
           observed_at = now(), lost_at = now(), terminal_at = now(),
           terminal_reason_code = 'external_instance_drift',
           reserved_run_id = NULL, reserved_attempt_number = NULL,
           reserved_process_id = NULL, reserved_workspace_version_id = NULL,
           reservation_expires_at = NULL, updated_at = now()
      FROM target
     WHERE runtime_instances.worker_instance_id = target.id
       AND runtime_instances.worker_epoch = target.current_epoch
       AND runtime_instances.reclaimed_at IS NULL
       AND runtime_instances.observed_state IN ('allocated', 'preparing', 'ready', 'closing')
    RETURNING runtime_instances.id
), completed AS (
    SELECT target.id, target.resource_id, target.worker_group_id, target.state,
           target.claim_version, target.current_epoch, true AS transition_applied
      FROM target
     WHERE (SELECT count(*) FROM revoked_credentials) >= 0
       AND (SELECT count(*) FROM lost_mounts) >= 0
       AND (SELECT count(*) FROM lost_runtimes) >= 0
)
SELECT * FROM completed
UNION ALL
SELECT worker_instances.id, worker_instances.resource_id,
       worker_instances.worker_group_id, worker_instances.state,
       worker_instances.claim_version, worker_instances.current_epoch,
       false AS transition_applied
  FROM worker_instances
 WHERE worker_instances.worker_group_id = sqlc.arg(worker_group_id)
   AND worker_instances.resource_id = sqlc.arg(resource_id)
   AND worker_instances.state = 'lost'
   AND worker_instances.claim_version = sqlc.arg(expected_claim_version) + 1
   AND NOT EXISTS (
       SELECT 1
         FROM worker_instances AS current_worker
        WHERE current_worker.worker_group_id = worker_instances.worker_group_id
          AND current_worker.resource_id = worker_instances.resource_id
          AND current_worker.state IN ('registering', 'active', 'draining')
   )
   AND NOT EXISTS (SELECT 1 FROM completed)
LIMIT 1;

-- name: ActivateWorkerInstance :one
WITH activation AS (
    SELECT worker_instances.*,
           worker_instances.state AS prior_state
      FROM worker_instances
      JOIN worker_groups ON worker_groups.id = worker_instances.worker_group_id
     WHERE worker_instances.id = sqlc.arg(worker_instance_id)
       AND worker_instances.worker_group_id = sqlc.arg(worker_group_id)
       AND worker_instances.current_epoch = sqlc.arg(worker_epoch)
       AND btrim(sqlc.arg(supervisor_version)::text) <> ''
       AND worker_groups.state IN ('active', 'paused')
       AND (NOT sqlc.arg(supports_run)::boolean OR worker_groups.allows_run)
       AND (NOT sqlc.arg(supports_build)::boolean OR worker_groups.allows_build)
       AND sqlc.arg(epoch_cpu_millis)::bigint >= worker_groups.required_cpu_millis
       AND sqlc.arg(epoch_memory_bytes)::bigint >= worker_groups.required_memory_bytes
       AND sqlc.arg(epoch_guest_ephemeral_disk_bytes)::bigint >= worker_groups.required_guest_ephemeral_disk_bytes
       AND sqlc.arg(epoch_build_cache_bytes)::bigint >= worker_groups.required_build_cache_bytes
       AND sqlc.arg(epoch_artifact_cache_bytes)::bigint >= worker_groups.required_artifact_cache_bytes
       AND (
           NOT sqlc.arg(supports_run)::boolean
           OR sqlc.arg(max_vm_slots)::integer >= worker_groups.required_vm_slots
       )
       AND (
           NOT sqlc.arg(supports_build)::boolean
           OR sqlc.arg(max_build_executors)::integer >= worker_groups.required_build_executors
       )
       AND NOT EXISTS (
           SELECT 1 FROM runtime_instances
            WHERE runtime_instances.worker_instance_id = worker_instances.id
              AND runtime_instances.worker_epoch < worker_instances.current_epoch
              AND runtime_instances.reclaimed_at IS NULL
       )
       AND (
           worker_instances.state = 'registering'
           OR (
               worker_instances.state = 'active'
               AND worker_instances.runtime_identity_id = sqlc.arg(runtime_identity_id)::text
               AND worker_instances.supervisor_version = sqlc.arg(supervisor_version)
               AND worker_instances.supports_run = sqlc.arg(supports_run)
               AND worker_instances.supports_build = sqlc.arg(supports_build)
               AND worker_instances.substrate_format = sqlc.arg(substrate_format)
               AND worker_instances.substrate_contract = sqlc.arg(substrate_contract)
               AND worker_instances.epoch_cpu_millis = sqlc.arg(epoch_cpu_millis)
               AND worker_instances.epoch_memory_bytes = sqlc.arg(epoch_memory_bytes)
               AND worker_instances.epoch_guest_ephemeral_disk_bytes = sqlc.arg(epoch_guest_ephemeral_disk_bytes)
               AND worker_instances.epoch_build_cache_bytes = sqlc.arg(epoch_build_cache_bytes)
               AND worker_instances.epoch_artifact_cache_bytes = sqlc.arg(epoch_artifact_cache_bytes)
               AND worker_instances.epoch_hugepages_bytes = sqlc.arg(epoch_hugepages_bytes)
               AND worker_instances.epoch_checkpoint_bytes = sqlc.arg(epoch_checkpoint_bytes)
               AND worker_instances.per_vm_cpu_millis = sqlc.arg(per_vm_cpu_millis)
               AND worker_instances.per_vm_memory_bytes = sqlc.arg(per_vm_memory_bytes)
               AND worker_instances.per_vm_guest_ephemeral_disk_bytes = sqlc.arg(per_vm_guest_ephemeral_disk_bytes)
               AND worker_instances.max_vm_slots = sqlc.arg(max_vm_slots)
               AND worker_instances.max_run_consumers = sqlc.arg(max_run_consumers)
               AND worker_instances.max_build_executors = sqlc.arg(max_build_executors)
               AND worker_instances.max_runtime_starts = sqlc.arg(max_runtime_starts)
           )
       )
     FOR UPDATE OF worker_instances
), runtime AS (
    INSERT INTO runtime_identities (
        id, runtime_arch, vm_runtime_contract, kernel_digest, initramfs_digest,
		rootfs_digest, last_seen_at
    )
    SELECT sqlc.arg(runtime_identity_id), sqlc.arg(runtime_arch), sqlc.arg(vm_runtime_contract),
           sqlc.arg(kernel_digest), sqlc.arg(initramfs_digest), sqlc.arg(rootfs_digest),
		   now()
      FROM activation
    ON CONFLICT (id) DO UPDATE SET last_seen_at = now()
     WHERE runtime_identities.runtime_arch = EXCLUDED.runtime_arch
       AND runtime_identities.vm_runtime_contract = EXCLUDED.vm_runtime_contract
       AND runtime_identities.kernel_digest = EXCLUDED.kernel_digest
       AND runtime_identities.initramfs_digest = EXCLUDED.initramfs_digest
	   AND runtime_identities.rootfs_digest = EXCLUDED.rootfs_digest
    RETURNING id
), activated AS (
    UPDATE worker_instances
       SET state = 'active', supervisor_version = sqlc.arg(supervisor_version),
           supports_run = sqlc.arg(supports_run), supports_build = sqlc.arg(supports_build),
           runtime_identity_id = runtime.id,
           substrate_format = sqlc.arg(substrate_format),
           substrate_contract = sqlc.arg(substrate_contract),
           epoch_cpu_millis = sqlc.arg(epoch_cpu_millis),
           epoch_memory_bytes = sqlc.arg(epoch_memory_bytes),
           epoch_guest_ephemeral_disk_bytes = sqlc.arg(epoch_guest_ephemeral_disk_bytes),
           epoch_build_cache_bytes = sqlc.arg(epoch_build_cache_bytes),
           epoch_artifact_cache_bytes = sqlc.arg(epoch_artifact_cache_bytes),
           epoch_hugepages_bytes = sqlc.arg(epoch_hugepages_bytes),
           epoch_checkpoint_bytes = sqlc.arg(epoch_checkpoint_bytes),
           per_vm_cpu_millis = sqlc.arg(per_vm_cpu_millis),
           per_vm_memory_bytes = sqlc.arg(per_vm_memory_bytes),
           per_vm_guest_ephemeral_disk_bytes = sqlc.arg(per_vm_guest_ephemeral_disk_bytes),
           max_vm_slots = sqlc.arg(max_vm_slots), max_run_consumers = sqlc.arg(max_run_consumers),
           max_build_executors = sqlc.arg(max_build_executors),
           max_runtime_starts = sqlc.arg(max_runtime_starts),
           activated_at = COALESCE(worker_instances.activated_at, now()),
           updated_at = now()
      FROM runtime, activation
     WHERE worker_instances.id = activation.id
    RETURNING worker_instances.*
), observation AS (
    INSERT INTO worker_observations (
        worker_instance_id, worker_epoch, cpu_pressure_bps, memory_pressure_bps,
        guest_ephemeral_disk_pressure_bps, build_cache_pressure_bps,
        artifact_cache_pressure_bps, checkpoint_pressure_bps, quarantined_resource_count,
        run_queue_depth, build_queue_depth, runtime_start_queue_depth, health_details,
        run_paused_reason, build_paused_reason, runtime_paused_reason, observed_at
    )
    SELECT activated.id, activated.current_epoch, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
           '{}'::jsonb,
           CASE WHEN activated.supports_run THEN 'datapath_unverified' END,
           CASE WHEN activated.supports_build THEN 'datapath_unverified' END,
           CASE WHEN activated.supports_run THEN 'datapath_unverified' END,
           now()
      FROM activated
    ON CONFLICT (worker_instance_id, worker_epoch) DO NOTHING
    RETURNING worker_instance_id
)
SELECT activated.*
  FROM activated
 WHERE EXISTS (SELECT 1 FROM observation)
    OR EXISTS (SELECT 1 FROM activation WHERE activation.prior_state = 'active');

-- name: RecordWorkerObservation :one
WITH target AS (
    SELECT worker_instances.id, worker_instances.current_epoch
      FROM worker_instances
     WHERE worker_instances.id = sqlc.arg(worker_instance_id)
       AND worker_instances.worker_group_id = sqlc.arg(worker_group_id)
       AND worker_instances.current_epoch = sqlc.arg(worker_epoch)
       AND worker_instances.state IN ('active','draining')
     FOR UPDATE OF worker_instances
)
INSERT INTO worker_observations (
    worker_instance_id, worker_epoch, cpu_pressure_bps, memory_pressure_bps,
    guest_ephemeral_disk_pressure_bps, build_cache_pressure_bps,
    artifact_cache_pressure_bps, checkpoint_pressure_bps, quarantined_resource_count,
    run_queue_depth, build_queue_depth, runtime_start_queue_depth,
    run_paused_reason, build_paused_reason, runtime_paused_reason,
    health_details, observed_at
)
SELECT target.id, target.current_epoch,
       sqlc.arg(cpu_pressure_bps), sqlc.arg(memory_pressure_bps),
       sqlc.arg(guest_ephemeral_disk_pressure_bps),
       sqlc.arg(build_cache_pressure_bps), sqlc.arg(artifact_cache_pressure_bps),
       sqlc.arg(checkpoint_pressure_bps), sqlc.arg(quarantined_resource_count),
       sqlc.arg(run_queue_depth), sqlc.arg(build_queue_depth),
       sqlc.arg(runtime_start_queue_depth), sqlc.narg(run_paused_reason),
       sqlc.narg(build_paused_reason), sqlc.narg(runtime_paused_reason),
       sqlc.arg(health_details), sqlc.arg(observed_at)
  FROM target
ON CONFLICT (worker_instance_id, worker_epoch) DO UPDATE
   SET cpu_pressure_bps = EXCLUDED.cpu_pressure_bps,
       memory_pressure_bps = EXCLUDED.memory_pressure_bps,
       guest_ephemeral_disk_pressure_bps = EXCLUDED.guest_ephemeral_disk_pressure_bps,
       build_cache_pressure_bps = EXCLUDED.build_cache_pressure_bps,
       artifact_cache_pressure_bps = EXCLUDED.artifact_cache_pressure_bps,
       checkpoint_pressure_bps = EXCLUDED.checkpoint_pressure_bps,
       quarantined_resource_count = EXCLUDED.quarantined_resource_count,
       run_queue_depth = EXCLUDED.run_queue_depth,
       build_queue_depth = EXCLUDED.build_queue_depth,
       runtime_start_queue_depth = EXCLUDED.runtime_start_queue_depth,
       run_paused_reason = EXCLUDED.run_paused_reason,
       build_paused_reason = EXCLUDED.build_paused_reason,
       runtime_paused_reason = EXCLUDED.runtime_paused_reason,
       health_details = EXCLUDED.health_details,
       observed_at = EXCLUDED.observed_at,
       updated_at = now()
RETURNING *;

-- name: CompleteWorkerStartupRecovery :one
WITH target AS (
    SELECT worker_instances.id, worker_instances.worker_group_id, worker_instances.current_epoch
      FROM worker_instances
     WHERE worker_instances.id = sqlc.arg(worker_instance_id)
       AND worker_instances.worker_group_id = sqlc.arg(worker_group_id)
       AND worker_instances.current_epoch = sqlc.arg(worker_epoch)
       AND NOT EXISTS (
           SELECT 1
             FROM runtime_instances
             JOIN run_leases
               ON run_leases.runtime_instance_id = runtime_instances.id
              AND run_leases.state IN ('assigned', 'starting', 'running')
            WHERE runtime_instances.worker_instance_id = worker_instances.id
              AND runtime_instances.worker_epoch < worker_instances.current_epoch
       )
       AND (
           worker_instances.state = 'registering'
           OR (
               worker_instances.state = 'draining'
               AND NOT worker_instances.supports_run
               AND NOT worker_instances.supports_build
           )
       )
     FOR UPDATE
), quarantined AS (
    SELECT value::uuid AS id
      FROM jsonb_array_elements_text(sqlc.arg(recovery_evidence)::jsonb -> 'quarantined') AS value
), reclaimed_runtimes AS (
    UPDATE runtime_instances
       SET observed_state = CASE WHEN observed_state IN ('closed','failed','lost') THEN observed_state ELSE 'lost' END,
           observed_version = observed_version + 1,
           observed_at = now(),
           lost_at = CASE WHEN observed_state IN ('closed','failed','lost') THEN lost_at ELSE now() END,
           terminal_at = COALESCE(terminal_at, now()),
           terminal_reason_code = COALESCE(terminal_reason_code, 'worker_startup_reclaimed'),
           reclaimed_at = now(),
           reclaim_evidence = jsonb_build_object(
               'method', 'host_reconciled',
               'completed_at', sqlc.arg(recovery_evidence)::jsonb ->> 'observed_at'
           ),
           reserved_run_id = NULL, reserved_attempt_number = NULL,
           reserved_process_id = NULL, reserved_workspace_version_id = NULL,
           reservation_expires_at = NULL, updated_at = now()
      FROM target
     WHERE runtime_instances.worker_instance_id = target.id
       AND runtime_instances.worker_epoch < target.current_epoch
       AND runtime_instances.reclaimed_at IS NULL
       AND runtime_instances.id NOT IN (SELECT id FROM quarantined)
       AND NOT EXISTS (
           SELECT 1
             FROM run_leases
            WHERE run_leases.runtime_instance_id = runtime_instances.id
              AND run_leases.state IN ('assigned', 'starting', 'running')
       )
    RETURNING runtime_instances.id
)
UPDATE worker_instances
   SET updated_at = now()
  FROM target
 WHERE worker_instances.id = target.id
   AND (SELECT count(*) FROM reclaimed_runtimes) >= 0
RETURNING worker_instances.*;
