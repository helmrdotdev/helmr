-- The owning operation holds Computer and Runtime authority. No caller-provided
-- key selection is accepted. Provider I/O happens only after committing these pins.

-- name: GetInitialComputerWriteKey :one
SELECT k.*
  FROM runtime_instances r
  JOIN workspaces c ON c.environment_id=r.environment_id AND c.id=r.workspace_id
  JOIN computer_data_keys k ON k.environment_id=c.environment_id AND k.computer_id=c.id
    AND k.id=COALESCE(r.computer_write_key_id,c.write_key_id) AND k.available
 WHERE r.id=sqlc.arg(runtime_instance_id) AND r.environment_id=sqlc.arg(environment_id)
   AND r.workspace_id=sqlc.arg(computer_id) AND r.reclaimed_at IS NULL;

-- name: CreateComputerKey :one
INSERT INTO computer_data_keys(id,environment_id,computer_id,wrapping_key_id,wrapped_key)
VALUES(sqlc.arg(id),sqlc.arg(environment_id),sqlc.arg(computer_id),sqlc.arg(wrapping_key_id),sqlc.arg(wrapped_key))
RETURNING *;

-- name: InitializeComputerWriteKey :execrows
UPDATE workspaces SET write_key_id=sqlc.arg(key_id)
 WHERE environment_id=sqlc.arg(environment_id) AND id=sqlc.arg(computer_id) AND write_key_id IS NULL;

-- name: PinRuntimeComputerKey :execrows
UPDATE runtime_instances SET computer_write_key_id=sqlc.arg(key_id)
 WHERE id=sqlc.arg(runtime_instance_id) AND environment_id=sqlc.arg(environment_id)
   AND workspace_id=sqlc.arg(computer_id) AND reclaimed_at IS NULL
   AND (computer_write_key_id IS NULL OR computer_write_key_id=sqlc.arg(key_id));
