-- name: AppendRunLogChunk :one
WITH current_session AS (
    SELECT runs.org_id,
           runs.id,
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
     FOR UPDATE OF runs
),
next_seq AS (
    SELECT COALESCE(MAX(run_log_chunks.seq), 0) + 1 AS seq
      FROM run_log_chunks
      JOIN current_session ON current_session.org_id = run_log_chunks.org_id
                            AND current_session.id = run_log_chunks.run_id
     WHERE run_log_chunks.stream = sqlc.arg(stream)::run_log_stream
),
inserted AS (
    INSERT INTO run_log_chunks (org_id, run_id, session_id, attempt_number, stream, seq, observed_seq, content, created_at)
    SELECT org_id,
           id,
           session_id,
           attempt_number,
           sqlc.arg(stream)::run_log_stream,
           next_seq.seq,
           sqlc.arg(observed_seq),
           sqlc.arg(content),
           now()
      FROM current_session
      JOIN next_seq ON true
    ON CONFLICT (org_id, run_id, session_id, stream, observed_seq) DO NOTHING
    RETURNING org_id, run_id, session_id, attempt_number, stream, seq, observed_seq, content, created_at
),
existing AS (
    SELECT run_log_chunks.org_id,
           run_log_chunks.run_id,
           run_log_chunks.session_id,
           run_log_chunks.attempt_number,
           run_log_chunks.stream,
           run_log_chunks.seq,
           run_log_chunks.observed_seq,
           run_log_chunks.content,
           run_log_chunks.created_at
      FROM run_log_chunks
      JOIN current_session ON current_session.org_id = run_log_chunks.org_id
                            AND current_session.id = run_log_chunks.run_id
                            AND current_session.session_id = run_log_chunks.session_id
     WHERE run_log_chunks.stream = sqlc.arg(stream)::run_log_stream
       AND run_log_chunks.observed_seq = sqlc.arg(observed_seq)
       AND NOT EXISTS (SELECT 1 FROM inserted)
),
selected_chunk AS (
    SELECT * FROM inserted
    UNION ALL
    SELECT * FROM existing
),
event AS (
    INSERT INTO run_events (org_id, run_id, session_id, attempt_number, kind, payload)
    SELECT sqlc.arg(org_id), run_id, session_id, attempt_number, sqlc.arg(kind), sqlc.arg(payload)
      FROM selected_chunk
     WHERE EXISTS (SELECT 1 FROM inserted)
    RETURNING id
)
SELECT selected_chunk.org_id,
       selected_chunk.run_id,
       selected_chunk.session_id,
       selected_chunk.attempt_number,
       selected_chunk.stream,
       selected_chunk.seq,
       selected_chunk.observed_seq,
       selected_chunk.content,
       selected_chunk.created_at
  FROM selected_chunk;

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
       COALESCE(MAX(sliced.total_bytes) FILTER (WHERE sliced.stream = 'stdout'), 0)::bigint AS stdout_cursor,
       COALESCE(MAX(sliced.total_bytes) FILTER (WHERE sliced.stream = 'stderr'), 0)::bigint AS stderr_cursor,
       now()::timestamptz AS updated_at
  FROM run_scope
  LEFT JOIN sliced ON true
 GROUP BY run_scope.id;
