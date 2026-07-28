-- name: CreateDeploymentDefinition :one
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
) VALUES (
    sqlc.arg(id),
    sqlc.arg(environment_id),
    sqlc.arg(deployment_id),
    sqlc.arg(kind),
    sqlc.arg(declared_id),
    sqlc.arg(manifest_version),
    sqlc.arg(manifest),
    sqlc.arg(manifest_digest),
    sqlc.narg(artifact_id)
)
RETURNING *;

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
   AND deployments.status = 'deployed'
 LIMIT 1;

-- name: GetCurrentDeploymentDefinition :one
SELECT deployment_definitions.*
  FROM deployment_definitions
  JOIN deployments
    ON deployments.environment_id = deployment_definitions.environment_id
   AND deployments.id = deployment_definitions.deployment_id
   AND deployments.status = 'deployed'
  JOIN environments
    ON environments.id = deployment_definitions.environment_id
   AND environments.current_deployment_id = deployment_definitions.deployment_id
 WHERE deployment_definitions.environment_id = sqlc.arg(environment_id)
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
   AND deployments.status = 'deployed'
 ORDER BY deployment_definitions.kind, deployment_definitions.declared_id;

-- name: GetDeploymentProgramAuthority :one
SELECT deployments.id AS deployment_id,
       deployments.environment_id,
       deployments.version AS deployment_version,
       deployments.program_artifact_id,
       program_artifact.digest AS program_artifact_digest,
       program_artifact.size_bytes AS program_artifact_size_bytes,
       program_artifact.media_type AS program_artifact_media_type,
       deployments.build_runtime_digest,
       deployments.build_contract_version,
       deployments.program_index_digest,
       deployments.queue_config
  FROM deployments
  JOIN artifacts AS program_artifact
    ON program_artifact.environment_id = deployments.environment_id
   AND program_artifact.id = deployments.program_artifact_id
   AND program_artifact.kind = 'deployment_program'
 WHERE deployments.environment_id = sqlc.arg(environment_id)
   AND deployments.id = sqlc.arg(deployment_id)
   AND deployments.status = 'deployed'
 LIMIT 1;
