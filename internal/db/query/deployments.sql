-- name: CreateDeployment :one
INSERT INTO deployments (
    id,
    org_id,
    project_id,
    environment_id,
    version,
    content_hash,
    deployment_source_digest,
    promote_on_deploy,
    status
) VALUES (
    sqlc.arg(id),
    sqlc.arg(org_id),
    sqlc.arg(project_id),
    sqlc.arg(environment_id),
    sqlc.arg(version),
    sqlc.arg(content_hash),
    sqlc.arg(deployment_source_digest),
    sqlc.arg(promote_on_deploy),
    sqlc.arg(status)
)
RETURNING *;

-- name: LockDeploymentReusableBuildKey :exec
SELECT pg_advisory_xact_lock(
    hashtextextended(
        concat_ws(
            ':',
            sqlc.arg(org_id)::uuid::text,
            sqlc.arg(project_id)::uuid::text,
            sqlc.arg(environment_id)::uuid::text,
            sqlc.arg(content_hash)::text
        ),
        0
    )
);

-- name: GetReusableDeploymentByContentHash :one
SELECT *
  FROM deployments
 WHERE org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND content_hash = sqlc.arg(content_hash)
   AND status IN ('queued', 'building', 'deployed');

-- name: UpdateDeploymentPromotionIntent :one
UPDATE deployments
   SET promote_on_deploy = deployments.promote_on_deploy OR sqlc.arg(promote_on_deploy)::boolean
 WHERE org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND id = sqlc.arg(id)
RETURNING *;

-- name: AllocateDeploymentVersion :one
WITH allocated AS (
    INSERT INTO deployment_version_counters (
        org_id,
        project_id,
        environment_id,
        prefix,
        next_ordinal
    ) VALUES (
        sqlc.arg(org_id),
        sqlc.arg(project_id),
        sqlc.arg(environment_id),
        sqlc.arg(prefix),
        2
    )
    ON CONFLICT (org_id, project_id, environment_id, prefix)
    DO UPDATE
       SET next_ordinal = deployment_version_counters.next_ordinal + 1,
           updated_at = now()
    RETURNING prefix, next_ordinal
)
SELECT concat(prefix, '.', next_ordinal - 1)::text AS version
  FROM allocated;

-- name: MarkDeploymentFailed :one
UPDATE deployments
   SET status = 'failed',
       failure = sqlc.arg(failure),
       failed_at = now()
 WHERE deployments.org_id = sqlc.arg(org_id)
   AND deployments.project_id = sqlc.arg(project_id)
   AND deployments.environment_id = sqlc.arg(environment_id)
   AND deployments.id = sqlc.arg(id)
   AND deployments.status IN ('queued', 'building')
RETURNING *;

-- name: LeaseQueuedDeploymentBuild :one
WITH candidate AS (
    SELECT deployments.id
      FROM deployments
     WHERE deployments.status = 'queued'
        OR (
            deployments.status = 'building'
            AND deployments.build_lease_expires_at < now()
        )
     ORDER BY deployments.created_at ASC
     LIMIT 1
     FOR UPDATE SKIP LOCKED
),
updated AS (
    UPDATE deployments
       SET status = 'building',
           building_at = COALESCE(deployments.building_at, now()),
           build_lease_id = sqlc.arg(build_lease_id),
           build_worker_instance_id = sqlc.arg(build_worker_instance_id),
           build_lease_expires_at = sqlc.arg(build_lease_expires_at),
           build_attempt = deployments.build_attempt + 1
      FROM candidate
     WHERE deployments.id = candidate.id
    RETURNING deployments.*
)
SELECT updated.id,
       updated.org_id,
       updated.project_id,
       updated.environment_id,
       updated.version,
       updated.content_hash,
       updated.deployment_source_digest,
       cas_objects.size_bytes AS source_size_bytes,
       cas_objects.media_type AS source_media_type,
       updated.build_manifest_digest,
       updated.deployment_manifest_digest,
       updated.status,
       updated.promote_on_deploy,
       updated.failure,
       updated.build_lease_id,
       updated.build_worker_instance_id,
       updated.build_lease_expires_at,
       updated.build_attempt,
       updated.created_at,
       updated.building_at,
       updated.built_at,
       updated.deployed_at,
       updated.failed_at
  FROM updated
  JOIN cas_objects ON cas_objects.digest = updated.deployment_source_digest;

-- name: CompleteDeploymentBuild :one
UPDATE deployments
   SET status = 'deployed',
       build_manifest_digest = sqlc.arg(build_manifest_digest),
       deployment_manifest_digest = sqlc.arg(deployment_manifest_digest),
       build_lease_id = NULL,
       build_worker_instance_id = NULL,
       build_lease_expires_at = NULL,
       built_at = COALESCE(built_at, now()),
       deployed_at = now()
 WHERE deployments.org_id = sqlc.arg(org_id)
   AND deployments.project_id = sqlc.arg(project_id)
   AND deployments.environment_id = sqlc.arg(environment_id)
   AND deployments.id = sqlc.arg(id)
   AND deployments.status = 'building'
   AND deployments.build_lease_id = sqlc.arg(build_lease_id)
   AND deployments.build_worker_instance_id = sqlc.arg(build_worker_instance_id)
   AND deployments.build_lease_expires_at > now()
RETURNING *;

-- name: FailDeploymentBuild :one
UPDATE deployments
   SET status = 'failed',
       failure = sqlc.arg(failure),
       build_lease_id = NULL,
       build_worker_instance_id = NULL,
       build_lease_expires_at = NULL,
       failed_at = now()
 WHERE deployments.org_id = sqlc.arg(org_id)
   AND deployments.project_id = sqlc.arg(project_id)
   AND deployments.environment_id = sqlc.arg(environment_id)
   AND deployments.id = sqlc.arg(id)
   AND deployments.status = 'building'
   AND deployments.build_lease_id = sqlc.arg(build_lease_id)
   AND deployments.build_worker_instance_id = sqlc.arg(build_worker_instance_id)
   AND deployments.build_lease_expires_at > now()
RETURNING *;

-- name: PromoteDeployment :one
WITH target AS (
    SELECT deployments.id,
           deployments.org_id,
           deployments.project_id,
           deployments.environment_id
      FROM deployments
     WHERE deployments.org_id = sqlc.arg(org_id)
       AND deployments.project_id = sqlc.arg(project_id)
       AND deployments.environment_id = sqlc.arg(environment_id)
       AND deployments.id = sqlc.arg(deployment_id)
       AND deployments.status = 'deployed'
),
previous AS (
    SELECT environments.current_deployment_id
      FROM environments
      JOIN target ON target.org_id = environments.org_id
                 AND target.project_id = environments.project_id
                 AND target.environment_id = environments.id
     FOR UPDATE OF environments
),
updated_environment AS (
    UPDATE environments
       SET current_deployment_id = target.id,
           updated_at = now()
      FROM target
     WHERE environments.org_id = target.org_id
       AND environments.project_id = target.project_id
       AND environments.id = target.environment_id
       AND environments.archived_at IS NULL
    RETURNING environments.current_deployment_id
),
promotion AS (
    INSERT INTO deployment_promotions (
        id,
        org_id,
        project_id,
        environment_id,
        deployment_id,
        previous_deployment_id,
        promoted_by_principal,
        reason
    )
    SELECT sqlc.arg(id),
           target.org_id,
           target.project_id,
           target.environment_id,
           target.id,
           previous.current_deployment_id,
           sqlc.arg(promoted_by_principal),
           sqlc.arg(reason)
      FROM target
      JOIN previous ON true
      JOIN updated_environment ON true
    RETURNING *
)
SELECT * FROM promotion;

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

-- name: GetDeploymentByVersion :one
SELECT *
  FROM deployments
 WHERE org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND version = sqlc.arg(version);

-- name: ListDeploymentsByVersionForOrg :many
SELECT *
  FROM deployments
 WHERE org_id = sqlc.arg(org_id)
   AND version = sqlc.arg(version)
 ORDER BY created_at ASC;

-- name: CreateDeploymentTask :one
INSERT INTO deployment_tasks (
    id,
    org_id,
    project_id,
    environment_id,
    deployment_id,
    task_id,
    file_path,
    export_name,
    handler_entrypoint,
    bundle_digest,
    requested_milli_cpu,
    requested_memory_mib,
    secret_declarations,
    resource_requirements,
    payload_schema,
    max_duration_seconds
) VALUES (
    sqlc.arg(id),
    sqlc.arg(org_id),
    sqlc.arg(project_id),
    sqlc.arg(environment_id),
    sqlc.arg(deployment_id),
    sqlc.arg(task_id),
    sqlc.arg(file_path),
    sqlc.arg(export_name),
    sqlc.arg(handler_entrypoint),
    sqlc.arg(bundle_digest),
    sqlc.arg(requested_milli_cpu),
    sqlc.arg(requested_memory_mib),
    sqlc.arg(secret_declarations),
    sqlc.arg(resource_requirements),
    sqlc.narg(payload_schema),
    sqlc.arg(max_duration_seconds)
)
RETURNING *;

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
   AND deployments.status = 'deployed'
 LIMIT 1;

-- name: ListDeploymentTasks :many
SELECT id,
       org_id,
       project_id,
       environment_id,
       deployment_id,
       task_id,
       file_path,
       export_name,
       handler_entrypoint,
       bundle_digest,
       requested_milli_cpu,
       requested_memory_mib,
       secret_declarations,
       resource_requirements,
       payload_schema,
       max_duration_seconds,
       created_at
  FROM deployment_tasks
 WHERE org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND deployment_id = sqlc.arg(deployment_id)
 ORDER BY task_id ASC;

-- name: GetCurrentDeploymentTask :one
SELECT deployment_tasks.*,
       deployments.deployment_source_digest
  FROM deployment_tasks
  JOIN deployments ON deployments.org_id = deployment_tasks.org_id
                  AND deployments.project_id = deployment_tasks.project_id
                  AND deployments.environment_id = deployment_tasks.environment_id
                  AND deployments.id = deployment_tasks.deployment_id
  JOIN environments ON environments.org_id = deployments.org_id
                   AND environments.project_id = deployments.project_id
                   AND environments.id = deployments.environment_id
                   AND environments.current_deployment_id = deployments.id
 WHERE deployment_tasks.org_id = sqlc.arg(org_id)
   AND deployment_tasks.project_id = sqlc.arg(project_id)
   AND deployment_tasks.environment_id = sqlc.arg(environment_id)
   AND deployment_tasks.task_id = sqlc.arg(task_id)
   AND deployments.status = 'deployed'
 LIMIT 1;

-- name: GetDeploymentTask :one
SELECT deployment_tasks.*,
       deployments.deployment_source_digest
  FROM deployment_tasks
  JOIN deployments ON deployments.org_id = deployment_tasks.org_id
                  AND deployments.project_id = deployment_tasks.project_id
                  AND deployments.environment_id = deployment_tasks.environment_id
                  AND deployments.id = deployment_tasks.deployment_id
 WHERE deployment_tasks.org_id = sqlc.arg(org_id)
   AND deployment_tasks.project_id = sqlc.arg(project_id)
   AND deployment_tasks.environment_id = sqlc.arg(environment_id)
   AND deployment_tasks.deployment_id = sqlc.arg(deployment_id)
   AND deployment_tasks.task_id = sqlc.arg(task_id)
   AND deployments.status = 'deployed'
 LIMIT 1;
