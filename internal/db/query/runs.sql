-- name: CreateScopedRun :one
WITH attempt_seed AS (
    SELECT uuidv7() AS id
),
created AS (
    INSERT INTO runs (
        id,
        org_id,
        cell_id,
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
        max_active_duration_ms,
        trace_id,
        root_span_id,
        current_attempt_id,
        current_attempt_number,
        schedule_id,
        schedule_instance_id,
        scheduled_at
    )
    SELECT sqlc.arg(id),
           sqlc.arg(org_id),
           sqlc.arg(cell_id),
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
           sqlc.arg(max_active_duration_ms),
           sqlc.arg(trace_id),
           sqlc.arg(root_span_id),
           attempt_seed.id,
           1,
           sqlc.narg(schedule_id),
           sqlc.narg(schedule_instance_id),
           sqlc.narg(scheduled_at)
      FROM attempt_seed
     WHERE sqlc.narg(schedule_instance_id)::uuid IS NULL
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
               AND task_schedule_instances.active
               AND (
                   task_schedule_instances.retry_after IS NULL
                   OR task_schedule_instances.retry_after <= now()
               )
               AND task_schedules.org_id = sqlc.arg(org_id)
               AND task_schedules.project_id = sqlc.arg(project_id)
               AND task_schedules.active
        )
    RETURNING id, org_id, cell_id, project_id, environment_id, deployment_id, deployment_task_id, workspace_id, session_id, deployment_version, api_version, sdk_version, cli_version, task_id, status, execution_status, terminal_outcome, metadata, tags, locked_retry_policy, trace_id, root_span_id, state_version, current_attempt_id, current_attempt_number, exit_code, output, created_at, updated_at
),
created_attempt AS (
    INSERT INTO run_attempts (id, org_id, cell_id, run_id, attempt_number, status)
    SELECT created.current_attempt_id,
           created.org_id,
           created.cell_id,
           created.id,
           created.current_attempt_number,
           'queued'
      FROM created
    RETURNING id, org_id, run_id, attempt_number
),
created_snapshot AS (
    INSERT INTO run_snapshots (org_id, cell_id, run_id, version, status, execution_status, attempt_id, operation_id, transition, reason)
    SELECT created.org_id,
           created.cell_id,
           created.id,
           created.state_version,
           created.status,
           created.execution_status,
           created.current_attempt_id,
           NULL::uuid,
           'run.created',
           sqlc.arg(event_payload)
      FROM created
      JOIN created_attempt ON created_attempt.run_id = created.id
    RETURNING run_snapshots.run_id
),
created_event_seq AS (
    INSERT INTO event_cursors (org_id, cell_id, subject_kind, subject_id, seq)
    SELECT created.org_id, created.cell_id, 'run', created.id, 1
      FROM created
      JOIN created_snapshot ON true
    ON CONFLICT (org_id, subject_kind, subject_id)
    DO UPDATE SET seq = event_cursors.seq + 1,
                  observed_at = now()
    RETURNING org_id, subject_kind, subject_id, seq
),
created_event AS (
    INSERT INTO event_hot_payloads (org_id, cell_id, project_id, environment_id, run_id, seq, attempt_id, attempt_number, trace_id, span_id, traceparent, category, severity, source, kind, message, payload, redaction_class, snapshot_version)
    SELECT created.org_id,
           created.cell_id,
           created.project_id,
           created.environment_id,
           created.id,
           created_event_seq.seq,
           created.current_attempt_id,
           created.current_attempt_number,
           created.trace_id,
           created.root_span_id,
           '00-' || created.trace_id || '-' || created.root_span_id || '-01',
           'lifecycle',
           'info',
           'control',
           'run.created',
           'run.created',
           sqlc.arg(event_payload),
           'internal',
           created.state_version
      FROM created
      JOIN created_event_seq ON created_event_seq.org_id = created.org_id
                            AND created_event_seq.subject_kind = 'run'
                            AND created_event_seq.subject_id = created.id
    RETURNING *
),
created_telemetry_outbox AS (
    INSERT INTO telemetry_outbox (org_id, cell_id, stream_kind, source_kind, source_id, idempotency_key, event_record_id, stream_key)
    SELECT created_event.org_id,
                  created_event.cell_id,
                  'event',
                  'event',
                  created_event.subject_id,
                  'event:' || created_event.subject_kind::text || ':' || created_event.subject_id::text || ':' || created_event.seq::text,
                  created_event.id,
                  'helmr:events:' || created_event.org_id::text || ':' || created_event.subject_kind::text || ':' || created_event.subject_id::text
      FROM created_event
    RETURNING id
)
SELECT created.id, created.org_id, created.project_id, created.environment_id, created.deployment_id, created.deployment_task_id, created.workspace_id, created.session_id, created.deployment_version, created.api_version, created.sdk_version, created.cli_version, created.task_id, created.status, created.execution_status, created.terminal_outcome, created.metadata, created.tags, created.locked_retry_policy, created.current_attempt_number, created.exit_code, created.output, created.created_at, created.updated_at
  FROM created
  JOIN created_snapshot ON true
  JOIN created_telemetry_outbox ON true;

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
           state_version = state_version + 1,
           finished_at = now(),
           observed_at = now()
      FROM eligible
     WHERE runs.org_id = eligible.org_id
       AND runs.id = eligible.id
       AND runs.status = 'queued'
    RETURNING runs.id, runs.org_id, runs.project_id, runs.environment_id, runs.session_id, runs.current_attempt_id, runs.current_attempt_number, runs.trace_id, runs.root_span_id, runs.state_version, runs.ttl
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
expired_attempts AS (
    UPDATE run_attempts
       SET status = 'expired',
           error_message = 'run ttl expired before execution started',
           finished_at = now(),
           observed_at = now()
      FROM expired_runs
     WHERE run_attempts.org_id = expired_runs.org_id
       AND run_attempts.run_id = expired_runs.id
       AND run_attempts.id = expired_runs.current_attempt_id
    RETURNING run_attempts.id, run_attempts.run_id
),
completed_queue_entries AS (
    UPDATE run_queue_items
       SET status = 'completed',
           dispatch_generation = dispatch_generation + 1,
           observed_at = now(),
           finished_at = now()
      FROM expired_runs
     WHERE run_queue_items.org_id = expired_runs.org_id
       AND run_queue_items.run_id = expired_runs.id
       AND run_queue_items.status IN ('queued', 'published', 'reserved')
    RETURNING run_queue_items.run_id
),
expired_snapshots AS (
    INSERT INTO run_snapshots (org_id, cell_id, run_id, version, status, execution_status, terminal_outcome, attempt_id, transition, reason)
    SELECT expired_runs.org_id,
           expired_runs.cell_id,
           expired_runs.id,
           expired_runs.state_version,
           'expired',
           'finished',
           'expired',
           expired_runs.current_attempt_id,
           'run.expired',
           jsonb_build_object('ttl', expired_runs.ttl, 'message', 'run ttl expired before execution started')
      FROM expired_runs
      JOIN expired_attempts ON expired_attempts.run_id = expired_runs.id
    RETURNING run_snapshots.run_id
),
expired_event_seq AS (
    INSERT INTO event_cursors (org_id, cell_id, subject_kind, subject_id, seq)
    SELECT expired_runs.org_id, expired_runs.cell_id, 'run', expired_runs.id, 1
      FROM expired_runs
      JOIN expired_snapshots ON expired_snapshots.run_id = expired_runs.id
    ON CONFLICT (org_id, subject_kind, subject_id)
    DO UPDATE SET seq = event_cursors.seq + 1,
                  observed_at = now()
    RETURNING org_id, subject_kind, subject_id, seq
),
expired_event AS (
    INSERT INTO event_hot_payloads (org_id, cell_id, project_id, environment_id, run_id, seq, attempt_id, attempt_number, trace_id, span_id, traceparent, category, severity, source, kind, message, payload, redaction_class, snapshot_version)
    SELECT expired_runs.org_id,
           expired_runs.cell_id,
           expired_runs.project_id,
           expired_runs.environment_id,
           expired_runs.id,
           expired_event_seq.seq,
           expired_runs.current_attempt_id,
           expired_runs.current_attempt_number,
           expired_runs.trace_id,
           expired_runs.root_span_id,
           '00-' || expired_runs.trace_id || '-' || expired_runs.root_span_id || '-01',
           'lifecycle',
           'warn',
           'control',
           'run.expired',
           'run.expired',
           jsonb_build_object('ttl', expired_runs.ttl, 'message', 'run ttl expired before execution started'),
           'internal',
           expired_runs.state_version
  FROM expired_runs
  JOIN expired_snapshots ON expired_snapshots.run_id = expired_runs.id
  JOIN expired_event_seq ON expired_event_seq.org_id = expired_runs.org_id
                        AND expired_event_seq.subject_kind = 'run'
                        AND expired_event_seq.subject_id = expired_runs.id
    RETURNING *
),
expired_telemetry_outbox AS (
    INSERT INTO telemetry_outbox (org_id, cell_id, stream_kind, source_kind, source_id, idempotency_key, event_record_id, stream_key)
    SELECT expired_event.org_id,
                  expired_event.cell_id,
                  'event',
                  'event',
                  expired_event.subject_id,
                  'event:' || expired_event.subject_kind::text || ':' || expired_event.subject_id::text || ':' || expired_event.seq::text,
                  expired_event.id,
                  'helmr:events:' || expired_event.org_id::text || ':' || expired_event.subject_kind::text || ':' || expired_event.subject_id::text
      FROM expired_event
    RETURNING id
)
SELECT expired_event.*
  FROM expired_event
  JOIN expired_telemetry_outbox ON true;

-- name: SetQueuedRunWorkspaceMount :exec
UPDATE runs
   SET workspace_mount_id = sqlc.arg(workspace_mount_id),
       observed_at = now()
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
           state_version = runs.state_version + 1,
           finished_at = now(),
           observed_at = now()
      FROM target
     WHERE runs.org_id = target.org_id
       AND runs.id = target.id
       AND runs.status = 'queued'
       AND runs.current_run_lease_id IS NULL
    RETURNING runs.id, runs.org_id, runs.project_id, runs.environment_id, runs.session_id, runs.current_attempt_id, runs.current_attempt_number, runs.trace_id, runs.root_span_id, runs.state_version, runs.error_message
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
failed_attempt AS (
    UPDATE run_attempts
       SET status = 'failed',
           error_message = failed_run.error_message,
           finished_at = now(),
           observed_at = now()
      FROM failed_run
     WHERE run_attempts.org_id = failed_run.org_id
       AND run_attempts.run_id = failed_run.id
       AND run_attempts.id = failed_run.current_attempt_id
    RETURNING run_attempts.id, run_attempts.run_id
),
completed_queue_entry AS (
    UPDATE run_queue_items
       SET status = 'completed',
           dispatch_generation = dispatch_generation + 1,
           observed_at = now(),
           finished_at = now()
      FROM failed_run
     WHERE run_queue_items.org_id = failed_run.org_id
       AND run_queue_items.run_id = failed_run.id
       AND run_queue_items.status IN ('queued', 'published', 'reserved')
    RETURNING run_queue_items.run_id
),
failed_snapshot AS (
    INSERT INTO run_snapshots (org_id, cell_id, run_id, version, status, execution_status, terminal_outcome, attempt_id, transition, reason)
    SELECT failed_run.org_id,
           failed_run.cell_id,
           failed_run.id,
           failed_run.state_version,
           'failed',
           'finished',
           'failed',
           failed_run.current_attempt_id,
           'run.failed',
           COALESCE(sqlc.arg(reason)::jsonb, '{}'::jsonb)
      FROM failed_run
      JOIN failed_attempt ON failed_attempt.run_id = failed_run.id
    RETURNING run_snapshots.run_id
),
failed_event_seq AS (
    INSERT INTO event_cursors (org_id, cell_id, subject_kind, subject_id, seq)
    SELECT failed_run.org_id, failed_run.cell_id, 'run', failed_run.id, 1
      FROM failed_run
      JOIN failed_snapshot ON failed_snapshot.run_id = failed_run.id
    ON CONFLICT (org_id, subject_kind, subject_id)
    DO UPDATE SET seq = event_cursors.seq + 1,
                  observed_at = now()
    RETURNING org_id, subject_kind, subject_id, seq
),
failed_event AS (
    INSERT INTO event_hot_payloads (org_id, cell_id, project_id, environment_id, run_id, seq, attempt_id, attempt_number, trace_id, span_id, traceparent, category, severity, source, kind, message, payload, redaction_class, snapshot_version)
    SELECT failed_run.org_id,
           failed_run.cell_id,
           failed_run.project_id,
           failed_run.environment_id,
           failed_run.id,
           failed_event_seq.seq,
           failed_run.current_attempt_id,
           failed_run.current_attempt_number,
           failed_run.trace_id,
           failed_run.root_span_id,
           '00-' || failed_run.trace_id || '-' || failed_run.root_span_id || '-01',
           'lifecycle',
           'error',
           'control',
           'run.failed',
           'run.failed',
           COALESCE(sqlc.arg(reason)::jsonb, '{}'::jsonb),
           'internal',
           failed_run.state_version
      FROM failed_run
      JOIN failed_snapshot ON failed_snapshot.run_id = failed_run.id
      JOIN failed_event_seq ON failed_event_seq.org_id = failed_run.org_id
                           AND failed_event_seq.subject_kind = 'run'
                           AND failed_event_seq.subject_id = failed_run.id
    RETURNING *
),
failed_telemetry_outbox AS (
    INSERT INTO telemetry_outbox (org_id, cell_id, stream_kind, source_kind, source_id, idempotency_key, event_record_id, stream_key)
    SELECT failed_event.org_id,
                  failed_event.cell_id,
                  'event',
                  'event',
                  failed_event.subject_id,
                  'event:' || failed_event.subject_kind::text || ':' || failed_event.subject_id::text || ':' || failed_event.seq::text,
                  failed_event.id,
                  'helmr:events:' || failed_event.org_id::text || ':' || failed_event.subject_kind::text || ':' || failed_event.subject_id::text
      FROM failed_event
    RETURNING id
)
SELECT failed_event.*
  FROM failed_event
  JOIN failed_telemetry_outbox ON true;

-- name: GetRunSummary :one
SELECT id, org_id, project_id, environment_id, deployment_id, deployment_task_id, session_id, deployment_version, api_version, sdk_version, cli_version, task_id, status, execution_status, terminal_outcome, metadata, tags, locked_retry_policy, current_attempt_number, exit_code, output, created_at, updated_at
FROM runs
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
WHERE org_id = sqlc.arg(org_id)
  AND project_id = sqlc.arg(project_id)
  AND environment_id = sqlc.arg(environment_id);

-- name: ListScopedRunSummaries :many
SELECT id, org_id, project_id, environment_id, deployment_id, deployment_task_id, session_id, deployment_version, api_version, sdk_version, cli_version, task_id, status, execution_status, terminal_outcome, metadata, tags, locked_retry_policy, current_attempt_number, exit_code, output, created_at, updated_at
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
  AND (
    sqlc.narg(session_id)::uuid IS NULL
    OR session_id = sqlc.narg(session_id)::uuid
  )
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg(row_limit);

-- name: CreateRunOperation :one
INSERT INTO run_operations (
    id,
    org_id,
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
    sqlc.arg(org_id),
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
           state_version = runs.state_version + 1,
           finished_at = CASE
             WHEN target.execution_status = 'executing' AND NOT sqlc.arg(force)::bool THEN runs.finished_at
             ELSE COALESCE(runs.finished_at, now())
           END,
           observed_at = now()
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
cancelled_attempt AS (
    UPDATE run_attempts
       SET status = 'cancelled',
           error_message = COALESCE(NULLIF(sqlc.arg(reason)::text, ''), 'run cancelled'),
           finished_at = CASE
             WHEN updated.execution_status = 'pending_cancel' THEN run_attempts.finished_at
             ELSE COALESCE(run_attempts.finished_at, now())
           END,
           observed_at = now()
      FROM updated
     WHERE run_attempts.org_id = updated.org_id
       AND run_attempts.run_id = updated.id
       AND run_attempts.id = updated.current_attempt_id
    RETURNING run_attempts.id, run_attempts.attempt_number
),
cancelled_queue AS (
    UPDATE run_queue_items
       SET status = 'cancelled',
           dispatch_generation = dispatch_generation + 1,
           observed_at = now(),
           finished_at = now()
      FROM updated
     WHERE run_queue_items.org_id = updated.org_id
       AND run_queue_items.run_id = updated.id
       AND run_queue_items.status IN ('queued', 'published', 'reserved', 'parked')
       AND (updated.execution_status <> 'pending_cancel' OR sqlc.arg(force)::bool)
    RETURNING run_queue_items.run_id
),
cancelled_run_waits AS (
    UPDATE run_waits
       SET state = 'cancelled',
           cancelled_at = now(),
           observed_at = now()
      FROM updated
     WHERE run_waits.org_id = updated.org_id
       AND run_waits.run_id = updated.id
       AND run_waits.state IN ('live_waiting', 'checkpointing', 'checkpointed_waiting', 'resolved_live', 'resolved_checkpointed', 'resuming')
    RETURNING run_waits.id
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
cancelled_session AS (
    UPDATE run_leases
       SET status = CASE WHEN updated.execution_status <> 'pending_cancel' OR sqlc.arg(force)::bool THEN 'cancelled'::run_lease_status ELSE run_leases.status END,
           released_at = CASE WHEN updated.execution_status <> 'pending_cancel' OR sqlc.arg(force)::bool THEN COALESCE(run_leases.released_at, now()) ELSE run_leases.released_at END,
           renewed_at = now()
      FROM updated
     WHERE run_leases.org_id = updated.org_id
       AND run_leases.run_id = updated.id
       AND run_leases.id = updated.previous_run_lease_id
       AND run_leases.status IN ('leased', 'running')
    RETURNING run_leases.id
),
released_concurrency AS (
    UPDATE run_queue_concurrency_leases
       SET released_at = now()
      FROM updated
     WHERE run_queue_concurrency_leases.org_id = updated.org_id
       AND run_queue_concurrency_leases.run_id = updated.id
       AND run_queue_concurrency_leases.released_at IS NULL
       AND (updated.execution_status <> 'pending_cancel' OR sqlc.arg(force)::bool)
    RETURNING run_queue_concurrency_leases.id
),
snapshot AS (
    INSERT INTO run_snapshots (org_id, cell_id, run_id, version, status, execution_status, terminal_outcome, attempt_id, run_lease_id, operation_id, previous_version, transition, reason)
    SELECT updated.org_id,
           updated.cell_id,
           updated.id,
           updated.state_version,
           updated.status,
           updated.execution_status,
           updated.terminal_outcome,
           updated.current_attempt_id,
           updated.current_run_lease_id,
           sqlc.arg(operation_id),
           updated.state_version - 1,
           CASE WHEN updated.execution_status = 'pending_cancel' THEN 'run.cancel_requested' ELSE 'run.cancelled' END,
           jsonb_build_object(
               'reason', COALESCE(NULLIF(sqlc.arg(reason)::text, ''), 'run cancelled'),
               'force', sqlc.arg(force)::bool
           )
      FROM updated
      JOIN cancelled_attempt ON true
    RETURNING run_snapshots.run_id
),
event_seq AS (
    INSERT INTO event_cursors (org_id, cell_id, subject_kind, subject_id, seq)
    SELECT updated.org_id, updated.cell_id, 'run', updated.id, 1
      FROM updated
      JOIN snapshot ON true
    ON CONFLICT (org_id, subject_kind, subject_id)
    DO UPDATE SET seq = event_cursors.seq + 1,
                  observed_at = now()
    RETURNING org_id, subject_kind, subject_id, seq
),
event AS (
    INSERT INTO event_hot_payloads (org_id, cell_id, project_id, environment_id, run_id, seq, attempt_id, run_lease_id, attempt_number, trace_id, span_id, traceparent, category, severity, source, kind, message, payload, redaction_class, snapshot_version)
    SELECT updated.org_id,
           updated.cell_id,
           updated.project_id,
           updated.environment_id,
           updated.id,
           event_seq.seq,
           updated.current_attempt_id,
           updated.current_run_lease_id,
           cancelled_attempt.attempt_number,
           updated.trace_id,
           updated.root_span_id,
           '00-' || updated.trace_id || '-' || updated.root_span_id || '-01',
           'lifecycle',
           'warn',
           'control',
           CASE WHEN updated.execution_status = 'pending_cancel' THEN 'run.cancel_requested' ELSE 'run.cancelled' END,
           CASE WHEN updated.execution_status = 'pending_cancel' THEN 'run.cancel_requested' ELSE 'run.cancelled' END,
           jsonb_build_object(
               'reason', COALESCE(NULLIF(sqlc.arg(reason)::text, ''), 'run cancelled'),
               'force', sqlc.arg(force)::bool
           ),
           'internal',
           updated.state_version
      FROM updated
      JOIN cancelled_attempt ON true
      JOIN event_seq ON event_seq.org_id = updated.org_id
                    AND event_seq.subject_kind = 'run'
                    AND event_seq.subject_id = updated.id
    RETURNING *
),
telemetry_outbox AS (
    INSERT INTO telemetry_outbox (org_id, cell_id, stream_kind, source_kind, source_id, idempotency_key, event_record_id, stream_key)
    SELECT event.org_id,
                  event.cell_id,
                  'event',
                  'event',
                  event.subject_id,
                  'event:' || event.subject_kind::text || ':' || event.subject_id::text || ':' || event.seq::text,
                  event.id,
                  'helmr:events:' || event.org_id::text || ':' || event.subject_kind::text || ':' || event.subject_id::text
      FROM event
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
SELECT updated.id, updated.org_id, updated.project_id, updated.environment_id, updated.deployment_id, updated.deployment_task_id, updated.session_id, updated.deployment_version, updated.api_version, updated.sdk_version, updated.cli_version, updated.task_id, updated.status, updated.execution_status, updated.terminal_outcome, updated.metadata, updated.tags, updated.locked_retry_policy, updated.current_attempt_number, updated.exit_code, updated.output, updated.created_at, updated.updated_at
  FROM updated
  JOIN operation_applied ON true
  JOIN telemetry_outbox ON true
	 WHERE (SELECT count(*) FROM cancelled_run_waits) >= 0
	   AND (SELECT count(*) FROM terminal_session_runs) >= 0
	   AND (SELECT count(*) FROM terminal_sessions) >= 0
UNION ALL
SELECT target.id, target.org_id, target.project_id, target.environment_id, target.deployment_id, target.deployment_task_id, target.session_id, target.deployment_version, target.api_version, target.sdk_version, target.cli_version, target.task_id, target.status, target.execution_status, target.terminal_outcome, target.metadata, target.tags, target.locked_retry_policy, target.current_attempt_number, target.exit_code, target.output, target.created_at, target.updated_at
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
           run_leases.attempt_id,
           run_leases.span_id,
           run_leases.parent_span_id,
           run_leases.traceparent,
           run_attempts.attempt_number
      FROM runs
      JOIN run_leases ON run_leases.id = runs.current_run_lease_id
                          AND run_leases.org_id = runs.org_id
                          AND run_leases.run_id = runs.id
      JOIN run_attempts ON run_attempts.org_id = run_leases.org_id
                       AND run_attempts.run_id = run_leases.run_id
                       AND run_attempts.id = run_leases.attempt_id
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
           observed_at = now()
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
    RETURNING runs.id, runs.org_id, runs.project_id, runs.environment_id, runs.deployment_id, runs.deployment_task_id, runs.deployment_version, runs.api_version, runs.sdk_version, runs.cli_version, runs.task_id, runs.status, runs.execution_status, runs.terminal_outcome, runs.metadata, runs.tags, runs.locked_retry_policy, runs.current_attempt_number, runs.exit_code, runs.output, runs.created_at, runs.updated_at
),
updated_with_context AS (
    SELECT updated.*,
           current_run_lease.run_lease_id,
           current_run_lease.attempt_id,
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
event_seq AS (
    INSERT INTO event_cursors (org_id, cell_id, subject_kind, subject_id, seq)
    SELECT updated_with_context.org_id,
           updated_with_context.cell_id,
           'run',
           updated_with_context.id,
           1
      FROM updated_with_context
    ON CONFLICT (org_id, subject_kind, subject_id)
    DO UPDATE SET seq = event_cursors.seq + 1,
                  observed_at = now()
    RETURNING org_id, subject_kind, subject_id, seq
),
inserted_event AS (
    INSERT INTO event_hot_payloads (org_id, cell_id, project_id, environment_id, run_id, seq, attempt_id, run_lease_id, attempt_number, trace_id, span_id, parent_span_id, traceparent, category, severity, source, kind, message, payload, redaction_class, snapshot_version)
    SELECT updated_with_context.org_id,
           updated_with_context.cell_id,
           updated_with_context.project_id,
           updated_with_context.environment_id,
           updated_with_context.id,
           event_seq.seq,
           updated_with_context.attempt_id,
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
           updated_with_context.state_version
      FROM updated_with_context
      JOIN event_seq ON event_seq.org_id = updated_with_context.org_id
                    AND event_seq.subject_kind = 'run'
                    AND event_seq.subject_id = updated_with_context.id
    RETURNING id
),
telemetry_outbox AS (
    INSERT INTO telemetry_outbox (org_id, cell_id, stream_kind, source_kind, source_id, idempotency_key, event_record_id, stream_key)
    SELECT inserted_event.org_id,
                  inserted_event.cell_id,
                  'event',
                  'event',
                  inserted_event.subject_id,
                  'event:' || inserted_event.subject_kind::text || ':' || inserted_event.subject_id::text || ':' || inserted_event.seq::text,
                  inserted_event.id,
                  'helmr:events:' || updated_with_context.org_id::text || ':run:' || updated_with_context.id::text
      FROM inserted_event
      JOIN updated_with_context ON true
    RETURNING id
)
SELECT updated.id, updated.org_id, updated.project_id, updated.environment_id, updated.deployment_id, updated.deployment_task_id, updated.deployment_version, updated.api_version, updated.sdk_version, updated.cli_version, updated.task_id, updated.status, updated.execution_status, updated.terminal_outcome, updated.metadata, updated.tags, updated.locked_retry_policy, updated.current_attempt_number, updated.exit_code, updated.output, updated.created_at, updated.updated_at, false AS metadata_too_large
  FROM updated
  JOIN telemetry_outbox ON true
UNION ALL
SELECT runs.id, runs.org_id, runs.project_id, runs.environment_id, runs.deployment_id, runs.deployment_task_id, runs.deployment_version, runs.api_version, runs.sdk_version, runs.cli_version, runs.task_id, runs.status, runs.execution_status, runs.terminal_outcome, runs.metadata, runs.tags, runs.locked_retry_policy, runs.current_attempt_number, runs.exit_code, runs.output, runs.created_at, runs.updated_at, true AS metadata_too_large
  FROM current_run_lease
  JOIN runs ON runs.org_id = current_run_lease.org_id
           AND runs.id = current_run_lease.id
 WHERE NOT EXISTS (SELECT 1 FROM updated);
