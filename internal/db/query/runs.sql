-- name: CreateRun :one
WITH default_scope AS (
    SELECT projects.id AS project_id,
           environments.id AS environment_id
      FROM projects
      JOIN environments ON environments.org_id = projects.org_id
                       AND environments.project_id = projects.id
                       AND environments.is_default
                       AND environments.archived_at IS NULL
     WHERE projects.org_id = sqlc.arg(org_id)
       AND projects.is_default
       AND projects.archived_at IS NULL
     LIMIT 1
),
created AS (
    INSERT INTO runs (
        id,
        org_id,
        project_id,
        environment_id,
        deployment_id,
        deployment_task_id,
        task_id,
        payload,
        secret_bindings,
        workspace_repository,
        workspace_installation_id,
        workspace_github_repository_id,
        workspace_ref,
        workspace_sha,
        workspace_subpath,
        workspace_ref_kind,
        workspace_ref_name,
        workspace_full_ref,
        workspace_default_branch,
        workspace_pr_number,
        workspace_pr_base_ref,
        workspace_pr_base_sha,
        workspace_pr_head_ref,
        workspace_pr_head_sha,
        max_duration_seconds
    ) VALUES (
        sqlc.arg(id),
        sqlc.arg(org_id),
        (SELECT project_id FROM default_scope),
        (SELECT environment_id FROM default_scope),
        sqlc.arg(deployment_id),
        sqlc.arg(deployment_task_id),
        sqlc.arg(task_id),
        sqlc.arg(payload),
        sqlc.arg(secret_bindings),
        sqlc.arg(workspace_repository),
        sqlc.arg(workspace_installation_id),
        sqlc.arg(workspace_github_repository_id),
        sqlc.arg(workspace_ref),
        sqlc.arg(workspace_sha),
        sqlc.arg(workspace_subpath),
        sqlc.arg(workspace_ref_kind),
        sqlc.arg(workspace_ref_name),
        sqlc.arg(workspace_full_ref),
        sqlc.arg(workspace_default_branch),
        sqlc.arg(workspace_pr_number),
        sqlc.arg(workspace_pr_base_ref),
        sqlc.arg(workspace_pr_base_sha),
        sqlc.arg(workspace_pr_head_ref),
        sqlc.arg(workspace_pr_head_sha),
        sqlc.arg(max_duration_seconds)
    )
    RETURNING id, org_id, project_id, environment_id, deployment_id, deployment_task_id, task_id, status, exit_code, output, created_at, updated_at
),
created_event AS (
    INSERT INTO run_events (org_id, run_id, kind, payload)
    SELECT created.org_id, created.id, 'run.created', sqlc.arg(event_payload)
      FROM created
    RETURNING id
)
SELECT created.id, created.org_id, created.project_id, created.environment_id, created.deployment_id, created.deployment_task_id, created.task_id, created.status, created.exit_code, created.output, created.created_at, created.updated_at
  FROM created
  JOIN created_event ON true;

-- name: CreateScopedRun :one
WITH created AS (
    INSERT INTO runs (
        id,
        org_id,
        project_id,
        environment_id,
        deployment_id,
        deployment_task_id,
        task_id,
        payload,
        secret_bindings,
        workspace_repository,
        workspace_installation_id,
        workspace_github_repository_id,
        workspace_ref,
        workspace_sha,
        workspace_subpath,
        workspace_ref_kind,
        workspace_ref_name,
        workspace_full_ref,
        workspace_default_branch,
        workspace_pr_number,
        workspace_pr_base_ref,
        workspace_pr_base_sha,
        workspace_pr_head_ref,
        workspace_pr_head_sha,
        max_duration_seconds
    ) VALUES (
        sqlc.arg(id),
        sqlc.arg(org_id),
        sqlc.arg(project_id),
        sqlc.arg(environment_id),
        sqlc.arg(deployment_id),
        sqlc.arg(deployment_task_id),
        sqlc.arg(task_id),
        sqlc.arg(payload),
        sqlc.arg(secret_bindings),
        sqlc.arg(workspace_repository),
        sqlc.arg(workspace_installation_id),
        sqlc.arg(workspace_github_repository_id),
        sqlc.arg(workspace_ref),
        sqlc.arg(workspace_sha),
        sqlc.arg(workspace_subpath),
        sqlc.arg(workspace_ref_kind),
        sqlc.arg(workspace_ref_name),
        sqlc.arg(workspace_full_ref),
        sqlc.arg(workspace_default_branch),
        sqlc.arg(workspace_pr_number),
        sqlc.arg(workspace_pr_base_ref),
        sqlc.arg(workspace_pr_base_sha),
        sqlc.arg(workspace_pr_head_ref),
        sqlc.arg(workspace_pr_head_sha),
        sqlc.arg(max_duration_seconds)
    )
    RETURNING id, org_id, project_id, environment_id, deployment_id, deployment_task_id, task_id, status, exit_code, output, created_at, updated_at
),
created_event AS (
    INSERT INTO run_events (org_id, run_id, kind, payload)
    SELECT created.org_id, created.id, 'run.created', sqlc.arg(event_payload)
      FROM created
    RETURNING id
)
SELECT created.id, created.org_id, created.project_id, created.environment_id, created.deployment_id, created.deployment_task_id, created.task_id, created.status, created.exit_code, created.output, created.created_at, created.updated_at
  FROM created
  JOIN created_event ON true;

-- name: GetRun :one
SELECT * FROM runs
WHERE org_id = $1 AND id = $2;

-- name: GetScopedRun :one
SELECT * FROM runs
WHERE org_id = sqlc.arg(org_id)
  AND project_id = sqlc.arg(project_id)
  AND environment_id = sqlc.arg(environment_id)
  AND id = sqlc.arg(id);

-- name: ListRuns :many
SELECT * FROM runs
WHERE org_id = $1
  AND (
    sqlc.arg(status_filter)::text = 'all'
    OR (sqlc.arg(status_filter)::text = 'live' AND status NOT IN ('succeeded', 'failed', 'cancelled'))
    OR status::text = sqlc.arg(status_filter)::text
  )
ORDER BY created_at DESC
LIMIT sqlc.arg(row_limit);

-- name: ListScopedRuns :many
SELECT * FROM runs
WHERE org_id = sqlc.arg(org_id)
  AND project_id = sqlc.arg(project_id)
  AND environment_id = sqlc.arg(environment_id)
  AND (
    sqlc.arg(status_filter)::text = 'all'
    OR (sqlc.arg(status_filter)::text = 'live' AND status NOT IN ('succeeded', 'failed', 'cancelled'))
    OR status::text = sqlc.arg(status_filter)::text
  )
ORDER BY created_at DESC
LIMIT sqlc.arg(row_limit);

-- name: GetRunSummary :one
SELECT id, org_id, project_id, environment_id, deployment_id, deployment_task_id, task_id, status, exit_code, output, created_at, updated_at
FROM runs
WHERE org_id = $1 AND id = $2;

-- name: GetScopedRunSummary :one
SELECT id, org_id, project_id, environment_id, deployment_id, deployment_task_id, task_id, status, exit_code, output, created_at, updated_at
FROM runs
WHERE org_id = sqlc.arg(org_id)
  AND project_id = sqlc.arg(project_id)
  AND environment_id = sqlc.arg(environment_id)
  AND id = sqlc.arg(id);

-- name: CountRunsByStatus :one
SELECT count(*) FILTER (WHERE status = 'queued') AS queued,
       count(*) FILTER (WHERE status = 'running') AS running,
       count(*) FILTER (WHERE status = 'checkpointing') AS checkpointing,
       count(*) FILTER (WHERE status = 'waiting') AS waiting,
       count(*) FILTER (WHERE status = 'succeeded') AS succeeded,
       count(*) FILTER (WHERE status = 'failed') AS failed,
       count(*) FILTER (WHERE status = 'cancelled') AS cancelled
FROM runs
WHERE org_id = sqlc.arg(org_id);

-- name: CountScopedRunsByStatus :one
SELECT count(*) FILTER (WHERE status = 'queued') AS queued,
       count(*) FILTER (WHERE status = 'running') AS running,
       count(*) FILTER (WHERE status = 'checkpointing') AS checkpointing,
       count(*) FILTER (WHERE status = 'waiting') AS waiting,
       count(*) FILTER (WHERE status = 'succeeded') AS succeeded,
       count(*) FILTER (WHERE status = 'failed') AS failed,
       count(*) FILTER (WHERE status = 'cancelled') AS cancelled
FROM runs
WHERE org_id = sqlc.arg(org_id)
  AND project_id = sqlc.arg(project_id)
  AND environment_id = sqlc.arg(environment_id);

-- name: ListRunSummaries :many
SELECT id, org_id, project_id, environment_id, deployment_id, deployment_task_id, task_id, status, exit_code, output, created_at, updated_at
FROM runs
WHERE org_id = $1
  AND (
    sqlc.arg(status_filter)::text = 'all'
    OR (sqlc.arg(status_filter)::text = 'live' AND status NOT IN ('succeeded', 'failed', 'cancelled'))
    OR (sqlc.arg(status_filter)::text = 'running' AND status = 'running')
    OR status::text = sqlc.arg(status_filter)::text
  )
ORDER BY created_at DESC
LIMIT sqlc.arg(row_limit);

-- name: ListScopedRunSummaries :many
SELECT id, org_id, project_id, environment_id, deployment_id, deployment_task_id, task_id, status, exit_code, output, created_at, updated_at
FROM runs
WHERE org_id = sqlc.arg(org_id)
  AND project_id = sqlc.arg(project_id)
  AND environment_id = sqlc.arg(environment_id)
  AND (
    sqlc.arg(status_filter)::text = 'all'
    OR (sqlc.arg(status_filter)::text = 'live' AND status NOT IN ('succeeded', 'failed', 'cancelled'))
    OR (sqlc.arg(status_filter)::text = 'running' AND status = 'running')
    OR status::text = sqlc.arg(status_filter)::text
  )
ORDER BY created_at DESC
LIMIT sqlc.arg(row_limit);
