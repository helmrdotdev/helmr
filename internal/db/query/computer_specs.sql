-- name: RegisterComputerSpec :one
WITH registered AS (
INSERT INTO computer_specs (
    id, environment_id, config, digest,
    seed_artifact_id, seed_digest, seed_size_bytes, seed_media_type
) VALUES (
    sqlc.arg(id), sqlc.arg(environment_id), sqlc.arg(config), sqlc.arg(digest),
    sqlc.arg(seed_artifact_id), sqlc.arg(seed_digest), sqlc.arg(seed_size_bytes), sqlc.arg(seed_media_type)
)
ON CONFLICT (environment_id, digest) DO UPDATE
    SET seed_artifact_id = COALESCE(computer_specs.seed_artifact_id, EXCLUDED.seed_artifact_id)
    WHERE computer_specs.config = EXCLUDED.config
      AND computer_specs.seed_digest = EXCLUDED.seed_digest
      AND computer_specs.seed_size_bytes = EXCLUDED.seed_size_bytes
      AND computer_specs.seed_media_type = EXCLUDED.seed_media_type
RETURNING *
), seed AS (
 INSERT INTO computer_seeds(id,environment_id,seed_digest,seed_size_bytes,seed_media_type,format_version,logical_bytes,source_artifact_id)
 SELECT gen_random_uuid(),environment_id,seed_digest,seed_size_bytes,seed_media_type,1,sqlc.arg(logical_bytes)::bigint,seed_artifact_id FROM registered
 ON CONFLICT (environment_id,seed_digest,format_version,logical_bytes) DO UPDATE
 SET source_artifact_id=COALESCE(computer_seeds.source_artifact_id,EXCLUDED.source_artifact_id),
     ready_at=CASE WHEN computer_seeds.payload_retired_at IS NOT NULL THEN NULL ELSE computer_seeds.ready_at END,
     payload_retired_at=NULL
 WHERE computer_seeds.seed_size_bytes=EXCLUDED.seed_size_bytes AND computer_seeds.seed_media_type=EXCLUDED.seed_media_type
 RETURNING environment_id,seed_digest
)
SELECT r.* FROM registered r JOIN seed s ON s.environment_id=r.environment_id AND s.seed_digest=r.seed_digest;

-- name: GetComputerSpec :one
SELECT * FROM computer_specs
 WHERE environment_id = sqlc.arg(environment_id) AND id = sqlc.arg(id);

-- name: DeleteUnusedComputerSeedArtifact :exec
DELETE FROM artifacts
 WHERE artifacts.environment_id = sqlc.arg(environment_id)
   AND artifacts.id = sqlc.arg(id)
   AND artifacts.kind = 'computer_image'
   AND NOT EXISTS (SELECT 1 FROM computer_seeds WHERE source_artifact_id=artifacts.id)
   AND NOT EXISTS (
       SELECT 1 FROM computer_specs
        WHERE computer_specs.environment_id = artifacts.environment_id
          AND computer_specs.seed_artifact_id = artifacts.id
   );
