-- These are transaction primitives. The publication owner must lock and validate
-- current preparation authority before registration and publication. Publication
-- commits the root and Computer-owned boot configuration and consumes its candidate
-- atomically. A receipt is not an execution or upload grant. No remote deletion is authorized by these queries.
-- A registration conflict (no row) is resolved with GetComputerInitialization:
-- mismatched, consumed and abandoned candidates must not be registered anew.
-- A digest already owned by another runtime raises a unique violation; replacement
-- runtimes produce their own candidate rather than transfer cleanup ownership.
-- A publisher that loses the root transition receives no row. Resolve the
-- winning receipt separately; never rewrite the committed root or its references.

-- name: RegisterComputerInitialization :one
WITH lifetime AS (
    INSERT INTO cas_object_lifetimes (digest) VALUES (sqlc.arg(digest))
    ON CONFLICT (digest) DO NOTHING
)
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

-- The caller owns current preparation authority and has verified the object.
-- Root publication, Computer configuration and candidate consumption are one
-- statement: none can be committed alone. Historical receipt retrieval uses GetComputerInitialization;
-- this mutation never reopens a consumed candidate or grants further execution.
-- name: PublishComputerInitialization :one
WITH candidate AS MATERIALIZED (
    SELECT initialization.*, artifacts.id AS verified_artifact_id
      FROM computer_initializations AS initialization
      JOIN artifacts
        ON artifacts.environment_id = initialization.environment_id
       AND artifacts.id = sqlc.arg(artifact_id)
       AND artifacts.kind = 'workspace_version'
       AND artifacts.digest = initialization.digest
       AND artifacts.size_bytes = initialization.size_bytes
       AND artifacts.media_type = initialization.media_type
     WHERE initialization.id = sqlc.arg(id)
       AND initialization.environment_id = sqlc.arg(environment_id)
       AND initialization.computer_id = sqlc.arg(computer_id)
       AND initialization.status = 'registered'
     FOR UPDATE OF initialization
), published AS (
    UPDATE workspace_versions AS version
       SET artifact_id = candidate.verified_artifact_id,
           content_digest = candidate.digest,
           size_bytes = candidate.logical_bytes,
           status = 'committed', published_at = clock_timestamp()
      FROM candidate, workspaces
     WHERE version.environment_id = candidate.environment_id
       AND version.workspace_id = candidate.computer_id
       AND version.id = candidate.version_id
       AND version.status = 'initializing'
       AND version.parent_version_id IS NULL
       AND workspaces.environment_id = version.environment_id
       AND workspaces.id = version.workspace_id
       AND workspaces.head_version_id = version.id
       AND workspaces.initial_config IS NULL
    RETURNING version.id, version.artifact_id, version.published_at
), configured AS (
    UPDATE workspaces AS computer
       SET initial_config = candidate.initial_config,
           updated_at = published.published_at
      FROM candidate, published
     WHERE computer.environment_id = candidate.environment_id
       AND computer.id = candidate.computer_id
       AND published.id = candidate.version_id
    RETURNING computer.id
)
UPDATE computer_initializations AS initialization
   SET status = 'consumed', artifact_id = published.artifact_id,
       consumed_at = published.published_at
  FROM published, configured
 WHERE initialization.id = sqlc.arg(id)
   AND initialization.computer_id = configured.id
   AND initialization.version_id = published.id
   AND initialization.status = 'registered'
RETURNING initialization.*;

-- name: AbandonComputerInitialization :one
UPDATE computer_initializations
   SET status = 'abandoned', abandoned_at = COALESCE(abandoned_at, clock_timestamp())
 WHERE id = sqlc.arg(id)
   AND environment_id = sqlc.arg(environment_id)
   AND computer_id = sqlc.arg(computer_id)
   AND status IN ('registered', 'abandoned')
RETURNING *;

-- Revocation only: take Runtime before candidate locks, matching publication.
-- A lost preparation cannot become usable again under its original fence. Keep
-- the abandoned row: neither VM termination nor this transition excludes a late
-- host upload, and neither authorizes deleting an object from shared storage.
-- name: AbandonRevokedComputerInitializations :execrows
WITH revoked AS MATERIALIZED (
    SELECT initialization.id
      FROM computer_initializations AS initialization
      JOIN runtime_instances AS runtime ON runtime.id = initialization.runtime_instance_id
     WHERE initialization.status = 'registered'
       AND (runtime.desired_state <> 'ready'
            OR runtime.desired_version <> initialization.runtime_desired_version
            OR runtime.observed_state IN ('closed', 'failed', 'lost')
            OR runtime.reclaimed_at IS NOT NULL
            OR runtime.reserved_workspace_version_id IS DISTINCT FROM initialization.version_id
            OR (runtime.observed_state = 'allocated'
                AND runtime.preparation_expires_at <= statement_timestamp()))
     ORDER BY initialization.created_at, initialization.id
     LIMIT sqlc.arg(row_limit)
     FOR UPDATE OF runtime SKIP LOCKED
)
UPDATE computer_initializations AS initialization
   SET status = 'abandoned', abandoned_at = clock_timestamp()
  FROM revoked
 WHERE initialization.id = revoked.id
   AND initialization.status = 'registered';

-- Historical receipt access is scoped to the original authenticated Worker and
-- recorded preparation fence. Current readiness/head/desired state is irrelevant.
-- name: GetWorkerComputerInitialization :one
SELECT initialization.*
  FROM computer_initializations AS initialization
  JOIN runtime_instances AS runtime ON runtime.id=initialization.runtime_instance_id
 WHERE initialization.runtime_instance_id=sqlc.arg(runtime_instance_id)
   AND initialization.runtime_desired_version=sqlc.arg(runtime_desired_version)
   AND runtime.worker_instance_id=sqlc.arg(worker_instance_id)
   AND runtime.worker_group_id=sqlc.arg(worker_group_id)
   AND runtime.worker_epoch=sqlc.arg(worker_epoch);
