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
           SELECT 1 FROM run_leases
            WHERE run_leases.worker_host_id = drain_target.id
              AND run_leases.process_reconciled_at IS NULL
       )
       AND NOT EXISTS (
           SELECT 1 FROM computer_instances
            WHERE computer_instances.worker_host_id = drain_target.id
              AND computer_instances.reclaimed_at IS NULL
       )
       AND NOT EXISTS (
           SELECT 1 FROM computer_commands c JOIN computer_instances i ON i.id=c.computer_instance_id
            WHERE i.worker_host_id=drain_target.id AND c.process_reconciled_at IS NULL
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
