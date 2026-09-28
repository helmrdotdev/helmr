-- name: LockIdempotencyClaim :one
SELECT * FROM idempotency_claims
 WHERE environment_id = sqlc.arg(environment_id)
   AND operation = sqlc.arg(operation)
   AND slot_hash = sqlc.arg(slot_hash)
 FOR UPDATE;

-- name: CreateIdempotencyClaim :one
INSERT INTO idempotency_claims (
    id,
    environment_id,
    operation,
    slot_hash,
    request_fingerprint,
    accepted_at,
    receipt_expires_at
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
DO NOTHING
RETURNING *;

-- name: PruneExpiredIdempotencyReceipts :execrows
WITH candidates AS MATERIALIZED (
    SELECT claims.id
      FROM idempotency_claims AS claims
     WHERE claims.status <> 'pending'
       AND claims.receipt_pruned_at IS NULL
       AND claims.receipt_expires_at <= statement_timestamp()
       AND NOT EXISTS (
           SELECT 1
             FROM computer_commands AS exec
             WHERE exec.environment_id = claims.environment_id
              AND exec.claim_id = claims.id
              AND (exec.terminal_at IS NULL
                   OR (exec.computer_instance_id IS NOT NULL AND exec.process_reconciled_at IS NULL))
       )
     ORDER BY claims.receipt_expires_at, claims.id
     LIMIT sqlc.arg(row_limit)
     FOR UPDATE OF claims SKIP LOCKED
)
UPDATE idempotency_claims AS claims
   SET receipt = NULL,
       receipt_pruned_at = statement_timestamp()
  FROM candidates
 WHERE claims.id = candidates.id;

-- name: GetIdempotencyClaim :one
SELECT *
  FROM idempotency_claims
 WHERE environment_id = sqlc.arg(environment_id)
   AND id = sqlc.arg(id);

-- name: CompleteIdempotencyClaim :one
UPDATE idempotency_claims
   SET status = 'completed',
       receipt = sqlc.arg(receipt),
       completed_at = now()
 WHERE environment_id = sqlc.arg(environment_id)
   AND id = sqlc.arg(id)
   AND request_fingerprint = sqlc.arg(request_fingerprint)
   AND status = 'pending'
RETURNING *;

-- name: FailIdempotencyClaim :one
UPDATE idempotency_claims
   SET status = 'failed',
       receipt = sqlc.arg(receipt),
       completed_at = now()
 WHERE environment_id = sqlc.arg(environment_id)
   AND id = sqlc.arg(id)
   AND request_fingerprint = sqlc.arg(request_fingerprint)
   AND status = 'pending'
RETURNING *;
