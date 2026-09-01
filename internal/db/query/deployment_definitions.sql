-- name: CreateDeploymentDefinitions :execrows
WITH input_definitions AS (
    SELECT input_ids.id,
           input_kinds.kind,
           input_declared_ids.declared_id,
           input_manifests.manifest,
           input_manifest_digests.manifest_digest,
           input_artifact_ids.artifact_id
      FROM unnest(sqlc.arg(ids)::uuid[])
           WITH ORDINALITY AS input_ids(id, position)
      JOIN unnest(sqlc.arg(kinds)::text[])
           WITH ORDINALITY AS input_kinds(kind, position)
        ON input_kinds.position = input_ids.position
      JOIN unnest(sqlc.arg(declared_ids)::text[])
           WITH ORDINALITY AS input_declared_ids(declared_id, position)
        ON input_declared_ids.position = input_ids.position
      JOIN unnest(sqlc.arg(manifests)::jsonb[])
           WITH ORDINALITY AS input_manifests(manifest, position)
        ON input_manifests.position = input_ids.position
      JOIN unnest(sqlc.arg(manifest_digests)::bytea[])
           WITH ORDINALITY AS input_manifest_digests(manifest_digest, position)
        ON input_manifest_digests.position = input_ids.position
      JOIN unnest(sqlc.arg(artifact_ids)::uuid[])
           WITH ORDINALITY AS input_artifact_ids(artifact_id, position)
        ON input_artifact_ids.position = input_ids.position
     WHERE cardinality(sqlc.arg(ids)::uuid[]) BETWEEN 0 AND 10000
       AND cardinality(sqlc.arg(kinds)::text[]) = cardinality(sqlc.arg(ids)::uuid[])
       AND cardinality(sqlc.arg(declared_ids)::text[]) = cardinality(sqlc.arg(ids)::uuid[])
       AND cardinality(sqlc.arg(manifests)::jsonb[]) = cardinality(sqlc.arg(ids)::uuid[])
       AND cardinality(sqlc.arg(manifest_digests)::bytea[]) = cardinality(sqlc.arg(ids)::uuid[])
       AND cardinality(sqlc.arg(artifact_ids)::uuid[]) = cardinality(sqlc.arg(ids)::uuid[])
)
INSERT INTO deployment_definitions (
    id,
    environment_id,
    deployment_id,
    kind,
    declared_id,
    manifest_version,
    manifest,
    manifest_digest,
    artifact_id
)
SELECT input_definitions.id,
       sqlc.arg(environment_id),
       sqlc.arg(deployment_id),
       input_definitions.kind,
       input_definitions.declared_id,
       sqlc.arg(manifest_version),
       input_definitions.manifest,
       input_definitions.manifest_digest,
       input_definitions.artifact_id
  FROM input_definitions;

-- name: GetDeploymentDefinition :one
SELECT deployment_definitions.*
  FROM deployment_definitions
  JOIN deployments
    ON deployments.environment_id = deployment_definitions.environment_id
   AND deployments.id = deployment_definitions.deployment_id
 WHERE deployment_definitions.environment_id = sqlc.arg(environment_id)
   AND deployment_definitions.deployment_id = sqlc.arg(deployment_id)
   AND deployment_definitions.kind = sqlc.arg(kind)
   AND deployment_definitions.declared_id = sqlc.arg(declared_id)
 LIMIT 1;

-- name: ListDeploymentDefinitionsForDeployment :many
SELECT deployment_definitions.*
  FROM deployment_definitions
  JOIN deployments
    ON deployments.environment_id = deployment_definitions.environment_id
   AND deployments.id = deployment_definitions.deployment_id
 WHERE deployment_definitions.environment_id = sqlc.arg(environment_id)
   AND deployment_definitions.deployment_id = sqlc.arg(deployment_id)
   AND (sqlc.narg(kind)::text IS NULL OR deployment_definitions.kind = sqlc.narg(kind)::text)
 ORDER BY deployment_definitions.kind, deployment_definitions.declared_id;

-- name: GetDeploymentProgramAuthority :one
SELECT deployments.id AS deployment_id,
       deployments.environment_id,
       deployments.version AS deployment_version,
       deployments.program_artifact_id,
       program_artifact.digest AS program_artifact_digest,
       program_artifact.size_bytes AS program_artifact_size_bytes,
       program_artifact.media_type AS program_artifact_media_type,
       deployments.runtime_artifact_digest,
       deployments.program_index_digest,
       deployments.queue_config
  FROM deployments
  JOIN artifacts AS program_artifact
    ON program_artifact.environment_id = deployments.environment_id
   AND program_artifact.id = deployments.program_artifact_id
   AND program_artifact.kind = 'deployment_program'
 WHERE deployments.environment_id = sqlc.arg(environment_id)
   AND deployments.id = sqlc.arg(deployment_id)
 LIMIT 1;

-- name: ListDefinitionSnapshots :many
SELECT declared_id
  FROM deployment_definitions
 WHERE environment_id = sqlc.arg(environment_id)
   AND deployment_id = sqlc.arg(deployment_id)
   AND kind = sqlc.arg(kind)
   AND (NOT sqlc.arg(has_after)::boolean OR declared_id COLLATE "C" > sqlc.arg(after_id)::text COLLATE "C")
 ORDER BY declared_id COLLATE "C", id
 LIMIT sqlc.arg(row_limit);

-- name: GetDefinitionSnapshot :one
SELECT declared_id
  FROM deployment_definitions
 WHERE environment_id = sqlc.arg(environment_id)
   AND deployment_id = sqlc.arg(deployment_id)
   AND kind = sqlc.arg(kind)
   AND declared_id = sqlc.arg(declared_id)
 LIMIT 1;
