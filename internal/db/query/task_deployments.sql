-- name: CreateTaskDeployment :one
INSERT INTO task_deployments (
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

-- name: ActivateTaskDeployment :one
WITH archived AS (
    UPDATE task_deployments
       SET status = 'archived',
           archived_at = now()
     WHERE org_id = sqlc.arg(org_id)
       AND project_id = sqlc.arg(project_id)
       AND environment_id = sqlc.arg(environment_id)
       AND id <> sqlc.arg(id)
       AND status = 'active'
    RETURNING id
)
UPDATE task_deployments
   SET status = 'active',
       deployed_at = now(),
       archived_at = NULL
 WHERE task_deployments.org_id = sqlc.arg(org_id)
   AND task_deployments.project_id = sqlc.arg(project_id)
   AND task_deployments.environment_id = sqlc.arg(environment_id)
   AND task_deployments.id = sqlc.arg(id)
   AND task_deployments.status = 'creating'
RETURNING *;

-- name: CreateDeployedTask :one
INSERT INTO deployed_tasks (
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

-- name: GetActiveTaskDeployment :one
SELECT id,
       org_id,
       project_id,
       environment_id,
       source_digest,
       status,
       created_at,
       deployed_at,
       archived_at
  FROM task_deployments
 WHERE org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND status = 'active'
 LIMIT 1;

-- name: ListDeployedTasksForDeployment :many
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
  FROM deployed_tasks
 WHERE org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND deployment_id = sqlc.arg(deployment_id)
 ORDER BY task_id ASC;

-- name: GetActiveDeployedTask :one
SELECT deployed_tasks.*,
       task_deployments.source_digest
  FROM deployed_tasks
  JOIN task_deployments ON task_deployments.org_id = deployed_tasks.org_id
                       AND task_deployments.project_id = deployed_tasks.project_id
                       AND task_deployments.environment_id = deployed_tasks.environment_id
                       AND task_deployments.id = deployed_tasks.deployment_id
 WHERE deployed_tasks.org_id = sqlc.arg(org_id)
   AND deployed_tasks.project_id = sqlc.arg(project_id)
   AND deployed_tasks.environment_id = sqlc.arg(environment_id)
   AND deployed_tasks.task_id = sqlc.arg(task_id)
   AND task_deployments.status = 'active'
 LIMIT 1;
