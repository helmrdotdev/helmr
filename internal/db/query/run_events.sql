-- name: AppendRunEventForExecution :one
WITH current_session AS (
    SELECT runs.id,
           runs.project_id,
           runs.environment_id,
           runs.trace_id,
           runs.state_version,
           run_execution_sessions.id AS session_id,
           run_execution_sessions.attempt_id,
           run_execution_sessions.span_id,
           run_execution_sessions.parent_span_id,
           run_execution_sessions.traceparent,
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
INSERT INTO run_events (org_id, project_id, environment_id, run_id, attempt_id, session_id, attempt_number, trace_id, span_id, parent_span_id, traceparent, category, source, kind, message, payload, redaction_class, snapshot_version)
SELECT sqlc.arg(org_id),
       project_id,
       environment_id,
       id,
       attempt_id,
       session_id,
       attempt_number,
       trace_id,
       span_id,
       parent_span_id,
       traceparent,
       CASE WHEN sqlc.arg(kind)::text = 'log' THEN 'log' ELSE 'guest' END,
       'worker',
       sqlc.arg(kind),
       sqlc.arg(kind),
       sqlc.arg(payload),
       CASE WHEN sqlc.arg(kind)::text LIKE 'emit.%' THEN 'internal' ELSE 'sensitive' END,
       state_version
  FROM current_session
RETURNING *;

-- name: AppendRunEvent :one
WITH target_run AS (
    SELECT runs.id,
           runs.project_id,
           runs.environment_id,
           runs.current_attempt_id,
           runs.current_attempt_number,
           runs.trace_id,
           runs.root_span_id,
           runs.state_version
      FROM runs
     WHERE runs.org_id = sqlc.arg(org_id)
       AND runs.id = sqlc.arg(run_id)
)
INSERT INTO run_events (org_id, project_id, environment_id, run_id, attempt_id, attempt_number, trace_id, span_id, traceparent, category, source, kind, message, payload, redaction_class, snapshot_version)
SELECT sqlc.arg(org_id),
       project_id,
       environment_id,
       id,
       current_attempt_id,
       current_attempt_number,
       trace_id,
       root_span_id,
       '00-' || trace_id || '-' || root_span_id || '-01',
       'system',
       'control',
       sqlc.arg(kind),
       sqlc.arg(kind),
       sqlc.arg(payload),
       'internal',
       state_version
  FROM target_run
RETURNING *;

-- name: ListRunEvents :many
SELECT * FROM run_events
WHERE org_id = $1 AND run_id = $2 AND id > $3
ORDER BY id ASC
LIMIT $4;
