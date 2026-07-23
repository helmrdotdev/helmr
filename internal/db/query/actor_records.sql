-- name: LockActorInputClaim :one
SELECT *
  FROM idempotency_claims
 WHERE environment_id = sqlc.arg(environment_id)
   AND id = sqlc.arg(id)
   AND operation = 'actor.input.send'
   AND retired_at IS NULL
 FOR UPDATE;

-- name: CreateActorStartInputRecord :one
WITH locked_actor AS MATERIALIZED (
    SELECT actors.*
      FROM actors
     WHERE actors.environment_id = sqlc.arg(environment_id)
       AND actors.id = sqlc.arg(actor_id)
       AND actors.state = 'open'
       AND actors.next_input_sequence = 1
       AND actors.committed_input_sequence = 0
       AND (
           sqlc.narg(claim_id)::uuid IS NULL
           OR EXISTS (
               SELECT 1
                 FROM idempotency_claims
                WHERE idempotency_claims.environment_id = actors.environment_id
                  AND idempotency_claims.id = sqlc.narg(claim_id)
                  AND idempotency_claims.operation = 'actor.start'
                  AND idempotency_claims.state = 'pending'
                  AND idempotency_claims.retired_at IS NULL
           )
       )
     FOR UPDATE OF actors
), advanced AS (
    UPDATE actors
       SET next_input_sequence = 2,
           updated_at = now()
      FROM locked_actor
     WHERE actors.id = locked_actor.id
    RETURNING actors.*
)
INSERT INTO actor_records (
    id,
    environment_id,
    actor_id,
    direction,
    sequence,
    data,
    content_type,
    source_kind,
    claim_id
)
SELECT sqlc.arg(id),
       advanced.environment_id,
       advanced.id,
       'input',
       1,
       sqlc.arg(data),
       'application/json',
       'external',
       sqlc.narg(claim_id)
  FROM advanced
RETURNING *;

-- name: AppendActorInputRecord :one
WITH selected_claim AS MATERIALIZED (
    SELECT id, state, request_fingerprint
      FROM idempotency_claims
     WHERE idempotency_claims.environment_id = sqlc.arg(environment_id)::uuid
       AND idempotency_claims.id = sqlc.narg(claim_id)
       AND idempotency_claims.operation = 'actor.input.send'
       AND idempotency_claims.retired_at IS NULL
     FOR UPDATE
), existing_record AS MATERIALIZED (
    SELECT actor_records.*
      FROM actor_records
      JOIN selected_claim ON selected_claim.id = actor_records.claim_id
     WHERE actor_records.actor_id = sqlc.arg(actor_id)
       AND actor_records.direction = 'input'
), locked_actor AS MATERIALIZED (
    SELECT actors.*
      FROM actors
     WHERE actors.environment_id = sqlc.arg(environment_id)
       AND actors.id = sqlc.arg(actor_id)
       AND actors.state = 'open'
       AND actors.next_input_sequence <= 9007199254740991
       AND NOT EXISTS (SELECT 1 FROM existing_record)
       AND (
           sqlc.narg(claim_id)::uuid IS NULL
           OR EXISTS (
               SELECT 1
                 FROM selected_claim
                WHERE selected_claim.state = 'pending'
                  AND request_fingerprint = sqlc.narg(expected_request_fingerprint)
           )
       )
     FOR UPDATE
), allocated AS (
    UPDATE actors
       SET next_input_sequence = actors.next_input_sequence + 1,
           manual_run_cancelled = false,
           updated_at = now()
      FROM locked_actor
     WHERE actors.id = locked_actor.id
    RETURNING actors.*, actors.next_input_sequence - 1 AS allocated_sequence
), inserted_record AS (
    INSERT INTO actor_records (
        id,
        environment_id,
        actor_id,
        direction,
        sequence,
        data,
        content_type,
        source_kind,
        source_run_id,
        claim_id
    )
    SELECT sqlc.arg(id),
           allocated.environment_id,
           allocated.id,
           'input',
           allocated.allocated_sequence,
           sqlc.arg(data),
           'application/json',
           sqlc.arg(source_kind),
           sqlc.narg(source_run_id),
           sqlc.narg(claim_id)
      FROM allocated
    RETURNING actor_records.*
)
SELECT inserted_record.*, false::boolean AS claim_fingerprint_mismatch, true::boolean AS appended
  FROM inserted_record
UNION ALL
SELECT existing_record.*,
       (selected_claim.request_fingerprint <> sqlc.narg(expected_request_fingerprint))::boolean
           AS claim_fingerprint_mismatch,
       false::boolean AS appended
  FROM existing_record
  JOIN selected_claim ON selected_claim.id = existing_record.claim_id;

-- name: CompleteActorInputClaim :one
UPDATE idempotency_claims
   SET state = 'completed',
       receipt = jsonb_build_object(
           'recordId', actor_records.id::text,
           'sequence', actor_records.sequence
       ),
       completed_at = transaction_timestamp()
  FROM actor_records
 WHERE idempotency_claims.environment_id = sqlc.arg(environment_id)::uuid
   AND idempotency_claims.id = sqlc.arg(claim_id)
   AND idempotency_claims.operation = 'actor.input.send'
   AND idempotency_claims.request_fingerprint = sqlc.arg(request_fingerprint)
   AND idempotency_claims.state = 'pending'
   AND idempotency_claims.retired_at IS NULL
   AND actor_records.environment_id = idempotency_claims.environment_id
   AND actor_records.actor_id = sqlc.arg(actor_id)
   AND actor_records.id = sqlc.arg(record_id)
   AND actor_records.direction = 'input'
   AND actor_records.claim_id = idempotency_claims.id
RETURNING idempotency_claims.*;

-- name: GetActorInputCurrentRun :one
SELECT runs.*
  FROM runs
 WHERE runs.environment_id = sqlc.arg(environment_id)
   AND runs.id = sqlc.arg(run_id)
   AND runs.actor_id = sqlc.arg(actor_id);

-- name: AppendActorOutputRecord :one
WITH locked_actor AS MATERIALIZED (
    SELECT actors.*
      FROM actors
      JOIN runs
        ON runs.actor_id = actors.id
       AND runs.id = sqlc.arg(producer_run_id)
       AND runs.workspace_id = actors.workspace_id
      JOIN run_attempts
        ON run_attempts.run_id = runs.id
       AND run_attempts.number = sqlc.arg(producer_attempt_number)
       AND run_attempts.workspace_id = actors.workspace_id
     WHERE actors.environment_id = sqlc.arg(environment_id)
       AND actors.id = sqlc.arg(actor_id)
       AND actors.current_run_id = runs.id
       AND actors.state = 'open'
       AND actors.next_output_sequence <= 9007199254740991
     FOR UPDATE OF actors
), allocated AS (
    UPDATE actors
       SET next_output_sequence = actors.next_output_sequence + 1,
           updated_at = now()
      FROM locked_actor
     WHERE actors.id = locked_actor.id
    RETURNING actors.*, actors.next_output_sequence - 1 AS allocated_sequence
)
INSERT INTO actor_records (
    id,
    environment_id,
    actor_id,
    direction,
    sequence,
    data,
    content_type,
    producer_run_id,
    producer_attempt_number
)
SELECT sqlc.arg(id),
       allocated.environment_id,
       allocated.id,
       'output',
       allocated.allocated_sequence,
       sqlc.arg(data),
       coalesce(nullif(sqlc.arg(content_type)::text, ''), 'application/json'),
       sqlc.arg(producer_run_id),
       sqlc.arg(producer_attempt_number)
  FROM allocated
RETURNING *;

-- name: ListActorInputRecords :many
SELECT *
  FROM actor_records
 WHERE actor_id = sqlc.arg(actor_id)
   AND direction = 'input'
   AND sequence > sqlc.arg(after_sequence)
 ORDER BY sequence, id
 LIMIT sqlc.arg(limit_count);

-- name: ListActorOutputRecords :many
SELECT *
  FROM actor_records
 WHERE actor_id = sqlc.arg(actor_id)
   AND direction = 'output'
   AND sequence > sqlc.arg(after_sequence)
 ORDER BY sequence, id
 LIMIT sqlc.arg(limit_count);

-- name: CommitActorInputCursor :one
UPDATE actors
   SET committed_input_sequence = sqlc.arg(committed_input_sequence),
       state_version = state_version + 1,
       updated_at = now()
 WHERE environment_id = sqlc.arg(environment_id)
   AND id = sqlc.arg(actor_id)
   AND current_run_id = sqlc.arg(run_id)
   AND run_generation = sqlc.arg(expected_run_generation)
   AND committed_input_sequence < sqlc.arg(committed_input_sequence)
   AND sqlc.arg(committed_input_sequence) < next_input_sequence
RETURNING *;

-- name: GetActorWakeupRecord :one
SELECT actor_records.*
  FROM actors
  JOIN actor_records
    ON actor_records.actor_id = actors.id
   AND actor_records.direction = 'input'
   AND actor_records.sequence > actors.committed_input_sequence
 WHERE actors.environment_id = sqlc.arg(environment_id)
   AND actors.id = sqlc.arg(actor_id)
   AND actors.state = 'open'
 ORDER BY actor_records.sequence, actor_records.id
LIMIT 1;

-- name: GetActorInputRecordAtSequenceForUpdate :one
SELECT *
  FROM actor_records
 WHERE environment_id = sqlc.arg(environment_id)
   AND actor_id = sqlc.arg(actor_id)
   AND direction = 'input'
   AND sequence = sqlc.arg(sequence)
 FOR UPDATE;

-- name: GetActorInputRecordByIDForUpdate :one
SELECT *
  FROM actor_records
 WHERE environment_id = sqlc.arg(environment_id)
   AND actor_id = sqlc.arg(actor_id)
   AND id = sqlc.arg(id)
   AND direction = 'input'
 FOR UPDATE;

-- name: CreateActorInputReconcileOutbox :exec
INSERT INTO outbox_messages (id, lane, topic, partition_key, payload, available_at)
VALUES (
    sqlc.arg(id),
    'control',
    'actor.input.reconcile',
    sqlc.arg(actor_id)::uuid::text,
    jsonb_build_object(
        'environmentId', sqlc.arg(environment_id)::uuid::text,
        'actorId', sqlc.arg(actor_id)::uuid::text,
        'recordId', sqlc.arg(record_id)::uuid::text
    ),
    transaction_timestamp()
)
ON CONFLICT (id) DO NOTHING;

-- name: LockActorForInputReconcile :one
SELECT *
  FROM actors
 WHERE environment_id = sqlc.arg(environment_id)
   AND id = sqlc.arg(actor_id)
 FOR UPDATE;

-- name: LockActorInputCurrentRun :one
SELECT runs.*
  FROM runs
 WHERE runs.environment_id = sqlc.arg(environment_id)
   AND runs.id = sqlc.arg(run_id)
   AND runs.actor_id = sqlc.arg(actor_id)
 FOR UPDATE;
