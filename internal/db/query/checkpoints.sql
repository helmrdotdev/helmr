-- name: GetRunRestorePayload :one
SELECT
    checkpoints.id AS checkpoint_id,
    checkpoints.manifest,
    run_suspensions.id AS run_suspension_id,
    restore_waitpoint.id AS waitpoint_id,
    restore_waitpoint.kind AS waitpoint_kind,
    run_suspensions.resolution_kind,
    run_suspensions.resolution
  FROM runs
  JOIN run_leases ON run_leases.org_id = runs.org_id
                      AND run_leases.run_id = runs.id
                      AND run_leases.id = runs.current_run_lease_id
                      AND run_leases.restore_checkpoint_id = runs.latest_checkpoint_id
  JOIN checkpoints ON checkpoints.org_id = runs.org_id
                  AND checkpoints.run_id = runs.id
                  AND checkpoints.id = runs.latest_checkpoint_id
  JOIN run_suspensions ON run_suspensions.org_id = runs.org_id
                AND run_suspensions.run_id = runs.id
                AND run_suspensions.checkpoint_id = checkpoints.id
  JOIN LATERAL (
      SELECT waitpoints.id,
             waitpoints.kind
        FROM run_suspension_waitpoints
        JOIN waitpoints ON waitpoints.org_id = run_suspension_waitpoints.org_id
                       AND waitpoints.id = run_suspension_waitpoints.waitpoint_id
       WHERE run_suspension_waitpoints.org_id = run_suspensions.org_id
         AND run_suspension_waitpoints.run_suspension_id = run_suspensions.id
       ORDER BY run_suspension_waitpoints.ordinal ASC
       LIMIT 1
  ) restore_waitpoint ON true
 WHERE runs.org_id = sqlc.arg(org_id)
   AND runs.id = sqlc.arg(run_id)
   AND runs.current_run_lease_id = sqlc.arg(run_lease_id)
   AND run_leases.worker_instance_id = sqlc.arg(worker_instance_id)
   AND run_leases.status IN ('leased', 'running')
   AND run_leases.lease_expires_at > now()
   AND runs.latest_checkpoint_id IS NOT NULL
   AND checkpoints.status = 'restoring'
   AND run_suspensions.status = 'resuming'
   AND run_suspensions.resolution_kind IS NOT NULL
 ORDER BY run_suspensions.resolved_at DESC
 LIMIT 1;
