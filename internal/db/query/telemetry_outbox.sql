-- name: ClaimEventIngestBatch :many
WITH candidates AS (
    SELECT telemetry_outbox.id,
           octet_length(telemetry_outbox.message)::bigint
               + octet_length(telemetry_outbox.payload::text)::bigint AS size_bytes
      FROM telemetry_outbox
     WHERE telemetry_outbox.stream_kind = 'event'
       AND telemetry_outbox.written_at IS NULL
       AND telemetry_outbox.state IN ('pending', 'claimed', 'failed')
       AND (telemetry_outbox.next_retry_at IS NULL OR telemetry_outbox.next_retry_at <= now())
     ORDER BY telemetry_outbox.id ASC
     LIMIT sqlc.arg(row_limit)
     FOR UPDATE SKIP LOCKED
),
claimed AS (
    SELECT sized.id
      FROM (
          SELECT candidates.id,
                 SUM(candidates.size_bytes) OVER (ORDER BY candidates.id ASC) AS cumulative_size_bytes
            FROM candidates
      ) AS sized
     WHERE sized.cumulative_size_bytes <= sqlc.arg(max_batch_bytes)::bigint
),
updated AS (
    UPDATE telemetry_outbox
       SET state = 'claimed',
           retry_count = telemetry_outbox.retry_count + 1,
           next_retry_at = now() + sqlc.arg(lease_duration)::interval,
           updated_at = now()
      FROM claimed
     WHERE telemetry_outbox.id = claimed.id
    RETURNING telemetry_outbox.*
)
SELECT updated.id AS outbox_id,
       updated.retry_count,
       COALESCE(updated.idempotency_key, '')::text AS idempotency_key,
       updated.source_kind AS subject_type,
       updated.source_id AS subject_id,
       updated.id AS seq,
       updated.org_id,
       updated.project_id,
       updated.environment_id,
       updated.run_id,
       updated.deployment_id,
       updated.run_lease_id,
       updated.attempt_number,
       updated.trace_id,
       updated.span_id,
       updated.parent_span_id,
       updated.traceparent,
       updated.category,
       updated.severity,
       updated.source,
       updated.kind,
       updated.message,
       updated.payload,
       updated.redaction_class,
       updated.snapshot_version,
       updated.observed_at AS occurred_at,
       updated.created_at
  FROM updated
 ORDER BY updated.id ASC;

-- name: ClaimRunLogIngestBatch :many
WITH candidates AS (
    SELECT telemetry_outbox.id,
           telemetry_outbox.size_bytes
      FROM telemetry_outbox
     WHERE telemetry_outbox.stream_kind = 'run_log'
       AND telemetry_outbox.written_at IS NULL
       AND telemetry_outbox.state IN ('pending', 'claimed', 'failed')
       AND (telemetry_outbox.next_retry_at IS NULL OR telemetry_outbox.next_retry_at <= now())
     ORDER BY telemetry_outbox.id ASC
     LIMIT sqlc.arg(row_limit)
     FOR UPDATE SKIP LOCKED
),
claimed AS (
    SELECT sized.id
      FROM (
          SELECT candidates.id,
                 SUM(candidates.size_bytes) OVER (ORDER BY candidates.id ASC) AS cumulative_size_bytes
            FROM candidates
      ) AS sized
     WHERE sized.cumulative_size_bytes <= sqlc.arg(max_batch_bytes)::bigint
),
updated AS (
    UPDATE telemetry_outbox
       SET state = 'claimed',
           retry_count = telemetry_outbox.retry_count + 1,
           next_retry_at = now() + sqlc.arg(lease_duration)::interval,
           updated_at = now()
      FROM claimed
     WHERE telemetry_outbox.id = claimed.id
    RETURNING telemetry_outbox.*
)
SELECT updated.id AS outbox_id,
       updated.retry_count,
       COALESCE(updated.idempotency_key, '')::text AS idempotency_key,
       updated.org_id,
       updated.project_id,
       updated.environment_id,
       updated.run_id,
       updated.run_lease_id,
       updated.attempt_number,
       updated.stream_name AS stream,
       updated.id AS seq,
       updated.observed_seq,
       updated.content,
       updated.size_bytes,
       updated.created_at
  FROM updated
 ORDER BY updated.id ASC;

-- name: ClaimLiveTelemetryOutbox :many
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
               AND earlier_outbox.org_id = telemetry_outbox.org_id
               AND earlier_outbox.source_kind = telemetry_outbox.source_kind
               AND earlier_outbox.source_id = telemetry_outbox.source_id
               AND earlier_outbox.stream_name = telemetry_outbox.stream_name
               AND earlier_outbox.id < telemetry_outbox.id
       )
     ORDER BY telemetry_outbox.id ASC
     LIMIT sqlc.arg(row_limit)
     FOR UPDATE SKIP LOCKED
),
updated AS (
    UPDATE telemetry_outbox
       SET publish_locked_until = now() + sqlc.arg(lease_duration)::interval,
           publish_attempts = telemetry_outbox.publish_attempts + 1,
           updated_at = now()
      FROM claimed
     WHERE telemetry_outbox.id = claimed.id
    RETURNING telemetry_outbox.*
)
SELECT updated.id AS outbox_id,
       updated.stream_kind,
       ('helmr:events:' || updated.org_id::text || ':' || updated.source_kind || ':' || updated.source_id::text)::text AS stream_key,
       updated.publish_attempts AS attempts,
       updated.id AS seq,
       updated.org_id,
       updated.project_id,
       updated.environment_id,
       updated.source_kind,
       updated.source_id,
       updated.stream_name,
       updated.run_id,
       updated.deployment_id,
       updated.run_lease_id,
       updated.attempt_number,
       updated.trace_id,
       updated.span_id,
       updated.parent_span_id,
       updated.traceparent,
       updated.category,
       updated.severity,
       updated.source,
       updated.kind,
       updated.message,
       updated.payload,
       updated.redaction_class,
       updated.snapshot_version,
       updated.observed_at AS occurred_at,
       updated.created_at
  FROM updated
 ORDER BY updated.id ASC;

-- name: MarkLiveTelemetryOutboxPublished :exec
UPDATE telemetry_outbox
   SET published_at = now(),
       publish_locked_until = NULL,
       updated_at = now(),
       publish_error = ''
 WHERE id = sqlc.arg(id);

-- name: MarkLiveTelemetryOutboxFailed :exec
UPDATE telemetry_outbox
   SET publish_locked_until = now() + sqlc.arg(retry_after)::interval,
       updated_at = now(),
       publish_error = sqlc.arg(publish_error)
 WHERE id = sqlc.arg(id)
   AND published_at IS NULL;

-- name: MarkTelemetryOutboxWritten :execrows
UPDATE telemetry_outbox
   SET state = 'written',
       written_at = now(),
       retry_count = 0,
       next_retry_at = NULL,
       updated_at = now(),
       ingest_error = ''
 WHERE id = ANY(sqlc.arg(ids)::bigint[])
   AND written_at IS NULL;

-- name: MarkTelemetryOutboxBatchFailed :execrows
UPDATE telemetry_outbox
   SET state = 'failed',
       next_retry_at = now() + sqlc.arg(retry_after)::interval,
       updated_at = now(),
       ingest_error = sqlc.arg(ingest_error)
 WHERE id = ANY(sqlc.arg(ids)::bigint[])
   AND written_at IS NULL;

-- name: PruneTelemetryOutboxWritten :execrows
WITH eligible AS (
    SELECT id
      FROM telemetry_outbox
     WHERE written_at < now() - sqlc.arg(retain_for)::interval
       AND (
            (stream_kind = 'event' AND published_at IS NOT NULL)
            OR stream_kind = 'run_log'
       )
     ORDER BY written_at ASC, id ASC
     LIMIT sqlc.arg(row_limit)
     FOR UPDATE SKIP LOCKED
)
DELETE FROM telemetry_outbox
 USING eligible
 WHERE telemetry_outbox.id = eligible.id;

-- name: GetTelemetryOutboxLifecycle :one
SELECT LEAST(
           (SELECT created_at
              FROM telemetry_outbox
             WHERE stream_kind = 'event'
               AND written_at IS NULL
               AND state IN ('pending', 'claimed', 'failed')
               AND (next_retry_at IS NULL OR next_retry_at <= now())
             ORDER BY id ASC LIMIT 1),
           (SELECT created_at
              FROM telemetry_outbox
             WHERE stream_kind = 'run_log'
               AND written_at IS NULL
               AND state IN ('pending', 'claimed', 'failed')
               AND (next_retry_at IS NULL OR next_retry_at <= now())
             ORDER BY id ASC LIMIT 1)
       )::timestamptz AS oldest_retry_created_at,
       (SELECT written_at
          FROM telemetry_outbox
         WHERE written_at < now() - sqlc.arg(retain_for)::interval
           AND ((stream_kind = 'event' AND published_at IS NOT NULL) OR stream_kind = 'run_log')
         ORDER BY written_at ASC, id ASC LIMIT 1) AS oldest_gc_written_at;
