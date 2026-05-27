-- name: GetRunRestorePayload :one
SELECT
    checkpoints.id AS checkpoint_id,
    checkpoints.manifest,
    waitpoints.id AS waitpoint_id,
    waitpoints.kind AS waitpoint_kind,
    waitpoints.resolution_kind,
    waitpoints.resolution
  FROM runs
  JOIN run_executions ON run_executions.org_id = runs.org_id
                      AND run_executions.run_id = runs.id
                      AND run_executions.id = runs.current_execution_id
                      AND run_executions.restore_checkpoint_id = runs.latest_checkpoint_id
  JOIN checkpoints ON checkpoints.org_id = runs.org_id
                  AND checkpoints.run_id = runs.id
                  AND checkpoints.id = runs.latest_checkpoint_id
  JOIN waitpoints ON waitpoints.org_id = runs.org_id
                 AND waitpoints.run_id = runs.id
                 AND waitpoints.checkpoint_id = checkpoints.id
 WHERE runs.org_id = sqlc.arg(org_id)
   AND runs.id = sqlc.arg(run_id)
   AND runs.current_execution_id = sqlc.arg(execution_id)
   AND run_executions.worker_instance_id = sqlc.arg(worker_instance_id)
   AND run_executions.status IN ('leased', 'running')
   AND run_executions.lease_expires_at > now()
   AND runs.latest_checkpoint_id IS NOT NULL
   AND checkpoints.status = 'restoring'
   AND waitpoints.status = 'resuming'
   AND waitpoints.resolution_kind IS NOT NULL
   AND EXISTS (
       SELECT 1
         FROM checkpoint_availability_leases
        WHERE checkpoint_availability_leases.org_id = checkpoints.org_id
          AND checkpoint_availability_leases.run_id = checkpoints.run_id
          AND checkpoint_availability_leases.checkpoint_id = checkpoints.id
          AND checkpoint_availability_leases.unavailable_at IS NULL
   )
 ORDER BY waitpoints.resolved_at DESC
 LIMIT 1;
