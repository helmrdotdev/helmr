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
           run_leases.attempt_id,
           run_leases.span_id,
           run_leases.parent_span_id,
           run_leases.traceparent,
           run_attempts.attempt_number
      FROM runs
      JOIN run_leases ON run_leases.id = runs.current_run_lease_id
                          AND run_leases.org_id = runs.org_id
                          AND run_leases.run_id = runs.id
      JOIN run_attempts ON run_attempts.org_id = run_leases.org_id
                       AND run_attempts.run_id = run_leases.run_id
                       AND run_attempts.id = run_leases.attempt_id
     WHERE runs.org_id = sqlc.arg(org_id)
       AND runs.id = sqlc.arg(run_id)
       AND runs.status = 'running'
       AND run_leases.id = sqlc.arg(run_lease_id)
       AND run_leases.worker_instance_id = sqlc.arg(worker_instance_id)
       AND run_leases.status IN ('leased', 'running')
       AND run_leases.lease_expires_at > now()
     FOR UPDATE OF runs
),
next_seq AS (
    SELECT COALESCE(MAX(run_log_chunks.seq), 0) + 1 AS seq
      FROM run_log_chunks
      JOIN current_run_lease ON current_run_lease.org_id = run_log_chunks.org_id
                            AND current_run_lease.id = run_log_chunks.run_id
),
inserted AS (
    INSERT INTO run_log_chunks (org_id, run_id, run_lease_id, attempt_number, stream, seq, observed_seq, content, size_bytes, created_at)
    SELECT org_id,
           id,
           run_lease_id,
           attempt_number,
           sqlc.arg(stream)::run_log_stream,
           next_seq.seq,
           sqlc.arg(observed_seq),
           sqlc.arg(content)::bytea,
           octet_length(sqlc.arg(content)::bytea)::bigint,
           now()
      FROM current_run_lease
      JOIN next_seq ON true
    ON CONFLICT (org_id, run_id, run_lease_id, stream, observed_seq) DO NOTHING
    RETURNING org_id, run_id, run_lease_id, attempt_number, stream, seq, observed_seq, content, size_bytes, created_at
),
existing AS (
    SELECT run_log_chunks.org_id,
           run_log_chunks.run_id,
           run_log_chunks.run_lease_id,
           run_log_chunks.attempt_number,
           run_log_chunks.stream,
           run_log_chunks.seq,
           run_log_chunks.observed_seq,
           run_log_chunks.content,
           run_log_chunks.size_bytes,
           run_log_chunks.created_at
      FROM run_log_chunks
      JOIN current_run_lease ON current_run_lease.org_id = run_log_chunks.org_id
                            AND current_run_lease.id = run_log_chunks.run_id
                            AND current_run_lease.run_lease_id = run_log_chunks.run_lease_id
     WHERE run_log_chunks.stream = sqlc.arg(stream)::run_log_stream
       AND run_log_chunks.observed_seq = sqlc.arg(observed_seq)
       AND NOT EXISTS (SELECT 1 FROM inserted)
),
selected_chunk AS (
    SELECT * FROM inserted
    UNION ALL
    SELECT * FROM existing
),
event_input AS (
    SELECT current_run_lease.org_id,
           current_run_lease.project_id,
           current_run_lease.environment_id,
           selected_chunk.run_id,
           current_run_lease.attempt_id,
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
     WHERE EXISTS (SELECT 1 FROM inserted)
),
event_seq AS (
    INSERT INTO event_subject_cursors (org_id, subject_type, subject_id, last_seq)
    SELECT event_input.org_id, 'run', event_input.run_id, 1
      FROM event_input
    ON CONFLICT (org_id, subject_type, subject_id)
    DO UPDATE SET last_seq = event_subject_cursors.last_seq + 1,
                  updated_at = now()
    RETURNING org_id, subject_type, subject_id, last_seq
),
event AS (
    INSERT INTO events (org_id, project_id, environment_id, run_id, seq, attempt_id, run_lease_id, attempt_number, trace_id, span_id, parent_span_id, traceparent, category, severity, source, kind, message, payload, redaction_class, snapshot_version)
    SELECT event_input.org_id,
           event_input.project_id,
           event_input.environment_id,
           event_input.run_id,
           event_seq.last_seq,
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
                    AND event_seq.subject_type = 'run'
                    AND event_seq.subject_id = event_input.run_id
    RETURNING *
),
event_outbox AS (
    INSERT INTO event_outbox (event_record_id, stream_key)
    SELECT event.id,
           'helmr:events:' || event.org_id::text || ':' || event.subject_type::text || ':' || event.subject_id::text
      FROM event
    RETURNING id
),
usage_event AS (
    INSERT INTO run_usage_events (org_id, project_id, environment_id, run_id, attempt_id, run_lease_id, trace_id, span_id, snapshot_version, kind, quantity, unit, attributes, idempotency_key)
    SELECT current_run_lease.org_id,
           current_run_lease.project_id,
           current_run_lease.environment_id,
           selected_chunk.run_id,
           current_run_lease.attempt_id,
           selected_chunk.run_lease_id,
           current_run_lease.trace_id,
           current_run_lease.span_id,
           current_run_lease.state_version,
           'log_bytes',
           selected_chunk.size_bytes,
           'bytes',
           jsonb_build_object('stream', selected_chunk.stream, 'observed_seq', selected_chunk.observed_seq),
           'log:' || selected_chunk.run_lease_id::text || ':' || selected_chunk.stream::text || ':' || selected_chunk.observed_seq::text
      FROM selected_chunk
      JOIN current_run_lease ON current_run_lease.org_id = selected_chunk.org_id
                          AND current_run_lease.id = selected_chunk.run_id
     WHERE EXISTS (SELECT 1 FROM inserted)
       AND selected_chunk.size_bytes > 0
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
 WHERE (SELECT count(*) FROM event_outbox) >= 0
   AND (SELECT count(*) FROM usage_event) >= 0;

-- name: GetRunLogSnapshot :one
WITH run_scope AS (
    SELECT runs.org_id, runs.id
      FROM runs
     WHERE runs.org_id = sqlc.arg(org_id)
       AND runs.id = sqlc.arg(run_id)
),
chunks AS (
    SELECT run_log_chunks.stream,
           run_log_chunks.seq,
           run_log_chunks.content,
           octet_length(run_log_chunks.content)::bigint AS size_bytes,
           SUM(octet_length(run_log_chunks.content)::bigint) OVER (
               PARTITION BY run_log_chunks.stream
               ORDER BY run_log_chunks.seq DESC
           ) AS reverse_bytes,
           SUM(octet_length(run_log_chunks.content)::bigint) OVER (
               PARTITION BY run_log_chunks.stream
           ) AS total_bytes
      FROM run_scope
      JOIN run_log_chunks ON run_log_chunks.org_id = run_scope.org_id
                         AND run_log_chunks.run_id = run_scope.id
),
sliced AS (
    SELECT stream,
           seq,
           total_bytes,
           CASE stream
             WHEN 'stdout' THEN
               CASE
                 WHEN reverse_bytes - size_bytes >= sqlc.arg(stdout_limit)::bigint THEN NULL
                 WHEN reverse_bytes > sqlc.arg(stdout_limit)::bigint THEN
                   substr(
                     content,
                     (size_bytes - (sqlc.arg(stdout_limit)::bigint - (reverse_bytes - size_bytes)) + 1)::int
                   )
                 ELSE content
               END
             WHEN 'stderr' THEN
               CASE
                 WHEN reverse_bytes - size_bytes >= sqlc.arg(stderr_limit)::bigint THEN NULL
                 WHEN reverse_bytes > sqlc.arg(stderr_limit)::bigint THEN
                   substr(
                     content,
                     (size_bytes - (sqlc.arg(stderr_limit)::bigint - (reverse_bytes - size_bytes)) + 1)::int
                   )
                 ELSE content
               END
           END AS content
      FROM chunks
)
SELECT run_scope.id AS run_id,
       COALESCE(
           string_agg(sliced.content, ''::bytea ORDER BY sliced.seq)
             FILTER (WHERE sliced.stream = 'stdout' AND sliced.content IS NOT NULL),
           ''::bytea
       )::bytea AS stdout,
       COALESCE(
           string_agg(sliced.content, ''::bytea ORDER BY sliced.seq)
             FILTER (WHERE sliced.stream = 'stderr' AND sliced.content IS NOT NULL),
           ''::bytea
       )::bytea AS stderr,
       (
           COALESCE(MAX(sliced.total_bytes) FILTER (WHERE sliced.stream = 'stdout'), 0) > sqlc.arg(stdout_limit)::bigint
           OR COALESCE(MAX(sliced.total_bytes) FILTER (WHERE sliced.stream = 'stderr'), 0) > sqlc.arg(stderr_limit)::bigint
       ) AS truncated,
       COALESCE(MAX(chunks.seq), 0)::bigint AS cursor,
       COALESCE(MAX(sliced.total_bytes) FILTER (WHERE sliced.stream = 'stdout'), 0)::bigint AS stdout_bytes,
       COALESCE(MAX(sliced.total_bytes) FILTER (WHERE sliced.stream = 'stderr'), 0)::bigint AS stderr_bytes,
       now()::timestamptz AS updated_at
  FROM run_scope
  LEFT JOIN chunks ON true
  LEFT JOIN sliced ON sliced.stream = chunks.stream
                  AND sliced.seq = chunks.seq
 GROUP BY run_scope.id;

-- name: ListRunLogChunksAfter :many
SELECT run_log_chunks.org_id,
       run_log_chunks.run_id,
       run_log_chunks.run_lease_id,
       run_log_chunks.attempt_number,
       run_log_chunks.stream,
       run_log_chunks.seq,
       run_log_chunks.observed_seq,
       run_log_chunks.content,
       run_log_chunks.size_bytes,
       run_log_chunks.created_at
  FROM run_log_chunks
 WHERE run_log_chunks.org_id = sqlc.arg(org_id)
   AND run_log_chunks.run_id = sqlc.arg(run_id)
   AND run_log_chunks.seq > sqlc.arg(seq)
 ORDER BY run_log_chunks.seq
 LIMIT sqlc.arg(row_limit);
