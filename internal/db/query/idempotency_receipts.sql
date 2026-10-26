-- name: LockIdempotencyReceipt :one
SELECT sqlc.embed(k),
    k.accepted_at<=clock_timestamp()-interval '720 hours' AND NOT EXISTS (
        SELECT 1 FROM commands c WHERE c.environment_id=k.environment_id AND c.id=k.command_id
        AND (c.terminal_at IS NULL OR (c.computer_lease_epoch IS NOT NULL AND c.process_reconciled_at IS NULL))
    ) AS expired
FROM idempotency_receipts k
WHERE k.environment_id=sqlc.arg(environment_id) AND k.operation=sqlc.arg(operation) AND k.slot_hash=sqlc.arg(slot_hash)
FOR UPDATE OF k;

-- name: CreateIdempotencyReceipt :one
INSERT INTO idempotency_receipts(environment_id,id,operation,slot_hash,request_fingerprint,
    scope_secret_name,scope_secret_id,scope_computer_id,scope_command_id,scope_computer_key,
    deployment_id,secret_id,secret_version_id,computer_id,command_id,receipt,
    accepted_at)
VALUES(sqlc.arg(environment_id),sqlc.arg(id),sqlc.arg(operation),sqlc.arg(slot_hash),sqlc.arg(request_fingerprint),
    sqlc.narg(scope_secret_name),sqlc.narg(scope_secret_id),sqlc.narg(scope_computer_id),sqlc.narg(scope_command_id),sqlc.narg(scope_computer_key),
    sqlc.narg(deployment_id),sqlc.narg(secret_id),sqlc.narg(secret_version_id),sqlc.narg(computer_id),sqlc.narg(command_id),sqlc.arg(receipt),
    statement_timestamp())
RETURNING *;

-- name: PruneExpiredIdempotencyReceipts :execrows
WITH candidates AS MATERIALIZED (
    SELECT k.environment_id,k.id FROM idempotency_receipts k
    WHERE k.receipt IS NOT NULL AND k.receipt_pruned_at IS NULL AND k.accepted_at<=statement_timestamp()-interval '720 hours'
    AND NOT EXISTS(SELECT 1 FROM commands c WHERE c.environment_id=k.environment_id AND c.id=k.command_id
        AND (c.terminal_at IS NULL OR (c.computer_lease_epoch IS NOT NULL AND c.process_reconciled_at IS NULL)))
    ORDER BY k.accepted_at,k.id LIMIT sqlc.arg(row_limit) FOR UPDATE OF k SKIP LOCKED
)
UPDATE idempotency_receipts k SET receipt=NULL,receipt_pruned_at=statement_timestamp()
FROM candidates c WHERE k.environment_id=c.environment_id AND k.id=c.id;
