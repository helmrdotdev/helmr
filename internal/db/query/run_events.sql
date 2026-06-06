-- name: AppendRunEventForExecution :one
WITH current_execution AS (
    SELECT runs.id,
           run_executions.id AS execution_id,
           run_executions.attempt_number
      FROM runs
      JOIN run_executions ON run_executions.id = runs.current_execution_id
                          AND run_executions.org_id = runs.org_id
                          AND run_executions.run_id = runs.id
     WHERE runs.org_id = sqlc.arg(org_id)
       AND runs.id = sqlc.arg(run_id)
       AND runs.status = 'running'
       AND run_executions.id = sqlc.arg(execution_id)
       AND run_executions.worker_instance_id = sqlc.arg(worker_instance_id)
       AND run_executions.status IN ('leased', 'running')
       AND run_executions.lease_expires_at > now()
)
INSERT INTO run_events (org_id, run_id, execution_id, attempt_number, kind, payload)
SELECT sqlc.arg(org_id), id, execution_id, attempt_number, sqlc.arg(kind), sqlc.arg(payload)
  FROM current_execution
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
