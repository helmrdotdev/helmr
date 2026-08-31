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
INSERT INTO worker_groups (id, token_id, region_id, name, description, state)
SELECT sqlc.arg(id), token.id, sqlc.arg(region_id), sqlc.arg(name),
       sqlc.arg(description), 'active'
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

-- name: GetWorkerGroupState :one
SELECT id, state, claim_version
  FROM worker_groups
 WHERE id = sqlc.arg(worker_group_id);

-- name: LockWorkerGroupForPoolMutation :one
SELECT *
  FROM worker_groups
 WHERE id = sqlc.arg(worker_group_id)
 FOR UPDATE;

-- name: GetWorkerPoolByGroupName :one
SELECT *
  FROM worker_pools
 WHERE worker_group_id = sqlc.arg(worker_group_id)
   AND name = sqlc.arg(name);

-- name: ListWorkerPools :many
SELECT *
  FROM worker_pools
 WHERE worker_group_id = sqlc.arg(worker_group_id)
 ORDER BY name, id;

-- name: CreatePendingWorkerPool :one
INSERT INTO worker_pools (id, worker_group_id, name, state, claim_version)
SELECT sqlc.arg(worker_pool_id), worker_groups.id, sqlc.arg(name),
       'pending', 1
  FROM worker_groups
 WHERE worker_groups.id = sqlc.arg(worker_group_id)
   AND worker_groups.claim_version = sqlc.arg(expected_group_claim_version)
   AND worker_groups.state IN ('active', 'paused')
RETURNING worker_pools.*;

-- name: SetWorkerGroupPrimaryPool :one
UPDATE worker_groups
   SET primary_pool_id = sqlc.arg(pool_id),
       claim_version = worker_groups.claim_version + 1,
       updated_at = now()
 WHERE worker_groups.id = sqlc.arg(worker_group_id)
   AND worker_groups.claim_version = sqlc.arg(expected_group_claim_version)
   AND worker_groups.state IN ('active', 'paused')
RETURNING worker_groups.*;

-- name: TransitionWorkerPoolLifecycle :one
WITH restore_profiles AS MATERIALIZED (
    SELECT DISTINCT source_lease.worker_group_id,
           source_runtime.runtime_identity_id,
           source_runtime.vm_vcpu_count,
           source_runtime.cpu_config_digest,
           source_lease.requested_cpu_millis,
           source_lease.requested_memory_bytes,
           source_lease.requested_guest_ephemeral_disk_bytes,
           runtime_substrates.substrate_format,
           runtime_substrates.substrate_contract,
           (runtime_substrates.id IS NOT NULL) AS substrate_known
      FROM run_checkpoints
      JOIN run_leases AS source_lease
        ON source_lease.id = run_checkpoints.source_run_lease_id
       AND source_lease.run_id = run_checkpoints.run_id
       AND source_lease.attempt_number = run_checkpoints.attempt_number
       AND source_lease.workspace_id = run_checkpoints.workspace_id
       AND source_lease.state = 'checkpointed'
      JOIN runtime_instances AS source_runtime
        ON source_runtime.id = source_lease.runtime_instance_id
       AND source_runtime.worker_group_id = source_lease.worker_group_id
      LEFT JOIN runtime_substrates
        ON runtime_substrates.id = source_runtime.runtime_substrate_id
       AND runtime_substrates.org_id = source_runtime.org_id
       AND runtime_substrates.project_id = source_runtime.project_id
       AND runtime_substrates.environment_id = source_runtime.environment_id
       AND runtime_substrates.deployment_definition_id = source_runtime.deployment_definition_id
     WHERE run_checkpoints.state = 'ready'
       AND (run_checkpoints.expires_at IS NULL
            OR run_checkpoints.expires_at > transaction_timestamp())
    UNION
    SELECT source_lease.worker_group_id,
           source_runtime.runtime_identity_id,
           source_runtime.vm_vcpu_count,
           source_runtime.cpu_config_digest,
           source_lease.requested_cpu_millis,
           source_lease.requested_memory_bytes,
           source_lease.requested_guest_ephemeral_disk_bytes,
           runtime_substrates.substrate_format,
           runtime_substrates.substrate_contract,
           (runtime_substrates.id IS NOT NULL) AS substrate_known
      FROM run_leases AS source_lease
      JOIN runtime_instances AS source_runtime
        ON source_runtime.id = source_lease.runtime_instance_id
       AND source_runtime.worker_group_id = source_lease.worker_group_id
       AND source_runtime.worker_instance_id = source_lease.worker_instance_id
       AND source_runtime.worker_epoch = source_lease.worker_epoch
       AND source_runtime.reclaimed_at IS NULL
      LEFT JOIN runtime_substrates
        ON runtime_substrates.id = source_runtime.runtime_substrate_id
       AND runtime_substrates.org_id = source_runtime.org_id
       AND runtime_substrates.project_id = source_runtime.project_id
       AND runtime_substrates.environment_id = source_runtime.environment_id
       AND runtime_substrates.deployment_definition_id = source_runtime.deployment_definition_id
     WHERE source_lease.state IN ('assigned', 'starting', 'running', 'checkpointing', 'finalizing')
)
UPDATE worker_pools AS target
       SET state = sqlc.arg(target_state)::text,
           claim_version = target.claim_version + 1,
           updated_at = now()
      FROM worker_groups
     WHERE target.id = sqlc.arg(worker_pool_id)
       AND target.worker_group_id = sqlc.arg(worker_group_id)
       AND target.claim_version = sqlc.arg(expected_pool_claim_version)
       AND worker_groups.id = target.worker_group_id
       AND worker_groups.state IN ('active', 'paused', 'draining')
       AND worker_groups.primary_pool_id IS DISTINCT FROM target.id
       AND (
           (
               sqlc.arg(target_state)::text = 'draining'
               AND target.state = 'active'
               AND NOT EXISTS (
                   SELECT 1
                     FROM restore_profiles
                    WHERE restore_profiles.worker_group_id = target.worker_group_id
                      AND target.runtime_identity_id = restore_profiles.runtime_identity_id
                      AND EXISTS (
                          SELECT 1 FROM worker_pool_cpu_shapes AS target_shape
                           WHERE target_shape.worker_pool_id = target.id
                             AND target_shape.vcpu_count = restore_profiles.vm_vcpu_count
                             AND target_shape.cpu_config_digest = restore_profiles.cpu_config_digest
                      )
                      AND target.per_vm_cpu_millis >= restore_profiles.requested_cpu_millis
                      AND target.per_vm_memory_bytes >= restore_profiles.requested_memory_bytes
                      AND target.per_vm_guest_ephemeral_disk_bytes >= restore_profiles.requested_guest_ephemeral_disk_bytes
                      AND (
                          NOT restore_profiles.substrate_known
                          OR (
                              target.substrate_format = restore_profiles.substrate_format
                              AND target.substrate_contract = restore_profiles.substrate_contract
                              AND NOT EXISTS (
                                  SELECT 1
                                    FROM worker_pools AS supplier
                                   WHERE supplier.worker_group_id = target.worker_group_id
                                     AND supplier.id <> target.id
                                     AND supplier.state = 'active'
                                     AND supplier.runtime_identity_id = restore_profiles.runtime_identity_id
                                     AND supplier.substrate_format = restore_profiles.substrate_format
                                     AND supplier.substrate_contract = restore_profiles.substrate_contract
                                     AND supplier.per_vm_cpu_millis >= restore_profiles.requested_cpu_millis
                                     AND supplier.per_vm_memory_bytes >= restore_profiles.requested_memory_bytes
                                     AND supplier.per_vm_guest_ephemeral_disk_bytes >= restore_profiles.requested_guest_ephemeral_disk_bytes
                                     AND EXISTS (
                                         SELECT 1 FROM worker_pool_cpu_shapes AS supplier_shape
                                          WHERE supplier_shape.worker_pool_id = supplier.id
                                            AND supplier_shape.vcpu_count = restore_profiles.vm_vcpu_count
                                            AND supplier_shape.cpu_config_digest = restore_profiles.cpu_config_digest
                                     )
                              )
                          )
                      )
               )
           )
           OR (
               sqlc.arg(target_state)::text = 'disabled'
               AND (
                   (
                       target.state = 'pending'
                       AND NOT EXISTS (
                           SELECT 1 FROM worker_instances
                            WHERE worker_instances.worker_group_id = target.worker_group_id
                              AND worker_instances.worker_pool_id = target.id
                              AND worker_instances.state IN ('registering', 'active', 'draining')
                       )
                       AND NOT EXISTS (
                           SELECT 1
                             FROM worker_instances
                            WHERE worker_instances.worker_group_id = target.worker_group_id
                              AND worker_instances.worker_pool_id = target.id
                              AND (
                                  EXISTS (
                                      SELECT 1 FROM runtime_instances
                                       WHERE runtime_instances.worker_group_id = worker_instances.worker_group_id
                                         AND runtime_instances.worker_instance_id = worker_instances.id
                                         AND runtime_instances.reclaimed_at IS NULL
                                  )
                                  OR EXISTS (
                                      SELECT 1 FROM run_leases
                                       WHERE run_leases.worker_group_id = worker_instances.worker_group_id
                                         AND run_leases.worker_instance_id = worker_instances.id
                                         AND run_leases.state IN ('assigned', 'starting', 'running', 'checkpointing', 'finalizing')
                                  )
                                  OR EXISTS (
                                      SELECT 1 FROM workspace_mounts
                                       WHERE workspace_mounts.worker_group_id = worker_instances.worker_group_id
                                         AND workspace_mounts.worker_instance_id = worker_instances.id
                                         AND workspace_mounts.state IN ('mounting', 'mounted', 'unmounting')
                                  )
                                  OR EXISTS (
                                      SELECT 1 FROM workspace_leases
                                       WHERE workspace_leases.worker_group_id = worker_instances.worker_group_id
                                         AND workspace_leases.worker_instance_id = worker_instances.id
                                         AND workspace_leases.state IN ('active', 'releasing')
                                  )
                                  OR EXISTS (
                                      SELECT 1 FROM workspace_processes
                                       WHERE workspace_processes.worker_group_id = worker_instances.worker_group_id
                                         AND workspace_processes.worker_instance_id = worker_instances.id
                                         AND workspace_processes.state IN ('starting', 'running', 'exit_requested')
                                  )
                              )
                       )
                   )
                   OR (
                       target.state = 'draining'
                       AND NOT EXISTS (
                           SELECT 1 FROM worker_instances
                            WHERE worker_instances.worker_group_id = target.worker_group_id
                              AND worker_instances.worker_pool_id = target.id
                              AND worker_instances.state IN ('registering', 'active', 'draining')
                       )
                       AND NOT EXISTS (
                           SELECT 1
                             FROM worker_instances
                            WHERE worker_instances.worker_group_id = target.worker_group_id
                              AND worker_instances.worker_pool_id = target.id
                              AND (
                                  EXISTS (
                                      SELECT 1 FROM runtime_instances
                                       WHERE runtime_instances.worker_group_id = worker_instances.worker_group_id
                                         AND runtime_instances.worker_instance_id = worker_instances.id
                                         AND runtime_instances.reclaimed_at IS NULL
                                  )
                                  OR EXISTS (
                                      SELECT 1 FROM run_leases
                                       WHERE run_leases.worker_group_id = worker_instances.worker_group_id
                                         AND run_leases.worker_instance_id = worker_instances.id
                                         AND run_leases.state IN ('assigned', 'starting', 'running', 'checkpointing', 'finalizing')
                                  )
                                  OR EXISTS (
                                      SELECT 1 FROM workspace_mounts
                                       WHERE workspace_mounts.worker_group_id = worker_instances.worker_group_id
                                         AND workspace_mounts.worker_instance_id = worker_instances.id
                                         AND workspace_mounts.state IN ('mounting', 'mounted', 'unmounting')
                                  )
                                  OR EXISTS (
                                      SELECT 1 FROM workspace_leases
                                       WHERE workspace_leases.worker_group_id = worker_instances.worker_group_id
                                         AND workspace_leases.worker_instance_id = worker_instances.id
                                         AND workspace_leases.state IN ('active', 'releasing')
                                  )
                                  OR EXISTS (
                                      SELECT 1 FROM workspace_processes
                                       WHERE workspace_processes.worker_group_id = worker_instances.worker_group_id
                                         AND workspace_processes.worker_instance_id = worker_instances.id
                                         AND workspace_processes.state IN ('starting', 'running', 'exit_requested')
                                  )
                              )
                       )
                       AND NOT EXISTS (
                           SELECT 1
                             FROM restore_profiles
	                            WHERE restore_profiles.worker_group_id = target.worker_group_id
	                              AND target.runtime_identity_id = restore_profiles.runtime_identity_id
                              AND EXISTS (
                                  SELECT 1 FROM worker_pool_cpu_shapes AS target_shape
                                   WHERE target_shape.worker_pool_id = target.id
                                     AND target_shape.vcpu_count = restore_profiles.vm_vcpu_count
                                     AND target_shape.cpu_config_digest = restore_profiles.cpu_config_digest
                              )
                              AND target.per_vm_cpu_millis >= restore_profiles.requested_cpu_millis
                              AND target.per_vm_memory_bytes >= restore_profiles.requested_memory_bytes
                              AND target.per_vm_guest_ephemeral_disk_bytes >= restore_profiles.requested_guest_ephemeral_disk_bytes
                              AND (
                                  NOT restore_profiles.substrate_known
                                  OR (
                                      target.substrate_format = restore_profiles.substrate_format
                                      AND target.substrate_contract = restore_profiles.substrate_contract
                                      AND NOT EXISTS (
                                          SELECT 1
                                            FROM worker_pools AS supplier
                                           WHERE supplier.worker_group_id = target.worker_group_id
                                             AND supplier.id <> target.id
	                                             AND supplier.state = 'active'
                                             AND supplier.runtime_identity_id = restore_profiles.runtime_identity_id
                                             AND supplier.substrate_format = restore_profiles.substrate_format
                                             AND supplier.substrate_contract = restore_profiles.substrate_contract
                                             AND supplier.per_vm_cpu_millis >= restore_profiles.requested_cpu_millis
                                             AND supplier.per_vm_memory_bytes >= restore_profiles.requested_memory_bytes
                                             AND supplier.per_vm_guest_ephemeral_disk_bytes >= restore_profiles.requested_guest_ephemeral_disk_bytes
                                             AND EXISTS (
                                                 SELECT 1 FROM worker_pool_cpu_shapes AS supplier_shape
                                                  WHERE supplier_shape.worker_pool_id = supplier.id
                                                    AND supplier_shape.vcpu_count = restore_profiles.vm_vcpu_count
                                                    AND supplier_shape.cpu_config_digest = restore_profiles.cpu_config_digest
                                             )
                                      )
                                  )
                              )
                       )
                   )
               )
           )
       )
RETURNING target.*;

-- name: ListCapacityWorkerPools :many
SELECT worker_pools.id,
       worker_pools.worker_group_id,
       worker_pools.name,
       worker_pools.runtime_identity_id,
       worker_pools.substrate_format,
       worker_pools.substrate_contract,
       worker_pools.capacity_cpu_millis,
       worker_pools.capacity_memory_bytes,
       worker_pools.capacity_guest_ephemeral_disk_bytes,
       worker_pools.per_vm_cpu_millis,
       worker_pools.per_vm_memory_bytes,
       worker_pools.per_vm_guest_ephemeral_disk_bytes,
       worker_pools.max_vm_slots,
       COALESCE((
           SELECT array_agg(worker_pool_cpu_shapes.vcpu_count ORDER BY worker_pool_cpu_shapes.vcpu_count)
             FROM worker_pool_cpu_shapes
            WHERE worker_pool_cpu_shapes.worker_pool_id = worker_pools.id
       ), ARRAY[]::integer[])::integer[] AS cpu_shape_vcpu_counts,
       COALESCE((
           SELECT array_agg(worker_pool_cpu_shapes.cpu_config_digest ORDER BY worker_pool_cpu_shapes.vcpu_count)
             FROM worker_pool_cpu_shapes
            WHERE worker_pool_cpu_shapes.worker_pool_id = worker_pools.id
       ), ARRAY[]::text[])::text[] AS cpu_shape_config_digests,
       COALESCE((
           SELECT count(*)
             FROM worker_instances
            WHERE worker_instances.worker_pool_id = worker_pools.id
              AND worker_instances.worker_group_id = worker_pools.worker_group_id
              AND worker_instances.state = 'registering'
       ), 0)::bigint AS registering_workers,
       COALESCE((
           SELECT count(*)
             FROM worker_instances
            WHERE worker_instances.worker_pool_id = worker_pools.id
              AND worker_instances.worker_group_id = worker_pools.worker_group_id
              AND worker_instances.state = 'active'
       ), 0)::bigint AS active_workers
  FROM worker_pools
 WHERE worker_pools.worker_group_id = sqlc.arg(worker_group_id)
   AND worker_pools.id = ANY(sqlc.arg(worker_pool_ids)::uuid[])
   AND worker_pools.state = 'active'
 ORDER BY worker_pools.id;

-- name: LockWorkerPool :one
SELECT *
  FROM worker_pools
 WHERE worker_group_id = sqlc.arg(worker_group_id)
   AND id = sqlc.arg(worker_pool_id)
 FOR UPDATE;

-- name: RequireCheckpointRestoreSupplier :one
SELECT supplier.id
  FROM run_leases AS source_lease
  JOIN runtime_instances AS source_runtime
    ON source_runtime.id = source_lease.runtime_instance_id
   AND source_runtime.worker_group_id = source_lease.worker_group_id
   AND source_runtime.worker_instance_id = source_lease.worker_instance_id
   AND source_runtime.worker_epoch = source_lease.worker_epoch
  JOIN worker_instances AS source_worker
    ON source_worker.id = source_lease.worker_instance_id
   AND source_worker.worker_group_id = source_lease.worker_group_id
   AND source_worker.worker_pool_id = sqlc.arg(source_worker_pool_id)
  JOIN worker_groups
    ON worker_groups.id = source_lease.worker_group_id
  JOIN worker_pools AS source_pool
    ON source_pool.id = source_worker.worker_pool_id
   AND source_pool.worker_group_id = source_worker.worker_group_id
  JOIN worker_pool_cpu_shapes AS source_shape
    ON source_shape.worker_pool_id = source_pool.id
   AND source_shape.vcpu_count = source_runtime.vm_vcpu_count
   AND source_shape.cpu_config_digest = source_runtime.cpu_config_digest
  LEFT JOIN runtime_substrates
    ON runtime_substrates.id = source_runtime.runtime_substrate_id
   AND runtime_substrates.org_id = source_runtime.org_id
   AND runtime_substrates.project_id = source_runtime.project_id
   AND runtime_substrates.environment_id = source_runtime.environment_id
   AND runtime_substrates.deployment_definition_id = source_runtime.deployment_definition_id
  JOIN worker_pools AS supplier
	ON supplier.worker_group_id = source_lease.worker_group_id
	AND supplier.state = 'active'
	AND supplier.runtime_identity_id = source_runtime.runtime_identity_id
   AND supplier.per_vm_cpu_millis >= source_lease.requested_cpu_millis
   AND supplier.per_vm_memory_bytes >= source_lease.requested_memory_bytes
   AND supplier.per_vm_guest_ephemeral_disk_bytes >= source_lease.requested_guest_ephemeral_disk_bytes
   AND (
       source_runtime.runtime_substrate_id IS NULL
       OR (
           supplier.substrate_format = runtime_substrates.substrate_format
           AND supplier.substrate_contract = runtime_substrates.substrate_contract
       )
   )
  JOIN worker_pool_cpu_shapes AS supplier_shape
    ON supplier_shape.worker_pool_id = supplier.id
   AND supplier_shape.vcpu_count = source_runtime.vm_vcpu_count
   AND supplier_shape.cpu_config_digest = source_runtime.cpu_config_digest
 WHERE source_lease.id = sqlc.arg(source_run_lease_id)
   AND source_lease.worker_group_id = sqlc.arg(worker_group_id)
   AND source_lease.worker_instance_id = sqlc.arg(worker_instance_id)
   AND source_lease.worker_epoch = sqlc.arg(worker_epoch)
   AND source_lease.state = 'checkpointing'
	AND worker_groups.state IN ('active', 'paused', 'draining')
	AND source_pool.state IN ('active', 'draining')
	AND source_pool.runtime_identity_id = source_runtime.runtime_identity_id
   AND source_pool.per_vm_cpu_millis >= source_lease.requested_cpu_millis
   AND source_pool.per_vm_memory_bytes >= source_lease.requested_memory_bytes
   AND source_pool.per_vm_guest_ephemeral_disk_bytes >= source_lease.requested_guest_ephemeral_disk_bytes
   AND (
       source_runtime.runtime_substrate_id IS NULL
       OR (
           source_pool.substrate_format = runtime_substrates.substrate_format
           AND source_pool.substrate_contract = runtime_substrates.substrate_contract
       )
   )
 LIMIT 1;

-- name: GetWorkerInstancePoolID :one
SELECT worker_pool_id
  FROM worker_instances
 WHERE id = sqlc.arg(worker_instance_id)
   AND worker_group_id = sqlc.arg(worker_group_id)
   AND current_epoch = sqlc.arg(worker_epoch);

-- name: LockWorkerInstanceForActivation :one
SELECT *
  FROM worker_instances
 WHERE id = sqlc.arg(worker_instance_id)
   AND worker_group_id = sqlc.arg(worker_group_id)
   AND worker_pool_id = sqlc.arg(worker_pool_id)
   AND current_epoch = sqlc.arg(worker_epoch)
 FOR UPDATE;

-- name: UpsertRuntimeIdentity :one
INSERT INTO runtime_identities (
    id, runtime_arch, vm_runtime_contract, vm_runtime_descriptor_digest,
    firecracker_digest, firecracker_version, snapshot_format_version,
    host_kernel_release, cpu_template_kind, cpu_template_digest,
    kernel_digest, initramfs_digest, rootfs_digest, last_seen_at
)
VALUES (
    sqlc.arg(id), sqlc.arg(runtime_arch), sqlc.arg(vm_runtime_contract),
    sqlc.arg(vm_runtime_descriptor_digest), sqlc.arg(firecracker_digest),
    sqlc.arg(firecracker_version), sqlc.arg(snapshot_format_version),
    sqlc.arg(host_kernel_release), sqlc.arg(cpu_template_kind),
    sqlc.narg(cpu_template_digest), sqlc.arg(kernel_digest),
    sqlc.arg(initramfs_digest), sqlc.arg(rootfs_digest), now()
)
ON CONFLICT (id) DO UPDATE SET last_seen_at = now()
 WHERE runtime_identities.runtime_arch = EXCLUDED.runtime_arch
   AND runtime_identities.vm_runtime_contract = EXCLUDED.vm_runtime_contract
   AND runtime_identities.vm_runtime_descriptor_digest = EXCLUDED.vm_runtime_descriptor_digest
   AND runtime_identities.firecracker_digest = EXCLUDED.firecracker_digest
   AND runtime_identities.firecracker_version = EXCLUDED.firecracker_version
   AND runtime_identities.snapshot_format_version = EXCLUDED.snapshot_format_version
   AND runtime_identities.host_kernel_release = EXCLUDED.host_kernel_release
   AND runtime_identities.cpu_template_kind = EXCLUDED.cpu_template_kind
   AND runtime_identities.cpu_template_digest IS NOT DISTINCT FROM EXCLUDED.cpu_template_digest
   AND runtime_identities.kernel_digest = EXCLUDED.kernel_digest
   AND runtime_identities.initramfs_digest = EXCLUDED.initramfs_digest
   AND runtime_identities.rootfs_digest = EXCLUDED.rootfs_digest
RETURNING *;

-- name: InsertWorkerPoolCPUShape :execrows
INSERT INTO worker_pool_cpu_shapes (worker_pool_id, vcpu_count, cpu_config_digest)
SELECT worker_pools.id, sqlc.arg(vcpu_count), sqlc.arg(cpu_config_digest)
  FROM worker_pools
 WHERE worker_pools.id = sqlc.arg(worker_pool_id)
   AND worker_pools.state = 'pending';

-- name: ListWorkerPoolCPUShapes :many
SELECT *
  FROM worker_pool_cpu_shapes
 WHERE worker_pool_id = sqlc.arg(worker_pool_id)
 ORDER BY vcpu_count;

-- name: SealWorkerPool :one
UPDATE worker_pools
   SET state = 'active',
       runtime_identity_id = sqlc.arg(runtime_identity_id),
       substrate_format = sqlc.arg(substrate_format),
       substrate_contract = sqlc.arg(substrate_contract),
       capacity_cpu_millis = sqlc.arg(capacity_cpu_millis),
       capacity_memory_bytes = sqlc.arg(capacity_memory_bytes),
       capacity_guest_ephemeral_disk_bytes = sqlc.arg(capacity_guest_ephemeral_disk_bytes),
       per_vm_cpu_millis = sqlc.arg(per_vm_cpu_millis),
       per_vm_memory_bytes = sqlc.arg(per_vm_memory_bytes),
       per_vm_guest_ephemeral_disk_bytes = sqlc.arg(per_vm_guest_ephemeral_disk_bytes),
       max_vm_slots = sqlc.arg(max_vm_slots),
       sealed_at = now(), updated_at = now()
 WHERE id = sqlc.arg(worker_pool_id)
   AND worker_group_id = sqlc.arg(worker_group_id)
   AND state = 'pending'
RETURNING *;

-- name: SetInitialWorkerGroupPrimaryPool :one
WITH selection AS (
    SELECT worker_groups.id AS worker_group_id,
           worker_pools.id AS worker_pool_id,
           worker_groups.primary_pool_id IS NULL
           AND NOT EXISTS (
               SELECT 1 FROM worker_pools AS other
                WHERE other.worker_group_id = worker_groups.id
                  AND other.id <> worker_pools.id
                  AND other.sealed_at IS NOT NULL
           ) AS set_primary
      FROM worker_groups
      JOIN worker_pools
        ON worker_pools.worker_group_id = worker_groups.id
       AND worker_pools.id = sqlc.arg(worker_pool_id)
       AND worker_pools.state = 'active'
     WHERE worker_groups.id = sqlc.arg(worker_group_id)
)
UPDATE worker_groups
   SET primary_pool_id = CASE
           WHEN selection.set_primary THEN selection.worker_pool_id
           ELSE worker_groups.primary_pool_id
       END,
       claim_version = worker_groups.claim_version + CASE
           WHEN selection.set_primary THEN 1
           ELSE 0
       END,
       updated_at = CASE
           WHEN selection.set_primary THEN now()
           ELSE worker_groups.updated_at
       END
  FROM selection
 WHERE worker_groups.id = selection.worker_group_id
RETURNING worker_groups.*;

-- name: TransitionWorkerGroupState :one
WITH transitioned AS (
    UPDATE worker_groups
       SET state = sqlc.arg(target_state),
           primary_pool_id = CASE
               WHEN sqlc.arg(target_state)::text = 'draining' THEN NULL
               ELSE worker_groups.primary_pool_id
           END,
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
                   SELECT 1 FROM worker_pools
                    WHERE worker_pools.worker_group_id = worker_groups.id
                      AND worker_pools.state IN ('pending', 'active', 'draining')
               )
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
SELECT id, resource_id, worker_group_id, worker_pool_id, state, claim_version, current_epoch
  FROM worker_instances
 WHERE worker_group_id = sqlc.arg(worker_group_id)
   AND resource_id = sqlc.arg(resource_id)
 ORDER BY (state IN ('registering', 'active', 'draining')) DESC, created_at DESC
 LIMIT 1;

-- name: GetCapacityWorkerInstance :one
SELECT id, resource_id, worker_group_id, worker_pool_id, state, claim_version, current_epoch,
       draining_at, termination_ready_at, lost_at,
       created_at, updated_at
  FROM worker_instances
 WHERE id = sqlc.arg(worker_instance_id);

-- name: ListCapacityWorkerInstances :many
WITH current_instances AS (
    SELECT DISTINCT ON (worker_group_id, resource_id)
           id, resource_id, worker_group_id, worker_pool_id, state, claim_version, current_epoch,
           draining_at, termination_ready_at, lost_at,
           created_at, updated_at
     FROM worker_instances
     WHERE (sqlc.narg(worker_group_id)::text IS NULL OR worker_group_id = sqlc.narg(worker_group_id))
       AND (
           NOT sqlc.arg(has_unreclaimed_runtime)::boolean
           OR EXISTS (
               SELECT 1
                 FROM runtime_instances
                WHERE runtime_instances.worker_instance_id = worker_instances.id
                  AND runtime_instances.reclaimed_at IS NULL
           )
       )
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

-- name: ListWorkerCapacityBins :many
WITH live_workers AS (
    SELECT worker_groups.id AS worker_group_id,
           worker_groups.primary_pool_id,
           worker_pools.id AS worker_pool_id,
           worker_instances.id AS worker_instance_id,
           worker_instances.current_epoch AS worker_epoch,
           worker_instances.runtime_identity_id,
           runtime_identities.runtime_arch,
           runtime_identities.vm_runtime_contract,
           worker_instances.substrate_format,
           worker_instances.substrate_contract,
           worker_instances.per_vm_cpu_millis,
           worker_instances.per_vm_memory_bytes,
           worker_instances.per_vm_guest_ephemeral_disk_bytes,
           worker_instances.max_vm_slots,
           worker_instances.max_runtime_starts,
           worker_instances.run_paused_reason,
           worker_instances.runtime_paused_reason,
           worker_instances.epoch_cpu_millis,
           worker_instances.epoch_memory_bytes,
           worker_instances.epoch_guest_ephemeral_disk_bytes
      FROM worker_groups
      JOIN worker_instances
        ON worker_instances.worker_group_id = worker_groups.id
       AND worker_instances.state = 'active'
       AND worker_instances.current_epoch IS NOT NULL
      JOIN worker_pools
        ON worker_pools.id = worker_instances.worker_pool_id
       AND worker_pools.worker_group_id = worker_instances.worker_group_id
       AND worker_pools.state = 'active'
      JOIN runtime_identities
        ON runtime_identities.id = worker_instances.runtime_identity_id
     WHERE (sqlc.arg(worker_group_id)::text = '' OR worker_groups.id = sqlc.arg(worker_group_id))
       AND (sqlc.arg(region_id)::text = '' OR worker_groups.region_id = sqlc.arg(region_id))
       AND worker_groups.state = 'active'
       AND worker_instances.observed_at >= transaction_timestamp()
           - sqlc.arg(observation_freshness_seconds)::bigint * interval '1 second'
), usage AS (
    SELECT live_workers.worker_instance_id,
           COALESCE((SELECT sum(runtime_instances.reserved_cpu_millis)
                      FROM runtime_instances
                     WHERE runtime_instances.worker_instance_id = live_workers.worker_instance_id
                        AND runtime_instances.worker_epoch = live_workers.worker_epoch
                        AND runtime_instances.reclaimed_at IS NULL), 0) AS cpu_millis,
           COALESCE((SELECT sum(runtime_instances.reserved_memory_bytes)
                      FROM runtime_instances
                     WHERE runtime_instances.worker_instance_id = live_workers.worker_instance_id
                        AND runtime_instances.worker_epoch = live_workers.worker_epoch
                        AND runtime_instances.reclaimed_at IS NULL), 0) AS memory_bytes,
           COALESCE((SELECT sum(runtime_instances.reserved_guest_ephemeral_disk_bytes)
                      FROM runtime_instances
                     WHERE runtime_instances.worker_instance_id = live_workers.worker_instance_id
                        AND runtime_instances.worker_epoch = live_workers.worker_epoch
                        AND runtime_instances.reclaimed_at IS NULL), 0) AS guest_ephemeral_disk_bytes,
           COALESCE((SELECT count(*) FROM runtime_instances
                      WHERE runtime_instances.worker_instance_id = live_workers.worker_instance_id
                        AND runtime_instances.worker_epoch = live_workers.worker_epoch
                        AND (runtime_instances.observed_state IN ('allocated', 'ready')
                             OR (runtime_instances.observed_state IN ('failed', 'lost') AND runtime_instances.reclaimed_at IS NULL))), 0)::bigint AS vm_slots,
           COALESCE((SELECT count(*) FROM run_leases
                      WHERE run_leases.worker_instance_id = live_workers.worker_instance_id
                        AND run_leases.worker_epoch = live_workers.worker_epoch
                        AND run_leases.state IN ('assigned', 'starting', 'running', 'checkpointing', 'finalizing')), 0)::bigint AS run_consumers,
           COALESCE((SELECT count(*) FROM runtime_instances
                      WHERE runtime_instances.worker_instance_id = live_workers.worker_instance_id
                        AND runtime_instances.worker_epoch = live_workers.worker_epoch
                        AND runtime_instances.observed_state = 'allocated'), 0)::bigint AS runtime_starts
      FROM live_workers
)
SELECT live_workers.worker_group_id,
       live_workers.primary_pool_id,
       live_workers.worker_pool_id,
       live_workers.worker_instance_id,
       live_workers.worker_epoch,
       live_workers.runtime_identity_id,
       live_workers.runtime_arch,
       live_workers.vm_runtime_contract,
       live_workers.substrate_format,
       live_workers.substrate_contract,
       live_workers.per_vm_cpu_millis,
       live_workers.per_vm_memory_bytes,
       live_workers.per_vm_guest_ephemeral_disk_bytes,
       GREATEST(live_workers.epoch_cpu_millis - usage.cpu_millis, 0)::bigint AS available_cpu_millis,
       GREATEST(live_workers.epoch_memory_bytes - usage.memory_bytes, 0)::bigint AS available_memory_bytes,
       GREATEST(live_workers.epoch_guest_ephemeral_disk_bytes - usage.guest_ephemeral_disk_bytes, 0)::bigint AS available_guest_ephemeral_disk_bytes,
       GREATEST(live_workers.max_vm_slots - usage.vm_slots, 0)::bigint AS available_vm_slots,
       GREATEST(live_workers.max_vm_slots - usage.run_consumers, 0)::bigint AS available_run_consumers,
       GREATEST(live_workers.max_runtime_starts - usage.runtime_starts, 0)::bigint AS available_runtime_starts,
	       live_workers.run_paused_reason,
	       live_workers.runtime_paused_reason,
       COALESCE((
           SELECT array_agg(worker_pool_cpu_shapes.vcpu_count ORDER BY worker_pool_cpu_shapes.vcpu_count)
             FROM worker_pool_cpu_shapes
            WHERE worker_pool_cpu_shapes.worker_pool_id = live_workers.worker_pool_id
       ), ARRAY[]::integer[])::integer[] AS cpu_shape_vcpu_counts,
       COALESCE((
           SELECT array_agg(worker_pool_cpu_shapes.cpu_config_digest ORDER BY worker_pool_cpu_shapes.vcpu_count)
             FROM worker_pool_cpu_shapes
            WHERE worker_pool_cpu_shapes.worker_pool_id = live_workers.worker_pool_id
	       ), ARRAY[]::text[])::text[] AS cpu_shape_config_digests
  FROM live_workers
  JOIN usage USING (worker_instance_id)
 ORDER BY live_workers.worker_instance_id;

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
           observed_at = now(), terminal_at = now(),
           terminal_reason_code = 'external_instance_drift',
           reserved_run_id = NULL, reserved_attempt_number = NULL,
           reserved_process_id = NULL, reserved_workspace_version_id = NULL,
           reservation_expires_at = NULL, updated_at = now()
      FROM target
     WHERE runtime_instances.worker_instance_id = target.id
       AND runtime_instances.worker_epoch = target.current_epoch
       AND runtime_instances.reclaimed_at IS NULL
       AND runtime_instances.observed_state IN ('allocated', 'ready')
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

-- name: ConfirmWorkerInstanceProviderAbsent :one
WITH target AS MATERIALIZED (
    SELECT worker_instances.id
      FROM worker_instances
     WHERE worker_instances.id = sqlc.arg(worker_instance_id)
       AND worker_instances.state IN ('registering', 'active', 'draining', 'lost')
     FOR UPDATE
), transitioned AS (
    UPDATE worker_instances
       SET state = 'lost',
           claim_version = worker_instances.claim_version
               + CASE WHEN worker_instances.state = 'lost' THEN 0 ELSE 1 END,
           lost_at = COALESCE(worker_instances.lost_at, now()),
           updated_at = CASE
               WHEN worker_instances.state = 'lost' THEN worker_instances.updated_at
               ELSE now()
           END
      FROM target
     WHERE worker_instances.id = target.id
    RETURNING worker_instances.id, worker_instances.resource_id,
              worker_instances.worker_group_id, worker_instances.worker_pool_id,
              worker_instances.state, worker_instances.claim_version,
              worker_instances.current_epoch, worker_instances.draining_at,
              worker_instances.termination_ready_at, worker_instances.lost_at,
              worker_instances.created_at, worker_instances.updated_at
), revoked_credentials AS (
    UPDATE worker_instance_credentials
       SET revoked_at = COALESCE(worker_instance_credentials.revoked_at, now())
      FROM transitioned
     WHERE worker_instance_credentials.worker_instance_id = transitioned.id
       AND worker_instance_credentials.revoked_at IS NULL
    RETURNING worker_instance_credentials.id
), lost_mounts AS (
    UPDATE workspace_mounts
       SET state = 'lost',
           lost_at = COALESCE(workspace_mounts.lost_at, now()),
           terminal_at = COALESCE(workspace_mounts.terminal_at, now()),
           terminal_reason_code = COALESCE(
               workspace_mounts.terminal_reason_code,
               'external_instance_drift'
           ),
           updated_at = now()
      FROM transitioned
     WHERE workspace_mounts.worker_instance_id = transitioned.id
       AND workspace_mounts.state IN ('mounting', 'mounted', 'unmounting')
    RETURNING workspace_mounts.id
)
SELECT transitioned.id, transitioned.resource_id,
       transitioned.worker_group_id, transitioned.worker_pool_id,
       transitioned.state, transitioned.claim_version,
       transitioned.current_epoch, transitioned.draining_at,
       transitioned.termination_ready_at, transitioned.lost_at,
       transitioned.created_at, transitioned.updated_at
  FROM transitioned
 WHERE (SELECT count(*) FROM revoked_credentials) >= 0
   AND (SELECT count(*) FROM lost_mounts) >= 0;

-- name: ReconcileProviderAbsentWorkerRuntimes :one
WITH runtime_candidates AS MATERIALIZED (
    SELECT runtime_instances.id,
           NOT EXISTS (
               SELECT 1
                 FROM run_leases
                WHERE run_leases.runtime_instance_id = runtime_instances.id
                  AND run_leases.state IN (
                      'assigned', 'starting', 'running', 'checkpointing', 'finalizing'
                  )
           ) AS reclaimable
      FROM runtime_instances
      JOIN worker_instances
        ON worker_instances.id = runtime_instances.worker_instance_id
       AND worker_instances.id = sqlc.arg(worker_instance_id)
       AND worker_instances.state = 'lost'
     WHERE runtime_instances.reclaimed_at IS NULL
     ORDER BY runtime_instances.id
     FOR UPDATE OF runtime_instances
), reconciled_runtimes AS (
    UPDATE runtime_instances
       SET observed_state = CASE
               WHEN runtime_instances.observed_state IN ('failed', 'lost')
               THEN runtime_instances.observed_state
               ELSE 'lost'
           END,
           observed_version = runtime_instances.observed_version + 1,
           observed_at = now(),
           terminal_at = COALESCE(runtime_instances.terminal_at, now()),
           terminal_reason_code = COALESCE(
               runtime_instances.terminal_reason_code,
               'external_instance_drift'
           ),
           reclaimed_at = CASE
               WHEN runtime_candidates.reclaimable THEN now()
               ELSE runtime_instances.reclaimed_at
           END,
           reclaim_evidence = CASE
               WHEN runtime_candidates.reclaimable THEN jsonb_build_object(
                   'method', 'provider_absent',
                   'completed_at', now()
               )
               ELSE runtime_instances.reclaim_evidence
           END,
           reserved_run_id = NULL,
           reserved_attempt_number = NULL,
           reserved_process_id = NULL,
           reserved_workspace_version_id = NULL,
           reservation_expires_at = NULL,
           updated_at = now()
      FROM runtime_candidates
     WHERE runtime_instances.id = runtime_candidates.id
       AND (
           runtime_candidates.reclaimable
           OR runtime_instances.observed_state NOT IN ('failed', 'lost')
           OR runtime_instances.reserved_run_id IS NOT NULL
           OR runtime_instances.reserved_process_id IS NOT NULL
           OR runtime_instances.reserved_workspace_version_id IS NOT NULL
           OR runtime_instances.reservation_expires_at IS NOT NULL
       )
    RETURNING runtime_instances.id
)
SELECT count(*) FROM reconciled_runtimes;

-- name: ActivateWorkerInstance :one
UPDATE worker_instances
   SET state = CASE
           WHEN worker_instances.state = 'draining'
               OR worker_groups.state = 'draining'
               OR worker_pools.state = 'draining'
           THEN 'draining'
           ELSE 'active'
       END,
       runtime_identity_id = sqlc.arg(runtime_identity_id),
       substrate_format = sqlc.arg(substrate_format),
       substrate_contract = sqlc.arg(substrate_contract),
       epoch_cpu_millis = sqlc.arg(epoch_cpu_millis),
       epoch_memory_bytes = sqlc.arg(epoch_memory_bytes),
       epoch_guest_ephemeral_disk_bytes = sqlc.arg(epoch_guest_ephemeral_disk_bytes),
       per_vm_cpu_millis = sqlc.arg(per_vm_cpu_millis),
       per_vm_memory_bytes = sqlc.arg(per_vm_memory_bytes),
       per_vm_guest_ephemeral_disk_bytes = sqlc.arg(per_vm_guest_ephemeral_disk_bytes),
       max_vm_slots = sqlc.arg(max_vm_slots),
       max_runtime_starts = sqlc.arg(max_runtime_starts),
       cpu_environment = sqlc.arg(cpu_environment)::jsonb,
       cpu_environment_digest = sqlc.arg(cpu_environment_digest),
       activated_at = COALESCE(worker_instances.activated_at, now()),
       draining_at = CASE
           WHEN worker_instances.state = 'draining'
               OR worker_groups.state = 'draining'
               OR worker_pools.state = 'draining'
           THEN COALESCE(worker_instances.draining_at, now())
           ELSE worker_instances.draining_at
       END,
       updated_at = now()
  FROM worker_groups, worker_pools
 WHERE worker_instances.id = sqlc.arg(worker_instance_id)
   AND worker_instances.worker_group_id = sqlc.arg(worker_group_id)
   AND worker_instances.current_epoch = sqlc.arg(worker_epoch)
   AND worker_groups.id = worker_instances.worker_group_id
   AND worker_groups.state IN ('active', 'paused', 'draining')
   AND worker_pools.id = worker_instances.worker_pool_id
   AND worker_pools.worker_group_id = worker_instances.worker_group_id
   AND worker_pools.state IN ('active', 'draining')
   AND NOT EXISTS (
       SELECT 1 FROM runtime_instances
        WHERE runtime_instances.worker_instance_id = worker_instances.id
          AND runtime_instances.worker_epoch < worker_instances.current_epoch
          AND runtime_instances.reclaimed_at IS NULL
   )
   AND (
       worker_instances.state = 'registering'
       OR (
           worker_instances.state = 'draining'
           AND worker_instances.runtime_identity_id IS NULL
           AND worker_instances.substrate_format = ''
           AND worker_instances.substrate_contract = ''
           AND worker_instances.epoch_cpu_millis = 0
           AND worker_instances.epoch_memory_bytes = 0
           AND worker_instances.epoch_guest_ephemeral_disk_bytes = 0
           AND worker_instances.per_vm_cpu_millis = 0
           AND worker_instances.per_vm_memory_bytes = 0
           AND worker_instances.per_vm_guest_ephemeral_disk_bytes = 0
           AND worker_instances.max_vm_slots = 0
           AND worker_instances.max_runtime_starts = 0
           AND worker_instances.cpu_environment IS NULL
           AND worker_instances.cpu_environment_digest IS NULL
           AND worker_instances.activated_at IS NULL
       )
       OR (
           worker_instances.state IN ('active', 'draining')
           AND worker_instances.runtime_identity_id = sqlc.arg(runtime_identity_id)::text
           AND worker_instances.substrate_format = sqlc.arg(substrate_format)
           AND worker_instances.substrate_contract = sqlc.arg(substrate_contract)
           AND worker_instances.epoch_cpu_millis = sqlc.arg(epoch_cpu_millis)
           AND worker_instances.epoch_memory_bytes = sqlc.arg(epoch_memory_bytes)
           AND worker_instances.epoch_guest_ephemeral_disk_bytes = sqlc.arg(epoch_guest_ephemeral_disk_bytes)
           AND worker_instances.per_vm_cpu_millis = sqlc.arg(per_vm_cpu_millis)
           AND worker_instances.per_vm_memory_bytes = sqlc.arg(per_vm_memory_bytes)
           AND worker_instances.per_vm_guest_ephemeral_disk_bytes = sqlc.arg(per_vm_guest_ephemeral_disk_bytes)
           AND worker_instances.max_vm_slots = sqlc.arg(max_vm_slots)
           AND worker_instances.max_runtime_starts = sqlc.arg(max_runtime_starts)
           AND worker_instances.cpu_environment = sqlc.arg(cpu_environment)::jsonb
           AND worker_instances.cpu_environment_digest = sqlc.arg(cpu_environment_digest)
       )
   )
RETURNING worker_instances.*;

-- name: RecordWorkerObservation :one
UPDATE worker_instances
   SET observed_at = transaction_timestamp(),
       run_paused_reason = sqlc.narg(run_paused_reason),
       runtime_paused_reason = sqlc.narg(runtime_paused_reason),
       updated_at = now()
 WHERE worker_instances.id = sqlc.arg(worker_instance_id)
   AND worker_instances.worker_group_id = sqlc.arg(worker_group_id)
   AND worker_instances.current_epoch = sqlc.arg(worker_epoch)
   AND worker_instances.state IN ('active', 'draining')
RETURNING worker_instances.*;

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
              AND run_leases.state IN ('assigned', 'starting', 'running', 'checkpointing', 'finalizing')
            WHERE runtime_instances.worker_instance_id = worker_instances.id
              AND runtime_instances.worker_epoch < worker_instances.current_epoch
       )
	       AND (
	           worker_instances.state = 'registering'
	           OR (
	               worker_instances.state = 'draining'
	               AND worker_instances.runtime_identity_id IS NULL
	           )
       )
     FOR UPDATE
), quarantined AS (
    SELECT value::uuid AS id
      FROM jsonb_array_elements_text(sqlc.arg(recovery_evidence)::jsonb -> 'quarantined') AS value
), reclaimable_runtimes AS MATERIALIZED (
    SELECT runtime_instances.id
      FROM runtime_instances
      JOIN target
        ON target.id = runtime_instances.worker_instance_id
     WHERE runtime_instances.worker_epoch < target.current_epoch
       AND runtime_instances.reclaimed_at IS NULL
       AND runtime_instances.id NOT IN (SELECT id FROM quarantined)
       AND NOT EXISTS (
           SELECT 1
             FROM run_leases
            WHERE run_leases.runtime_instance_id = runtime_instances.id
              AND run_leases.state IN ('assigned', 'starting', 'running', 'checkpointing', 'finalizing')
       )
     ORDER BY runtime_instances.id
       FOR UPDATE OF runtime_instances
), lost_mounts AS (
    UPDATE workspace_mounts
       SET state = 'lost',
           lost_at = now(),
           terminal_at = now(),
           terminal_reason_code = 'worker_startup_reclaimed',
           updated_at = now()
     WHERE workspace_mounts.runtime_instance_id IN (SELECT id FROM reclaimable_runtimes)
       AND workspace_mounts.state IN ('mounting', 'mounted', 'unmounting')
    RETURNING workspace_mounts.id
), reclaimed_runtimes AS (
    UPDATE runtime_instances
       SET observed_state = CASE WHEN observed_state IN ('closed','failed','lost') THEN observed_state ELSE 'lost' END,
           observed_version = observed_version + 1,
           observed_at = now(),
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
     WHERE runtime_instances.id IN (SELECT id FROM reclaimable_runtimes)
       AND (SELECT count(*) FROM lost_mounts) >= 0
    RETURNING runtime_instances.id
)
UPDATE worker_instances
   SET updated_at = now()
  FROM target
 WHERE worker_instances.id = target.id
   AND (SELECT count(*) FROM lost_mounts) >= 0
   AND (SELECT count(*) FROM reclaimed_runtimes) >= 0
RETURNING worker_instances.*;
