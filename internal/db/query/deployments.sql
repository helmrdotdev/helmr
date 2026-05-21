-- name: CreateDeployment :one
INSERT INTO deployments (
    id,
    org_id,
    project_id,
    environment_id,
    source_digest,
    status
) VALUES (
    sqlc.arg(id),
    sqlc.arg(org_id),
    sqlc.arg(project_id),
    sqlc.arg(environment_id),
    sqlc.arg(source_digest),
    sqlc.arg(status)
)
RETURNING *;

-- name: MarkDeploymentDeployed :one
UPDATE deployments
   SET status = 'deployed',
       deployed_at = now()
 WHERE deployments.org_id = sqlc.arg(org_id)
   AND deployments.project_id = sqlc.arg(project_id)
   AND deployments.environment_id = sqlc.arg(environment_id)
   AND deployments.id = sqlc.arg(id)
   AND deployments.status = 'creating'
RETURNING *;

-- name: AssignDeploymentLabel :one
INSERT INTO deployment_labels (
    org_id,
    project_id,
    environment_id,
    label,
    deployment_id
) VALUES (
    sqlc.arg(org_id),
    sqlc.arg(project_id),
    sqlc.arg(environment_id),
    sqlc.arg(label),
    sqlc.arg(deployment_id)
)
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
    module_path,
    export_name,
    requested_milli_cpu,
    requested_memory_mib
) VALUES (
    sqlc.arg(id),
    sqlc.arg(org_id),
    sqlc.arg(project_id),
    sqlc.arg(environment_id),
    sqlc.arg(deployment_id),
    sqlc.arg(task_id),
    sqlc.arg(module_path),
    sqlc.arg(export_name),
    sqlc.arg(requested_milli_cpu),
    sqlc.arg(requested_memory_mib)
)
RETURNING *;

-- name: GetCurrentDeployment :one
SELECT deployments.id,
       deployments.org_id,
       deployments.project_id,
       deployments.environment_id,
       deployments.source_digest,
       deployments.status,
       deployments.created_at,
       deployments.deployed_at
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
       module_path,
       export_name,
       requested_milli_cpu,
       requested_memory_mib,
       created_at
  FROM deployment_tasks
 WHERE org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND deployment_id = sqlc.arg(deployment_id)
 ORDER BY task_id ASC;

-- name: GetCurrentDeploymentTask :one
SELECT deployment_tasks.*,
       deployments.source_digest
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
