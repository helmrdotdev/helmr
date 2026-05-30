-- name: RecordWaitpointResponse :one
WITH target_waitpoint AS (
    SELECT waitpoints.*,
           run_waits.id AS run_wait_id,
           run_waits.run_id
      FROM run_waits
      JOIN run_wait_dependencies ON run_wait_dependencies.org_id = run_waits.org_id
                                AND run_wait_dependencies.run_wait_id = run_waits.id
      JOIN waitpoints ON waitpoints.org_id = run_wait_dependencies.org_id
                     AND waitpoints.id = run_wait_dependencies.waitpoint_id
      JOIN runs ON runs.org_id = run_waits.org_id
               AND runs.id = run_waits.run_id
     WHERE run_waits.org_id = sqlc.arg(org_id)
       AND run_waits.run_id = sqlc.arg(run_id)
       AND waitpoints.id = sqlc.arg(waitpoint_id)
       AND waitpoints.kind = sqlc.arg(kind)
       AND waitpoints.status = 'pending'
       AND run_waits.status = 'waiting'
       AND runs.status = 'waiting'
       AND runs.current_execution_id IS NULL
       AND EXISTS (
           SELECT 1
             FROM run_queue_items
            WHERE run_queue_items.org_id = run_waits.org_id
              AND run_queue_items.run_id = run_waits.run_id
              AND run_queue_items.status = 'suspended'
       )
     FOR UPDATE OF run_waits, waitpoints, runs
)
INSERT INTO waitpoint_responses (
    id,
    org_id,
    run_id,
    run_wait_id,
    waitpoint_id,
    response_key,
    action,
    resolution_kind,
    resolution,
    event_payload,
    completed_by_principal,
    completed_via,
    external_subject,
    metadata
)
SELECT
    sqlc.arg(id),
    target_waitpoint.org_id,
    target_waitpoint.run_id,
    target_waitpoint.run_wait_id,
    target_waitpoint.id,
    sqlc.arg(response_key),
    sqlc.arg(action),
    sqlc.arg(resolution_kind),
    sqlc.arg(resolution),
    sqlc.arg(event_payload)::jsonb,
    sqlc.narg(completed_by_principal),
    sqlc.narg(completed_via),
    sqlc.narg(external_subject),
    sqlc.arg(metadata)::jsonb
  FROM target_waitpoint
ON CONFLICT (org_id, run_id, run_wait_id, waitpoint_id, response_key) DO UPDATE
   SET action = EXCLUDED.action,
       resolution_kind = EXCLUDED.resolution_kind,
       resolution = EXCLUDED.resolution,
       event_payload = EXCLUDED.event_payload,
       completed_by_principal = EXCLUDED.completed_by_principal,
       completed_via = EXCLUDED.completed_via,
       external_subject = EXCLUDED.external_subject,
       metadata = waitpoint_responses.metadata || EXCLUDED.metadata
RETURNING *;
