-- name: CreateRootRunFromCurrentDeployment :one
WITH selected_target AS MATERIALIZED (
    SELECT definitions.environment_id,
           definitions.deployment_id,
           definitions.id AS deployment_definition_id,
           definitions.declared_id AS entrypoint_declared_id,
           workspaces.org_id,
           workspaces.project_id,
           workspaces.id AS workspace_id
      FROM environments
      JOIN deployment_definitions AS definitions
        ON definitions.environment_id = environments.id
       AND definitions.deployment_id = environments.current_deployment_id
       AND definitions.kind = 'task'
       AND definitions.declared_id = sqlc.arg(entrypoint_declared_id)
      JOIN deployments
        ON deployments.environment_id = definitions.environment_id
       AND deployments.id = definitions.deployment_id
       AND deployments.status = 'deployed'
      JOIN workspaces
        ON workspaces.environment_id = environments.id
       AND workspaces.id = sqlc.arg(workspace_id)
       AND workspaces.org_id = sqlc.arg(org_id)
       AND workspaces.project_id = sqlc.arg(project_id)
      JOIN workspace_versions
        ON workspace_versions.workspace_id = workspaces.id
       AND workspace_versions.id = sqlc.arg(base_workspace_version_id)
       AND workspace_versions.state = 'committed'
     WHERE environments.id = sqlc.arg(environment_id)
       AND environments.org_id = sqlc.arg(org_id)
       AND environments.project_id = sqlc.arg(project_id)
       AND environments.current_deployment_id IS NOT NULL
       AND (
           sqlc.narg(claim_id)::uuid IS NULL
           OR EXISTS (
               SELECT 1
                 FROM idempotency_claims
                WHERE idempotency_claims.environment_id = environments.id
                  AND idempotency_claims.id = sqlc.narg(claim_id)
                  AND idempotency_claims.operation = 'task.start'
                  AND idempotency_claims.state = 'pending'
                  AND idempotency_claims.retired_at IS NULL
           )
       )
     FOR UPDATE OF environments
), created_run AS (
    INSERT INTO runs (
        id,
        public_id,
        org_id,
        project_id,
        environment_id,
        deployment_id,
        deployment_definition_id,
        entrypoint_kind,
        entrypoint_declared_id,
        cause_kind,
        workspace_id,
        base_workspace_version_id,
        payload,
        metadata,
        tags,
        queue_name,
        concurrency_key,
        queue_concurrency_limit,
        priority,
        queue_origin_at,
        queue_score_at,
        queued_expires_at,
        max_active_duration_ms,
        retry_policy,
        trace_id,
        root_span_id,
        claim_id
    )
    SELECT sqlc.arg(id),
           sqlc.arg(public_id),
           selected_target.org_id,
           selected_target.project_id,
           selected_target.environment_id,
           selected_target.deployment_id,
           selected_target.deployment_definition_id,
           'task',
           selected_target.entrypoint_declared_id,
           sqlc.arg(cause_kind),
           selected_target.workspace_id,
           sqlc.arg(base_workspace_version_id),
           sqlc.narg(payload),
           coalesce(sqlc.narg(metadata)::jsonb, '{}'::jsonb),
           coalesce(sqlc.narg(tags)::text[], '{}'::text[]),
           sqlc.arg(queue_name),
           sqlc.narg(concurrency_key),
           sqlc.narg(queue_concurrency_limit),
           sqlc.arg(priority),
           sqlc.arg(queue_origin_at),
           sqlc.arg(queue_score_at),
           sqlc.narg(queued_expires_at),
           sqlc.arg(max_active_duration_ms),
           sqlc.arg(retry_policy),
           sqlc.narg(trace_id),
           sqlc.arg(root_span_id),
           sqlc.narg(claim_id)
      FROM selected_target
    RETURNING runs.*
), created_attempt AS (
    INSERT INTO run_attempts (
        run_id,
        number,
        entrypoint_kind,
        workspace_id,
        base_workspace_version_id
    )
    SELECT created_run.id,
           1,
           created_run.entrypoint_kind,
           created_run.workspace_id,
           created_run.base_workspace_version_id
      FROM created_run
    RETURNING run_id
)
SELECT created_run.*
  FROM created_run
  JOIN created_attempt ON created_attempt.run_id = created_run.id;

-- name: CreateAdmittedRootTaskRun :one
WITH created_run AS (
    INSERT INTO runs (
        id,
        public_id,
        org_id,
        project_id,
        environment_id,
        deployment_id,
        deployment_definition_id,
        entrypoint_kind,
        entrypoint_declared_id,
        cause_kind,
        schedule_id,
        schedule_generation,
        scheduled_at,
        previous_scheduled_at,
        schedule_timezone,
        workspace_id,
        base_workspace_version_id,
        payload,
        metadata,
        tags,
        queue_name,
        concurrency_key,
        queue_concurrency_limit,
        priority,
        queue_origin_at,
        queue_score_at,
        queued_expires_at,
        max_active_duration_ms,
        retry_policy,
        trace_id,
        root_span_id,
        claim_id
    )
    VALUES (
        sqlc.arg(id),
        sqlc.arg(public_id),
        sqlc.arg(org_id),
        sqlc.arg(project_id),
        sqlc.arg(environment_id),
        sqlc.arg(deployment_id),
        sqlc.arg(deployment_definition_id),
        'task',
        sqlc.arg(entrypoint_declared_id),
        sqlc.arg(cause_kind),
        sqlc.narg(schedule_id),
        sqlc.narg(schedule_generation),
        sqlc.narg(scheduled_at),
        sqlc.narg(previous_scheduled_at),
        sqlc.narg(schedule_timezone),
        sqlc.arg(workspace_id),
        sqlc.arg(base_workspace_version_id),
        sqlc.narg(payload),
        coalesce(sqlc.narg(metadata)::jsonb, '{}'::jsonb),
        coalesce(sqlc.narg(tags)::text[], '{}'::text[]),
        sqlc.arg(queue_name),
        sqlc.narg(concurrency_key),
        sqlc.narg(queue_concurrency_limit),
        sqlc.arg(priority)::integer,
        now(),
        now() - (sqlc.arg(priority)::double precision * interval '1 second'),
        CASE
            WHEN sqlc.narg(queued_ttl_ms)::bigint IS NULL THEN NULL
            ELSE now() + (sqlc.narg(queued_ttl_ms)::bigint::double precision * interval '1 millisecond')
        END,
        sqlc.arg(max_active_duration_ms),
        sqlc.arg(retry_policy),
        sqlc.narg(trace_id),
        sqlc.arg(root_span_id),
        sqlc.narg(claim_id)
    )
    RETURNING *
), created_attempt AS (
    INSERT INTO run_attempts (
        run_id,
        number,
        entrypoint_kind,
        workspace_id,
        base_workspace_version_id
    )
    SELECT created_run.id,
           1,
           created_run.entrypoint_kind,
           created_run.workspace_id,
           created_run.base_workspace_version_id
      FROM created_run
    RETURNING run_id
)
SELECT created_run.*
  FROM created_run
  JOIN created_attempt ON created_attempt.run_id = created_run.id;

-- name: CreateChildRunFromParentDeployment :one
WITH selected_target AS MATERIALIZED (
    SELECT definitions.environment_id,
           definitions.deployment_id,
           definitions.id AS deployment_definition_id,
           definitions.declared_id AS entrypoint_declared_id,
           parent.org_id,
           parent.project_id,
           parent.id AS parent_run_id,
           workspaces.id AS workspace_id
      FROM runs AS parent
      JOIN deployment_definitions AS definitions
        ON definitions.environment_id = parent.environment_id
       AND definitions.deployment_id = parent.deployment_id
       AND definitions.kind = 'task'
       AND definitions.declared_id = sqlc.arg(entrypoint_declared_id)
      JOIN workspaces
        ON workspaces.environment_id = parent.environment_id
       AND workspaces.id = sqlc.arg(workspace_id)
       AND workspaces.org_id = parent.org_id
       AND workspaces.project_id = parent.project_id
      JOIN workspace_versions
        ON workspace_versions.workspace_id = workspaces.id
       AND workspace_versions.id = sqlc.arg(base_workspace_version_id)
       AND workspace_versions.state = 'committed'
      JOIN idempotency_claims
        ON idempotency_claims.environment_id = parent.environment_id
       AND idempotency_claims.id = sqlc.arg(claim_id)
       AND idempotency_claims.operation = 'task.child.invoke'
       AND idempotency_claims.state = 'pending'
       AND idempotency_claims.retired_at IS NULL
     WHERE parent.environment_id = sqlc.arg(environment_id)
       AND parent.id = sqlc.arg(parent_run_id)
       AND parent.status IN ('queued', 'running', 'waiting', 'retry_delayed', 'cancel_requested')
     FOR UPDATE OF parent
), created_run AS (
    INSERT INTO runs (
        id,
        public_id,
        org_id,
        project_id,
        environment_id,
        deployment_id,
        deployment_definition_id,
        entrypoint_kind,
        entrypoint_declared_id,
        cause_kind,
        parent_run_id,
        parent_owns_lifecycle,
        workspace_id,
        base_workspace_version_id,
        payload,
        metadata,
        tags,
        queue_name,
        concurrency_key,
        queue_concurrency_limit,
        priority,
        queue_origin_at,
        queue_score_at,
        queued_expires_at,
        max_active_duration_ms,
        retry_policy,
        trace_id,
        root_span_id,
        claim_id
    )
    SELECT sqlc.arg(id),
           sqlc.arg(public_id),
           selected_target.org_id,
           selected_target.project_id,
           selected_target.environment_id,
           selected_target.deployment_id,
           selected_target.deployment_definition_id,
           'task',
           selected_target.entrypoint_declared_id,
           'child',
           selected_target.parent_run_id,
           sqlc.arg(parent_owns_lifecycle),
           selected_target.workspace_id,
           sqlc.arg(base_workspace_version_id),
           sqlc.narg(payload),
           coalesce(sqlc.narg(metadata)::jsonb, '{}'::jsonb),
           coalesce(sqlc.narg(tags)::text[], '{}'::text[]),
           sqlc.arg(queue_name),
           sqlc.narg(concurrency_key),
           sqlc.narg(queue_concurrency_limit),
           sqlc.arg(priority),
           sqlc.arg(queue_origin_at),
           sqlc.arg(queue_score_at),
           sqlc.narg(queued_expires_at),
           sqlc.arg(max_active_duration_ms),
           sqlc.arg(retry_policy),
           sqlc.narg(trace_id),
           sqlc.arg(root_span_id),
           sqlc.arg(claim_id)
      FROM selected_target
    RETURNING runs.*
), created_attempt AS (
    INSERT INTO run_attempts (
        run_id,
        number,
        entrypoint_kind,
        workspace_id,
        base_workspace_version_id
    )
    SELECT created_run.id,
           1,
           created_run.entrypoint_kind,
           created_run.workspace_id,
           created_run.base_workspace_version_id
      FROM created_run
    RETURNING run_id
)
SELECT created_run.*
  FROM created_run
  JOIN created_attempt ON created_attempt.run_id = created_run.id;

-- name: GetRun :one
SELECT *
  FROM runs
 WHERE environment_id = sqlc.arg(environment_id)
   AND id = sqlc.arg(id);

-- name: ListQueuedRunsForQueue :many
SELECT *
  FROM runs
 WHERE environment_id = sqlc.arg(environment_id)
   AND queue_name = sqlc.arg(queue_name)
   AND concurrency_key IS NOT DISTINCT FROM sqlc.narg(concurrency_key)::text
   AND status = 'queued'
   AND current_run_lease_id IS NULL
   AND (first_lease_at IS NOT NULL OR queued_expires_at IS NULL OR queued_expires_at > now())
 ORDER BY queue_score_at, id
 LIMIT sqlc.arg(limit_count);

-- name: ExpireInitiallyQueuedRuns :many
UPDATE runs
   SET status = 'expired',
       terminal_at = now(),
       terminal_reason_code = 'queued_ttl_expired',
       state_version = state_version + 1,
       updated_at = now()
 WHERE status = 'queued'
   AND first_lease_at IS NULL
   AND queued_expires_at IS NOT NULL
   AND queued_expires_at <= now()
RETURNING *;

-- name: RequestRunCancellation :one
UPDATE runs
   SET status = 'cancel_requested',
       state_version = state_version + 1,
       updated_at = now()
 WHERE environment_id = sqlc.arg(environment_id)
   AND id = sqlc.arg(id)
   AND status IN ('queued', 'running', 'waiting', 'retry_delayed')
RETURNING *;
