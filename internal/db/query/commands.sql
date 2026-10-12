-- name: CreateCommand :one
INSERT INTO commands (
    id, environment_id, computer_id, argv, cwd, env, stdin,
    timeout_ms, caller_kind, caller_id
) VALUES (
    sqlc.arg(id), sqlc.arg(environment_id), sqlc.arg(computer_id),
    sqlc.arg(argv), sqlc.narg(cwd), sqlc.arg(env), sqlc.arg(stdin), sqlc.arg(timeout_ms),
    sqlc.arg(caller_kind), sqlc.arg(caller_id)
) RETURNING *;

-- name: GetCommandByRetryKey :one
SELECT command.* FROM commands command
 JOIN idempotency_receipts k ON k.environment_id=command.environment_id AND k.command_id=command.id AND k.operation='computer.exec'
 WHERE k.environment_id=sqlc.arg(environment_id) AND k.id=sqlc.arg(retry_key_id);

-- name: GetCommand :one
SELECT command.* FROM commands command
 JOIN environments e ON e.id=command.environment_id
 WHERE e.org_id=sqlc.arg(org_id) AND e.project_id=sqlc.arg(project_id)
 AND command.environment_id=sqlc.arg(environment_id) AND command.id=sqlc.arg(command_id);

-- name: GetCommandTarget :one
SELECT command.environment_id, command.computer_id, e.org_id, e.project_id
 FROM commands command JOIN environments e ON e.id=command.environment_id
 WHERE command.id=sqlc.arg(command_id) AND command.environment_id=sqlc.arg(environment_id);

-- name: LockCommand :one
SELECT * FROM commands WHERE environment_id=sqlc.arg(environment_id)
 AND computer_id=sqlc.arg(computer_id) AND id=sqlc.arg(command_id) FOR UPDATE;

-- name: RequestCommandCancellation :one
UPDATE commands SET cancel_requested_at=COALESCE(cancel_requested_at,clock_timestamp()),
 status=CASE WHEN computer_lease_epoch IS NULL THEN 'cancelled' ELSE 'stopping' END,
 terminal_at=CASE WHEN computer_lease_epoch IS NULL THEN clock_timestamp() END,
 terminal_reason_code=CASE WHEN computer_lease_epoch IS NULL THEN 'computer_command_cancelled' END,
 result_expires_at=CASE WHEN computer_lease_epoch IS NULL THEN clock_timestamp()+interval '30 days' END,
 revision=revision+1,updated_at=clock_timestamp()
 WHERE id=sqlc.arg(command_id) AND environment_id=sqlc.arg(environment_id) AND terminal_at IS NULL
 RETURNING *;

-- name: PruneExpiredCommandResults :execrows
WITH expired AS (
 SELECT environment_id,id FROM commands WHERE terminal_at IS NOT NULL AND result_pruned_at IS NULL
 AND result_expires_at<=clock_timestamp()
 AND (computer_lease_epoch IS NULL OR process_reconciled_at IS NOT NULL)
 ORDER BY result_expires_at,id LIMIT sqlc.arg(row_limit) FOR UPDATE SKIP LOCKED
)
UPDATE commands c SET argv=NULL,cwd=NULL,env=NULL,stdin=NULL,error=NULL,
 result_pruned_at=clock_timestamp(),updated_at=clock_timestamp()
 FROM expired WHERE (c.environment_id,c.id)=(expired.environment_id,expired.id);

-- name: GetCommandLogState :one
SELECT command.* FROM commands command
 JOIN environments e ON e.id=command.environment_id
 WHERE e.org_id=sqlc.arg(org_id) AND command.environment_id=sqlc.arg(environment_id)
 AND command.id=sqlc.arg(command_id);
