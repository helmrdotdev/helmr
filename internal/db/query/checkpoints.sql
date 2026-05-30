-- name: GetRunRestorePayload :one
SELECT
    checkpoints.id AS checkpoint_id,
    checkpoints.manifest,
    restore_waitpoint.id AS waitpoint_id,
    restore_waitpoint.kind AS waitpoint_kind,
    run_waits.resolution_kind,
    run_waits.resolution
  FROM runs
  JOIN run_executions ON run_executions.org_id = runs.org_id
                      AND run_executions.run_id = runs.id
                      AND run_executions.id = runs.current_execution_id
                      AND run_executions.restore_checkpoint_id = runs.latest_checkpoint_id
  JOIN checkpoints ON checkpoints.org_id = runs.org_id
                  AND checkpoints.run_id = runs.id
                  AND checkpoints.id = runs.latest_checkpoint_id
  JOIN run_waits ON run_waits.org_id = runs.org_id
                AND run_waits.run_id = runs.id
                AND run_waits.checkpoint_id = checkpoints.id
  JOIN LATERAL (
      SELECT waitpoints.id,
             waitpoints.kind
        FROM run_wait_dependencies
        JOIN waitpoints ON waitpoints.org_id = run_wait_dependencies.org_id
                       AND waitpoints.id = run_wait_dependencies.waitpoint_id
       WHERE run_wait_dependencies.org_id = run_waits.org_id
         AND run_wait_dependencies.run_wait_id = run_waits.id
       ORDER BY run_wait_dependencies.ordinal ASC
       LIMIT 1
  ) restore_waitpoint ON true
 WHERE runs.org_id = sqlc.arg(org_id)
   AND runs.id = sqlc.arg(run_id)
   AND runs.current_execution_id = sqlc.arg(execution_id)
   AND run_executions.worker_instance_id = sqlc.arg(worker_instance_id)
   AND run_executions.status IN ('leased', 'running')
   AND run_executions.lease_expires_at > now()
   AND runs.latest_checkpoint_id IS NOT NULL
   AND checkpoints.status = 'restoring'
   AND run_waits.status = 'resuming'
   AND run_waits.resolution_kind IS NOT NULL
 ORDER BY run_waits.resolved_at DESC
 LIMIT 1;
