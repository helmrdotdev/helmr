-- name: AppendRunStreamRecord :one
WITH valid_claim AS MATERIALIZED (
    SELECT id, state
      FROM idempotency_claims
     WHERE idempotency_claims.environment_id = sqlc.arg(environment_id)::uuid
       AND idempotency_claims.id = sqlc.narg(claim_id)
       AND idempotency_claims.operation = 'run_stream.append'
       AND idempotency_claims.request_fingerprint = sqlc.narg(request_fingerprint)
       AND idempotency_claims.retired_at IS NULL
     FOR UPDATE
), existing_record AS MATERIALIZED (
    SELECT run_stream_records.*
      FROM run_stream_records
      JOIN valid_claim ON valid_claim.id = run_stream_records.claim_id
     WHERE run_stream_records.run_stream_id = sqlc.arg(run_stream_id)
     FOR UPDATE
),
locked_stream AS (
    SELECT run_streams.*
      FROM run_streams
     WHERE run_streams.org_id = sqlc.arg(org_id)
       AND run_streams.project_id = sqlc.arg(project_id)
       AND run_streams.environment_id = sqlc.arg(environment_id)
       AND run_streams.run_id = sqlc.arg(producer_run_id)
       AND run_streams.id = sqlc.arg(run_stream_id)
       AND NOT EXISTS (SELECT 1 FROM existing_record)
       AND (
           sqlc.narg(claim_id)::uuid IS NULL
           OR EXISTS (
               SELECT 1
                 FROM valid_claim
                WHERE valid_claim.state = 'pending'
           )
       )
     FOR UPDATE
),
allocated_stream AS (
    UPDATE run_streams
       SET next_sequence = run_streams.next_sequence + 1,
           updated_at = now()
      FROM locked_stream
     WHERE run_streams.id = locked_stream.id
    RETURNING run_streams.*, run_streams.next_sequence - 1 AS allocated_sequence
),
inserted_record AS (
    INSERT INTO run_stream_records (
        id,
        public_id,
        org_id,
        project_id,
        environment_id,
        run_stream_id,
        producer_run_id,
        sequence,
        data,
        content_type,
        claim_id,
        producer_attempt_number
    )
    SELECT sqlc.arg(id),
           sqlc.arg(public_id),
           allocated_stream.org_id,
           allocated_stream.project_id,
           allocated_stream.environment_id,
           allocated_stream.id,
           allocated_stream.run_id,
           allocated_stream.allocated_sequence,
           sqlc.arg(data),
           coalesce(nullif(sqlc.arg(content_type)::text, ''), 'application/json'),
           sqlc.narg(claim_id),
           sqlc.arg(producer_attempt_number)
      FROM allocated_stream
    RETURNING run_stream_records.*
)
SELECT * FROM inserted_record
UNION ALL
SELECT * FROM existing_record;

-- name: GetRunStreamRecord :one
SELECT *
  FROM run_stream_records
 WHERE org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND run_stream_id = sqlc.arg(run_stream_id)
   AND id = sqlc.arg(id);

-- name: ListRunStreamRecords :many
SELECT *
  FROM run_stream_records
 WHERE org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND run_stream_id = sqlc.arg(run_stream_id)
   AND sequence > sqlc.arg(after_sequence)
 ORDER BY sequence, id
 LIMIT sqlc.arg(limit_count);
