-- The owning operation holds Computer and Instance authority. No caller-provided
-- key selection is accepted. Provider I/O happens only after committing these pins.

-- name: GetInstanceComputerWriteKey :one
SELECT k.*
  FROM computer_instances r
  JOIN computers c ON c.environment_id=r.environment_id AND c.id=r.computer_id
  JOIN computer_data_keys k ON k.environment_id=c.environment_id AND k.computer_id=c.id
    AND k.id=COALESCE(r.write_key_id,c.write_key_id) AND k.available
 WHERE r.id=sqlc.arg(computer_instance_id) AND r.environment_id=sqlc.arg(environment_id)
   AND r.computer_id=sqlc.arg(computer_id) AND r.reclaimed_at IS NULL;

-- name: CreateComputerKey :one
INSERT INTO computer_data_keys(id,environment_id,computer_id,wrapping_key_id,wrapped_key)
VALUES(sqlc.arg(id),sqlc.arg(environment_id),sqlc.arg(computer_id),sqlc.arg(wrapping_key_id),sqlc.arg(wrapped_key))
RETURNING *;

-- name: InitializeComputerWriteKey :execrows
UPDATE computers SET write_key_id=sqlc.arg(key_id)
 WHERE environment_id=sqlc.arg(environment_id) AND id=sqlc.arg(computer_id) AND write_key_id IS NULL;

-- name: PinInstanceComputerKey :execrows
UPDATE computer_instances SET write_key_id=sqlc.arg(key_id)
 WHERE id=sqlc.arg(computer_instance_id) AND environment_id=sqlc.arg(environment_id)
   AND computer_id=sqlc.arg(computer_id) AND reclaimed_at IS NULL
   AND (write_key_id IS NULL OR write_key_id=sqlc.arg(key_id));

-- A key remains available while any object, current writer or unreclaimed
-- Instance needs it. Restrictive availability FKs arbitrate concurrent adoption.
-- name: ListUnreferencedComputerKeys :many
SELECT k.id FROM computer_data_keys k
WHERE k.available
 AND NOT EXISTS(SELECT 1 FROM computers c WHERE c.write_key_id=k.id)
 AND NOT EXISTS(SELECT 1 FROM computer_instances r WHERE r.retained_write_key_id=k.id)
 AND NOT EXISTS(SELECT 1 FROM computer_object_keys o WHERE o.key_id=k.id)
ORDER BY k.id LIMIT sqlc.arg(row_limit);

-- Erase wrapped material, preserving the irreversible key identity/audit row.
-- name: RetireUnreferencedComputerKey :execrows
UPDATE computer_data_keys k SET retired_at=clock_timestamp(), wrapped_key=NULL
WHERE k.id=sqlc.arg(id) AND k.available
 AND NOT EXISTS(SELECT 1 FROM computers c WHERE c.write_key_id=k.id)
 AND NOT EXISTS(SELECT 1 FROM computer_instances r WHERE r.retained_write_key_id=k.id)
 AND NOT EXISTS(SELECT 1 FROM computer_object_keys o WHERE o.key_id=k.id);
