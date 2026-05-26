-- name: CreateDeployment :one
INSERT INTO deployments (
    id,
    org_id,
    project_id,
    environment_id,
    content_hash,
    deployment_source_digest,
    status
) VALUES (
    sqlc.arg(id),
    sqlc.arg(org_id),
    sqlc.arg(project_id),
    sqlc.arg(environment_id),
    sqlc.arg(content_hash),
    sqlc.arg(deployment_source_digest),
    sqlc.arg(status)
)
ON CONFLICT (org_id, project_id, environment_id, content_hash)
WHERE status IN ('queued', 'building', 'deployed')
DO UPDATE
   SET deployment_source_digest = deployments.deployment_source_digest
RETURNING *;

-- name: MarkDeploymentFailed :one
UPDATE deployments
   SET status = 'failed',
       error_json = sqlc.arg(error_json),
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
       updated.content_hash,
       updated.deployment_source_digest,
       cas_objects.size_bytes AS source_size_bytes,
       cas_objects.media_type AS source_media_type,
       updated.build_manifest_digest,
       updated.deployment_manifest_digest,
       updated.status,
       updated.error_json,
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
       error_json = sqlc.arg(error_json),
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

-- name: AssignDeploymentLabel :one
INSERT INTO deployment_labels (
    org_id,
    project_id,
    environment_id,
    label,
    deployment_id
) SELECT
    sqlc.arg(org_id),
    sqlc.arg(project_id),
    sqlc.arg(environment_id),
    sqlc.arg(label),
    sqlc.arg(deployment_id)
  FROM deployments
 WHERE deployments.org_id = sqlc.arg(org_id)
   AND deployments.project_id = sqlc.arg(project_id)
   AND deployments.environment_id = sqlc.arg(environment_id)
   AND deployments.id = sqlc.arg(deployment_id)
   AND deployments.status = 'deployed'
ON CONFLICT (org_id, project_id, environment_id, label) DO UPDATE
   SET deployment_id = excluded.deployment_id,
       assigned_at = now()
RETURNING *;

-- name: GetDeployment :one
SELECT *
  FROM deployments
 WHERE org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND id = sqlc.arg(id);

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
    secrets_json,
    resources_json,
    payload_schema_json,
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
    sqlc.arg(secrets_json),
    sqlc.arg(resources_json),
    sqlc.narg(payload_schema_json),
    sqlc.arg(max_duration_seconds)
)
RETURNING *;

-- name: GetCurrentDeployment :one
SELECT deployments.id,
       deployments.org_id,
       deployments.project_id,
       deployments.environment_id,
       deployments.content_hash,
       deployments.deployment_source_digest,
       deployments.build_manifest_digest,
       deployments.deployment_manifest_digest,
       deployments.status,
       deployments.error_json,
       deployments.created_at,
       deployments.building_at,
       deployments.built_at,
       deployments.deployed_at,
       deployments.failed_at
  FROM deployments
  JOIN deployment_labels ON deployment_labels.org_id = deployments.org_id
                        AND deployment_labels.project_id = deployments.project_id
                        AND deployment_labels.environment_id = deployments.environment_id
                        AND deployment_labels.deployment_id = deployments.id
                        AND deployment_labels.label = 'current'
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
       secrets_json,
       resources_json,
       payload_schema_json,
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
  JOIN deployment_labels ON deployment_labels.org_id = deployments.org_id
                        AND deployment_labels.project_id = deployments.project_id
                        AND deployment_labels.environment_id = deployments.environment_id
                        AND deployment_labels.deployment_id = deployments.id
                        AND deployment_labels.label = 'current'
 WHERE deployment_tasks.org_id = sqlc.arg(org_id)
   AND deployment_tasks.project_id = sqlc.arg(project_id)
   AND deployment_tasks.environment_id = sqlc.arg(environment_id)
   AND deployment_tasks.task_id = sqlc.arg(task_id)
   AND deployments.status = 'deployed'
 LIMIT 1;
