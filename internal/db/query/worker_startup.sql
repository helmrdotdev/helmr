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
       drain_reason = CASE
           WHEN worker_hosts.status = 'draining'
               OR worker_groups.status = 'draining'
               OR worker_pools.status = 'draining'
           THEN COALESCE(worker_hosts.drain_reason, 'admin')
           ELSE worker_hosts.drain_reason
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
       SELECT 1 FROM computer_leases
        WHERE computer_leases.worker_host_id = worker_hosts.id
          AND computer_leases.worker_epoch < worker_hosts.current_epoch
          AND computer_leases.fenced_at IS NULL
   )
   AND NOT EXISTS (
       SELECT 1 FROM computer_preparations p
        WHERE p.worker_host_id = worker_hosts.id
          AND p.worker_epoch < worker_hosts.current_epoch
          AND p.fenced_at IS NULL
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

-- Physical Computer closure is recorded before this supply acknowledgement.
-- Quarantined allocations remain unfenced and continue blocking activation.
-- name: CompleteWorkerStartupRecovery :one
WITH target AS (
 SELECT worker_hosts.id,worker_hosts.current_epoch FROM worker_hosts
 WHERE worker_hosts.id=sqlc.arg(worker_host_id) AND worker_hosts.worker_group_id=sqlc.arg(worker_group_id)
 AND worker_hosts.current_epoch=sqlc.arg(worker_epoch)
 AND (worker_hosts.status='registering' OR (worker_hosts.status='draining' AND worker_hosts.vm_platform_id IS NULL))
 FOR UPDATE
), quarantined AS (
 SELECT value::uuid AS id FROM jsonb_array_elements_text(sqlc.arg(recovery_evidence)::jsonb -> 'quarantined') AS value
)
UPDATE worker_hosts SET updated_at=now() FROM target
 WHERE worker_hosts.id=target.id AND NOT EXISTS (
 SELECT 1 FROM computer_leases l WHERE l.worker_host_id=target.id AND l.worker_epoch<target.current_epoch
 AND l.fenced_at IS NULL AND l.computer_instance_id NOT IN (SELECT quarantined.id FROM quarantined))
 AND NOT EXISTS (
 SELECT 1 FROM computer_preparations p WHERE p.worker_host_id=target.id AND p.worker_epoch<target.current_epoch
 AND p.fenced_at IS NULL AND p.instance_id NOT IN (SELECT quarantined.id FROM quarantined))
 RETURNING worker_hosts.*;

-- name: GetWorkerHostStatus :one
SELECT worker_hosts.*,
       vm_platforms.rootfs_digest,
       vm_platforms.contract,
       vm_platforms.arch,
	       COALESCE((
	           worker_hosts.status = 'active'
	           AND worker_groups.status = 'active'
           AND worker_hosts.observed_at >= transaction_timestamp()
               - sqlc.arg(observation_freshness_seconds)::bigint * interval '1 second'
           AND worker_hosts.run_paused_reason IS NULL
       ), false)::boolean AS run_ready,
	       COALESCE((
	           worker_hosts.status = 'active'
	           AND worker_groups.status = 'active'
           AND worker_hosts.observed_at >= transaction_timestamp()
               - sqlc.arg(observation_freshness_seconds)::bigint * interval '1 second'
           AND worker_hosts.vm_paused_reason IS NULL
       ), false)::boolean AS instance_ready,
       COALESCE((
           worker_hosts.status = 'active'
           AND worker_groups.status = 'active'
           AND worker_hosts.observed_at >= transaction_timestamp()
               - sqlc.arg(observation_freshness_seconds)::bigint * interval '1 second'
	           AND worker_hosts.run_paused_reason IS NULL
	           AND worker_hosts.vm_paused_reason IS NULL
       ), false)::boolean AS all_configured_roles_ready,
       (SELECT count(*)::int FROM computer_leases i
          WHERE i.worker_host_id=worker_hosts.id AND i.worker_epoch=worker_hosts.current_epoch
            AND i.fenced_at IS NULL)
       + (SELECT count(*)::int FROM computer_preparations p
          WHERE p.worker_host_id=worker_hosts.id AND p.worker_epoch=worker_hosts.current_epoch
            AND p.fenced_at IS NULL) AS active_instances
  FROM worker_hosts
  JOIN worker_groups ON worker_groups.id = worker_hosts.worker_group_id
  LEFT JOIN vm_platforms ON vm_platforms.id = worker_hosts.vm_platform_id
 WHERE worker_hosts.id = sqlc.arg(id)
   AND worker_hosts.worker_group_id = sqlc.arg(worker_group_id);
