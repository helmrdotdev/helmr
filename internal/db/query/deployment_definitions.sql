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
    workspace_architecture,
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
    sqlc.narg(workspace_architecture),
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
       deployments.program_code_artifact_id,
       program_code.digest AS program_code_digest,
       program_code.size_bytes AS program_code_size_bytes,
       program_code.media_type AS program_code_media_type,
       deployments.program_dependency_artifact_id,
       program_dependencies.digest AS program_dependency_digest,
       program_dependencies.size_bytes AS program_dependency_size_bytes,
       program_dependencies.media_type AS program_dependency_media_type,
       deployments.program_runtime_digest,
       deployments.program_architecture,
       deployments.build_contract_version,
       deployments.queue_config
  FROM deployments
  JOIN artifacts AS program_code
    ON program_code.environment_id = deployments.environment_id
   AND program_code.id = deployments.program_code_artifact_id
  JOIN artifacts AS program_dependencies
    ON program_dependencies.environment_id = deployments.environment_id
   AND program_dependencies.id = deployments.program_dependency_artifact_id
 WHERE deployments.environment_id = sqlc.arg(environment_id)
   AND deployments.id = sqlc.arg(deployment_id)
   AND deployments.status = 'deployed'
 LIMIT 1;
