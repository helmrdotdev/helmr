-- name: AppendRunLogChunk :one
WITH event_args AS (
    SELECT sqlc.arg(kind)::text AS event_kind,
           sqlc.arg(payload)::jsonb AS event_payload
),
current_run_lease AS (
    SELECT runs.org_id,
           runs.worker_group_id,
           runs.project_id,
           runs.environment_id,
           runs.trace_id,
           runs.state_version,
           runs.id,
           run_leases.id AS run_lease_id,
           run_leases.span_id,
           run_leases.parent_span_id,
           run_leases.traceparent,
           run_leases.attempt_number
      FROM runs
      JOIN run_leases ON run_leases.id = runs.current_run_lease_id
                     AND run_leases.org_id = runs.org_id
                     AND run_leases.run_id = runs.id
      JOIN worker_groups
        ON worker_groups.id = runs.worker_group_id
       AND worker_groups.state IN ('active', 'draining')
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
candidate AS (
    SELECT current_run_lease.*,
           sqlc.arg(stream)::run_log_stream AS stream,
           sqlc.arg(observed_seq)::bigint AS observed_seq,
           sqlc.arg(content)::bytea AS content,
           octet_length(sqlc.arg(content)::bytea)::bigint AS size_bytes,
           'run_log:' || current_run_lease.run_lease_id::text || ':' || (sqlc.arg(stream)::run_log_stream)::text || ':' || (sqlc.arg(observed_seq)::bigint)::text AS idempotency_key
      FROM current_run_lease
),
inserted_chunk AS (
    INSERT INTO telemetry_outbox (
        org_id, worker_group_id, stream_kind, source_kind, source_id, stream_name,
        idempotency_key, project_id, environment_id, run_id, run_lease_id, attempt_number,
        trace_id, span_id, parent_span_id, traceparent, source, kind, message, payload,
        content, size_bytes, observed_seq, redaction_class, retention_class, observed_at
    )
    SELECT candidate.org_id,
           candidate.worker_group_id,
           'run_log',
           'run',
           candidate.id,
           candidate.stream::text,
           candidate.idempotency_key,
           candidate.project_id,
           candidate.environment_id,
           candidate.id,
           candidate.run_lease_id,
           candidate.attempt_number,
           candidate.trace_id,
           candidate.span_id,
           candidate.parent_span_id,
           candidate.traceparent,
           'worker',
           'run.log',
           'run.log',
           jsonb_build_object('stream', candidate.stream, 'observed_seq', candidate.observed_seq, 'bytes', candidate.size_bytes),
           candidate.content,
           candidate.size_bytes,
           candidate.observed_seq,
           'standard',
           'standard',
           now()
      FROM candidate
    ON CONFLICT (worker_group_id, stream_kind, idempotency_key) DO NOTHING
    RETURNING telemetry_outbox.org_id,
              telemetry_outbox.worker_group_id,
              telemetry_outbox.run_id,
              telemetry_outbox.run_lease_id,
              telemetry_outbox.attempt_number,
              telemetry_outbox.stream_name::run_log_stream AS stream,
              telemetry_outbox.id AS seq,
              telemetry_outbox.observed_seq,
              telemetry_outbox.content,
              telemetry_outbox.size_bytes,
              telemetry_outbox.created_at,
              true AS is_new
),
existing_chunk AS (
    SELECT telemetry_outbox.org_id,
           telemetry_outbox.worker_group_id,
           telemetry_outbox.run_id,
           telemetry_outbox.run_lease_id,
           telemetry_outbox.attempt_number,
           telemetry_outbox.stream_name::run_log_stream AS stream,
           telemetry_outbox.id AS seq,
           telemetry_outbox.observed_seq,
           telemetry_outbox.content,
           telemetry_outbox.size_bytes,
           telemetry_outbox.created_at,
           false AS is_new
      FROM telemetry_outbox
      JOIN candidate ON candidate.worker_group_id = telemetry_outbox.worker_group_id
                    AND telemetry_outbox.stream_kind = 'run_log'
                    AND telemetry_outbox.idempotency_key = candidate.idempotency_key
     WHERE NOT EXISTS (SELECT 1 FROM inserted_chunk)
),
selected_chunk AS (
    SELECT * FROM inserted_chunk
    UNION ALL
    SELECT * FROM existing_chunk
),
event_input AS (
    SELECT current_run_lease.org_id,
           current_run_lease.worker_group_id,
           current_run_lease.project_id,
           current_run_lease.environment_id,
           selected_chunk.run_id,
           selected_chunk.run_lease_id,
           selected_chunk.attempt_number,
           current_run_lease.trace_id,
           current_run_lease.span_id,
           current_run_lease.parent_span_id,
           current_run_lease.traceparent,
           'log' AS category,
           'info' AS severity,
           'worker' AS source,
           event_args.event_kind AS kind,
           event_args.event_kind AS message,
           event_args.event_payload AS payload,
           'sensitive' AS redaction_class,
           current_run_lease.state_version AS snapshot_version
      FROM selected_chunk
      JOIN current_run_lease ON current_run_lease.org_id = selected_chunk.org_id
                            AND current_run_lease.worker_group_id = selected_chunk.worker_group_id
                            AND current_run_lease.id = selected_chunk.run_id
      CROSS JOIN event_args
     WHERE selected_chunk.is_new
),
event AS (
    INSERT INTO telemetry_outbox (
        org_id, worker_group_id, stream_kind, source_kind, source_id, project_id,
        environment_id, run_id, run_lease_id, attempt_number, trace_id, span_id,
        parent_span_id, traceparent, category, severity, source, kind, message,
        payload, redaction_class, snapshot_version, observed_at
    )
    SELECT event_input.org_id,
           event_input.worker_group_id,
           'event',
           'run',
           event_input.run_id,
           event_input.project_id,
           event_input.environment_id,
           event_input.run_id,
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
           event_input.snapshot_version,
           now()
      FROM event_input
    RETURNING id
),
meter_event AS (
    INSERT INTO meter_events (org_id, worker_group_id, project_id, environment_id, source_type, source_id, run_id, attempt_number, trace_id, span_id, meter, quantity, unit, details, idempotency_key)
    SELECT current_run_lease.org_id,
           current_run_lease.worker_group_id,
           current_run_lease.project_id,
           current_run_lease.environment_id,
           'run_log',
           selected_chunk.run_lease_id,
           selected_chunk.run_id,
           current_run_lease.attempt_number,
           current_run_lease.trace_id,
           current_run_lease.span_id,
           'log_bytes',
           selected_chunk.size_bytes,
           'bytes',
           jsonb_build_object('stream', selected_chunk.stream, 'observed_seq', selected_chunk.observed_seq),
           'log:' || selected_chunk.run_lease_id::text || ':' || selected_chunk.stream::text || ':' || selected_chunk.observed_seq::text
      FROM selected_chunk
      JOIN current_run_lease ON current_run_lease.org_id = selected_chunk.org_id
                            AND current_run_lease.worker_group_id = selected_chunk.worker_group_id
                            AND current_run_lease.id = selected_chunk.run_id
     WHERE selected_chunk.is_new
       AND selected_chunk.size_bytes > 0
    ON CONFLICT DO NOTHING
    RETURNING *
),
meter_event_outbox AS (
    INSERT INTO telemetry_outbox (
        org_id, worker_group_id, stream_kind, source_kind, source_id, project_id,
        environment_id, run_id, attempt_number, trace_id, span_id, kind, payload,
        idempotency_key, observed_at
    )
    SELECT meter_event.org_id,
           meter_event.worker_group_id,
           'meter_event',
           meter_event.source_type,
           meter_event.source_id,
           meter_event.project_id,
           meter_event.environment_id,
           meter_event.run_id,
           meter_event.attempt_number,
           meter_event.trace_id,
           meter_event.span_id,
           meter_event.meter,
           meter_event.details,
           meter_event.idempotency_key,
           meter_event.occurred_at
      FROM meter_event
    ON CONFLICT DO NOTHING
    RETURNING id
)
SELECT selected_chunk.org_id,
       selected_chunk.run_id,
       selected_chunk.run_lease_id,
       selected_chunk.attempt_number,
       selected_chunk.stream,
       selected_chunk.seq,
       selected_chunk.observed_seq,
       selected_chunk.content,
       selected_chunk.size_bytes,
       selected_chunk.created_at
 FROM selected_chunk
 WHERE (SELECT count(*) FROM event) >= 0
   AND (SELECT count(*) FROM meter_event_outbox) >= 0;
