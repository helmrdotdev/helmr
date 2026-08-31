-- name: CreateDeployment :one
INSERT INTO deployments (
    id,
    org_id,
    project_id,
    environment_id,
    version,
    bundle_digest,
    runtime_artifact_digest,
    program_artifact_id,
    program_index_digest,
    queue_config
) VALUES (
    sqlc.arg(id),
    sqlc.arg(org_id),
    sqlc.arg(project_id),
    sqlc.arg(environment_id),
    sqlc.arg(version),
    sqlc.arg(bundle_digest),
    sqlc.arg(runtime_artifact_digest),
    sqlc.arg(program_artifact_id),
    sqlc.arg(program_index_digest),
    sqlc.arg(queue_config)
)
ON CONFLICT (environment_id, bundle_digest) DO UPDATE SET
    bundle_digest = deployments.bundle_digest
RETURNING *;

-- name: LockDeploymentBundle :exec
SELECT pg_advisory_xact_lock(
    hashtextextended(
        jsonb_build_array(
            'helmr.deployment-bundle.v0',
            sqlc.arg(environment_id)::uuid::text,
            sqlc.arg(bundle_digest)::text
        )::text,
        0
    )
);

-- name: GetDeploymentByBundleDigest :one
SELECT *
  FROM deployments
 WHERE environment_id = sqlc.arg(environment_id)
   AND bundle_digest = sqlc.arg(bundle_digest);

-- name: PromoteDeployment :exec
UPDATE environments
   SET current_deployment_id = sqlc.arg(deployment_id),
       updated_at = now()
 WHERE org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND id = sqlc.arg(environment_id);

-- name: LockDeploymentPromotionTarget :one
SELECT deployments.*
  FROM environments
  JOIN deployments
    ON deployments.org_id = environments.org_id
   AND deployments.project_id = environments.project_id
   AND deployments.environment_id = environments.id
 WHERE environments.org_id = sqlc.arg(org_id)
   AND environments.project_id = sqlc.arg(project_id)
   AND environments.id = sqlc.arg(environment_id)
   AND deployments.id = sqlc.arg(deployment_id)
 FOR NO KEY UPDATE OF environments;

-- name: GetDeployment :one
SELECT *
  FROM deployments
 WHERE org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND id = sqlc.arg(id);

-- name: GetDeploymentForOrg :one
SELECT *
  FROM deployments
 WHERE org_id = sqlc.arg(org_id)
   AND id = sqlc.arg(id);

-- name: ListScopedDeployments :many
SELECT deployments.id,
       deployments.version,
       deployments.bundle_digest,
       deployments.created_at
  FROM deployments
 WHERE deployments.org_id = sqlc.arg(org_id)
   AND deployments.project_id = sqlc.arg(project_id)
   AND deployments.environment_id = sqlc.arg(environment_id)
   AND (
       NOT sqlc.arg(has_after)::boolean
       OR (deployments.created_at, deployments.id) < (
           sqlc.arg(after_created_at)::timestamptz,
           sqlc.arg(after_id)::uuid
       )
   )
 ORDER BY deployments.created_at DESC, deployments.id DESC
 LIMIT sqlc.arg(row_limit);

-- name: GetCurrentDeployment :one
SELECT deployments.*
  FROM deployments
  JOIN environments ON environments.org_id = deployments.org_id
                   AND environments.project_id = deployments.project_id
                   AND environments.id = deployments.environment_id
                   AND environments.current_deployment_id = deployments.id
 WHERE deployments.org_id = sqlc.arg(org_id)
   AND deployments.project_id = sqlc.arg(project_id)
   AND deployments.environment_id = sqlc.arg(environment_id)
 LIMIT 1;

-- name: GetCurrentDeploymentForRoute :one
SELECT deployments.*
  FROM deployments
  JOIN environments ON environments.org_id = deployments.org_id
                   AND environments.project_id = deployments.project_id
                   AND environments.id = deployments.environment_id
                   AND environments.current_deployment_id = deployments.id
 WHERE deployments.org_id = sqlc.arg(org_id)
   AND deployments.project_id = sqlc.arg(project_id)
   AND deployments.environment_id = sqlc.arg(environment_id)
 LIMIT 1;
