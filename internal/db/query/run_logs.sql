-- name: AppendRunLogChunk :one
WITH event_args AS (
    SELECT sqlc.arg(kind)::text AS event_kind,
           sqlc.arg(payload)::jsonb AS event_payload
),
current_run_lease AS (
    SELECT runs.org_id,
           runs.project_id,
           runs.environment_id,
           runs.trace_id,
           runs.state_version,
           runs.id,
           run_leases.id AS run_lease_id,
           run_leases.span_id,
           run_leases.parent_span_id,
           run_leases.traceparent,
           run_leases.task_attempt_number AS attempt_number
      FROM runs
      JOIN run_leases ON run_leases.id = runs.current_run_lease_id
                     AND run_leases.org_id = runs.org_id
                     AND run_leases.run_id = runs.id
     WHERE runs.org_id = sqlc.arg(org_id)
       AND runs.id = sqlc.arg(run_id)
       AND runs.status = 'running'
       AND run_leases.id = sqlc.arg(run_lease_id)
       AND run_leases.worker_instance_id = sqlc.arg(worker_instance_id)
       AND run_leases.state IN ('starting', 'running')
       AND run_leases.expires_at > now()
),
candidate AS (
    SELECT current_run_lease.*,
           sqlc.arg(stream)::text AS stream,
           sqlc.arg(observed_seq)::bigint AS observed_seq,
           sqlc.arg(content)::bytea AS content,
           octet_length(sqlc.arg(content)::bytea)::bigint AS size_bytes,
           event_args.event_kind,
           event_args.event_payload,
           'run_log:' || current_run_lease.run_lease_id::text || ':' || sqlc.arg(stream)::text || ':' || (sqlc.arg(observed_seq)::bigint)::text AS idempotency_key
      FROM current_run_lease
      CROSS JOIN event_args
),
inserted_chunk AS (
    INSERT INTO telemetry_outbox (
        org_id, stream_kind, source_kind, source_id, stream_name,
        idempotency_key, project_id, environment_id, run_id, run_lease_id, attempt_number,
        trace_id, span_id, parent_span_id, traceparent, source, kind, message, payload,
        content, size_bytes, observed_seq, redaction_class, retention_class, observed_at
    )
    SELECT candidate.org_id,
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
           jsonb_build_object(
               'stream', candidate.stream,
               'observed_seq', candidate.observed_seq,
               'bytes', candidate.size_bytes,
               'event_kind', candidate.event_kind,
               'event_payload', candidate.event_payload
           ),
           candidate.content,
           candidate.size_bytes,
           candidate.observed_seq,
           'standard',
           'standard',
           now()
      FROM candidate
    ON CONFLICT (org_id, stream_kind, source_kind, source_id, stream_name, idempotency_key)
    DO UPDATE SET content = telemetry_outbox.content
     WHERE telemetry_outbox.content IS NOT DISTINCT FROM excluded.content
       AND telemetry_outbox.size_bytes = excluded.size_bytes
       AND telemetry_outbox.payload = excluded.payload
    RETURNING telemetry_outbox.org_id,
              telemetry_outbox.run_id,
              telemetry_outbox.run_lease_id,
              telemetry_outbox.attempt_number,
              telemetry_outbox.stream_name AS stream,
              telemetry_outbox.id AS seq,
              telemetry_outbox.observed_seq,
              telemetry_outbox.content,
              telemetry_outbox.size_bytes,
              telemetry_outbox.created_at,
              (xmax = 0) AS is_new,
              true AS replay_matches
),
selected_chunk AS (
    SELECT * FROM inserted_chunk
),
event_input AS (
    SELECT current_run_lease.org_id,
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
                            AND current_run_lease.id = selected_chunk.run_id
      CROSS JOIN event_args
     WHERE selected_chunk.is_new
),
event AS (
    INSERT INTO telemetry_outbox (
        org_id, stream_kind, source_kind, source_id, project_id,
        environment_id, run_id, run_lease_id, attempt_number, trace_id, span_id,
        parent_span_id, traceparent, category, severity, source, kind, message,
        payload, redaction_class, snapshot_version, observed_at
    )
    SELECT event_input.org_id,
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
    INSERT INTO meter_events (
        org_id, project_id, environment_id, run_id, run_lease_id,
        attempt_number, trace_id, span_id, meter, quantity, unit, details,
        idempotency_key, idempotency_fingerprint
    )
    SELECT current_run_lease.org_id,
           current_run_lease.project_id,
           current_run_lease.environment_id,
           selected_chunk.run_id,
           selected_chunk.run_lease_id,
           current_run_lease.attempt_number,
           current_run_lease.trace_id,
           current_run_lease.span_id,
           'log_bytes',
           selected_chunk.size_bytes,
           'bytes',
           jsonb_build_object('stream', selected_chunk.stream, 'observed_seq', selected_chunk.observed_seq),
           'log:' || selected_chunk.run_lease_id::text || ':' || selected_chunk.stream::text || ':' || selected_chunk.observed_seq::text,
           jsonb_build_object(
               'quantity', selected_chunk.size_bytes,
               'unit', 'bytes',
               'details', jsonb_build_object('stream', selected_chunk.stream, 'observed_seq', selected_chunk.observed_seq)
           )::text
      FROM selected_chunk
      JOIN current_run_lease ON current_run_lease.org_id = selected_chunk.org_id
                            AND current_run_lease.id = selected_chunk.run_id
     WHERE selected_chunk.is_new
       AND selected_chunk.size_bytes > 0
    ON CONFLICT (org_id, source_type, source_id, meter, idempotency_key)
    DO UPDATE SET idempotency_fingerprint = meter_events.idempotency_fingerprint
     WHERE meter_events.idempotency_fingerprint = excluded.idempotency_fingerprint
    RETURNING *
),
meter_event_outbox AS (
    INSERT INTO telemetry_outbox (
        org_id, stream_kind, source_kind, source_id, project_id,
        environment_id, run_id, run_lease_id, meter_event_id, attempt_number,
        trace_id, span_id, kind, payload, idempotency_key, observed_at
    )
    SELECT meter_event.org_id,
           'meter_event',
           meter_event.source_type,
           meter_event.source_id,
           meter_event.project_id,
           meter_event.environment_id,
           meter_event.run_id,
           meter_event.run_lease_id,
           meter_event.id,
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
       selected_chunk.created_at,
       selected_chunk.replay_matches
 FROM selected_chunk
 WHERE (SELECT count(*) FROM event) >= 0
   AND (
       NOT selected_chunk.is_new
       OR selected_chunk.size_bytes = 0
       OR EXISTS (SELECT 1 FROM meter_event_outbox)
   );
