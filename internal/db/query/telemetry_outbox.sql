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

-- name: ClaimMeterEventIngestBatch :many
WITH claimed AS (
    SELECT telemetry_outbox.id
      FROM telemetry_outbox
     WHERE telemetry_outbox.stream_kind = 'meter_event'
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
       COALESCE(updated.idempotency_key, '')::text AS idempotency_key,
       meter_events.org_id,
       meter_events.project_id,
       meter_events.environment_id,
       meter_events.source_type,
       meter_events.source_id,
       meter_events.run_id,
       meter_events.deployment_id,
       meter_events.attempt_number,
       meter_events.trace_id,
       meter_events.span_id,
       meter_events.meter,
       meter_events.quantity,
       meter_events.unit,
       meter_events.measured_from,
       meter_events.measured_to,
       meter_events.details,
       meter_events.idempotency_fingerprint,
       meter_events.occurred_at,
       meter_events.created_at
  FROM updated
  JOIN meter_events ON meter_events.id = updated.meter_event_id
 ORDER BY updated.id ASC;

-- name: ClaimWorkspaceProcessTerminalOutputIngestBatch :many
WITH claimed AS (
    SELECT telemetry_outbox.id
      FROM telemetry_outbox
     WHERE telemetry_outbox.stream_kind = 'terminal_output'
       AND telemetry_outbox.source_kind = 'workspace_process'
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
       COALESCE(updated.idempotency_key, '')::text AS idempotency_key,
       updated.org_id,
       updated.project_id,
       updated.environment_id,
       updated.workspace_id,
       updated.resource_kind,
       updated.resource_id,
       updated.stream_name,
       COALESCE(updated.offset_start, 0)::bigint AS offset_start,
       updated.offset_end,
       updated.content AS data,
       updated.observed_at
  FROM updated
 ORDER BY updated.id ASC;

-- name: ClaimLiveTelemetryOutbox :many
WITH claimed AS (
    SELECT telemetry_outbox.id
      FROM telemetry_outbox
     WHERE telemetry_outbox.stream_kind IN ('event', 'run_log', 'terminal_output')
       AND telemetry_outbox.published_at IS NULL
       AND (telemetry_outbox.publish_locked_until IS NULL OR telemetry_outbox.publish_locked_until < now())
       AND telemetry_outbox.state <> 'dead_lettered'
       AND NOT EXISTS (
            SELECT 1
              FROM telemetry_outbox AS earlier_outbox
             WHERE earlier_outbox.stream_kind = telemetry_outbox.stream_kind
               AND earlier_outbox.published_at IS NULL
               AND earlier_outbox.state <> 'dead_lettered'
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
           updated_at = now(),
           last_error = ''
      FROM claimed
     WHERE telemetry_outbox.id = claimed.id
    RETURNING telemetry_outbox.*
)
SELECT updated.id AS outbox_id,
       updated.stream_kind,
       CASE updated.stream_kind
           WHEN 'event' THEN
               ('helmr:events:' || updated.org_id::text || ':' || updated.source_kind || ':' || updated.source_id::text)::text
           WHEN 'run_log' THEN
               ('helmr:run_logs:' || updated.org_id::text || ':' || updated.run_id::text)::text
           WHEN 'terminal_output' THEN
               ('helmr:terminal_outputs:' || updated.org_id::text || ':' || updated.workspace_id::text || ':' || updated.resource_kind || ':' || updated.resource_id::text || ':' || updated.stream_name)::text
           ELSE ''
       END AS stream_key,
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
       updated.workspace_id,
       updated.resource_kind,
       updated.resource_id,
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
       COALESCE(updated.content, ''::bytea) AS content,
       COALESCE(updated.size_bytes, 0)::bigint AS size_bytes,
       COALESCE(updated.observed_seq, 0)::bigint AS observed_seq,
       COALESCE(updated.offset_start, 0)::bigint AS offset_start,
       COALESCE(updated.offset_end, 0)::bigint AS offset_end,
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
       last_error = ''
 WHERE id = sqlc.arg(id);

-- name: MarkLiveTelemetryOutboxFailed :exec
UPDATE telemetry_outbox
   SET publish_locked_until = now() + sqlc.arg(retry_after)::interval,
       updated_at = now(),
       last_error = sqlc.arg(last_error)
 WHERE id = sqlc.arg(id)
   AND published_at IS NULL;

-- name: HasUnpublishedLiveTelemetryOutbox :one
SELECT EXISTS (
    SELECT 1
      FROM telemetry_outbox
     WHERE telemetry_outbox.org_id = sqlc.arg(org_id)
       AND telemetry_outbox.stream_kind = sqlc.arg(stream_kind)
       AND telemetry_outbox.source_kind = sqlc.arg(source_kind)
       AND telemetry_outbox.source_id = sqlc.arg(source_id)
       AND telemetry_outbox.stream_name = sqlc.arg(stream_name)
       AND (telemetry_outbox.published_at IS NULL OR telemetry_outbox.written_at IS NULL)
       AND telemetry_outbox.state <> 'dead_lettered'
);

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

-- name: PruneTelemetryOutboxWritten :many
DELETE FROM telemetry_outbox
 WHERE (
        (
            written_at IS NOT NULL
            AND (stream_kind NOT IN ('event', 'run_log', 'terminal_output') OR published_at IS NOT NULL)
            AND written_at < now() - sqlc.arg(retain_for)::interval
        )
        OR (
            state = 'dead_lettered'
            AND updated_at < now() - sqlc.arg(retain_for)::interval
        )
 )
RETURNING id;
