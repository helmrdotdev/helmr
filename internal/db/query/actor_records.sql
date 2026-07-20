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
SELECT inserted_record.*, false::boolean AS claim_fingerprint_mismatch
  FROM inserted_record
UNION ALL
SELECT existing_record.*,
       (selected_claim.request_fingerprint <> sqlc.narg(expected_request_fingerprint))::boolean
           AS claim_fingerprint_mismatch
  FROM existing_record
  JOIN selected_claim ON selected_claim.id = existing_record.claim_id;

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
       no_progress_input_sequence = NULL,
       no_progress_count = 0,
       last_no_progress_run_id = NULL,
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
