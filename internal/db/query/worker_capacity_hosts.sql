-- name: GetCapacityWorkerHost :one
SELECT id, resource_id, worker_group_id, worker_pool_id, status, claim_version, current_epoch,
       draining_at, drain_reason, termination_ready_at, lost_at,
       created_at, updated_at,
       (SELECT count(*) FROM (SELECT CASE WHEN EXISTS(SELECT 1 FROM checkpoints cp WHERE cp.environment_id=computer_leases.environment_id AND cp.computer_id=computer_leases.computer_id AND cp.source_lease_epoch=computer_leases.epoch AND cp.status='ready') THEN 'retained' ELSE 'computer' END::text kind,environment_id,computer_id owner_id,epoch,allocation_id,
 worker_host_id,worker_epoch,reserved_cpu_millis,reserved_memory_bytes,reserved_scratch_bytes,
 initialized_at IS NULL starting
 FROM computer_leases WHERE fenced_at IS NULL
 UNION ALL
 SELECT 'preparation',environment_id,id,executor_epoch,allocation_id,
 worker_host_id,worker_epoch,reserved_cpu_millis,reserved_memory_bytes,reserved_scratch_bytes,
 started_at IS NULL
 FROM image_builds WHERE worker_host_id IS NOT NULL AND fenced_at IS NULL
 UNION ALL
 SELECT 'seed',environment_id,id,executor_epoch,allocation_id,
 worker_host_id,worker_epoch,reserved_cpu_millis,reserved_memory_bytes,reserved_scratch_bytes,
 started_at IS NULL
 FROM boot_seed_builds WHERE worker_host_id IS NOT NULL AND fenced_at IS NULL) allocation_usage WHERE worker_host_id=worker_hosts.id)::bigint AS unreclaimed_instances,
       (SELECT count(*) FROM session_processes p JOIN computer_leases l ON (l.environment_id,l.computer_id,l.epoch)=(p.environment_id,p.computer_id,p.computer_lease_epoch) WHERE l.worker_host_id=worker_hosts.id AND l.fenced_at IS NULL AND p.fenced_at IS NULL)::bigint AS unreconciled_session_processes
  FROM worker_hosts
 WHERE worker_hosts.id = sqlc.arg(worker_host_id);

-- name: ListCapacityWorkerHosts :many
WITH current_instances AS (
    SELECT DISTINCT ON (worker_group_id, resource_id)
           id, resource_id, worker_group_id, worker_pool_id, status, claim_version, current_epoch,
           draining_at, drain_reason, termination_ready_at, lost_at,
           created_at, updated_at,
       (SELECT count(*) FROM (SELECT CASE WHEN EXISTS(SELECT 1 FROM checkpoints cp WHERE cp.environment_id=computer_leases.environment_id AND cp.computer_id=computer_leases.computer_id AND cp.source_lease_epoch=computer_leases.epoch AND cp.status='ready') THEN 'retained' ELSE 'computer' END::text kind,environment_id,computer_id owner_id,epoch,allocation_id,
 worker_host_id,worker_epoch,reserved_cpu_millis,reserved_memory_bytes,reserved_scratch_bytes,
 initialized_at IS NULL starting
 FROM computer_leases WHERE fenced_at IS NULL
 UNION ALL
 SELECT 'preparation',environment_id,id,executor_epoch,allocation_id,
 worker_host_id,worker_epoch,reserved_cpu_millis,reserved_memory_bytes,reserved_scratch_bytes,
 started_at IS NULL
 FROM image_builds WHERE worker_host_id IS NOT NULL AND fenced_at IS NULL
 UNION ALL
 SELECT 'seed',environment_id,id,executor_epoch,allocation_id,
 worker_host_id,worker_epoch,reserved_cpu_millis,reserved_memory_bytes,reserved_scratch_bytes,
 started_at IS NULL
 FROM boot_seed_builds WHERE worker_host_id IS NOT NULL AND fenced_at IS NULL) allocation_usage WHERE worker_host_id=worker_hosts.id)::bigint AS unreclaimed_instances,
       (SELECT count(*) FROM session_processes p JOIN computer_leases l ON (l.environment_id,l.computer_id,l.epoch)=(p.environment_id,p.computer_id,p.computer_lease_epoch) WHERE l.worker_host_id=worker_hosts.id AND l.fenced_at IS NULL AND p.fenced_at IS NULL)::bigint AS unreconciled_session_processes
     FROM worker_hosts
     WHERE (sqlc.narg(worker_group_id)::uuid IS NULL OR worker_group_id = sqlc.narg(worker_group_id))
       AND (
           NOT sqlc.arg(has_unreclaimed_instance)::boolean
           OR EXISTS (
               SELECT 1 FROM (SELECT CASE WHEN EXISTS(SELECT 1 FROM checkpoints cp WHERE cp.environment_id=computer_leases.environment_id AND cp.computer_id=computer_leases.computer_id AND cp.source_lease_epoch=computer_leases.epoch AND cp.status='ready') THEN 'retained' ELSE 'computer' END::text kind,environment_id,computer_id owner_id,epoch,allocation_id,
 worker_host_id,worker_epoch,reserved_cpu_millis,reserved_memory_bytes,reserved_scratch_bytes,
 initialized_at IS NULL starting
 FROM computer_leases WHERE fenced_at IS NULL
 UNION ALL
 SELECT 'preparation',environment_id,id,executor_epoch,allocation_id,
 worker_host_id,worker_epoch,reserved_cpu_millis,reserved_memory_bytes,reserved_scratch_bytes,
 started_at IS NULL
 FROM image_builds WHERE worker_host_id IS NOT NULL AND fenced_at IS NULL
 UNION ALL
 SELECT 'seed',environment_id,id,executor_epoch,allocation_id,
 worker_host_id,worker_epoch,reserved_cpu_millis,reserved_memory_bytes,reserved_scratch_bytes,
 started_at IS NULL
 FROM boot_seed_builds WHERE worker_host_id IS NOT NULL AND fenced_at IS NULL) allocation_usage WHERE worker_host_id=worker_hosts.id)
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

-- name: ConfirmWorkerHostProviderAbsent :one
WITH target AS MATERIALIZED (
    SELECT worker_hosts.id
      FROM worker_hosts
     WHERE worker_hosts.id = sqlc.arg(worker_host_id)
       AND worker_hosts.status IN ('registering', 'active', 'draining', 'termination_ready', 'lost')
     FOR UPDATE
), transitioned AS (
    UPDATE worker_hosts
       SET status = 'lost',
           termination_ready_at = NULL,
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
              worker_hosts.current_epoch, worker_hosts.draining_at, worker_hosts.drain_reason,
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
       transitioned.current_epoch, transitioned.draining_at, transitioned.drain_reason,
       transitioned.termination_ready_at, transitioned.lost_at,
       transitioned.created_at, transitioned.updated_at
  FROM transitioned
 WHERE (SELECT count(*) FROM revoked_host_secrets) >= 0;

-- name: ReadCapacitySupplyIdentity :one
-- One snapshot covers the full inventory. Limit overflow is unavailable evidence.
WITH pools AS (
 SELECT id,status,claim_version,sealed_at,vm_platform_id,capacity_cpu_millis,
 capacity_memory_bytes,capacity_guest_ephemeral_disk_bytes,per_vm_cpu_millis,
 per_vm_memory_bytes,per_vm_guest_ephemeral_disk_bytes,max_vm_slots
 FROM worker_pools WHERE worker_group_id=$1 AND status<>'disabled' ORDER BY id LIMIT 65
), hosts AS (
 SELECT DISTINCT ON (resource_id) id,resource_id,worker_pool_id,status,claim_version,current_epoch
 FROM worker_hosts WHERE worker_group_id=$1 AND status<>'lost' ORDER BY resource_id,created_at DESC,id DESC LIMIT 1001
)
SELECT jsonb_build_array(g.id,g.status,g.claim_version,g.primary_pool_id,
 (SELECT jsonb_agg(to_jsonb(pools) ORDER BY id) FROM pools),
 (SELECT jsonb_agg(to_jsonb(hosts) ORDER BY resource_id,id) FROM hosts))::text AS identity,
 ((SELECT count(*) FROM pools)<=64 AND (SELECT count(*) FROM hosts)<=1000)::boolean AS complete
FROM worker_groups g WHERE g.id=$1;
