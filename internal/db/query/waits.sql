-- name: GetWaitForRunWait :one
SELECT waits.*
  FROM waits
  JOIN run_waits ON run_waits.org_id = waits.org_id
                AND run_waits.project_id = waits.project_id
                AND run_waits.environment_id = waits.environment_id
                AND run_waits.wait_id = waits.id
 WHERE waits.org_id = sqlc.arg(org_id)
   AND waits.project_id = sqlc.arg(project_id)
   AND waits.environment_id = sqlc.arg(environment_id)
   AND run_waits.id = sqlc.arg(run_wait_id)::uuid;

-- name: ResolveImmediateTokenWaitForRunWait :one
WITH target AS MATERIALIZED (
    SELECT waits.id AS wait_id,
           waits.org_id,
           waits.project_id,
           waits.environment_id,
           run_waits.id AS run_wait_id,
           waits.state AS wait_state,
           tokens.state AS token_state,
           tokens.completion_data
      FROM waits
      JOIN run_waits ON run_waits.org_id = waits.org_id
                    AND run_waits.wait_id = waits.id
      JOIN tokens ON tokens.org_id = waits.org_id
                 AND tokens.project_id = waits.project_id
                 AND tokens.environment_id = waits.environment_id
                 AND tokens.id = waits.token_id
     WHERE waits.org_id = sqlc.arg(org_id)
       AND run_waits.worker_group_id = sqlc.arg(worker_group_id)
       AND waits.project_id = sqlc.arg(project_id)
       AND waits.environment_id = sqlc.arg(environment_id)
       AND run_waits.id = sqlc.arg(run_wait_id)::uuid
       AND waits.kind = 'token'
       AND (
           (
               waits.state = 'pending'
               AND tokens.state IN ('completed', 'cancelled', 'expired')
           )
           OR (
               waits.state IN ('completed', 'cancelled', 'expired')
           )
       )
       AND run_waits.state IN ('hot_waiting', 'checkpointed_waiting')
     FOR UPDATE OF waits, run_waits
),
updated_wait AS (
    UPDATE waits
       SET state = CASE
             WHEN waits.state <> 'pending' THEN waits.state
             WHEN target.token_state = 'completed' THEN 'completed'::wait_state
             WHEN target.token_state = 'cancelled' THEN 'cancelled'::wait_state
             ELSE 'expired'::wait_state
           END,
           result = CASE
             WHEN waits.state <> 'pending' THEN waits.result
             WHEN target.token_state = 'completed' THEN COALESCE(target.completion_data, 'null'::jsonb)
             ELSE waits.result
           END,
           completed_at = COALESCE(waits.completed_at, now()),
           updated_at = now()
      FROM target
     WHERE waits.org_id = target.org_id
       AND waits.id = target.wait_id
    RETURNING waits.*
),
updated_run_wait AS (
    UPDATE run_waits
       SET state = 'resuming',
           resuming_at = COALESCE(run_waits.resuming_at, now()),
           updated_at = now()
      FROM target
      JOIN updated_wait ON updated_wait.id = target.wait_id
     WHERE run_waits.org_id = target.org_id
       AND run_waits.id = target.run_wait_id
       AND run_waits.state IN ('hot_waiting', 'checkpointed_waiting')
    RETURNING run_waits.*
)
SELECT updated_run_wait.*
  FROM updated_run_wait;

-- name: ResolveDueTimerWaits :many
WITH due_waits AS MATERIALIZED (
    SELECT waits.id AS wait_id,
           waits.org_id,
           run_waits.id AS run_wait_id
      FROM waits
      JOIN run_waits ON run_waits.org_id = waits.org_id
                    AND run_waits.wait_id = waits.id
     WHERE waits.org_id = sqlc.arg(org_id)
       AND run_waits.worker_group_id = sqlc.arg(worker_group_id)
       AND waits.kind = 'timer'
       AND waits.state = 'pending'
       AND waits.completed_after <= now()
       AND run_waits.state IN ('hot_waiting', 'checkpointed_waiting')
     ORDER BY waits.completed_after ASC, waits.id ASC
     LIMIT sqlc.arg(limit_count)
     FOR UPDATE OF waits, run_waits
),
completed_waits AS (
    UPDATE waits
       SET state = 'completed',
           result = 'null'::jsonb,
           completed_at = COALESCE(waits.completed_at, now()),
           updated_at = now()
      FROM due_waits
     WHERE waits.org_id = due_waits.org_id
       AND waits.id = due_waits.wait_id
    RETURNING waits.id, waits.org_id
),
updated_run_waits AS (
    UPDATE run_waits
       SET state = 'resuming',
           resuming_at = COALESCE(run_waits.resuming_at, now()),
           updated_at = now()
      FROM due_waits
      JOIN completed_waits ON completed_waits.org_id = due_waits.org_id
                          AND completed_waits.id = due_waits.wait_id
     WHERE run_waits.org_id = due_waits.org_id
       AND run_waits.id = due_waits.run_wait_id
       AND run_waits.state IN ('hot_waiting', 'checkpointed_waiting')
    RETURNING run_waits.*
)
SELECT *
  FROM updated_run_waits;

-- name: ResolveDueTimerWaitForRunWait :one
WITH due_wait AS MATERIALIZED (
    SELECT waits.id AS wait_id,
           waits.org_id,
           run_waits.id AS run_wait_id
      FROM waits
      JOIN run_waits ON run_waits.org_id = waits.org_id
                    AND run_waits.wait_id = waits.id
     WHERE waits.org_id = sqlc.arg(org_id)
       AND waits.project_id = sqlc.arg(project_id)
       AND waits.environment_id = sqlc.arg(environment_id)
       AND run_waits.id = sqlc.arg(run_wait_id)::uuid
       AND waits.kind = 'timer'
       AND waits.state = 'pending'
       AND waits.completed_after <= now()
       AND run_waits.state IN ('hot_waiting', 'checkpointed_waiting')
     FOR UPDATE OF waits, run_waits
),
completed_wait AS (
    UPDATE waits
       SET state = 'completed',
           result = 'null'::jsonb,
           completed_at = COALESCE(waits.completed_at, now()),
           updated_at = now()
      FROM due_wait
     WHERE waits.org_id = due_wait.org_id
       AND waits.id = due_wait.wait_id
    RETURNING waits.id, waits.org_id
),
updated_run_wait AS (
    UPDATE run_waits
       SET state = 'resuming',
           resuming_at = COALESCE(run_waits.resuming_at, now()),
           updated_at = now()
      FROM due_wait
      JOIN completed_wait ON completed_wait.org_id = due_wait.org_id
                         AND completed_wait.id = due_wait.wait_id
     WHERE run_waits.org_id = due_wait.org_id
       AND run_waits.id = due_wait.run_wait_id
       AND run_waits.state IN ('hot_waiting', 'checkpointed_waiting')
    RETURNING run_waits.*
)
SELECT *
  FROM updated_run_wait;
