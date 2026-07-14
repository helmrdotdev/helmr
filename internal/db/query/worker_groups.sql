-- name: ReconcileWorkerGroup :one
WITH desired_group AS (
    INSERT INTO worker_groups (
        id, region_id, name, description, state, enrollment_policy_fingerprint,
        allowed_attestation_fingerprints, launch_attestation_fingerprint,
        allows_run, allows_build, protocol_version,
        required_cpu_millis, required_memory_bytes, required_workload_disk_bytes,
        required_scratch_bytes, required_build_cache_bytes, required_artifact_cache_bytes,
        required_vm_slots, required_build_executors
    ) VALUES (
        sqlc.arg(id), sqlc.arg(region_id), sqlc.arg(name), sqlc.arg(description),
        'active', sqlc.arg(enrollment_policy_fingerprint), sqlc.arg(allowed_attestation_fingerprints),
        sqlc.narg(launch_attestation_fingerprint),
        sqlc.arg(allows_run), sqlc.arg(allows_build), sqlc.arg(protocol_version),
        sqlc.arg(required_cpu_millis), sqlc.arg(required_memory_bytes),
        sqlc.arg(required_workload_disk_bytes), sqlc.arg(required_scratch_bytes),
        sqlc.arg(required_build_cache_bytes), sqlc.arg(required_artifact_cache_bytes),
        sqlc.arg(required_vm_slots), sqlc.arg(required_build_executors)
    )
    ON CONFLICT (id) DO UPDATE
       SET claim_version = CASE
               WHEN worker_groups.enrollment_policy_fingerprint IS DISTINCT FROM EXCLUDED.enrollment_policy_fingerprint
                 OR worker_groups.protocol_version IS DISTINCT FROM EXCLUDED.protocol_version
                 OR worker_groups.required_cpu_millis IS DISTINCT FROM EXCLUDED.required_cpu_millis
                 OR worker_groups.required_memory_bytes IS DISTINCT FROM EXCLUDED.required_memory_bytes
                 OR worker_groups.required_workload_disk_bytes IS DISTINCT FROM EXCLUDED.required_workload_disk_bytes
                 OR worker_groups.required_scratch_bytes IS DISTINCT FROM EXCLUDED.required_scratch_bytes
                 OR worker_groups.required_build_cache_bytes IS DISTINCT FROM EXCLUDED.required_build_cache_bytes
                 OR worker_groups.required_artifact_cache_bytes IS DISTINCT FROM EXCLUDED.required_artifact_cache_bytes
                 OR worker_groups.required_vm_slots IS DISTINCT FROM EXCLUDED.required_vm_slots
                 OR worker_groups.required_build_executors IS DISTINCT FROM EXCLUDED.required_build_executors
               THEN worker_groups.claim_version + 1 ELSE worker_groups.claim_version END,
           region_id = EXCLUDED.region_id, name = EXCLUDED.name,
           description = EXCLUDED.description, state = 'active',
           enrollment_policy_fingerprint = EXCLUDED.enrollment_policy_fingerprint,
           allowed_attestation_fingerprints = EXCLUDED.allowed_attestation_fingerprints,
           launch_attestation_fingerprint = EXCLUDED.launch_attestation_fingerprint,
           allows_run = EXCLUDED.allows_run, allows_build = EXCLUDED.allows_build,
           required_cpu_millis = EXCLUDED.required_cpu_millis,
           required_memory_bytes = EXCLUDED.required_memory_bytes,
           required_workload_disk_bytes = EXCLUDED.required_workload_disk_bytes,
           required_scratch_bytes = EXCLUDED.required_scratch_bytes,
           required_build_cache_bytes = EXCLUDED.required_build_cache_bytes,
           required_artifact_cache_bytes = EXCLUDED.required_artifact_cache_bytes,
           required_vm_slots = EXCLUDED.required_vm_slots,
           required_build_executors = EXCLUDED.required_build_executors,
           protocol_version = EXCLUDED.protocol_version
    RETURNING *
), lost_workers AS (
    UPDATE worker_instances
       SET state = (CASE WHEN current_epoch IS NULL THEN 'disabled' ELSE 'lost' END)::worker_instance_state,
           claim_version = worker_instances.claim_version + 1,
           disabled_at = CASE WHEN current_epoch IS NULL THEN COALESCE(disabled_at, now()) ELSE disabled_at END,
           lost_at = CASE WHEN current_epoch IS NULL THEN lost_at ELSE COALESCE(lost_at, now()) END,
           updated_at = now()
     FROM desired_group
     WHERE worker_instances.worker_group_id = desired_group.id
       AND worker_instances.state IN ('registering', 'active', 'draining')
       AND (
           NOT (worker_instances.attestation_fingerprint = ANY(sqlc.arg(allowed_attestation_fingerprints)::text[]))
           OR (worker_instances.supports_run AND NOT desired_group.allows_run)
           OR (worker_instances.supports_build AND NOT desired_group.allows_build)
           OR (worker_instances.state <> 'registering' AND (
               worker_instances.certified_cpu_millis < desired_group.required_cpu_millis
               OR worker_instances.certified_memory_bytes < desired_group.required_memory_bytes
               OR worker_instances.certified_workload_disk_bytes < desired_group.required_workload_disk_bytes
               OR worker_instances.certified_scratch_bytes < desired_group.required_scratch_bytes
               OR worker_instances.certified_build_cache_bytes < desired_group.required_build_cache_bytes
               OR worker_instances.certified_artifact_cache_bytes < desired_group.required_artifact_cache_bytes
               OR worker_instances.max_vm_slots < desired_group.required_vm_slots
               OR worker_instances.max_build_executors < desired_group.required_build_executors
           ))
       )
    RETURNING worker_instances.id, worker_instances.current_epoch
), revoked AS (
    UPDATE worker_instance_credentials
       SET revoked_at = COALESCE(revoked_at, now())
     WHERE worker_instance_credentials.worker_instance_id IN (SELECT id FROM lost_workers)
       AND worker_instance_credentials.revoked_at IS NULL
    RETURNING worker_instance_credentials.id
), lost_mounts AS (
    UPDATE workspace_mounts
       SET state = 'lost', lost_at = now(), terminal_at = now(),
           terminal_reason_code = 'enrollment_policy_changed', updated_at = now()
     WHERE workspace_mounts.worker_instance_id IN (SELECT id FROM lost_workers)
       AND workspace_mounts.state IN ('mounting', 'mounted', 'unmounting')
    RETURNING workspace_mounts.id
), lost_runtimes AS (
    UPDATE runtime_instances
       SET observed_state = 'lost', observed_version = observed_version + 1,
           observed_at = now(), lost_at = now(), terminal_at = now(),
           terminal_reason_code = 'enrollment_policy_changed', updated_at = now()
     WHERE runtime_instances.worker_instance_id IN (SELECT id FROM lost_workers)
       AND runtime_instances.reclaimed_at IS NULL
       AND runtime_instances.observed_state IN ('allocated', 'preparing', 'ready', 'closing')
    RETURNING runtime_instances.id
), lost_slots AS (
    UPDATE worker_network_slots
       SET state = 'lost', generation = generation + 1, lost_at = now(),
           state_reason_code = 'enrollment_policy_changed', updated_at = now()
     WHERE worker_network_slots.worker_instance_id IN (SELECT id FROM lost_workers)
       AND worker_network_slots.state IN ('assigned', 'bound', 'reclaiming', 'quarantined')
    RETURNING worker_network_slots.id
)
SELECT desired_group.* FROM desired_group
 WHERE (SELECT count(*) FROM revoked) >= 0
   AND (SELECT count(*) FROM lost_mounts) >= 0
   AND (SELECT count(*) FROM lost_runtimes) >= 0
   AND (SELECT count(*) FROM lost_slots) >= 0;

-- name: LockWorkerGroupsForReconciliation :many
SELECT worker_groups.id
  FROM worker_groups
 WHERE worker_groups.region_id = sqlc.arg(region_id)
   AND worker_groups.id = ANY(sqlc.arg(desired_ids)::text[])
 ORDER BY worker_groups.id
 FOR UPDATE OF worker_groups;

-- name: LockAbsentWorkerGroups :many
SELECT worker_groups.id
  FROM worker_groups
 WHERE worker_groups.region_id = sqlc.arg(region_id)
   AND worker_groups.state <> 'disabled'
   AND NOT (worker_groups.id = ANY(sqlc.arg(desired_ids)::text[]))
 ORDER BY worker_groups.id
 FOR UPDATE OF worker_groups;

-- name: DisableAbsentWorkerGroups :many
WITH disabled_groups AS (
    UPDATE worker_groups
       SET state = 'disabled', claim_version = claim_version + 1, updated_at = now()
     WHERE worker_groups.region_id = sqlc.arg(region_id)
       AND worker_groups.state <> 'disabled'
       AND NOT (worker_groups.id = ANY(sqlc.arg(desired_ids)::text[]))
       AND NOT EXISTS (
           SELECT 1 FROM worker_instances
            WHERE worker_instances.worker_group_id = worker_groups.id
              AND worker_instances.state IN ('registering', 'active', 'draining', 'disabled', 'lost')
              AND worker_instances.provider_terminated_at IS NULL
       )
    RETURNING *
), revoked AS (
    UPDATE worker_instance_credentials
       SET revoked_at = COALESCE(revoked_at, now())
     WHERE worker_group_id IN (SELECT id FROM disabled_groups)
       AND revoked_at IS NULL
    RETURNING worker_instance_credentials.id
), lost_workers AS (
    UPDATE worker_instances
       SET state = (CASE WHEN current_epoch IS NULL THEN 'disabled' ELSE 'lost' END)::worker_instance_state,
           claim_version = claim_version + 1,
           disabled_at = CASE WHEN current_epoch IS NULL THEN COALESCE(disabled_at, now()) ELSE disabled_at END,
           lost_at = CASE WHEN current_epoch IS NULL THEN lost_at ELSE COALESCE(lost_at, now()) END,
           updated_at = now()
     WHERE worker_group_id IN (SELECT id FROM disabled_groups)
       AND state IN ('registering', 'active', 'draining')
    RETURNING worker_instances.id
), lost_mounts AS (
    UPDATE workspace_mounts
       SET state = 'lost', lost_at = now(), terminal_at = now(),
           terminal_reason_code = 'worker_group_removed', updated_at = now()
     WHERE worker_instance_id IN (SELECT id FROM lost_workers)
       AND state IN ('mounting', 'mounted', 'unmounting')
    RETURNING workspace_mounts.id
), lost_runtimes AS (
    UPDATE runtime_instances
       SET observed_state = 'lost', observed_version = observed_version + 1,
           observed_at = now(), lost_at = now(), terminal_at = now(),
           terminal_reason_code = 'worker_group_removed', updated_at = now()
     WHERE worker_instance_id IN (SELECT id FROM lost_workers)
       AND reclaimed_at IS NULL
       AND observed_state IN ('allocated', 'preparing', 'ready', 'closing')
    RETURNING runtime_instances.id
), lost_slots AS (
    UPDATE worker_network_slots
       SET state = 'lost', generation = generation + 1, lost_at = now(),
           state_reason_code = 'worker_group_removed', updated_at = now()
     WHERE worker_instance_id IN (SELECT id FROM lost_workers)
       AND state IN ('assigned', 'bound', 'reclaiming', 'quarantined')
    RETURNING worker_network_slots.id
)
SELECT disabled_groups.* FROM disabled_groups
 WHERE (SELECT count(*) FROM revoked) >= 0
   AND (SELECT count(*) FROM lost_mounts) >= 0
   AND (SELECT count(*) FROM lost_runtimes) >= 0
   AND (SELECT count(*) FROM lost_slots) >= 0
 ORDER BY disabled_groups.id;

-- name: ListLiveAbsentWorkerGroupIDs :many
SELECT worker_groups.id
 FROM worker_groups
 WHERE worker_groups.region_id = sqlc.arg(region_id)
   AND NOT (worker_groups.id = ANY(sqlc.arg(desired_ids)::text[]))
   AND EXISTS (
       SELECT 1 FROM worker_instances
        WHERE worker_instances.worker_group_id = worker_groups.id
          AND worker_instances.state IN ('registering', 'active', 'draining', 'disabled', 'lost')
          AND worker_instances.provider_terminated_at IS NULL
   )
 ORDER BY worker_groups.id;

-- name: ListWorkerGroups :many
SELECT *
  FROM worker_groups
 WHERE region_id = sqlc.arg(region_id)
 ORDER BY name ASC
 LIMIT sqlc.arg(row_limit);

-- name: GetControlWorkerGroupReadiness :one
SELECT id AS worker_group_id,
       state,
       state = 'active' AS routable
  FROM worker_groups
 WHERE id = sqlc.arg(worker_group_id);

-- name: CertifyWorkerInstance :one
WITH runtime AS (
    INSERT INTO runtime_identities (
        id, runtime_arch, runtime_abi, kernel_digest, initramfs_digest,
        rootfs_digest, cni_profile, last_seen_at
    ) VALUES (
        sqlc.arg(runtime_identity_id), sqlc.arg(runtime_arch), sqlc.arg(runtime_abi),
        sqlc.arg(kernel_digest), sqlc.arg(initramfs_digest), sqlc.arg(rootfs_digest),
        sqlc.arg(cni_profile), now()
    )
    ON CONFLICT (id) DO UPDATE SET last_seen_at = now()
     WHERE runtime_identities.runtime_arch = EXCLUDED.runtime_arch
       AND runtime_identities.runtime_abi = EXCLUDED.runtime_abi
       AND runtime_identities.kernel_digest = EXCLUDED.kernel_digest
       AND runtime_identities.initramfs_digest = EXCLUDED.initramfs_digest
       AND runtime_identities.rootfs_digest = EXCLUDED.rootfs_digest
       AND runtime_identities.cni_profile = EXCLUDED.cni_profile
    RETURNING id
), activation AS (
    SELECT worker_instances.id, worker_instances.worker_group_id,
           worker_instances.current_epoch
      FROM worker_instances
      JOIN worker_groups ON worker_groups.id = worker_instances.worker_group_id
     WHERE worker_instances.id = sqlc.arg(worker_instance_id)
       AND worker_instances.worker_group_id = sqlc.arg(worker_group_id)
       AND worker_instances.current_epoch = sqlc.arg(worker_epoch)
       AND worker_instances.state = 'registering'
       AND (NOT sqlc.arg(supports_run)::boolean OR worker_groups.allows_run)
       AND (NOT sqlc.arg(supports_build)::boolean OR worker_groups.allows_build)
       AND sqlc.arg(certified_cpu_millis)::bigint >= worker_groups.required_cpu_millis
       AND sqlc.arg(certified_memory_bytes)::bigint >= worker_groups.required_memory_bytes
       AND sqlc.arg(certified_workload_disk_bytes)::bigint >= worker_groups.required_workload_disk_bytes
       AND sqlc.arg(certified_scratch_bytes)::bigint >= worker_groups.required_scratch_bytes
       AND sqlc.arg(certified_build_cache_bytes)::bigint >= worker_groups.required_build_cache_bytes
       AND sqlc.arg(certified_artifact_cache_bytes)::bigint >= worker_groups.required_artifact_cache_bytes
       AND sqlc.arg(max_vm_slots)::integer >= worker_groups.required_vm_slots
       AND sqlc.arg(max_build_executors)::integer >= worker_groups.required_build_executors
       AND (NOT sqlc.arg(supports_run)::boolean
            OR worker_instances.startup_inventory_epoch = worker_instances.current_epoch)
     FOR UPDATE
), slots AS (
    INSERT INTO worker_network_slots (
        id, worker_group_id, worker_instance_id, worker_epoch, slot_name,
        generation, state
    )
    SELECT (
               substr(md5(activation.id::text || ':' || activation.current_epoch::text || ':' || slot.ordinal::text), 1, 8) || '-' ||
               substr(md5(activation.id::text || ':' || activation.current_epoch::text || ':' || slot.ordinal::text), 9, 4) || '-' ||
               substr(md5(activation.id::text || ':' || activation.current_epoch::text || ':' || slot.ordinal::text), 13, 4) || '-' ||
               substr(md5(activation.id::text || ':' || activation.current_epoch::text || ':' || slot.ordinal::text), 17, 4) || '-' ||
               substr(md5(activation.id::text || ':' || activation.current_epoch::text || ':' || slot.ordinal::text), 21, 12)
           )::uuid,
           activation.worker_group_id, activation.id, activation.current_epoch,
           'vm-' || lpad(slot.ordinal::text, 4, '0'), 1, 'available'
      FROM activation
      CROSS JOIN LATERAL generate_series(1, sqlc.arg(max_vm_slots)::integer) AS slot(ordinal)
     WHERE sqlc.arg(supports_run)::boolean
    RETURNING worker_instance_id
), certified AS (
    UPDATE worker_instances
       SET state = 'active', protocol_version = sqlc.arg(protocol_version),
           supervisor_version = sqlc.arg(supervisor_version),
           supports_run = sqlc.arg(supports_run), supports_build = sqlc.arg(supports_build),
           runtime_identity_id = runtime.id,
           certified_cpu_millis = sqlc.arg(certified_cpu_millis),
           certified_memory_bytes = sqlc.arg(certified_memory_bytes),
           certified_workload_disk_bytes = sqlc.arg(certified_workload_disk_bytes),
           certified_scratch_bytes = sqlc.arg(certified_scratch_bytes),
           certified_build_cache_bytes = sqlc.arg(certified_build_cache_bytes),
           certified_artifact_cache_bytes = sqlc.arg(certified_artifact_cache_bytes),
           certified_hugepages_bytes = sqlc.arg(certified_hugepages_bytes),
           certified_checkpoint_bytes = sqlc.arg(certified_checkpoint_bytes),
           per_vm_cpu_millis = sqlc.arg(per_vm_cpu_millis),
           per_vm_memory_bytes = sqlc.arg(per_vm_memory_bytes),
           per_vm_workload_disk_bytes = sqlc.arg(per_vm_workload_disk_bytes),
           per_vm_scratch_bytes = sqlc.arg(per_vm_scratch_bytes),
           max_vm_slots = sqlc.arg(max_vm_slots), max_run_consumers = sqlc.arg(max_run_consumers),
           max_build_executors = sqlc.arg(max_build_executors),
           max_runtime_starts = sqlc.arg(max_runtime_starts),
           certification_profile = sqlc.arg(certification_profile),
           certification_fingerprint = sqlc.arg(certification_fingerprint),
           certified_at = now(), activated_at = now(), updated_at = now()
      FROM runtime, activation
     WHERE worker_instances.id = activation.id
       AND (NOT sqlc.arg(supports_run)::boolean
            OR (SELECT count(*) FROM slots) = sqlc.arg(max_vm_slots)::integer)
    RETURNING worker_instances.*
), observation AS (
    INSERT INTO worker_observations (
        worker_instance_id, worker_epoch, cpu_pressure_bps, memory_pressure_bps,
        workload_disk_pressure_bps, scratch_pressure_bps, build_cache_pressure_bps,
        artifact_cache_pressure_bps, checkpoint_pressure_bps, leaked_slot_count,
        run_queue_depth, build_queue_depth, runtime_start_queue_depth, health_details,
        observed_at
    )
    SELECT certified.id, certified.current_epoch, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
           '{}'::jsonb, now() FROM certified
    ON CONFLICT (worker_instance_id, worker_epoch) DO NOTHING
    RETURNING worker_instance_id
)
SELECT certified.*
  FROM certified
  JOIN observation ON observation.worker_instance_id = certified.id;

-- name: RecordWorkerObservation :one
WITH target AS (
    SELECT worker_instances.id, worker_instances.current_epoch
      FROM worker_instances
     WHERE worker_instances.id = sqlc.arg(worker_instance_id)
       AND worker_instances.worker_group_id = sqlc.arg(worker_group_id)
       AND worker_instances.current_epoch = sqlc.arg(worker_epoch)
       AND worker_instances.state IN ('active','draining')
     FOR UPDATE
)
INSERT INTO worker_observations (
    worker_instance_id, worker_epoch, cpu_pressure_bps, memory_pressure_bps,
    workload_disk_pressure_bps, scratch_pressure_bps, build_cache_pressure_bps,
    artifact_cache_pressure_bps, checkpoint_pressure_bps, leaked_slot_count,
    run_queue_depth, build_queue_depth, runtime_start_queue_depth,
    run_paused_reason, build_paused_reason, runtime_paused_reason,
    health_details, observed_at
)
SELECT target.id, target.current_epoch,
       sqlc.arg(cpu_pressure_bps), sqlc.arg(memory_pressure_bps),
       sqlc.arg(workload_disk_pressure_bps), sqlc.arg(scratch_pressure_bps),
       sqlc.arg(build_cache_pressure_bps), sqlc.arg(artifact_cache_pressure_bps),
       sqlc.arg(checkpoint_pressure_bps), sqlc.arg(leaked_slot_count),
       sqlc.arg(run_queue_depth), sqlc.arg(build_queue_depth),
       sqlc.arg(runtime_start_queue_depth), sqlc.narg(run_paused_reason),
       sqlc.narg(build_paused_reason), sqlc.narg(runtime_paused_reason),
       sqlc.arg(health_details), sqlc.arg(observed_at)
  FROM target
ON CONFLICT (worker_instance_id, worker_epoch) DO UPDATE
   SET cpu_pressure_bps = EXCLUDED.cpu_pressure_bps,
       memory_pressure_bps = EXCLUDED.memory_pressure_bps,
       workload_disk_pressure_bps = EXCLUDED.workload_disk_pressure_bps,
       scratch_pressure_bps = EXCLUDED.scratch_pressure_bps,
       build_cache_pressure_bps = EXCLUDED.build_cache_pressure_bps,
       artifact_cache_pressure_bps = EXCLUDED.artifact_cache_pressure_bps,
       checkpoint_pressure_bps = EXCLUDED.checkpoint_pressure_bps,
       leaked_slot_count = EXCLUDED.leaked_slot_count,
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

-- name: RenewWorkerCertification :one
UPDATE worker_instances
   SET certified_at = now(), updated_at = now()
  FROM worker_groups
 WHERE worker_instances.id = sqlc.arg(worker_instance_id)
   AND worker_groups.id = worker_instances.worker_group_id
	AND worker_instances.worker_group_id = sqlc.arg(worker_group_id)
	AND worker_instances.current_epoch = sqlc.arg(worker_epoch)
	AND worker_instances.state = 'active'
	AND worker_instances.runtime_identity_id = sqlc.arg(runtime_identity_id)::text
	AND worker_instances.protocol_version = sqlc.arg(protocol_version)
	AND worker_instances.supports_run = sqlc.arg(supports_run)
	AND worker_instances.supports_build = sqlc.arg(supports_build)
	AND worker_instances.certified_cpu_millis = sqlc.arg(certified_cpu_millis)
	AND worker_instances.certified_memory_bytes = sqlc.arg(certified_memory_bytes)
	AND worker_instances.certified_workload_disk_bytes = sqlc.arg(certified_workload_disk_bytes)
	AND worker_instances.certified_scratch_bytes = sqlc.arg(certified_scratch_bytes)
	AND worker_instances.certified_build_cache_bytes = sqlc.arg(certified_build_cache_bytes)
	AND worker_instances.certified_artifact_cache_bytes = sqlc.arg(certified_artifact_cache_bytes)
	AND worker_instances.certified_hugepages_bytes = sqlc.arg(certified_hugepages_bytes)
	AND worker_instances.certified_checkpoint_bytes = sqlc.arg(certified_checkpoint_bytes)
	AND worker_instances.per_vm_cpu_millis = sqlc.arg(per_vm_cpu_millis)
	AND worker_instances.per_vm_memory_bytes = sqlc.arg(per_vm_memory_bytes)
	AND worker_instances.per_vm_workload_disk_bytes = sqlc.arg(per_vm_workload_disk_bytes)
	AND worker_instances.per_vm_scratch_bytes = sqlc.arg(per_vm_scratch_bytes)
	AND worker_instances.max_vm_slots = sqlc.arg(max_vm_slots)
	AND worker_instances.max_run_consumers = sqlc.arg(max_vm_slots)
	AND worker_instances.max_build_executors = sqlc.arg(max_build_executors)
	AND worker_instances.max_runtime_starts = sqlc.arg(max_runtime_starts)
	AND worker_instances.certified_cpu_millis >= worker_groups.required_cpu_millis
	AND worker_instances.certified_memory_bytes >= worker_groups.required_memory_bytes
	AND worker_instances.certified_workload_disk_bytes >= worker_groups.required_workload_disk_bytes
	AND worker_instances.certified_scratch_bytes >= worker_groups.required_scratch_bytes
	AND worker_instances.certified_build_cache_bytes >= worker_groups.required_build_cache_bytes
	AND worker_instances.certified_artifact_cache_bytes >= worker_groups.required_artifact_cache_bytes
	AND worker_instances.max_vm_slots >= worker_groups.required_vm_slots
	AND worker_instances.max_build_executors >= worker_groups.required_build_executors
RETURNING *;

-- name: RecordWorkerStartupRecovery :one
WITH target AS (
    SELECT worker_instances.id, worker_instances.worker_group_id, worker_instances.current_epoch
      FROM worker_instances
     WHERE worker_instances.id = sqlc.arg(worker_instance_id)
       AND worker_instances.worker_group_id = sqlc.arg(worker_group_id)
       AND worker_instances.current_epoch = sqlc.arg(worker_epoch)
       AND worker_instances.state = 'registering'
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
           terminal_reason_code = COALESCE(terminal_reason_code, 'startup_inventory_reclaimed'),
           reclaimed_at = now(), updated_at = now()
      FROM target
     WHERE runtime_instances.worker_instance_id = target.id
       AND runtime_instances.worker_epoch < target.current_epoch
       AND runtime_instances.reclaimed_at IS NULL
       AND runtime_instances.id NOT IN (SELECT id FROM quarantined)
    RETURNING runtime_instances.id
), reclaimed_slots AS (
    UPDATE worker_network_slots
       SET state = 'lost', generation = generation + 1,
           runtime_instance_id = NULL, host_interface_name = NULL,
           guest_address = NULL, gateway_address = NULL, subnet = NULL,
           tap_name = NULL, netns_name = NULL, guest_mac = NULL,
           reclaiming_at = NULL, quarantined_at = NULL, lost_at = now(),
           reclaimed_at = now(), reclaim_evidence = sqlc.arg(recovery_evidence)::jsonb,
           state_reason_code = 'startup_inventory_reclaimed', state_error = NULL, updated_at = now()
      FROM target
     WHERE worker_network_slots.worker_instance_id = target.id
       AND worker_network_slots.worker_epoch < target.current_epoch
       AND NOT EXISTS (
           SELECT 1 FROM quarantined
            WHERE quarantined.id = worker_network_slots.runtime_instance_id
       )
       AND (worker_network_slots.state <> 'lost' OR worker_network_slots.reclaimed_at IS NULL)
    RETURNING worker_network_slots.id
), quarantined_slots AS (
    UPDATE worker_network_slots
       SET state = 'quarantined', quarantined_at = now(),
           state_reason_code = 'startup_inventory_quarantined',
           state_error = sqlc.arg(recovery_evidence)::jsonb, updated_at = now()
      FROM target
     WHERE worker_network_slots.worker_instance_id = target.id
       AND worker_network_slots.worker_epoch < target.current_epoch
       AND worker_network_slots.runtime_instance_id IN (SELECT id FROM quarantined)
    RETURNING worker_network_slots.id
)
UPDATE worker_instances
   SET startup_inventory_epoch = target.current_epoch,
       startup_inventory_evidence = sqlc.arg(recovery_evidence)::jsonb,
       updated_at = now()
  FROM target
 WHERE worker_instances.id = target.id
   AND (SELECT count(*) FROM reclaimed_runtimes) >= 0
   AND (SELECT count(*) FROM quarantined_slots) >= 0
   AND (SELECT count(*) FROM reclaimed_slots) >= 0
RETURNING worker_instances.*;
