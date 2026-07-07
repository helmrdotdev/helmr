-- name: RequeueExpiredLeasedRunLeases :exec
WITH expired AS (
    SELECT runs.id AS run_id,
           runs.org_id,
           runs.worker_group_id,
           run_leases.id AS run_lease_id
      FROM runs
      JOIN run_leases
        ON run_leases.org_id = runs.org_id
       AND run_leases.run_id = runs.id
       AND run_leases.id = runs.current_run_lease_id
     WHERE runs.org_id = sqlc.arg(org_id)
       AND runs.worker_group_id = sqlc.arg(worker_group_id)
       AND runs.status = 'running'
       AND run_leases.status = 'leased'
       AND run_leases.lease_expires_at <= now()
     FOR UPDATE OF runs, run_leases
),
updated_runs AS (
    UPDATE runs
       SET status = 'queued',
           execution_status = 'queued',
           current_run_lease_id = NULL,
           dispatch_generation = runs.dispatch_generation + 1,
           dispatch_attempt_count = dispatch_attempt_count + 1,
           last_enqueued_at = NULL,
           last_enqueue_error = 'worker lease expired before execution started',
           state_version = state_version + 1,
           active_started_at = NULL,
           updated_at = now()
      FROM expired
     WHERE runs.org_id = expired.org_id
       AND runs.id = expired.run_id
    RETURNING runs.id,
              runs.org_id,
              runs.worker_group_id,
              runs.project_id,
              runs.environment_id,
              runs.current_attempt_number,
              runs.trace_id,
              runs.root_span_id,
              runs.state_version,
              expired.run_lease_id
),
requeued_snapshots AS (
    INSERT INTO run_state_snapshots (org_id, worker_group_id, run_id, version, status, execution_status, attempt_number, run_lease_id, previous_version, transition, reason, error)
    SELECT updated_runs.org_id,
           updated_runs.worker_group_id,
           updated_runs.id,
           updated_runs.state_version,
           'queued',
           'queued',
           updated_runs.current_attempt_number,
           updated_runs.run_lease_id,
           updated_runs.state_version - 1,
           'run.dispatch_requeued',
           jsonb_build_object('message', 'worker lease expired before execution started'),
           jsonb_build_object('message', 'worker lease expired before execution started')
      FROM updated_runs
    RETURNING run_state_snapshots.run_id, run_state_snapshots.version
),
requeued_events AS (
    INSERT INTO telemetry_outbox (
        org_id, worker_group_id, stream_kind, source_kind, source_id, project_id,
        environment_id, run_id, deployment_id, run_lease_id, attempt_number,
        trace_id, span_id, parent_span_id, traceparent, category, severity, source,
        kind, message, payload, redaction_class, snapshot_version, observed_at
    )
    SELECT updated_runs.org_id,
           updated_runs.worker_group_id,
           'event',
           CASE WHEN NULL::uuid IS NOT NULL THEN 'deployment' ELSE 'run' END,
           COALESCE(NULL::uuid, updated_runs.id),
           updated_runs.project_id,
           updated_runs.environment_id,
           updated_runs.id,
           NULL::uuid,
           updated_runs.run_lease_id,
           updated_runs.current_attempt_number,
           updated_runs.trace_id,
           updated_runs.root_span_id,
           NULL::text,
           '00-' || updated_runs.trace_id || '-' || updated_runs.root_span_id || '-01',
           COALESCE(NULLIF('lifecycle', ''), 'system'),
           COALESCE(NULLIF('warn', ''), 'info'),
           COALESCE(NULLIF('control', ''), 'control'),
           'run.dispatch_requeued',
           COALESCE('run.dispatch_requeued', ''),
           COALESCE(jsonb_build_object('message', 'worker lease expired before execution started'), '{}'::jsonb),
           COALESCE(NULLIF('internal', ''), 'internal'),
           updated_runs.state_version,
           now()
      FROM updated_runs
      JOIN requeued_snapshots ON requeued_snapshots.run_id = updated_runs.id
    RETURNING id
),
released_workspace_leases AS (
    UPDATE workspace_leases
       SET state = 'released',
           released_at = COALESCE(released_at, now()),
           renewed_at = now(),
           updated_at = now()
      FROM updated_runs
     WHERE workspace_leases.org_id = updated_runs.org_id
       AND workspace_leases.owner_run_id = updated_runs.id
       AND workspace_leases.lease_kind = 'write'
       AND workspace_leases.state = 'active'
       AND workspace_leases.released_at IS NULL
    RETURNING workspace_leases.id
),
cleanup AS (
    SELECT (SELECT count(*) FROM released_workspace_leases) AS released_workspace_lease_count,
           (SELECT count(*) FROM requeued_events) AS requeued_telemetry_outbox_count
)
UPDATE run_leases
   SET lost_at = COALESCE(lost_at, now()),
       renewed_at = now(),
       status = 'lost'
  FROM expired
 WHERE run_leases.org_id = expired.org_id
   AND run_leases.id = expired.run_lease_id
   AND (SELECT released_workspace_lease_count + requeued_telemetry_outbox_count FROM cleanup) >= 0;

-- name: LockRunLeaseConcurrencyScope :exec
SELECT pg_advisory_xact_lock(hashtextextended(
           'run-queue-concurrency:' ||
           runs.org_id::text || ':' ||
           runs.worker_group_id || ':' ||
           runs.project_id::text || ':' ||
           runs.environment_id::text || ':' ||
           runs.queue_class || ':' ||
           runs.queue_name || ':' ||
           COALESCE(runs.concurrency_key, ''),
           0
       ))
  FROM runs
 WHERE runs.org_id = sqlc.arg(org_id)
   AND runs.id = sqlc.arg(run_id)
   AND COALESCE(runs.queue_concurrency_limit, 0) > 0;

-- name: AbandonLeasedRunLease :exec
WITH abandoned AS (
    UPDATE runs
       SET status = 'queued',
           execution_status = 'queued',
           current_run_lease_id = NULL,
           dispatch_generation = runs.dispatch_generation + 1,
           dispatch_attempt_count = dispatch_attempt_count + 1,
           last_enqueued_at = NULL,
           last_enqueue_error = 'worker payload build abandoned',
           state_version = state_version + 1,
           updated_at = now()
     WHERE runs.org_id = sqlc.arg(org_id)
       AND runs.id = sqlc.arg(run_id)
       AND runs.status = 'running'
       AND runs.current_run_lease_id = sqlc.arg(run_lease_id)
       AND EXISTS (
           SELECT 1
             FROM run_leases
            WHERE run_leases.org_id = sqlc.arg(org_id)
              AND run_leases.run_id = sqlc.arg(run_id)
              AND run_leases.id = sqlc.arg(run_lease_id)
              AND run_leases.worker_instance_id = sqlc.arg(worker_instance_id)
              AND run_leases.status = 'leased'
       )
    RETURNING runs.id,
              runs.org_id,
              runs.worker_group_id,
              runs.project_id,
              runs.environment_id,
              runs.current_attempt_number,
              runs.trace_id,
              runs.root_span_id,
              runs.state_version
),
abandoned_snapshots AS (
    INSERT INTO run_state_snapshots (org_id, worker_group_id, run_id, version, status, execution_status, attempt_number, run_lease_id, worker_instance_id, previous_version, transition, reason, error)
    SELECT abandoned.org_id,
           abandoned.worker_group_id,
           abandoned.id,
           abandoned.state_version,
           'queued',
           'queued',
           abandoned.current_attempt_number,
           sqlc.arg(run_lease_id),
           sqlc.arg(worker_instance_id),
           abandoned.state_version - 1,
           'run.dispatch_requeued',
           jsonb_build_object('message', 'worker payload build abandoned'),
           jsonb_build_object('message', 'worker payload build abandoned')
      FROM abandoned
    RETURNING run_state_snapshots.run_id, run_state_snapshots.version
),
abandoned_events AS (
    INSERT INTO telemetry_outbox (
        org_id, worker_group_id, stream_kind, source_kind, source_id, project_id,
        environment_id, run_id, deployment_id, run_lease_id, attempt_number,
        trace_id, span_id, parent_span_id, traceparent, category, severity, source,
        kind, message, payload, redaction_class, snapshot_version, observed_at
    )
    SELECT abandoned.org_id,
           abandoned.worker_group_id,
           'event',
           CASE WHEN NULL::uuid IS NOT NULL THEN 'deployment' ELSE 'run' END,
           COALESCE(NULL::uuid, abandoned.id),
           abandoned.project_id,
           abandoned.environment_id,
           abandoned.id,
           NULL::uuid,
           sqlc.arg(run_lease_id)::uuid,
           abandoned.current_attempt_number,
           abandoned.trace_id,
           abandoned.root_span_id,
           NULL::text,
           '00-' || abandoned.trace_id || '-' || abandoned.root_span_id || '-01',
           COALESCE(NULLIF('lifecycle', ''), 'system'),
           COALESCE(NULLIF('warn', ''), 'info'),
           COALESCE(NULLIF('control', ''), 'control'),
           'run.dispatch_requeued',
           COALESCE('run.dispatch_requeued', ''),
           COALESCE(jsonb_build_object('message', 'worker payload build abandoned'), '{}'::jsonb),
           COALESCE(NULLIF('internal', ''), 'internal'),
           abandoned.state_version,
           now()
      FROM abandoned
      JOIN abandoned_snapshots ON abandoned_snapshots.run_id = abandoned.id
    RETURNING id
),
released_workspace_leases AS (
    UPDATE workspace_leases
       SET state = 'released',
           released_at = COALESCE(released_at, now()),
           renewed_at = now(),
           updated_at = now()
      FROM abandoned
     WHERE workspace_leases.org_id = abandoned.org_id
       AND workspace_leases.owner_run_id = abandoned.id
       AND workspace_leases.lease_kind = 'write'
       AND workspace_leases.state = 'active'
       AND workspace_leases.released_at IS NULL
    RETURNING workspace_leases.id
),
cleanup AS (
    SELECT (SELECT count(*) FROM released_workspace_leases) AS released_workspace_lease_count,
           (SELECT count(*) FROM abandoned_events) AS abandoned_telemetry_outbox_count
)
UPDATE run_leases
   SET lost_at = COALESCE(lost_at, now()),
       renewed_at = now(),
       status = 'lost'
  FROM abandoned
 WHERE run_leases.org_id = abandoned.org_id
   AND run_leases.run_id = abandoned.id
   AND run_leases.id = sqlc.arg(run_lease_id)
   AND run_leases.worker_instance_id = sqlc.arg(worker_instance_id)
   AND run_leases.status = 'leased'
   AND (SELECT released_workspace_lease_count + abandoned_telemetry_outbox_count FROM cleanup) >= 0;

-- name: FailExpiredRunningRunLeases :exec
WITH expired AS (
    SELECT runs.*,
           run_leases.id AS source_run_lease_id,
           run_leases.worker_instance_id AS source_worker_instance_id,
           run_leases.attempt_number AS source_attempt_number,
           run_leases.trace_id AS source_trace_id,
           run_leases.span_id AS source_span_id,
           run_leases.parent_span_id AS source_parent_span_id,
           run_leases.traceparent AS source_traceparent,
           run_leases.restore_runtime_checkpoint_id AS source_restore_runtime_checkpoint_id
      FROM runs
      JOIN run_leases
        ON run_leases.org_id = runs.org_id
       AND run_leases.run_id = runs.id
       AND run_leases.id = runs.current_run_lease_id
     WHERE runs.org_id = sqlc.arg(org_id)
       AND runs.worker_group_id = sqlc.arg(worker_group_id)
       AND (
           runs.status = 'running'
           OR (
               runs.status = 'cancelled'
               AND runs.execution_status = 'pending_cancel'
           )
       )
       AND run_leases.status = 'running'
       AND run_leases.lease_expires_at <= now()
     FOR UPDATE OF runs, run_leases
),
effective_expiry AS (
    SELECT expired.id,
           CASE
             WHEN expired.status = 'cancelled' AND expired.execution_status = 'pending_cancel' THEN 'cancelled'::run_status
             ELSE 'failed'::run_status
           END AS terminal_status,
           CASE
             WHEN expired.status = 'cancelled' AND expired.execution_status = 'pending_cancel' THEN 'cancelled'::run_terminal_outcome
             ELSE 'failed'::run_terminal_outcome
           END AS terminal_outcome,
           CASE
             WHEN expired.status = 'cancelled' AND expired.execution_status = 'pending_cancel' THEN COALESCE(expired.error_message, 'run cancelled')
             ELSE 'worker lease expired while run was executing'
           END AS error_message,
           CASE
             WHEN expired.status = 'cancelled' AND expired.execution_status = 'pending_cancel' THEN 'run.cancelled'
             ELSE 'run.failed'
           END AS terminal_event_kind,
           CASE
             WHEN expired.status = 'cancelled' AND expired.execution_status = 'pending_cancel'
             THEN jsonb_build_object('reason', COALESCE(expired.error_message, 'run cancelled'), 'origin', 'lease_sweeper')
             ELSE jsonb_build_object('failure_kind', 'worker_lease_expired', 'detail', jsonb_build_object('message', 'worker lease expired while run was executing'))
           END AS terminal_event_payload,
           CASE
             WHEN expired.status = 'cancelled' AND expired.execution_status = 'pending_cancel' THEN '{}'::jsonb
             ELSE jsonb_build_object('failure_kind', 'worker_lease_expired', 'message', 'worker lease expired while run was executing')
           END AS error_payload
      FROM expired
),
retry_plan AS (
    SELECT expired.id AS run_id,
           expired.org_id,
           expired.worker_group_id,
           expired.project_id,
           expired.environment_id,
           expired.source_run_lease_id,
           expired.source_attempt_number,
           expired.source_attempt_number + 1 AS next_attempt_number,
           'transient_error'::text AS reason,
           delay.delay_ms,
           now() + ((delay.delay_ms::text || ' milliseconds')::interval) AS retry_after
      FROM expired
      JOIN effective_expiry ON effective_expiry.id = expired.id
      CROSS JOIN LATERAL (
          SELECT COALESCE(NULLIF(expired.locked_retry_policy ->> 'maxAttempts', '')::int, 1) AS max_attempts,
                 COALESCE(NULLIF(expired.locked_retry_policy #>> '{backoff,minMs}', '')::bigint, 1000) AS min_ms,
                 COALESCE(NULLIF(expired.locked_retry_policy #>> '{backoff,maxMs}', '')::bigint, 30000) AS max_ms,
                 COALESCE(NULLIF(expired.locked_retry_policy #>> '{backoff,factor}', '')::numeric, 2) AS factor,
                 COALESCE(NULLIF(expired.locked_retry_policy #>> '{backoff,jitter}', ''), 'full') AS jitter
      ) policy
      CROSS JOIN LATERAL (
          SELECT LEAST(
                     GREATEST(policy.max_ms, 0),
                     GREATEST(0, round(GREATEST(policy.min_ms, 0)::numeric * power(GREATEST(policy.factor, 0), expired.source_attempt_number - 1))::bigint)
                 ) AS base_delay_ms
      ) base_delay
      CROSS JOIN LATERAL (
          SELECT CASE
                   WHEN policy.jitter = 'full' THEN floor(random() * GREATEST(base_delay.base_delay_ms, 1))::bigint
                   ELSE base_delay.base_delay_ms
                 END AS delay_ms
      ) delay
     WHERE effective_expiry.terminal_status = 'failed'
       AND jsonb_typeof(expired.locked_retry_policy) = 'object'
       AND COALESCE((expired.locked_retry_policy ->> 'enabled')::boolean, false)
       AND expired.source_attempt_number < policy.max_attempts
),
failed_runs AS (
    UPDATE runs
       SET status = CASE WHEN retry_plan.run_id IS NOT NULL THEN 'queued'::run_status ELSE effective_expiry.terminal_status END,
           execution_status = CASE WHEN retry_plan.run_id IS NOT NULL THEN 'queued'::run_execution_status ELSE 'finished'::run_execution_status END,
           terminal_outcome = CASE WHEN retry_plan.run_id IS NOT NULL THEN NULL ELSE effective_expiry.terminal_outcome END,
           current_run_lease_id = NULL,
           current_attempt_number = COALESCE(retry_plan.next_attempt_number, runs.current_attempt_number),
           queue_timestamp = COALESCE(retry_plan.retry_after, runs.queue_timestamp),
           dispatch_generation = runs.dispatch_generation + 1,
           last_enqueued_at = NULL,
           last_enqueue_error = '',
           error_message = CASE WHEN retry_plan.run_id IS NOT NULL THEN NULL ELSE effective_expiry.error_message END,
           state_version = runs.state_version + CASE WHEN retry_plan.run_id IS NOT NULL THEN 2 ELSE 1 END,
           active_elapsed_ms = LEAST(
               runs.active_elapsed_ms
               + CASE
                   WHEN runs.active_started_at IS NULL THEN 0
                   ELSE GREATEST(floor(extract(epoch from (now() - runs.active_started_at)) * 1000)::bigint, 0)
                 END,
               runs.max_active_duration_ms
           ),
           active_started_at = NULL,
           finished_at = CASE WHEN retry_plan.run_id IS NOT NULL THEN NULL ELSE COALESCE(runs.finished_at, now()) END,
           updated_at = now()
      FROM expired
      JOIN effective_expiry ON effective_expiry.id = expired.id
      LEFT JOIN retry_plan ON retry_plan.run_id = expired.id
     WHERE runs.org_id = expired.org_id
       AND runs.id = expired.id
    RETURNING runs.*, expired.source_run_lease_id, expired.source_worker_instance_id, expired.source_attempt_number, expired.source_trace_id, expired.source_span_id, expired.source_parent_span_id, expired.source_traceparent, expired.source_restore_runtime_checkpoint_id
),
terminal_session_runs AS (
    UPDATE session_runs
       SET ended_at = now()
      FROM failed_runs
      LEFT JOIN retry_plan ON retry_plan.run_id = failed_runs.id
     WHERE session_runs.org_id = failed_runs.org_id
       AND retry_plan.run_id IS NULL
       AND session_runs.project_id = failed_runs.project_id
       AND session_runs.environment_id = failed_runs.environment_id
       AND session_runs.session_id = failed_runs.session_id
       AND session_runs.run_id = failed_runs.id
    RETURNING session_runs.id
),
released_workspace_leases AS (
    UPDATE workspace_leases
       SET state = 'released',
           released_at = COALESCE(released_at, now()),
           renewed_at = now(),
           updated_at = now()
      FROM failed_runs
     WHERE workspace_leases.org_id = failed_runs.org_id
       AND workspace_leases.owner_run_id = failed_runs.id
       AND workspace_leases.lease_kind = 'write'
       AND workspace_leases.state = 'active'
       AND workspace_leases.released_at IS NULL
    RETURNING workspace_leases.id
),
cancelled_run_waits AS (
    UPDATE run_waits
       SET state = 'cancelled',
           cancelled_at = now(),
           updated_at = now()
      FROM failed_runs
     WHERE run_waits.org_id = failed_runs.org_id
       AND run_waits.run_id = failed_runs.id
       AND run_waits.state IN ('hot_waiting', 'checkpointing', 'checkpointed_waiting', 'resuming')
    RETURNING run_waits.org_id, run_waits.run_id, run_waits.id, run_waits.wait_id
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
acknowledged_cancelled_worker_commands AS (
    UPDATE worker_commands
       SET acknowledged_at = COALESCE(worker_commands.acknowledged_at, now()),
           delivery_locked_until = NULL,
           updated_at = now()
      FROM cancelled_run_waits
     WHERE worker_commands.org_id = cancelled_run_waits.org_id
       AND worker_commands.run_id = cancelled_run_waits.run_id
       AND worker_commands.run_wait_id = cancelled_run_waits.id
       AND worker_commands.acknowledged_at IS NULL
    RETURNING worker_commands.id
),
invalidated_runtime_checkpoints AS (
    UPDATE runtime_checkpoints
       SET state = 'invalid',
           error_message = 'worker lease expired while run was executing',
           invalidated_at = now()
      FROM failed_runs
     WHERE runtime_checkpoints.org_id = failed_runs.org_id
       AND runtime_checkpoints.run_id = failed_runs.id
       AND runtime_checkpoints.state = 'creating'
    RETURNING runtime_checkpoints.id
),
failed_runtime_checkpoint_restores AS (
    UPDATE runtime_checkpoint_restores
       SET status = 'failed',
           error_message = 'worker lease expired while run was executing',
           finished_at = COALESCE(runtime_checkpoint_restores.finished_at, now()),
           updated_at = now()
      FROM failed_runs
     WHERE runtime_checkpoint_restores.org_id = failed_runs.org_id
       AND runtime_checkpoint_restores.run_id = failed_runs.id
       AND runtime_checkpoint_restores.run_lease_id = failed_runs.source_run_lease_id
       AND runtime_checkpoint_restores.runtime_checkpoint_id = failed_runs.source_restore_runtime_checkpoint_id
       AND runtime_checkpoint_restores.status = 'restoring'
    RETURNING runtime_checkpoint_restores.id
),
failed_snapshot AS (
    INSERT INTO run_state_snapshots (org_id, worker_group_id, run_id, version, status, execution_status, terminal_outcome, attempt_number, run_lease_id, worker_instance_id, runtime_checkpoint_id, previous_version, transition, reason, error)
    SELECT failed_runs.org_id,
           failed_runs.worker_group_id,
           failed_runs.id,
           CASE WHEN retry_plan.run_id IS NOT NULL THEN failed_runs.state_version - 1 ELSE failed_runs.state_version END,
           effective_expiry.terminal_status,
           'finished',
           effective_expiry.terminal_outcome,
           failed_runs.source_attempt_number,
           failed_runs.source_run_lease_id,
           failed_runs.source_worker_instance_id,
           failed_runs.source_restore_runtime_checkpoint_id,
           CASE WHEN retry_plan.run_id IS NOT NULL THEN failed_runs.state_version - 2 ELSE failed_runs.state_version - 1 END,
           effective_expiry.terminal_event_kind,
           effective_expiry.terminal_event_payload,
           effective_expiry.error_payload
      FROM failed_runs
      JOIN effective_expiry ON effective_expiry.id = failed_runs.id
      LEFT JOIN retry_plan ON retry_plan.run_id = failed_runs.id
    RETURNING run_state_snapshots.run_id, run_state_snapshots.version
),
retry_snapshot AS (
    INSERT INTO run_state_snapshots (org_id, worker_group_id, run_id, version, status, execution_status, attempt_number, run_lease_id, worker_instance_id, previous_version, transition, reason)
    SELECT failed_runs.org_id,
           failed_runs.worker_group_id,
           failed_runs.id,
           failed_runs.state_version,
           failed_runs.status,
           failed_runs.execution_status,
           failed_runs.current_attempt_number,
           failed_runs.source_run_lease_id,
           failed_runs.source_worker_instance_id,
           failed_runs.state_version - 1,
           'run.retry_scheduled',
           jsonb_build_object(
               'reason', retry_plan.reason,
               'previous_attempt_number', retry_plan.source_attempt_number,
               'next_attempt_number', retry_plan.next_attempt_number,
               'retry_after', retry_plan.retry_after,
               'delay_ms', retry_plan.delay_ms
           )
      FROM failed_runs
      JOIN retry_plan ON retry_plan.run_id = failed_runs.id
    RETURNING run_state_snapshots.run_id, run_state_snapshots.version
),
event_inputs(
    event_ordinal,
    org_id,
    worker_group_id,
    project_id,
    environment_id,
    run_id,
    run_lease_id,
    attempt_number,
    trace_id,
    span_id,
    parent_span_id,
    traceparent,
    category,
    severity,
    source,
    kind,
    message,
    payload,
    redaction_class,
    snapshot_version
) AS (
    SELECT 1 AS event_ordinal,
           failed_runs.org_id,
           failed_runs.worker_group_id,
           failed_runs.project_id,
           failed_runs.environment_id,
           failed_runs.id AS run_id,
           failed_runs.source_run_lease_id,
           failed_runs.source_attempt_number,
           failed_runs.source_trace_id,
           failed_runs.source_span_id,
           failed_runs.source_parent_span_id,
           failed_runs.source_traceparent,
           'lifecycle',
           CASE WHEN effective_expiry.terminal_status = 'cancelled' THEN 'warn' ELSE 'error' END,
           'lease_sweeper',
           effective_expiry.terminal_event_kind,
           effective_expiry.terminal_event_kind,
           effective_expiry.terminal_event_payload,
           'internal',
           failed_snapshot.version
      FROM failed_runs
      JOIN effective_expiry ON effective_expiry.id = failed_runs.id
      JOIN failed_snapshot ON failed_snapshot.run_id = failed_runs.id
    UNION ALL
    SELECT 2 AS event_ordinal,
           failed_runs.org_id,
           failed_runs.worker_group_id,
           failed_runs.project_id,
           failed_runs.environment_id,
           failed_runs.id AS run_id,
           NULL::uuid,
           failed_runs.current_attempt_number,
           failed_runs.trace_id,
           failed_runs.root_span_id,
           NULL::text,
           '00-' || failed_runs.trace_id || '-' || failed_runs.root_span_id || '-01',
           'lifecycle',
           'warn',
           'lease_sweeper',
           'run.retry_scheduled',
           'run.retry_scheduled',
           jsonb_build_object(
               'reason', retry_plan.reason,
               'previous_attempt_number', retry_plan.source_attempt_number,
               'next_attempt_number', retry_plan.next_attempt_number,
               'retry_after', retry_plan.retry_after,
               'delay_ms', retry_plan.delay_ms
           ),
           'internal',
           retry_snapshot.version
      FROM failed_runs
      JOIN retry_plan ON retry_plan.run_id = failed_runs.id
      JOIN retry_snapshot ON retry_snapshot.run_id = failed_runs.id
),
failed_events AS (
    INSERT INTO telemetry_outbox (
        org_id, worker_group_id, stream_kind, source_kind, source_id, project_id,
        environment_id, run_id, deployment_id, run_lease_id, attempt_number,
        trace_id, span_id, parent_span_id, traceparent, category, severity, source,
        kind, message, payload, redaction_class, snapshot_version, observed_at
    )
    SELECT event_inputs.org_id,
           event_inputs.worker_group_id,
           'event',
           CASE WHEN NULL::uuid IS NOT NULL THEN 'deployment' ELSE 'run' END,
           COALESCE(NULL::uuid, event_inputs.run_id),
           event_inputs.project_id,
           event_inputs.environment_id,
           event_inputs.run_id,
           NULL::uuid,
           event_inputs.run_lease_id,
           event_inputs.attempt_number,
           event_inputs.trace_id,
           event_inputs.span_id,
           event_inputs.parent_span_id,
           event_inputs.traceparent,
           COALESCE(NULLIF(event_inputs.category, ''), 'system'),
           COALESCE(NULLIF(event_inputs.severity, ''), 'info'),
           COALESCE(NULLIF(event_inputs.source, ''), 'control'),
           event_inputs.kind,
           COALESCE(event_inputs.message, ''),
           COALESCE(event_inputs.payload, '{}'::jsonb),
           COALESCE(NULLIF(event_inputs.redaction_class, ''), 'internal'),
           event_inputs.snapshot_version,
           now()
      FROM event_inputs
    RETURNING id
),
active_time_delta AS (
    SELECT GREATEST(
               failed_runs.active_elapsed_ms
               - COALESCE((
                   SELECT SUM(meter_events.quantity)::bigint
                     FROM meter_events
                    WHERE meter_events.org_id = failed_runs.org_id
                      AND meter_events.run_id = failed_runs.id
                      AND meter_events.meter = 'active_time'
               ), 0),
               0
           )::bigint AS quantity
      FROM failed_runs
),
active_time_meter_event AS (
    INSERT INTO meter_events (org_id, worker_group_id, project_id, environment_id, source_type, source_id, run_id, attempt_number, trace_id, span_id, meter, quantity, unit, measured_to, details, idempotency_key)
    SELECT failed_runs.org_id,
           failed_runs.worker_group_id,
           failed_runs.project_id,
           failed_runs.environment_id,
           'run_lease',
           failed_runs.source_run_lease_id,
           failed_runs.id,
           failed_runs.source_attempt_number,
           failed_runs.source_trace_id,
           failed_runs.source_span_id,
           'active_time',
           active_time_delta.quantity,
           'ms',
           now(),
           jsonb_build_object('phase', 'expired'),
           'active_time:' || failed_runs.source_run_lease_id::text || ':expired'
      FROM failed_runs
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
cleanup AS (
    SELECT
        (SELECT count(*) FROM terminal_session_runs) AS terminal_session_runs,
        (SELECT count(*) FROM released_workspace_leases) AS released_workspace_leases,
        (SELECT count(*) FROM cancelled_run_waits) AS cancelled_run_waits,
        (SELECT count(*) FROM acknowledged_cancelled_worker_commands) AS acknowledged_cancelled_worker_commands,
        (SELECT count(*) FROM invalidated_runtime_checkpoints) AS invalidated_runtime_checkpoints,
        (SELECT count(*) FROM failed_runtime_checkpoint_restores) AS failed_runtime_checkpoint_restores,
        (SELECT count(*) FROM failed_snapshot) AS failed_snapshots,
        (SELECT count(*) FROM retry_snapshot) AS retry_snapshots,
        (SELECT count(*) FROM failed_events) AS failed_events,
        (SELECT count(*) FROM failed_events) AS telemetry_outboxes,
        (SELECT count(*) FROM active_time_meter_event_outbox) AS active_time_meter_events
)
UPDATE run_leases
   SET lost_at = COALESCE(lost_at, now()),
       renewed_at = now(),
       active_duration_ms = failed_runs.active_elapsed_ms,
       status = 'lost'
  FROM failed_runs
 WHERE run_leases.org_id = failed_runs.org_id
   AND run_leases.id = failed_runs.source_run_lease_id
   AND (SELECT terminal_session_runs + released_workspace_leases + cancelled_run_waits + acknowledged_cancelled_worker_commands + invalidated_runtime_checkpoints + failed_runtime_checkpoint_restores + failed_snapshots + retry_snapshots + failed_events + telemetry_outboxes + active_time_meter_events FROM cleanup) >= 0;

-- name: LeaseRunLease :one
WITH worker_scope AS MATERIALIZED (
    SELECT worker_instances.*
      FROM worker_instances
     WHERE worker_instances.id = sqlc.arg(worker_instance_id)
       AND worker_instances.status = 'active'
     FOR UPDATE OF worker_instances
),
candidate AS MATERIALIZED (
    SELECT runs.*,
           worker_scope.protocol_version AS worker_protocol_version
      FROM runs
      JOIN worker_scope ON worker_scope.worker_group_id = runs.worker_group_id
     WHERE runs.org_id = sqlc.arg(org_id)
       AND runs.id = sqlc.arg(run_id)
       AND runs.status = 'queued'
       AND runs.current_run_lease_id IS NULL
       AND runs.dispatch_generation = sqlc.arg(dispatch_generation)
       AND runs.queue_timestamp <= now()
       AND (runs.queued_expires_at IS NULL OR runs.queued_expires_at > now())
       AND (
           runs.latest_runtime_checkpoint_id IS NULL
           OR EXISTS (
               SELECT 1
                 FROM runtime_checkpoints
                WHERE runtime_checkpoints.org_id = runs.org_id
                  AND runtime_checkpoints.worker_group_id = runs.worker_group_id
                  AND runtime_checkpoints.project_id = runs.project_id
                  AND runtime_checkpoints.environment_id = runs.environment_id
                  AND runtime_checkpoints.run_id = runs.id
                  AND runtime_checkpoints.id = runs.latest_runtime_checkpoint_id
                  AND runtime_checkpoints.state = 'ready'
                  AND (runtime_checkpoints.expires_at IS NULL OR runtime_checkpoints.expires_at > now())
                  AND EXISTS (
                      SELECT 1
                        FROM workspaces
                       WHERE workspaces.org_id = runs.org_id
                         AND workspaces.project_id = runs.project_id
                         AND workspaces.environment_id = runs.environment_id
                         AND workspaces.id = runs.workspace_id
                         AND workspaces.current_version_id = runtime_checkpoints.base_workspace_version_id
                  )
           )
       )
     FOR UPDATE OF runs
),
active_concurrency AS (
    SELECT count(run_leases.id)::int AS active_count
      FROM candidate
      JOIN run_leases
        ON run_leases.org_id = candidate.org_id
       AND run_leases.worker_group_id = candidate.worker_group_id
       AND run_leases.project_id = candidate.project_id
       AND run_leases.environment_id = candidate.environment_id
       AND run_leases.queue_class = candidate.queue_class
       AND run_leases.queue_name = candidate.queue_name
       AND run_leases.concurrency_key IS NOT DISTINCT FROM candidate.concurrency_key
       AND run_leases.status IN ('leased', 'running')
       AND run_leases.lease_expires_at > now()
),
concurrency_guard AS (
    SELECT candidate.*
      FROM candidate
      LEFT JOIN active_concurrency ON true
     WHERE COALESCE(candidate.queue_concurrency_limit, 0) <= 0
        OR COALESCE(active_concurrency.active_count, 0) < candidate.queue_concurrency_limit
),
workspace_write_lease AS (
    INSERT INTO workspace_leases (
        org_id,
        worker_group_id,
        project_id,
        environment_id,
        workspace_id,
        workspace_mount_id,
        lease_kind,
        owner_run_id,
        base_version_id,
        acquired_version_id,
        acquired_fencing_generation,
        fencing_token,
        heartbeat_token,
        expires_at
    )
    SELECT concurrency_guard.org_id,
           concurrency_guard.worker_group_id,
           concurrency_guard.project_id,
           concurrency_guard.environment_id,
           concurrency_guard.workspace_id,
           concurrency_guard.workspace_mount_id,
           'write',
           concurrency_guard.id,
           workspace_mounts.base_version_id,
           workspaces.current_version_id,
           GREATEST(workspace_mounts.fencing_generation, 0) + 1,
           uuidv7()::text,
           uuidv7()::text,
           sqlc.arg(lease_expires_at)
	      FROM concurrency_guard
	      JOIN workspaces ON workspaces.org_id = concurrency_guard.org_id
                    AND workspaces.project_id = concurrency_guard.project_id
                    AND workspaces.environment_id = concurrency_guard.environment_id
                    AND workspaces.id = concurrency_guard.workspace_id
      JOIN workspace_mounts ON workspace_mounts.org_id = concurrency_guard.org_id
                          AND workspace_mounts.project_id = concurrency_guard.project_id
                          AND workspace_mounts.environment_id = concurrency_guard.environment_id
                          AND workspace_mounts.workspace_id = concurrency_guard.workspace_id
                          AND workspace_mounts.id = concurrency_guard.workspace_mount_id
     WHERE concurrency_guard.workspace_id IS NOT NULL
       AND concurrency_guard.workspace_mount_id IS NOT NULL
    ON CONFLICT DO NOTHING
    RETURNING workspace_leases.*
),
workspace_lease_guard AS (
    SELECT 1
      FROM concurrency_guard
     WHERE concurrency_guard.workspace_id IS NULL
        OR concurrency_guard.workspace_mount_id IS NULL
        OR EXISTS (SELECT 1 FROM workspace_write_lease)
),
leased_run_lease AS (
    INSERT INTO run_leases (
        id,
        org_id,
        worker_group_id,
        project_id,
        environment_id,
        queue_class,
        queue_name,
        concurrency_key,
        run_id,
        worker_instance_id,
        dispatch_message_id,
        dispatch_generation,
        dispatch_lease_id,
        dispatch_attempt,
        attempt_number,
        status,
        lease_expires_at,
        runtime_id,
        worker_protocol_version,
        trace_id,
        span_id,
        parent_span_id,
        traceparent,
        restore_runtime_checkpoint_id
    )
    SELECT sqlc.arg(run_lease_id),
           concurrency_guard.org_id,
           concurrency_guard.worker_group_id,
           concurrency_guard.project_id,
           concurrency_guard.environment_id,
           concurrency_guard.queue_class,
           concurrency_guard.queue_name,
           concurrency_guard.concurrency_key,
           concurrency_guard.id,
           sqlc.arg(worker_instance_id),
           sqlc.arg(dispatch_message_id)::text,
           concurrency_guard.dispatch_generation,
           sqlc.arg(dispatch_lease_id),
           sqlc.arg(dispatch_attempt),
           concurrency_guard.current_attempt_number,
           'leased',
           sqlc.arg(lease_expires_at),
           concurrency_guard.runtime_id,
           concurrency_guard.worker_protocol_version,
           concurrency_guard.trace_id,
           sqlc.arg(run_lease_span_id),
           concurrency_guard.root_span_id,
           '00-' || concurrency_guard.trace_id || '-' || sqlc.arg(run_lease_span_id)::text || '-01',
           concurrency_guard.latest_runtime_checkpoint_id
      FROM concurrency_guard
      JOIN workspace_lease_guard ON true
    RETURNING *
),
updated AS (
    UPDATE runs
       SET status = 'running',
           execution_status = 'leased',
           current_run_lease_id = leased_run_lease.id,
           active_started_at = COALESCE(runs.active_started_at, now()),
           started_at = COALESCE(runs.started_at, now()),
           state_version = runs.state_version + 1,
           updated_at = now()
      FROM leased_run_lease
      JOIN workspace_lease_guard ON true
     WHERE runs.org_id = leased_run_lease.org_id
       AND runs.id = leased_run_lease.run_id
    RETURNING runs.*
),
leased_snapshot AS (
    INSERT INTO run_state_snapshots (org_id, worker_group_id, run_id, version, status, execution_status, attempt_number, run_lease_id, worker_instance_id, previous_version, transition, reason)
    SELECT updated.org_id,
           updated.worker_group_id,
           updated.id,
           updated.state_version,
           updated.status,
           updated.execution_status,
           updated.current_attempt_number,
           leased_run_lease.id,
           leased_run_lease.worker_instance_id,
           updated.state_version - 1,
           'run_lease.leased',
           '{}'::jsonb
      FROM updated, leased_run_lease
    RETURNING run_state_snapshots.run_id
),
runtime_checkpoint_restore AS (
    INSERT INTO runtime_checkpoint_restores (
        org_id,
        worker_group_id,
        project_id,
        environment_id,
        run_id,
        runtime_checkpoint_id,
        run_wait_id,
        run_lease_id,
        worker_instance_id
    )
    SELECT leased_run_lease.org_id,
           leased_run_lease.worker_group_id,
           leased_run_lease.project_id,
           leased_run_lease.environment_id,
           leased_run_lease.run_id,
           leased_run_lease.restore_runtime_checkpoint_id,
           runtime_checkpoints.owner_run_wait_id,
           leased_run_lease.id,
           leased_run_lease.worker_instance_id
      FROM leased_run_lease
      JOIN runtime_checkpoints
        ON runtime_checkpoints.org_id = leased_run_lease.org_id
       AND runtime_checkpoints.worker_group_id = leased_run_lease.worker_group_id
       AND runtime_checkpoints.project_id = leased_run_lease.project_id
       AND runtime_checkpoints.environment_id = leased_run_lease.environment_id
       AND runtime_checkpoints.run_id = leased_run_lease.run_id
       AND runtime_checkpoints.id = leased_run_lease.restore_runtime_checkpoint_id
       AND runtime_checkpoints.state = 'ready'
       AND (runtime_checkpoints.expires_at IS NULL OR runtime_checkpoints.expires_at > now())
     WHERE leased_run_lease.restore_runtime_checkpoint_id IS NOT NULL
    RETURNING id
),
runtime_checkpoint_restore_cleanup AS (
    SELECT count(*) AS restore_count
      FROM runtime_checkpoint_restore
)
SELECT
    updated.id,
    updated.org_id,
    updated.worker_group_id,
    updated.project_id,
    updated.environment_id,
    updated.session_id,
    updated.task_id,
    updated.deployment_version AS run_deployment_version,
    updated.api_version AS run_api_version,
    updated.sdk_version AS run_sdk_version,
    updated.cli_version AS run_cli_version,
    updated.status,
    updated.payload,
    updated.current_attempt_number,
    updated.state_version,
    deployment_tasks.id AS deployment_task_id,
    deployment_tasks.file_path AS deployment_task_file_path,
    deployment_tasks.export_name AS deployment_task_export_name,
    deployment_tasks.handler_entrypoint AS deployment_task_handler_entrypoint,
    task_bundle_artifacts.digest AS deployment_task_bundle_digest,
    deployment_tasks.bundle_format_version AS deployment_task_bundle_format_version,
    deployment_tasks.secret_declarations AS deployment_task_secret_declarations,
    deployments.version AS deployment_version,
    deployments.api_version AS deployment_api_version,
    deployments.sdk_version AS deployment_sdk_version,
    deployments.cli_version AS deployment_cli_version,
    deployments.worker_protocol_version AS deployment_worker_protocol_version,
    source_artifacts.digest AS deployment_source_digest,
    updated.max_active_duration_ms,
    updated.exit_code,
    updated.error_message,
    updated.created_at,
    updated.updated_at,
    updated.started_at,
    updated.finished_at,
    updated.requested_milli_cpu,
    updated.requested_memory_mib,
    updated.requested_disk_mib,
    updated.requested_execution_slots,
    updated.runtime_id AS requirements_runtime_id,
    updated.runtime_arch AS requirements_runtime_arch,
    updated.runtime_abi AS requirements_runtime_abi,
    updated.kernel_digest AS requirements_kernel_digest,
    updated.initramfs_digest AS requirements_initramfs_digest,
    updated.rootfs_digest AS requirements_rootfs_digest,
    updated.cni_profile AS requirements_cni_profile,
    updated.network_policy AS requirements_network_policy,
    updated.placement AS requirements_placement,
    leased_run_lease.id AS run_lease_id,
    leased_run_lease.worker_instance_id AS run_lease_worker_instance_id,
    leased_run_lease.dispatch_message_id AS run_lease_dispatch_message_id,
    leased_run_lease.dispatch_lease_id AS run_lease_dispatch_lease_id,
    leased_run_lease.dispatch_attempt AS run_lease_dispatch_attempt,
    leased_run_lease.attempt_number AS run_lease_attempt_number,
    leased_run_lease.lease_expires_at AS run_lease_expires_at,
    leased_run_lease.worker_protocol_version AS run_lease_worker_protocol_version,
    leased_run_lease.trace_id AS run_lease_trace_id,
    leased_run_lease.span_id AS run_lease_span_id,
    leased_run_lease.traceparent AS run_lease_traceparent,
    leased_run_lease.restore_runtime_checkpoint_id AS run_lease_restore_runtime_checkpoint_id,
    updated.active_elapsed_ms AS active_duration_ms,
    workspaces.id AS workspace_id,
    workspace_write_lease.id AS workspace_lease_id,
    workspace_mounts.id AS workspace_mount_id,
    COALESCE(workspace_mounts.fencing_generation, 0)::bigint AS workspace_mount_fencing_generation,
    COALESCE(workspace_write_lease.fencing_token, '')::text AS workspace_fencing_token,
    workspaces.deployment_sandbox_id AS workspace_deployment_sandbox_id,
    deployment_sandboxes.image_artifact_format AS workspace_sandbox_image_artifact_format,
    image_artifacts.digest AS workspace_sandbox_image_artifact_digest,
    image_artifacts.size_bytes AS workspace_sandbox_image_artifact_size_bytes,
    image_artifacts.media_type AS workspace_sandbox_image_artifact_media_type,
    deployment_sandboxes.image_digest AS workspace_sandbox_image_digest,
    deployment_sandboxes.image_format AS workspace_sandbox_image_format,
    deployment_sandboxes.rootfs_digest AS workspace_sandbox_rootfs_digest,
    deployment_sandboxes.runtime_abi AS workspace_runtime_abi,
    deployment_sandboxes.guestd_abi AS workspace_guestd_abi,
    deployment_sandboxes.adapter_abi AS workspace_adapter_abi,
    workspace_mounts.base_version_id AS workspace_base_version_id,
    deployment_sandboxes.workspace_mount_path AS workspace_mount_path,
    workspace_artifacts.digest AS workspace_artifact_digest,
    workspace_artifacts.size_bytes AS workspace_artifact_size_bytes,
    workspace_artifacts.media_type AS workspace_artifact_media_type,
    workspace_versions.artifact_encoding AS workspace_artifact_encoding,
    workspace_versions.artifact_entry_count AS workspace_artifact_entry_count,
    NULL::uuid AS workspace_runtime_substrate_artifact_id,
    ''::text AS workspace_runtime_substrate_digest,
    ''::text AS workspace_runtime_substrate_format,
    ''::text AS workspace_runtime_substrate_builder_abi,
    ''::text AS workspace_runtime_substrate_layout_abi,
    0::bigint AS workspace_runtime_substrate_size_bytes,
    ''::text AS workspace_runtime_substrate_artifact_digest,
    0::bigint AS workspace_runtime_substrate_artifact_size_bytes,
    ''::text AS workspace_runtime_substrate_artifact_media_type
FROM updated
JOIN leased_run_lease ON true
JOIN workspace_lease_guard ON true
JOIN leased_snapshot ON true
JOIN runtime_checkpoint_restore_cleanup ON runtime_checkpoint_restore_cleanup.restore_count >= 0
JOIN deployments ON deployments.org_id = updated.org_id
                AND deployments.id = updated.deployment_id
JOIN deployment_tasks ON deployment_tasks.org_id = updated.org_id
                     AND deployment_tasks.deployment_id = updated.deployment_id
                     AND deployment_tasks.id = updated.deployment_task_id
JOIN artifacts AS task_bundle_artifacts
  ON task_bundle_artifacts.org_id = deployment_tasks.org_id
 AND task_bundle_artifacts.project_id = deployment_tasks.project_id
 AND task_bundle_artifacts.environment_id = deployment_tasks.environment_id
 AND task_bundle_artifacts.id = deployment_tasks.bundle_artifact_id
JOIN artifacts AS source_artifacts
  ON source_artifacts.org_id = deployments.org_id
 AND source_artifacts.project_id = deployments.project_id
 AND source_artifacts.environment_id = deployments.environment_id
 AND source_artifacts.id = deployments.deployment_source_artifact_id
LEFT JOIN workspaces ON workspaces.org_id = updated.org_id
                    AND workspaces.project_id = updated.project_id
                    AND workspaces.environment_id = updated.environment_id
                    AND workspaces.id = updated.workspace_id
LEFT JOIN deployment_sandboxes ON deployment_sandboxes.org_id = workspaces.org_id
                              AND deployment_sandboxes.project_id = workspaces.project_id
                              AND deployment_sandboxes.environment_id = workspaces.environment_id
                              AND deployment_sandboxes.id = workspaces.deployment_sandbox_id
LEFT JOIN artifacts AS image_artifacts
  ON image_artifacts.org_id = deployment_sandboxes.org_id
 AND image_artifacts.project_id = deployment_sandboxes.project_id
 AND image_artifacts.environment_id = deployment_sandboxes.environment_id
 AND image_artifacts.id = deployment_sandboxes.image_artifact_id
LEFT JOIN workspace_mounts ON workspace_mounts.org_id = updated.org_id
                          AND workspace_mounts.project_id = updated.project_id
                          AND workspace_mounts.environment_id = updated.environment_id
                          AND workspace_mounts.id = updated.workspace_mount_id
LEFT JOIN workspace_write_lease ON workspace_write_lease.org_id = updated.org_id
                               AND workspace_write_lease.workspace_id = updated.workspace_id
                               AND workspace_write_lease.workspace_mount_id = updated.workspace_mount_id
LEFT JOIN workspace_versions ON workspace_versions.org_id = workspaces.org_id
                            AND workspace_versions.workspace_id = workspaces.id
                            AND workspace_versions.id = workspaces.current_version_id
LEFT JOIN artifacts AS workspace_artifacts
  ON workspace_artifacts.org_id = workspace_versions.org_id
 AND workspace_artifacts.project_id = updated.project_id
 AND workspace_artifacts.environment_id = updated.environment_id
 AND workspace_artifacts.id = workspace_versions.artifact_id;

-- name: StartRunLease :one
WITH current_run AS MATERIALIZED (
    SELECT runs.id, runs.org_id, runs.worker_group_id, runs.current_run_lease_id, runs.state_version
      FROM runs
     WHERE runs.org_id = sqlc.arg(org_id)
       AND runs.id = sqlc.arg(run_id)
       AND runs.current_run_lease_id = sqlc.arg(run_lease_id)
       AND runs.status = 'running'
     FOR UPDATE OF runs
),
started_lease AS (
    UPDATE run_leases
       SET status = 'running',
           started_at = COALESCE(started_at, now()),
           renewed_at = now()
      FROM current_run
     WHERE run_leases.org_id = current_run.org_id
       AND run_leases.run_id = current_run.id
       AND run_leases.id = sqlc.arg(run_lease_id)
       AND run_leases.worker_instance_id = sqlc.arg(worker_instance_id)
       AND run_leases.dispatch_message_id = sqlc.arg(dispatch_message_id)
       AND run_leases.dispatch_lease_id = sqlc.arg(dispatch_lease_id)
       AND run_leases.status = 'leased'
       AND run_leases.lease_expires_at > now()
    RETURNING run_leases.*
),
started_run AS (
    UPDATE runs
       SET execution_status = 'executing',
           dispatch_attempt_count = 0,
           state_version = runs.state_version + 1,
           updated_at = now()
      FROM started_lease
     WHERE runs.org_id = started_lease.org_id
       AND runs.id = started_lease.run_id
       AND runs.current_run_lease_id = started_lease.id
       AND runs.status = 'running'
    RETURNING runs.id, runs.org_id, runs.worker_group_id, runs.current_run_lease_id, runs.state_version
),
started_snapshot AS (
    INSERT INTO run_state_snapshots (org_id, worker_group_id, run_id, version, status, execution_status, attempt_number, run_lease_id, worker_instance_id, previous_version, transition, reason)
    SELECT started_run.org_id,
           started_run.worker_group_id,
           started_run.id,
           started_run.state_version,
           'running',
           'executing',
           started_lease.attempt_number,
           started_lease.id,
           started_lease.worker_instance_id,
           started_run.state_version - 1,
           'run_lease.started',
           '{}'::jsonb
      FROM started_run, started_lease
    RETURNING run_state_snapshots.run_id
)
SELECT started_lease.*
  FROM started_lease
  JOIN started_snapshot ON true;

-- name: RenewRunLease :one
WITH renewed_lease AS (
    UPDATE run_leases
       SET lease_expires_at = sqlc.arg(lease_expires_at),
           renewed_at = now()
      FROM runs
     WHERE run_leases.org_id = sqlc.arg(org_id)
       AND run_leases.run_id = sqlc.arg(run_id)
       AND run_leases.id = sqlc.arg(run_lease_id)
       AND run_leases.worker_instance_id = sqlc.arg(worker_instance_id)
       AND run_leases.dispatch_message_id = sqlc.arg(dispatch_message_id)
       AND run_leases.dispatch_lease_id = sqlc.arg(dispatch_lease_id)
       AND run_leases.status IN ('leased', 'running')
       AND run_leases.lease_expires_at > now()
       AND runs.org_id = run_leases.org_id
       AND runs.id = run_leases.run_id
       AND runs.worker_group_id = run_leases.worker_group_id
       AND runs.current_run_lease_id = run_leases.id
       AND runs.dispatch_generation = run_leases.dispatch_generation
       AND runs.status = 'running'
    RETURNING run_leases.*
),
renewed_workspace_lease AS (
    UPDATE workspace_leases
       SET expires_at = sqlc.arg(lease_expires_at),
           renewed_at = now(),
           updated_at = now()
      FROM renewed_lease
     WHERE workspace_leases.org_id = renewed_lease.org_id
       AND workspace_leases.owner_run_id = renewed_lease.run_id
       AND workspace_leases.lease_kind = 'write'
       AND workspace_leases.state = 'active'
       AND workspace_leases.released_at IS NULL
    RETURNING workspace_leases.id
),
cleanup AS (
    SELECT count(*) AS renewed_workspace_lease_count FROM renewed_workspace_lease
)
SELECT renewed_lease.id, renewed_lease.org_id, renewed_lease.run_id, renewed_lease.worker_instance_id, renewed_lease.worker_protocol_version, renewed_lease.dispatch_message_id, renewed_lease.dispatch_lease_id, renewed_lease.dispatch_attempt, renewed_lease.attempt_number, renewed_lease.lease_expires_at, renewed_lease.trace_id, renewed_lease.span_id, renewed_lease.traceparent
  FROM renewed_lease
 WHERE (SELECT renewed_workspace_lease_count FROM cleanup) >= 0;

-- name: GetRunLeaseQueueLease :one
SELECT run_leases.id,
       run_leases.org_id,
       run_leases.run_id,
       run_leases.worker_instance_id,
       run_leases.worker_protocol_version,
       run_leases.dispatch_message_id,
       run_leases.dispatch_lease_id,
       run_leases.dispatch_attempt,
       run_leases.attempt_number,
       run_leases.lease_expires_at,
       run_leases.trace_id,
       run_leases.span_id,
       run_leases.traceparent,
       run_leases.worker_group_id,
       run_leases.queue_class,
       run_leases.queue_name,
       runs.project_id,
       runs.environment_id,
       runs.deployment_id,
       runs.task_id,
       runs.session_id
  FROM run_leases
  JOIN runs ON runs.org_id = run_leases.org_id
           AND runs.id = run_leases.run_id
 WHERE run_leases.org_id = sqlc.arg(org_id)
   AND run_leases.worker_group_id = sqlc.arg(worker_group_id)
   AND run_leases.run_id = sqlc.arg(run_id)
   AND run_leases.id = sqlc.arg(run_lease_id)
   AND run_leases.worker_instance_id = sqlc.arg(worker_instance_id)
   AND run_leases.status IN ('leased', 'running')
   AND run_leases.lease_expires_at > now();

-- name: GetCurrentRunningRunLease :one
SELECT run_leases.id,
       run_leases.org_id,
       run_leases.run_id,
       run_leases.worker_instance_id,
       run_leases.worker_protocol_version,
       run_leases.dispatch_message_id,
       run_leases.dispatch_lease_id,
       run_leases.dispatch_attempt,
       run_leases.attempt_number,
       run_leases.lease_expires_at,
       run_leases.trace_id,
       run_leases.span_id,
       run_leases.traceparent,
       run_leases.worker_group_id,
       run_leases.queue_class,
       run_leases.queue_name,
       runs.project_id,
       runs.environment_id,
       runs.deployment_id,
       runs.task_id,
       runs.session_id
  FROM run_leases
  JOIN runs ON runs.org_id = run_leases.org_id
           AND runs.id = run_leases.run_id
           AND runs.current_run_lease_id = run_leases.id
 WHERE run_leases.org_id = sqlc.arg(org_id)
   AND run_leases.worker_group_id = sqlc.arg(worker_group_id)
   AND run_leases.run_id = sqlc.arg(run_id)
   AND run_leases.id = sqlc.arg(run_lease_id)
   AND run_leases.worker_instance_id = sqlc.arg(worker_instance_id)
   AND run_leases.status = 'running'
   AND run_leases.lease_expires_at > now();

-- name: GetRunLeaseRuntimeRelease :one
SELECT runtime_releases.*
  FROM run_leases
  JOIN runtime_releases ON runtime_releases.runtime_id = run_leases.runtime_id
 WHERE run_leases.org_id = sqlc.arg(org_id)
   AND run_leases.run_id = sqlc.arg(run_id)
   AND run_leases.id = sqlc.arg(run_lease_id)
   AND run_leases.worker_instance_id = sqlc.arg(worker_instance_id)
   AND run_leases.status IN ('leased', 'running')
   AND run_leases.lease_expires_at > now();

-- name: ReleaseRunLease :one
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
     FOR UPDATE OF sessions
),
eligible AS MATERIALIZED (
    SELECT runs.*,
           run_leases.id AS source_run_lease_id,
           run_leases.attempt_number AS source_attempt_number,
           run_leases.trace_id AS source_trace_id,
           run_leases.span_id AS source_span_id,
           run_leases.parent_span_id AS source_parent_span_id,
           run_leases.traceparent AS source_traceparent,
           run_leases.restore_runtime_checkpoint_id AS source_restore_runtime_checkpoint_id
      FROM runs
      JOIN run_leases
        ON run_leases.org_id = runs.org_id
       AND run_leases.run_id = runs.id
       AND run_leases.id = sqlc.arg(run_lease_id)
       AND run_leases.worker_instance_id = sqlc.arg(worker_instance_id)
       AND run_leases.dispatch_message_id = sqlc.arg(dispatch_message_id)
       AND run_leases.dispatch_lease_id = sqlc.arg(dispatch_lease_id)
       AND run_leases.status IN ('leased', 'running')
       AND run_leases.lease_expires_at > now()
      JOIN worker_instances
        ON worker_instances.id = sqlc.arg(worker_instance_id)
       AND worker_instances.worker_group_id = runs.worker_group_id
      JOIN worker_groups
        ON worker_groups.id = runs.worker_group_id
       AND worker_groups.state IN ('active', 'draining')
      LEFT JOIN locked_session
        ON locked_session.id = runs.session_id
     WHERE runs.org_id = sqlc.arg(org_id)
       AND runs.id = sqlc.arg(run_id)
       AND runs.current_run_lease_id = sqlc.arg(run_lease_id)
       AND locked_session.id = runs.session_id
       AND (
           runs.status = 'running'
           OR (
               runs.status = 'cancelled'
               AND runs.execution_status = 'pending_cancel'
           )
       )
       AND (
           sqlc.arg(run_status)::run_status <> 'succeeded'
           OR (
               runs.status = 'cancelled'
               AND runs.execution_status = 'pending_cancel'
           )
           OR (
               sqlc.narg(workspace_lease_id)::uuid IS NOT NULL
               AND sqlc.narg(workspace_fencing_token)::text IS NOT NULL
               AND sqlc.narg(workspace_artifact_digest)::text IS NOT NULL
               AND sqlc.narg(workspace_artifact_size_bytes)::bigint IS NOT NULL
               AND sqlc.narg(workspace_artifact_media_type)::text IS NOT NULL
               AND sqlc.narg(workspace_artifact_encoding)::text IS NOT NULL
               AND sqlc.narg(workspace_artifact_entry_count)::int IS NOT NULL
               AND NOT EXISTS (
                   SELECT 1
                     FROM cas_objects
                    WHERE cas_objects.org_id = runs.org_id
                      AND cas_objects.digest = sqlc.narg(workspace_artifact_digest)::text
                      AND (
                          cas_objects.size_bytes <> sqlc.narg(workspace_artifact_size_bytes)::bigint
                          OR cas_objects.media_type <> sqlc.narg(workspace_artifact_media_type)::text
                      )
               )
               AND EXISTS (
                   SELECT 1
                     FROM sessions
                     JOIN workspaces
                       ON workspaces.org_id = sessions.org_id
                      AND workspaces.project_id = sessions.project_id
                      AND workspaces.environment_id = sessions.environment_id
                      AND workspaces.id = sessions.workspace_id
                     JOIN workspace_leases
                       ON workspace_leases.org_id = workspaces.org_id
                      AND workspace_leases.project_id = workspaces.project_id
                      AND workspace_leases.environment_id = workspaces.environment_id
                      AND workspace_leases.workspace_id = workspaces.id
                      AND workspace_leases.owner_run_id = runs.id
                      AND workspace_leases.id = sqlc.narg(workspace_lease_id)::uuid
                      AND workspace_leases.fencing_token = sqlc.narg(workspace_fencing_token)::text
                      AND workspace_leases.lease_kind = 'write'
                      AND workspace_leases.state = 'active'
                      AND workspace_leases.base_version_id IS NOT DISTINCT FROM sqlc.narg(workspace_base_version_id)::uuid
                      AND workspace_leases.released_at IS NULL
                      AND workspace_leases.expires_at > now()
                    WHERE sessions.org_id = runs.org_id
                      AND sessions.project_id = runs.project_id
                      AND sessions.environment_id = runs.environment_id
                      AND sessions.id = runs.session_id
                      AND sessions.status = 'open'
                      AND sessions.current_run_id = runs.id
                      AND workspaces.state = 'active'
                      AND workspaces.current_version_id IS NOT DISTINCT FROM sqlc.narg(workspace_base_version_id)::uuid
               )
           )
       )
     FOR UPDATE OF runs, run_leases
),
effective_release AS (
    SELECT eligible.id,
           CASE
             WHEN eligible.status = 'cancelled' AND eligible.execution_status = 'pending_cancel' THEN 'cancelled'::run_status
             ELSE sqlc.arg(run_status)::run_status
           END AS run_status,
           CASE
             WHEN eligible.status = 'cancelled' AND eligible.execution_status = 'pending_cancel' THEN NULL::int
             ELSE sqlc.narg(exit_code)::int
           END AS exit_code,
           CASE
             WHEN eligible.status = 'cancelled' AND eligible.execution_status = 'pending_cancel' THEN NULL::jsonb
             ELSE sqlc.arg(output)::jsonb
           END AS output,
           CASE
             WHEN eligible.status = 'cancelled' AND eligible.execution_status = 'pending_cancel'
             THEN COALESCE(sqlc.narg(error_message)::text, 'run cancelled')::text
             ELSE sqlc.narg(error_message)::text
           END AS error_message,
           CASE
             WHEN eligible.status = 'cancelled' AND eligible.execution_status = 'pending_cancel' THEN 'run.cancelled'
             WHEN sqlc.arg(run_status)::run_status = 'succeeded' THEN 'run.completed'
             WHEN sqlc.arg(run_status)::run_status = 'cancelled' THEN 'run.cancelled'
             ELSE 'run.failed'
           END AS terminal_event_kind,
           CASE
             WHEN eligible.status = 'cancelled' AND eligible.execution_status = 'pending_cancel'
             THEN jsonb_build_object('reason', COALESCE(sqlc.narg(error_message)::text, 'run cancelled'), 'origin', 'cancel_operation')
             ELSE COALESCE(sqlc.arg(terminal_event_payload)::jsonb, '{}'::jsonb)
           END AS terminal_event_payload
      FROM eligible
),
retry_failure AS (
    SELECT eligible.id AS run_id,
           CASE
             WHEN effective_release.run_status <> 'failed' THEN ''
             WHEN (effective_release.terminal_event_payload ->> 'failure_kind') = 'max_duration' THEN 'timeout'
             WHEN effective_release.exit_code IS NOT NULL AND effective_release.exit_code <> 0 THEN 'non_zero_exit'
             WHEN (effective_release.terminal_event_payload ->> 'failure_kind') IN ('task_not_found', 'duplicate_task_id', 'missing_config', 'task_parse_failed') THEN 'non_retryable'
             ELSE 'transient_error'
           END AS reason
      FROM eligible
      JOIN effective_release ON effective_release.id = eligible.id
),
retry_plan AS (
    SELECT eligible.id AS run_id,
           eligible.org_id,
           eligible.worker_group_id,
           eligible.project_id,
           eligible.environment_id,
           eligible.source_run_lease_id,
           eligible.source_attempt_number,
           eligible.source_attempt_number + 1 AS next_attempt_number,
           retry_failure.reason,
           delay.delay_ms,
           now() + ((delay.delay_ms::text || ' milliseconds')::interval) AS retry_after
      FROM eligible
      JOIN effective_release ON effective_release.id = eligible.id
      JOIN retry_failure ON retry_failure.run_id = eligible.id
      CROSS JOIN LATERAL (
          SELECT COALESCE(NULLIF(eligible.locked_retry_policy ->> 'maxAttempts', '')::int, 1) AS max_attempts,
                 COALESCE(NULLIF(eligible.locked_retry_policy #>> '{backoff,minMs}', '')::bigint, 1000) AS min_ms,
                 COALESCE(NULLIF(eligible.locked_retry_policy #>> '{backoff,maxMs}', '')::bigint, 30000) AS max_ms,
                 COALESCE(NULLIF(eligible.locked_retry_policy #>> '{backoff,factor}', '')::numeric, 2) AS factor,
                 COALESCE(NULLIF(eligible.locked_retry_policy #>> '{backoff,jitter}', ''), 'full') AS jitter
      ) policy
      CROSS JOIN LATERAL (
          SELECT LEAST(
                     GREATEST(policy.max_ms, 0),
                     GREATEST(0, round(GREATEST(policy.min_ms, 0)::numeric * power(GREATEST(policy.factor, 0), eligible.source_attempt_number - 1))::bigint)
                 ) AS base_delay_ms
      ) base_delay
      CROSS JOIN LATERAL (
          SELECT CASE
                   WHEN policy.jitter = 'full' THEN floor(random() * GREATEST(base_delay.base_delay_ms, 1))::bigint
                   ELSE base_delay.base_delay_ms
                 END AS delay_ms
      ) delay
     WHERE effective_release.run_status = 'failed'
       AND jsonb_typeof(eligible.locked_retry_policy) = 'object'
       AND COALESCE((eligible.locked_retry_policy ->> 'enabled')::boolean, false)
       AND retry_failure.reason <> 'non_retryable'
       AND eligible.source_attempt_number < policy.max_attempts
),
released AS (
    UPDATE runs
       SET status = CASE WHEN retry_plan.run_id IS NOT NULL THEN 'queued'::run_status ELSE effective_release.run_status END,
           execution_status = CASE WHEN retry_plan.run_id IS NOT NULL THEN 'queued'::run_execution_status ELSE 'finished'::run_execution_status END,
           terminal_outcome = CASE WHEN retry_plan.run_id IS NOT NULL THEN NULL ELSE effective_release.run_status::text::run_terminal_outcome END,
           current_run_lease_id = NULL,
           current_attempt_number = COALESCE(retry_plan.next_attempt_number, runs.current_attempt_number),
           queue_timestamp = COALESCE(retry_plan.retry_after, runs.queue_timestamp),
           dispatch_generation = runs.dispatch_generation + 1,
           last_enqueued_at = NULL,
           last_enqueue_error = '',
           state_version = runs.state_version + CASE WHEN retry_plan.run_id IS NOT NULL THEN 2 ELSE 1 END,
           active_elapsed_ms = LEAST(
               runs.active_elapsed_ms
               + CASE
                   WHEN runs.active_started_at IS NULL THEN 0
                   ELSE GREATEST(floor(extract(epoch from (now() - runs.active_started_at)) * 1000)::bigint, 0)
                 END,
               runs.max_active_duration_ms
           ),
           active_started_at = NULL,
           output = CASE WHEN retry_plan.run_id IS NOT NULL THEN NULL ELSE effective_release.output END,
           exit_code = CASE WHEN retry_plan.run_id IS NOT NULL THEN NULL ELSE effective_release.exit_code END,
           error_message = CASE WHEN retry_plan.run_id IS NOT NULL THEN NULL ELSE effective_release.error_message END,
           finished_at = CASE WHEN retry_plan.run_id IS NOT NULL THEN NULL ELSE COALESCE(runs.finished_at, now()) END,
           updated_at = now()
      FROM eligible
      JOIN effective_release ON effective_release.id = eligible.id
      LEFT JOIN retry_plan ON retry_plan.run_id = eligible.id
     WHERE runs.org_id = eligible.org_id
       AND runs.id = eligible.id
    RETURNING runs.*, eligible.source_run_lease_id, eligible.source_attempt_number
),
released_run_lease AS (
    UPDATE run_leases
       SET status = CASE WHEN released.status = 'cancelled' THEN 'cancelled'::run_lease_status ELSE 'released'::run_lease_status END,
           released_at = COALESCE(released_at, now()),
           renewed_at = now(),
           active_duration_ms = released.active_elapsed_ms
      FROM released
     WHERE run_leases.org_id = released.org_id
       AND run_leases.run_id = released.id
       AND run_leases.id = sqlc.arg(run_lease_id)
    RETURNING run_leases.*
),
released_session_run AS (
    UPDATE session_runs
       SET ended_at = now()
      FROM released
      LEFT JOIN retry_plan ON retry_plan.run_id = released.id
     WHERE retry_plan.run_id IS NULL
       AND session_runs.org_id = released.org_id
       AND session_runs.project_id = released.project_id
       AND session_runs.environment_id = released.environment_id
       AND session_runs.session_id = released.session_id
       AND session_runs.run_id = released.id
    RETURNING session_runs.id
),
workspace_commit_input AS (
    SELECT released.org_id,
           released.worker_group_id,
           released.project_id,
           released.environment_id,
           released.id AS run_id,
           released.workspace_id,
           workspace_leases.id AS workspace_lease_id,
           workspace_leases.workspace_mount_id,
           workspace_leases.base_version_id,
           uuidv7() AS artifact_id,
           uuidv7() AS workspace_version_id,
           sqlc.narg(workspace_version_public_id)::text AS workspace_version_public_id,
           sqlc.narg(workspace_artifact_digest)::text AS artifact_digest,
           sqlc.narg(workspace_artifact_size_bytes)::bigint AS artifact_size_bytes,
           sqlc.narg(workspace_artifact_media_type)::text AS artifact_media_type,
           sqlc.narg(workspace_artifact_encoding)::text AS artifact_encoding,
           sqlc.narg(workspace_artifact_entry_count)::int AS artifact_entry_count
      FROM released
      JOIN effective_release ON effective_release.id = released.id
      JOIN workspaces
        ON workspaces.org_id = released.org_id
       AND workspaces.project_id = released.project_id
       AND workspaces.environment_id = released.environment_id
       AND workspaces.id = released.workspace_id
       AND workspaces.state = 'active'
      JOIN workspace_leases
        ON workspace_leases.org_id = workspaces.org_id
       AND workspace_leases.project_id = workspaces.project_id
       AND workspace_leases.environment_id = workspaces.environment_id
       AND workspace_leases.workspace_id = workspaces.id
       AND workspace_leases.owner_run_id = released.id
       AND workspace_leases.id = sqlc.narg(workspace_lease_id)::uuid
       AND workspace_leases.fencing_token = sqlc.narg(workspace_fencing_token)::text
       AND workspace_leases.lease_kind = 'write'
       AND workspace_leases.state = 'active'
       AND workspace_leases.base_version_id IS NOT DISTINCT FROM sqlc.narg(workspace_base_version_id)::uuid
       AND workspace_leases.released_at IS NULL
     WHERE released.status = 'succeeded'
       AND effective_release.run_status = 'succeeded'
       AND workspaces.current_version_id IS NOT DISTINCT FROM workspace_leases.base_version_id
),
published_workspace_cas_object AS (
    INSERT INTO cas_objects (org_id, digest, size_bytes, media_type)
    SELECT workspace_commit_input.org_id,
           workspace_commit_input.artifact_digest,
           workspace_commit_input.artifact_size_bytes,
           workspace_commit_input.artifact_media_type
      FROM workspace_commit_input
    ON CONFLICT (org_id, digest) DO UPDATE
       SET size_bytes = cas_objects.size_bytes
     WHERE cas_objects.size_bytes = EXCLUDED.size_bytes
       AND cas_objects.media_type = EXCLUDED.media_type
    RETURNING org_id, digest
),
inserted_workspace_artifact AS (
    INSERT INTO artifacts (
        id,
        org_id,
        project_id,
        environment_id,
        digest,
        kind,
        size_bytes,
        media_type,
        created_by_worker_instance_id
    )
    SELECT workspace_commit_input.artifact_id,
           workspace_commit_input.org_id,
           workspace_commit_input.project_id,
           workspace_commit_input.environment_id,
           workspace_commit_input.artifact_digest,
           'workspace_version'::artifact_kind,
           workspace_commit_input.artifact_size_bytes,
           workspace_commit_input.artifact_media_type,
           sqlc.arg(worker_instance_id)
      FROM workspace_commit_input
      JOIN published_workspace_cas_object
        ON published_workspace_cas_object.org_id = workspace_commit_input.org_id
       AND published_workspace_cas_object.digest = workspace_commit_input.artifact_digest
    RETURNING id
),
published_workspace_version AS (
    INSERT INTO workspace_versions (
        id,
        public_id,
        org_id,
        project_id,
        environment_id,
        workspace_id,
        parent_version_id,
        source_workspace_mount_id,
        source_write_lease_id,
        kind,
        state,
        artifact_id,
        artifact_encoding,
        artifact_entry_count,
        content_digest,
        size_bytes,
        produced_by_run_id,
        promoted_at
    )
    SELECT workspace_commit_input.workspace_version_id,
           workspace_commit_input.workspace_version_public_id,
           workspace_commit_input.org_id,
           workspace_commit_input.project_id,
           workspace_commit_input.environment_id,
           workspace_commit_input.workspace_id,
           workspace_commit_input.base_version_id,
           workspace_commit_input.workspace_mount_id,
           workspace_commit_input.workspace_lease_id,
           'user',
           'ready',
           workspace_commit_input.artifact_id,
           workspace_commit_input.artifact_encoding,
           workspace_commit_input.artifact_entry_count,
           workspace_commit_input.artifact_digest,
           workspace_commit_input.artifact_size_bytes,
           workspace_commit_input.run_id,
           now()
      FROM workspace_commit_input
      JOIN inserted_workspace_artifact ON inserted_workspace_artifact.id = workspace_commit_input.artifact_id
    RETURNING id, org_id, project_id, environment_id, workspace_id, parent_version_id
),
advanced_workspace AS (
    UPDATE workspaces
       SET current_version_id = published_workspace_version.id,
           dirty_state = 'clean',
           last_activity_at = now(),
           updated_at = now()
      FROM published_workspace_version
     WHERE workspaces.org_id = published_workspace_version.org_id
       AND workspaces.project_id = published_workspace_version.project_id
       AND workspaces.environment_id = published_workspace_version.environment_id
       AND workspaces.id = published_workspace_version.workspace_id
       AND workspaces.current_version_id IS NOT DISTINCT FROM published_workspace_version.parent_version_id
    RETURNING workspaces.id
),
released_workspace_lease AS (
    UPDATE workspace_leases
       SET state = 'released',
           released_at = COALESCE(released_at, now()),
           renewed_at = now(),
           updated_at = now()
      FROM released
     WHERE workspace_leases.org_id = released.org_id
       AND workspace_leases.owner_run_id = released.id
       AND workspace_leases.lease_kind = 'write'
       AND workspace_leases.state = 'active'
       AND workspace_leases.released_at IS NULL
    RETURNING workspace_leases.id, workspace_leases.workspace_id, workspace_leases.workspace_mount_id
),
waiting_runtime_instance AS (
    UPDATE runtime_instances
       SET state = 'waiting_hot',
           owner_run_id = NULL,
           owner_run_lease_id = NULL,
           owner_run_wait_id = NULL,
           owner_run_state_version = NULL,
           waiting_at = now(),
           updated_at = now()
      FROM released
      JOIN workspace_mounts
        ON workspace_mounts.org_id = released.org_id
       AND workspace_mounts.project_id = released.project_id
       AND workspace_mounts.environment_id = released.environment_id
       AND workspace_mounts.id = released.workspace_mount_id
      JOIN workspaces
        ON workspaces.org_id = workspace_mounts.org_id
       AND workspaces.project_id = workspace_mounts.project_id
       AND workspaces.environment_id = workspace_mounts.environment_id
       AND workspaces.id = workspace_mounts.workspace_id
      JOIN released_workspace_lease
        ON released_workspace_lease.workspace_id = workspace_mounts.workspace_id
       AND released_workspace_lease.workspace_mount_id = workspace_mounts.id
     WHERE released.status = 'succeeded'
       AND runtime_instances.org_id = workspace_mounts.org_id
       AND runtime_instances.id = workspace_mounts.runtime_instance_id
       AND runtime_instances.worker_instance_id = sqlc.arg(worker_instance_id)
       AND runtime_instances.workspace_mount_id = workspace_mounts.id
       AND runtime_instances.state = 'running'
       AND workspace_mounts.state = 'mounted'
       AND workspace_mounts.dirty_generation = 0
       AND workspaces.state = 'active'
       AND workspaces.deleted_at IS NULL
       AND NOT EXISTS (
           SELECT 1
             FROM workspace_leases AS active_workspace_leases
            WHERE active_workspace_leases.org_id = workspace_mounts.org_id
              AND active_workspace_leases.project_id = workspace_mounts.project_id
              AND active_workspace_leases.environment_id = workspace_mounts.environment_id
              AND active_workspace_leases.workspace_id = workspace_mounts.workspace_id
              AND active_workspace_leases.workspace_mount_id = workspace_mounts.id
              AND active_workspace_leases.id <> released_workspace_lease.id
              AND active_workspace_leases.state IN ('active', 'releasing')
              AND active_workspace_leases.expires_at > now()
       )
       AND NOT EXISTS (
           SELECT 1
             FROM workspace_processes
            WHERE workspace_processes.org_id = workspace_mounts.org_id
              AND workspace_processes.project_id = workspace_mounts.project_id
              AND workspace_processes.environment_id = workspace_mounts.environment_id
              AND workspace_processes.workspace_id = workspace_mounts.workspace_id
              AND (workspace_processes.workspace_mount_id = workspace_mounts.id OR workspace_processes.workspace_mount_id IS NULL)
              AND workspace_processes.kind = 'command'
              AND workspace_processes.state IN ('queued', 'starting', 'running')
       )
       AND NOT EXISTS (
           SELECT 1
             FROM workspace_processes
            WHERE workspace_processes.org_id = workspace_mounts.org_id
              AND workspace_processes.project_id = workspace_mounts.project_id
              AND workspace_processes.environment_id = workspace_mounts.environment_id
              AND workspace_processes.workspace_id = workspace_mounts.workspace_id
              AND (workspace_processes.workspace_mount_id = workspace_mounts.id OR workspace_processes.workspace_mount_id IS NULL)
              AND workspace_processes.kind = 'pty'
              AND workspace_processes.state IN ('starting', 'running', 'closing')
       )
    RETURNING runtime_instances.id
),
cancelled_run_waits AS (
    UPDATE run_waits
       SET state = 'cancelled',
           cancelled_at = now(),
           updated_at = now()
      FROM released
     WHERE run_waits.org_id = released.org_id
       AND run_waits.run_id = released.id
       AND run_waits.state IN ('hot_waiting', 'checkpointing', 'checkpointed_waiting', 'resuming')
    RETURNING run_waits.org_id, run_waits.run_id, run_waits.id, run_waits.wait_id
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
acknowledged_cancelled_worker_commands AS (
    UPDATE worker_commands
       SET acknowledged_at = COALESCE(worker_commands.acknowledged_at, now()),
           delivery_locked_until = NULL,
           updated_at = now()
      FROM cancelled_run_waits
     WHERE worker_commands.org_id = cancelled_run_waits.org_id
       AND worker_commands.run_id = cancelled_run_waits.run_id
       AND worker_commands.run_wait_id = cancelled_run_waits.id
       AND worker_commands.acknowledged_at IS NULL
    RETURNING worker_commands.id
),
invalidated_runtime_checkpoints AS (
    UPDATE runtime_checkpoints
       SET state = 'invalid',
           error_message = COALESCE(released.error_message, 'run lease released'),
           invalidated_at = now()
      FROM released
     WHERE runtime_checkpoints.org_id = released.org_id
       AND runtime_checkpoints.run_id = released.id
       AND runtime_checkpoints.state = 'creating'
    RETURNING runtime_checkpoints.id
),
completed_runtime_checkpoint_restore AS (
    UPDATE runtime_checkpoint_restores
       SET status = 'restored',
           error_message = NULL,
           finished_at = COALESCE(runtime_checkpoint_restores.finished_at, now()),
           updated_at = now()
      FROM released
      JOIN released_run_lease ON true
      JOIN effective_release ON effective_release.id = released.id
     WHERE runtime_checkpoint_restores.org_id = released.org_id
       AND runtime_checkpoint_restores.run_id = released.id
       AND runtime_checkpoint_restores.run_lease_id = released_run_lease.id
       AND runtime_checkpoint_restores.runtime_checkpoint_id = released_run_lease.restore_runtime_checkpoint_id
       AND runtime_checkpoint_restores.status = 'restoring'
       AND effective_release.run_status = 'succeeded'
    RETURNING runtime_checkpoint_restores.id
),
failed_runtime_checkpoint_restore AS (
    UPDATE runtime_checkpoint_restores
       SET status = 'failed',
           error_message = COALESCE(effective_release.error_message, effective_release.terminal_event_kind),
           finished_at = COALESCE(runtime_checkpoint_restores.finished_at, now()),
           updated_at = now()
      FROM released
      JOIN released_run_lease ON true
      JOIN effective_release ON effective_release.id = released.id
     WHERE runtime_checkpoint_restores.org_id = released.org_id
       AND runtime_checkpoint_restores.run_id = released.id
       AND runtime_checkpoint_restores.run_lease_id = released_run_lease.id
       AND runtime_checkpoint_restores.runtime_checkpoint_id = released_run_lease.restore_runtime_checkpoint_id
       AND runtime_checkpoint_restores.status = 'restoring'
       AND effective_release.run_status <> 'succeeded'
    RETURNING runtime_checkpoint_restores.id
),
active_time_delta AS (
    SELECT GREATEST(
               released_run_lease.active_duration_ms
               - COALESCE((
                   SELECT SUM(meter_events.quantity)::bigint
                     FROM meter_events
                    WHERE meter_events.org_id = released.org_id
                      AND meter_events.run_id = released.id
                      AND meter_events.meter = 'active_time'
               ), 0),
               0
           )::bigint AS quantity
      FROM released
      JOIN released_run_lease ON true
),
active_time_meter_event AS (
    INSERT INTO meter_events (org_id, worker_group_id, project_id, environment_id, source_type, source_id, run_id, attempt_number, trace_id, span_id, meter, quantity, unit, measured_to, details, idempotency_key)
    SELECT released.org_id,
           released.worker_group_id,
           released.project_id,
           released.environment_id,
           'run_lease',
           released_run_lease.id,
           released.id,
           released_run_lease.attempt_number,
           released_run_lease.trace_id,
           released_run_lease.span_id,
           'active_time',
           active_time_delta.quantity,
           'ms',
           now(),
           jsonb_build_object('phase', 'final'),
           'active_time:' || released_run_lease.id::text || ':final'
      FROM released
      JOIN released_run_lease ON true
     JOIN active_time_delta ON true
    WHERE active_time_delta.quantity > 0
    ON CONFLICT DO NOTHING
    RETURNING *
),
output_meter_event AS (
    INSERT INTO meter_events (org_id, worker_group_id, project_id, environment_id, source_type, source_id, run_id, attempt_number, trace_id, span_id, meter, quantity, unit, measured_to, details, idempotency_key)
    SELECT released.org_id,
           released.worker_group_id,
           released.project_id,
           released.environment_id,
           'run_lease',
           released_run_lease.id,
           released.id,
           released_run_lease.attempt_number,
           released_run_lease.trace_id,
           released_run_lease.span_id,
           'output_bytes',
           octet_length(effective_release.output::text)::bigint,
           'bytes',
           now(),
           jsonb_build_object('terminal_event_kind', effective_release.terminal_event_kind),
           'output:' || released_run_lease.id::text || ':final'
      FROM released
      JOIN released_run_lease ON true
      JOIN effective_release ON effective_release.id = released.id
     WHERE effective_release.output IS NOT NULL
       AND octet_length(effective_release.output::text) > 0
    ON CONFLICT DO NOTHING
    RETURNING *
),
meter_event_outbox AS (
    INSERT INTO telemetry_outbox (
        org_id, worker_group_id, stream_kind, source_kind, source_id, project_id,
        environment_id, run_id, attempt_number, trace_id, span_id, kind, payload,
        idempotency_key, observed_at
    )
    SELECT meter_event.org_id,
           meter_event.worker_group_id,
           'meter_event',
           meter_event.source_type,
           meter_event.source_id,
           meter_event.project_id,
           meter_event.environment_id,
           meter_event.run_id,
           meter_event.attempt_number,
           meter_event.trace_id,
           meter_event.span_id,
           meter_event.meter,
           meter_event.details,
           meter_event.idempotency_key,
           meter_event.occurred_at
      FROM (
          SELECT * FROM active_time_meter_event
          UNION ALL
          SELECT * FROM output_meter_event
      ) meter_event
    ON CONFLICT DO NOTHING
    RETURNING id
),
released_snapshot AS (
    INSERT INTO run_state_snapshots (org_id, worker_group_id, run_id, version, status, execution_status, terminal_outcome, attempt_number, run_lease_id, worker_instance_id, previous_version, transition, reason, error)
    SELECT released.org_id,
           released.worker_group_id,
           released.id,
           CASE WHEN retry_plan.run_id IS NOT NULL THEN released.state_version - 1 ELSE released.state_version END,
           effective_release.run_status,
           'finished',
           effective_release.run_status::text::run_terminal_outcome,
           released.source_attempt_number,
           released.source_run_lease_id,
           released_run_lease.worker_instance_id,
           CASE WHEN retry_plan.run_id IS NOT NULL THEN released.state_version - 2 ELSE released.state_version - 1 END,
           CASE
             WHEN effective_release.run_status = 'succeeded' THEN 'run.completed'
             WHEN effective_release.run_status = 'cancelled' THEN 'run.cancelled'
             ELSE 'run.failed'
           END,
           effective_release.terminal_event_payload,
           CASE
             WHEN effective_release.run_status = 'failed' THEN effective_release.terminal_event_payload
             ELSE '{}'::jsonb
           END
      FROM released
      JOIN effective_release ON effective_release.id = released.id
      JOIN released_run_lease ON true
      LEFT JOIN retry_plan ON retry_plan.run_id = released.id
    RETURNING run_state_snapshots.run_id, run_state_snapshots.version
),
retry_snapshot AS (
    INSERT INTO run_state_snapshots (org_id, worker_group_id, run_id, version, status, execution_status, attempt_number, run_lease_id, worker_instance_id, previous_version, transition, reason)
    SELECT released.org_id,
           released.worker_group_id,
           released.id,
           released.state_version,
           released.status,
           released.execution_status,
           released.current_attempt_number,
           released.source_run_lease_id,
           released_run_lease.worker_instance_id,
           released.state_version - 1,
           'run.retry_scheduled',
           jsonb_build_object(
               'reason', retry_plan.reason,
               'previous_attempt_number', retry_plan.source_attempt_number,
               'next_attempt_number', retry_plan.next_attempt_number,
               'retry_after', retry_plan.retry_after,
               'delay_ms', retry_plan.delay_ms
           )
      FROM released
      JOIN retry_plan ON retry_plan.run_id = released.id
      JOIN released_run_lease ON true
    RETURNING run_state_snapshots.run_id
),
event_inputs(
    event_ordinal,
    org_id,
    worker_group_id,
    project_id,
    environment_id,
    run_id,
    run_lease_id,
    attempt_number,
    trace_id,
    span_id,
    parent_span_id,
    traceparent,
    category,
    severity,
    source,
    kind,
    message,
    payload,
    redaction_class,
    snapshot_version
) AS (
    SELECT 1 AS event_ordinal,
           released.org_id,
           released.worker_group_id,
           released.project_id,
           released.environment_id,
           released.id AS run_id,
           released_run_lease.id AS run_lease_id,
           released_run_lease.attempt_number,
           released_run_lease.trace_id,
           released_run_lease.span_id,
           released_run_lease.parent_span_id,
           released_run_lease.traceparent,
           'lifecycle' AS category,
           CASE WHEN effective_release.run_status = 'succeeded' THEN 'info' ELSE 'error' END AS severity,
           'control' AS source,
           effective_release.terminal_event_kind AS kind,
           effective_release.terminal_event_kind AS message,
           effective_release.terminal_event_payload AS payload,
           'internal' AS redaction_class,
           released_snapshot.version AS snapshot_version
      FROM released
      JOIN released_run_lease ON true
      JOIN released_snapshot ON released_snapshot.run_id = released.id
      JOIN effective_release ON effective_release.id = released.id
    UNION ALL
    SELECT 2 AS event_ordinal,
           released.org_id,
           released.worker_group_id,
           released.project_id,
           released.environment_id,
           released.id AS run_id,
           NULL::uuid,
           released.current_attempt_number,
           released.trace_id,
           released.root_span_id,
           NULL::text,
           '00-' || released.trace_id || '-' || released.root_span_id || '-01',
           'lifecycle',
           'warn',
           'control',
           'run.retry_scheduled',
           'run.retry_scheduled',
           jsonb_build_object(
               'reason', retry_plan.reason,
               'previous_attempt_number', retry_plan.source_attempt_number,
               'next_attempt_number', retry_plan.next_attempt_number,
               'retry_after', retry_plan.retry_after,
               'delay_ms', retry_plan.delay_ms
           ),
           'internal',
           released.state_version
      FROM released
      JOIN retry_plan ON retry_plan.run_id = released.id
      JOIN retry_snapshot ON retry_snapshot.run_id = released.id
),
events AS (
    INSERT INTO telemetry_outbox (
        org_id, worker_group_id, stream_kind, source_kind, source_id, project_id,
        environment_id, run_id, deployment_id, run_lease_id, attempt_number,
        trace_id, span_id, parent_span_id, traceparent, category, severity, source,
        kind, message, payload, redaction_class, snapshot_version, observed_at
    )
    SELECT event_inputs.org_id,
           event_inputs.worker_group_id,
           'event',
           CASE WHEN NULL::uuid IS NOT NULL THEN 'deployment' ELSE 'run' END,
           COALESCE(NULL::uuid, event_inputs.run_id),
           event_inputs.project_id,
           event_inputs.environment_id,
           event_inputs.run_id,
           NULL::uuid,
           event_inputs.run_lease_id,
           event_inputs.attempt_number,
           event_inputs.trace_id,
           event_inputs.span_id,
           event_inputs.parent_span_id,
           event_inputs.traceparent,
           COALESCE(NULLIF(event_inputs.category, ''), 'system'),
           COALESCE(NULLIF(event_inputs.severity, ''), 'info'),
           COALESCE(NULLIF(event_inputs.source, ''), 'control'),
           event_inputs.kind,
           COALESCE(event_inputs.message, ''),
           COALESCE(event_inputs.payload, '{}'::jsonb),
           COALESCE(NULLIF(event_inputs.redaction_class, ''), 'internal'),
           event_inputs.snapshot_version,
           now()
      FROM event_inputs
    RETURNING id
),
cleanup AS (
    SELECT
        (SELECT count(*) FROM released_session_run) AS released_session_runs,
        (SELECT count(*) FROM published_workspace_version) AS workspace_versions,
        (SELECT count(*) FROM advanced_workspace) AS advanced_workspaces,
        (SELECT count(*) FROM released_workspace_lease) AS released_workspace_leases,
        (SELECT count(*) FROM waiting_runtime_instance) AS waiting_runtime_instances,
        (SELECT count(*) FROM cancelled_run_waits) AS cancelled_run_waits,
        (SELECT count(*) FROM acknowledged_cancelled_worker_commands) AS acknowledged_cancelled_worker_commands,
        (SELECT count(*) FROM invalidated_runtime_checkpoints) AS invalidated_runtime_checkpoints,
        (SELECT count(*) FROM completed_runtime_checkpoint_restore) AS completed_runtime_checkpoint_restores,
        (SELECT count(*) FROM failed_runtime_checkpoint_restore) AS failed_runtime_checkpoint_restores,
        (SELECT count(*) FROM active_time_meter_event) AS active_time_meter_events,
        (SELECT count(*) FROM output_meter_event) AS output_meter_events,
        (SELECT count(*) FROM meter_event_outbox) AS meter_event_outboxes,
        (SELECT count(*) FROM released_snapshot) AS released_snapshots,
        (SELECT count(*) FROM retry_snapshot) AS retry_snapshots,
        (SELECT count(*) FROM events) AS events,
        (SELECT count(*) FROM telemetry_outbox) AS telemetry_outboxes
),
idempotent_released AS (
    SELECT runs.*,
           run_leases.id AS source_run_lease_id,
           run_leases.attempt_number AS source_attempt_number
      FROM runs
      JOIN run_leases
        ON run_leases.org_id = runs.org_id
       AND run_leases.run_id = runs.id
       AND run_leases.id = sqlc.arg(run_lease_id)
       AND run_leases.worker_instance_id = sqlc.arg(worker_instance_id)
       AND run_leases.dispatch_message_id = sqlc.arg(dispatch_message_id)
       AND run_leases.dispatch_lease_id = sqlc.arg(dispatch_lease_id)
       AND run_leases.status IN ('released', 'cancelled')
     WHERE runs.org_id = sqlc.arg(org_id)
       AND runs.id = sqlc.arg(run_id)
       AND runs.current_run_lease_id IS NULL
       AND NOT EXISTS (SELECT 1 FROM released)
       AND (
           (
               runs.status = sqlc.arg(run_status)::run_status
               AND runs.exit_code IS NOT DISTINCT FROM sqlc.narg(exit_code)::int
               AND runs.error_message IS NOT DISTINCT FROM sqlc.narg(error_message)::text
               AND runs.output IS NOT DISTINCT FROM sqlc.arg(output)::jsonb
           )
           OR (
               sqlc.arg(run_status)::run_status = 'failed'
               AND runs.status = 'queued'
               AND runs.execution_status = 'queued'
           )
           OR (
               runs.status = 'cancelled'
               AND runs.execution_status = 'finished'
           )
       )
)
SELECT released.*,
       released_run_lease.id AS run_lease_id,
       released_run_lease.worker_instance_id AS run_lease_worker_instance_id,
       released_run_lease.dispatch_message_id AS run_lease_dispatch_message_id,
       released_run_lease.dispatch_lease_id AS run_lease_dispatch_lease_id,
       released_run_lease.dispatch_attempt AS run_lease_dispatch_attempt,
       released_run_lease.attempt_number AS run_lease_attempt_number,
       released_run_lease.lease_expires_at AS run_lease_expires_at,
       released_run_lease.worker_protocol_version AS run_lease_worker_protocol_version,
       released_run_lease.trace_id AS run_lease_trace_id,
       released_run_lease.span_id AS run_lease_span_id,
       released_run_lease.traceparent AS run_lease_traceparent
  FROM released
  JOIN released_run_lease ON true
  JOIN released_snapshot ON released_snapshot.run_id = released.id
 WHERE (SELECT released_session_runs + workspace_versions + advanced_workspaces + released_workspace_leases + waiting_runtime_instances + cancelled_run_waits + acknowledged_cancelled_worker_commands + invalidated_runtime_checkpoints + completed_runtime_checkpoint_restores + failed_runtime_checkpoint_restores + active_time_meter_events + output_meter_events + meter_event_outboxes + released_snapshots + retry_snapshots + events + telemetry_outboxes FROM cleanup) >= 0
UNION ALL
SELECT idempotent_released.*,
       run_leases.id AS run_lease_id,
       run_leases.worker_instance_id AS run_lease_worker_instance_id,
       run_leases.dispatch_message_id AS run_lease_dispatch_message_id,
       run_leases.dispatch_lease_id AS run_lease_dispatch_lease_id,
       run_leases.dispatch_attempt AS run_lease_dispatch_attempt,
       run_leases.attempt_number AS run_lease_attempt_number,
       run_leases.lease_expires_at AS run_lease_expires_at,
       run_leases.worker_protocol_version AS run_lease_worker_protocol_version,
       run_leases.trace_id AS run_lease_trace_id,
       run_leases.span_id AS run_lease_span_id,
       run_leases.traceparent AS run_lease_traceparent
  FROM idempotent_released
  JOIN run_leases
    ON run_leases.org_id = idempotent_released.org_id
   AND run_leases.run_id = idempotent_released.id
   AND run_leases.id = idempotent_released.source_run_lease_id;
