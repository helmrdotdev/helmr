-- name: LockPlatformRetryKey :one
SELECT sqlc.embed(k),
    k.receipt_expires_at<=clock_timestamp() AND NOT EXISTS (
        SELECT 1 FROM computer_commands c WHERE c.environment_id=k.environment_id AND c.id=k.command_id
        AND (c.terminal_at IS NULL OR (c.computer_lease_epoch IS NOT NULL AND c.process_reconciled_at IS NULL))
    ) AS expired
FROM platform_retry_keys k
WHERE k.environment_id=sqlc.arg(environment_id) AND k.operation=sqlc.arg(operation) AND k.slot_hash=sqlc.arg(slot_hash)
FOR UPDATE OF k;

-- name: CreatePlatformRetryKey :one
INSERT INTO platform_retry_keys(environment_id,id,operation,slot_hash,request_fingerprint,
    scope_secret_name,scope_secret_id,scope_computer_id,scope_command_id,scope_caller_kind,scope_computer_key,
    accepted_at,receipt_expires_at)
VALUES(sqlc.arg(environment_id),sqlc.arg(id),sqlc.arg(operation),sqlc.arg(slot_hash),sqlc.arg(request_fingerprint),
    sqlc.narg(scope_secret_name),sqlc.narg(scope_secret_id),sqlc.narg(scope_computer_id),sqlc.narg(scope_command_id),sqlc.narg(scope_caller_kind),sqlc.narg(scope_computer_key),
    statement_timestamp(),statement_timestamp()+interval '720 hours')
ON CONFLICT(environment_id,operation,slot_hash) DO NOTHING RETURNING *;

-- name: CompletePlatformRetryKey :one
UPDATE platform_retry_keys SET deployment_id=sqlc.narg(deployment_id),secret_id=sqlc.narg(secret_id),secret_version_id=sqlc.narg(secret_version_id),
    computer_id=sqlc.narg(computer_id),command_id=sqlc.narg(command_id),receipt=sqlc.arg(receipt)
WHERE environment_id=sqlc.arg(environment_id) AND id=sqlc.arg(id) AND request_fingerprint=sqlc.arg(request_fingerprint)
    AND receipt IS NULL AND receipt_pruned_at IS NULL
RETURNING *;

-- name: GetPlatformRetryKey :one
SELECT * FROM platform_retry_keys WHERE environment_id=sqlc.arg(environment_id) AND id=sqlc.arg(id);

-- name: PruneExpiredPlatformRetryReceipts :execrows
WITH candidates AS MATERIALIZED (
    SELECT k.environment_id,k.id FROM platform_retry_keys k
    WHERE k.receipt IS NOT NULL AND k.receipt_pruned_at IS NULL AND k.receipt_expires_at<=statement_timestamp()
    AND NOT EXISTS(SELECT 1 FROM computer_commands c WHERE c.environment_id=k.environment_id AND c.id=k.command_id
        AND (c.terminal_at IS NULL OR (c.computer_lease_epoch IS NOT NULL AND c.process_reconciled_at IS NULL)))
    ORDER BY k.receipt_expires_at,k.id LIMIT sqlc.arg(row_limit) FOR UPDATE OF k SKIP LOCKED
)
UPDATE platform_retry_keys k SET receipt=NULL,receipt_pruned_at=statement_timestamp()
FROM candidates c WHERE k.environment_id=c.environment_id AND k.id=c.id;
