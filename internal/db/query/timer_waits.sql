-- name: CreateTimerWait :one
INSERT INTO timer_waits (
    id,
    org_id,
    worker_group_id,
    project_id,
    environment_id,
    run_wait_id,
    fire_at
)
SELECT sqlc.arg(id),
       run_waits.org_id,
       run_waits.worker_group_id,
       run_waits.project_id,
       run_waits.environment_id,
       run_waits.id,
       sqlc.arg(fire_at)
  FROM run_waits
 WHERE run_waits.org_id = sqlc.arg(org_id)
   AND run_waits.project_id = sqlc.arg(project_id)
   AND run_waits.environment_id = sqlc.arg(environment_id)
   AND run_waits.id = sqlc.arg(run_wait_id)
   AND run_waits.kind = 'timer'
RETURNING *;

-- name: ResolveDueTimerWaits :many
WITH due_waits AS (
    SELECT timer_waits.id,
           timer_waits.org_id,
           timer_waits.run_wait_id
      FROM timer_waits
      JOIN run_waits ON run_waits.org_id = timer_waits.org_id
                    AND run_waits.id = timer_waits.run_wait_id
     WHERE timer_waits.org_id = sqlc.arg(org_id)
       AND timer_waits.worker_group_id = sqlc.arg(worker_group_id)
       AND timer_waits.fire_at <= now()
       AND run_waits.state IN ('live_waiting', 'checkpointed_waiting')
     ORDER BY timer_waits.fire_at ASC, timer_waits.id ASC
     LIMIT sqlc.arg(limit_count)
     FOR UPDATE OF run_waits
),
resolved_wait AS (
    UPDATE run_waits
       SET state = CASE
             WHEN run_waits.state = 'live_waiting' THEN 'resolved_live'::run_wait_state
             WHEN run_waits.state = 'checkpointed_waiting' THEN 'resolved_checkpointed'::run_wait_state
             ELSE run_waits.state
           END,
           resolved_at = now(),
           updated_at = now()
      FROM due_waits
     WHERE run_waits.org_id = due_waits.org_id
       AND run_waits.id = due_waits.run_wait_id
       AND run_waits.state IN ('live_waiting', 'checkpointed_waiting')
    RETURNING run_waits.*
)
SELECT resolved_wait.*
  FROM resolved_wait;

-- name: ResolveDueTimerWaitForRunWait :one
WITH due_wait AS (
    SELECT timer_waits.id,
           timer_waits.org_id,
           timer_waits.run_wait_id
      FROM timer_waits
      JOIN run_waits ON run_waits.org_id = timer_waits.org_id
                    AND run_waits.id = timer_waits.run_wait_id
     WHERE timer_waits.org_id = sqlc.arg(org_id)
       AND timer_waits.project_id = sqlc.arg(project_id)
       AND timer_waits.environment_id = sqlc.arg(environment_id)
       AND timer_waits.run_wait_id = sqlc.arg(run_wait_id)
       AND timer_waits.fire_at <= now()
       AND run_waits.state IN ('live_waiting', 'checkpointed_waiting')
     FOR UPDATE OF run_waits
),
resolved_wait AS (
    UPDATE run_waits
       SET state = CASE
             WHEN run_waits.state = 'live_waiting' THEN 'resolved_live'::run_wait_state
             WHEN run_waits.state = 'checkpointed_waiting' THEN 'resolved_checkpointed'::run_wait_state
             ELSE run_waits.state
           END,
           resolved_at = now(),
           updated_at = now()
      FROM due_wait
     WHERE run_waits.org_id = due_wait.org_id
       AND run_waits.id = due_wait.run_wait_id
       AND run_waits.state IN ('live_waiting', 'checkpointed_waiting')
    RETURNING run_waits.*
)
SELECT resolved_wait.*
  FROM resolved_wait;

-- name: GetTimerWaitForRunWait :one
SELECT *
  FROM timer_waits
 WHERE org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND run_wait_id = sqlc.arg(run_wait_id);
