-- name: LockWorkerDrainCompletion :one
SELECT worker_instances.id
  FROM worker_instances
  JOIN worker_groups ON worker_groups.id = worker_instances.worker_group_id
 WHERE worker_instances.id = sqlc.arg(worker_instance_id)
   AND worker_instances.worker_group_id = sqlc.arg(worker_group_id)
   AND worker_instances.current_epoch = sqlc.arg(worker_epoch)
   AND worker_instances.state IN ('draining', 'disabled')
   AND worker_groups.state IN ('active', 'draining')
 FOR UPDATE OF worker_instances;

-- name: CompleteWorkerDrain :one
WITH target AS MATERIALIZED (
    SELECT worker_instances.*
      FROM worker_instances
      JOIN worker_groups ON worker_groups.id = worker_instances.worker_group_id
     WHERE worker_instances.id = sqlc.arg(worker_instance_id)
       AND worker_instances.worker_group_id = sqlc.arg(worker_group_id)
       AND worker_instances.current_epoch = sqlc.arg(worker_epoch)
       AND worker_instances.state IN ('draining', 'disabled')
       AND worker_groups.state IN ('active', 'draining')
     FOR UPDATE OF worker_instances
), completed AS (
    UPDATE worker_instances
       SET state = 'disabled',
           disabled_at = COALESCE(worker_instances.disabled_at, now()),
           drain_cleanup_fingerprint = sqlc.arg(cleanup_fingerprint),
           drain_cleanup_evidence = sqlc.arg(cleanup_evidence),
           updated_at = now()
      FROM target
     WHERE worker_instances.id = target.id
       AND target.state = 'draining'
       AND target.epoch_started_at IS NOT NULL
       AND sqlc.arg(observed_at)::timestamptz >= target.epoch_started_at
       AND sqlc.arg(observed_at)::timestamptz <= now() + interval '1 minute'
       AND NOT EXISTS (
           SELECT 1 FROM run_leases
            WHERE run_leases.worker_instance_id = target.id
              AND run_leases.worker_epoch = target.current_epoch
              AND run_leases.state IN ('assigned', 'starting', 'running', 'checkpointing')
       )
       AND NOT EXISTS (
           SELECT 1 FROM deployment_build_leases
            WHERE deployment_build_leases.worker_instance_id = target.id
              AND deployment_build_leases.worker_epoch = target.current_epoch
              AND deployment_build_leases.state IN ('assigned', 'starting', 'running')
       )
       AND NOT EXISTS (
           SELECT 1 FROM runtime_instances
            WHERE runtime_instances.worker_instance_id = target.id
              AND runtime_instances.worker_epoch = target.current_epoch
              AND runtime_instances.reclaimed_at IS NULL
       )
       AND NOT EXISTS (
           SELECT 1 FROM workspace_mounts
            WHERE workspace_mounts.worker_instance_id = target.id
              AND workspace_mounts.worker_epoch = target.current_epoch
              AND workspace_mounts.state IN ('mounting', 'mounted', 'unmounting')
       )
       AND NOT EXISTS (
           SELECT 1 FROM workspace_leases
            WHERE workspace_leases.worker_instance_id = target.id
              AND workspace_leases.worker_epoch = target.current_epoch
              AND workspace_leases.state IN ('active', 'releasing')
       )
       AND NOT EXISTS (
           SELECT 1 FROM workspace_process_operations
            WHERE workspace_process_operations.claimed_by_worker_instance_id = target.id
              AND workspace_process_operations.claimed_worker_epoch = target.current_epoch
              AND workspace_process_operations.state IN ('claimed', 'running')
       )
       AND NOT EXISTS (
           SELECT 1 FROM workspace_processes
            WHERE workspace_processes.worker_instance_id = target.id
              AND workspace_processes.worker_epoch = target.current_epoch
              AND workspace_processes.state IN ('starting', 'running', 'closing')
       )
       AND NOT EXISTS (
           SELECT 1 FROM worker_network_slots
            WHERE worker_network_slots.worker_instance_id = target.id
              AND worker_network_slots.worker_epoch = target.current_epoch
              AND worker_network_slots.state IN ('assigned', 'bound', 'reclaiming', 'quarantined')
       )
    RETURNING worker_instances.id, worker_instances.worker_group_id, worker_instances.state,
              worker_instances.drain_cleanup_fingerprint, worker_instances.drain_cleanup_evidence
), result AS (
    SELECT completed.* FROM completed
    UNION ALL
    SELECT target.id, target.worker_group_id, target.state,
           target.drain_cleanup_fingerprint, target.drain_cleanup_evidence
      FROM target
     WHERE target.state = 'disabled'
       AND target.drain_cleanup_fingerprint = sqlc.arg(cleanup_fingerprint)
       AND target.drain_cleanup_evidence = sqlc.arg(cleanup_evidence)
       AND NOT EXISTS (SELECT 1 FROM completed)
)
SELECT * FROM result;
