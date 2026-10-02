-- Called after Computer/Instance locks. The seed is addressed only by that
-- Computer's admitted specification; content-addressing does not grant access.
-- name: LockComputerSeed :one
SELECT s.* FROM computer_seeds s JOIN computer_specs spec
 ON spec.environment_id=s.environment_id AND spec.seed_digest=s.seed_digest
 AND spec.seed_size_bytes=s.seed_size_bytes AND spec.seed_media_type=s.seed_media_type
 WHERE spec.environment_id=sqlc.arg(environment_id) AND spec.id=sqlc.arg(computer_spec_id)
 AND spec.seed_artifact_id IS NOT NULL AND s.source_artifact_id IS NOT NULL
 AND s.format_version=1 AND s.logical_bytes=sqlc.arg(logical_bytes)
 FOR UPDATE OF s;

-- name: GetComputerSeedKey :one
SELECT k.* FROM computer_data_keys k JOIN computer_instances i
 ON i.environment_id=k.environment_id AND i.seed_key_id=k.id
 WHERE i.id=sqlc.arg(computer_instance_id) AND k.available AND k.is_seed_key;

-- Shared conversion survives only while admitted specs or physical attempts need
-- it. Historical spec/Instance identities alone do not retain the payload.
-- name: LockUnusedComputerSeeds :many
 SELECT s.id FROM computer_seeds s WHERE s.payload_retired_at IS NULL
 AND NOT EXISTS(SELECT 1 FROM computer_specs spec WHERE spec.environment_id=s.environment_id
   AND spec.seed_digest=s.seed_digest AND spec.seed_size_bytes=s.seed_size_bytes
   AND spec.seed_media_type=s.seed_media_type AND spec.seed_artifact_id IS NOT NULL)
 AND NOT EXISTS(SELECT 1 FROM computer_instances i WHERE i.seed_id=s.id AND i.reclaimed_at IS NULL)
 ORDER BY s.id LIMIT sqlc.arg(row_limit) FOR UPDATE SKIP LOCKED;

-- Recheck using a fresh statement snapshot while the candidate locks are held.
-- name: RetireUnusedComputerSeeds :execrows
UPDATE computer_seeds s SET root_id=NULL,source_artifact_id=NULL,payload_retired_at=clock_timestamp(),
 preparation_instance_id=NULL,preparation_key_id=NULL,lease_expires_at=NULL
 WHERE s.id=ANY(sqlc.arg(seed_ids)::uuid[]) AND s.payload_retired_at IS NULL
 AND NOT EXISTS(SELECT 1 FROM computer_specs spec WHERE spec.environment_id=s.environment_id
   AND spec.seed_digest=s.seed_digest AND spec.seed_size_bytes=s.seed_size_bytes
   AND spec.seed_media_type=s.seed_media_type AND spec.seed_artifact_id IS NOT NULL)
 AND NOT EXISTS(SELECT 1 FROM computer_instances i WHERE i.seed_id=s.id AND i.reclaimed_at IS NULL);
