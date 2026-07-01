-- name: UpsertRuntimeSubstrateArtifact :one
INSERT INTO runtime_substrate_artifacts (
    id,
    org_id,
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
ON CONFLICT (org_id, project_id, environment_id, deployment_sandbox_id, substrate_digest, substrate_format, builder_abi, layout_abi)
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
   AND artifacts.project_id = runtime_substrate_artifacts.project_id
   AND artifacts.environment_id = runtime_substrate_artifacts.environment_id
   AND artifacts.id = runtime_substrate_artifacts.artifact_id
 WHERE runtime_substrate_artifacts.org_id = sqlc.arg(org_id)
   AND runtime_substrate_artifacts.project_id = sqlc.arg(project_id)
   AND runtime_substrate_artifacts.environment_id = sqlc.arg(environment_id)
   AND runtime_substrate_artifacts.deployment_sandbox_id = sqlc.arg(deployment_sandbox_id)
   AND runtime_substrate_artifacts.substrate_digest = sqlc.arg(substrate_digest)
   AND runtime_substrate_artifacts.substrate_format = sqlc.arg(substrate_format)
   AND runtime_substrate_artifacts.builder_abi = sqlc.arg(builder_abi)
   AND runtime_substrate_artifacts.layout_abi = sqlc.arg(layout_abi)
   AND runtime_substrate_artifacts.retired_at IS NULL
 LIMIT 1;
