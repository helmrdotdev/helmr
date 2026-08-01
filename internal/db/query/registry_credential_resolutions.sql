-- name: LockRegistryCredentialBuildLease :one
SELECT sqlc.embed(deployments),
       sqlc.embed(deployment_build_leases),
       deployment_source_artifacts.digest AS submitted_source_digest,
       worker_instances.runtime_identity_id AS runtime_identity_id
  FROM deployments
  JOIN deployment_build_leases
    ON deployment_build_leases.deployment_id = deployments.id
   AND deployment_build_leases.id = deployments.current_build_lease_id
  JOIN artifacts AS deployment_source_artifacts
    ON deployment_source_artifacts.environment_id = deployments.environment_id
   AND deployment_source_artifacts.id = deployments.deployment_source_artifact_id
   AND deployment_source_artifacts.kind = 'deployment_source'
  JOIN worker_instances
    ON worker_instances.id = deployment_build_leases.worker_instance_id
   AND worker_instances.current_epoch = deployment_build_leases.worker_epoch
   AND worker_instances.runtime_identity_id IS NOT NULL
 WHERE deployments.environment_id = sqlc.arg(environment_id)
   AND deployments.id = sqlc.arg(deployment_id)
   AND deployments.status = 'building'
   AND deployment_build_leases.id = sqlc.arg(build_lease_id)
   AND deployment_build_leases.lease_sequence = sqlc.arg(build_lease_generation)
   AND deployment_build_leases.worker_instance_id = sqlc.arg(worker_instance_id)
   AND deployment_build_leases.worker_epoch = sqlc.arg(worker_epoch)
   AND deployment_build_leases.state = 'running'
   AND deployment_build_leases.expires_at > transaction_timestamp()
 FOR UPDATE OF deployments, deployment_build_leases;

-- name: LockRegistryCredentialImageOperation :one
SELECT *
  FROM idempotency_claims
 WHERE environment_id = sqlc.arg(environment_id)
   AND id = sqlc.arg(image_operation_id)
   AND operation = 'workspace.image.build'
   AND state = 'pending'
   AND retired_at IS NULL
 FOR UPDATE;

-- name: LockWorkspaceImageOperationForResult :one
SELECT *
  FROM idempotency_claims
 WHERE environment_id = sqlc.arg(environment_id)
   AND id = sqlc.arg(image_operation_id)
   AND operation = 'workspace.image.build'
   AND retired_at IS NULL
 FOR UPDATE;

-- name: LockCompletedWorkspaceImageOperation :one
SELECT *
  FROM idempotency_claims
 WHERE environment_id = sqlc.arg(environment_id)
   AND id = sqlc.arg(image_operation_id)
   AND operation = 'workspace.image.build'
   AND state = 'completed'
   AND retired_at IS NULL
 FOR SHARE;

-- name: LockRegistryCredentialSecretsByName :many
SELECT secrets.*
  FROM secrets
 WHERE secrets.environment_id = sqlc.arg(environment_id)
   AND secrets.name = ANY(sqlc.arg(secret_names)::text[])
 ORDER BY secrets.id
 LIMIT 8
 FOR UPDATE OF secrets;

-- name: CreateRegistryCredentialResolution :one
INSERT INTO registry_credential_resolutions (
    id,
    environment_id,
    deployment_id,
    build_lease_id,
    image_operation_id,
    plan_digest,
    registry_authority,
    username,
    secret_id,
    secret_version_id,
    revocation_generation
)
SELECT
    sqlc.arg(id),
    sqlc.arg(environment_id),
    sqlc.arg(deployment_id),
    sqlc.arg(build_lease_id),
    sqlc.arg(image_operation_id),
    sqlc.arg(plan_digest),
    sqlc.arg(registry_authority),
    sqlc.arg(username),
    sqlc.arg(secret_id),
    sqlc.arg(secret_version_id),
    sqlc.arg(revocation_generation)
 WHERE EXISTS (
       SELECT 1
         FROM idempotency_claims
        WHERE idempotency_claims.environment_id = sqlc.arg(environment_id)
          AND idempotency_claims.id = sqlc.arg(image_operation_id)
          AND idempotency_claims.operation = 'workspace.image.build'
          AND idempotency_claims.state = 'pending'
          AND idempotency_claims.retired_at IS NULL
 )
RETURNING *;

-- name: ListRegistryCredentialResolutions :many
SELECT registry_credential_resolutions.*
  FROM registry_credential_resolutions
  JOIN idempotency_claims
    ON idempotency_claims.environment_id = registry_credential_resolutions.environment_id
   AND idempotency_claims.id = registry_credential_resolutions.image_operation_id
   AND idempotency_claims.operation = 'workspace.image.build'
 WHERE registry_credential_resolutions.environment_id = sqlc.arg(environment_id)
   AND registry_credential_resolutions.deployment_id = sqlc.arg(deployment_id)
   AND registry_credential_resolutions.build_lease_id = sqlc.arg(build_lease_id)
   AND registry_credential_resolutions.image_operation_id = sqlc.arg(image_operation_id)
 ORDER BY registry_credential_resolutions.registry_authority;

-- name: LockRegistryCredentialResolutionForDelivery :one
SELECT sqlc.embed(registry_credential_resolutions),
       sqlc.embed(idempotency_claims),
       sqlc.embed(secrets),
       sqlc.embed(secret_versions)
  FROM registry_credential_resolutions
  JOIN idempotency_claims
    ON idempotency_claims.environment_id = registry_credential_resolutions.environment_id
   AND idempotency_claims.id = registry_credential_resolutions.image_operation_id
   AND idempotency_claims.operation = 'workspace.image.build'
  JOIN secrets
    ON secrets.environment_id = registry_credential_resolutions.environment_id
   AND secrets.id = registry_credential_resolutions.secret_id
  JOIN secret_versions
    ON secret_versions.secret_id = registry_credential_resolutions.secret_id
   AND secret_versions.id = registry_credential_resolutions.secret_version_id
 WHERE registry_credential_resolutions.environment_id = sqlc.arg(environment_id)
   AND registry_credential_resolutions.deployment_id = sqlc.arg(deployment_id)
   AND registry_credential_resolutions.build_lease_id = sqlc.arg(build_lease_id)
   AND registry_credential_resolutions.image_operation_id = sqlc.arg(image_operation_id)
   AND registry_credential_resolutions.id = sqlc.arg(resolution_id)
   AND registry_credential_resolutions.registry_authority = sqlc.arg(registry_authority)
   AND registry_credential_resolutions.plan_digest = sqlc.arg(plan_digest)
   AND idempotency_claims.state = 'pending'
   AND idempotency_claims.retired_at IS NULL
   AND secrets.state = 'active'
   AND secrets.revocation_generation = registry_credential_resolutions.revocation_generation
 FOR SHARE OF idempotency_claims, secrets, secret_versions;

-- name: ListRevokedImageOperationIDsForBuildLease :many
SELECT DISTINCT registry_credential_resolutions.image_operation_id
  FROM registry_credential_resolutions
  JOIN deployments
    ON deployments.id = registry_credential_resolutions.deployment_id
   AND deployments.current_build_lease_id = registry_credential_resolutions.build_lease_id
  JOIN secrets
    ON secrets.environment_id = registry_credential_resolutions.environment_id
   AND secrets.id = registry_credential_resolutions.secret_id
 WHERE registry_credential_resolutions.build_lease_id = sqlc.arg(build_lease_id)
   AND (
       secrets.state <> 'active'
       OR secrets.revocation_generation <> registry_credential_resolutions.revocation_generation
   )
 ORDER BY registry_credential_resolutions.image_operation_id
 LIMIT 10000;
