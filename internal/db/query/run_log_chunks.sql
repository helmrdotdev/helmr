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
           run_leases.attempt_id,
           run_leases.span_id,
           run_leases.parent_span_id,
           run_leases.traceparent,
           run_attempts.attempt_number
      FROM runs
      JOIN run_leases ON run_leases.id = runs.current_run_lease_id
                          AND run_leases.org_id = runs.org_id
                          AND run_leases.run_id = runs.id
      JOIN worker_groups
        ON worker_groups.id = runs.worker_group_id
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
inserted_run_log_cursor AS (
    INSERT INTO run_log_cursors (org_id, run_id, stream_name, seq, cursor, idempotency_key)
    SELECT current_run_lease.org_id,
           current_run_lease.id,
           '__run__',
           1,
           'rlc1.' || current_run_lease.org_id::text || '.' || current_run_lease.id::text || '.__run__.1',
           '__head__'
      FROM current_run_lease
    ON CONFLICT (org_id, run_id, stream_name, idempotency_key) DO NOTHING
    RETURNING run_log_cursors.org_id,
              run_log_cursors.run_id,
              run_log_cursors.seq,
              true AS inserted
),
existing_run_log_cursor AS (
    SELECT run_log_cursors.org_id,
           run_log_cursors.run_id,
           run_log_cursors.seq,
           false AS inserted
      FROM run_log_cursors
      JOIN current_run_lease ON current_run_lease.org_id = run_log_cursors.org_id
                            AND current_run_lease.id = run_log_cursors.run_id
     WHERE run_log_cursors.stream_name = '__run__'
       AND run_log_cursors.idempotency_key = '__head__'
       AND NOT EXISTS (SELECT 1 FROM inserted_run_log_cursor)
     FOR UPDATE OF run_log_cursors
),
locked_run_log_cursor AS (
    SELECT * FROM inserted_run_log_cursor
    UNION ALL
    SELECT * FROM existing_run_log_cursor
),
selected_cursor AS (
    INSERT INTO run_log_cursors (org_id, run_id, attempt_id, run_lease_id, stream_name, seq, cursor, idempotency_key)
    SELECT locked_run_log_cursor.org_id,
           locked_run_log_cursor.run_id,
           current_run_lease.attempt_id,
           current_run_lease.run_lease_id,
           (sqlc.arg(stream)::run_log_stream)::text,
           CASE WHEN locked_run_log_cursor.inserted THEN locked_run_log_cursor.seq ELSE locked_run_log_cursor.seq + 1 END,
           'rlc1.' || locked_run_log_cursor.org_id::text || '.' || locked_run_log_cursor.run_id::text || '.' || (sqlc.arg(stream)::run_log_stream)::text || '.' || (CASE WHEN locked_run_log_cursor.inserted THEN locked_run_log_cursor.seq ELSE locked_run_log_cursor.seq + 1 END)::text,
           'log:' || current_run_lease.run_lease_id::text || ':' || (sqlc.arg(stream)::run_log_stream)::text || ':' || (sqlc.arg(observed_seq)::bigint)::text
      FROM locked_run_log_cursor
      JOIN current_run_lease ON current_run_lease.org_id = locked_run_log_cursor.org_id
                            AND current_run_lease.id = locked_run_log_cursor.run_id
    ON CONFLICT (org_id, run_id, stream_name, idempotency_key)
    DO UPDATE SET observed_at = run_log_cursors.observed_at
    RETURNING run_log_cursors.org_id,
              run_log_cursors.run_id,
              run_log_cursors.stream_name,
              run_log_cursors.seq
),
advanced_run_log_cursor AS (
    UPDATE run_log_cursors
       SET seq = selected_cursor.seq,
           cursor = 'rlc1.' || run_log_cursors.org_id::text || '.' || run_log_cursors.run_id::text || '.__run__.' || selected_cursor.seq::text,
           observed_at = now()
      FROM selected_cursor
      JOIN locked_run_log_cursor ON locked_run_log_cursor.org_id = selected_cursor.org_id
                                AND locked_run_log_cursor.run_id = selected_cursor.run_id
     WHERE run_log_cursors.org_id = selected_cursor.org_id
       AND run_log_cursors.run_id = selected_cursor.run_id
       AND run_log_cursors.stream_name = '__run__'
       AND run_log_cursors.idempotency_key = '__head__'
       AND NOT locked_run_log_cursor.inserted
       AND selected_cursor.seq > run_log_cursors.seq
    RETURNING run_log_cursors.id
),
selected_chunk AS (
    INSERT INTO run_log_hot_chunks (org_id, worker_group_id, run_id, run_lease_id, attempt_number, stream, seq, observed_seq, content, size_bytes, created_at)
    SELECT current_run_lease.org_id,
           current_run_lease.worker_group_id,
           current_run_lease.id,
           current_run_lease.run_lease_id,
           current_run_lease.attempt_number,
           sqlc.arg(stream)::run_log_stream,
           selected_cursor.seq,
           sqlc.arg(observed_seq)::bigint,
           sqlc.arg(content)::bytea,
           octet_length(sqlc.arg(content)::bytea)::bigint,
           now()
      FROM current_run_lease
      JOIN selected_cursor ON selected_cursor.org_id = current_run_lease.org_id
                          AND selected_cursor.run_id = current_run_lease.id
    ON CONFLICT (org_id, run_id, run_lease_id, stream, observed_seq)
    DO UPDATE SET size_bytes = run_log_hot_chunks.size_bytes
    RETURNING run_log_hot_chunks.org_id,
              run_log_hot_chunks.worker_group_id,
              run_log_hot_chunks.run_id,
              run_log_hot_chunks.run_lease_id,
              run_log_hot_chunks.attempt_number,
              run_log_hot_chunks.stream,
              run_log_hot_chunks.seq,
              run_log_hot_chunks.observed_seq,
              run_log_hot_chunks.content,
              run_log_hot_chunks.size_bytes,
              run_log_hot_chunks.created_at,
              (
                  (SELECT locked_run_log_cursor.inserted FROM locked_run_log_cursor)
                  OR EXISTS (SELECT 1 FROM advanced_run_log_cursor)
              ) AS is_new
),
event_input AS (
    SELECT current_run_lease.org_id,
           current_run_lease.worker_group_id,
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
                            AND current_run_lease.worker_group_id = selected_chunk.worker_group_id
                            AND current_run_lease.id = selected_chunk.run_id
      CROSS JOIN event_args
     WHERE selected_chunk.is_new
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
event AS (
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
event_telemetry_outbox AS (
    INSERT INTO telemetry_outbox (org_id, worker_group_id, stream_kind, source_kind, source_id, seq, idempotency_key)
    SELECT event.org_id,
                  event.worker_group_id,
                  'event',
                  event.subject_type,
                  event.subject_id,
                  event.seq,
                  'event:' || event.subject_type::text || ':' || event.subject_id::text || ':' || event.seq::text
      FROM event
    RETURNING id
),
run_log_telemetry_outbox AS (
    INSERT INTO telemetry_outbox (org_id, worker_group_id, stream_kind, source_kind, source_id, stream_name, seq, idempotency_key)
    SELECT selected_chunk.org_id,
           selected_chunk.worker_group_id,
           'run_log',
           'run',
           selected_chunk.run_id,
           selected_chunk.stream::text,
           selected_chunk.seq,
           'run_log:' || selected_chunk.run_id::text || ':' || selected_chunk.stream::text || ':' || selected_chunk.seq::text
      FROM selected_chunk
     WHERE selected_chunk.is_new
    RETURNING id
),
usage_event AS (
    INSERT INTO usage_ledger_entries (org_id, project_id, environment_id, source_type, source_id, run_id, attempt_number, trace_id, span_id, meter, quantity, unit, details, idempotency_key)
    SELECT current_run_lease.org_id,
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
 WHERE (SELECT count(*) FROM event_telemetry_outbox) >= 0
   AND (SELECT count(*) FROM run_log_telemetry_outbox) >= 0
   AND (SELECT count(*) FROM advanced_run_log_cursor) >= 0
   AND (SELECT count(*) FROM usage_event) >= 0;

-- name: GetRunLogSnapshot :one
WITH run_scope AS (
    SELECT runs.org_id, runs.id
      FROM runs
     WHERE runs.org_id = sqlc.arg(org_id)
       AND runs.id = sqlc.arg(run_id)
),
chunks AS (
    SELECT run_log_hot_chunks.stream,
           run_log_hot_chunks.seq,
           run_log_hot_chunks.content,
           octet_length(run_log_hot_chunks.content)::bigint AS size_bytes,
           SUM(octet_length(run_log_hot_chunks.content)::bigint) OVER (
               PARTITION BY run_log_hot_chunks.stream
               ORDER BY run_log_hot_chunks.seq DESC
           ) AS reverse_bytes,
           SUM(octet_length(run_log_hot_chunks.content)::bigint) OVER (
               PARTITION BY run_log_hot_chunks.stream
           ) AS total_bytes
      FROM run_scope
      JOIN run_log_hot_chunks ON run_log_hot_chunks.org_id = run_scope.org_id
                         AND run_log_hot_chunks.run_id = run_scope.id
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
SELECT *
  FROM run_log_hot_chunks
 WHERE run_log_hot_chunks.org_id = sqlc.arg(org_id)
   AND run_log_hot_chunks.run_id = sqlc.arg(run_id)
   AND run_log_hot_chunks.seq > sqlc.arg(seq)
 ORDER BY run_log_hot_chunks.seq
 LIMIT sqlc.arg(row_limit);
