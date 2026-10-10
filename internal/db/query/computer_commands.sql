-- name: CreateComputerCommand :one
INSERT INTO computer_commands (
    id, environment_id, computer_id, argv, cwd, env, stdin,
    timeout_ms, created_by_subject_type, created_by_subject_id
) VALUES (
    sqlc.arg(id), sqlc.arg(environment_id), sqlc.arg(computer_id),
    sqlc.arg(argv), sqlc.narg(cwd), sqlc.arg(env), sqlc.arg(stdin), sqlc.arg(timeout_ms),
    sqlc.arg(created_by_subject_type), sqlc.arg(created_by_subject_id)
) RETURNING *;

-- name: GetComputerCommandByRetryKey :one
SELECT command.* FROM computer_commands command
 JOIN platform_retry_keys k ON k.environment_id=command.environment_id AND k.command_id=command.id AND k.operation='computer.exec'
 WHERE k.environment_id=sqlc.arg(environment_id) AND k.id=sqlc.arg(retry_key_id);

-- name: GetCommand :one
SELECT command.* FROM computer_commands command
 JOIN environments e ON e.id=command.environment_id
 WHERE e.org_id=sqlc.arg(org_id) AND e.project_id=sqlc.arg(project_id)
 AND command.environment_id=sqlc.arg(environment_id) AND command.id=sqlc.arg(command_id);

-- name: GetComputerCommandTarget :one
SELECT command.environment_id, command.computer_id, e.org_id, e.project_id
 FROM computer_commands command JOIN environments e ON e.id=command.environment_id
 WHERE command.id=sqlc.arg(command_id) AND command.environment_id=sqlc.arg(environment_id);

-- name: LockComputerCommand :one
SELECT * FROM computer_commands WHERE environment_id=sqlc.arg(environment_id)
 AND computer_id=sqlc.arg(computer_id) AND id=sqlc.arg(command_id) FOR UPDATE;

-- name: RequestComputerCommandCancellation :one
UPDATE computer_commands SET cancel_requested_at=COALESCE(cancel_requested_at,clock_timestamp()),
 status=CASE WHEN computer_lease_epoch IS NULL THEN 'cancelled' ELSE 'stopping' END,
 terminal_at=CASE WHEN computer_lease_epoch IS NULL THEN clock_timestamp() END,
 terminal_reason_code=CASE WHEN computer_lease_epoch IS NULL THEN 'computer_command_cancelled' END,
 result_expires_at=CASE WHEN computer_lease_epoch IS NULL THEN clock_timestamp()+interval '30 days' END,
 revision=revision+1,updated_at=clock_timestamp()
 WHERE id=sqlc.arg(command_id) AND environment_id=sqlc.arg(environment_id) AND terminal_at IS NULL
 RETURNING *;

-- name: PruneExpiredComputerCommandResults :execrows
WITH expired AS (
 SELECT environment_id,id FROM computer_commands WHERE terminal_at IS NOT NULL AND result_pruned_at IS NULL
 AND result_expires_at<=clock_timestamp()
 AND (computer_lease_epoch IS NULL OR process_reconciled_at IS NOT NULL)
 ORDER BY result_expires_at,id LIMIT sqlc.arg(row_limit) FOR UPDATE SKIP LOCKED
)
UPDATE computer_commands c SET argv=NULL,cwd=NULL,env=NULL,stdin=NULL,error=NULL,
 result_pruned_at=clock_timestamp(),updated_at=clock_timestamp()
 FROM expired WHERE (c.environment_id,c.id)=(expired.environment_id,expired.id);

-- name: GetCommandLogState :one
SELECT command.* FROM computer_commands command
 JOIN environments e ON e.id=command.environment_id
 WHERE e.org_id=sqlc.arg(org_id) AND command.environment_id=sqlc.arg(environment_id)
 AND command.id=sqlc.arg(command_id);
