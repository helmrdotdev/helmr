
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
-- name: LockWorkerPoolActiveHosts :many
SELECT id FROM worker_hosts
 WHERE worker_group_id=sqlc.arg(worker_group_id)
   AND worker_pool_id=sqlc.arg(worker_pool_id) AND status='active'
 ORDER BY id
 FOR UPDATE;
-- name: CountReadyWorkerPoolHosts :one
SELECT count(*)::bigint
  FROM worker_hosts h
  JOIN worker_pools p ON p.id = h.worker_pool_id AND p.worker_group_id = h.worker_group_id
 WHERE h.id = ANY(sqlc.arg(worker_host_ids)::uuid[])
   AND h.status = 'active'
   AND h.current_epoch IS NOT NULL
   AND h.activated_at IS NOT NULL
   AND h.observed_at >= clock_timestamp() - sqlc.arg(observation_freshness_seconds)::bigint * interval '1 second'
   AND h.run_paused_reason IS NULL AND h.vm_paused_reason IS NULL
   AND h.vm_platform_id = p.vm_platform_id
   AND h.per_vm_cpu_millis >= p.per_vm_cpu_millis
   AND h.per_vm_memory_bytes >= p.per_vm_memory_bytes
   AND h.per_vm_guest_ephemeral_disk_bytes >= p.per_vm_guest_ephemeral_disk_bytes
   AND h.max_vm_slots > 0 AND h.max_vm_starts > 0
;
-- name: SetWorkerGroupPrimaryPool :one
UPDATE worker_groups
   SET primary_pool_id = sqlc.arg(pool_id),
       claim_version = worker_groups.claim_version + 1,
       updated_at = now()
 WHERE worker_groups.id = sqlc.arg(worker_group_id)
   AND worker_groups.claim_version = sqlc.arg(expected_group_claim_version)
   AND worker_groups.status IN ('active', 'paused')
RETURNING worker_groups.*;
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
-- name: GetWorkerHostStatusByResource :one
SELECT id, resource_id, worker_group_id, worker_pool_id, status, claim_version, current_epoch
  FROM worker_hosts
 WHERE worker_group_id = sqlc.arg(worker_group_id)
   AND resource_id = sqlc.arg(resource_id)
 ORDER BY (status IN ('registering', 'active', 'draining')) DESC, created_at DESC
 LIMIT 1;
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
 SELECT h.worker_host_id,
   COALESCE(sum(a.cpu),0)::bigint cpu_millis,
   COALESCE(sum(a.memory),0)::bigint memory_bytes,
   COALESCE(sum(a.scratch),0)::bigint guest_ephemeral_disk_bytes,
   count(a.host_id)::bigint vm_slots,
   count(a.host_id) FILTER (WHERE a.starting)::bigint instance_starts
 FROM live_workers h LEFT JOIN (
  SELECT worker_host_id host_id,reserved_cpu_millis cpu,reserved_memory_bytes memory,reserved_scratch_bytes scratch,initialized_at IS NULL starting
  FROM computer_leases WHERE fenced_at IS NULL
  UNION ALL
  SELECT worker_host_id,reserved_cpu_millis,reserved_memory_bytes,reserved_scratch_bytes,delivered_at IS NULL
  FROM computer_preparations WHERE worker_host_id IS NOT NULL AND fenced_at IS NULL
 ) a ON a.host_id=h.worker_host_id
 GROUP BY h.worker_host_id
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
