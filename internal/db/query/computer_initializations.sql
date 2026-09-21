-- These are transaction primitives. The publication owner must lock and validate
-- current preparation authority before registration, and publish the version in
-- the same transaction as consumption. A registration/replay is not an execution
-- or upload grant. No remote deletion is authorized by these queries.
-- A registration conflict (no row) is resolved with GetComputerInitialization:
-- mismatched, consumed and abandoned candidates must not be registered anew.
-- A digest already owned by another runtime raises a unique violation; replacement
-- runtimes produce their own candidate rather than transfer cleanup ownership.
-- Consumption can also raise a unique violation if another initialization won
-- for this Computer. The owner must roll back before resolving that receipt.

-- name: RegisterComputerInitialization :one
INSERT INTO computer_initializations (
    id, environment_id, computer_id, version_id, runtime_instance_id,
    runtime_desired_version, ownership_generation, writer_generation,
    digest, size_bytes, logical_bytes, media_type, initial_config
) VALUES (
    sqlc.arg(id), sqlc.arg(environment_id), sqlc.arg(computer_id), sqlc.arg(version_id),
    sqlc.arg(runtime_instance_id), sqlc.arg(runtime_desired_version),
    sqlc.arg(ownership_generation), sqlc.arg(writer_generation),
    sqlc.arg(digest), sqlc.arg(size_bytes), sqlc.arg(logical_bytes), sqlc.arg(media_type), sqlc.arg(initial_config)
)
ON CONFLICT (runtime_instance_id) DO UPDATE
    SET runtime_instance_id = computer_initializations.runtime_instance_id
  WHERE computer_initializations.status = 'registered'
    AND computer_initializations.environment_id = EXCLUDED.environment_id
    AND computer_initializations.computer_id = EXCLUDED.computer_id
    AND computer_initializations.version_id = EXCLUDED.version_id
    AND computer_initializations.runtime_desired_version = EXCLUDED.runtime_desired_version
    AND computer_initializations.ownership_generation = EXCLUDED.ownership_generation
    AND computer_initializations.writer_generation = EXCLUDED.writer_generation
    AND computer_initializations.digest = EXCLUDED.digest
    AND computer_initializations.size_bytes = EXCLUDED.size_bytes
    AND computer_initializations.logical_bytes = EXCLUDED.logical_bytes
    AND computer_initializations.media_type = EXCLUDED.media_type
    AND computer_initializations.initial_config = EXCLUDED.initial_config
RETURNING *;

-- name: GetComputerInitialization :one
SELECT * FROM computer_initializations
 WHERE environment_id = sqlc.arg(environment_id)
   AND computer_id = sqlc.arg(computer_id)
   AND runtime_instance_id = sqlc.arg(runtime_instance_id);

-- name: ConsumeComputerInitialization :one
UPDATE computer_initializations AS initialization
   SET status = 'consumed', artifact_id = artifacts.id,
       consumed_at = COALESCE(initialization.consumed_at, clock_timestamp())
  FROM artifacts
 WHERE initialization.id = sqlc.arg(id)
   AND initialization.environment_id = sqlc.arg(environment_id)
   AND initialization.computer_id = sqlc.arg(computer_id)
   AND artifacts.environment_id = initialization.environment_id
   AND artifacts.id = sqlc.arg(artifact_id)
   AND artifacts.kind = 'workspace_version'
   AND artifacts.digest = initialization.digest
   AND artifacts.size_bytes = initialization.size_bytes
   AND artifacts.media_type = initialization.media_type
   AND (initialization.status = 'registered'
        OR (initialization.status = 'consumed' AND initialization.artifact_id = artifacts.id))
RETURNING initialization.*;

-- name: AbandonComputerInitialization :one
UPDATE computer_initializations
   SET status = 'abandoned', abandoned_at = COALESCE(abandoned_at, clock_timestamp())
 WHERE id = sqlc.arg(id)
   AND environment_id = sqlc.arg(environment_id)
   AND computer_id = sqlc.arg(computer_id)
   AND status IN ('registered', 'abandoned')
RETURNING *;
