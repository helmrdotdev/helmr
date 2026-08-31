-- name: CreateActorStartInputRecord :one
WITH locked_actor AS MATERIALIZED (
    SELECT sessions.*
      FROM sessions
     WHERE sessions.environment_id = sqlc.arg(environment_id)
       AND sessions.id = sqlc.arg(session_id)
       AND sessions.state = 'open'
       AND sessions.next_input_sequence = 1
       AND sessions.committed_input_sequence = 0
       AND (
           sqlc.narg(claim_id)::uuid IS NULL
           OR EXISTS (
               SELECT 1
                 FROM idempotency_claims
                WHERE idempotency_claims.environment_id = sessions.environment_id
                  AND idempotency_claims.id = sqlc.narg(claim_id)
                  AND idempotency_claims.operation = 'actor.start'
                  AND idempotency_claims.state = 'pending'
                  AND idempotency_claims.retired_at IS NULL
           )
       )
     FOR UPDATE OF sessions
), advanced AS (
    UPDATE sessions
       SET next_input_sequence = 2,
           updated_at = now()
      FROM locked_actor
     WHERE sessions.id = locked_actor.id
    RETURNING sessions.*
)
INSERT INTO session_records (
    id,
    environment_id,
    session_id,
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
       AND idempotency_claims.operation = 'session.input.send'
       AND idempotency_claims.retired_at IS NULL
     FOR UPDATE
), existing_record AS MATERIALIZED (
    SELECT session_records.*
      FROM session_records
      JOIN selected_claim ON selected_claim.id = session_records.claim_id
     WHERE session_records.session_id = sqlc.arg(session_id)
       AND session_records.direction = 'input'
), locked_actor AS MATERIALIZED (
    SELECT sessions.*
      FROM sessions
     WHERE sessions.environment_id = sqlc.arg(environment_id)
       AND sessions.id = sqlc.arg(session_id)
       AND sessions.state = 'open'
       AND sessions.next_input_sequence <= 9007199254740991
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
    UPDATE sessions
       SET next_input_sequence = sessions.next_input_sequence + 1,
           manual_run_cancelled = false,
           updated_at = now()
      FROM locked_actor
     WHERE sessions.id = locked_actor.id
    RETURNING sessions.*, sessions.next_input_sequence - 1 AS allocated_sequence
), inserted_record AS (
    INSERT INTO session_records (
        id,
        environment_id,
        session_id,
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
    RETURNING session_records.*
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
	       'session_record_id', session_records.id::text,
           'sequence', session_records.sequence
       ),
       completed_at = transaction_timestamp()
  FROM session_records
 WHERE idempotency_claims.environment_id = sqlc.arg(environment_id)::uuid
   AND idempotency_claims.id = sqlc.arg(claim_id)
   AND idempotency_claims.operation = 'session.input.send'
   AND idempotency_claims.request_fingerprint = sqlc.arg(request_fingerprint)
   AND idempotency_claims.state = 'pending'
   AND idempotency_claims.retired_at IS NULL
   AND session_records.environment_id = idempotency_claims.environment_id
   AND session_records.session_id = sqlc.arg(session_id)
   AND session_records.id = sqlc.arg(record_id)
   AND session_records.direction = 'input'
   AND session_records.claim_id = idempotency_claims.id
RETURNING idempotency_claims.*;

-- name: GetActorInputCurrentRun :one
SELECT runs.*
  FROM runs
 WHERE runs.environment_id = sqlc.arg(environment_id)
   AND runs.id = sqlc.arg(run_id)
   AND runs.session_id = sqlc.arg(session_id);

-- name: AppendActorOutputRecord :one
WITH selected_claim AS MATERIALIZED (
    SELECT id, state, request_fingerprint
      FROM idempotency_claims
     WHERE idempotency_claims.environment_id = sqlc.arg(environment_id)::uuid
       AND idempotency_claims.id = sqlc.narg(claim_id)
       AND idempotency_claims.operation = 'session.output.append'
       AND idempotency_claims.retired_at IS NULL
     FOR UPDATE
), existing_record AS MATERIALIZED (
    SELECT session_records.*
      FROM session_records
      JOIN selected_claim ON selected_claim.id = session_records.claim_id
     WHERE session_records.session_id = sqlc.arg(session_id)
       AND session_records.direction = 'output'
), locked_actor AS MATERIALIZED (
    SELECT sessions.*
      FROM sessions
      JOIN runs
        ON runs.session_id = sessions.id
       AND runs.id = sqlc.arg(producer_run_id)
       AND runs.workspace_id = sessions.workspace_id
      JOIN run_attempts
        ON run_attempts.run_id = runs.id
       AND run_attempts.number = sqlc.arg(producer_attempt_number)
       AND run_attempts.workspace_id = sessions.workspace_id
     WHERE sessions.environment_id = sqlc.arg(environment_id)
       AND sessions.id = sqlc.arg(session_id)
       AND sessions.current_run_id = runs.id
       AND sessions.state IN ('open', 'closing')
       AND sessions.next_output_sequence <= 9007199254740991
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
     FOR UPDATE OF sessions
), allocated AS (
    UPDATE sessions
       SET next_output_sequence = sessions.next_output_sequence + 1,
           updated_at = now()
      FROM locked_actor
     WHERE sessions.id = locked_actor.id
    RETURNING sessions.*, sessions.next_output_sequence - 1 AS allocated_sequence
), inserted_record AS (
    INSERT INTO session_records (
        id,
        environment_id,
        session_id,
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
    RETURNING session_records.*
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
	       'session_record_id', session_records.id::text,
           'sequence', session_records.sequence
       ),
       completed_at = transaction_timestamp()
  FROM session_records
 WHERE idempotency_claims.environment_id = sqlc.arg(environment_id)::uuid
   AND idempotency_claims.id = sqlc.arg(claim_id)
   AND idempotency_claims.operation = 'session.output.append'
   AND idempotency_claims.request_fingerprint = sqlc.arg(request_fingerprint)
   AND idempotency_claims.state = 'pending'
   AND idempotency_claims.retired_at IS NULL
   AND session_records.environment_id = idempotency_claims.environment_id
   AND session_records.session_id = sqlc.arg(session_id)
   AND session_records.id = sqlc.arg(record_id)
   AND session_records.direction = 'output'
   AND session_records.claim_id = idempotency_claims.id
RETURNING idempotency_claims.*;

-- name: GetActorOutputRecordByID :one
SELECT *
  FROM session_records
 WHERE environment_id = sqlc.arg(environment_id)
   AND session_id = sqlc.arg(session_id)
   AND id = sqlc.arg(id)
   AND direction = 'output';

-- name: ReadPublicActorOutputPage :many
WITH scoped_actor AS MATERIALIZED (
    SELECT sessions.id,
           sessions.output_retention_floor,
           sessions.next_output_sequence,
           CASE
               WHEN sqlc.arg(after_present)::boolean
               THEN sqlc.arg(after_sequence)::bigint
               ELSE sessions.output_retention_floor - 1
           END::bigint AS effective_after
     FROM sessions
     WHERE sessions.environment_id = sqlc.arg(environment_id)
       AND sessions.id = sqlc.arg(session_id)
)
SELECT scoped_actor.id AS session_id,
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
      SELECT session_records.id AS record_id,
             session_records.sequence,
             session_records.data,
             session_records.content_type,
             session_records.created_at,
             runs.id AS run_id,
             session_records.producer_attempt_number,
             deployments.id AS deployment_id
        FROM session_records
        JOIN runs
          ON runs.session_id = session_records.session_id
         AND runs.id = session_records.producer_run_id
        JOIN deployments
          ON deployments.environment_id = runs.environment_id
         AND deployments.id = runs.deployment_id
       WHERE session_records.session_id = scoped_actor.id
         AND session_records.direction = 'output'
         AND session_records.sequence > scoped_actor.effective_after
         AND session_records.sequence < scoped_actor.next_output_sequence
       ORDER BY session_records.sequence, session_records.id
       LIMIT sqlc.arg(limit_count)::integer
  ) AS page ON true
 ORDER BY page.sequence NULLS LAST, page.record_id NULLS LAST;

-- name: GetActorInputRecordAtSequenceForUpdate :one
SELECT *
  FROM session_records
 WHERE environment_id = sqlc.arg(environment_id)
   AND session_id = sqlc.arg(session_id)
   AND direction = 'input'
   AND sequence = sqlc.arg(sequence)
 FOR UPDATE;

-- name: GetActorInputRecordByIDForUpdate :one
SELECT *
  FROM session_records
 WHERE environment_id = sqlc.arg(environment_id)
   AND session_id = sqlc.arg(session_id)
   AND id = sqlc.arg(id)
   AND direction = 'input'
 FOR UPDATE;

-- name: CreateActorInputReconcileOutbox :exec
INSERT INTO control_outbox (id, topic, payload, available_at)
VALUES (
    sqlc.arg(id),
    'session.input.reconcile',
    jsonb_build_object(
        'environmentId', sqlc.arg(environment_id)::uuid::text,
        'sessionId', sqlc.arg(session_id)::uuid::text,
        'recordId', sqlc.arg(record_id)::uuid::text
    ),
    transaction_timestamp()
)
ON CONFLICT (id) DO NOTHING;

-- name: LockActorForInputReconcile :one
SELECT *
  FROM sessions
 WHERE environment_id = sqlc.arg(environment_id)
   AND id = sqlc.arg(session_id)
 FOR UPDATE;

-- name: LockActorInputCurrentRun :one
SELECT runs.*
  FROM runs
 WHERE runs.environment_id = sqlc.arg(environment_id)
   AND runs.id = sqlc.arg(run_id)
   AND runs.session_id = sqlc.arg(session_id)
 FOR UPDATE;
