-- name: LockWorkerDrainCompletion :one
SELECT worker_hosts.id, worker_hosts.status, worker_hosts.claim_version,
       worker_hosts.current_epoch, worker_hosts.termination_ready_at
  FROM worker_hosts
  JOIN worker_groups ON worker_groups.id = worker_hosts.worker_group_id
 WHERE worker_hosts.id = sqlc.arg(worker_host_id)
   AND worker_hosts.worker_group_id = sqlc.arg(worker_group_id)
   AND worker_hosts.current_epoch = sqlc.arg(worker_epoch)
   AND worker_hosts.status IN ('draining', 'termination_ready')
   AND worker_groups.status IN ('active', 'paused', 'draining')
 FOR UPDATE OF worker_hosts;

-- name: CompleteWorkerDrain :one
WITH target AS MATERIALIZED (
    SELECT worker_hosts.*
      FROM worker_hosts
      JOIN worker_groups ON worker_groups.id = worker_hosts.worker_group_id
     WHERE worker_hosts.id = sqlc.arg(worker_host_id)
       AND worker_hosts.worker_group_id = sqlc.arg(worker_group_id)
       AND worker_hosts.current_epoch = sqlc.arg(worker_epoch)
       AND worker_hosts.status IN ('draining', 'termination_ready')
       AND worker_hosts.claim_version IN (
           sqlc.arg(expected_claim_version)::bigint,
           sqlc.arg(expected_claim_version)::bigint + 1
       )
       AND worker_groups.status IN ('active', 'paused', 'draining')
     FOR UPDATE OF worker_hosts
), eligible AS (
    SELECT drain_target.id
      FROM target AS drain_target
     WHERE drain_target.status = 'draining'
       AND drain_target.epoch_started_at IS NOT NULL
       AND EXISTS (
           SELECT 1 FROM worker_host_secrets
            WHERE worker_host_secrets.worker_host_id = drain_target.id
              AND worker_host_secrets.claim_version = drain_target.claim_version
              AND worker_host_secrets.revoked_at IS NULL
       )
       AND NOT EXISTS (
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
 FROM boot_seed_builds WHERE worker_host_id IS NOT NULL AND fenced_at IS NULL) allocation_usage WHERE worker_host_id=drain_target.id
       )
), completed AS (
    UPDATE worker_hosts
       SET status = 'termination_ready',
           claim_version = worker_hosts.claim_version + 1,
           termination_ready_at = now(),
           updated_at = now()
      FROM eligible
     WHERE worker_hosts.id = eligible.id
    RETURNING worker_hosts.id, worker_hosts.worker_group_id,
              worker_hosts.current_epoch, worker_hosts.status,
              worker_hosts.claim_version, worker_hosts.termination_ready_at
), revoked AS (
    UPDATE worker_host_secrets
       SET revoked_at = now()
      FROM completed
     WHERE worker_host_secrets.worker_host_id = completed.id
       AND worker_host_secrets.revoked_at IS NULL
    RETURNING worker_host_secrets.id
), result AS (
    SELECT completed.*
      FROM completed
     WHERE (SELECT count(*) FROM revoked) = 1
    UNION ALL
    SELECT target.id, target.worker_group_id, target.current_epoch, target.status,
           target.claim_version, target.termination_ready_at
      FROM target
     WHERE target.status = 'termination_ready'
       AND target.claim_version = sqlc.arg(expected_claim_version) + 1
       AND target.termination_ready_at IS NOT NULL
       AND NOT EXISTS (SELECT 1 FROM completed)
)
SELECT * FROM result;
