-- name: LockRuntimeSubstrateAuthority :one
SELECT deployments.org_id,
       deployments.project_id,
       deployments.environment_id,
       deployment_definitions.id AS deployment_definition_id
  FROM runtime_instances
  JOIN worker_instances
    ON worker_instances.id = runtime_instances.worker_instance_id
   AND worker_instances.worker_group_id = runtime_instances.worker_group_id
   AND worker_instances.current_epoch = runtime_instances.worker_epoch
  JOIN deployment_definitions
    ON deployment_definitions.environment_id = runtime_instances.environment_id
   AND deployment_definitions.id = runtime_instances.deployment_definition_id
   AND deployment_definitions.kind = 'workspace'
  JOIN deployments
    ON deployments.environment_id = deployment_definitions.environment_id
   AND deployments.id = deployment_definitions.deployment_id
 WHERE runtime_instances.deployment_definition_id = sqlc.arg(deployment_definition_id)
   AND runtime_instances.worker_instance_id = sqlc.arg(worker_instance_id)
   AND runtime_instances.worker_group_id = sqlc.arg(worker_group_id)
   AND runtime_instances.worker_epoch = sqlc.arg(worker_epoch)
   AND runtime_instances.reclaimed_at IS NULL
   AND runtime_instances.observed_state IN ('allocated', 'preparing', 'ready')
   AND worker_instances.state IN ('active', 'draining')
   AND worker_instances.supports_run
   AND worker_instances.substrate_format = sqlc.arg(substrate_format)
   AND worker_instances.substrate_builder_abi = sqlc.arg(builder_abi)
   AND worker_instances.substrate_layout_abi = sqlc.arg(layout_abi)
 LIMIT 1
 FOR SHARE OF runtime_instances, worker_instances;

-- name: InsertRuntimeSubstrate :execrows
INSERT INTO runtime_substrates (
    id,
    org_id,
    project_id,
    environment_id,
    deployment_definition_id,
    artifact_id,
    substrate_digest,
    substrate_format,
    builder_abi,
    layout_abi,
    substrate_size_bytes,
    source,
    created_by_worker_instance_id
) VALUES (
    sqlc.arg(id),
    sqlc.arg(org_id),
    sqlc.arg(project_id),
    sqlc.arg(environment_id),
    sqlc.arg(deployment_definition_id),
    sqlc.arg(artifact_id),
    sqlc.arg(substrate_digest),
    sqlc.arg(substrate_format),
    sqlc.arg(builder_abi),
    sqlc.arg(layout_abi),
    sqlc.arg(substrate_size_bytes),
    COALESCE(sqlc.arg(source)::jsonb, '{}'::jsonb),
    sqlc.narg(created_by_worker_instance_id)
)
ON CONFLICT ON CONSTRAINT runtime_substrates_input_key DO NOTHING;

-- name: GetRuntimeSubstrateRegistration :one
SELECT *
  FROM runtime_substrates
 WHERE org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND deployment_definition_id = sqlc.arg(deployment_definition_id)
   AND substrate_format = sqlc.arg(substrate_format)
   AND builder_abi = sqlc.arg(builder_abi)
   AND layout_abi = sqlc.arg(layout_abi)
   AND artifact_id = sqlc.arg(artifact_id)
   AND substrate_digest = sqlc.arg(substrate_digest)
   AND substrate_size_bytes = sqlc.arg(substrate_size_bytes)
 LIMIT 1;

-- name: GetRuntimeSubstrateForWorkspaceDefinition :one
SELECT runtime_substrates.*,
       artifacts.digest AS artifact_digest,
       artifacts.size_bytes AS artifact_size_bytes,
       artifacts.media_type AS artifact_media_type
  FROM runtime_substrates
  JOIN artifacts
    ON artifacts.org_id = runtime_substrates.org_id
   AND artifacts.project_id = runtime_substrates.project_id
   AND artifacts.environment_id = runtime_substrates.environment_id
   AND artifacts.id = runtime_substrates.artifact_id
  JOIN deployment_definitions
    ON deployment_definitions.environment_id = runtime_substrates.environment_id
   AND deployment_definitions.id = runtime_substrates.deployment_definition_id
   AND deployment_definitions.kind = 'workspace'
  JOIN deployments
    ON deployments.environment_id = deployment_definitions.environment_id
   AND deployments.id = deployment_definitions.deployment_id
 WHERE runtime_substrates.org_id = sqlc.arg(org_id)
   AND runtime_substrates.project_id = sqlc.arg(project_id)
   AND runtime_substrates.environment_id = sqlc.arg(environment_id)
   AND runtime_substrates.deployment_definition_id = sqlc.arg(deployment_definition_id)
   AND runtime_substrates.substrate_digest = sqlc.arg(substrate_digest)
   AND runtime_substrates.substrate_format = sqlc.arg(substrate_format)
   AND runtime_substrates.builder_abi = sqlc.arg(builder_abi)
   AND runtime_substrates.layout_abi = sqlc.arg(layout_abi)
 LIMIT 1;
