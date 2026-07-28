-- name: LockLiveIdempotencyClaim :one
SELECT idempotency_claims.*,
       coalesce(idempotency_claims.expires_at <= transaction_timestamp(), false)::boolean AS expired
  FROM idempotency_claims
 WHERE idempotency_claims.environment_id = sqlc.arg(environment_id)
   AND idempotency_claims.operation = sqlc.arg(operation)
   AND idempotency_claims.slot_hash = sqlc.arg(slot_hash)
   AND idempotency_claims.retired_at IS NULL
 FOR UPDATE;

-- name: RetireExpiredIdempotencyClaims :many
WITH candidates AS (
    SELECT id
      FROM idempotency_claims
     WHERE retired_at IS NULL
       AND expires_at <= transaction_timestamp()
     ORDER BY expires_at, id
     LIMIT sqlc.arg(row_limit)
     FOR UPDATE SKIP LOCKED
)
UPDATE idempotency_claims
   SET retired_at = statement_timestamp()
  FROM candidates
 WHERE idempotency_claims.id = candidates.id
RETURNING idempotency_claims.*;

-- name: CollectRetiredIdempotencyClaims :many
WITH candidates AS (
    SELECT id
      FROM idempotency_claims
     WHERE retired_at IS NOT NULL
       AND NOT EXISTS (
           SELECT 1
             FROM runs
            WHERE runs.claim_id = idempotency_claims.id
       )
       AND NOT EXISTS (
           SELECT 1
             FROM actor_records
            WHERE actor_records.claim_id = idempotency_claims.id
       )
       AND NOT EXISTS (
           SELECT 1
             FROM run_waits
            WHERE run_waits.child_claim_id = idempotency_claims.id
       )
       AND NOT EXISTS (
           SELECT 1
             FROM workspace_processes
            WHERE workspace_processes.claim_id = idempotency_claims.id
       )
     ORDER BY retired_at, id
     LIMIT sqlc.arg(row_limit)
     FOR UPDATE SKIP LOCKED
)
DELETE FROM idempotency_claims
 USING candidates
 WHERE idempotency_claims.id = candidates.id
RETURNING idempotency_claims.*;

-- name: CreateIdempotencyClaim :one
INSERT INTO idempotency_claims (
    id,
    environment_id,
    operation,
    slot_hash,
    request_fingerprint,
    accepted_at,
    expires_at
)
VALUES (
    sqlc.arg(id),
    sqlc.arg(environment_id),
    sqlc.arg(operation),
    sqlc.arg(slot_hash),
    sqlc.arg(request_fingerprint),
    statement_timestamp(),
    CASE
        WHEN sqlc.arg(operation)::text = 'task.child.invoke' THEN NULL
        ELSE statement_timestamp() + interval '30 days'
    END
)
ON CONFLICT (environment_id, operation, slot_hash)
    WHERE retired_at IS NULL
DO NOTHING
RETURNING *;

-- name: GetIdempotencyClaim :one
SELECT *
  FROM idempotency_claims
 WHERE environment_id = sqlc.arg(environment_id)
   AND id = sqlc.arg(id);

-- name: CompleteIdempotencyClaim :one
UPDATE idempotency_claims
   SET state = 'completed',
       receipt = sqlc.arg(receipt),
       completed_at = now()
 WHERE environment_id = sqlc.arg(environment_id)
   AND id = sqlc.arg(id)
   AND request_fingerprint = sqlc.arg(request_fingerprint)
   AND state = 'pending'
   AND retired_at IS NULL
RETURNING *;

-- name: FailIdempotencyClaim :one
UPDATE idempotency_claims
   SET state = 'failed',
       receipt = sqlc.arg(receipt),
       completed_at = now()
 WHERE environment_id = sqlc.arg(environment_id)
   AND id = sqlc.arg(id)
   AND request_fingerprint = sqlc.arg(request_fingerprint)
   AND state = 'pending'
   AND retired_at IS NULL
RETURNING *;

-- name: RetireExpiredIdempotencyClaim :one
UPDATE idempotency_claims
   SET retired_at = now()
 WHERE environment_id = sqlc.arg(environment_id)
   AND id = sqlc.arg(id)
   AND retired_at IS NULL
   AND expires_at <= now()
RETURNING *;
