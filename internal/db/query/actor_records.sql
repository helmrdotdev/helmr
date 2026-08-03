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
WITH selected_claim AS MATERIALIZED (
    SELECT id, state, request_fingerprint
      FROM idempotency_claims
     WHERE idempotency_claims.environment_id = sqlc.arg(environment_id)::uuid
       AND idempotency_claims.id = sqlc.narg(claim_id)
       AND idempotency_claims.operation = 'actor.output.append'
       AND idempotency_claims.retired_at IS NULL
     FOR UPDATE
), existing_record AS MATERIALIZED (
    SELECT actor_records.*
      FROM actor_records
      JOIN selected_claim ON selected_claim.id = actor_records.claim_id
     WHERE actor_records.actor_id = sqlc.arg(actor_id)
       AND actor_records.direction = 'output'
), locked_actor AS MATERIALIZED (
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
       AND actors.state IN ('open', 'closing')
       AND actors.next_output_sequence <= 9007199254740991
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
     FOR UPDATE OF actors
), allocated AS (
    UPDATE actors
       SET next_output_sequence = actors.next_output_sequence + 1,
           updated_at = now()
      FROM locked_actor
     WHERE actors.id = locked_actor.id
    RETURNING actors.*, actors.next_output_sequence - 1 AS allocated_sequence
), inserted_record AS (
    INSERT INTO actor_records (
        id,
        environment_id,
        actor_id,
        direction,
        sequence,
        data,
        content_type,
        producer_run_id,
        producer_attempt_number,
        claim_id
    )
    SELECT sqlc.arg(id),
           allocated.environment_id,
           allocated.id,
           'output',
           allocated.allocated_sequence,
           sqlc.arg(data),
           sqlc.arg(content_type),
           sqlc.arg(producer_run_id),
           sqlc.arg(producer_attempt_number),
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

-- name: CompleteActorOutputClaim :one
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
   AND idempotency_claims.operation = 'actor.output.append'
   AND idempotency_claims.request_fingerprint = sqlc.arg(request_fingerprint)
   AND idempotency_claims.state = 'pending'
   AND idempotency_claims.retired_at IS NULL
   AND actor_records.environment_id = idempotency_claims.environment_id
   AND actor_records.actor_id = sqlc.arg(actor_id)
   AND actor_records.id = sqlc.arg(record_id)
   AND actor_records.direction = 'output'
   AND actor_records.claim_id = idempotency_claims.id
RETURNING idempotency_claims.*;

-- name: GetActorOutputRecordByID :one
SELECT *
  FROM actor_records
 WHERE environment_id = sqlc.arg(environment_id)
   AND actor_id = sqlc.arg(actor_id)
   AND id = sqlc.arg(id)
   AND direction = 'output';

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

-- name: ReadPublicActorOutputPage :many
WITH scoped_actor AS MATERIALIZED (
    SELECT actors.id,
           actors.output_retention_floor,
           actors.next_output_sequence,
           CASE
               WHEN sqlc.arg(after_present)::boolean
               THEN sqlc.arg(after_sequence)::bigint
               ELSE actors.output_retention_floor - 1
           END::bigint AS effective_after
      FROM actors
     WHERE actors.environment_id = sqlc.arg(environment_id)
       AND actors.actor_declared_id = sqlc.arg(actor_declared_id)
       AND (
           (
               sqlc.narg(address_id)::uuid IS NOT NULL
               AND actors.id = sqlc.narg(address_id)::uuid
           )
           OR
           (
               sqlc.narg(address_key)::text IS NOT NULL
               AND actors.key = sqlc.narg(address_key)::text
           )
       )
)
SELECT scoped_actor.id AS actor_id,
       scoped_actor.output_retention_floor,
       scoped_actor.next_output_sequence,
       scoped_actor.effective_after::bigint AS effective_after,
       page.record_id,
       coalesce(page.sequence, 0)::bigint AS sequence,
       coalesce(page.data, 'null'::jsonb)::jsonb AS data,
       coalesce(page.content_type, '')::text AS content_type,
       coalesce(page.created_at, 'epoch'::timestamptz)::timestamptz AS created_at,
       page.run_id,
       coalesce(page.producer_attempt_number, 0)::integer AS producer_attempt_number,
       page.deployment_id
  FROM scoped_actor
  LEFT JOIN LATERAL (
      SELECT actor_records.id AS record_id,
             actor_records.sequence,
             actor_records.data,
             actor_records.content_type,
             actor_records.created_at,
             runs.id AS run_id,
             actor_records.producer_attempt_number,
             deployments.id AS deployment_id
        FROM actor_records
        JOIN runs
          ON runs.actor_id = actor_records.actor_id
         AND runs.id = actor_records.producer_run_id
        JOIN deployments
          ON deployments.environment_id = runs.environment_id
         AND deployments.id = runs.deployment_id
       WHERE actor_records.actor_id = scoped_actor.id
         AND actor_records.direction = 'output'
         AND actor_records.sequence > scoped_actor.effective_after
         AND actor_records.sequence < scoped_actor.next_output_sequence
       ORDER BY actor_records.sequence, actor_records.id
       LIMIT sqlc.arg(limit_count)::integer
  ) AS page ON true
 ORDER BY page.sequence NULLS LAST, page.record_id NULLS LAST;

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
