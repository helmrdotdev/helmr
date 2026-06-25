-- name: CreateTimerWait :one
INSERT INTO timer_waits (
    id,
    org_id,
    project_id,
    environment_id,
    run_wait_id,
    fire_at
)
SELECT sqlc.arg(id),
       run_waits.org_id,
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
       AND timer_waits.fire_at <= now()
       AND run_waits.state = 'waiting'
     ORDER BY timer_waits.fire_at ASC, timer_waits.id ASC
     LIMIT sqlc.arg(limit_count)
     FOR UPDATE OF run_waits
),
resolved_wait AS (
    UPDATE run_waits
       SET state = 'resolved',
           resolved_at = now(),
           updated_at = now()
      FROM due_waits
     WHERE run_waits.org_id = due_waits.org_id
       AND run_waits.id = due_waits.run_wait_id
       AND run_waits.state = 'waiting'
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
       AND run_waits.state = 'waiting'
     FOR UPDATE OF run_waits
),
resolved_wait AS (
    UPDATE run_waits
       SET state = 'resolved',
           resolved_at = now(),
           updated_at = now()
      FROM due_wait
     WHERE run_waits.org_id = due_wait.org_id
       AND run_waits.id = due_wait.run_wait_id
       AND run_waits.state = 'waiting'
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
