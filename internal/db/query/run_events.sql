-- name: AppendRunEventForExecution :one
WITH current_session AS (
    SELECT runs.id,
           run_execution_sessions.id AS session_id,
           run_attempts.attempt_number
      FROM runs
      JOIN run_execution_sessions ON run_execution_sessions.id = runs.current_session_id
                          AND run_execution_sessions.org_id = runs.org_id
                          AND run_execution_sessions.run_id = runs.id
      JOIN run_attempts ON run_attempts.org_id = run_execution_sessions.org_id
                       AND run_attempts.run_id = run_execution_sessions.run_id
                       AND run_attempts.id = run_execution_sessions.attempt_id
     WHERE runs.org_id = sqlc.arg(org_id)
       AND runs.id = sqlc.arg(run_id)
       AND runs.status = 'running'
       AND run_execution_sessions.id = sqlc.arg(session_id)
       AND run_execution_sessions.worker_instance_id = sqlc.arg(worker_instance_id)
       AND run_execution_sessions.status IN ('leased', 'running')
       AND run_execution_sessions.lease_expires_at > now()
)
INSERT INTO run_events (org_id, run_id, session_id, attempt_number, kind, payload)
SELECT sqlc.arg(org_id), id, session_id, attempt_number, sqlc.arg(kind), sqlc.arg(payload)
  FROM current_session
RETURNING *;

-- name: AppendRunEvent :one
INSERT INTO run_events (org_id, run_id, kind, payload)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: ListRunEvents :many
SELECT * FROM run_events
WHERE org_id = $1 AND run_id = $2 AND id > $3
ORDER BY id ASC
LIMIT $4;
