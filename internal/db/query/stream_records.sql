-- name: GetStreamRecordByIdempotencyKey :one
SELECT *
 FROM stream_records
 WHERE org_id = sqlc.arg(org_id)
   AND stream_id = sqlc.arg(stream_id)
   AND idempotency_key = sqlc.arg(idempotency_key)
   AND sqlc.arg(idempotency_key)::text <> '';

-- name: GetStreamRecord :one
SELECT *
 FROM stream_records
 WHERE org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND id = sqlc.arg(id);

-- name: AppendStreamRecord :one
WITH existing_record AS MATERIALIZED (
    SELECT stream_records.*
     FROM stream_records
     WHERE stream_records.org_id = sqlc.arg(org_id)
       AND stream_records.project_id = sqlc.arg(project_id)
       AND stream_records.environment_id = sqlc.arg(environment_id)
       AND stream_records.stream_id = sqlc.arg(stream_id)
       AND stream_records.idempotency_key = sqlc.arg(idempotency_key)
       AND sqlc.arg(idempotency_key)::text <> ''
     FOR UPDATE
),
locked_stream AS (
    SELECT streams.*
     FROM streams
     WHERE streams.org_id = sqlc.arg(org_id)
       AND streams.project_id = sqlc.arg(project_id)
       AND streams.environment_id = sqlc.arg(environment_id)
       AND streams.id = sqlc.arg(stream_id)
       AND streams.direction = sqlc.arg(direction)::stream_direction
       AND NOT EXISTS (SELECT 1 FROM existing_record)
     FOR UPDATE
),
allocated_stream AS (
    UPDATE streams
       SET next_sequence = streams.next_sequence + 1
      FROM locked_stream
     WHERE streams.org_id = locked_stream.org_id
       AND streams.id = locked_stream.id
    RETURNING streams.*, streams.next_sequence - 1 AS allocated_sequence
),
inserted_record AS (
    INSERT INTO stream_records (
        id,
        public_id,
        org_id,
        worker_group_id,
        project_id,
        environment_id,
        session_id,
        stream_id,
        direction,
        sequence,
        data,
        correlation_id,
        content_type,
        idempotency_key,
        idempotency_fingerprint,
        source_type,
        source_id,
        public_access_token_id
    )
    SELECT sqlc.arg(id),
           sqlc.arg(public_id),
           allocated_stream.org_id,
           allocated_stream.worker_group_id,
           allocated_stream.project_id,
           allocated_stream.environment_id,
           allocated_stream.session_id,
           allocated_stream.id,
           allocated_stream.direction,
           allocated_stream.allocated_sequence,
           COALESCE(sqlc.arg(data)::jsonb, 'null'::jsonb),
           COALESCE(sqlc.arg(correlation_id)::text, ''),
           COALESCE(NULLIF(sqlc.arg(content_type)::text, ''), 'application/json'),
           COALESCE(sqlc.arg(idempotency_key)::text, ''),
           COALESCE(sqlc.arg(idempotency_fingerprint)::text, ''),
           sqlc.arg(source_type)::stream_record_source_type,
           COALESCE(sqlc.arg(source_id)::text, ''),
           sqlc.narg(public_access_token_id)::uuid
      FROM allocated_stream
    RETURNING stream_records.*
),
selected_record AS (
    SELECT inserted_record.*, false::boolean AS is_cached
      FROM inserted_record
    UNION ALL
    SELECT existing_record.*, true::boolean AS is_cached
      FROM existing_record
)
SELECT selected_record.*,
       (
           selected_record.is_cached
           AND selected_record.idempotency_fingerprint <> COALESCE(sqlc.arg(idempotency_fingerprint)::text, '')
       )::boolean AS idempotency_fingerprint_mismatch
  FROM selected_record;

-- name: ListStreamRecords :many
SELECT stream_records.*
  FROM stream_records
  JOIN streams ON streams.org_id = stream_records.org_id
              AND streams.id = stream_records.stream_id
 WHERE stream_records.org_id = sqlc.arg(org_id)
   AND stream_records.project_id = sqlc.arg(project_id)
   AND stream_records.environment_id = sqlc.arg(environment_id)
   AND stream_records.stream_id = sqlc.arg(stream_id)
   AND streams.direction = sqlc.arg(direction)::stream_direction
   AND stream_records.sequence > sqlc.arg(after_sequence)::bigint
   AND (
       COALESCE(sqlc.narg(correlation_id)::text, '') = ''
       OR stream_records.correlation_id = COALESCE(sqlc.narg(correlation_id)::text, '')
   )
 ORDER BY stream_records.sequence ASC, stream_records.id ASC
 LIMIT sqlc.arg(limit_count);

-- name: ResolveStreamWaitsForStream :many
WITH candidate_raw AS (
    SELECT waits.id AS wait_id,
           waits.org_id,
           waits.project_id,
           waits.environment_id,
           run_waits.id AS run_wait_id,
           run_waits.worker_group_id,
           run_waits.run_id,
           waits.stream_id,
           waits.created_at,
           next_record.id AS record_id,
           next_record.sequence,
           next_record.data
      FROM waits
      JOIN run_waits ON run_waits.org_id = waits.org_id
                    AND run_waits.wait_id = waits.id
      JOIN LATERAL (
          SELECT stream_records.*
           FROM stream_records
           WHERE stream_records.org_id = waits.org_id
             AND stream_records.stream_id = waits.stream_id
             AND stream_records.sequence > COALESCE(waits.stream_sequence, 0)
             AND (
                 waits.correlation_key = ''
                 OR stream_records.correlation_id = waits.correlation_key
             )
           ORDER BY stream_records.sequence ASC, stream_records.id ASC
           LIMIT 1
      ) next_record ON true
     WHERE waits.org_id = sqlc.arg(org_id)
       AND run_waits.worker_group_id = sqlc.arg(worker_group_id)
       AND waits.project_id = sqlc.arg(project_id)
       AND waits.environment_id = sqlc.arg(environment_id)
       AND waits.stream_id = sqlc.arg(stream_id)
       AND waits.kind = 'stream'
       AND waits.state = 'pending'
       AND run_waits.state IN ('hot_waiting', 'checkpointed_waiting')
     ORDER BY waits.created_at ASC, waits.id ASC
     FOR UPDATE OF waits, run_waits
),
matched_wait AS (
    UPDATE waits
       SET stream_record_id = candidate_raw.record_id,
           stream_sequence = candidate_raw.sequence,
           result = jsonb_build_object(
               'stream', streams.name,
               'sequence', candidate_raw.sequence,
               'data', candidate_raw.data
           ),
           state = 'completed',
           completed_at = COALESCE(waits.completed_at, now()),
           updated_at = now()
      FROM candidate_raw
      JOIN streams ON streams.org_id = candidate_raw.org_id
                  AND streams.id = candidate_raw.stream_id
     WHERE waits.org_id = candidate_raw.org_id
       AND waits.id = candidate_raw.wait_id
       AND waits.state = 'pending'
    RETURNING waits.id AS wait_id,
              candidate_raw.run_wait_id,
              waits.org_id,
              candidate_raw.worker_group_id,
              waits.project_id,
              waits.environment_id,
              candidate_raw.run_id,
              waits.stream_id,
              candidate_raw.record_id,
              candidate_raw.sequence,
              candidate_raw.data
),
resolved_wait AS (
    UPDATE run_waits
       SET state = 'resuming',
           resuming_at = COALESCE(run_waits.resuming_at, now()),
           updated_at = now()
      FROM matched_wait
     WHERE run_waits.org_id = matched_wait.org_id
       AND run_waits.worker_group_id = matched_wait.worker_group_id
       AND run_waits.id = matched_wait.run_wait_id
       AND run_waits.state IN ('hot_waiting', 'checkpointed_waiting')
    RETURNING run_waits.*
)
SELECT resolved_wait.id AS run_wait_id,
       resolved_wait.org_id,
       resolved_wait.worker_group_id,
       resolved_wait.project_id,
       resolved_wait.environment_id,
       resolved_wait.run_id,
       matched_wait.stream_id,
       matched_wait.record_id,
       matched_wait.sequence,
       matched_wait.data
  FROM resolved_wait
  JOIN matched_wait ON matched_wait.org_id = resolved_wait.org_id
                   AND matched_wait.worker_group_id = resolved_wait.worker_group_id
                   AND matched_wait.run_wait_id = resolved_wait.id;

-- name: ResolveStreamWaitForRunWait :one
WITH candidate_raw AS (
    SELECT waits.id AS wait_id,
           waits.org_id,
           waits.project_id,
           waits.environment_id,
           run_waits.id AS run_wait_id,
           run_waits.worker_group_id,
           run_waits.run_id,
           waits.stream_id,
           next_record.id AS record_id,
           next_record.sequence,
           next_record.data
      FROM waits
      JOIN run_waits ON run_waits.org_id = waits.org_id
                    AND run_waits.wait_id = waits.id
      JOIN LATERAL (
          SELECT stream_records.*
           FROM stream_records
           WHERE stream_records.org_id = waits.org_id
             AND stream_records.stream_id = waits.stream_id
             AND stream_records.sequence > COALESCE(waits.stream_sequence, 0)
             AND (
                 waits.correlation_key = ''
                 OR stream_records.correlation_id = waits.correlation_key
             )
           ORDER BY stream_records.sequence ASC, stream_records.id ASC
           LIMIT 1
      ) next_record ON true
     WHERE waits.org_id = sqlc.arg(org_id)
       AND run_waits.worker_group_id = sqlc.arg(worker_group_id)
       AND waits.project_id = sqlc.arg(project_id)
       AND waits.environment_id = sqlc.arg(environment_id)
       AND run_waits.id = sqlc.arg(run_wait_id)
       AND waits.kind = 'stream'
       AND waits.state = 'pending'
       AND run_waits.state IN ('hot_waiting', 'checkpointed_waiting')
     FOR UPDATE OF waits, run_waits
),
matched_wait AS (
    UPDATE waits
       SET stream_record_id = candidate_raw.record_id,
           stream_sequence = candidate_raw.sequence,
           result = jsonb_build_object(
               'stream', streams.name,
               'sequence', candidate_raw.sequence,
               'data', candidate_raw.data
           ),
           state = 'completed',
           completed_at = COALESCE(waits.completed_at, now()),
           updated_at = now()
      FROM candidate_raw
      JOIN streams ON streams.org_id = candidate_raw.org_id
                  AND streams.id = candidate_raw.stream_id
     WHERE waits.org_id = candidate_raw.org_id
       AND waits.id = candidate_raw.wait_id
       AND waits.state = 'pending'
    RETURNING waits.id AS wait_id,
              candidate_raw.run_wait_id,
              waits.org_id,
              candidate_raw.worker_group_id,
              waits.project_id,
              waits.environment_id,
              candidate_raw.run_id,
              waits.stream_id,
              candidate_raw.record_id,
              candidate_raw.sequence,
              candidate_raw.data
),
resolved_wait AS (
    UPDATE run_waits
       SET state = 'resuming',
           resuming_at = COALESCE(run_waits.resuming_at, now()),
           updated_at = now()
      FROM matched_wait
     WHERE run_waits.org_id = matched_wait.org_id
       AND run_waits.worker_group_id = matched_wait.worker_group_id
       AND run_waits.id = matched_wait.run_wait_id
       AND run_waits.state IN ('hot_waiting', 'checkpointed_waiting')
    RETURNING run_waits.*
)
SELECT resolved_wait.id AS run_wait_id,
       resolved_wait.org_id,
       resolved_wait.worker_group_id,
       resolved_wait.project_id,
       resolved_wait.environment_id,
       resolved_wait.run_id,
       matched_wait.stream_id,
       matched_wait.record_id,
       matched_wait.sequence,
       matched_wait.data
  FROM resolved_wait
  JOIN matched_wait ON matched_wait.org_id = resolved_wait.org_id
                   AND matched_wait.worker_group_id = resolved_wait.worker_group_id
                   AND matched_wait.run_wait_id = resolved_wait.id;
