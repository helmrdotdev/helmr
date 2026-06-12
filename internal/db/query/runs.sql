-- name: CreateScopedRun :one
WITH attempt_seed AS (
    SELECT uuidv7() AS id
),
created AS (
    INSERT INTO runs (
        id,
        org_id,
        project_id,
        environment_id,
        deployment_id,
        deployment_task_id,
        deployment_version,
        api_version,
        sdk_version,
        cli_version,
        task_id,
        payload,
        metadata,
        tags,
        idempotency_key,
        idempotency_key_expires_at,
        idempotency_key_options,
        idempotency_request_hash,
        locked_retry_policy,
        replayed_from_run_id,
        replay_operation_id,
        queue_name,
        queue_concurrency_limit,
        concurrency_key,
        priority,
        queue_timestamp,
        ttl,
        queued_expires_at,
        max_duration_seconds,
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
           sqlc.arg(project_id),
           sqlc.arg(environment_id),
           sqlc.arg(deployment_id),
           sqlc.arg(deployment_task_id),
           sqlc.arg(deployment_version),
           sqlc.arg(api_version),
           sqlc.arg(sdk_version),
           sqlc.arg(cli_version),
           sqlc.arg(task_id),
           sqlc.arg(payload),
           coalesce(sqlc.arg(metadata)::jsonb, '{}'::jsonb),
           coalesce(sqlc.arg(tags)::text[], '{}'::text[]),
           sqlc.narg(idempotency_key),
           sqlc.narg(idempotency_key_expires_at),
           coalesce(sqlc.arg(idempotency_key_options)::jsonb, '{}'::jsonb),
           sqlc.narg(idempotency_request_hash),
           coalesce(sqlc.arg(locked_retry_policy)::jsonb, 'false'::jsonb),
           sqlc.narg(replayed_from_run_id),
           sqlc.narg(replay_operation_id),
           sqlc.arg(queue_name),
           sqlc.narg(queue_concurrency_limit),
           sqlc.narg(concurrency_key),
           sqlc.arg(priority),
           sqlc.arg(queue_timestamp),
           sqlc.arg(ttl),
           sqlc.narg(queued_expires_at),
           sqlc.arg(max_duration_seconds),
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
    RETURNING id, org_id, project_id, environment_id, deployment_id, deployment_task_id, deployment_version, api_version, sdk_version, cli_version, task_id, status, execution_status, terminal_outcome, metadata, tags, locked_retry_policy, replayed_from_run_id, replay_operation_id, trace_id, root_span_id, state_version, current_attempt_id, current_attempt_number, exit_code, output, created_at, updated_at
),
created_attempt AS (
    INSERT INTO run_attempts (id, org_id, run_id, attempt_number, status, cause)
    SELECT created.current_attempt_id,
           created.org_id,
           created.id,
           created.current_attempt_number,
           'queued',
           CASE WHEN created.replayed_from_run_id IS NULL THEN 'original' ELSE 'replay' END
      FROM created
    RETURNING id, org_id, run_id, attempt_number
),
created_snapshot AS (
    INSERT INTO run_snapshots (org_id, run_id, version, status, execution_status, attempt_id, operation_id, transition, reason)
    SELECT created.org_id,
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
    INSERT INTO event_subject_cursors (org_id, subject_type, subject_id, last_seq)
    SELECT created.org_id, 'run', created.id, 1
      FROM created
      JOIN created_snapshot ON true
    ON CONFLICT (org_id, subject_type, subject_id)
    DO UPDATE SET last_seq = event_subject_cursors.last_seq + 1,
                  updated_at = now()
    RETURNING org_id, subject_type, subject_id, last_seq
),
created_event AS (
    INSERT INTO events (org_id, project_id, environment_id, run_id, seq, attempt_id, attempt_number, trace_id, span_id, traceparent, category, severity, source, kind, message, payload, redaction_class, snapshot_version)
    SELECT created.org_id,
           created.project_id,
           created.environment_id,
           created.id,
           created_event_seq.last_seq,
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
                            AND created_event_seq.subject_type = 'run'
                            AND created_event_seq.subject_id = created.id
    RETURNING *
),
created_event_outbox AS (
    INSERT INTO event_outbox (event_record_id, stream_key)
    SELECT created_event.id,
           'helmr:events:' || created_event.org_id::text || ':' || created_event.subject_type::text || ':' || created_event.subject_id::text
      FROM created_event
    RETURNING id
)
SELECT created.id, created.org_id, created.project_id, created.environment_id, created.deployment_id, created.deployment_task_id, created.deployment_version, created.api_version, created.sdk_version, created.cli_version, created.task_id, created.status, created.execution_status, created.terminal_outcome, created.metadata, created.tags, created.locked_retry_policy, created.replayed_from_run_id, created.current_attempt_number, created.exit_code, created.output, created.created_at, created.updated_at
  FROM created
  JOIN created_snapshot ON true
  JOIN created_event_outbox ON true;

-- name: GetRun :one
SELECT * FROM runs
WHERE org_id = $1 AND id = $2;

-- name: GetScopedRunByIdempotencyKey :one
SELECT id, org_id, project_id, environment_id, deployment_id, deployment_task_id, deployment_version, api_version, sdk_version, cli_version, task_id, status, execution_status, terminal_outcome, metadata, tags, locked_retry_policy, replayed_from_run_id, current_attempt_number, exit_code, output, created_at, updated_at, idempotency_key_expires_at, idempotency_request_hash, schedule_id, schedule_instance_id, scheduled_at
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
       AND runs.current_session_id IS NULL
       AND runs.queued_expires_at IS NOT NULL
       AND runs.queued_expires_at <= now()
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
           updated_at = now()
      FROM eligible
     WHERE runs.org_id = eligible.org_id
       AND runs.id = eligible.id
       AND runs.status = 'queued'
    RETURNING runs.id, runs.org_id, runs.project_id, runs.environment_id, runs.current_attempt_id, runs.current_attempt_number, runs.trace_id, runs.root_span_id, runs.state_version, runs.ttl
),
expired_attempts AS (
    UPDATE run_attempts
       SET status = 'expired',
           error_message = 'run ttl expired before execution started',
           finished_at = now(),
           updated_at = now()
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
           updated_at = now(),
           finished_at = now()
      FROM expired_runs
     WHERE run_queue_items.org_id = expired_runs.org_id
       AND run_queue_items.run_id = expired_runs.id
       AND run_queue_items.status IN ('queued', 'published', 'reserved')
    RETURNING run_queue_items.run_id
),
expired_snapshots AS (
    INSERT INTO run_snapshots (org_id, run_id, version, status, execution_status, terminal_outcome, attempt_id, transition, reason)
    SELECT expired_runs.org_id,
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
    INSERT INTO event_subject_cursors (org_id, subject_type, subject_id, last_seq)
    SELECT expired_runs.org_id, 'run', expired_runs.id, 1
      FROM expired_runs
      JOIN expired_snapshots ON expired_snapshots.run_id = expired_runs.id
    ON CONFLICT (org_id, subject_type, subject_id)
    DO UPDATE SET last_seq = event_subject_cursors.last_seq + 1,
                  updated_at = now()
    RETURNING org_id, subject_type, subject_id, last_seq
),
expired_event AS (
    INSERT INTO events (org_id, project_id, environment_id, run_id, seq, attempt_id, attempt_number, trace_id, span_id, traceparent, category, severity, source, kind, message, payload, redaction_class, snapshot_version)
    SELECT expired_runs.org_id,
           expired_runs.project_id,
           expired_runs.environment_id,
           expired_runs.id,
           expired_event_seq.last_seq,
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
                        AND expired_event_seq.subject_type = 'run'
                        AND expired_event_seq.subject_id = expired_runs.id
    RETURNING *
),
expired_event_outbox AS (
    INSERT INTO event_outbox (event_record_id, stream_key)
    SELECT expired_event.id,
           'helmr:events:' || expired_event.org_id::text || ':' || expired_event.subject_type::text || ':' || expired_event.subject_id::text
      FROM expired_event
    RETURNING id
)
SELECT expired_event.*
  FROM expired_event
  JOIN expired_event_outbox ON true;

-- name: GetRunSummary :one
SELECT id, org_id, project_id, environment_id, deployment_id, deployment_task_id, deployment_version, api_version, sdk_version, cli_version, task_id, status, execution_status, terminal_outcome, metadata, tags, locked_retry_policy, replayed_from_run_id, current_attempt_number, exit_code, output, created_at, updated_at
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
SELECT id, org_id, project_id, environment_id, deployment_id, deployment_task_id, deployment_version, api_version, sdk_version, cli_version, task_id, status, execution_status, terminal_outcome, metadata, tags, locked_retry_policy, replayed_from_run_id, current_attempt_number, exit_code, output, created_at, updated_at
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
target AS (
    SELECT runs.*
      FROM runs
      JOIN operation ON operation.org_id = runs.org_id
                    AND operation.run_id = runs.id
     WHERE runs.org_id = sqlc.arg(org_id)
       AND runs.id = sqlc.arg(run_id)
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
           current_session_id = CASE
             WHEN target.execution_status = 'executing' AND NOT sqlc.arg(force)::bool THEN runs.current_session_id
             ELSE NULL
           END,
           error_message = COALESCE(NULLIF(sqlc.arg(reason)::text, ''), 'run cancelled'),
           state_version = runs.state_version + 1,
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
    RETURNING runs.*, target.current_session_id AS previous_session_id
),
cancelled_attempt AS (
    UPDATE run_attempts
       SET status = 'cancelled',
           error_message = COALESCE(NULLIF(sqlc.arg(reason)::text, ''), 'run cancelled'),
           finished_at = CASE
             WHEN updated.execution_status = 'pending_cancel' THEN run_attempts.finished_at
             ELSE COALESCE(run_attempts.finished_at, now())
           END,
           updated_at = now()
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
           updated_at = now(),
           finished_at = now()
      FROM updated
     WHERE run_queue_items.org_id = updated.org_id
       AND run_queue_items.run_id = updated.id
       AND run_queue_items.status IN ('queued', 'published', 'reserved', 'suspended')
       AND (updated.execution_status <> 'pending_cancel' OR sqlc.arg(force)::bool)
    RETURNING run_queue_items.run_id
),
cancelled_run_waits AS (
    UPDATE run_waits
       SET status = 'cancelled',
           failure = jsonb_build_object('reason', COALESCE(NULLIF(sqlc.arg(reason)::text, ''), 'run cancelled'), 'source', 'cancel_operation'),
           resolution_kind = 'cancelled',
           resolution = jsonb_build_object('reason', COALESCE(NULLIF(sqlc.arg(reason)::text, ''), 'run cancelled'), 'source', 'cancel_operation'),
           failed_at = now(),
           updated_at = now()
      FROM updated
     WHERE run_waits.org_id = updated.org_id
       AND run_waits.run_id = updated.id
       AND run_waits.status IN ('opening', 'waiting', 'resuming')
    RETURNING run_waits.id, run_waits.org_id
),
cancelled_waitpoints AS (
    UPDATE waitpoints
       SET status = 'cancelled',
           resolution_kind = 'cancelled',
           output = 'null'::jsonb,
           resolution = jsonb_build_object('reason', COALESCE(NULLIF(sqlc.arg(reason)::text, ''), 'run cancelled'), 'source', 'cancel_operation'),
           output_is_error = true,
           completed_at = now(),
           updated_at = now()
      FROM cancelled_run_waits
      JOIN run_wait_dependencies ON run_wait_dependencies.org_id = cancelled_run_waits.org_id
                                AND run_wait_dependencies.run_wait_id = cancelled_run_waits.id
     WHERE waitpoints.org_id = run_wait_dependencies.org_id
       AND waitpoints.id = run_wait_dependencies.waitpoint_id
       AND waitpoints.status = 'pending'
    RETURNING waitpoints.id
),
cancelled_session AS (
    UPDATE run_execution_sessions
       SET status = CASE WHEN updated.execution_status <> 'pending_cancel' OR sqlc.arg(force)::bool THEN 'cancelled'::run_execution_session_status ELSE run_execution_sessions.status END,
           released_at = CASE WHEN updated.execution_status <> 'pending_cancel' OR sqlc.arg(force)::bool THEN COALESCE(run_execution_sessions.released_at, now()) ELSE run_execution_sessions.released_at END,
           renewed_at = now()
      FROM updated
     WHERE run_execution_sessions.org_id = updated.org_id
       AND run_execution_sessions.run_id = updated.id
       AND run_execution_sessions.id = updated.previous_session_id
       AND run_execution_sessions.status IN ('leased', 'running')
    RETURNING run_execution_sessions.id
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
    INSERT INTO run_snapshots (org_id, run_id, version, status, execution_status, terminal_outcome, attempt_id, session_id, operation_id, previous_version, transition, reason)
    SELECT updated.org_id,
           updated.id,
           updated.state_version,
           updated.status,
           updated.execution_status,
           updated.terminal_outcome,
           updated.current_attempt_id,
           updated.current_session_id,
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
    INSERT INTO event_subject_cursors (org_id, subject_type, subject_id, last_seq)
    SELECT updated.org_id, 'run', updated.id, 1
      FROM updated
      JOIN snapshot ON true
    ON CONFLICT (org_id, subject_type, subject_id)
    DO UPDATE SET last_seq = event_subject_cursors.last_seq + 1,
                  updated_at = now()
    RETURNING org_id, subject_type, subject_id, last_seq
),
event AS (
    INSERT INTO events (org_id, project_id, environment_id, run_id, seq, attempt_id, session_id, attempt_number, trace_id, span_id, traceparent, category, severity, source, kind, message, payload, redaction_class, snapshot_version)
    SELECT updated.org_id,
           updated.project_id,
           updated.environment_id,
           updated.id,
           event_seq.last_seq,
           updated.current_attempt_id,
           updated.current_session_id,
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
                    AND event_seq.subject_type = 'run'
                    AND event_seq.subject_id = updated.id
    RETURNING *
),
event_outbox AS (
    INSERT INTO event_outbox (event_record_id, stream_key)
    SELECT event.id,
           'helmr:events:' || event.org_id::text || ':' || event.subject_type::text || ':' || event.subject_id::text
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
SELECT updated.id, updated.org_id, updated.project_id, updated.environment_id, updated.deployment_id, updated.deployment_task_id, updated.deployment_version, updated.api_version, updated.sdk_version, updated.cli_version, updated.task_id, updated.status, updated.execution_status, updated.terminal_outcome, updated.metadata, updated.tags, updated.locked_retry_policy, updated.replayed_from_run_id, updated.current_attempt_number, updated.exit_code, updated.output, updated.created_at, updated.updated_at
  FROM updated
  JOIN operation_applied ON true
  JOIN event_outbox ON true
UNION ALL
SELECT target.id, target.org_id, target.project_id, target.environment_id, target.deployment_id, target.deployment_task_id, target.deployment_version, target.api_version, target.sdk_version, target.cli_version, target.task_id, target.status, target.execution_status, target.terminal_outcome, target.metadata, target.tags, target.locked_retry_policy, target.replayed_from_run_id, target.current_attempt_number, target.exit_code, target.output, target.created_at, target.updated_at
  FROM target
  JOIN operation_applied ON true
 WHERE NOT EXISTS (SELECT 1 FROM updated);
