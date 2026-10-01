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
INSERT INTO worker_groups (id, token_id, region_id, name, description, status)
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

-- name: GetWorkerGroupStatus :one
SELECT id, status, claim_version
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
INSERT INTO worker_pools (id, worker_group_id, name, status, claim_version)
SELECT sqlc.arg(worker_pool_id), worker_groups.id, sqlc.arg(name),
       'pending', 1
  FROM worker_groups
 WHERE worker_groups.id = sqlc.arg(worker_group_id)
   AND worker_groups.claim_version = sqlc.arg(expected_group_claim_version)
   AND worker_groups.status IN ('active', 'paused')
RETURNING worker_pools.*;

-- name: SetWorkerGroupPrimaryPool :one
UPDATE worker_groups
   SET primary_pool_id = sqlc.arg(pool_id),
       claim_version = worker_groups.claim_version + 1,
       updated_at = now()
 WHERE worker_groups.id = sqlc.arg(worker_group_id)
   AND worker_groups.claim_version = sqlc.arg(expected_group_claim_version)
   AND worker_groups.status IN ('active', 'paused')
RETURNING worker_groups.*;

-- name: TransitionWorkerPoolLifecycle :one
WITH restore_profiles AS MATERIALIZED (
 SELECT DISTINCT i.worker_group_id,i.vm_platform_id,i.vm_vcpu_count,i.cpu_config_digest,
   i.reserved_cpu_millis AS requested_cpu_millis,i.reserved_memory_bytes AS requested_memory_bytes,
   i.reserved_guest_ephemeral_disk_bytes AS requested_guest_ephemeral_disk_bytes
 FROM computer_instances i
 WHERE i.reclaimed_at IS NULL OR EXISTS(SELECT 1 FROM computer_checkpoints c
   WHERE c.source_computer_instance_id=i.id AND c.status='ready' AND c.resume_committed_at IS NULL
     AND (c.expires_at IS NULL OR c.expires_at>clock_timestamp()))
)
UPDATE worker_pools AS target
       SET status = sqlc.arg(target_status)::text,
           claim_version = target.claim_version + 1,
           updated_at = now()
      FROM worker_groups
     WHERE target.id = sqlc.arg(worker_pool_id)
       AND target.worker_group_id = sqlc.arg(worker_group_id)
       AND target.claim_version = sqlc.arg(expected_pool_claim_version)
       AND worker_groups.id = target.worker_group_id
       AND worker_groups.status IN ('active', 'paused', 'draining')
       AND worker_groups.primary_pool_id IS DISTINCT FROM target.id
       AND (
           (
               sqlc.arg(target_status)::text = 'draining'
               AND target.status = 'active'
               AND NOT EXISTS (
                   SELECT 1
                     FROM restore_profiles
                    WHERE restore_profiles.worker_group_id = target.worker_group_id
                      AND target.vm_platform_id = restore_profiles.vm_platform_id
                      AND EXISTS (
                          SELECT 1 FROM worker_pool_cpu_shapes AS target_shape
                           WHERE target_shape.worker_pool_id = target.id
                             AND target_shape.vcpu_count = restore_profiles.vm_vcpu_count
                             AND target_shape.cpu_config_digest = restore_profiles.cpu_config_digest
                      )
                      AND target.per_vm_cpu_millis >= restore_profiles.requested_cpu_millis
                      AND target.per_vm_memory_bytes >= restore_profiles.requested_memory_bytes
                      AND target.per_vm_guest_ephemeral_disk_bytes >= restore_profiles.requested_guest_ephemeral_disk_bytes
                      AND NOT EXISTS (
                          SELECT 1
                            FROM worker_pools AS supplier
                           WHERE supplier.worker_group_id = target.worker_group_id
                             AND supplier.id <> target.id
                             AND supplier.status = 'active'
                             AND supplier.vm_platform_id = restore_profiles.vm_platform_id
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
           OR (
               sqlc.arg(target_status)::text = 'disabled'
               AND (
                   (
                       target.status = 'pending'
                       AND NOT EXISTS (
                           SELECT 1 FROM worker_hosts
                            WHERE worker_hosts.worker_group_id = target.worker_group_id
                              AND worker_hosts.worker_pool_id = target.id
                              AND worker_hosts.status IN ('registering', 'active', 'draining')
                       )
                       AND NOT EXISTS (
                           SELECT 1
                             FROM worker_hosts
                            WHERE worker_hosts.worker_group_id = target.worker_group_id
                              AND worker_hosts.worker_pool_id = target.id
                              AND (
                                  EXISTS (
                                      SELECT 1 FROM computer_instances
                                       WHERE computer_instances.worker_group_id = worker_hosts.worker_group_id
                                         AND computer_instances.worker_host_id = worker_hosts.id
                                         AND computer_instances.reclaimed_at IS NULL
                                  )
                                  OR EXISTS (
                                      SELECT 1 FROM run_leases
                                       WHERE run_leases.worker_group_id = worker_hosts.worker_group_id
                                         AND run_leases.worker_host_id = worker_hosts.id
                                         AND run_leases.status IN ('assigned', 'starting', 'running', 'checkpointing', 'finalizing')
                                  )
                                  OR EXISTS (
                                      SELECT 1 FROM computer_commands JOIN computer_instances command_instance ON command_instance.id=computer_commands.computer_instance_id
                                       WHERE command_instance.worker_group_id = worker_hosts.worker_group_id
                                         AND command_instance.worker_host_id = worker_hosts.id
                                         AND computer_commands.process_reconciled_at IS NULL
                                  )
                              )
                       )
                   )
                   OR (
                       target.status = 'draining'
                       AND NOT EXISTS (
                           SELECT 1 FROM worker_hosts
                            WHERE worker_hosts.worker_group_id = target.worker_group_id
                              AND worker_hosts.worker_pool_id = target.id
                              AND worker_hosts.status IN ('registering', 'active', 'draining')
                       )
                       AND NOT EXISTS (
                           SELECT 1
                             FROM worker_hosts
                            WHERE worker_hosts.worker_group_id = target.worker_group_id
                              AND worker_hosts.worker_pool_id = target.id
                              AND (
                                  EXISTS (
                                      SELECT 1 FROM computer_instances
                                       WHERE computer_instances.worker_group_id = worker_hosts.worker_group_id
                                         AND computer_instances.worker_host_id = worker_hosts.id
                                         AND computer_instances.reclaimed_at IS NULL
                                  )
                                  OR EXISTS (
                                      SELECT 1 FROM run_leases
                                       WHERE run_leases.worker_group_id = worker_hosts.worker_group_id
                                         AND run_leases.worker_host_id = worker_hosts.id
                                         AND run_leases.status IN ('assigned', 'starting', 'running', 'checkpointing', 'finalizing')
                                  )
                                  OR EXISTS (
                                      SELECT 1 FROM computer_commands JOIN computer_instances command_instance ON command_instance.id=computer_commands.computer_instance_id
                                       WHERE command_instance.worker_group_id = worker_hosts.worker_group_id
                                         AND command_instance.worker_host_id = worker_hosts.id
                                         AND computer_commands.process_reconciled_at IS NULL
                                  )
                              )
                       )
                       AND NOT EXISTS (
                           SELECT 1
                             FROM restore_profiles
	                            WHERE restore_profiles.worker_group_id = target.worker_group_id
	                              AND target.vm_platform_id = restore_profiles.vm_platform_id
                              AND EXISTS (
                                  SELECT 1 FROM worker_pool_cpu_shapes AS target_shape
                                   WHERE target_shape.worker_pool_id = target.id
                                     AND target_shape.vcpu_count = restore_profiles.vm_vcpu_count
                                     AND target_shape.cpu_config_digest = restore_profiles.cpu_config_digest
                              )
                              AND target.per_vm_cpu_millis >= restore_profiles.requested_cpu_millis
                              AND target.per_vm_memory_bytes >= restore_profiles.requested_memory_bytes
                              AND target.per_vm_guest_ephemeral_disk_bytes >= restore_profiles.requested_guest_ephemeral_disk_bytes
                              AND NOT EXISTS (
                                  SELECT 1
                                    FROM worker_pools AS supplier
                                   WHERE supplier.worker_group_id = target.worker_group_id
                                     AND supplier.id <> target.id
                                     AND supplier.status = 'active'
                                     AND supplier.vm_platform_id = restore_profiles.vm_platform_id
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
RETURNING target.*;

-- name: ListCapacityWorkerPools :many
SELECT worker_pools.id,
       worker_pools.worker_group_id,
       worker_pools.name,
       worker_pools.vm_platform_id,
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
             FROM worker_hosts
            WHERE worker_hosts.worker_pool_id = worker_pools.id
              AND worker_hosts.worker_group_id = worker_pools.worker_group_id
              AND worker_hosts.status = 'registering'
       ), 0)::bigint AS registering_workers,
       COALESCE((
           SELECT count(*)
             FROM worker_hosts
            WHERE worker_hosts.worker_pool_id = worker_pools.id
              AND worker_hosts.worker_group_id = worker_pools.worker_group_id
              AND worker_hosts.status = 'active'
       ), 0)::bigint AS active_workers
  FROM worker_pools
 WHERE worker_pools.worker_group_id = sqlc.arg(worker_group_id)
   AND worker_pools.id = ANY(sqlc.arg(worker_pool_ids)::uuid[])
   AND worker_pools.status = 'active'
 ORDER BY worker_pools.id;

-- name: LockWorkerPool :one
SELECT *
  FROM worker_pools
 WHERE worker_group_id = sqlc.arg(worker_group_id)
   AND id = sqlc.arg(worker_pool_id)
 FOR UPDATE;

-- name: RequireCheckpointRestoreSupplier :one
SELECT supplier.id
FROM computer_instances i
JOIN worker_hosts h ON h.id=i.worker_host_id AND h.worker_group_id=i.worker_group_id
 AND h.worker_pool_id=sqlc.arg(source_worker_pool_id)
JOIN worker_groups g ON g.id=i.worker_group_id AND g.status IN ('active','paused','draining')
JOIN worker_pools supplier ON supplier.worker_group_id=i.worker_group_id AND supplier.status='active'
 AND supplier.vm_platform_id=i.vm_platform_id
 AND supplier.per_vm_cpu_millis>=i.reserved_cpu_millis
 AND supplier.per_vm_memory_bytes>=i.reserved_memory_bytes
 AND supplier.per_vm_guest_ephemeral_disk_bytes>=i.reserved_guest_ephemeral_disk_bytes
JOIN worker_pool_cpu_shapes shape ON shape.worker_pool_id=supplier.id
 AND shape.vcpu_count=i.vm_vcpu_count AND shape.cpu_config_digest=i.cpu_config_digest
WHERE i.id=sqlc.arg(computer_instance_id) AND i.worker_group_id=sqlc.arg(worker_group_id)
 AND i.worker_host_id=sqlc.arg(worker_host_id) AND i.worker_epoch=sqlc.arg(worker_epoch)
 AND i.writer_generation=sqlc.arg(writer_generation) AND i.reclaimed_at IS NULL
 AND i.admission_state='checkpointing' AND i.capture_checkpoint_id=sqlc.arg(checkpoint_id)
LIMIT 1;

-- name: GetWorkerHostPoolID :one
SELECT worker_pool_id
  FROM worker_hosts
 WHERE id = sqlc.arg(worker_host_id)
   AND worker_group_id = sqlc.arg(worker_group_id)
   AND current_epoch = sqlc.arg(worker_epoch);

-- name: LockWorkerHostForActivation :one
SELECT *
  FROM worker_hosts
 WHERE id = sqlc.arg(worker_host_id)
   AND worker_group_id = sqlc.arg(worker_group_id)
   AND worker_pool_id = sqlc.arg(worker_pool_id)
   AND current_epoch = sqlc.arg(worker_epoch)
 FOR UPDATE;

-- name: UpsertVMPlatform :one
INSERT INTO vm_platforms (
    id, arch, contract, descriptor_digest,
    firecracker_digest, firecracker_version, snapshot_format_version,
    host_kernel_release, cpu_template_kind, cpu_template_digest,
    kernel_digest, initramfs_digest, rootfs_digest, last_seen_at
)
VALUES (
    sqlc.arg(id), sqlc.arg(arch), sqlc.arg(contract),
    sqlc.arg(descriptor_digest), sqlc.arg(firecracker_digest),
    sqlc.arg(firecracker_version), sqlc.arg(snapshot_format_version),
    sqlc.arg(host_kernel_release), sqlc.arg(cpu_template_kind),
    sqlc.narg(cpu_template_digest), sqlc.arg(kernel_digest),
    sqlc.arg(initramfs_digest), sqlc.arg(rootfs_digest), now()
)
ON CONFLICT (id) DO UPDATE SET last_seen_at = now()
 WHERE vm_platforms.arch = EXCLUDED.arch
   AND vm_platforms.contract = EXCLUDED.contract
   AND vm_platforms.descriptor_digest = EXCLUDED.descriptor_digest
   AND vm_platforms.firecracker_digest = EXCLUDED.firecracker_digest
   AND vm_platforms.firecracker_version = EXCLUDED.firecracker_version
   AND vm_platforms.snapshot_format_version = EXCLUDED.snapshot_format_version
   AND vm_platforms.host_kernel_release = EXCLUDED.host_kernel_release
   AND vm_platforms.cpu_template_kind = EXCLUDED.cpu_template_kind
   AND vm_platforms.cpu_template_digest IS NOT DISTINCT FROM EXCLUDED.cpu_template_digest
   AND vm_platforms.kernel_digest = EXCLUDED.kernel_digest
   AND vm_platforms.initramfs_digest = EXCLUDED.initramfs_digest
   AND vm_platforms.rootfs_digest = EXCLUDED.rootfs_digest
RETURNING *;

-- name: InsertWorkerPoolCPUShape :execrows
INSERT INTO worker_pool_cpu_shapes (worker_pool_id, vcpu_count, cpu_config_digest)
SELECT worker_pools.id, sqlc.arg(vcpu_count), sqlc.arg(cpu_config_digest)
  FROM worker_pools
 WHERE worker_pools.id = sqlc.arg(worker_pool_id)
   AND worker_pools.status = 'pending';

-- name: ListWorkerPoolCPUShapes :many
SELECT *
  FROM worker_pool_cpu_shapes
 WHERE worker_pool_id = sqlc.arg(worker_pool_id)
 ORDER BY vcpu_count;

-- name: SealWorkerPool :one
UPDATE worker_pools
   SET status = 'active',
       vm_platform_id = sqlc.arg(vm_platform_id),
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
   AND status = 'pending'
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
       AND worker_pools.status = 'active'
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

-- name: TransitionWorkerGroupStatus :one
WITH transitioned AS (
    UPDATE worker_groups
       SET status = sqlc.arg(target_status),
           primary_pool_id = CASE
               WHEN sqlc.arg(target_status)::text = 'draining' THEN NULL
               ELSE worker_groups.primary_pool_id
           END,
           claim_version = worker_groups.claim_version + 1,
           updated_at = now()
     WHERE worker_groups.id = sqlc.arg(worker_group_id)
       AND worker_groups.claim_version = sqlc.arg(expected_claim_version)
       AND (
           (worker_groups.status = 'active' AND sqlc.arg(target_status)::text IN ('paused', 'draining'))
           OR (worker_groups.status = 'paused' AND sqlc.arg(target_status)::text IN ('active', 'draining'))
           OR (
               worker_groups.status = 'draining'
               AND sqlc.arg(target_status)::text = 'disabled'
               AND NOT EXISTS (
                   SELECT 1 FROM worker_pools
                    WHERE worker_pools.worker_group_id = worker_groups.id
                      AND worker_pools.status IN ('pending', 'active', 'draining')
               )
               AND NOT EXISTS (
                   SELECT 1 FROM worker_hosts
                    WHERE worker_hosts.worker_group_id = worker_groups.id
                      AND worker_hosts.status IN ('registering', 'active', 'draining')
               )
               AND NOT EXISTS (
                   SELECT 1 FROM run_leases
                    WHERE run_leases.worker_group_id = worker_groups.id
                      AND run_leases.status IN ('assigned', 'starting', 'running', 'checkpointing', 'finalizing')
               )
               AND NOT EXISTS (
                   SELECT 1 FROM computer_instances
                    WHERE computer_instances.worker_group_id = worker_groups.id
                      AND computer_instances.reclaimed_at IS NULL
               )
               AND NOT EXISTS (
                   SELECT 1 FROM computer_commands JOIN computer_instances command_instance ON command_instance.id=computer_commands.computer_instance_id
                    WHERE command_instance.worker_group_id = worker_groups.id
                      AND computer_commands.process_reconciled_at IS NULL
               )
           )
       )
    RETURNING worker_groups.id, worker_groups.status, worker_groups.claim_version
)
SELECT id, status, claim_version, true AS transition_applied
  FROM transitioned
UNION ALL
SELECT worker_groups.id, worker_groups.status, worker_groups.claim_version,
       false AS transition_applied
  FROM worker_groups
 WHERE worker_groups.id = sqlc.arg(worker_group_id)
   AND worker_groups.status = sqlc.arg(target_status)::text
   AND worker_groups.claim_version = sqlc.arg(expected_claim_version) + 1
   AND NOT EXISTS (SELECT 1 FROM transitioned)
LIMIT 1;

-- name: GetWorkerHostStatusByResource :one
SELECT id, resource_id, worker_group_id, worker_pool_id, status, claim_version, current_epoch
  FROM worker_hosts
 WHERE worker_group_id = sqlc.arg(worker_group_id)
   AND resource_id = sqlc.arg(resource_id)
 ORDER BY (status IN ('registering', 'active', 'draining')) DESC, created_at DESC
 LIMIT 1;

-- name: GetCapacityWorkerHost :one
SELECT id, resource_id, worker_group_id, worker_pool_id, status, claim_version, current_epoch,
       draining_at, termination_ready_at, lost_at,
       created_at, updated_at
  FROM worker_hosts
 WHERE id = sqlc.arg(worker_host_id);

-- name: ListCapacityWorkerHosts :many
WITH current_instances AS (
    SELECT DISTINCT ON (worker_group_id, resource_id)
           id, resource_id, worker_group_id, worker_pool_id, status, claim_version, current_epoch,
           draining_at, termination_ready_at, lost_at,
           created_at, updated_at
     FROM worker_hosts
     WHERE (sqlc.narg(worker_group_id)::uuid IS NULL OR worker_group_id = sqlc.narg(worker_group_id))
       AND (
           NOT sqlc.arg(has_unreclaimed_instance)::boolean
           OR EXISTS (
               SELECT 1
                 FROM computer_instances
                WHERE computer_instances.worker_host_id = worker_hosts.id
                  AND computer_instances.reclaimed_at IS NULL
           )
       )
       AND (
           cardinality(sqlc.arg(resource_ids)::text[]) = 0
           OR resource_id = ANY(sqlc.arg(resource_ids)::text[])
       )
     ORDER BY worker_group_id, resource_id,
              (status IN ('registering', 'active', 'draining')) DESC,
              created_at DESC, id DESC
)
SELECT *
  FROM current_instances
 WHERE (
       cardinality(sqlc.arg(statuses)::text[]) = 0
       OR status = ANY(sqlc.arg(statuses)::text[])
   )
 ORDER BY worker_group_id, resource_id
 LIMIT sqlc.arg(row_limit);

-- name: ListWorkerCapacityBins :many
WITH live_workers AS (
    SELECT worker_groups.id AS worker_group_id,
           worker_groups.primary_pool_id,
           worker_pools.id AS worker_pool_id,
           worker_hosts.id AS worker_host_id,
           worker_hosts.current_epoch AS worker_epoch,
           worker_hosts.vm_platform_id,
           vm_platforms.arch,
           vm_platforms.contract,
           worker_hosts.per_vm_cpu_millis,
           worker_hosts.per_vm_memory_bytes,
           worker_hosts.per_vm_guest_ephemeral_disk_bytes,
           worker_hosts.max_vm_slots,
           worker_hosts.max_vm_starts,
           worker_hosts.run_paused_reason,
           worker_hosts.vm_paused_reason,
           worker_hosts.epoch_cpu_millis,
           worker_hosts.epoch_memory_bytes,
           worker_hosts.epoch_guest_ephemeral_disk_bytes
      FROM worker_groups
      JOIN worker_hosts
        ON worker_hosts.worker_group_id = worker_groups.id
       AND worker_hosts.status = 'active'
       AND worker_hosts.current_epoch IS NOT NULL
      JOIN worker_pools
        ON worker_pools.id = worker_hosts.worker_pool_id
       AND worker_pools.worker_group_id = worker_hosts.worker_group_id
       AND worker_pools.status = 'active'
      JOIN vm_platforms
        ON vm_platforms.id = worker_hosts.vm_platform_id
     WHERE (sqlc.narg(worker_group_id)::uuid IS NULL OR worker_groups.id = sqlc.narg(worker_group_id))
       AND (sqlc.arg(region_id)::text = '' OR worker_groups.region_id = sqlc.arg(region_id))
       AND worker_groups.status = 'active'
       AND worker_hosts.observed_at >= transaction_timestamp()
           - sqlc.arg(observation_freshness_seconds)::bigint * interval '1 second'
     ORDER BY worker_hosts.id
     LIMIT sqlc.arg(row_limit)
), usage AS (
    SELECT live_workers.worker_host_id,
           COALESCE((SELECT sum(computer_instances.reserved_cpu_millis)
                      FROM computer_instances
                     WHERE computer_instances.worker_host_id = live_workers.worker_host_id
                        AND computer_instances.worker_epoch = live_workers.worker_epoch
                        AND computer_instances.reclaimed_at IS NULL), 0) AS cpu_millis,
           COALESCE((SELECT sum(computer_instances.reserved_memory_bytes)
                      FROM computer_instances
                     WHERE computer_instances.worker_host_id = live_workers.worker_host_id
                        AND computer_instances.worker_epoch = live_workers.worker_epoch
                        AND computer_instances.reclaimed_at IS NULL), 0) AS memory_bytes,
           COALESCE((SELECT sum(computer_instances.reserved_guest_ephemeral_disk_bytes)
                      FROM computer_instances
                     WHERE computer_instances.worker_host_id = live_workers.worker_host_id
                        AND computer_instances.worker_epoch = live_workers.worker_epoch
                        AND computer_instances.reclaimed_at IS NULL), 0) AS guest_ephemeral_disk_bytes,
           COALESCE((SELECT count(*) FROM computer_instances
                      WHERE computer_instances.worker_host_id = live_workers.worker_host_id
                        AND computer_instances.worker_epoch = live_workers.worker_epoch
                        AND (computer_instances.observed_state IN ('allocated', 'ready')
                             OR (computer_instances.observed_state IN ('failed', 'lost') AND computer_instances.reclaimed_at IS NULL))), 0)::bigint AS vm_slots,
           COALESCE((SELECT count(*) FROM computer_instances
                      WHERE computer_instances.worker_host_id = live_workers.worker_host_id
                        AND computer_instances.worker_epoch = live_workers.worker_epoch
                        AND computer_instances.observed_state = 'allocated'), 0)::bigint AS instance_starts
      FROM live_workers
)
SELECT live_workers.worker_group_id,
       live_workers.primary_pool_id,
       live_workers.worker_pool_id,
       live_workers.worker_host_id,
       live_workers.worker_epoch,
       live_workers.vm_platform_id,
       live_workers.arch,
       live_workers.contract,
       live_workers.per_vm_cpu_millis,
       live_workers.per_vm_memory_bytes,
       live_workers.per_vm_guest_ephemeral_disk_bytes,
       GREATEST(live_workers.epoch_cpu_millis - usage.cpu_millis, 0)::bigint AS available_cpu_millis,
       GREATEST(live_workers.epoch_memory_bytes - usage.memory_bytes, 0)::bigint AS available_memory_bytes,
       GREATEST(live_workers.epoch_guest_ephemeral_disk_bytes - usage.guest_ephemeral_disk_bytes, 0)::bigint AS available_guest_ephemeral_disk_bytes,
       GREATEST(live_workers.max_vm_slots - usage.vm_slots, 0)::bigint AS available_vm_slots,
       GREATEST(live_workers.max_vm_starts - usage.instance_starts, 0)::bigint AS available_instance_starts,
	       live_workers.run_paused_reason,
	       live_workers.vm_paused_reason,
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
 JOIN usage USING (worker_host_id)
 ORDER BY live_workers.worker_host_id;

-- name: SelectComputerInstanceCapacity :one
WITH compatible_workers AS (
    SELECT worker_groups.id AS worker_group_id,
           worker_hosts.id AS worker_host_id,
           worker_hosts.current_epoch AS worker_epoch,
           worker_hosts.vm_platform_id,
           worker_hosts.epoch_cpu_millis,
           worker_hosts.epoch_memory_bytes,
           worker_hosts.epoch_guest_ephemeral_disk_bytes,
           worker_hosts.max_vm_slots,
           worker_hosts.max_vm_starts
      FROM worker_groups
      JOIN worker_hosts
        ON worker_hosts.worker_group_id = worker_groups.id
       AND worker_hosts.status = 'active'
       AND worker_hosts.current_epoch IS NOT NULL
      JOIN worker_pools
        ON worker_pools.id = worker_hosts.worker_pool_id
       AND worker_pools.worker_group_id = worker_hosts.worker_group_id
       AND worker_pools.status = 'active'
      JOIN vm_platforms
        ON vm_platforms.id = worker_hosts.vm_platform_id
     WHERE worker_groups.region_id = sqlc.arg(region_id)
       AND worker_groups.status = 'active'
       AND worker_hosts.observed_at >= transaction_timestamp()
           - sqlc.arg(observation_freshness_seconds)::bigint * interval '1 second'
       AND worker_hosts.run_paused_reason IS NULL
       AND worker_hosts.vm_paused_reason IS NULL
       AND vm_platforms.arch = sqlc.arg(run_architecture)
       AND vm_platforms.contract = sqlc.arg(contract)
       AND worker_hosts.per_vm_cpu_millis >= sqlc.arg(required_cpu_millis)
       AND worker_hosts.per_vm_memory_bytes >= sqlc.arg(required_memory_bytes)
       AND worker_hosts.per_vm_guest_ephemeral_disk_bytes >= sqlc.arg(required_guest_ephemeral_disk_bytes)
       AND (
           (sqlc.arg(required_vm_platform_id)::text = ''
            AND worker_groups.primary_pool_id IS NOT NULL
            AND worker_hosts.worker_pool_id = worker_groups.primary_pool_id)
           OR
           (sqlc.arg(required_vm_platform_id)::text <> ''
            AND worker_groups.id = sqlc.arg(required_worker_group_id)
            AND worker_hosts.vm_platform_id = sqlc.arg(required_vm_platform_id)
            AND sqlc.arg(required_vm_vcpu_count)::integer > 0
            AND sqlc.arg(required_cpu_config_digest)::text <> ''
            AND EXISTS (
                SELECT 1
                  FROM worker_pool_cpu_shapes
                 WHERE worker_pool_cpu_shapes.worker_pool_id = worker_hosts.worker_pool_id
                   AND worker_pool_cpu_shapes.vcpu_count = sqlc.arg(required_vm_vcpu_count)
                   AND worker_pool_cpu_shapes.cpu_config_digest = sqlc.arg(required_cpu_config_digest)
            ))
       )
), available_workers AS (
    SELECT compatible_workers.*,
           usage.cpu_millis,
           usage.memory_bytes,
           usage.guest_ephemeral_disk_bytes,
           usage.vm_slots,
           usage.instance_starts
      FROM compatible_workers
     CROSS JOIN LATERAL (
         SELECT COALESCE((
                    SELECT sum(computer_instances.reserved_cpu_millis)
                      FROM computer_instances
                     WHERE computer_instances.worker_host_id = compatible_workers.worker_host_id
                       AND computer_instances.worker_epoch = compatible_workers.worker_epoch
                       AND computer_instances.reclaimed_at IS NULL
                ), 0) AS cpu_millis,
                COALESCE((
                    SELECT sum(computer_instances.reserved_memory_bytes)
                      FROM computer_instances
                     WHERE computer_instances.worker_host_id = compatible_workers.worker_host_id
                       AND computer_instances.worker_epoch = compatible_workers.worker_epoch
                       AND computer_instances.reclaimed_at IS NULL
                ), 0) AS memory_bytes,
                COALESCE((
                    SELECT sum(computer_instances.reserved_guest_ephemeral_disk_bytes)
                      FROM computer_instances
                     WHERE computer_instances.worker_host_id = compatible_workers.worker_host_id
                       AND computer_instances.worker_epoch = compatible_workers.worker_epoch
                       AND computer_instances.reclaimed_at IS NULL
                ), 0) AS guest_ephemeral_disk_bytes,
                COALESCE((
                    SELECT count(*)
                      FROM computer_instances
                     WHERE computer_instances.worker_host_id = compatible_workers.worker_host_id
                       AND computer_instances.worker_epoch = compatible_workers.worker_epoch
                       AND (computer_instances.observed_state IN ('allocated', 'ready')
                            OR (computer_instances.observed_state IN ('failed', 'lost')
                                AND computer_instances.reclaimed_at IS NULL))
                ), 0)::bigint AS vm_slots,
                COALESCE((
                    SELECT count(*)
                      FROM computer_instances
                     WHERE computer_instances.worker_host_id = compatible_workers.worker_host_id
                       AND computer_instances.worker_epoch = compatible_workers.worker_epoch
                       AND computer_instances.observed_state = 'allocated'
                ), 0)::bigint AS instance_starts
     ) AS usage
)
SELECT worker_group_id,
       worker_host_id,
       worker_epoch,
       vm_platform_id
  FROM available_workers
 WHERE epoch_cpu_millis - cpu_millis >= sqlc.arg(required_cpu_millis)
   AND epoch_memory_bytes - memory_bytes >= sqlc.arg(required_memory_bytes)
   AND epoch_guest_ephemeral_disk_bytes - guest_ephemeral_disk_bytes >= sqlc.arg(required_guest_ephemeral_disk_bytes)
   AND max_vm_slots > vm_slots
   AND max_vm_starts > instance_starts
 ORDER BY worker_host_id
 LIMIT 1;

-- name: ListComputerInstanceCapacityPressureCandidates :many
SELECT worker_groups.id AS worker_group_id,
       worker_hosts.id AS worker_host_id,
       worker_hosts.current_epoch AS worker_epoch,
       worker_hosts.vm_platform_id
  FROM worker_groups
  JOIN worker_hosts
    ON worker_hosts.worker_group_id = worker_groups.id
   AND worker_hosts.status = 'active'
   AND worker_hosts.current_epoch IS NOT NULL
  JOIN worker_pools
    ON worker_pools.id = worker_hosts.worker_pool_id
   AND worker_pools.worker_group_id = worker_hosts.worker_group_id
   AND worker_pools.status = 'active'
  JOIN vm_platforms
    ON vm_platforms.id = worker_hosts.vm_platform_id
 WHERE worker_groups.region_id = sqlc.arg(region_id)
   AND worker_groups.status = 'active'
   AND (sqlc.narg(after_worker_host_id)::uuid IS NULL
        OR worker_hosts.id > sqlc.narg(after_worker_host_id))
   AND worker_hosts.observed_at >= transaction_timestamp()
       - sqlc.arg(observation_freshness_seconds)::bigint * interval '1 second'
   AND worker_hosts.run_paused_reason IS NULL
   AND worker_hosts.vm_paused_reason IS NULL
   AND vm_platforms.arch = sqlc.arg(run_architecture)
   AND vm_platforms.contract = sqlc.arg(contract)
   AND worker_hosts.per_vm_cpu_millis >= sqlc.arg(required_cpu_millis)
   AND worker_hosts.per_vm_memory_bytes >= sqlc.arg(required_memory_bytes)
   AND worker_hosts.per_vm_guest_ephemeral_disk_bytes >= sqlc.arg(required_guest_ephemeral_disk_bytes)
   AND (
       (sqlc.arg(required_vm_platform_id)::text = ''
        AND worker_groups.primary_pool_id IS NOT NULL
        AND worker_hosts.worker_pool_id = worker_groups.primary_pool_id)
       OR
       (sqlc.arg(required_vm_platform_id)::text <> ''
        AND worker_groups.id = sqlc.arg(required_worker_group_id)
        AND worker_hosts.vm_platform_id = sqlc.arg(required_vm_platform_id)
        AND sqlc.arg(required_vm_vcpu_count)::integer > 0
        AND sqlc.arg(required_cpu_config_digest)::text <> ''
        AND EXISTS (
            SELECT 1
              FROM worker_pool_cpu_shapes
             WHERE worker_pool_cpu_shapes.worker_pool_id = worker_hosts.worker_pool_id
               AND worker_pool_cpu_shapes.vcpu_count = sqlc.arg(required_vm_vcpu_count)
               AND worker_pool_cpu_shapes.cpu_config_digest = sqlc.arg(required_cpu_config_digest)
        ))
   )
 ORDER BY worker_hosts.id
 LIMIT sqlc.arg(row_limit);

-- name: MarkWorkerHostLost :one
WITH target AS (
    UPDATE worker_hosts
       SET status = 'lost', claim_version = worker_hosts.claim_version + 1,
           lost_at = COALESCE(worker_hosts.lost_at, now()), updated_at = now()
     WHERE worker_hosts.worker_group_id = sqlc.arg(worker_group_id)
       AND worker_hosts.resource_id = sqlc.arg(resource_id)
       AND worker_hosts.claim_version = sqlc.arg(expected_claim_version)
       AND worker_hosts.status IN ('registering', 'active', 'draining')
    RETURNING worker_hosts.id, worker_hosts.resource_id,
              worker_hosts.worker_group_id, worker_hosts.status,
              worker_hosts.claim_version, worker_hosts.current_epoch
), revoked_host_secrets AS (
    UPDATE worker_host_secrets
       SET revoked_at = COALESCE(revoked_at, now())
      FROM target
     WHERE worker_host_secrets.worker_host_id = target.id
       AND worker_host_secrets.revoked_at IS NULL
    RETURNING worker_host_secrets.id
), lost_instances AS (
    UPDATE computer_instances
       SET observed_state = 'lost', observed_version = observed_version + 1,
           observed_at = now(), terminal_at = now(),
           terminal_reason_code = 'external_instance_drift',
           mount_state='lost', admission_state='closed', updated_at=now()
      FROM target
     WHERE computer_instances.worker_host_id = target.id
       AND computer_instances.worker_epoch = target.current_epoch
       AND computer_instances.reclaimed_at IS NULL
       AND computer_instances.observed_state IN ('allocated', 'ready')
    RETURNING computer_instances.id
), completed AS (
    SELECT target.id, target.resource_id, target.worker_group_id, target.status,
           target.claim_version, target.current_epoch, true AS transition_applied
      FROM target
     WHERE (SELECT count(*) FROM revoked_host_secrets) >= 0
       AND (SELECT count(*) FROM lost_instances) >= 0
)
SELECT * FROM completed
UNION ALL
SELECT worker_hosts.id, worker_hosts.resource_id,
       worker_hosts.worker_group_id, worker_hosts.status,
       worker_hosts.claim_version, worker_hosts.current_epoch,
       false AS transition_applied
  FROM worker_hosts
 WHERE worker_hosts.worker_group_id = sqlc.arg(worker_group_id)
   AND worker_hosts.resource_id = sqlc.arg(resource_id)
   AND worker_hosts.status = 'lost'
   AND worker_hosts.claim_version = sqlc.arg(expected_claim_version) + 1
   AND NOT EXISTS (
       SELECT 1
         FROM worker_hosts AS current_worker
        WHERE current_worker.worker_group_id = worker_hosts.worker_group_id
          AND current_worker.resource_id = worker_hosts.resource_id
          AND current_worker.status IN ('registering', 'active', 'draining')
   )
   AND NOT EXISTS (SELECT 1 FROM completed)
LIMIT 1;

-- name: ConfirmWorkerHostProviderAbsent :one
WITH target AS MATERIALIZED (
    SELECT worker_hosts.id
      FROM worker_hosts
     WHERE worker_hosts.id = sqlc.arg(worker_host_id)
       AND worker_hosts.status IN ('registering', 'active', 'draining', 'lost')
     FOR UPDATE
), transitioned AS (
    UPDATE worker_hosts
       SET status = 'lost',
           claim_version = worker_hosts.claim_version
               + CASE WHEN worker_hosts.status = 'lost' THEN 0 ELSE 1 END,
           lost_at = COALESCE(worker_hosts.lost_at, now()),
           updated_at = CASE
               WHEN worker_hosts.status = 'lost' THEN worker_hosts.updated_at
               ELSE now()
           END
      FROM target
     WHERE worker_hosts.id = target.id
    RETURNING worker_hosts.id, worker_hosts.resource_id,
              worker_hosts.worker_group_id, worker_hosts.worker_pool_id,
              worker_hosts.status, worker_hosts.claim_version,
              worker_hosts.current_epoch, worker_hosts.draining_at,
              worker_hosts.termination_ready_at, worker_hosts.lost_at,
              worker_hosts.created_at, worker_hosts.updated_at
), revoked_host_secrets AS (
    UPDATE worker_host_secrets
       SET revoked_at = COALESCE(worker_host_secrets.revoked_at, now())
      FROM transitioned
     WHERE worker_host_secrets.worker_host_id = transitioned.id
       AND worker_host_secrets.revoked_at IS NULL
    RETURNING worker_host_secrets.id

)
SELECT transitioned.id, transitioned.resource_id,
       transitioned.worker_group_id, transitioned.worker_pool_id,
       transitioned.status, transitioned.claim_version,
       transitioned.current_epoch, transitioned.draining_at,
       transitioned.termination_ready_at, transitioned.lost_at,
       transitioned.created_at, transitioned.updated_at
  FROM transitioned
 WHERE (SELECT count(*) FROM revoked_host_secrets) >= 0;

-- Provider absence is verified by the operation owner before this transaction.
-- Logical Run/Command outcomes are settled separately from physical exclusion.
-- name: ReconcileProviderAbsentWorkerInstances :one
WITH candidates AS MATERIALIZED (
 SELECT i.id FROM computer_instances i JOIN worker_hosts h ON h.id=i.worker_host_id
 WHERE h.id=sqlc.arg(worker_host_id) AND h.status='lost' AND i.reclaimed_at IS NULL
 ORDER BY i.id FOR UPDATE OF i
), reclaimed AS (
 UPDATE computer_instances i SET observed_state=CASE WHEN i.observed_state='failed' THEN 'failed' ELSE 'lost' END,
 observed_version=i.observed_version+1,observed_at=now(),terminal_at=coalesce(i.terminal_at,now()),
 terminal_reason_code=coalesce(i.terminal_reason_code,'external_instance_drift'),
 reclaimed_at=now(),reclaim_evidence=jsonb_build_object('method','provider_absent','completed_at',now()),
 mount_state='lost',admission_state='closed',updated_at=now()
 FROM candidates c WHERE i.id=c.id RETURNING i.id,i.writer_generation,i.reclaimed_at
), reconciled AS (
 UPDATE run_leases l SET process_reconciled_at=i.reclaimed_at,updated_at=now()
 FROM reclaimed i WHERE l.computer_instance_id=i.id AND l.writer_generation=i.writer_generation
 AND l.process_reconciled_at IS NULL RETURNING l.id
)
SELECT count(*) FROM reclaimed WHERE (SELECT count(*) FROM reconciled)>=0;

-- name: ActivateWorkerHost :one
UPDATE worker_hosts
   SET status = CASE
           WHEN worker_hosts.status = 'draining'
               OR worker_groups.status = 'draining'
               OR worker_pools.status = 'draining'
           THEN 'draining'
           ELSE 'active'
       END,
       vm_platform_id = sqlc.arg(vm_platform_id),
       epoch_cpu_millis = sqlc.arg(epoch_cpu_millis),
       epoch_memory_bytes = sqlc.arg(epoch_memory_bytes),
       epoch_guest_ephemeral_disk_bytes = sqlc.arg(epoch_guest_ephemeral_disk_bytes),
       per_vm_cpu_millis = sqlc.arg(per_vm_cpu_millis),
       per_vm_memory_bytes = sqlc.arg(per_vm_memory_bytes),
       per_vm_guest_ephemeral_disk_bytes = sqlc.arg(per_vm_guest_ephemeral_disk_bytes),
       max_vm_slots = sqlc.arg(max_vm_slots),
       max_vm_starts = sqlc.arg(max_vm_starts),
       cpu_environment = sqlc.arg(cpu_environment)::jsonb,
       cpu_environment_digest = sqlc.arg(cpu_environment_digest),
       activated_at = COALESCE(worker_hosts.activated_at, now()),
       draining_at = CASE
           WHEN worker_hosts.status = 'draining'
               OR worker_groups.status = 'draining'
               OR worker_pools.status = 'draining'
           THEN COALESCE(worker_hosts.draining_at, now())
           ELSE worker_hosts.draining_at
       END,
       updated_at = now()
  FROM worker_groups, worker_pools
 WHERE worker_hosts.id = sqlc.arg(worker_host_id)
   AND worker_hosts.worker_group_id = sqlc.arg(worker_group_id)
   AND worker_hosts.current_epoch = sqlc.arg(worker_epoch)
   AND worker_groups.id = worker_hosts.worker_group_id
   AND worker_groups.status IN ('active', 'paused', 'draining')
   AND worker_pools.id = worker_hosts.worker_pool_id
   AND worker_pools.worker_group_id = worker_hosts.worker_group_id
   AND worker_pools.status IN ('active', 'draining')
   AND NOT EXISTS (
       SELECT 1 FROM computer_instances
        WHERE computer_instances.worker_host_id = worker_hosts.id
          AND computer_instances.worker_epoch < worker_hosts.current_epoch
          AND computer_instances.reclaimed_at IS NULL
   )
   AND (
       worker_hosts.status = 'registering'
       OR (
           worker_hosts.status = 'draining'
           AND worker_hosts.vm_platform_id IS NULL
           AND worker_hosts.epoch_cpu_millis = 0
           AND worker_hosts.epoch_memory_bytes = 0
           AND worker_hosts.epoch_guest_ephemeral_disk_bytes = 0
           AND worker_hosts.per_vm_cpu_millis = 0
           AND worker_hosts.per_vm_memory_bytes = 0
           AND worker_hosts.per_vm_guest_ephemeral_disk_bytes = 0
           AND worker_hosts.max_vm_slots = 0
           AND worker_hosts.max_vm_starts = 0
           AND worker_hosts.cpu_environment IS NULL
           AND worker_hosts.cpu_environment_digest IS NULL
           AND worker_hosts.activated_at IS NULL
       )
       OR (
           worker_hosts.status IN ('active', 'draining')
           AND worker_hosts.vm_platform_id = sqlc.arg(vm_platform_id)::text
           AND worker_hosts.epoch_cpu_millis = sqlc.arg(epoch_cpu_millis)
           AND worker_hosts.epoch_memory_bytes = sqlc.arg(epoch_memory_bytes)
           AND worker_hosts.epoch_guest_ephemeral_disk_bytes = sqlc.arg(epoch_guest_ephemeral_disk_bytes)
           AND worker_hosts.per_vm_cpu_millis = sqlc.arg(per_vm_cpu_millis)
           AND worker_hosts.per_vm_memory_bytes = sqlc.arg(per_vm_memory_bytes)
           AND worker_hosts.per_vm_guest_ephemeral_disk_bytes = sqlc.arg(per_vm_guest_ephemeral_disk_bytes)
           AND worker_hosts.max_vm_slots = sqlc.arg(max_vm_slots)
           AND worker_hosts.max_vm_starts = sqlc.arg(max_vm_starts)
           AND worker_hosts.cpu_environment = sqlc.arg(cpu_environment)::jsonb
           AND worker_hosts.cpu_environment_digest = sqlc.arg(cpu_environment_digest)
       )
   )
RETURNING worker_hosts.*;

-- name: RecordWorkerObservation :one
UPDATE worker_hosts
   SET observed_at = transaction_timestamp(),
       run_paused_reason = sqlc.narg(run_paused_reason),
       vm_paused_reason = sqlc.narg(vm_paused_reason),
       updated_at = now()
 WHERE worker_hosts.id = sqlc.arg(worker_host_id)
   AND worker_hosts.worker_group_id = sqlc.arg(worker_group_id)
   AND worker_hosts.current_epoch = sqlc.arg(worker_epoch)
   AND worker_hosts.status IN ('active', 'draining')
RETURNING worker_hosts.*;

-- name: CompleteWorkerStartupRecovery :one
WITH target AS (
    SELECT worker_hosts.id, worker_hosts.worker_group_id, worker_hosts.current_epoch
      FROM worker_hosts
     WHERE worker_hosts.id = sqlc.arg(worker_host_id)
       AND worker_hosts.worker_group_id = sqlc.arg(worker_group_id)
       AND worker_hosts.current_epoch = sqlc.arg(worker_epoch)

	       AND (
	           worker_hosts.status = 'registering'
	           OR (
	               worker_hosts.status = 'draining'
	               AND worker_hosts.vm_platform_id IS NULL
	           )
       )
     FOR UPDATE
), quarantined AS (
    SELECT value::uuid AS id
      FROM jsonb_array_elements_text(sqlc.arg(recovery_evidence)::jsonb -> 'quarantined') AS value
), reclaimable_instances AS MATERIALIZED (
    SELECT computer_instances.id
      FROM computer_instances
      JOIN target
        ON target.id = computer_instances.worker_host_id
     WHERE computer_instances.worker_epoch < target.current_epoch
       AND computer_instances.reclaimed_at IS NULL
       AND computer_instances.id NOT IN (SELECT id FROM quarantined)
     ORDER BY computer_instances.id
       FOR UPDATE OF computer_instances
), reclaimed_instances AS (
    UPDATE computer_instances
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
           mount_state='lost', admission_state='closed', updated_at=now()
     WHERE computer_instances.id IN (SELECT id FROM reclaimable_instances)
    RETURNING computer_instances.id,computer_instances.writer_generation,computer_instances.reclaimed_at
), reconciled_processes AS (
    UPDATE run_leases l SET process_reconciled_at=i.reclaimed_at,updated_at=now()
      FROM reclaimed_instances i
     WHERE l.computer_instance_id=i.id AND l.writer_generation=i.writer_generation
       AND l.process_reconciled_at IS NULL
    RETURNING l.id
)
UPDATE worker_hosts
   SET updated_at = now()
  FROM target
 WHERE worker_hosts.id = target.id
   AND (SELECT count(*) FROM reclaimed_instances) >= 0
   AND (SELECT count(*) FROM reconciled_processes) >= 0
RETURNING worker_hosts.*;
