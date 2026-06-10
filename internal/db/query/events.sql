-- name: AppendRunEventForExecution :one
WITH event_args AS (
    SELECT sqlc.arg(kind)::text AS event_kind,
           sqlc.arg(payload)::jsonb AS event_payload
),
current_session AS (
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
),
event_input AS (
    SELECT sqlc.arg(org_id) AS org_id,
           current_session.project_id,
           current_session.environment_id,
           current_session.id AS run_id,
           current_session.attempt_id,
           current_session.session_id,
           current_session.attempt_number,
           current_session.trace_id,
           current_session.span_id,
           current_session.parent_span_id,
           current_session.traceparent,
           CASE WHEN event_args.event_kind = 'log' THEN 'log' ELSE 'guest' END AS category,
           'info' AS severity,
           'worker' AS source,
           event_args.event_kind AS kind,
           event_args.event_kind AS message,
           event_args.event_payload AS payload,
           CASE WHEN event_args.event_kind LIKE 'emit.%' THEN 'internal' ELSE 'sensitive' END AS redaction_class,
           current_session.state_version AS snapshot_version
  FROM current_session
  CROSS JOIN event_args
),
event_seq AS (
    INSERT INTO event_subject_cursors (org_id, subject_type, subject_id, last_seq)
    SELECT event_input.org_id,
           'run',
           event_input.run_id,
           1
      FROM event_input
    ON CONFLICT (org_id, subject_type, subject_id)
    DO UPDATE SET last_seq = event_subject_cursors.last_seq + 1,
                  updated_at = now()
    RETURNING org_id, subject_type, subject_id, last_seq
),
inserted_event AS (
    INSERT INTO events (org_id, project_id, environment_id, run_id, seq, attempt_id, session_id, attempt_number, trace_id, span_id, parent_span_id, traceparent, category, severity, source, kind, message, payload, redaction_class, snapshot_version)
    SELECT event_input.org_id,
           event_input.project_id,
           event_input.environment_id,
           event_input.run_id,
           event_seq.last_seq,
           event_input.attempt_id,
           event_input.session_id,
           event_input.attempt_number,
           event_input.trace_id,
           event_input.span_id,
           event_input.parent_span_id,
           event_input.traceparent,
           event_input.category,
           event_input.severity,
           event_input.source,
           event_input.kind,
           event_input.message,
           event_input.payload,
           event_input.redaction_class,
           event_input.snapshot_version
      FROM event_input
      JOIN event_seq ON event_seq.org_id = event_input.org_id
                    AND event_seq.subject_type = 'run'
                    AND event_seq.subject_id = event_input.run_id
    RETURNING *
),
inserted_outbox AS (
    INSERT INTO event_outbox (event_record_id, stream_key)
    SELECT inserted_event.id,
           'helmr:events:' || inserted_event.org_id::text || ':' || inserted_event.subject_type::text || ':' || inserted_event.subject_id::text
      FROM inserted_event
    RETURNING id
)
SELECT inserted_event.*
FROM inserted_event
JOIN inserted_outbox ON true;

-- name: AppendRunEvent :one
WITH event_args AS (
    SELECT sqlc.arg(kind)::text AS event_kind,
           sqlc.arg(payload)::jsonb AS event_payload
),
target_run AS (
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
),
event_input AS (
    SELECT sqlc.arg(org_id) AS org_id,
           target_run.project_id,
           target_run.environment_id,
           target_run.id AS run_id,
           target_run.current_attempt_id AS attempt_id,
           NULL::uuid AS session_id,
           target_run.current_attempt_number AS attempt_number,
           target_run.trace_id,
           target_run.root_span_id AS span_id,
           NULL::text AS parent_span_id,
           '00-' || target_run.trace_id || '-' || target_run.root_span_id || '-01' AS traceparent,
           'system' AS category,
           'info' AS severity,
           'control' AS source,
           event_args.event_kind AS kind,
           event_args.event_kind AS message,
           event_args.event_payload AS payload,
           'internal' AS redaction_class,
           target_run.state_version AS snapshot_version
  FROM target_run
  CROSS JOIN event_args
),
event_seq AS (
    INSERT INTO event_subject_cursors (org_id, subject_type, subject_id, last_seq)
    SELECT event_input.org_id,
           'run',
           event_input.run_id,
           1
      FROM event_input
    ON CONFLICT (org_id, subject_type, subject_id)
    DO UPDATE SET last_seq = event_subject_cursors.last_seq + 1,
                  updated_at = now()
    RETURNING org_id, subject_type, subject_id, last_seq
),
inserted_event AS (
    INSERT INTO events (org_id, project_id, environment_id, run_id, seq, attempt_id, session_id, attempt_number, trace_id, span_id, parent_span_id, traceparent, category, severity, source, kind, message, payload, redaction_class, snapshot_version)
    SELECT event_input.org_id,
           event_input.project_id,
           event_input.environment_id,
           event_input.run_id,
           event_seq.last_seq,
           event_input.attempt_id,
           event_input.session_id,
           event_input.attempt_number,
           event_input.trace_id,
           event_input.span_id,
           event_input.parent_span_id,
           event_input.traceparent,
           event_input.category,
           event_input.severity,
           event_input.source,
           event_input.kind,
           event_input.message,
           event_input.payload,
           event_input.redaction_class,
           event_input.snapshot_version
      FROM event_input
      JOIN event_seq ON event_seq.org_id = event_input.org_id
                    AND event_seq.subject_type = 'run'
                    AND event_seq.subject_id = event_input.run_id
    RETURNING *
),
inserted_outbox AS (
    INSERT INTO event_outbox (event_record_id, stream_key)
    SELECT inserted_event.id,
           'helmr:events:' || inserted_event.org_id::text || ':' || inserted_event.subject_type::text || ':' || inserted_event.subject_id::text
      FROM inserted_event
    RETURNING id
)
SELECT inserted_event.*
FROM inserted_event
JOIN inserted_outbox ON true;

-- name: AppendDeploymentEvent :one
WITH target_deployment AS (
    SELECT deployments.id,
           deployments.org_id,
           deployments.project_id,
           deployments.environment_id
      FROM deployments
     WHERE deployments.org_id = sqlc.arg(org_id)
       AND deployments.project_id = sqlc.arg(project_id)
       AND deployments.environment_id = sqlc.arg(environment_id)
       AND deployments.id = sqlc.arg(deployment_id)
),
event_input AS (
    SELECT target_deployment.org_id,
           target_deployment.project_id,
           target_deployment.environment_id,
           target_deployment.id AS deployment_id,
           sqlc.arg(category)::text AS category,
           sqlc.arg(severity)::text AS severity,
           sqlc.arg(source)::text AS source,
           sqlc.arg(kind)::text AS kind,
           sqlc.arg(message)::text AS message,
           sqlc.arg(payload)::jsonb AS payload,
           sqlc.arg(redaction_class)::text AS redaction_class
  FROM target_deployment
),
event_seq AS (
    INSERT INTO event_subject_cursors (org_id, subject_type, subject_id, last_seq)
    SELECT event_input.org_id,
           'deployment',
           event_input.deployment_id,
           1
      FROM event_input
    ON CONFLICT (org_id, subject_type, subject_id)
    DO UPDATE SET last_seq = event_subject_cursors.last_seq + 1,
                  updated_at = now()
    RETURNING org_id, subject_type, subject_id, last_seq
),
inserted_event AS (
    INSERT INTO events (org_id, project_id, environment_id, deployment_id, seq, category, severity, source, kind, message, payload, redaction_class)
    SELECT event_input.org_id,
           event_input.project_id,
           event_input.environment_id,
           event_input.deployment_id,
           event_seq.last_seq,
           event_input.category,
           event_input.severity,
           event_input.source,
           event_input.kind,
           event_input.message,
           event_input.payload,
           event_input.redaction_class
      FROM event_input
      JOIN event_seq ON event_seq.org_id = event_input.org_id
                    AND event_seq.subject_type = 'deployment'
                    AND event_seq.subject_id = event_input.deployment_id
    RETURNING *
),
inserted_outbox AS (
    INSERT INTO event_outbox (event_record_id, stream_key)
    SELECT inserted_event.id,
           'helmr:events:' || inserted_event.org_id::text || ':' || inserted_event.subject_type::text || ':' || inserted_event.subject_id::text
      FROM inserted_event
    RETURNING id
)
SELECT inserted_event.*
FROM inserted_event
JOIN inserted_outbox ON true;

-- name: ListSubjectEvents :many
SELECT *
  FROM events
 WHERE org_id = sqlc.arg(org_id)
   AND subject_type = sqlc.arg(subject_type)::event_subject_type
   AND subject_id = sqlc.arg(subject_id)
   AND seq > sqlc.arg(seq)
 ORDER BY seq ASC
 LIMIT sqlc.arg(row_limit);

-- name: ClaimEventOutbox :many
WITH claimed AS (
    SELECT event_outbox.id
      FROM event_outbox
      JOIN events ON events.id = event_outbox.event_record_id
     WHERE event_outbox.published_at IS NULL
       AND (event_outbox.locked_until IS NULL OR event_outbox.locked_until < now())
       AND NOT EXISTS (
            SELECT 1
              FROM event_outbox AS earlier_outbox
              JOIN events AS earlier_event
                ON earlier_event.id = earlier_outbox.event_record_id
             WHERE earlier_outbox.published_at IS NULL
               AND earlier_event.subject_type = events.subject_type
               AND earlier_event.subject_id = events.subject_id
               AND earlier_event.seq < events.seq
       )
     ORDER BY event_outbox.id ASC
     LIMIT sqlc.arg(row_limit)
     FOR UPDATE SKIP LOCKED
),
updated AS (
    UPDATE event_outbox
       SET locked_until = now() + sqlc.arg(lease_duration)::interval,
           attempts = event_outbox.attempts + 1,
           last_error = ''
      FROM claimed
     WHERE event_outbox.id = claimed.id
    RETURNING event_outbox.*
)
SELECT updated.id AS outbox_id,
       updated.stream_key,
       updated.attempts,
       events.id AS event_record_id,
       events.subject_type,
       events.subject_id,
       events.seq,
       events.org_id,
       events.project_id,
       events.environment_id,
       events.run_id,
       events.deployment_id,
       events.attempt_id,
       events.session_id,
       events.attempt_number,
       events.trace_id,
       events.span_id,
       events.parent_span_id,
       events.traceparent,
       events.category,
       events.severity,
       events.source,
       events.kind,
       events.message,
       events.payload,
       events.redaction_class,
       events.snapshot_version,
       events.occurred_at,
       events.created_at
  FROM updated
  JOIN events ON events.id = updated.event_record_id
 ORDER BY updated.id ASC;

-- name: MarkEventOutboxPublished :exec
UPDATE event_outbox
   SET published_at = now(),
       locked_until = NULL,
       last_error = ''
 WHERE id = sqlc.arg(id);

-- name: MarkEventOutboxFailed :exec
UPDATE event_outbox
   SET locked_until = now() + sqlc.arg(retry_after)::interval,
       last_error = sqlc.arg(last_error)
 WHERE id = sqlc.arg(id)
   AND published_at IS NULL;
