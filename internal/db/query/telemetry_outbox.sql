-- name: ClaimEventIngestBatch :many
WITH claimed AS (
    SELECT telemetry_outbox.id
      FROM telemetry_outbox
     WHERE telemetry_outbox.stream_kind = 'event'
       AND telemetry_outbox.written_at IS NULL
       AND telemetry_outbox.state IN ('pending', 'claimed', 'failed')
       AND (telemetry_outbox.next_retry_at IS NULL OR telemetry_outbox.next_retry_at <= now())
     ORDER BY telemetry_outbox.id ASC
     LIMIT sqlc.arg(row_limit)
     FOR UPDATE SKIP LOCKED
),
updated AS (
    UPDATE telemetry_outbox
       SET state = 'claimed',
           retry_count = telemetry_outbox.retry_count + 1,
           next_retry_at = now() + sqlc.arg(lease_duration)::interval,
           updated_at = now(),
           last_error = ''
      FROM claimed
     WHERE telemetry_outbox.id = claimed.id
    RETURNING telemetry_outbox.*
)
SELECT updated.id AS outbox_id,
       updated.retry_count,
       updated.idempotency_key,
       events.*
  FROM updated
  JOIN event_hot_payloads AS events ON events.org_id = updated.org_id
                                   AND events.cell_id = updated.cell_id
                                   AND events.subject_type = updated.source_kind::event_subject_type
                                   AND events.subject_id = updated.source_id
                                   AND events.seq = updated.seq
 ORDER BY updated.id ASC;

-- name: ClaimRunLogIngestBatch :many
WITH claimed AS (
    SELECT telemetry_outbox.id
      FROM telemetry_outbox
     WHERE telemetry_outbox.stream_kind = 'run_log'
       AND telemetry_outbox.written_at IS NULL
       AND telemetry_outbox.state IN ('pending', 'claimed', 'failed')
       AND (telemetry_outbox.next_retry_at IS NULL OR telemetry_outbox.next_retry_at <= now())
     ORDER BY telemetry_outbox.id ASC
     LIMIT sqlc.arg(row_limit)
     FOR UPDATE SKIP LOCKED
),
updated AS (
    UPDATE telemetry_outbox
       SET state = 'claimed',
           retry_count = telemetry_outbox.retry_count + 1,
           next_retry_at = now() + sqlc.arg(lease_duration)::interval,
           updated_at = now(),
           last_error = ''
      FROM claimed
     WHERE telemetry_outbox.id = claimed.id
    RETURNING telemetry_outbox.*
)
SELECT updated.id AS outbox_id,
       updated.retry_count,
       updated.idempotency_key,
       chunks.org_id,
       chunks.cell_id,
       runs.project_id,
       runs.environment_id,
       chunks.run_id,
       run_leases.attempt_id,
       chunks.run_lease_id,
       chunks.attempt_number,
       chunks.stream,
       chunks.seq,
       chunks.observed_seq,
       chunks.content,
       chunks.size_bytes,
       chunks.created_at
  FROM updated
  JOIN run_log_hot_chunks AS chunks ON chunks.org_id = updated.org_id
                                   AND chunks.cell_id = updated.cell_id
                                   AND chunks.run_id = updated.source_id
                                   AND chunks.stream::text = updated.stream_name
                                   AND chunks.seq = updated.seq
  JOIN runs ON runs.org_id = chunks.org_id
           AND runs.id = chunks.run_id
  JOIN run_leases ON run_leases.org_id = chunks.org_id
                 AND run_leases.run_id = chunks.run_id
                 AND run_leases.id = chunks.run_lease_id
 ORDER BY updated.id ASC;

-- name: ClaimWorkspaceExecTerminalOutputIngestBatch :many
WITH claimed AS (
    SELECT telemetry_outbox.id
      FROM telemetry_outbox
     WHERE telemetry_outbox.stream_kind = 'terminal_output'
       AND telemetry_outbox.source_kind = 'workspace_exec'
       AND telemetry_outbox.written_at IS NULL
       AND telemetry_outbox.state IN ('pending', 'claimed', 'failed')
       AND (telemetry_outbox.next_retry_at IS NULL OR telemetry_outbox.next_retry_at <= now())
       AND EXISTS (
             SELECT 1
               FROM workspace_exec_stream_chunks AS chunks
              WHERE chunks.org_id = telemetry_outbox.org_id
                AND chunks.cell_id = telemetry_outbox.cell_id
                AND chunks.exec_id = telemetry_outbox.source_id
                AND chunks.stream::text = telemetry_outbox.stream_name
                AND chunks.offset_start = telemetry_outbox.seq
       )
     ORDER BY telemetry_outbox.id ASC
     LIMIT sqlc.arg(row_limit)
     FOR UPDATE SKIP LOCKED
),
updated AS (
    UPDATE telemetry_outbox
       SET state = 'claimed',
           retry_count = telemetry_outbox.retry_count + 1,
           next_retry_at = now() + sqlc.arg(lease_duration)::interval,
           updated_at = now(),
           last_error = ''
      FROM claimed
     WHERE telemetry_outbox.id = claimed.id
    RETURNING telemetry_outbox.*
)
SELECT updated.id AS outbox_id,
       updated.retry_count,
       updated.idempotency_key,
       chunks.org_id,
       chunks.cell_id,
       chunks.project_id,
       chunks.environment_id,
       chunks.workspace_id,
       'workspace_exec'::text AS resource_kind,
       chunks.exec_id AS resource_id,
       chunks.stream::text AS stream_name,
       chunks.offset_start,
       chunks.offset_end,
       chunks.data,
       chunks.observed_at
  FROM updated
  JOIN workspace_exec_stream_chunks AS chunks ON chunks.org_id = updated.org_id
                                             AND chunks.cell_id = updated.cell_id
                                             AND chunks.exec_id = updated.source_id
                                             AND chunks.stream::text = updated.stream_name
                                             AND chunks.offset_start = updated.seq
 ORDER BY updated.id ASC;

-- name: ClaimWorkspacePtyTerminalOutputIngestBatch :many
WITH claimed AS (
    SELECT telemetry_outbox.id
      FROM telemetry_outbox
     WHERE telemetry_outbox.stream_kind = 'terminal_output'
       AND telemetry_outbox.source_kind = 'workspace_pty'
       AND telemetry_outbox.written_at IS NULL
       AND telemetry_outbox.state IN ('pending', 'claimed', 'failed')
       AND (telemetry_outbox.next_retry_at IS NULL OR telemetry_outbox.next_retry_at <= now())
       AND EXISTS (
             SELECT 1
               FROM workspace_pty_stream_chunks AS chunks
              WHERE chunks.org_id = telemetry_outbox.org_id
                AND chunks.cell_id = telemetry_outbox.cell_id
                AND chunks.pty_session_id = telemetry_outbox.source_id
                AND chunks.stream::text = telemetry_outbox.stream_name
                AND chunks.offset_start = telemetry_outbox.seq
       )
     ORDER BY telemetry_outbox.id ASC
     LIMIT sqlc.arg(row_limit)
     FOR UPDATE SKIP LOCKED
),
updated AS (
    UPDATE telemetry_outbox
       SET state = 'claimed',
           retry_count = telemetry_outbox.retry_count + 1,
           next_retry_at = now() + sqlc.arg(lease_duration)::interval,
           updated_at = now(),
           last_error = ''
      FROM claimed
     WHERE telemetry_outbox.id = claimed.id
    RETURNING telemetry_outbox.*
)
SELECT updated.id AS outbox_id,
       updated.retry_count,
       updated.idempotency_key,
       chunks.org_id,
       chunks.cell_id,
       chunks.project_id,
       chunks.environment_id,
       chunks.workspace_id,
       'workspace_pty'::text AS resource_kind,
       chunks.pty_session_id AS resource_id,
       chunks.stream::text AS stream_name,
       chunks.offset_start,
       chunks.offset_end,
       chunks.data,
       chunks.observed_at
  FROM updated
  JOIN workspace_pty_stream_chunks AS chunks ON chunks.org_id = updated.org_id
                                            AND chunks.cell_id = updated.cell_id
                                            AND chunks.pty_session_id = updated.source_id
                                            AND chunks.stream::text = updated.stream_name
                                            AND chunks.offset_start = updated.seq
 ORDER BY updated.id ASC;

-- name: DeadLetterOrphanedTelemetryOutbox :many
WITH candidates AS (
    SELECT telemetry_outbox.id
      FROM telemetry_outbox
     WHERE telemetry_outbox.written_at IS NULL
       AND telemetry_outbox.state IN ('pending', 'claimed', 'failed')
       AND (
            (
                telemetry_outbox.stream_kind = 'event'
                AND telemetry_outbox.source_kind IN ('run', 'deployment')
                AND NOT EXISTS (
                    SELECT 1
                      FROM event_hot_payloads AS events
                     WHERE events.org_id = telemetry_outbox.org_id
                       AND events.cell_id = telemetry_outbox.cell_id
                       AND events.subject_type = telemetry_outbox.source_kind::event_subject_type
                       AND events.subject_id = telemetry_outbox.source_id
                       AND events.seq = telemetry_outbox.seq
                )
            )
            OR (
                telemetry_outbox.stream_kind = 'run_log'
                AND NOT EXISTS (
                    SELECT 1
                      FROM run_log_hot_chunks AS chunks
                     WHERE chunks.org_id = telemetry_outbox.org_id
                       AND chunks.cell_id = telemetry_outbox.cell_id
                       AND chunks.run_id = telemetry_outbox.source_id
                       AND chunks.stream::text = telemetry_outbox.stream_name
                       AND chunks.seq = telemetry_outbox.seq
                )
            )
            OR (
                telemetry_outbox.stream_kind = 'terminal_output'
                AND telemetry_outbox.source_kind = 'workspace_exec'
                AND NOT EXISTS (
                    SELECT 1
                      FROM workspace_exec_stream_chunks AS chunks
                     WHERE chunks.org_id = telemetry_outbox.org_id
                       AND chunks.cell_id = telemetry_outbox.cell_id
                       AND chunks.exec_id = telemetry_outbox.source_id
                       AND chunks.stream::text = telemetry_outbox.stream_name
                       AND chunks.offset_start = telemetry_outbox.seq
                )
            )
            OR (
                telemetry_outbox.stream_kind = 'terminal_output'
                AND telemetry_outbox.source_kind = 'workspace_pty'
                AND NOT EXISTS (
                    SELECT 1
                      FROM workspace_pty_stream_chunks AS chunks
                     WHERE chunks.org_id = telemetry_outbox.org_id
                       AND chunks.cell_id = telemetry_outbox.cell_id
                       AND chunks.pty_session_id = telemetry_outbox.source_id
                       AND chunks.stream::text = telemetry_outbox.stream_name
                       AND chunks.offset_start = telemetry_outbox.seq
                )
            )
       )
     ORDER BY telemetry_outbox.id ASC
     LIMIT sqlc.arg(row_limit)
     FOR UPDATE SKIP LOCKED
),
dead_lettered AS (
    UPDATE telemetry_outbox
       SET state = 'dead_lettered',
           next_retry_at = NULL,
           updated_at = now(),
           last_error = 'telemetry payload missing'
      FROM candidates
     WHERE telemetry_outbox.id = candidates.id
       AND telemetry_outbox.written_at IS NULL
    RETURNING telemetry_outbox.*
),
replay_errors AS (
    INSERT INTO telemetry_replay_errors (org_id, cell_id, stream_kind, source_kind, source_id, stream_name, seq, state, retry_count, last_error, next_retry_at)
    SELECT org_id,
           cell_id,
           stream_kind,
           source_kind,
           source_id,
           stream_name,
           seq,
           'dead_lettered',
           retry_count,
           last_error,
           NULL
      FROM dead_lettered
    RETURNING id
)
SELECT dead_lettered.id
  FROM dead_lettered
 ORDER BY dead_lettered.id;

-- name: MarkTelemetryOutboxWritten :exec
UPDATE telemetry_outbox
   SET state = 'written',
       written_at = now(),
       retry_count = 0,
       next_retry_at = NULL,
       updated_at = now(),
       last_error = ''
 WHERE id = ANY(sqlc.arg(ids)::bigint[]);

-- name: MarkTelemetryOutboxBatchFailed :exec
UPDATE telemetry_outbox
   SET state = 'failed',
       next_retry_at = now() + sqlc.arg(retry_after)::interval,
       updated_at = now(),
       last_error = sqlc.arg(last_error)
 WHERE id = ANY(sqlc.arg(ids)::bigint[])
   AND written_at IS NULL;

-- name: RequeueWrittenTelemetryOutbox :exec
UPDATE telemetry_outbox
   SET state = 'failed',
       written_at = NULL,
       next_retry_at = now() + sqlc.arg(retry_after)::interval,
       updated_at = now(),
       last_error = sqlc.arg(last_error)
 WHERE id = ANY(sqlc.arg(ids)::bigint[])
   AND written_at IS NOT NULL;

-- name: DeadLetterTelemetryOutbox :exec
WITH dead_lettered AS (
    UPDATE telemetry_outbox
       SET state = 'dead_lettered',
           next_retry_at = NULL,
           updated_at = now(),
           last_error = sqlc.arg(last_error)
     WHERE telemetry_outbox.id = sqlc.arg(id)
       AND telemetry_outbox.written_at IS NULL
    RETURNING *
)
INSERT INTO telemetry_replay_errors (org_id, cell_id, stream_kind, source_kind, source_id, stream_name, seq, state, retry_count, last_error, next_retry_at)
SELECT org_id,
       cell_id,
       stream_kind,
       source_kind,
       source_id,
       stream_name,
       seq,
       'retryable',
       retry_count,
       last_error,
       NULL
  FROM dead_lettered;

-- name: ListDeadLetteredTelemetrySeqs :many
SELECT telemetry_outbox.seq
  FROM telemetry_outbox
 WHERE telemetry_outbox.org_id = sqlc.arg(org_id)
   AND telemetry_outbox.cell_id = sqlc.arg(cell_id)
   AND telemetry_outbox.stream_kind = sqlc.arg(stream_kind)::telemetry_stream_kind
   AND telemetry_outbox.source_kind = sqlc.arg(source_kind)
   AND telemetry_outbox.source_id = sqlc.arg(source_id)
   AND (
        sqlc.arg(stream_name)::text = ''
        OR telemetry_outbox.stream_name = sqlc.arg(stream_name)::text
   )
   AND telemetry_outbox.seq > sqlc.arg(after_seq)
   AND telemetry_outbox.seq <= sqlc.arg(watermark_seq)
   AND telemetry_outbox.state = 'dead_lettered'
UNION
SELECT telemetry_replay_errors.seq
  FROM telemetry_replay_errors
 WHERE telemetry_replay_errors.org_id = sqlc.arg(org_id)
   AND telemetry_replay_errors.cell_id = sqlc.arg(cell_id)
   AND telemetry_replay_errors.stream_kind = sqlc.arg(stream_kind)::telemetry_stream_kind
   AND telemetry_replay_errors.source_kind = sqlc.arg(source_kind)
   AND telemetry_replay_errors.source_id = sqlc.arg(source_id)
   AND (
        sqlc.arg(stream_name)::text = ''
        OR telemetry_replay_errors.stream_name = sqlc.arg(stream_name)::text
   )
   AND telemetry_replay_errors.seq > sqlc.arg(after_seq)
   AND telemetry_replay_errors.seq <= sqlc.arg(watermark_seq)
   AND telemetry_replay_errors.state = 'dead_lettered'
ORDER BY seq;

-- name: GetTelemetryIngestFrontier :one
SELECT COALESCE(
    (
        SELECT MIN(seq) - 1
                 FROM telemetry_outbox
                WHERE org_id = sqlc.arg(org_id)
                  AND cell_id = sqlc.arg(cell_id)
                  AND stream_kind = sqlc.arg(stream_kind)::telemetry_stream_kind
                  AND source_kind = sqlc.arg(source_kind)
                  AND source_id = sqlc.arg(source_id)
                  AND stream_name = sqlc.arg(stream_name)
                  AND written_at IS NULL
                  AND state <> 'dead_lettered'
           ),
    sqlc.arg(max_written_seq)::bigint
)::bigint AS watermark_seq;

-- name: GetRunLogIngestFrontier :one
SELECT COALESCE(
    (
        SELECT MIN(seq) - 1
          FROM telemetry_outbox
         WHERE org_id = sqlc.arg(org_id)
           AND cell_id = sqlc.arg(cell_id)
           AND stream_kind = 'run_log'
           AND source_kind = 'run'
           AND source_id = sqlc.arg(run_id)
           AND written_at IS NULL
           AND state <> 'dead_lettered'
    ),
    sqlc.arg(max_written_seq)::bigint
)::bigint AS watermark_seq;

-- name: GetTerminalOutputIngestFrontier :one
SELECT COALESCE(
    (
        SELECT MIN(seq)
          FROM telemetry_outbox
         WHERE org_id = sqlc.arg(org_id)
           AND cell_id = sqlc.arg(cell_id)
           AND stream_kind = 'terminal_output'
           AND source_kind = sqlc.arg(source_kind)
           AND source_id = sqlc.arg(source_id)
           AND stream_name = sqlc.arg(stream_name)
           AND written_at IS NULL
           AND state <> 'dead_lettered'
    ),
    sqlc.arg(max_written_offset)::bigint
)::bigint AS watermark_offset;

-- name: PruneTelemetryOutboxWritten :many
DELETE FROM telemetry_outbox
 WHERE (
        (
            written_at IS NOT NULL
            AND (stream_kind <> 'event' OR published_at IS NOT NULL)
            AND written_at < now() - sqlc.arg(retain_for)::interval
        )
        OR (
            state = 'dead_lettered'
            AND updated_at < now() - sqlc.arg(retain_for)::interval
        )
 )
RETURNING id;
