-- name: TransitionWorkerPoolLifecycle :one
WITH restore_profiles AS MATERIALIZED (
 SELECT DISTINCT h.worker_group_id,l.vm_platform_id,l.vm_vcpu_count,l.cpu_config_digest,
   l.reserved_cpu_millis AS requested_cpu_millis,l.reserved_memory_bytes AS requested_memory_bytes,
   l.reserved_scratch_bytes AS requested_guest_ephemeral_disk_bytes
 FROM computer_leases l JOIN worker_hosts h ON h.id=l.worker_host_id
 WHERE h.worker_group_id=sqlc.arg(worker_group_id) AND l.fenced_at IS NULL
 UNION
 SELECT h.worker_group_id,l.vm_platform_id,l.vm_vcpu_count,l.cpu_config_digest,
   l.reserved_cpu_millis,l.reserved_memory_bytes,l.reserved_scratch_bytes
 FROM computer_checkpoints c
 JOIN computer_leases l ON (l.environment_id,l.computer_id,l.epoch)=(c.environment_id,c.computer_id,c.source_lease_epoch)
 JOIN worker_hosts h ON h.id=l.worker_host_id
 WHERE h.worker_group_id=sqlc.arg(worker_group_id)
   AND c.status IN ('capturing','sealed','ready','restoring','aborting')
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
     (sqlc.arg(target_status)::text = 'draining' AND target.status = 'active')
     OR (sqlc.arg(target_status)::text = 'disabled' AND target.status IN ('pending','draining')
       AND NOT EXISTS (
         SELECT 1 FROM worker_hosts h
         WHERE h.worker_group_id=target.worker_group_id AND h.worker_pool_id=target.id
           AND (h.status IN ('registering','active','draining')
             OR EXISTS (SELECT 1 FROM computer_leases l
               WHERE l.worker_host_id=h.id AND l.fenced_at IS NULL)
             OR EXISTS (SELECT 1 FROM computer_preparations p
               WHERE p.worker_host_id=h.id AND p.fenced_at IS NULL))
       ))
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
RETURNING target.*;

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
                   SELECT 1 FROM computer_leases l JOIN worker_hosts h ON h.id=l.worker_host_id
                    WHERE h.worker_group_id=worker_groups.id AND l.fenced_at IS NULL
               )
               AND NOT EXISTS (
                   SELECT 1 FROM computer_preparations p JOIN worker_hosts h ON h.id=p.worker_host_id
                    WHERE h.worker_group_id=worker_groups.id AND p.fenced_at IS NULL
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
), completed AS (
    SELECT target.id, target.resource_id, target.worker_group_id, target.status,
           target.claim_version, target.current_epoch, true AS transition_applied
      FROM target
     WHERE (SELECT count(*) FROM revoked_host_secrets) >= 0
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

-- name: ListWorkerGroupRetainedProfiles :many
WITH retained_ids AS (
 SELECT l.environment_id,l.computer_id,l.epoch
 FROM computer_leases l JOIN worker_hosts h ON h.id=l.worker_host_id
 WHERE h.worker_group_id=sqlc.arg(worker_group_id) AND l.fenced_at IS NULL
 UNION
 SELECT l.environment_id,l.computer_id,l.epoch
 FROM computer_checkpoints c
 JOIN computer_leases l ON (l.environment_id,l.computer_id,l.epoch)=(c.environment_id,c.computer_id,c.source_lease_epoch)
 JOIN worker_hosts h ON h.id=l.worker_host_id
 WHERE h.worker_group_id=sqlc.arg(worker_group_id) AND c.status IN ('capturing','sealed','ready','restoring','aborting')
), retained AS (
 SELECT l.vm_platform_id,l.vm_vcpu_count,l.cpu_config_digest,l.reserved_cpu_millis,l.reserved_memory_bytes,l.reserved_scratch_bytes,l.fenced_at,
   (SELECT count(*) FROM computer_checkpoints c
    WHERE (c.environment_id,c.computer_id,c.source_lease_epoch)=(l.environment_id,l.computer_id,l.epoch)
      AND c.status IN ('capturing','sealed','aborting'))::bigint AS captures,
   (SELECT count(*) FROM computer_checkpoints c
    WHERE (c.environment_id,c.computer_id,c.source_lease_epoch)=(l.environment_id,l.computer_id,l.epoch)
      AND c.status IN ('ready','restoring'))::bigint AS parked
 FROM retained_ids r JOIN computer_leases l ON (l.environment_id,l.computer_id,l.epoch)=(r.environment_id,r.computer_id,r.epoch)
), profiles AS (
 SELECT vm_platform_id, vm_vcpu_count, cpu_config_digest,
   reserved_cpu_millis, reserved_memory_bytes, reserved_scratch_bytes AS reserved_guest_ephemeral_disk_bytes,
   count(*) FILTER (WHERE fenced_at IS NULL)::bigint AS live_instances,
   sum(captures)::bigint AS capturing_checkpoints, sum(parked)::bigint AS parked_checkpoints
 FROM retained WHERE fenced_at IS NULL OR captures>0 OR parked>0
 GROUP BY vm_platform_id,vm_vcpu_count,cpu_config_digest,reserved_cpu_millis,reserved_memory_bytes,reserved_scratch_bytes
)
SELECT p.*,
 (SELECT count(*) FROM worker_pools supplier
  WHERE supplier.worker_group_id=sqlc.arg(worker_group_id) AND supplier.status='active' AND supplier.sealed_at IS NOT NULL
    AND supplier.vm_platform_id=p.vm_platform_id AND supplier.per_vm_cpu_millis>=p.reserved_cpu_millis
    AND supplier.per_vm_memory_bytes>=p.reserved_memory_bytes
    AND supplier.per_vm_guest_ephemeral_disk_bytes>=p.reserved_guest_ephemeral_disk_bytes
    AND EXISTS (SELECT 1 FROM worker_pool_cpu_shapes s WHERE s.worker_pool_id=supplier.id
      AND s.vcpu_count=p.vm_vcpu_count AND s.cpu_config_digest=p.cpu_config_digest))::bigint AS eligible_pools
FROM profiles p
WHERE sqlc.narg(worker_pool_id)::uuid IS NULL OR EXISTS (
 SELECT 1 FROM worker_pools target WHERE target.id=sqlc.narg(worker_pool_id) AND target.worker_group_id=sqlc.arg(worker_group_id)
   AND target.vm_platform_id=p.vm_platform_id AND target.per_vm_cpu_millis>=p.reserved_cpu_millis
   AND target.per_vm_memory_bytes>=p.reserved_memory_bytes
   AND target.per_vm_guest_ephemeral_disk_bytes>=p.reserved_guest_ephemeral_disk_bytes
   AND EXISTS (SELECT 1 FROM worker_pool_cpu_shapes s WHERE s.worker_pool_id=target.id
     AND s.vcpu_count=p.vm_vcpu_count AND s.cpu_config_digest=p.cpu_config_digest)
)
ORDER BY vm_platform_id,vm_vcpu_count,cpu_config_digest,reserved_cpu_millis,reserved_memory_bytes,reserved_guest_ephemeral_disk_bytes
LIMIT sqlc.arg(row_limit);

-- name: DrainWorkerHost :one
WITH transitioned AS (
    UPDATE worker_hosts
       SET status = 'draining',
           claim_version = worker_hosts.claim_version + 1,
           draining_at = COALESCE(draining_at, now()),
           drain_reason = sqlc.arg(drain_reason)::text, updated_at = now()
     WHERE worker_hosts.id = sqlc.arg(id)
       AND worker_hosts.worker_group_id = sqlc.arg(worker_group_id)
       AND worker_hosts.current_epoch = sqlc.arg(expected_epoch)
       AND worker_hosts.claim_version = sqlc.arg(expected_claim_version)
       AND worker_hosts.status = 'active'
    RETURNING *
), target AS (
    SELECT transitioned.* FROM transitioned
    UNION ALL
    SELECT worker_hosts.*
      FROM worker_hosts
     WHERE worker_hosts.id = sqlc.arg(id)
       AND worker_hosts.worker_group_id = sqlc.arg(worker_group_id)
       AND worker_hosts.current_epoch = sqlc.arg(expected_epoch)
       AND worker_hosts.status = 'draining'
       AND worker_hosts.claim_version IN (sqlc.arg(expected_claim_version), sqlc.arg(expected_claim_version) + 1)
       AND NOT EXISTS (SELECT 1 FROM transitioned)
), host_secret_fence AS (
    UPDATE worker_host_secrets
       SET claim_version = target.claim_version
      FROM target
     WHERE worker_host_secrets.worker_host_id = target.id
       AND worker_host_secrets.revoked_at IS NULL
       AND worker_host_secrets.claim_version < target.claim_version
    RETURNING worker_host_secrets.id
)
SELECT target.*
  FROM target
 WHERE (SELECT count(*) FROM host_secret_fence) >= 0;

-- name: FenceWorkerHost :one
WITH target AS (
    UPDATE worker_hosts
       SET status = 'lost', claim_version = claim_version + 1,
           lost_at = COALESCE(lost_at, now()), updated_at = now()
     WHERE worker_hosts.id = sqlc.arg(id)
       AND worker_hosts.worker_group_id = sqlc.arg(worker_group_id)
       AND worker_hosts.current_epoch = sqlc.arg(expected_epoch)
       AND worker_hosts.claim_version = sqlc.arg(expected_claim_version)
       AND worker_hosts.status IN ('active', 'draining')
    RETURNING *
), revoked_host_secrets AS (
    UPDATE worker_host_secrets
       SET revoked_at = COALESCE(revoked_at, now())
      FROM target
     WHERE worker_host_secrets.worker_host_id = target.id
       AND worker_host_secrets.revoked_at IS NULL
    RETURNING worker_host_secrets.id
)
SELECT target.*
  FROM target
 WHERE (SELECT count(*) FROM revoked_host_secrets) >= 0
UNION ALL
SELECT worker_hosts.*
  FROM worker_hosts
 WHERE worker_hosts.id = sqlc.arg(id)
   AND worker_hosts.worker_group_id = sqlc.arg(worker_group_id)
   AND worker_hosts.current_epoch = sqlc.arg(expected_epoch)
   AND worker_hosts.status = 'lost'
   AND worker_hosts.claim_version = sqlc.arg(expected_claim_version) + 1
   AND NOT EXISTS (SELECT 1 FROM target)
LIMIT 1;
