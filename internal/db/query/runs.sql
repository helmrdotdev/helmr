-- name: CreateScopedRun :one
WITH run_scope AS MATERIALIZED (
    SELECT sessions.org_id,
           sessions.worker_group_id,
           sessions.project_id,
           sessions.environment_id,
           sessions.id AS session_id,
           sessions.workspace_id
      FROM sessions
      JOIN workspaces
        ON workspaces.org_id = sessions.org_id
       AND workspaces.project_id = sessions.project_id
       AND workspaces.environment_id = sessions.environment_id
       AND workspaces.id = sessions.workspace_id
       AND workspaces.worker_group_id = sessions.worker_group_id
      JOIN worker_groups
        ON worker_groups.id = sessions.worker_group_id
       AND (
           worker_groups.state = 'active'
           OR (
               sqlc.arg(allow_draining_route)::boolean
               AND worker_groups.state = 'draining'
           )
       )
     WHERE sessions.org_id = sqlc.arg(org_id)
       AND sessions.project_id = sqlc.arg(project_id)
       AND sessions.environment_id = sqlc.arg(environment_id)
       AND sessions.id = sqlc.arg(session_id)
	       AND sessions.workspace_id = sqlc.arg(workspace_id)
	),
deployment_task AS MATERIALIZED (
    SELECT deployment_tasks.*
      FROM deployment_tasks
      JOIN deployments
        ON deployments.org_id = deployment_tasks.org_id
       AND deployments.project_id = deployment_tasks.project_id
       AND deployments.environment_id = deployment_tasks.environment_id
       AND deployments.id = deployment_tasks.deployment_id
       AND deployments.status = 'deployed'
     WHERE deployment_tasks.org_id = sqlc.arg(org_id)
       AND deployment_tasks.project_id = sqlc.arg(project_id)
       AND deployment_tasks.environment_id = sqlc.arg(environment_id)
       AND deployment_tasks.deployment_id = sqlc.arg(deployment_id)
       AND deployment_tasks.id = sqlc.arg(deployment_task_id)
       AND deployment_tasks.task_id = sqlc.arg(task_id)
),
selected_runtime AS MATERIALIZED (
    SELECT runtime_releases.runtime_id,
           runtime_releases.runtime_arch,
           runtime_releases.runtime_abi,
           runtime_releases.kernel_digest,
           runtime_releases.initramfs_digest,
           runtime_releases.rootfs_digest,
           runtime_releases.cni_profile
      FROM runtime_releases
      JOIN runtime_release_selections ON runtime_release_selections.runtime_id = runtime_releases.runtime_id
     LIMIT 1
),
	created AS (
    INSERT INTO runs (
        id,
        public_id,
        org_id,
        worker_group_id,
        project_id,
        environment_id,
        deployment_id,
        deployment_task_id,
        workspace_id,
        deployment_version,
        api_version,
        sdk_version,
        cli_version,
        task_id,
        session_id,
        payload,
        metadata,
        tags,
        locked_retry_policy,
        queue_name,
        queue_concurrency_limit,
        concurrency_key,
        priority,
        queue_timestamp,
	        ttl,
	        queued_expires_at,
	        requested_milli_cpu,
	        requested_memory_mib,
	        requested_disk_mib,
	        requested_execution_slots,
	        runtime_id,
	        runtime_arch,
	        runtime_abi,
	        kernel_digest,
	        initramfs_digest,
	        rootfs_digest,
	        cni_profile,
	        network_policy,
	        placement,
	        max_active_duration_ms,
	        trace_id,
	        root_span_id,
	        current_attempt_number,
	        schedule_id,
        schedule_instance_id,
        scheduled_at
    )
    SELECT sqlc.arg(id),
           sqlc.arg(public_id),
           sqlc.arg(org_id),
           run_scope.worker_group_id,
           sqlc.arg(project_id),
           sqlc.arg(environment_id),
           sqlc.arg(deployment_id),
           sqlc.arg(deployment_task_id),
           sqlc.arg(workspace_id),
           sqlc.arg(deployment_version),
           sqlc.arg(api_version),
           sqlc.arg(sdk_version),
           sqlc.arg(cli_version),
           sqlc.arg(task_id),
           sqlc.arg(session_id),
           sqlc.arg(payload),
           coalesce(sqlc.arg(metadata)::jsonb, '{}'::jsonb),
           coalesce(sqlc.arg(tags)::text[], '{}'::text[]),
           coalesce(sqlc.arg(locked_retry_policy)::jsonb, '{"enabled": false}'::jsonb),
           sqlc.arg(queue_name),
           sqlc.narg(queue_concurrency_limit),
           sqlc.narg(concurrency_key),
           sqlc.arg(priority),
           sqlc.arg(queue_timestamp),
	           sqlc.arg(ttl),
	           sqlc.narg(queued_expires_at),
	           deployment_task.requested_milli_cpu,
	           deployment_task.requested_memory_mib,
	           deployment_task.requested_disk_mib,
	           deployment_task.requested_execution_slots,
	           selected_runtime.runtime_id,
	           selected_runtime.runtime_arch,
	           selected_runtime.runtime_abi,
	           selected_runtime.kernel_digest,
	           selected_runtime.initramfs_digest,
	           selected_runtime.rootfs_digest,
	           selected_runtime.cni_profile,
	           deployment_task.network_policy,
	           deployment_task.placement,
	           sqlc.arg(max_active_duration_ms),
	           sqlc.arg(trace_id),
	           sqlc.arg(root_span_id),
	           1,
	           sqlc.narg(schedule_id),
	           sqlc.narg(schedule_instance_id),
	           sqlc.narg(scheduled_at)
	      FROM run_scope
	      JOIN deployment_task ON true
	      JOIN selected_runtime ON true
	     WHERE (
            sqlc.narg(schedule_instance_id)::uuid IS NULL
            OR EXISTS (
            SELECT 1
              FROM task_schedule_instances
              JOIN task_schedules ON task_schedules.id = task_schedule_instances.schedule_id
             WHERE task_schedule_instances.id = sqlc.narg(schedule_instance_id)
               AND task_schedule_instances.generation = sqlc.narg(schedule_generation)
               AND task_schedule_instances.next_fire_at = sqlc.narg(scheduled_at)
               AND task_schedule_instances.schedule_id = sqlc.narg(schedule_id)
               AND task_schedule_instances.org_id = sqlc.arg(org_id)
               AND task_schedule_instances.project_id = sqlc.arg(project_id)
               AND task_schedule_instances.environment_id = sqlc.arg(environment_id)
               AND task_schedule_instances.enabled
               AND (
                   task_schedule_instances.retry_after IS NULL
                   OR task_schedule_instances.retry_after <= now()
               )
               AND task_schedules.org_id = sqlc.arg(org_id)
               AND task_schedules.project_id = sqlc.arg(project_id)
               AND task_schedules.enabled
        )
	       )
		    RETURNING *
),
created_snapshot AS (
    INSERT INTO run_state_snapshots (org_id, worker_group_id, run_id, version, status, execution_status, attempt_number, operation_id, transition, reason)
    SELECT created.org_id,
           created.worker_group_id,
           created.id,
           created.state_version,
           created.status,
           created.execution_status,
           created.current_attempt_number,
           NULL::uuid,
           'run.created',
           sqlc.arg(event_payload)
      FROM created
    RETURNING run_state_snapshots.run_id
),
created_event AS (
    INSERT INTO telemetry_outbox (
        org_id, worker_group_id, stream_kind, source_kind, source_id, project_id,
        environment_id, run_id, deployment_id, run_lease_id, attempt_number,
        trace_id, span_id, parent_span_id, traceparent, category, severity, source,
        kind, message, payload, redaction_class, snapshot_version, observed_at
    )
    SELECT created.org_id,
           created.worker_group_id,
           'event',
           CASE WHEN NULL::uuid IS NOT NULL THEN 'deployment' ELSE 'run' END,
           COALESCE(NULL::uuid, created.id),
           created.project_id,
           created.environment_id,
           created.id,
           NULL::uuid,
           NULL::uuid,
           created.current_attempt_number,
           created.trace_id,
           created.root_span_id,
           NULL::text,
           '00-' || created.trace_id || '-' || created.root_span_id || '-01',
           COALESCE(NULLIF('lifecycle', ''), 'system'),
           COALESCE(NULLIF('info', ''), 'info'),
           COALESCE(NULLIF('control', ''), 'control'),
           'run.created',
           COALESCE('run.created', ''),
           COALESCE(sqlc.arg(event_payload), '{}'::jsonb),
           COALESCE(NULLIF('internal', ''), 'internal'),
           created.state_version,
           now()
      FROM created
      JOIN created_snapshot ON true
    RETURNING id
)
SELECT created.*
  FROM created
  JOIN created_snapshot ON true
  JOIN created_event ON true;

-- name: GetRun :one
SELECT * FROM runs
WHERE org_id = $1 AND id = $2;

-- name: ExpireQueuedRuns :exec
WITH locked_sessions AS MATERIALIZED (
    SELECT sessions.id
      FROM runs
      JOIN sessions
        ON sessions.org_id = runs.org_id
       AND sessions.project_id = runs.project_id
       AND sessions.environment_id = runs.environment_id
       AND sessions.id = runs.session_id
     WHERE runs.org_id = sqlc.arg(org_id)
       AND runs.worker_group_id = sqlc.arg(worker_group_id)
       AND runs.status = 'queued'
       AND runs.current_run_lease_id IS NULL
       AND runs.queued_expires_at IS NOT NULL
       AND runs.queued_expires_at <= now()
     FOR UPDATE OF sessions
),
eligible AS (
    SELECT runs.id, runs.org_id
      FROM runs
      LEFT JOIN locked_sessions
        ON locked_sessions.id = runs.session_id
     WHERE runs.org_id = sqlc.arg(org_id)
       AND runs.worker_group_id = sqlc.arg(worker_group_id)
       AND runs.status = 'queued'
       AND runs.current_run_lease_id IS NULL
       AND runs.queued_expires_at IS NOT NULL
       AND runs.queued_expires_at <= now()
       AND locked_sessions.id = runs.session_id
     FOR UPDATE OF runs
),
expired_runs AS (
    UPDATE runs
	       SET status = 'expired',
	           execution_status = 'finished',
	           terminal_outcome = 'expired',
	           error_message = 'run ttl expired before execution started',
	           dispatch_generation = dispatch_generation + 1,
	           state_version = state_version + 1,
	           finished_at = now(),
	           updated_at = now()
      FROM eligible
     WHERE runs.org_id = eligible.org_id
       AND runs.id = eligible.id
       AND runs.status = 'queued'
	    RETURNING runs.id, runs.org_id, runs.worker_group_id, runs.project_id, runs.environment_id, runs.session_id, runs.current_attempt_number, runs.trace_id, runs.root_span_id, runs.state_version, runs.ttl
),
expired_session_runs AS (
    UPDATE session_runs
       SET ended_at = now()
      FROM expired_runs
     WHERE session_runs.org_id = expired_runs.org_id
       AND session_runs.project_id = expired_runs.project_id
       AND session_runs.environment_id = expired_runs.environment_id
       AND session_runs.session_id = expired_runs.session_id
       AND session_runs.run_id = expired_runs.id
    RETURNING session_runs.id
),
expired_sessions AS (
    SELECT expired_runs.session_id AS id
      FROM expired_runs
),
expired_snapshots AS (
    INSERT INTO run_state_snapshots (org_id, worker_group_id, run_id, version, status, execution_status, terminal_outcome, attempt_number, transition, reason, error)
    SELECT expired_runs.org_id,
           expired_runs.worker_group_id,
           expired_runs.id,
           expired_runs.state_version,
           'expired',
           'finished',
           'expired',
           expired_runs.current_attempt_number,
           'run.expired',
           jsonb_build_object('ttl', expired_runs.ttl, 'message', 'run ttl expired before execution started'),
           jsonb_build_object('message', 'run ttl expired before execution started')
      FROM expired_runs
    RETURNING run_state_snapshots.run_id
),
expired_event AS (
    INSERT INTO telemetry_outbox (
        org_id, worker_group_id, stream_kind, source_kind, source_id, project_id,
        environment_id, run_id, deployment_id, run_lease_id, attempt_number,
        trace_id, span_id, parent_span_id, traceparent, category, severity, source,
        kind, message, payload, redaction_class, snapshot_version, observed_at
    )
    SELECT expired_runs.org_id,
           expired_runs.worker_group_id,
           'event',
           CASE WHEN NULL::uuid IS NOT NULL THEN 'deployment' ELSE 'run' END,
           COALESCE(NULL::uuid, expired_runs.id),
           expired_runs.project_id,
           expired_runs.environment_id,
           expired_runs.id,
           NULL::uuid,
           NULL::uuid,
           expired_runs.current_attempt_number,
           expired_runs.trace_id,
           expired_runs.root_span_id,
           NULL::text,
           '00-' || expired_runs.trace_id || '-' || expired_runs.root_span_id || '-01',
           COALESCE(NULLIF('lifecycle', ''), 'system'),
           COALESCE(NULLIF('warn', ''), 'info'),
           COALESCE(NULLIF('control', ''), 'control'),
           'run.expired',
           COALESCE('run.expired', ''),
           COALESCE(jsonb_build_object('ttl', expired_runs.ttl, 'message', 'run ttl expired before execution started'), '{}'::jsonb),
           COALESCE(NULLIF('internal', ''), 'internal'),
           expired_runs.state_version,
           now()
      FROM expired_runs
      JOIN expired_snapshots ON expired_snapshots.run_id = expired_runs.id
    RETURNING id
)
SELECT expired_event.*
  FROM expired_event;

-- name: SetQueuedRunWorkspaceMount :exec
UPDATE runs
   SET workspace_mount_id = sqlc.arg(workspace_mount_id),
       updated_at = now()
 WHERE runs.org_id = sqlc.arg(org_id)
   AND runs.id = sqlc.arg(run_id)
   AND runs.workspace_id = sqlc.arg(workspace_id)
   AND runs.status = 'queued'
   AND runs.current_run_lease_id IS NULL;

-- name: ListQueuedRunsForWorkspaceMount :many
SELECT runs.id
  FROM runs
 WHERE runs.org_id = sqlc.arg(org_id)
   AND runs.workspace_id = sqlc.arg(workspace_id)
   AND runs.workspace_mount_id = sqlc.arg(workspace_mount_id)
   AND runs.status = 'queued'
   AND runs.current_run_lease_id IS NULL
 ORDER BY runs.queue_timestamp ASC, runs.id ASC;

-- name: FailQueuedRun :exec
WITH locked_session AS MATERIALIZED (
    SELECT sessions.id
      FROM runs
      JOIN sessions
        ON sessions.org_id = runs.org_id
       AND sessions.project_id = runs.project_id
       AND sessions.environment_id = runs.environment_id
       AND sessions.id = runs.session_id
     WHERE runs.org_id = sqlc.arg(org_id)
       AND runs.id = sqlc.arg(run_id)
       AND runs.status = 'queued'
       AND runs.current_run_lease_id IS NULL
     FOR UPDATE OF sessions
),
target AS (
    SELECT runs.*
      FROM runs
      JOIN locked_session ON locked_session.id = runs.session_id
     WHERE runs.org_id = sqlc.arg(org_id)
       AND runs.id = sqlc.arg(run_id)
       AND runs.status = 'queued'
       AND runs.current_run_lease_id IS NULL
     FOR UPDATE OF runs
),
failed_run AS (
    UPDATE runs
       SET status = 'failed',
	           execution_status = 'finished',
	           terminal_outcome = 'failed',
	           error_message = COALESCE(NULLIF(sqlc.arg(error_message)::text, ''), 'run failed before execution started'),
	           dispatch_generation = dispatch_generation + 1,
	           state_version = runs.state_version + 1,
	           finished_at = now(),
	           updated_at = now()
      FROM target
     WHERE runs.org_id = target.org_id
       AND runs.id = target.id
       AND runs.status = 'queued'
       AND runs.current_run_lease_id IS NULL
	    RETURNING runs.id, runs.org_id, runs.worker_group_id, runs.project_id, runs.environment_id, runs.session_id, runs.current_attempt_number, runs.trace_id, runs.root_span_id, runs.state_version, runs.error_message
),
failed_session_run AS (
    UPDATE session_runs
       SET ended_at = now()
      FROM failed_run
     WHERE session_runs.org_id = failed_run.org_id
       AND session_runs.project_id = failed_run.project_id
       AND session_runs.environment_id = failed_run.environment_id
       AND session_runs.session_id = failed_run.session_id
       AND session_runs.run_id = failed_run.id
    RETURNING session_runs.id
),
failed_session AS (
    SELECT failed_run.session_id AS id
      FROM failed_run
),
failed_snapshot AS (
    INSERT INTO run_state_snapshots (org_id, worker_group_id, run_id, version, status, execution_status, terminal_outcome, attempt_number, transition, reason, error)
    SELECT failed_run.org_id,
           failed_run.worker_group_id,
           failed_run.id,
           failed_run.state_version,
           'failed',
           'finished',
           'failed',
           failed_run.current_attempt_number,
           'run.failed',
           COALESCE(sqlc.arg(reason)::jsonb, '{}'::jsonb),
           COALESCE(sqlc.arg(reason)::jsonb, '{}'::jsonb)
      FROM failed_run
    RETURNING run_state_snapshots.run_id
),
failed_event AS (
    INSERT INTO telemetry_outbox (
        org_id, worker_group_id, stream_kind, source_kind, source_id, project_id,
        environment_id, run_id, deployment_id, run_lease_id, attempt_number,
        trace_id, span_id, parent_span_id, traceparent, category, severity, source,
        kind, message, payload, redaction_class, snapshot_version, observed_at
    )
    SELECT failed_run.org_id,
           failed_run.worker_group_id,
           'event',
           CASE WHEN NULL::uuid IS NOT NULL THEN 'deployment' ELSE 'run' END,
           COALESCE(NULL::uuid, failed_run.id),
           failed_run.project_id,
           failed_run.environment_id,
           failed_run.id,
           NULL::uuid,
           NULL::uuid,
           failed_run.current_attempt_number,
           failed_run.trace_id,
           failed_run.root_span_id,
           NULL::text,
           '00-' || failed_run.trace_id || '-' || failed_run.root_span_id || '-01',
           COALESCE(NULLIF('lifecycle', ''), 'system'),
           COALESCE(NULLIF('error', ''), 'info'),
           COALESCE(NULLIF('control', ''), 'control'),
           'run.failed',
           COALESCE('run.failed', ''),
           COALESCE(COALESCE(sqlc.arg(reason)::jsonb, '{}'::jsonb), '{}'::jsonb),
           COALESCE(NULLIF('internal', ''), 'internal'),
           failed_run.state_version,
           now()
      FROM failed_run
      JOIN failed_snapshot ON failed_snapshot.run_id = failed_run.id
    RETURNING id
)
SELECT failed_event.*
  FROM failed_event;

-- name: GetRunSummary :one
SELECT * FROM runs
WHERE org_id = $1 AND id = $2;

-- name: CountScopedRunsByStatus :one
SELECT count(*) FILTER (WHERE status = 'queued') AS queued,
       count(*) FILTER (WHERE status = 'running') AS running,
       count(*) FILTER (WHERE status = 'waiting') AS waiting,
       count(*) FILTER (WHERE status = 'succeeded') AS succeeded,
       count(*) FILTER (WHERE status = 'failed') AS failed,
       count(*) FILTER (WHERE status = 'cancelled') AS cancelled,
       count(*) FILTER (WHERE status = 'expired') AS expired
FROM runs
WHERE runs.org_id = sqlc.arg(org_id)
  AND runs.project_id = sqlc.arg(project_id)
  AND runs.environment_id = sqlc.arg(environment_id);

-- name: ListScopedRunSummaries :many
SELECT runs.*
FROM runs
WHERE runs.org_id = sqlc.arg(org_id)
  AND runs.project_id = sqlc.arg(project_id)
  AND runs.environment_id = sqlc.arg(environment_id)
  AND (
    sqlc.arg(status_filter)::text = 'all'
    OR (sqlc.arg(status_filter)::text = 'live' AND runs.status NOT IN ('succeeded', 'failed', 'cancelled', 'expired'))
    OR (sqlc.arg(status_filter)::text = 'running' AND runs.status = 'running')
    OR runs.status::text = sqlc.arg(status_filter)::text
  )
  AND (
    sqlc.narg(session_id)::uuid IS NULL
    OR runs.session_id = sqlc.narg(session_id)::uuid
  )
ORDER BY runs.created_at DESC, runs.id DESC
LIMIT sqlc.arg(row_limit);

-- name: CreateRunOperation :one
INSERT INTO run_operations (
    id,
    public_id,
    org_id,
    worker_group_id,
    project_id,
    environment_id,
    run_id,
    kind,
    actor_kind,
    actor_id,
    api_key_id,
    reason,
    request,
    idempotency_key
) VALUES (
    sqlc.arg(id),
    sqlc.arg(public_id),
    sqlc.arg(org_id),
    sqlc.arg(worker_group_id),
    sqlc.arg(project_id),
    sqlc.arg(environment_id),
    sqlc.arg(run_id),
    sqlc.arg(kind),
    sqlc.arg(actor_kind),
    sqlc.arg(actor_id),
    sqlc.narg(api_key_id),
    sqlc.arg(reason),
    coalesce(sqlc.arg(request)::jsonb, '{}'::jsonb),
    sqlc.arg(idempotency_key)
)
ON CONFLICT (org_id, project_id, environment_id, run_id, kind, idempotency_key)
WHERE idempotency_key <> ''
DO UPDATE
   SET request = run_operations.request
RETURNING *;

-- name: GetRunOperation :one
SELECT *
  FROM run_operations
 WHERE org_id = sqlc.arg(org_id)
   AND id = sqlc.arg(id);

-- name: MarkRunOperationApplied :one
UPDATE run_operations
   SET status = 'applied',
       result = coalesce(sqlc.arg(result)::jsonb, '{}'::jsonb),
       applied_at = now()
 WHERE id = sqlc.arg(id)
   AND org_id = sqlc.arg(org_id)
   AND status = 'requested'
RETURNING *;

-- name: MarkRunOperationRejected :one
UPDATE run_operations
   SET status = 'rejected',
       result = coalesce(sqlc.arg(result)::jsonb, '{}'::jsonb),
       rejected_at = now()
 WHERE id = sqlc.arg(id)
   AND org_id = sqlc.arg(org_id)
   AND status = 'requested'
RETURNING *;

-- name: CancelRun :one
WITH operation AS (
    SELECT *
      FROM run_operations
     WHERE run_operations.org_id = sqlc.arg(org_id)
       AND run_operations.run_id = sqlc.arg(run_id)
       AND run_operations.id = sqlc.arg(operation_id)
       AND run_operations.kind = 'cancel'
       AND run_operations.status = 'requested'
     FOR UPDATE
),
locked_session AS MATERIALIZED (
    SELECT sessions.id
      FROM runs
      JOIN operation ON operation.org_id = runs.org_id
                    AND operation.run_id = runs.id
      JOIN sessions
        ON sessions.org_id = runs.org_id
       AND sessions.project_id = runs.project_id
       AND sessions.environment_id = runs.environment_id
       AND sessions.id = runs.session_id
     WHERE runs.org_id = sqlc.arg(org_id)
       AND runs.worker_group_id = sqlc.arg(worker_group_id)
       AND runs.id = sqlc.arg(run_id)
     FOR UPDATE OF sessions
),
target AS (
    SELECT runs.*
      FROM runs
      JOIN operation ON operation.org_id = runs.org_id
                    AND operation.run_id = runs.id
      LEFT JOIN locked_session
        ON locked_session.id = runs.session_id
     WHERE runs.org_id = sqlc.arg(org_id)
       AND runs.worker_group_id = sqlc.arg(worker_group_id)
       AND runs.id = sqlc.arg(run_id)
       AND locked_session.id = runs.session_id
     FOR UPDATE
),
updated AS (
    UPDATE runs
       SET status = 'cancelled',
           execution_status = CASE
             WHEN target.execution_status = 'executing' AND NOT sqlc.arg(force)::bool THEN 'pending_cancel'::run_execution_status
             ELSE 'finished'::run_execution_status
           END,
           terminal_outcome = 'cancelled',
           current_run_lease_id = CASE
             WHEN target.execution_status = 'executing' AND NOT sqlc.arg(force)::bool THEN runs.current_run_lease_id
             ELSE NULL
           END,
           error_message = COALESCE(NULLIF(sqlc.arg(reason)::text, ''), 'run cancelled'),
           dispatch_generation = CASE
             WHEN target.execution_status = 'executing' AND NOT sqlc.arg(force)::bool THEN runs.dispatch_generation
             ELSE runs.dispatch_generation + 1
           END,
           state_version = runs.state_version + 1,
           active_elapsed_ms = CASE
             WHEN target.execution_status = 'executing' AND NOT sqlc.arg(force)::bool THEN runs.active_elapsed_ms
             ELSE LEAST(
               runs.active_elapsed_ms
               + CASE
                   WHEN runs.active_started_at IS NULL THEN 0
                   ELSE GREATEST(floor(extract(epoch from (now() - runs.active_started_at)) * 1000)::bigint, 0)
                 END,
               runs.max_active_duration_ms
             )
           END,
           active_started_at = CASE
             WHEN target.execution_status = 'executing' AND NOT sqlc.arg(force)::bool THEN runs.active_started_at
             ELSE NULL
           END,
           finished_at = CASE
             WHEN target.execution_status = 'executing' AND NOT sqlc.arg(force)::bool THEN runs.finished_at
             ELSE COALESCE(runs.finished_at, now())
           END,
           updated_at = now()
      FROM target
     WHERE runs.org_id = target.org_id
       AND runs.id = target.id
       AND (
           target.status NOT IN ('succeeded', 'failed', 'cancelled', 'expired')
           OR (
               target.status = 'cancelled'
               AND target.execution_status = 'pending_cancel'
               AND sqlc.arg(force)::bool
           )
       )
    RETURNING runs.*, target.current_run_lease_id AS previous_run_lease_id
),
cancelled_run_waits AS (
    UPDATE run_waits
       SET state = 'cancelled',
           cancelled_at = now(),
           updated_at = now()
      FROM updated
     WHERE run_waits.org_id = updated.org_id
       AND run_waits.run_id = updated.id
       AND run_waits.state IN ('hot_waiting', 'checkpointing', 'checkpointed_waiting', 'resuming')
    RETURNING run_waits.org_id, run_waits.wait_id
),
cancelled_waits AS (
    UPDATE waits
       SET state = 'cancelled',
           completed_at = COALESCE(waits.completed_at, now()),
           updated_at = now()
      FROM cancelled_run_waits
     WHERE waits.org_id = cancelled_run_waits.org_id
       AND waits.id = cancelled_run_waits.wait_id
       AND waits.state = 'pending'
    RETURNING waits.id
),
terminal_session_runs AS (
    UPDATE session_runs
       SET ended_at = now()
      FROM updated
     WHERE (updated.execution_status <> 'pending_cancel' OR sqlc.arg(force)::bool)
       AND session_runs.org_id = updated.org_id
       AND session_runs.project_id = updated.project_id
       AND session_runs.environment_id = updated.environment_id
       AND session_runs.session_id = updated.session_id
       AND session_runs.run_id = updated.id
    RETURNING session_runs.id
),
terminal_sessions AS (
    SELECT updated.session_id AS id
      FROM updated
     WHERE (updated.execution_status <> 'pending_cancel' OR sqlc.arg(force)::bool)
),
cancelled_run_lease AS (
    UPDATE run_leases
       SET status = CASE WHEN updated.execution_status <> 'pending_cancel' OR sqlc.arg(force)::bool THEN 'cancelled'::run_lease_status ELSE run_leases.status END,
           released_at = CASE WHEN updated.execution_status <> 'pending_cancel' OR sqlc.arg(force)::bool THEN COALESCE(run_leases.released_at, now()) ELSE run_leases.released_at END,
           renewed_at = now(),
           active_duration_ms = CASE WHEN updated.execution_status <> 'pending_cancel' OR sqlc.arg(force)::bool THEN updated.active_elapsed_ms ELSE run_leases.active_duration_ms END
      FROM updated
     WHERE run_leases.org_id = updated.org_id
       AND run_leases.run_id = updated.id
       AND run_leases.id = updated.previous_run_lease_id
       AND run_leases.status IN ('leased', 'running')
    RETURNING run_leases.*
),
released_workspace_leases AS (
    UPDATE workspace_leases
       SET state = 'released',
           released_at = COALESCE(released_at, now()),
           renewed_at = now(),
           updated_at = now()
      FROM updated
     WHERE (updated.execution_status <> 'pending_cancel' OR sqlc.arg(force)::bool)
       AND workspace_leases.org_id = updated.org_id
       AND workspace_leases.owner_run_id = updated.id
       AND workspace_leases.lease_kind = 'write'
       AND workspace_leases.state = 'active'
       AND workspace_leases.released_at IS NULL
    RETURNING workspace_leases.id
),
invalidated_runtime_checkpoints AS (
    UPDATE runtime_checkpoints
       SET state = 'invalid',
           error_message = COALESCE(NULLIF(sqlc.arg(reason)::text, ''), 'run cancelled'),
           invalidated_at = now()
      FROM updated
     WHERE (updated.execution_status <> 'pending_cancel' OR sqlc.arg(force)::bool)
       AND runtime_checkpoints.org_id = updated.org_id
       AND runtime_checkpoints.run_id = updated.id
       AND runtime_checkpoints.state = 'creating'
    RETURNING runtime_checkpoints.id
),
failed_runtime_checkpoint_restores AS (
    UPDATE runtime_checkpoint_restores
       SET status = 'failed',
           error_message = COALESCE(NULLIF(sqlc.arg(reason)::text, ''), 'run cancelled'),
           finished_at = COALESCE(runtime_checkpoint_restores.finished_at, now()),
           updated_at = now()
      FROM updated
      JOIN cancelled_run_lease ON cancelled_run_lease.org_id = updated.org_id
                              AND cancelled_run_lease.run_id = updated.id
                              AND cancelled_run_lease.id = updated.previous_run_lease_id
     WHERE (updated.execution_status <> 'pending_cancel' OR sqlc.arg(force)::bool)
       AND runtime_checkpoint_restores.org_id = updated.org_id
       AND runtime_checkpoint_restores.run_id = updated.id
       AND runtime_checkpoint_restores.run_lease_id = cancelled_run_lease.id
       AND runtime_checkpoint_restores.runtime_checkpoint_id = cancelled_run_lease.restore_runtime_checkpoint_id
       AND runtime_checkpoint_restores.status = 'restoring'
    RETURNING runtime_checkpoint_restores.id
),
active_time_delta AS (
    SELECT GREATEST(
               cancelled_run_lease.active_duration_ms
               - COALESCE((
                   SELECT SUM(meter_events.quantity)::bigint
                     FROM meter_events
                    WHERE meter_events.org_id = updated.org_id
                      AND meter_events.run_id = updated.id
                      AND meter_events.meter = 'active_time'
               ), 0),
               0
           )::bigint AS quantity
      FROM updated
      JOIN cancelled_run_lease ON cancelled_run_lease.org_id = updated.org_id
                              AND cancelled_run_lease.run_id = updated.id
                              AND cancelled_run_lease.id = updated.previous_run_lease_id
     WHERE updated.execution_status <> 'pending_cancel' OR sqlc.arg(force)::bool
),
active_time_meter_event AS (
    INSERT INTO meter_events (org_id, worker_group_id, project_id, environment_id, source_type, source_id, run_id, attempt_number, trace_id, span_id, meter, quantity, unit, measured_to, details, idempotency_key)
    SELECT updated.org_id,
           updated.worker_group_id,
           updated.project_id,
           updated.environment_id,
           'run_lease',
           cancelled_run_lease.id,
           updated.id,
           cancelled_run_lease.attempt_number,
           cancelled_run_lease.trace_id,
           cancelled_run_lease.span_id,
           'active_time',
           active_time_delta.quantity,
           'ms',
           now(),
           jsonb_build_object('phase', 'cancelled', 'force', sqlc.arg(force)::bool),
           'active_time:' || cancelled_run_lease.id::text || ':cancelled'
      FROM updated
      JOIN cancelled_run_lease ON cancelled_run_lease.org_id = updated.org_id
                              AND cancelled_run_lease.run_id = updated.id
                              AND cancelled_run_lease.id = updated.previous_run_lease_id
     JOIN active_time_delta ON true
    WHERE active_time_delta.quantity > 0
    ON CONFLICT DO NOTHING
    RETURNING *
),
active_time_meter_event_outbox AS (
    INSERT INTO telemetry_outbox (
        org_id, worker_group_id, stream_kind, source_kind, source_id, project_id,
        environment_id, run_id, attempt_number, trace_id, span_id, kind, payload,
        idempotency_key, observed_at
    )
    SELECT active_time_meter_event.org_id,
           active_time_meter_event.worker_group_id,
           'meter_event',
           active_time_meter_event.source_type,
           active_time_meter_event.source_id,
           active_time_meter_event.project_id,
           active_time_meter_event.environment_id,
           active_time_meter_event.run_id,
           active_time_meter_event.attempt_number,
           active_time_meter_event.trace_id,
           active_time_meter_event.span_id,
           active_time_meter_event.meter,
           active_time_meter_event.details,
           active_time_meter_event.idempotency_key,
           active_time_meter_event.occurred_at
      FROM active_time_meter_event
    ON CONFLICT DO NOTHING
    RETURNING id
),
snapshot AS (
    INSERT INTO run_state_snapshots (org_id, worker_group_id, run_id, version, status, execution_status, terminal_outcome, attempt_number, run_lease_id, operation_id, previous_version, transition, reason)
    SELECT updated.org_id,
           updated.worker_group_id,
           updated.id,
           updated.state_version,
           updated.status,
           updated.execution_status,
           updated.terminal_outcome,
           updated.current_attempt_number,
           updated.previous_run_lease_id,
           sqlc.arg(operation_id),
           updated.state_version - 1,
           CASE WHEN updated.execution_status = 'pending_cancel' THEN 'run.cancel_requested' ELSE 'run.cancelled' END,
           jsonb_build_object(
               'reason', COALESCE(NULLIF(sqlc.arg(reason)::text, ''), 'run cancelled'),
               'force', sqlc.arg(force)::bool
           )
      FROM updated
    RETURNING run_state_snapshots.run_id
),
event AS (
    INSERT INTO telemetry_outbox (
        org_id, worker_group_id, stream_kind, source_kind, source_id, project_id,
        environment_id, run_id, deployment_id, run_lease_id, attempt_number,
        trace_id, span_id, parent_span_id, traceparent, category, severity, source,
        kind, message, payload, redaction_class, snapshot_version, observed_at
    )
    SELECT updated.org_id,
           updated.worker_group_id,
           'event',
           CASE WHEN NULL::uuid IS NOT NULL THEN 'deployment' ELSE 'run' END,
           COALESCE(NULL::uuid, updated.id),
           updated.project_id,
           updated.environment_id,
           updated.id,
           NULL::uuid,
           updated.previous_run_lease_id,
           updated.current_attempt_number,
           updated.trace_id,
           updated.root_span_id,
           NULL::text,
           '00-' || updated.trace_id || '-' || updated.root_span_id || '-01',
           COALESCE(NULLIF('lifecycle', ''), 'system'),
           COALESCE(NULLIF('warn', ''), 'info'),
           COALESCE(NULLIF('control', ''), 'control'),
           CASE WHEN updated.execution_status = 'pending_cancel' THEN 'run.cancel_requested' ELSE 'run.cancelled' END,
           COALESCE(CASE WHEN updated.execution_status = 'pending_cancel' THEN 'run.cancel_requested' ELSE 'run.cancelled' END, ''),
           COALESCE(jsonb_build_object(
              'reason', COALESCE(NULLIF(sqlc.arg(reason)::text, ''), 'run cancelled'),
              'force', sqlc.arg(force)::bool
          ), '{}'::jsonb),
           COALESCE(NULLIF('internal', ''), 'internal'),
           updated.state_version,
           now()
      FROM updated
      JOIN snapshot ON true
    RETURNING id
),
operation_applied AS (
    UPDATE run_operations
       SET status = CASE WHEN EXISTS (SELECT 1 FROM updated) THEN 'applied'::run_operation_status ELSE 'rejected'::run_operation_status END,
           result = CASE
             WHEN EXISTS (SELECT 1 FROM updated)
             THEN jsonb_build_object('run_id', sqlc.arg(run_id)::uuid, 'status', 'cancelled')
             ELSE jsonb_build_object('run_id', sqlc.arg(run_id)::uuid, 'status', (SELECT status FROM target)::text, 'reason', 'run is already terminal')
           END,
           applied_at = CASE WHEN EXISTS (SELECT 1 FROM updated) THEN now() ELSE run_operations.applied_at END,
           rejected_at = CASE WHEN EXISTS (SELECT 1 FROM updated) THEN run_operations.rejected_at ELSE now() END
      FROM operation
     WHERE run_operations.id = operation.id
       AND run_operations.org_id = operation.org_id
       AND run_operations.status = 'requested'
    RETURNING run_operations.id
)
SELECT updated.*
  FROM updated
  JOIN operation_applied ON true
  JOIN event ON true
 WHERE (SELECT count(*) FROM cancelled_run_waits) >= 0
   AND (SELECT count(*) FROM terminal_session_runs) >= 0
   AND (SELECT count(*) FROM terminal_sessions) >= 0
   AND (SELECT count(*) FROM cancelled_run_lease) >= 0
   AND (SELECT count(*) FROM released_workspace_leases) >= 0
   AND (SELECT count(*) FROM invalidated_runtime_checkpoints) >= 0
   AND (SELECT count(*) FROM failed_runtime_checkpoint_restores) >= 0
   AND (SELECT count(*) FROM active_time_meter_event_outbox) >= 0
UNION ALL
SELECT target.*, NULL::uuid AS previous_run_lease_id
  FROM target
  JOIN operation_applied ON true
 WHERE NOT EXISTS (SELECT 1 FROM updated);

-- name: UpdateRunMetadataForExecution :one
WITH current_run_lease AS (
    SELECT runs.id,
           runs.org_id,
           runs.project_id,
           runs.environment_id,
	           runs.trace_id,
	           runs.state_version,
	           run_leases.id AS run_lease_id,
	           run_leases.span_id,
	           run_leases.parent_span_id,
	           run_leases.traceparent,
	           run_leases.attempt_number
	      FROM runs
	      JOIN run_leases ON run_leases.id = runs.current_run_lease_id
	                          AND run_leases.org_id = runs.org_id
	                          AND run_leases.run_id = runs.id
     WHERE runs.org_id = sqlc.arg(org_id)
       AND runs.id = sqlc.arg(run_id)
       AND runs.current_run_lease_id = sqlc.arg(run_lease_id)
       AND runs.status = 'running'
       AND run_leases.status IN ('leased', 'running')
       AND run_leases.lease_expires_at > now()
),
updated AS (
    UPDATE runs
       SET metadata = CASE sqlc.arg(operation)::text
             WHEN 'set' THEN jsonb_set(
                 COALESCE(runs.metadata, '{}'::jsonb),
                 ARRAY[sqlc.arg(key)::text],
                 sqlc.arg(value)::jsonb,
                 true
               )
             WHEN 'patch' THEN COALESCE(runs.metadata, '{}'::jsonb) || sqlc.arg(patch)::jsonb
             WHEN 'increment' THEN jsonb_set(
                 COALESCE(runs.metadata, '{}'::jsonb),
                 ARRAY[sqlc.arg(key)::text],
                 to_jsonb(COALESCE((runs.metadata ->> sqlc.arg(key)::text)::numeric, 0) + sqlc.arg(amount)::numeric),
                 true
               )
             ELSE runs.metadata
           END,
           updated_at = now()
      FROM current_run_lease
     WHERE runs.org_id = current_run_lease.org_id
       AND runs.id = current_run_lease.id
       AND runs.current_run_lease_id = sqlc.arg(run_lease_id)
       AND runs.status = 'running'
       AND octet_length((
           CASE sqlc.arg(operation)::text
             WHEN 'set' THEN jsonb_set(
                 COALESCE(runs.metadata, '{}'::jsonb),
                 ARRAY[sqlc.arg(key)::text],
                 sqlc.arg(value)::jsonb,
                 true
               )
             WHEN 'patch' THEN COALESCE(runs.metadata, '{}'::jsonb) || sqlc.arg(patch)::jsonb
             WHEN 'increment' THEN jsonb_set(
                 COALESCE(runs.metadata, '{}'::jsonb),
                 ARRAY[sqlc.arg(key)::text],
                 to_jsonb(COALESCE((runs.metadata ->> sqlc.arg(key)::text)::numeric, 0) + sqlc.arg(amount)::numeric),
                 true
               )
             ELSE runs.metadata
           END
       )::text) <= sqlc.arg(max_metadata_bytes)::integer
    RETURNING runs.id, runs.org_id, runs.worker_group_id, runs.project_id, runs.environment_id, runs.deployment_id, runs.deployment_task_id, runs.deployment_version, runs.api_version, runs.sdk_version, runs.cli_version, runs.task_id, runs.status, runs.execution_status, runs.terminal_outcome, runs.metadata, runs.tags, runs.locked_retry_policy, runs.current_attempt_number, runs.exit_code, runs.output, runs.created_at, runs.updated_at
),
	updated_with_context AS (
	    SELECT updated.*,
	           current_run_lease.run_lease_id,
	           current_run_lease.attempt_number,
           current_run_lease.trace_id,
           current_run_lease.span_id,
           current_run_lease.parent_span_id,
           current_run_lease.traceparent,
           current_run_lease.state_version
      FROM updated
      JOIN current_run_lease ON current_run_lease.org_id = updated.org_id
                           AND current_run_lease.id = updated.id
),
inserted_event AS (
    INSERT INTO telemetry_outbox (
        org_id, worker_group_id, stream_kind, source_kind, source_id, project_id,
        environment_id, run_id, run_lease_id, attempt_number, trace_id, span_id,
        parent_span_id, traceparent, category, severity, source, kind, message,
        payload, redaction_class, snapshot_version, observed_at
    )
    SELECT updated_with_context.org_id,
           updated_with_context.worker_group_id,
           'event',
           'run',
           updated_with_context.id,
           updated_with_context.project_id,
           updated_with_context.environment_id,
           updated_with_context.id,
           updated_with_context.run_lease_id,
           updated_with_context.attempt_number,
           updated_with_context.trace_id,
           updated_with_context.span_id,
           updated_with_context.parent_span_id,
           updated_with_context.traceparent,
           'guest',
           'info',
           'worker',
           'run.metadata.updated',
           'run.metadata.updated',
           jsonb_build_object(
               'operation', sqlc.arg(operation)::text,
               'key', NULLIF(sqlc.arg(key)::text, '')
           ),
           'sensitive',
           updated_with_context.state_version,
           now()
      FROM updated_with_context
    RETURNING id
)
SELECT updated.id, updated.org_id, updated.worker_group_id, updated.project_id, updated.environment_id, updated.deployment_id, updated.deployment_task_id, updated.deployment_version, updated.api_version, updated.sdk_version, updated.cli_version, updated.task_id, updated.status, updated.execution_status, updated.terminal_outcome, updated.metadata, updated.tags, updated.locked_retry_policy, updated.current_attempt_number, updated.exit_code, updated.output, updated.created_at, updated.updated_at, false AS metadata_too_large
  FROM updated
  JOIN inserted_event ON true
UNION ALL
SELECT runs.id, runs.org_id, runs.worker_group_id, runs.project_id, runs.environment_id, runs.deployment_id, runs.deployment_task_id, runs.deployment_version, runs.api_version, runs.sdk_version, runs.cli_version, runs.task_id, runs.status, runs.execution_status, runs.terminal_outcome, runs.metadata, runs.tags, runs.locked_retry_policy, runs.current_attempt_number, runs.exit_code, runs.output, runs.created_at, runs.updated_at, true AS metadata_too_large
  FROM current_run_lease
  JOIN runs ON runs.org_id = current_run_lease.org_id
           AND runs.id = current_run_lease.id
 WHERE NOT EXISTS (SELECT 1 FROM updated);
