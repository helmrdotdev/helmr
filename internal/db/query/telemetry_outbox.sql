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
       updated.source_kind::event_subject_type AS subject_type,
       updated.source_id AS subject_id,
       updated.id AS seq,
       updated.org_id,
       updated.worker_group_id,
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
       updated.worker_group_id,
       updated.project_id,
       updated.environment_id,
       updated.run_id,
       updated.run_lease_id,
       updated.attempt_number,
       updated.stream_name::run_log_stream AS stream,
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
       meter_events.worker_group_id,
       meter_events.project_id,
       meter_events.environment_id,
       meter_events.source_type,
       meter_events.source_id,
       meter_events.run_id,
       meter_events.attempt_number,
       meter_events.trace_id,
       meter_events.span_id,
       meter_events.meter,
       meter_events.quantity,
       meter_events.unit,
       meter_events.measured_to,
       meter_events.details,
       meter_events.occurred_at,
       meter_events.created_at
  FROM updated
  JOIN meter_events ON meter_events.org_id = updated.org_id
                   AND meter_events.source_type = updated.source_kind
                   AND meter_events.source_id = updated.source_id
                   AND meter_events.meter = updated.kind
                   AND meter_events.idempotency_key = updated.idempotency_key
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
       updated.worker_group_id,
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

-- name: DeadLetterOrphanedTelemetryOutbox :many
SELECT telemetry_outbox.id
  FROM telemetry_outbox
 WHERE false
 LIMIT sqlc.arg(row_limit);

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
INSERT INTO telemetry_replay_errors (org_id, worker_group_id, stream_kind, source_kind, source_id, stream_name, seq, state, retry_count, last_error, next_retry_at)
SELECT org_id,
       worker_group_id,
       stream_kind,
       source_kind,
       source_id,
       stream_name,
       id,
       'dead_lettered',
       retry_count,
       last_error,
       NULL
  FROM dead_lettered;

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
