-- name: UpsertRuntimeSubstrateArtifact :one
INSERT INTO runtime_substrate_artifacts (
    id,
    org_id,
    cell_id,
    project_id,
    environment_id,
    deployment_sandbox_id,
    artifact_id,
    substrate_digest,
    substrate_format,
    builder_abi,
    layout_abi,
    substrate_size_bytes,
    source,
    created_by_worker_instance_id,
    last_referenced_at
) VALUES (
    sqlc.arg(id),
    sqlc.arg(org_id),
    sqlc.arg(cell_id),
    sqlc.arg(project_id),
    sqlc.arg(environment_id),
    sqlc.arg(deployment_sandbox_id),
    sqlc.arg(artifact_id),
    sqlc.arg(substrate_digest),
    sqlc.arg(substrate_format),
    sqlc.arg(builder_abi),
    sqlc.arg(layout_abi),
    sqlc.arg(substrate_size_bytes),
    COALESCE(sqlc.arg(source)::jsonb, '{}'::jsonb),
    sqlc.narg(created_by_worker_instance_id),
    now()
)
ON CONFLICT (org_id, cell_id, project_id, environment_id, deployment_sandbox_id, substrate_digest, substrate_format, builder_abi, layout_abi)
DO UPDATE
   SET retired_at = NULL,
       last_referenced_at = now(),
       updated_at = now()
RETURNING *;

-- name: GetRuntimeSubstrateArtifactForSandbox :one
SELECT runtime_substrate_artifacts.*,
       artifacts.digest AS artifact_digest,
       artifacts.size_bytes AS artifact_size_bytes,
       artifacts.media_type AS artifact_media_type
  FROM runtime_substrate_artifacts
  JOIN artifacts
    ON artifacts.org_id = runtime_substrate_artifacts.org_id
   AND artifacts.cell_id = runtime_substrate_artifacts.cell_id
   AND artifacts.project_id = runtime_substrate_artifacts.project_id
   AND artifacts.environment_id = runtime_substrate_artifacts.environment_id
   AND artifacts.id = runtime_substrate_artifacts.artifact_id
  JOIN deployment_sandboxes
    ON deployment_sandboxes.org_id = runtime_substrate_artifacts.org_id
   AND deployment_sandboxes.project_id = runtime_substrate_artifacts.project_id
   AND deployment_sandboxes.environment_id = runtime_substrate_artifacts.environment_id
   AND deployment_sandboxes.id = runtime_substrate_artifacts.deployment_sandbox_id
  JOIN deployments
    ON deployments.org_id = deployment_sandboxes.org_id
   AND deployments.project_id = deployment_sandboxes.project_id
   AND deployments.environment_id = deployment_sandboxes.environment_id
   AND deployments.id = deployment_sandboxes.deployment_id
   AND deployments.build_cell_id = runtime_substrate_artifacts.cell_id
  JOIN environment_cells
    ON environment_cells.org_id = runtime_substrate_artifacts.org_id
   AND environment_cells.project_id = runtime_substrate_artifacts.project_id
   AND environment_cells.environment_id = runtime_substrate_artifacts.environment_id
   AND environment_cells.cell_id = runtime_substrate_artifacts.cell_id
   AND environment_cells.route_generation = deployments.build_route_generation
   AND environment_cells.route_state IN ('active', 'draining')
  JOIN org_cells ON org_cells.org_id = environment_cells.org_id
                AND org_cells.cell_id = environment_cells.cell_id
                AND org_cells.state = 'active'
  JOIN cells ON cells.id = environment_cells.cell_id
            AND cells.region_id = environment_cells.region_id
            AND cells.state = 'active'
 WHERE runtime_substrate_artifacts.org_id = sqlc.arg(org_id)
   AND runtime_substrate_artifacts.cell_id = sqlc.arg(cell_id)
   AND runtime_substrate_artifacts.project_id = sqlc.arg(project_id)
   AND runtime_substrate_artifacts.environment_id = sqlc.arg(environment_id)
   AND runtime_substrate_artifacts.deployment_sandbox_id = sqlc.arg(deployment_sandbox_id)
   AND runtime_substrate_artifacts.substrate_digest = sqlc.arg(substrate_digest)
   AND runtime_substrate_artifacts.substrate_format = sqlc.arg(substrate_format)
   AND runtime_substrate_artifacts.builder_abi = sqlc.arg(builder_abi)
   AND runtime_substrate_artifacts.layout_abi = sqlc.arg(layout_abi)
   AND runtime_substrate_artifacts.retired_at IS NULL
 LIMIT 1;
