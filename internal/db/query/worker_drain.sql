-- name: LockWorkerDrainCompletion :one
SELECT worker_instances.id, worker_instances.state, worker_instances.claim_version,
       worker_instances.current_epoch, worker_instances.termination_ready_at
  FROM worker_instances
  JOIN worker_groups ON worker_groups.id = worker_instances.worker_group_id
 WHERE worker_instances.id = sqlc.arg(worker_instance_id)
   AND worker_instances.worker_group_id = sqlc.arg(worker_group_id)
   AND worker_instances.current_epoch = sqlc.arg(worker_epoch)
   AND worker_instances.state IN ('draining', 'termination_ready')
   AND worker_groups.state IN ('active', 'paused', 'draining')
 FOR UPDATE OF worker_instances;

-- name: CompleteWorkerDrain :one
WITH target AS MATERIALIZED (
    SELECT worker_instances.*
      FROM worker_instances
      JOIN worker_groups ON worker_groups.id = worker_instances.worker_group_id
     WHERE worker_instances.id = sqlc.arg(worker_instance_id)
       AND worker_instances.worker_group_id = sqlc.arg(worker_group_id)
       AND worker_instances.current_epoch = sqlc.arg(worker_epoch)
       AND worker_instances.state IN ('draining', 'termination_ready')
       AND worker_instances.claim_version IN (
           sqlc.arg(expected_claim_version)::bigint,
           sqlc.arg(expected_claim_version)::bigint + 1
       )
       AND worker_groups.state IN ('active', 'paused', 'draining')
     FOR UPDATE OF worker_instances
), eligible AS (
    SELECT drain_target.id
      FROM target AS drain_target
     WHERE drain_target.state = 'draining'
       AND drain_target.epoch_started_at IS NOT NULL
       AND sqlc.arg(observed_at)::timestamptz >= drain_target.epoch_started_at
       AND sqlc.arg(observed_at)::timestamptz <= now() + interval '1 minute'
       AND EXISTS (
           SELECT 1 FROM worker_instance_credentials
            WHERE worker_instance_credentials.worker_instance_id = drain_target.id
              AND worker_instance_credentials.claim_version = drain_target.claim_version
              AND worker_instance_credentials.revoked_at IS NULL
       )
       AND NOT EXISTS (
           SELECT 1 FROM run_leases
            WHERE run_leases.worker_instance_id = drain_target.id
              AND run_leases.state IN ('assigned', 'starting', 'running', 'checkpointing', 'finalizing')
       )
       AND NOT EXISTS (
           SELECT 1 FROM runtime_instances
            WHERE runtime_instances.worker_instance_id = drain_target.id
              AND runtime_instances.reclaimed_at IS NULL
       )
       AND NOT EXISTS (
           SELECT 1 FROM workspace_mounts
            WHERE workspace_mounts.worker_instance_id = drain_target.id
              AND workspace_mounts.state IN ('mounting', 'mounted', 'unmounting')
       )
       AND NOT EXISTS (
           SELECT 1 FROM workspace_leases
            WHERE workspace_leases.worker_instance_id = drain_target.id
              AND workspace_leases.state IN ('active', 'releasing')
       )
       AND NOT EXISTS (
           SELECT 1 FROM workspace_processes
            WHERE workspace_processes.worker_instance_id = drain_target.id
              AND workspace_processes.state IN ('starting', 'running', 'exit_requested')
       )
), completed AS (
    UPDATE worker_instances
       SET state = 'termination_ready',
           claim_version = worker_instances.claim_version + 1,
           termination_ready_at = now(),
           updated_at = now()
      FROM eligible
     WHERE worker_instances.id = eligible.id
    RETURNING worker_instances.id, worker_instances.worker_group_id,
              worker_instances.current_epoch, worker_instances.state,
              worker_instances.claim_version, worker_instances.termination_ready_at
), revoked AS (
    UPDATE worker_instance_credentials
       SET revoked_at = now()
      FROM completed
     WHERE worker_instance_credentials.worker_instance_id = completed.id
       AND worker_instance_credentials.revoked_at IS NULL
    RETURNING worker_instance_credentials.id
), result AS (
    SELECT completed.*
      FROM completed
     WHERE (SELECT count(*) FROM revoked) = 1
    UNION ALL
    SELECT target.id, target.worker_group_id, target.current_epoch, target.state,
           target.claim_version, target.termination_ready_at
      FROM target
     WHERE target.state = 'termination_ready'
       AND target.claim_version = sqlc.arg(expected_claim_version) + 1
       AND target.termination_ready_at IS NOT NULL
       AND NOT EXISTS (SELECT 1 FROM completed)
)
SELECT * FROM result;
