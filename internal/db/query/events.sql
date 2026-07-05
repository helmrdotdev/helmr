-- name: AppendRunEventForExecution :one
WITH event_args AS (
    SELECT sqlc.arg(kind)::text AS event_kind,
           sqlc.arg(payload)::jsonb AS event_payload
),
current_run_lease AS (
    SELECT runs.id,
           runs.worker_group_id,
           runs.project_id,
           runs.environment_id,
           runs.trace_id,
           runs.state_version,
           run_leases.id AS run_lease_id,
           run_leases.attempt_id,
           run_leases.span_id,
           run_leases.parent_span_id,
           run_leases.traceparent,
           run_attempts.attempt_number
      FROM runs
      JOIN run_leases ON run_leases.id = runs.current_run_lease_id
                          AND run_leases.org_id = runs.org_id
                          AND run_leases.run_id = runs.id
      JOIN (
    SELECT placement_project.org_id,
           placement_project.id AS project_id,
           target_environment.id AS environment_id,
           placement_worker_group.region_id AS region_id,
           placement_worker_group.id AS worker_group_id,
           placement_worker_group.state AS worker_group_state
      FROM projects AS placement_project
      JOIN environments AS target_environment
        ON target_environment.org_id = placement_project.org_id
       AND target_environment.project_id = placement_project.id
      JOIN worker_groups AS placement_worker_group
        ON true
) AS project_worker_group_placement
        ON project_worker_group_placement.org_id = runs.org_id
       AND project_worker_group_placement.project_id = runs.project_id
       AND project_worker_group_placement.environment_id = runs.environment_id
       AND project_worker_group_placement.worker_group_id = runs.worker_group_id
       AND project_worker_group_placement.worker_group_state IN ('active', 'draining')
      JOIN worker_groups ON worker_groups.id = project_worker_group_placement.worker_group_id
                AND worker_groups.state IN ('active', 'draining')
      JOIN run_attempts ON run_attempts.org_id = run_leases.org_id
                       AND run_attempts.run_id = run_leases.run_id
                       AND run_attempts.id = run_leases.attempt_id
	     WHERE runs.org_id = sqlc.arg(org_id)
	       AND runs.worker_group_id = sqlc.arg(worker_group_id)
	       AND runs.id = sqlc.arg(run_id)
	       AND runs.status = 'running'
	       AND run_leases.worker_group_id = sqlc.arg(worker_group_id)
	       AND run_leases.id = sqlc.arg(run_lease_id)
       AND run_leases.worker_instance_id = sqlc.arg(worker_instance_id)
       AND run_leases.status IN ('leased', 'running')
       AND run_leases.lease_expires_at > now()
),
event_input AS (
    SELECT sqlc.arg(org_id) AS org_id,
           current_run_lease.worker_group_id,
           current_run_lease.project_id,
           current_run_lease.environment_id,
           current_run_lease.id AS run_id,
           current_run_lease.attempt_id,
           current_run_lease.run_lease_id,
           current_run_lease.attempt_number,
           current_run_lease.trace_id,
           current_run_lease.span_id,
           current_run_lease.parent_span_id,
           current_run_lease.traceparent,
           CASE WHEN event_args.event_kind = 'log' THEN 'log' ELSE 'guest' END AS category,
           'info' AS severity,
           'worker' AS source,
           event_args.event_kind AS kind,
           event_args.event_kind AS message,
           event_args.event_payload AS payload,
           'sensitive' AS redaction_class,
           current_run_lease.state_version AS snapshot_version
  FROM current_run_lease
  CROSS JOIN event_args
),
event_seq AS (
    INSERT INTO event_cursors (org_id, worker_group_id, subject_kind, subject_id, seq)
    SELECT event_input.org_id,
           event_input.worker_group_id,
           'run',
           event_input.run_id,
           1
      FROM event_input
    ON CONFLICT (org_id, worker_group_id, subject_kind, subject_id)
    DO UPDATE SET seq = event_cursors.seq + 1,
                  observed_at = now()
    RETURNING event_cursors.org_id, event_cursors.worker_group_id, event_cursors.subject_kind, event_cursors.subject_id, event_cursors.seq
),
inserted_event AS (
    INSERT INTO event_hot_payloads (org_id, worker_group_id, project_id, environment_id, run_id, seq, attempt_id, run_lease_id, attempt_number, trace_id, span_id, parent_span_id, traceparent, category, severity, source, kind, message, payload, redaction_class, snapshot_version)
    SELECT event_input.org_id,
           event_input.worker_group_id,
           event_input.project_id,
           event_input.environment_id,
           event_input.run_id,
           event_seq.seq,
           event_input.attempt_id,
           event_input.run_lease_id,
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
                    AND event_seq.worker_group_id = event_input.worker_group_id
                    AND event_seq.subject_kind = 'run'
                    AND event_seq.subject_id = event_input.run_id
    RETURNING *
),
inserted_outbox AS (
    INSERT INTO telemetry_outbox (org_id, worker_group_id, stream_kind, source_kind, source_id, seq, idempotency_key)
    SELECT inserted_event.org_id,
           inserted_event.worker_group_id,
           'event',
           inserted_event.subject_type,
           inserted_event.subject_id,
           inserted_event.seq,
           'event:' || inserted_event.subject_type::text || ':' || inserted_event.subject_id::text || ':' || inserted_event.seq::text
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
           runs.worker_group_id,
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
           target_run.worker_group_id,
           target_run.project_id,
           target_run.environment_id,
           target_run.id AS run_id,
           target_run.current_attempt_id AS attempt_id,
           NULL::uuid AS run_lease_id,
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
    INSERT INTO event_cursors (org_id, worker_group_id, subject_kind, subject_id, seq)
    SELECT event_input.org_id,
           event_input.worker_group_id,
           'run',
           event_input.run_id,
           1
      FROM event_input
    ON CONFLICT (org_id, worker_group_id, subject_kind, subject_id)
    DO UPDATE SET seq = event_cursors.seq + 1,
                  observed_at = now()
    RETURNING event_cursors.org_id, event_cursors.worker_group_id, event_cursors.subject_kind, event_cursors.subject_id, event_cursors.seq
),
inserted_event AS (
    INSERT INTO event_hot_payloads (org_id, worker_group_id, project_id, environment_id, run_id, seq, attempt_id, run_lease_id, attempt_number, trace_id, span_id, parent_span_id, traceparent, category, severity, source, kind, message, payload, redaction_class, snapshot_version)
    SELECT event_input.org_id,
           event_input.worker_group_id,
           event_input.project_id,
           event_input.environment_id,
           event_input.run_id,
           event_seq.seq,
           event_input.attempt_id,
           event_input.run_lease_id,
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
                    AND event_seq.worker_group_id = event_input.worker_group_id
                    AND event_seq.subject_kind = 'run'
                    AND event_seq.subject_id = event_input.run_id
    RETURNING *
),
inserted_outbox AS (
    INSERT INTO telemetry_outbox (org_id, worker_group_id, stream_kind, source_kind, source_id, seq, idempotency_key)
    SELECT inserted_event.org_id,
           inserted_event.worker_group_id,
           'event',
           inserted_event.subject_type,
           inserted_event.subject_id,
           inserted_event.seq,
           'event:' || inserted_event.subject_type::text || ':' || inserted_event.subject_id::text || ':' || inserted_event.seq::text
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
           deployments.build_worker_group_id AS worker_group_id,
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
           target_deployment.worker_group_id,
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
    INSERT INTO event_cursors (org_id, worker_group_id, subject_kind, subject_id, seq)
    SELECT event_input.org_id,
           event_input.worker_group_id,
           'deployment',
           event_input.deployment_id,
           1
      FROM event_input
    ON CONFLICT (org_id, worker_group_id, subject_kind, subject_id)
    DO UPDATE SET seq = event_cursors.seq + 1,
                  observed_at = now()
    RETURNING event_cursors.org_id, event_cursors.worker_group_id, event_cursors.subject_kind, event_cursors.subject_id, event_cursors.seq
),
inserted_event AS (
    INSERT INTO event_hot_payloads (org_id, worker_group_id, project_id, environment_id, deployment_id, seq, category, severity, source, kind, message, payload, redaction_class)
    SELECT event_input.org_id,
           event_input.worker_group_id,
           event_input.project_id,
           event_input.environment_id,
           event_input.deployment_id,
           event_seq.seq,
           event_input.category,
           event_input.severity,
           event_input.source,
           event_input.kind,
           event_input.message,
           event_input.payload,
           event_input.redaction_class
      FROM event_input
      JOIN event_seq ON event_seq.org_id = event_input.org_id
                    AND event_seq.worker_group_id = event_input.worker_group_id
                    AND event_seq.subject_kind = 'deployment'
                    AND event_seq.subject_id = event_input.deployment_id
    RETURNING *
),
inserted_outbox AS (
    INSERT INTO telemetry_outbox (org_id, worker_group_id, stream_kind, source_kind, source_id, seq, idempotency_key)
    SELECT inserted_event.org_id,
           inserted_event.worker_group_id,
           'event',
           inserted_event.subject_type,
           inserted_event.subject_id,
           inserted_event.seq,
           'event:' || inserted_event.subject_type::text || ':' || inserted_event.subject_id::text || ':' || inserted_event.seq::text
      FROM inserted_event
    RETURNING id
)
SELECT inserted_event.*
FROM inserted_event
JOIN inserted_outbox ON true;

-- name: ListSubjectEvents :many
SELECT *
  FROM event_hot_payloads AS events
 WHERE org_id = sqlc.arg(org_id)
   AND subject_type = sqlc.arg(subject_type)::event_subject_type
   AND subject_id = sqlc.arg(subject_id)
   AND seq > sqlc.arg(seq)
 ORDER BY seq ASC
 LIMIT sqlc.arg(row_limit);

-- name: ClaimEventOutbox :many
WITH claimed AS (
    SELECT telemetry_outbox.id
      FROM telemetry_outbox
     WHERE telemetry_outbox.stream_kind = 'event'
       AND telemetry_outbox.published_at IS NULL
       AND (telemetry_outbox.publish_locked_until IS NULL OR telemetry_outbox.publish_locked_until < now())
       AND NOT EXISTS (
            SELECT 1
              FROM telemetry_outbox AS earlier_outbox
             WHERE earlier_outbox.stream_kind = 'event'
               AND earlier_outbox.published_at IS NULL
               AND earlier_outbox.state <> 'dead_lettered'
               AND earlier_outbox.org_id = telemetry_outbox.org_id
               AND earlier_outbox.worker_group_id = telemetry_outbox.worker_group_id
               AND earlier_outbox.source_kind = telemetry_outbox.source_kind
               AND earlier_outbox.source_id = telemetry_outbox.source_id
               AND earlier_outbox.seq < telemetry_outbox.seq
       )
       AND EXISTS (
             SELECT 1
               FROM event_hot_payloads AS events
              WHERE events.org_id = telemetry_outbox.org_id
                AND events.worker_group_id = telemetry_outbox.worker_group_id
                AND events.subject_type = telemetry_outbox.source_kind::event_subject_type
                AND events.subject_id = telemetry_outbox.source_id
                AND events.seq = telemetry_outbox.seq
       )
     ORDER BY telemetry_outbox.id ASC
     LIMIT sqlc.arg(row_limit)
     FOR UPDATE SKIP LOCKED
),
updated AS (
    UPDATE telemetry_outbox
       SET publish_locked_until = now() + sqlc.arg(lease_duration)::interval,
           publish_attempts = telemetry_outbox.publish_attempts + 1,
           updated_at = now(),
           last_error = ''
      FROM claimed
     WHERE telemetry_outbox.id = claimed.id
    RETURNING telemetry_outbox.*
)
SELECT updated.id AS outbox_id,
       ('helmr:events:' || updated.org_id::text || ':' || updated.worker_group_id || ':' || updated.source_kind || ':' || updated.source_id::text)::text AS stream_key,
       updated.publish_attempts AS attempts,
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
       events.run_lease_id,
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
  JOIN event_hot_payloads AS events ON events.org_id = updated.org_id
                                   AND events.worker_group_id = updated.worker_group_id
                                   AND events.subject_type = updated.source_kind::event_subject_type
                                   AND events.subject_id = updated.source_id
                                   AND events.seq = updated.seq
 ORDER BY updated.id ASC;

-- name: MarkEventOutboxPublished :exec
UPDATE telemetry_outbox
   SET published_at = now(),
       publish_locked_until = NULL,
       updated_at = now(),
       last_error = ''
 WHERE id = sqlc.arg(id);

-- name: MarkEventOutboxFailed :exec
UPDATE telemetry_outbox
   SET publish_locked_until = now() + sqlc.arg(retry_after)::interval,
       updated_at = now(),
       last_error = sqlc.arg(last_error)
 WHERE id = sqlc.arg(id)
   AND published_at IS NULL;
