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
        idempotency_key,
        idempotency_key_expires_at,
        idempotency_key_options,
        idempotency_request_hash,
        queue_name,
        queue_concurrency_limit,
        concurrency_key,
        priority,
        queue_timestamp,
        ttl,
        queued_expires_at,
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
        max_duration_seconds,
        schedule_id,
        schedule_instance_id,
        scheduled_at
    )
    SELECT sqlc.arg(id),
           sqlc.arg(org_id),
           sqlc.arg(project_id),
           sqlc.arg(environment_id),
           sqlc.arg(deployment_id),
           sqlc.arg(deployment_task_id),
           sqlc.arg(task_id),
           sqlc.arg(payload),
           sqlc.arg(secret_bindings),
           sqlc.narg(idempotency_key),
           sqlc.narg(idempotency_key_expires_at),
           coalesce(sqlc.arg(idempotency_key_options)::jsonb, '{}'::jsonb),
           sqlc.narg(idempotency_request_hash),
           sqlc.arg(queue_name),
           sqlc.narg(queue_concurrency_limit),
           sqlc.narg(concurrency_key),
           sqlc.arg(priority),
           sqlc.arg(queue_timestamp),
           sqlc.arg(ttl),
           sqlc.narg(queued_expires_at),
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
           sqlc.arg(max_duration_seconds),
           sqlc.narg(schedule_id),
           sqlc.narg(schedule_instance_id),
           sqlc.narg(scheduled_at)
     WHERE sqlc.narg(schedule_instance_id)::uuid IS NULL
        OR EXISTS (
            SELECT 1
              FROM task_schedule_instances
              JOIN task_schedules ON task_schedules.id = task_schedule_instances.schedule_id
             WHERE task_schedule_instances.id = sqlc.narg(schedule_instance_id)
               AND task_schedule_instances.generation = sqlc.narg(schedule_generation)
               AND task_schedule_instances.next_scheduled_at = sqlc.narg(scheduled_at)
               AND task_schedule_instances.schedule_id = sqlc.narg(schedule_id)
               AND task_schedule_instances.org_id = sqlc.arg(org_id)
               AND task_schedule_instances.project_id = sqlc.arg(project_id)
               AND task_schedule_instances.environment_id = sqlc.arg(environment_id)
               AND task_schedule_instances.active
               AND (
                   task_schedule_instances.retry_after IS NULL
                   OR task_schedule_instances.retry_after <= now()
               )
               AND task_schedules.org_id = sqlc.arg(org_id)
               AND task_schedules.project_id = sqlc.arg(project_id)
               AND task_schedules.environment_id = sqlc.arg(environment_id)
               AND task_schedules.active
               AND task_schedules.deleted_at IS NULL
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

-- name: GetScopedRunByIdempotencyKey :one
SELECT id, org_id, project_id, environment_id, deployment_id, deployment_task_id, task_id, status, exit_code, output, created_at, updated_at, idempotency_key_expires_at, idempotency_request_hash, schedule_id, schedule_instance_id, scheduled_at
FROM runs
WHERE org_id = sqlc.arg(org_id)
  AND project_id = sqlc.arg(project_id)
  AND environment_id = sqlc.arg(environment_id)
  AND task_id = sqlc.arg(task_id)
  AND idempotency_key = sqlc.arg(idempotency_key);

-- name: ClearRunIdempotencyKey :exec
UPDATE runs
   SET idempotency_key = NULL,
       idempotency_key_expires_at = NULL,
       idempotency_key_options = '{}'::jsonb,
       idempotency_request_hash = NULL
 WHERE org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND id = sqlc.arg(id);

-- name: ExpireQueuedRuns :exec
WITH eligible AS (
    SELECT runs.id, runs.org_id
      FROM runs
     WHERE runs.org_id = sqlc.arg(org_id)
       AND runs.status = 'queued'
       AND runs.current_execution_id IS NULL
       AND runs.queued_expires_at IS NOT NULL
       AND runs.queued_expires_at <= now()
     FOR UPDATE OF runs
),
expired_runs AS (
    UPDATE runs
       SET status = 'expired',
           error_message = 'run ttl expired before execution started',
           finished_at = now(),
           updated_at = now()
      FROM eligible
     WHERE runs.org_id = eligible.org_id
       AND runs.id = eligible.id
       AND runs.status = 'queued'
    RETURNING runs.id, runs.org_id, runs.ttl
),
completed_queue_entries AS (
    UPDATE run_queue_items
       SET status = 'completed',
           dispatch_generation = dispatch_generation + 1,
           updated_at = now(),
           finished_at = now()
      FROM expired_runs
     WHERE run_queue_items.org_id = expired_runs.org_id
       AND run_queue_items.run_id = expired_runs.id
       AND run_queue_items.status IN ('queued', 'published', 'reserved')
    RETURNING run_queue_items.run_id
)
INSERT INTO run_events (org_id, run_id, kind, payload)
SELECT expired_runs.org_id,
       expired_runs.id,
       'run.expired',
       jsonb_build_object('ttl', expired_runs.ttl, 'message', 'run ttl expired before execution started')
  FROM expired_runs;

-- name: GetRunSummary :one
SELECT id, org_id, project_id, environment_id, deployment_id, deployment_task_id, task_id, status, exit_code, output, created_at, updated_at
FROM runs
WHERE org_id = $1 AND id = $2;

-- name: CountRunsByStatus :one
SELECT count(*) FILTER (WHERE status = 'queued') AS queued,
       count(*) FILTER (WHERE status = 'running') AS running,
       count(*) FILTER (WHERE status = 'waiting') AS waiting,
       count(*) FILTER (WHERE status = 'succeeded') AS succeeded,
       count(*) FILTER (WHERE status = 'failed') AS failed,
       count(*) FILTER (WHERE status = 'cancelled') AS cancelled,
       count(*) FILTER (WHERE status = 'expired') AS expired
FROM runs
WHERE org_id = sqlc.arg(org_id);

-- name: CountScopedRunsByStatus :one
SELECT count(*) FILTER (WHERE status = 'queued') AS queued,
       count(*) FILTER (WHERE status = 'running') AS running,
       count(*) FILTER (WHERE status = 'waiting') AS waiting,
       count(*) FILTER (WHERE status = 'succeeded') AS succeeded,
       count(*) FILTER (WHERE status = 'failed') AS failed,
       count(*) FILTER (WHERE status = 'cancelled') AS cancelled,
       count(*) FILTER (WHERE status = 'expired') AS expired
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
    OR (sqlc.arg(status_filter)::text = 'live' AND status NOT IN ('succeeded', 'failed', 'cancelled', 'expired'))
    OR (sqlc.arg(status_filter)::text = 'running' AND status = 'running')
    OR status::text = sqlc.arg(status_filter)::text
  )
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg(row_limit);

-- name: ListScopedRunSummaries :many
SELECT id, org_id, project_id, environment_id, deployment_id, deployment_task_id, task_id, status, exit_code, output, created_at, updated_at
FROM runs
WHERE org_id = sqlc.arg(org_id)
  AND project_id = sqlc.arg(project_id)
  AND environment_id = sqlc.arg(environment_id)
  AND (
    sqlc.arg(status_filter)::text = 'all'
    OR (sqlc.arg(status_filter)::text = 'live' AND status NOT IN ('succeeded', 'failed', 'cancelled', 'expired'))
    OR (sqlc.arg(status_filter)::text = 'running' AND status = 'running')
    OR status::text = sqlc.arg(status_filter)::text
  )
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg(row_limit);
