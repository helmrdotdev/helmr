-- name: ClaimEventIngestBatch :many
WITH candidates AS (
    SELECT telemetry_outbox.id,
           telemetry_outbox.ingest_size_bytes AS size_bytes
      FROM telemetry_outbox
     WHERE telemetry_outbox.stream_kind = 'event'
       AND telemetry_outbox.written_at IS NULL
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
       SET retry_count = telemetry_outbox.retry_count + 1,
           next_retry_at = now() + sqlc.arg(lease_duration)::interval,
           updated_at = now()
      FROM claimed
     WHERE telemetry_outbox.id = claimed.id
    RETURNING telemetry_outbox.*
)
SELECT updated.id AS outbox_id,
       updated.retry_count,
       updated.source_kind AS subject_type,
       updated.source_id AS subject_id,
       updated.id AS seq,
       environments.org_id,
       environments.project_id,
       updated.environment_id,
       updated.deployment_id,
       updated.category,
       updated.severity,
       updated.source,
       updated.kind,
       updated.message,
       updated.payload,
       updated.redaction_class,
       updated.observed_at AS occurred_at,
       updated.created_at
  FROM updated JOIN environments ON environments.id=updated.environment_id
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
               AND earlier_outbox.environment_id = telemetry_outbox.environment_id
               AND earlier_outbox.source_kind = telemetry_outbox.source_kind
               AND earlier_outbox.source_id = telemetry_outbox.source_id
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
       ('helmr:events:' || environments.org_id::text || ':' || updated.source_kind || ':' || updated.source_id::text)::text AS stream_key,
       updated.publish_attempts AS attempts,
       updated.id AS seq,
       environments.org_id,
       environments.project_id,
       updated.environment_id,
       updated.source_kind,
       updated.source_id,
       updated.deployment_id,
       updated.category,
       updated.severity,
       updated.source,
       updated.kind,
       updated.message,
       updated.payload,
       updated.redaction_class,
       updated.observed_at AS occurred_at,
       updated.created_at
  FROM updated JOIN environments ON environments.id=updated.environment_id
 ORDER BY updated.id ASC;

-- name: MarkLiveTelemetryOutboxBatchPublished :execrows
WITH completions AS (
    SELECT input_ids.id,
           input_attempts.publish_attempts
      FROM unnest(sqlc.arg(ids)::bigint[])
           WITH ORDINALITY AS input_ids(id, position)
      JOIN unnest(sqlc.arg(expected_publish_attempts)::integer[])
           WITH ORDINALITY AS input_attempts(publish_attempts, position)
        ON input_attempts.position = input_ids.position
)
UPDATE telemetry_outbox
   SET published_at = now(),
       publish_locked_until = NULL,
       updated_at = now(),
       publish_error = ''
  FROM completions
 WHERE telemetry_outbox.id = completions.id
   AND telemetry_outbox.publish_attempts = completions.publish_attempts
   AND telemetry_outbox.published_at IS NULL AND telemetry_outbox.stream_kind='event';

-- name: MarkLiveTelemetryOutboxBatchFailed :execrows
WITH failures AS (
    SELECT input_ids.id,
           input_attempts.publish_attempts,
           input_retries.retry_after,
           input_errors.publish_error
      FROM unnest(sqlc.arg(ids)::bigint[])
           WITH ORDINALITY AS input_ids(id, position)
      JOIN unnest(sqlc.arg(expected_publish_attempts)::integer[])
           WITH ORDINALITY AS input_attempts(publish_attempts, position)
        ON input_attempts.position = input_ids.position
      JOIN unnest(sqlc.arg(retry_afters)::interval[])
           WITH ORDINALITY AS input_retries(retry_after, position)
        ON input_retries.position = input_ids.position
      JOIN unnest(sqlc.arg(publish_errors)::text[])
           WITH ORDINALITY AS input_errors(publish_error, position)
        ON input_errors.position = input_ids.position
)
UPDATE telemetry_outbox
   SET publish_locked_until = now() + failures.retry_after,
       updated_at = now(),
       publish_error = failures.publish_error
  FROM failures
 WHERE telemetry_outbox.id = failures.id
   AND telemetry_outbox.publish_attempts = failures.publish_attempts
   AND telemetry_outbox.published_at IS NULL AND telemetry_outbox.stream_kind='event';

-- name: MarkTelemetryOutboxWritten :execrows
WITH completions AS (
    SELECT input_ids.id,
           input_retries.retry_count
      FROM unnest(sqlc.arg(ids)::bigint[])
           WITH ORDINALITY AS input_ids(id, position)
      JOIN unnest(sqlc.arg(expected_retry_counts)::integer[])
           WITH ORDINALITY AS input_retries(retry_count, position)
        ON input_retries.position = input_ids.position
)
UPDATE telemetry_outbox
   SET written_at = now(),
       retry_count = 0,
       next_retry_at = NULL,
       updated_at = now(),
       ingest_error = ''
  FROM completions
 WHERE telemetry_outbox.id = completions.id
   AND telemetry_outbox.retry_count = completions.retry_count
   AND telemetry_outbox.written_at IS NULL AND telemetry_outbox.stream_kind='event';

-- name: MarkTelemetryOutboxBatchFailed :execrows
WITH failures AS (
    SELECT input_ids.id,
           input_retries.retry_count
      FROM unnest(sqlc.arg(ids)::bigint[])
           WITH ORDINALITY AS input_ids(id, position)
      JOIN unnest(sqlc.arg(expected_retry_counts)::integer[])
           WITH ORDINALITY AS input_retries(retry_count, position)
        ON input_retries.position = input_ids.position
)
UPDATE telemetry_outbox
   SET next_retry_at = now() + sqlc.arg(retry_after)::interval,
       updated_at = now(),
       ingest_error = sqlc.arg(ingest_error)
  FROM failures
 WHERE telemetry_outbox.id = failures.id
   AND telemetry_outbox.retry_count = failures.retry_count
   AND telemetry_outbox.written_at IS NULL AND telemetry_outbox.stream_kind='event';

-- name: PruneTelemetryOutboxWritten :execrows
WITH eligible AS (
 SELECT id FROM telemetry_outbox WHERE stream_kind='event' AND published_at IS NOT NULL
 AND written_at<now()-sqlc.arg(retain_for)::interval
 ORDER BY written_at,id LIMIT sqlc.arg(row_limit) FOR UPDATE SKIP LOCKED
) DELETE FROM telemetry_outbox USING eligible WHERE telemetry_outbox.id=eligible.id;

-- name: GetTelemetryOutboxLifecycle :one
SELECT (SELECT created_at FROM telemetry_outbox WHERE stream_kind='event' AND written_at IS NULL
 AND (next_retry_at IS NULL OR next_retry_at<=now()) ORDER BY id LIMIT 1)::timestamptz AS oldest_retry_created_at,
 (SELECT written_at FROM telemetry_outbox WHERE stream_kind='event' AND published_at IS NOT NULL
 AND written_at<now()-sqlc.arg(retain_for)::interval ORDER BY written_at,id LIMIT 1)::timestamptz AS oldest_gc_written_at;
