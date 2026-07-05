-- name: RequeueExpiredLeasedRunLeases :exec
WITH eligible AS (
    SELECT runs.id AS run_id,
           run_leases.id AS run_lease_id,
           run_attempts.attempt_number,
           run_leases.restore_runtime_checkpoint_id
      FROM runs
      JOIN run_leases ON run_leases.id = runs.current_run_lease_id
                          AND run_leases.org_id = runs.org_id
                          AND run_leases.run_id = runs.id
      JOIN run_attempts ON run_attempts.org_id = run_leases.org_id
                       AND run_attempts.run_id = run_leases.run_id
                       AND run_attempts.id = run_leases.attempt_id
     WHERE runs.org_id = sqlc.arg(org_id)
       AND runs.cell_id = sqlc.arg(cell_id)
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
           state_version = state_version + 1,
           updated_at = now()
      FROM eligible
     WHERE runs.id = eligible.run_id
       AND runs.status = 'running'
       AND runs.current_run_lease_id = eligible.run_lease_id
    RETURNING eligible.run_id, eligible.run_lease_id, eligible.attempt_number, eligible.restore_runtime_checkpoint_id, runs.cell_id, runs.project_id, runs.environment_id, runs.current_attempt_id, runs.state_version, runs.queued_expires_at
),
requeued_attempts AS (
    UPDATE run_attempts
       SET status = 'queued',
           updated_at = now()
      FROM updated_runs
     WHERE run_attempts.org_id = sqlc.arg(org_id)
       AND run_attempts.cell_id = sqlc.arg(cell_id)
       AND run_attempts.run_id = updated_runs.run_id
       AND run_attempts.id = updated_runs.current_attempt_id
    RETURNING run_attempts.id, run_attempts.run_id
),
abandoned_runtime_checkpoint_restore AS (
    UPDATE runtime_checkpoint_restores
       SET status = 'abandoned',
           error_message = 'worker lease expired before execution started',
           finished_at = COALESCE(runtime_checkpoint_restores.finished_at, now()),
           updated_at = now()
      FROM updated_runs
     WHERE runtime_checkpoint_restores.org_id = sqlc.arg(org_id)
       AND runtime_checkpoint_restores.cell_id = sqlc.arg(cell_id)
       AND runtime_checkpoint_restores.run_id = updated_runs.run_id
       AND runtime_checkpoint_restores.run_lease_id = updated_runs.run_lease_id
       AND runtime_checkpoint_restores.runtime_checkpoint_id = updated_runs.restore_runtime_checkpoint_id
       AND runtime_checkpoint_restores.status = 'restoring'
    RETURNING runtime_checkpoint_restores.id
),
requeued_queue_entries AS (
    UPDATE run_queue_items
       SET status = 'queued',
           dispatch_message_id = NULL,
           reserved_by_worker_instance_id = NULL,
           reservation_expires_at = NULL,
           queued_expires_at = updated_runs.queued_expires_at,
           dispatch_generation = dispatch_generation + 1,
           last_error = 'worker lease expired before execution started',
           enqueued_at = now(),
           updated_at = now(),
           finished_at = NULL
      FROM updated_runs
      JOIN run_leases ON run_leases.org_id = sqlc.arg(org_id)
                         AND run_leases.cell_id = sqlc.arg(cell_id)
                         AND run_leases.run_id = updated_runs.run_id
                         AND run_leases.id = updated_runs.run_lease_id
     WHERE run_queue_items.org_id = sqlc.arg(org_id)
       AND run_queue_items.cell_id = sqlc.arg(cell_id)
       AND run_queue_items.run_id = updated_runs.run_id
       AND run_queue_items.reserved_by_worker_instance_id = run_leases.worker_instance_id
       AND run_queue_items.dispatch_message_id = run_leases.dispatch_message_id
       AND run_queue_items.status = 'reserved'
    RETURNING run_queue_items.run_id
),
released_concurrency_slots AS (
    UPDATE run_queue_concurrency_leases
       SET released_at = now()
      FROM updated_runs
     WHERE run_queue_concurrency_leases.org_id = sqlc.arg(org_id)
       AND run_queue_concurrency_leases.run_id = updated_runs.run_id
       AND run_queue_concurrency_leases.run_lease_id = updated_runs.run_lease_id
       AND run_queue_concurrency_leases.released_at IS NULL
    RETURNING run_queue_concurrency_leases.id
),
released_workspace_leases AS (
    UPDATE workspace_leases
       SET state = 'released',
           released_at = now(),
           renewed_at = now(),
           updated_at = now()
      FROM updated_runs
     WHERE workspace_leases.org_id = sqlc.arg(org_id)
       AND workspace_leases.owner_run_id = updated_runs.run_id
       AND workspace_leases.lease_kind = 'write'
       AND workspace_leases.released_at IS NULL
    RETURNING workspace_leases.id
),
lost_event_seq AS (
    INSERT INTO event_cursors (org_id, cell_id, subject_kind, subject_id, seq)
    SELECT sqlc.arg(org_id), updated_runs.cell_id, 'run', updated_runs.run_id, 1
      FROM updated_runs
    ON CONFLICT (org_id, cell_id, subject_kind, subject_id)
    DO UPDATE SET seq = event_cursors.seq + 1,
                  observed_at = now()
    RETURNING org_id, subject_kind, subject_id, seq
),
lost_events AS (
    INSERT INTO event_hot_payloads (org_id, cell_id, project_id, environment_id, run_id, seq, attempt_id, run_lease_id, attempt_number, trace_id, span_id, parent_span_id, traceparent, category, severity, source, kind, message, payload, redaction_class, snapshot_version)
    SELECT sqlc.arg(org_id),
           updated_runs.cell_id,
           updated_runs.project_id,
           updated_runs.environment_id,
           updated_runs.run_id,
           lost_event_seq.seq,
           run_leases.attempt_id,
           updated_runs.run_lease_id,
           updated_runs.attempt_number,
           run_leases.trace_id,
           run_leases.span_id,
           run_leases.parent_span_id,
           run_leases.traceparent,
           'worker',
           'warn',
           'lease_sweeper',
           'run.execution_lost',
           'run.execution_lost',
           jsonb_build_object(
               'reason', 'worker lease expired before execution started',
               'origin', 'lease_sweeper'
           ),
           'internal',
           updated_runs.state_version
      FROM updated_runs
      JOIN run_leases ON run_leases.org_id = sqlc.arg(org_id)
                                  AND run_leases.run_id = updated_runs.run_id
                                  AND run_leases.id = updated_runs.run_lease_id
      JOIN lost_event_seq ON lost_event_seq.org_id = sqlc.arg(org_id)
                         AND lost_event_seq.subject_kind = 'run'
                         AND lost_event_seq.subject_id = updated_runs.run_id
    RETURNING *
),
lost_telemetry_outbox AS (
    INSERT INTO telemetry_outbox (org_id, cell_id, stream_kind, source_kind, source_id, seq, idempotency_key)
    SELECT lost_events.org_id,
                  lost_events.cell_id,
                  'event',
                  lost_events.subject_type,
                  lost_events.subject_id,
                  lost_events.seq,
                  'event:' || lost_events.subject_type::text || ':' || lost_events.subject_id::text || ':' || lost_events.seq::text
      FROM lost_events
    RETURNING id
),
lost_snapshots AS (
    INSERT INTO run_snapshots (org_id, cell_id, run_id, version, status, execution_status, attempt_id, run_lease_id, transition, reason)
    SELECT sqlc.arg(org_id),
           updated_runs.cell_id,
           updated_runs.run_id,
           updated_runs.state_version,
           'queued',
           'queued',
           updated_runs.current_attempt_id,
           updated_runs.run_lease_id,
           'run_lease.lost_requeued',
           jsonb_build_object('reason', 'worker lease expired before execution started', 'origin', 'lease_sweeper')
      FROM updated_runs
      JOIN requeued_attempts ON requeued_attempts.run_id = updated_runs.run_id
    RETURNING run_snapshots.run_id
),
cleanup AS (
    SELECT
        (SELECT count(*) FROM abandoned_runtime_checkpoint_restore) AS abandoned_runtime_checkpoint_restore_count,
        (SELECT count(*) FROM requeued_queue_entries) AS requeued_queue_entry_count,
        (SELECT count(*) FROM released_concurrency_slots) AS released_concurrency_slot_count,
        (SELECT count(*) FROM released_workspace_leases) AS released_workspace_lease_count,
        (SELECT count(*) FROM lost_telemetry_outbox) AS lost_event_count,
        (SELECT count(*) FROM lost_snapshots) AS lost_snapshot_count
)
UPDATE run_leases
   SET lost_at = COALESCE(lost_at, now()),
       renewed_at = now(),
       status = 'lost'
 FROM updated_runs
 WHERE run_leases.id = updated_runs.run_lease_id
   AND run_leases.run_id = updated_runs.run_id
   AND (SELECT abandoned_runtime_checkpoint_restore_count + requeued_queue_entry_count + released_concurrency_slot_count + released_workspace_lease_count + lost_event_count + lost_snapshot_count FROM cleanup) >= 0;

-- name: AbandonLeasedRunLease :exec
WITH abandoned AS (
    UPDATE runs
       SET status = 'queued',
           execution_status = 'queued',
           current_run_lease_id = NULL,
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
    RETURNING runs.id, runs.org_id, runs.cell_id, runs.current_attempt_id, runs.state_version
),
requeued_attempt AS (
    UPDATE run_attempts
       SET status = 'queued',
           updated_at = now()
      FROM abandoned
     WHERE run_attempts.org_id = abandoned.org_id
       AND run_attempts.run_id = abandoned.id
       AND run_attempts.id = abandoned.current_attempt_id
    RETURNING run_attempts.id, run_attempts.run_id
),
abandoned_runtime_checkpoint_restore AS (
    UPDATE runtime_checkpoint_restores
       SET status = 'abandoned',
           error_message = 'worker lease abandoned before execution started',
           finished_at = COALESCE(runtime_checkpoint_restores.finished_at, now()),
           updated_at = now()
      FROM abandoned
     WHERE runtime_checkpoint_restores.org_id = sqlc.arg(org_id)
       AND runtime_checkpoint_restores.run_id = abandoned.id
       AND runtime_checkpoint_restores.run_lease_id = sqlc.arg(run_lease_id)
       AND runtime_checkpoint_restores.status = 'restoring'
    RETURNING runtime_checkpoint_restores.id
),
released_concurrency_slots AS (
    UPDATE run_queue_concurrency_leases
       SET released_at = now()
      FROM abandoned
     WHERE run_queue_concurrency_leases.org_id = sqlc.arg(org_id)
       AND run_queue_concurrency_leases.run_id = abandoned.id
       AND run_queue_concurrency_leases.run_lease_id = sqlc.arg(run_lease_id)
       AND run_queue_concurrency_leases.released_at IS NULL
    RETURNING run_queue_concurrency_leases.id
),
released_workspace_leases AS (
    UPDATE workspace_leases
       SET state = 'released',
           released_at = now(),
           renewed_at = now(),
           updated_at = now()
      FROM abandoned
     WHERE workspace_leases.org_id = sqlc.arg(org_id)
       AND workspace_leases.owner_run_id = abandoned.id
       AND workspace_leases.lease_kind = 'write'
       AND workspace_leases.released_at IS NULL
    RETURNING workspace_leases.id
),
abandoned_snapshot AS (
    INSERT INTO run_snapshots (org_id, cell_id, run_id, version, status, execution_status, attempt_id, run_lease_id, transition, reason)
    SELECT abandoned.org_id,
           abandoned.cell_id,
           abandoned.id,
           abandoned.state_version,
           'queued',
           'queued',
           abandoned.current_attempt_id,
           sqlc.arg(run_lease_id),
           'run_lease.abandoned_requeued',
           jsonb_build_object('reason', 'worker payload build abandoned')
      FROM abandoned
      JOIN requeued_attempt ON true
    RETURNING run_snapshots.run_id
),
cleanup AS (
    SELECT
        (SELECT count(*) FROM abandoned_runtime_checkpoint_restore) AS abandoned_runtime_checkpoint_restore_count,
        (SELECT count(*) FROM released_concurrency_slots) AS released_concurrency_slot_count,
        (SELECT count(*) FROM released_workspace_leases) AS released_workspace_lease_count,
        (SELECT count(*) FROM abandoned_snapshot) AS abandoned_snapshot_count
)
UPDATE run_leases
   SET lost_at = COALESCE(lost_at, now()),
       renewed_at = now(),
       status = 'lost'
  FROM abandoned
 WHERE run_leases.org_id = sqlc.arg(org_id)
   AND run_leases.run_id = abandoned.id
   AND run_leases.id = sqlc.arg(run_lease_id)
   AND run_leases.worker_instance_id = sqlc.arg(worker_instance_id)
   AND run_leases.status = 'leased'
   AND (SELECT abandoned_runtime_checkpoint_restore_count + released_concurrency_slot_count + released_workspace_lease_count + abandoned_snapshot_count FROM cleanup) >= 0;

-- name: FailExpiredRunningRunLeases :exec
WITH locked_sessions AS MATERIALIZED (
    SELECT sessions.org_id,
           sessions.id
      FROM runs
      JOIN run_leases ON run_leases.id = runs.current_run_lease_id
                          AND run_leases.org_id = runs.org_id
                          AND run_leases.run_id = runs.id
      JOIN sessions
        ON sessions.org_id = runs.org_id
       AND sessions.project_id = runs.project_id
       AND sessions.environment_id = runs.environment_id
       AND sessions.id = runs.session_id
     WHERE runs.org_id = sqlc.arg(org_id)
       AND runs.cell_id = sqlc.arg(cell_id)
       AND (
           runs.status = 'running'
           OR (
               runs.status = 'cancelled'
               AND runs.execution_status = 'pending_cancel'
           )
       )
       AND run_leases.status = 'running'
       AND run_leases.lease_expires_at <= now()
     FOR UPDATE OF sessions
),
eligible AS (
    SELECT runs.org_id,
           runs.cell_id,
           runs.id AS run_id,
           runs.project_id,
           runs.environment_id,
           runs.current_attempt_id AS previous_attempt_id,
           run_attempts.attempt_number AS previous_attempt_number,
           run_leases.id AS run_lease_id,
           run_leases.restore_runtime_checkpoint_id,
           runs.status AS previous_status,
           runs.execution_status AS previous_execution_status,
           runs.locked_retry_policy
      FROM runs
      JOIN run_leases ON run_leases.id = runs.current_run_lease_id
                          AND run_leases.org_id = runs.org_id
                          AND run_leases.run_id = runs.id
      JOIN run_attempts ON run_attempts.org_id = run_leases.org_id
                       AND run_attempts.run_id = run_leases.run_id
                       AND run_attempts.id = run_leases.attempt_id
      LEFT JOIN locked_sessions
        ON locked_sessions.org_id = runs.org_id
       AND locked_sessions.id = runs.session_id
     WHERE runs.org_id = sqlc.arg(org_id)
       AND runs.cell_id = sqlc.arg(cell_id)
       AND locked_sessions.id = runs.session_id
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
retry_candidate AS (
    SELECT eligible.run_id,
           eligible.org_id,
           eligible.cell_id,
           eligible.project_id,
           eligible.environment_id,
           eligible.previous_attempt_id,
           eligible.previous_attempt_number,
           uuidv7() AS next_attempt_id,
           eligible.previous_attempt_number + 1 AS next_attempt_number,
           'infra_lost'::text AS reason,
           delay.delay_ms,
           now() + ((delay.delay_ms::text || ' milliseconds')::interval) AS retry_after,
           eligible.locked_retry_policy
      FROM eligible
      CROSS JOIN LATERAL (
          SELECT (eligible.locked_retry_policy ->> 'maxAttempts')::int AS max_attempts,
                 COALESCE(NULLIF(eligible.locked_retry_policy #>> '{backoff,minMs}', '')::bigint, 1000) AS min_ms,
                 COALESCE(NULLIF(eligible.locked_retry_policy #>> '{backoff,maxMs}', '')::bigint, 30000) AS max_ms,
                 COALESCE(NULLIF(eligible.locked_retry_policy #>> '{backoff,factor}', '')::numeric, 2) AS factor,
                 COALESCE(NULLIF(eligible.locked_retry_policy #>> '{backoff,jitter}', ''), 'full') AS jitter
      ) policy
      CROSS JOIN LATERAL (
          SELECT LEAST(
                     GREATEST(policy.max_ms, 0),
                     GREATEST(
                         0,
                         round(GREATEST(policy.min_ms, 0)::numeric * power(GREATEST(policy.factor, 0), eligible.previous_attempt_number - 1))::bigint
                     )
                 ) AS base_delay_ms
      ) base_delay
      CROSS JOIN LATERAL (
          SELECT CASE
                   WHEN policy.jitter = 'full' THEN floor(random() * GREATEST(base_delay.base_delay_ms, 1))::bigint
                   ELSE base_delay.base_delay_ms
                 END AS delay_ms
      ) delay
         WHERE jsonb_typeof(eligible.locked_retry_policy) = 'object'
           AND COALESCE((eligible.locked_retry_policy ->> 'enabled')::boolean, true)
           AND eligible.previous_status = 'running'
           AND eligible.previous_attempt_number < policy.max_attempts
    ),
checkpointing_dirty_loss_scope AS MATERIALIZED (
    SELECT eligible.run_id,
           eligible.project_id,
           eligible.environment_id,
           workspace_leases.workspace_id,
           run_waits.id AS run_wait_id,
           workspace_mounts.id AS workspace_mount_id,
           workspace_mounts.dirty_generation
      FROM eligible
      JOIN run_waits ON run_waits.org_id = eligible.org_id
                    AND run_waits.project_id = eligible.project_id
                    AND run_waits.environment_id = eligible.environment_id
                    AND run_waits.run_id = eligible.run_id
                    AND run_waits.state = 'checkpointing'
                    AND run_waits.workspace_version_id IS NULL
      JOIN workspace_leases
        ON workspace_leases.org_id = eligible.org_id
       AND workspace_leases.project_id = eligible.project_id
       AND workspace_leases.environment_id = eligible.environment_id
       AND workspace_leases.owner_run_id = eligible.run_id
       AND workspace_leases.lease_kind = 'write'
       AND workspace_leases.state IN ('active', 'releasing')
       AND workspace_leases.released_at IS NULL
      JOIN workspace_mounts
        ON workspace_mounts.org_id = workspace_leases.org_id
       AND workspace_mounts.project_id = workspace_leases.project_id
       AND workspace_mounts.environment_id = workspace_leases.environment_id
       AND workspace_mounts.workspace_id = workspace_leases.workspace_id
       AND workspace_mounts.id = workspace_leases.workspace_mount_id
       AND workspace_mounts.dirty_generation > 0
),
dirty_lost_workspaces AS (
    UPDATE workspaces
       SET state = 'recovery_required',
           dirty_state = 'dirty_state_lost',
           updated_at = now()
      FROM checkpointing_dirty_loss_scope
     WHERE workspaces.org_id = sqlc.arg(org_id)
       AND workspaces.project_id = checkpointing_dirty_loss_scope.project_id
       AND workspaces.environment_id = checkpointing_dirty_loss_scope.environment_id
       AND workspaces.id = checkpointing_dirty_loss_scope.workspace_id
       AND workspaces.state = 'active'
       AND workspaces.dirty_state <> 'clean'
       AND EXISTS (
           SELECT 1
             FROM run_waits
            WHERE run_waits.org_id = sqlc.arg(org_id)
              AND run_waits.project_id = checkpointing_dirty_loss_scope.project_id
              AND run_waits.environment_id = checkpointing_dirty_loss_scope.environment_id
              AND run_waits.run_id = checkpointing_dirty_loss_scope.run_id
              AND run_waits.id = checkpointing_dirty_loss_scope.run_wait_id
              AND run_waits.state = 'checkpointing'
              AND run_waits.workspace_version_id IS NULL
       )
       AND EXISTS (
           SELECT 1
             FROM workspace_mounts
            WHERE workspace_mounts.org_id = sqlc.arg(org_id)
              AND workspace_mounts.project_id = checkpointing_dirty_loss_scope.project_id
              AND workspace_mounts.environment_id = checkpointing_dirty_loss_scope.environment_id
              AND workspace_mounts.workspace_id = checkpointing_dirty_loss_scope.workspace_id
              AND workspace_mounts.id = checkpointing_dirty_loss_scope.workspace_mount_id
              AND workspace_mounts.dirty_generation > 0
       )
    RETURNING checkpointing_dirty_loss_scope.run_id, workspaces.id
),
retry_plan AS (
    SELECT retry_candidate.*
      FROM retry_candidate
     WHERE NOT EXISTS (
           SELECT 1
             FROM dirty_lost_workspaces
            WHERE dirty_lost_workspaces.run_id = retry_candidate.run_id
     )
),
retry_attempt AS (
    INSERT INTO run_attempts (id, org_id, cell_id, run_id, attempt_number, status, previous_attempt_id)
    SELECT retry_plan.next_attempt_id,
           retry_plan.org_id,
           retry_plan.cell_id,
           retry_plan.run_id,
           retry_plan.next_attempt_number,
           'queued',
           retry_plan.previous_attempt_id
      FROM retry_plan
    RETURNING id, org_id, run_id, attempt_number
),
updated_runs AS (
    UPDATE runs
       SET status = CASE
             WHEN eligible.previous_status = 'cancelled' AND eligible.previous_execution_status = 'pending_cancel' THEN 'cancelled'::run_status
             WHEN retry_plan.run_id IS NOT NULL THEN 'queued'::run_status
             ELSE 'failed'::run_status
           END,
           execution_status = CASE WHEN retry_plan.run_id IS NOT NULL THEN 'queued'::run_execution_status ELSE 'finished'::run_execution_status END,
           terminal_outcome = CASE
             WHEN retry_plan.run_id IS NOT NULL THEN NULL
             WHEN eligible.previous_status = 'cancelled' AND eligible.previous_execution_status = 'pending_cancel' THEN 'cancelled'::run_terminal_outcome
             ELSE 'failed'::run_terminal_outcome
           END,
           current_run_lease_id = NULL,
           current_attempt_id = COALESCE(retry_attempt.id, runs.current_attempt_id),
           current_attempt_number = COALESCE(retry_attempt.attempt_number, runs.current_attempt_number),
           queue_timestamp = COALESCE(retry_plan.retry_after, runs.queue_timestamp),
           error_message = CASE
             WHEN retry_plan.run_id IS NOT NULL THEN NULL
             WHEN eligible.previous_status = 'cancelled' AND eligible.previous_execution_status = 'pending_cancel' THEN COALESCE(runs.error_message, 'run cancelled')
             ELSE 'worker lease expired'
           END,
           state_version = state_version + CASE WHEN retry_plan.run_id IS NOT NULL THEN 2 ELSE 1 END,
           finished_at = CASE WHEN retry_plan.run_id IS NOT NULL THEN NULL ELSE COALESCE(finished_at, now()) END,
           active_elapsed_ms = LEAST(
               runs.active_elapsed_ms
               +
               CASE
                 WHEN runs.active_started_at IS NULL THEN 0
                 ELSE GREATEST((EXTRACT(EPOCH FROM (now() - runs.active_started_at)) * 1000)::bigint, 0)
               END,
               runs.max_active_duration_ms
           ),
           active_started_at = NULL,
           updated_at = now()
      FROM eligible
      LEFT JOIN retry_plan ON retry_plan.run_id = eligible.run_id
      LEFT JOIN retry_attempt ON retry_attempt.org_id = retry_plan.org_id
                             AND retry_attempt.run_id = retry_plan.run_id
     WHERE runs.id = eligible.run_id
       AND (
           runs.status = 'running'
           OR (
               runs.status = 'cancelled'
               AND runs.execution_status = 'pending_cancel'
           )
       )
       AND runs.current_run_lease_id = eligible.run_lease_id
    RETURNING eligible.run_id,
              eligible.run_lease_id,
              eligible.previous_attempt_id,
              eligible.previous_attempt_number,
              eligible.restore_runtime_checkpoint_id,
              runs.cell_id,
              runs.project_id,
              runs.environment_id,
              runs.session_id,
              runs.current_attempt_id,
              runs.current_attempt_number,
              runs.state_version,
              runs.status,
              runs.execution_status,
              runs.error_message,
              runs.active_elapsed_ms,
              runs.locked_retry_policy
),
terminal_session_runs AS (
    UPDATE session_runs
       SET ended_at = now()
      FROM updated_runs
      LEFT JOIN retry_plan ON retry_plan.run_id = updated_runs.run_id
     WHERE retry_plan.run_id IS NULL
       AND session_runs.org_id = sqlc.arg(org_id)
       AND session_runs.project_id = updated_runs.project_id
       AND session_runs.environment_id = updated_runs.environment_id
       AND session_runs.session_id = updated_runs.session_id
       AND session_runs.run_id = updated_runs.run_id
    RETURNING session_runs.id
),
terminal_sessions AS (
    SELECT updated_runs.session_id AS id
      FROM updated_runs
      LEFT JOIN retry_plan ON retry_plan.run_id = updated_runs.run_id
     WHERE retry_plan.run_id IS NULL
),
failed_attempts AS (
    UPDATE run_attempts
       SET status = CASE WHEN updated_runs.status = 'cancelled' THEN 'cancelled'::run_attempt_status ELSE 'failed'::run_attempt_status END,
           error_message = CASE WHEN updated_runs.status = 'cancelled' THEN COALESCE(updated_runs.error_message, 'run cancelled') ELSE 'worker lease expired' END,
           finished_at = COALESCE(run_attempts.finished_at, now()),
           updated_at = now()
      FROM updated_runs
     WHERE run_attempts.org_id = sqlc.arg(org_id)
       AND run_attempts.run_id = updated_runs.run_id
       AND run_attempts.id = updated_runs.previous_attempt_id
    RETURNING run_attempts.id, run_attempts.run_id
),
cancelled_run_waits AS (
    UPDATE run_waits
       SET state = 'cancelled',
           cancelled_at = now(),
           updated_at = now()
      FROM updated_runs
     WHERE run_waits.org_id = sqlc.arg(org_id)
       AND run_waits.run_id = updated_runs.run_id
       AND run_waits.state IN ('live_waiting', 'checkpointing', 'checkpointed_waiting', 'resolved_live')
    RETURNING run_waits.org_id, run_waits.run_id, run_waits.id
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
           error_message = CASE WHEN updated_runs.status = 'cancelled' THEN COALESCE(updated_runs.error_message, 'run cancelled') ELSE 'worker lease expired' END,
           invalidated_at = now()
      FROM updated_runs
     WHERE runtime_checkpoints.run_id = updated_runs.run_id
       AND runtime_checkpoints.state = 'creating'
    RETURNING runtime_checkpoints.run_id, runtime_checkpoints.id
),
failed_runtime_checkpoint_restores AS (
    UPDATE runtime_checkpoint_restores
       SET status = 'failed',
           error_message = CASE WHEN updated_runs.status = 'cancelled' THEN COALESCE(updated_runs.error_message, 'run cancelled') ELSE 'worker lease expired' END,
           finished_at = COALESCE(runtime_checkpoint_restores.finished_at, now()),
           updated_at = now()
      FROM updated_runs
     WHERE runtime_checkpoint_restores.org_id = sqlc.arg(org_id)
       AND runtime_checkpoint_restores.run_id = updated_runs.run_id
       AND runtime_checkpoint_restores.run_lease_id = updated_runs.run_lease_id
       AND runtime_checkpoint_restores.runtime_checkpoint_id = updated_runs.restore_runtime_checkpoint_id
       AND runtime_checkpoint_restores.status = 'restoring'
    RETURNING runtime_checkpoint_restores.id
),
completed_queue_entries AS (
    UPDATE run_queue_items
       SET status = CASE WHEN retry_plan.run_id IS NOT NULL THEN 'queued'::run_queue_status ELSE 'completed'::run_queue_status END,
           queue_timestamp = COALESCE(retry_plan.retry_after, run_queue_items.queue_timestamp),
           dispatch_message_id = CASE WHEN retry_plan.run_id IS NOT NULL THEN NULL ELSE run_queue_items.dispatch_message_id END,
           reserved_by_worker_instance_id = CASE WHEN retry_plan.run_id IS NOT NULL THEN NULL ELSE run_queue_items.reserved_by_worker_instance_id END,
           reservation_expires_at = CASE WHEN retry_plan.run_id IS NOT NULL THEN NULL ELSE run_queue_items.reservation_expires_at END,
           dispatch_generation = dispatch_generation + 1,
           last_error = CASE WHEN retry_plan.run_id IS NOT NULL THEN '' ELSE run_queue_items.last_error END,
           enqueued_at = CASE WHEN retry_plan.run_id IS NOT NULL THEN now() ELSE run_queue_items.enqueued_at END,
           updated_at = now(),
           finished_at = CASE WHEN retry_plan.run_id IS NOT NULL THEN NULL ELSE now() END
      FROM updated_runs
      JOIN run_leases ON run_leases.org_id = sqlc.arg(org_id)
                         AND run_leases.run_id = updated_runs.run_id
                         AND run_leases.id = updated_runs.run_lease_id
      LEFT JOIN retry_plan ON retry_plan.run_id = updated_runs.run_id
     WHERE run_queue_items.org_id = sqlc.arg(org_id)
       AND run_queue_items.run_id = updated_runs.run_id
       AND run_queue_items.reserved_by_worker_instance_id = run_leases.worker_instance_id
       AND run_queue_items.dispatch_message_id = run_leases.dispatch_message_id
       AND run_queue_items.status = 'reserved'
    RETURNING run_queue_items.run_id
),
released_concurrency_slots AS (
    UPDATE run_queue_concurrency_leases
       SET released_at = now()
      FROM updated_runs
     WHERE run_queue_concurrency_leases.org_id = sqlc.arg(org_id)
       AND run_queue_concurrency_leases.run_id = updated_runs.run_id
       AND run_queue_concurrency_leases.run_lease_id = updated_runs.run_lease_id
       AND run_queue_concurrency_leases.released_at IS NULL
    RETURNING run_queue_concurrency_leases.id
),
released_workspace_leases AS (
    UPDATE workspace_leases
       SET state = 'released',
           released_at = now(),
           renewed_at = now(),
           updated_at = now()
      FROM updated_runs
     WHERE workspace_leases.org_id = sqlc.arg(org_id)
       AND workspace_leases.owner_run_id = updated_runs.run_id
       AND workspace_leases.lease_kind = 'write'
       AND workspace_leases.released_at IS NULL
    RETURNING workspace_leases.id
),
failed_snapshots AS (
    INSERT INTO run_snapshots (org_id, cell_id, run_id, version, status, execution_status, terminal_outcome, attempt_id, run_lease_id, transition, reason)
    SELECT sqlc.arg(org_id),
           updated_runs.cell_id,
           updated_runs.run_id,
           CASE WHEN retry_plan.run_id IS NOT NULL THEN updated_runs.state_version - 1 ELSE updated_runs.state_version END,
           CASE WHEN retry_plan.run_id IS NOT NULL THEN 'failed'::run_status ELSE updated_runs.status END,
           'finished',
           CASE WHEN retry_plan.run_id IS NOT NULL THEN 'failed'::run_terminal_outcome ELSE updated_runs.status::text::run_terminal_outcome END,
           updated_runs.previous_attempt_id,
           updated_runs.run_lease_id,
           CASE WHEN updated_runs.status = 'cancelled' THEN 'run_lease.lost_cancelled' ELSE 'run_lease.lost_failed' END,
           jsonb_build_object(
               'reason', CASE WHEN updated_runs.status = 'cancelled' THEN COALESCE(updated_runs.error_message, 'run cancelled') ELSE 'worker lease expired' END,
               'origin', 'lease_sweeper'
           )
      FROM updated_runs
      JOIN failed_attempts ON failed_attempts.run_id = updated_runs.run_id
      LEFT JOIN retry_plan ON retry_plan.run_id = updated_runs.run_id
    RETURNING run_snapshots.run_id
),
retry_decision AS (
    INSERT INTO run_retry_decisions (org_id, cell_id, project_id, environment_id, run_id, attempt_id, run_lease_id, snapshot_version, decision, reason, error_class, retry_after, next_attempt_number, policy_snapshot, error)
    SELECT sqlc.arg(org_id),
           updated_runs.cell_id,
           updated_runs.project_id,
           updated_runs.environment_id,
           updated_runs.run_id,
           updated_runs.previous_attempt_id,
           updated_runs.run_lease_id,
           updated_runs.state_version - 1,
           'retry',
           retry_plan.reason,
           retry_plan.reason,
           retry_plan.retry_after,
           retry_plan.next_attempt_number,
           updated_runs.locked_retry_policy,
           jsonb_build_object('failure_kind', 'infra_lost', 'detail', jsonb_build_object('message', 'worker lease expired'))
      FROM updated_runs
      JOIN retry_plan ON retry_plan.run_id = updated_runs.run_id
    ON CONFLICT DO NOTHING
    RETURNING id
),
retry_snapshot AS (
    INSERT INTO run_snapshots (org_id, cell_id, run_id, version, status, execution_status, attempt_id, previous_version, transition, reason)
    SELECT sqlc.arg(org_id),
           updated_runs.cell_id,
           updated_runs.run_id,
           updated_runs.state_version,
           updated_runs.status,
           updated_runs.execution_status,
           updated_runs.current_attempt_id,
           updated_runs.state_version - 1,
           'run.retry_scheduled',
           jsonb_build_object(
               'reason', retry_plan.reason,
               'previous_attempt_id', retry_plan.previous_attempt_id,
               'previous_attempt_number', retry_plan.previous_attempt_number,
               'next_attempt_id', retry_plan.next_attempt_id,
               'next_attempt_number', retry_plan.next_attempt_number,
               'retry_after', retry_plan.retry_after,
               'delay_ms', retry_plan.delay_ms
           )
      FROM updated_runs
      JOIN retry_plan ON retry_plan.run_id = updated_runs.run_id
      JOIN completed_queue_entries ON completed_queue_entries.run_id = updated_runs.run_id
    RETURNING run_snapshots.run_id
),
event_inputs AS (
    SELECT 1 AS event_ordinal,
           sqlc.arg(org_id) AS org_id,
           updated_runs.cell_id,
           updated_runs.project_id,
           updated_runs.environment_id,
           updated_runs.run_id,
           updated_runs.previous_attempt_id AS attempt_id,
           updated_runs.run_lease_id,
           updated_runs.previous_attempt_number AS attempt_number,
           run_leases.trace_id,
           run_leases.span_id,
           run_leases.parent_span_id,
           run_leases.traceparent,
           'lifecycle' AS category,
           CASE WHEN updated_runs.status = 'cancelled' THEN 'warn' ELSE 'error' END AS severity,
           'lease_sweeper' AS source,
           CASE WHEN updated_runs.status = 'cancelled' THEN 'run.cancelled' ELSE 'run.failed' END AS kind,
           CASE WHEN updated_runs.status = 'cancelled' THEN 'run.cancelled' ELSE 'run.failed' END AS message,
           CASE
             WHEN updated_runs.status = 'cancelled'
             THEN jsonb_build_object('reason', COALESCE(updated_runs.error_message, 'run cancelled'), 'origin', 'lease_sweeper')
             ELSE jsonb_build_object(
                 'failure_kind', 'worker_lease_expired',
                 'detail', jsonb_build_object('message', 'worker lease expired')
             )
           END AS payload,
           'internal' AS redaction_class,
           CASE WHEN retry_plan.run_id IS NOT NULL THEN updated_runs.state_version - 1 ELSE updated_runs.state_version END AS snapshot_version
      FROM updated_runs
      JOIN run_leases ON run_leases.org_id = sqlc.arg(org_id)
                                  AND run_leases.run_id = updated_runs.run_id
                                  AND run_leases.id = updated_runs.run_lease_id
      LEFT JOIN retry_plan ON retry_plan.run_id = updated_runs.run_id
    UNION ALL
    SELECT 2 AS event_ordinal,
           sqlc.arg(org_id) AS org_id,
           updated_runs.cell_id,
           updated_runs.project_id,
           updated_runs.environment_id,
           updated_runs.run_id,
           updated_runs.previous_attempt_id,
           updated_runs.run_lease_id,
           updated_runs.previous_attempt_number,
           run_leases.trace_id,
           run_leases.span_id,
           run_leases.parent_span_id,
           run_leases.traceparent,
           'worker',
           'warn',
           'lease_sweeper',
           'run.execution_lost',
           'run.execution_lost',
           jsonb_build_object(
               'reason', 'worker lease expired',
               'origin', 'lease_sweeper'
           ),
           'internal',
           CASE WHEN retry_plan.run_id IS NOT NULL THEN updated_runs.state_version - 1 ELSE updated_runs.state_version END
      FROM updated_runs
      JOIN run_leases ON run_leases.org_id = sqlc.arg(org_id)
                                  AND run_leases.run_id = updated_runs.run_id
                                  AND run_leases.id = updated_runs.run_lease_id
      LEFT JOIN retry_plan ON retry_plan.run_id = updated_runs.run_id
    UNION ALL
    SELECT 3 AS event_ordinal,
           sqlc.arg(org_id) AS org_id,
           updated_runs.cell_id,
           updated_runs.project_id,
           updated_runs.environment_id,
           updated_runs.run_id,
           updated_runs.current_attempt_id,
           NULL::uuid,
           updated_runs.current_attempt_number,
           runs.trace_id,
           runs.root_span_id,
           NULL::text,
           '00-' || runs.trace_id || '-' || runs.root_span_id || '-01',
           'lifecycle',
           'warn',
           'control',
           'run.retry_scheduled',
           'run.retry_scheduled',
           jsonb_build_object(
               'reason', retry_plan.reason,
               'previous_attempt_id', retry_plan.previous_attempt_id,
               'previous_attempt_number', retry_plan.previous_attempt_number,
               'next_attempt_id', retry_plan.next_attempt_id,
               'next_attempt_number', retry_plan.next_attempt_number,
               'retry_after', retry_plan.retry_after,
               'delay_ms', retry_plan.delay_ms
           ),
           'internal',
           updated_runs.state_version
      FROM updated_runs
      JOIN retry_plan ON retry_plan.run_id = updated_runs.run_id
      JOIN runs ON runs.org_id = sqlc.arg(org_id)
               AND runs.id = updated_runs.run_id
      JOIN retry_snapshot ON true
),
event_subject_counts AS (
    SELECT org_id, cell_id, run_id, count(*)::bigint AS event_count
      FROM event_inputs
     GROUP BY org_id, cell_id, run_id
),
event_seq AS (
    INSERT INTO event_cursors (org_id, cell_id, subject_kind, subject_id, seq)
    SELECT org_id, cell_id, 'run', run_id, event_count
      FROM event_subject_counts
    ON CONFLICT (org_id, cell_id, subject_kind, subject_id)
    DO UPDATE SET seq = event_cursors.seq + EXCLUDED.seq,
                  observed_at = now()
    RETURNING org_id, subject_kind, subject_id, seq
),
events AS (
    INSERT INTO event_hot_payloads (org_id, cell_id, project_id, environment_id, run_id, seq, attempt_id, run_lease_id, attempt_number, trace_id, span_id, parent_span_id, traceparent, category, severity, source, kind, message, payload, redaction_class, snapshot_version)
    SELECT event_inputs.org_id,
           event_inputs.cell_id,
           event_inputs.project_id,
           event_inputs.environment_id,
           event_inputs.run_id,
           event_seq.seq - event_subject_counts.event_count + row_number() OVER (PARTITION BY event_inputs.org_id, event_inputs.run_id ORDER BY event_inputs.event_ordinal),
           event_inputs.attempt_id,
           event_inputs.run_lease_id,
           event_inputs.attempt_number,
           event_inputs.trace_id,
           event_inputs.span_id,
           event_inputs.parent_span_id,
           event_inputs.traceparent,
           event_inputs.category,
           event_inputs.severity,
           event_inputs.source,
           event_inputs.kind,
           event_inputs.message,
           event_inputs.payload,
           event_inputs.redaction_class,
           event_inputs.snapshot_version
      FROM event_inputs
      JOIN event_subject_counts ON event_subject_counts.org_id = event_inputs.org_id
                               AND event_subject_counts.run_id = event_inputs.run_id
      JOIN event_seq ON event_seq.org_id = event_inputs.org_id
                    AND event_seq.subject_kind = 'run'
                    AND event_seq.subject_id = event_inputs.run_id
    RETURNING *
),
telemetry_outbox AS (
    INSERT INTO telemetry_outbox (org_id, cell_id, stream_kind, source_kind, source_id, seq, idempotency_key)
    SELECT events.org_id,
                  events.cell_id,
                  'event',
                  events.subject_type,
                  events.subject_id,
                  events.seq,
                  'event:' || events.subject_type::text || ':' || events.subject_id::text || ':' || events.seq::text
      FROM events
    RETURNING id
),
active_time_delta AS (
    SELECT updated_runs.run_id,
           GREATEST(
               updated_runs.active_elapsed_ms
               - COALESCE((
                   SELECT SUM(usage_facts.quantity)::bigint
                     FROM usage_facts
                    WHERE usage_facts.org_id = sqlc.arg(org_id)
                      AND usage_facts.run_id = updated_runs.run_id
                      AND usage_facts.meter = 'active_time'
               ), 0),
               0
           )::bigint AS quantity
      FROM updated_runs
),
active_time_usage_event AS (
    INSERT INTO usage_facts (org_id, cell_id, project_id, environment_id, source_kind, source_id, run_id, attempt_id, run_lease_id, trace_id, span_id, snapshot_version, meter, quantity, unit, measured_to, details, idempotency_key)
    SELECT sqlc.arg(org_id),
           updated_runs.cell_id,
           updated_runs.project_id,
           updated_runs.environment_id,
           'run_lease',
           updated_runs.run_lease_id,
           updated_runs.run_id,
           updated_runs.previous_attempt_id,
           updated_runs.run_lease_id,
           run_leases.trace_id,
           run_leases.span_id,
           CASE WHEN retry_plan.run_id IS NOT NULL THEN updated_runs.state_version - 1 ELSE updated_runs.state_version END,
           'active_time',
           active_time_delta.quantity,
           'ms',
           now(),
           jsonb_build_object('phase', 'lost'),
           'active_time:' || updated_runs.run_lease_id::text || ':lost'
      FROM updated_runs
      JOIN active_time_delta ON active_time_delta.run_id = updated_runs.run_id
      JOIN run_leases ON run_leases.org_id = sqlc.arg(org_id)
                     AND run_leases.run_id = updated_runs.run_id
                     AND run_leases.id = updated_runs.run_lease_id
      LEFT JOIN retry_plan ON retry_plan.run_id = updated_runs.run_id
     WHERE active_time_delta.quantity > 0
    ON CONFLICT DO NOTHING
    RETURNING id
),
cleanup AS (
    SELECT
        (SELECT count(*) FROM dirty_lost_workspaces) AS dirty_lost_workspaces,
        (SELECT count(*) FROM cancelled_run_waits) AS cancelled_run_waits,
        (SELECT count(*) FROM invalidated_runtime_checkpoints) AS invalidated_runtime_checkpoints,
        (SELECT count(*) FROM failed_runtime_checkpoint_restores) AS failed_runtime_checkpoint_restores,
        (SELECT count(*) FROM completed_queue_entries) AS completed_queue_entries,
        (SELECT count(*) FROM released_concurrency_slots) AS released_concurrency_slots,
        (SELECT count(*) FROM released_workspace_leases) AS released_workspace_leases,
        (SELECT count(*) FROM events WHERE kind IN ('run.cancelled', 'run.failed')) AS terminal_events,
        (SELECT count(*) FROM events WHERE kind = 'run.execution_lost') AS lost_events,
        (SELECT count(*) FROM failed_snapshots) AS failed_snapshots,
        (SELECT count(*) FROM retry_decision) AS retry_decisions,
        (SELECT count(*) FROM events WHERE kind = 'run.retry_scheduled') AS retry_events,
        (SELECT count(*) FROM telemetry_outbox) AS telemetry_outboxes,
        (SELECT count(*) FROM active_time_usage_event) AS active_time_usage_events
)
UPDATE run_leases
   SET lost_at = COALESCE(lost_at, now()),
       renewed_at = now(),
       active_duration_ms = updated_runs.active_elapsed_ms,
       status = 'lost'
  FROM updated_runs
 WHERE run_leases.id = updated_runs.run_lease_id
   AND run_leases.run_id = updated_runs.run_id
   AND (SELECT dirty_lost_workspaces + cancelled_run_waits + invalidated_runtime_checkpoints + failed_runtime_checkpoint_restores + completed_queue_entries + released_concurrency_slots + released_workspace_leases + terminal_events + lost_events + failed_snapshots + retry_decisions + retry_events + telemetry_outboxes + active_time_usage_events FROM cleanup) >= 0;

-- name: LeaseRunLease :one
WITH
locked_dispatch AS MATERIALIZED (
    SELECT run_queue_items.run_id,
           run_queue_items.org_id,
           run_queue_items.cell_id,
           run_queue_items.route_generation,
           run_queue_items.reserved_by_worker_instance_id,
           run_queue_items.dispatch_message_id
      FROM run_queue_items
     WHERE run_queue_items.org_id = sqlc.arg(org_id)
       AND run_queue_items.run_id = sqlc.arg(run_id)
       AND run_queue_items.reserved_by_worker_instance_id = sqlc.arg(worker_instance_id)
       AND run_queue_items.dispatch_message_id = sqlc.narg(dispatch_message_id)::text
       AND run_queue_items.status = 'reserved'
       AND run_queue_items.reservation_expires_at > now()
     FOR UPDATE OF run_queue_items
),
locked_worker_instance AS MATERIALIZED (
    SELECT worker_instances.*
      FROM worker_instances
      JOIN locked_dispatch ON locked_dispatch.reserved_by_worker_instance_id = worker_instances.id
                          AND locked_dispatch.cell_id = worker_instances.cell_id
     WHERE worker_instances.status = 'active'
     FOR UPDATE OF worker_instances
),
dispatch AS (
    SELECT locked_dispatch.run_id,
           locked_dispatch.cell_id,
           locked_dispatch.route_generation,
           locked_dispatch.reserved_by_worker_instance_id AS worker_instance_id,
           locked_dispatch.dispatch_message_id,
           worker_instances.available_milli_cpu,
           worker_instances.available_memory_mib,
           worker_instances.available_disk_mib,
           worker_instances.available_execution_slots,
           worker_instances.total_milli_cpu,
           worker_instances.total_memory_mib,
           worker_instances.total_disk_mib,
           worker_instances.region,
           worker_instances.labels,
           worker_instances.runtime_id,
           worker_instances.runtime_arch,
           worker_instances.runtime_abi,
           worker_instances.kernel_digest,
           worker_instances.initramfs_digest,
           worker_instances.rootfs_digest,
           worker_instances.cni_profile,
           worker_instances.worker_group_id,
           worker_instances.protocol_version,
           active.used_milli_cpu,
           active.used_memory_mib,
           active.used_disk_mib,
           active.used_slots,
           active_runtime_instances.used_milli_cpu AS used_runtime_instance_milli_cpu,
           active_runtime_instances.used_memory_mib AS used_runtime_instance_memory_mib,
           active_runtime_instances.used_disk_mib AS used_runtime_instance_disk_mib,
           active_runtime_instances.used_slots AS used_runtime_instance_slots
      FROM locked_dispatch
      JOIN locked_worker_instance AS worker_instances ON worker_instances.id = locked_dispatch.reserved_by_worker_instance_id
      LEFT JOIN LATERAL (
          SELECT COALESCE(sum(run_runtime_requirements.requested_milli_cpu), 0)::bigint AS used_milli_cpu,
                 COALESCE(sum(run_runtime_requirements.requested_memory_mib), 0)::bigint AS used_memory_mib,
                 COALESCE(sum(run_runtime_requirements.requested_disk_mib), 0)::bigint AS used_disk_mib,
                 COALESCE(sum(run_runtime_requirements.requested_execution_slots), 0)::int AS used_slots
            FROM run_leases
            JOIN runs ON runs.org_id = run_leases.org_id
                     AND runs.id = run_leases.run_id
                     AND runs.workspace_mount_id IS NULL
            JOIN run_runtime_requirements ON run_runtime_requirements.org_id = run_leases.org_id
                                 AND run_runtime_requirements.run_id = run_leases.run_id
           WHERE run_leases.worker_instance_id = worker_instances.id
             AND run_leases.status IN ('leased', 'running')
      ) active ON true
      LEFT JOIN LATERAL (
          SELECT COALESCE(sum(runtime_instances.reserved_cpu_millis), 0)::bigint AS used_milli_cpu,
                 COALESCE(sum(runtime_instances.reserved_memory_mib), 0)::bigint AS used_memory_mib,
                 COALESCE(sum(runtime_instances.reserved_disk_mib), 0)::bigint AS used_disk_mib,
                 COALESCE(sum(runtime_instances.reserved_execution_slots), 0)::int AS used_slots
            FROM runtime_instances
           WHERE runtime_instances.worker_instance_id = worker_instances.id
             AND runtime_instances.state IN ('preparing', 'ready', 'binding', 'running', 'waiting_hot', 'checkpointing', 'stopping')
             AND (
                 runtime_instances.expires_at IS NULL
                 OR runtime_instances.expires_at > now()
             )
      ) active_runtime_instances ON true
),
candidate AS (
    SELECT runs.id,
           runs.org_id,
           runs.cell_id,
           runs.project_id,
           runs.environment_id,
           runs.workspace_id,
           runs.trace_id,
           runs.root_span_id,
           runs.latest_runtime_checkpoint_id,
           runs.session_id,
           runs.queue_name,
           runs.queue_concurrency_limit,
           runs.concurrency_key,
           runs.active_elapsed_ms,
           runs.current_attempt_id,
           runs.current_attempt_number,
           run_runtime_requirements.runtime_id
      FROM runs
      JOIN dispatch ON dispatch.run_id = runs.id
                   AND dispatch.cell_id = runs.cell_id
      JOIN deployments ON deployments.org_id = runs.org_id
                      AND deployments.id = runs.deployment_id
      JOIN environment_cells
        ON environment_cells.org_id = runs.org_id
       AND environment_cells.project_id = runs.project_id
       AND environment_cells.environment_id = runs.environment_id
       AND environment_cells.cell_id = runs.cell_id
       AND environment_cells.cell_id = dispatch.cell_id
       AND environment_cells.route_generation = dispatch.route_generation
       AND environment_cells.route_state IN ('active', 'draining')
      JOIN org_cells ON org_cells.org_id = environment_cells.org_id
                    AND org_cells.cell_id = environment_cells.cell_id
                    AND org_cells.state = 'active'
      JOIN cells ON cells.id = environment_cells.cell_id
                AND cells.state IN ('active', 'draining')
      JOIN cell_health ON cell_health.cell_id = environment_cells.cell_id
                      AND cell_health.state IN ('healthy', 'degraded')
                      AND cell_health.routing_fresh_until > now()
      JOIN run_runtime_requirements ON run_runtime_requirements.org_id = runs.org_id
                                    AND run_runtime_requirements.cell_id = runs.cell_id
                                    AND run_runtime_requirements.run_id = runs.id
      JOIN LATERAL (
          SELECT COALESCE(run_runtime_requirements.placement->'tags', run_runtime_requirements.placement->'Tags') AS placement_tags,
                 COALESCE(NULLIF(run_runtime_requirements.placement->>'region', ''), NULLIF(run_runtime_requirements.placement->>'Region', ''), '') AS placement_region,
                 COALESCE(NULLIF(run_runtime_requirements.placement->>'dedicated_key', ''), NULLIF(run_runtime_requirements.placement->>'DedicatedKey', ''), '') AS dedicated_key,
                 COALESCE(NULLIF(run_runtime_requirements.placement->>'snapshot_key', ''), NULLIF(run_runtime_requirements.placement->>'SnapshotKey', ''), '') AS snapshot_key
      ) placement ON true
     WHERE runs.org_id = sqlc.arg(org_id)
       AND runs.id = sqlc.arg(run_id)
       AND runs.status = 'queued'
       AND runs.current_run_lease_id IS NULL
       AND EXISTS (
           SELECT 1
             FROM sessions
            WHERE sessions.org_id = runs.org_id
              AND sessions.project_id = runs.project_id
              AND sessions.environment_id = runs.environment_id
              AND sessions.id = runs.session_id
              AND sessions.current_run_id = runs.id
              AND sessions.status = 'open'
       )
       AND deployments.worker_protocol_version = dispatch.protocol_version
       AND (runs.queued_expires_at IS NULL OR runs.queued_expires_at > now())
       AND run_runtime_requirements.worker_group_id = dispatch.worker_group_id
       AND run_runtime_requirements.runtime_id = dispatch.runtime_id
       AND run_runtime_requirements.runtime_arch = dispatch.runtime_arch
       AND run_runtime_requirements.runtime_abi = dispatch.runtime_abi
       AND run_runtime_requirements.kernel_digest = dispatch.kernel_digest
       AND run_runtime_requirements.initramfs_digest = dispatch.initramfs_digest
       AND run_runtime_requirements.rootfs_digest = dispatch.rootfs_digest
       AND run_runtime_requirements.cni_profile = dispatch.cni_profile
       AND (
           placement.placement_tags IS NULL
           OR placement.placement_tags = 'null'::jsonb
           OR (
               jsonb_typeof(placement.placement_tags) = 'object'
               AND dispatch.labels @> placement.placement_tags
           )
       )
       AND (placement.dedicated_key = '' OR dispatch.labels->>'dedicated_key' = placement.dedicated_key)
       AND (placement.snapshot_key = '' OR dispatch.labels->>'snapshot_key' = placement.snapshot_key)
       AND (
           runs.latest_runtime_checkpoint_id IS NULL
           OR EXISTS (
               SELECT 1
                 FROM runtime_checkpoints
                WHERE runtime_checkpoints.org_id = sqlc.arg(org_id)
                  AND runtime_checkpoints.run_id = runs.id
                  AND runtime_checkpoints.id = runs.latest_runtime_checkpoint_id
                  AND runtime_checkpoints.state = 'ready'
                  AND (runtime_checkpoints.expires_at IS NULL OR runtime_checkpoints.expires_at > now())
                  AND runtime_checkpoints.runtime_id = dispatch.runtime_id
                  AND runtime_checkpoints.runtime_arch = dispatch.runtime_arch
                  AND runtime_checkpoints.runtime_abi = dispatch.runtime_abi
                  AND runtime_checkpoints.kernel_digest = dispatch.kernel_digest
                  AND runtime_checkpoints.initramfs_digest = dispatch.initramfs_digest
                  AND runtime_checkpoints.rootfs_digest = dispatch.rootfs_digest
                  AND (runtime_checkpoints.runtime_vcpus IS NULL OR runtime_checkpoints.runtime_vcpus = ((run_runtime_requirements.requested_milli_cpu + 999) / 1000))
                  AND (runtime_checkpoints.runtime_memory_mib IS NULL OR runtime_checkpoints.runtime_memory_mib = run_runtime_requirements.requested_memory_mib)
                  AND (
                      runtime_checkpoints.runtime_scratch_disk_mib IS NULL
                      OR runtime_checkpoints.runtime_scratch_disk_mib = CASE
                          WHEN run_runtime_requirements.requested_disk_mib > 0 THEN run_runtime_requirements.requested_disk_mib
                          ELSE dispatch.total_disk_mib
                      END
                  )
                  AND runtime_checkpoints.cni_profile = dispatch.cni_profile
           )
       )
     FOR UPDATE OF runs
),
workspace_candidate AS (
    SELECT candidate.id AS run_id,
           candidate.org_id,
           candidate.cell_id,
           candidate.project_id,
           candidate.environment_id,
           workspaces.id AS workspace_id,
           workspaces.deployment_sandbox_id,
           workspaces.sandbox_fingerprint,
           workspaces.current_version_id AS workspace_base_version_id,
           deployment_sandboxes.workspace_mount_path AS workspace_mount_path,
           current_workspace_version.artifact_encoding AS workspace_artifact_encoding,
           current_workspace_version.artifact_entry_count AS workspace_artifact_entry_count,
           current_workspace_version.artifact_id AS workspace_artifact_id,
           deployment_sandboxes.image_artifact_id,
           deployment_sandboxes.image_artifact_format,
           deployment_sandboxes.image_digest,
           deployment_sandboxes.image_format,
           image_artifact.digest AS sandbox_image_artifact_digest,
           image_artifact.size_bytes AS sandbox_image_artifact_size_bytes,
           image_artifact.media_type AS sandbox_image_artifact_media_type,
           workspace_artifact.digest AS workspace_artifact_digest,
           workspace_artifact.size_bytes AS workspace_artifact_size_bytes,
           workspace_artifact.media_type AS workspace_artifact_media_type,
           deployment_sandboxes.rootfs_digest,
           deployment_sandboxes.runtime_abi AS deployment_sandbox_runtime_abi,
           deployment_sandboxes.guestd_abi,
           deployment_sandboxes.adapter_abi,
           run_runtime_requirements.requested_milli_cpu,
           run_runtime_requirements.requested_memory_mib,
           run_runtime_requirements.requested_disk_mib,
           run_runtime_requirements.requested_execution_slots,
           run_runtime_requirements.runtime_id
      FROM candidate
      JOIN workspaces ON workspaces.org_id = sqlc.arg(org_id)
                     AND workspaces.project_id = candidate.project_id
                     AND workspaces.environment_id = candidate.environment_id
                     AND workspaces.id = candidate.workspace_id
                     AND workspaces.state = 'active'
      JOIN deployment_sandboxes
        ON deployment_sandboxes.org_id = workspaces.org_id
       AND deployment_sandboxes.project_id = workspaces.project_id
       AND deployment_sandboxes.environment_id = workspaces.environment_id
       AND deployment_sandboxes.id = workspaces.deployment_sandbox_id
      JOIN run_runtime_requirements ON run_runtime_requirements.org_id = sqlc.arg(org_id)
                                    AND run_runtime_requirements.run_id = candidate.id
      JOIN workspace_versions AS current_workspace_version
        ON current_workspace_version.org_id = workspaces.org_id
       AND current_workspace_version.workspace_id = workspaces.id
       AND current_workspace_version.id = workspaces.current_version_id
       AND current_workspace_version.state = 'ready'
      JOIN artifacts AS workspace_artifact
        ON workspace_artifact.org_id = workspaces.org_id
       AND workspace_artifact.project_id = workspaces.project_id
       AND workspace_artifact.environment_id = workspaces.environment_id
       AND workspace_artifact.id = current_workspace_version.artifact_id
       AND workspace_artifact.kind = 'workspace_version'
       AND workspace_artifact.media_type = 'application/vnd.helmr.workspace.v0.tar'
      JOIN artifacts AS image_artifact
        ON image_artifact.org_id = deployment_sandboxes.org_id
       AND image_artifact.project_id = deployment_sandboxes.project_id
       AND image_artifact.environment_id = deployment_sandboxes.environment_id
       AND image_artifact.id = deployment_sandboxes.image_artifact_id
       AND image_artifact.kind = 'sandbox_image'
       AND image_artifact.media_type = 'application/vnd.helmr.sandbox-image.v0.oci-tar'
),
existing_workspace_mount AS (
    SELECT workspace_mounts.id,
           workspace_mounts.org_id,
           workspace_mounts.project_id,
           workspace_mounts.environment_id,
           workspace_mounts.workspace_id,
           workspace_mounts.fencing_generation,
           runtime_instances.id AS runtime_instance_id,
           runtime_instances.reserved_cpu_millis,
           runtime_instances.reserved_memory_mib,
           runtime_instances.reserved_disk_mib,
           runtime_instances.reserved_execution_slots
      FROM workspace_candidate
      JOIN workspace_mounts
        ON workspace_mounts.org_id = sqlc.arg(org_id)
       AND workspace_mounts.project_id = workspace_candidate.project_id
       AND workspace_mounts.environment_id = workspace_candidate.environment_id
       AND workspace_mounts.workspace_id = workspace_candidate.workspace_id
       AND workspace_mounts.state = 'mounted'
      JOIN runtime_instances
        ON runtime_instances.org_id = workspace_mounts.org_id
       AND runtime_instances.id = workspace_mounts.runtime_instance_id
       AND runtime_instances.workspace_mount_id = workspace_mounts.id
       AND runtime_instances.worker_instance_id = sqlc.arg(worker_instance_id)
       AND runtime_instances.state IN ('binding', 'running', 'waiting_hot')
       AND (
           runtime_instances.expires_at IS NULL
           OR runtime_instances.expires_at > now()
       )
),
workspace_mount AS (
    SELECT * FROM existing_workspace_mount
),
workspace_ready_candidate AS (
    SELECT candidate.*
      FROM candidate
      JOIN dispatch ON dispatch.run_id = candidate.id
      JOIN workspace_candidate ON workspace_candidate.run_id = candidate.id
      JOIN workspace_mount
        ON workspace_mount.org_id = sqlc.arg(org_id)
       AND workspace_mount.project_id = workspace_candidate.project_id
       AND workspace_mount.environment_id = workspace_candidate.environment_id
       AND workspace_mount.workspace_id = workspace_candidate.workspace_id
     WHERE workspace_candidate.requested_milli_cpu <= GREATEST(dispatch.available_milli_cpu - dispatch.used_milli_cpu - dispatch.used_runtime_instance_milli_cpu + workspace_mount.reserved_cpu_millis, 0)
       AND workspace_candidate.requested_memory_mib <= GREATEST(dispatch.available_memory_mib - dispatch.used_memory_mib - dispatch.used_runtime_instance_memory_mib + workspace_mount.reserved_memory_mib, 0)
       AND workspace_candidate.requested_disk_mib <= GREATEST(dispatch.available_disk_mib - dispatch.used_disk_mib - dispatch.used_runtime_instance_disk_mib + workspace_mount.reserved_disk_mib, 0)
       AND workspace_candidate.requested_execution_slots <= GREATEST(dispatch.available_execution_slots - dispatch.used_slots - dispatch.used_runtime_instance_slots + workspace_mount.reserved_execution_slots, 0)
       AND NOT EXISTS (
               SELECT 1
                 FROM workspace_leases
            WHERE workspace_leases.org_id = workspace_mount.org_id
              AND workspace_leases.workspace_id = workspace_mount.workspace_id
              AND workspace_leases.lease_kind = 'write'
              AND workspace_leases.state IN ('active', 'releasing')
       )
),
runtime_resume_capacity AS (
    SELECT workspace_ready_candidate.*
      FROM workspace_ready_candidate
      JOIN workspace_candidate ON workspace_candidate.run_id = workspace_ready_candidate.id
     WHERE workspace_ready_candidate.latest_runtime_checkpoint_id IS NULL
        OR EXISTS (
            SELECT 1
              FROM runtime_checkpoints
             WHERE runtime_checkpoints.org_id = sqlc.arg(org_id)
               AND runtime_checkpoints.run_id = workspace_ready_candidate.id
               AND runtime_checkpoints.id = workspace_ready_candidate.latest_runtime_checkpoint_id
               AND runtime_checkpoints.base_workspace_version_id = workspace_candidate.workspace_base_version_id
               AND runtime_checkpoints.state = 'ready'
               AND (runtime_checkpoints.expires_at IS NULL OR runtime_checkpoints.expires_at > now())
        )
),
concurrency_scope_lock AS MATERIALIZED (
    SELECT runtime_resume_capacity.id AS run_id,
           true AS locked
      FROM runtime_resume_capacity
      CROSS JOIN LATERAL (
          SELECT pg_advisory_xact_lock(
                     hashtext(sqlc.arg(org_id)::text || ':' || runtime_resume_capacity.cell_id || ':' || runtime_resume_capacity.environment_id::text),
                     hashtext(runtime_resume_capacity.queue_name || ':' || COALESCE(runtime_resume_capacity.concurrency_key, ''))
                 )
      ) lock
     WHERE runtime_resume_capacity.queue_concurrency_limit IS NOT NULL
),
concurrency_capacity AS (
    SELECT runtime_resume_capacity.*
      FROM runtime_resume_capacity
      LEFT JOIN concurrency_scope_lock ON concurrency_scope_lock.run_id = runtime_resume_capacity.id
     WHERE runtime_resume_capacity.queue_concurrency_limit IS NULL
        OR (
            concurrency_scope_lock.locked
            AND (
                SELECT count(*)::int
                 FROM run_queue_concurrency_leases
                WHERE run_queue_concurrency_leases.org_id = sqlc.arg(org_id)
                  AND run_queue_concurrency_leases.cell_id = runtime_resume_capacity.cell_id
                  AND run_queue_concurrency_leases.environment_id = runtime_resume_capacity.environment_id
                  AND run_queue_concurrency_leases.queue_name = runtime_resume_capacity.queue_name
                  AND COALESCE(run_queue_concurrency_leases.concurrency_key, '') = COALESCE(runtime_resume_capacity.concurrency_key, '')
                  AND run_queue_concurrency_leases.released_at IS NULL
            ) < runtime_resume_capacity.queue_concurrency_limit
        )
),
concurrency_slot_candidate AS (
    SELECT concurrency_capacity.*,
           slots.slot_ordinal
      FROM concurrency_capacity
      CROSS JOIN LATERAL generate_series(1, concurrency_capacity.queue_concurrency_limit) AS slots(slot_ordinal)
     WHERE concurrency_capacity.queue_concurrency_limit IS NOT NULL
       AND NOT EXISTS (
            SELECT 1
             FROM run_queue_concurrency_leases
             WHERE run_queue_concurrency_leases.org_id = sqlc.arg(org_id)
               AND run_queue_concurrency_leases.cell_id = concurrency_capacity.cell_id
               AND run_queue_concurrency_leases.environment_id = concurrency_capacity.environment_id
               AND run_queue_concurrency_leases.queue_name = concurrency_capacity.queue_name
               AND COALESCE(run_queue_concurrency_leases.concurrency_key, '') = COALESCE(concurrency_capacity.concurrency_key, '')
               AND run_queue_concurrency_leases.slot_ordinal = slots.slot_ordinal
               AND run_queue_concurrency_leases.released_at IS NULL
       )
     ORDER BY slots.slot_ordinal
     LIMIT 1
),
concurrency_slot AS (
    INSERT INTO run_queue_concurrency_leases (
        org_id,
        cell_id,
        project_id,
        environment_id,
        run_id,
        run_lease_id,
        queue_name,
        concurrency_key,
        slot_ordinal
    )
    SELECT sqlc.arg(org_id),
           concurrency_slot_candidate.cell_id,
           concurrency_slot_candidate.project_id,
           concurrency_slot_candidate.environment_id,
           concurrency_slot_candidate.id,
           sqlc.arg(run_lease_id),
           concurrency_slot_candidate.queue_name,
           concurrency_slot_candidate.concurrency_key,
           concurrency_slot_candidate.slot_ordinal
      FROM concurrency_slot_candidate
    ON CONFLICT DO NOTHING
    RETURNING id
),
leaseable_capacity AS (
    SELECT concurrency_capacity.*
      FROM concurrency_capacity
     WHERE concurrency_capacity.queue_concurrency_limit IS NULL
    UNION ALL
    SELECT concurrency_capacity.*
      FROM concurrency_capacity
     WHERE concurrency_capacity.queue_concurrency_limit IS NOT NULL
       AND EXISTS (SELECT 1 FROM concurrency_slot)
),
restore_runtime_checkpoint AS (
    SELECT runtime_checkpoints.id,
           runtime_checkpoints.run_id,
           run_waits.id AS run_wait_id
      FROM leaseable_capacity
      JOIN workspace_candidate ON workspace_candidate.run_id = leaseable_capacity.id
      JOIN runtime_checkpoints
        ON runtime_checkpoints.org_id = sqlc.arg(org_id)
       AND runtime_checkpoints.run_id = leaseable_capacity.id
       AND runtime_checkpoints.id = leaseable_capacity.latest_runtime_checkpoint_id
       AND runtime_checkpoints.base_workspace_version_id = workspace_candidate.workspace_base_version_id
       AND runtime_checkpoints.state = 'ready'
       AND (runtime_checkpoints.expires_at IS NULL OR runtime_checkpoints.expires_at > now())
      JOIN run_waits
        ON run_waits.org_id = sqlc.arg(org_id)
       AND run_waits.project_id = leaseable_capacity.project_id
       AND run_waits.environment_id = leaseable_capacity.environment_id
       AND run_waits.run_id = leaseable_capacity.id
       AND run_waits.runtime_checkpoint_id = runtime_checkpoints.id
       AND run_waits.state = 'resuming'
),
fenced_workspace_mount AS (
    UPDATE workspace_mounts
       SET fencing_generation = workspace_mounts.fencing_generation + 1,
           updated_at = now()
      FROM workspace_mount
      JOIN workspace_candidate
        ON workspace_candidate.project_id = workspace_mount.project_id
       AND workspace_candidate.environment_id = workspace_mount.environment_id
       AND workspace_candidate.workspace_id = workspace_mount.workspace_id
      JOIN leaseable_capacity
        ON leaseable_capacity.id = workspace_candidate.run_id
     WHERE workspace_mounts.org_id = workspace_mount.org_id
       AND workspace_mounts.project_id = workspace_mount.project_id
       AND workspace_mounts.environment_id = workspace_mount.environment_id
       AND workspace_mounts.workspace_id = workspace_mount.workspace_id
       AND workspace_mounts.id = workspace_mount.id
       AND workspace_mounts.fencing_generation = workspace_mount.fencing_generation
       AND NOT EXISTS (
           SELECT 1
             FROM workspace_leases
            WHERE workspace_leases.org_id = workspace_mount.org_id
              AND workspace_leases.workspace_id = workspace_mount.workspace_id
              AND workspace_leases.lease_kind = 'write'
              AND workspace_leases.state IN ('active', 'releasing')
       )
    RETURNING workspace_mounts.id,
              workspace_mounts.org_id,
              workspace_mounts.project_id,
              workspace_mounts.environment_id,
              workspace_mounts.workspace_id,
              workspace_mounts.fencing_generation,
              workspace_mounts.runtime_instance_id
),
workspace_write_lease AS (
    INSERT INTO workspace_leases (
        org_id,
        cell_id,
        project_id,
        environment_id,
        workspace_id,
        workspace_mount_id,
        lease_kind,
        state,
        owner_run_id,
        base_version_id,
        acquired_version_id,
        acquired_fencing_generation,
        fencing_token,
        heartbeat_token,
        expires_at
    )
    SELECT sqlc.arg(org_id),
           workspace_candidate.cell_id,
           workspace_candidate.project_id,
           workspace_candidate.environment_id,
           workspace_candidate.workspace_id,
           fenced_workspace_mount.id,
           'write',
           'active',
           workspace_candidate.run_id,
           workspace_candidate.workspace_base_version_id,
           workspace_candidate.workspace_base_version_id,
           fenced_workspace_mount.fencing_generation,
           uuidv7()::text,
           uuidv7()::text,
           sqlc.arg(lease_expires_at)
     FROM leaseable_capacity
     JOIN workspace_candidate ON workspace_candidate.run_id = leaseable_capacity.id
     JOIN fenced_workspace_mount ON fenced_workspace_mount.workspace_id = workspace_candidate.workspace_id
     WHERE (
           leaseable_capacity.latest_runtime_checkpoint_id IS NULL
           OR EXISTS (SELECT 1 FROM restore_runtime_checkpoint WHERE restore_runtime_checkpoint.run_id = leaseable_capacity.id)
       )
    ON CONFLICT DO NOTHING
    RETURNING id, workspace_id, workspace_mount_id, owner_run_id, fencing_token, acquired_fencing_generation
),
released_concurrency_slot_without_workspace AS (
    UPDATE run_queue_concurrency_leases
       SET released_at = now()
      FROM concurrency_slot
     WHERE run_queue_concurrency_leases.id = concurrency_slot.id
       AND NOT EXISTS (SELECT 1 FROM workspace_write_lease)
    RETURNING run_queue_concurrency_leases.id
),
leaseable_workspace AS (
    SELECT leaseable_capacity.*,
           workspace_write_lease.id AS workspace_lease_id,
           workspace_write_lease.workspace_mount_id AS workspace_mount_id,
           workspace_write_lease.acquired_fencing_generation AS workspace_mount_fencing_generation,
           workspace_write_lease.fencing_token AS workspace_fencing_token,
           workspace_candidate.deployment_sandbox_id,
           workspace_candidate.image_artifact_format AS sandbox_image_artifact_format,
           workspace_candidate.sandbox_image_artifact_digest,
           workspace_candidate.sandbox_image_artifact_size_bytes,
           workspace_candidate.sandbox_image_artifact_media_type,
           workspace_candidate.image_digest AS sandbox_image_digest,
           workspace_candidate.image_format AS sandbox_image_format,
           workspace_candidate.rootfs_digest AS sandbox_rootfs_digest,
           workspace_candidate.deployment_sandbox_runtime_abi,
           workspace_candidate.guestd_abi,
           workspace_candidate.adapter_abi,
           workspace_candidate.workspace_base_version_id,
           workspace_candidate.workspace_mount_path,
           workspace_candidate.workspace_artifact_digest,
           workspace_candidate.workspace_artifact_size_bytes,
           workspace_candidate.workspace_artifact_media_type,
           workspace_candidate.workspace_artifact_encoding,
           workspace_candidate.workspace_artifact_entry_count,
           COALESCE(runtime_instance_substrate_artifact.id, runtime_substrate_candidate.id) AS runtime_substrate_artifact_id,
           COALESCE(runtime_instance_substrate_artifact.substrate_digest, runtime_substrate_candidate.substrate_digest) AS runtime_substrate_digest,
           COALESCE(runtime_instance_substrate_artifact.substrate_format, runtime_substrate_candidate.substrate_format) AS runtime_substrate_format,
           COALESCE(runtime_instance_substrate_artifact.builder_abi, runtime_substrate_candidate.builder_abi) AS runtime_substrate_builder_abi,
           COALESCE(runtime_instance_substrate_artifact.layout_abi, runtime_substrate_candidate.layout_abi) AS runtime_substrate_layout_abi,
           COALESCE(runtime_instance_substrate_artifact.substrate_size_bytes, runtime_substrate_candidate.substrate_size_bytes) AS runtime_substrate_size_bytes,
           COALESCE(runtime_instance_substrate_artifact.artifact_digest, runtime_substrate_candidate.artifact_digest) AS runtime_substrate_artifact_digest,
           COALESCE(runtime_instance_substrate_artifact.artifact_size_bytes, runtime_substrate_candidate.artifact_size_bytes) AS runtime_substrate_artifact_size_bytes,
           COALESCE(runtime_instance_substrate_artifact.artifact_media_type, runtime_substrate_candidate.artifact_media_type) AS runtime_substrate_artifact_media_type
     FROM leaseable_capacity
     LEFT JOIN workspace_candidate ON workspace_candidate.run_id = leaseable_capacity.id
     JOIN workspace_write_lease ON workspace_write_lease.owner_run_id = leaseable_capacity.id
                                  AND workspace_write_lease.workspace_id = workspace_candidate.workspace_id
     JOIN fenced_workspace_mount ON fenced_workspace_mount.id = workspace_write_lease.workspace_mount_id
     LEFT JOIN runtime_instances AS lease_runtime_instance
       ON lease_runtime_instance.org_id = workspace_candidate.org_id
      AND lease_runtime_instance.project_id = workspace_candidate.project_id
      AND lease_runtime_instance.environment_id = workspace_candidate.environment_id
      AND lease_runtime_instance.id = fenced_workspace_mount.runtime_instance_id
     LEFT JOIN LATERAL (
        SELECT runtime_substrate_artifacts.id,
               runtime_substrate_artifacts.substrate_digest,
               runtime_substrate_artifacts.substrate_format,
               runtime_substrate_artifacts.builder_abi,
               runtime_substrate_artifacts.layout_abi,
               runtime_substrate_artifacts.substrate_size_bytes,
               artifacts.digest AS artifact_digest,
               artifacts.size_bytes AS artifact_size_bytes,
               artifacts.media_type AS artifact_media_type
          FROM runtime_substrate_artifacts
          JOIN artifacts
            ON artifacts.org_id = runtime_substrate_artifacts.org_id
           AND artifacts.cell_id = runtime_substrate_artifacts.cell_id
           AND artifacts.project_id = runtime_substrate_artifacts.project_id
           AND artifacts.environment_id = runtime_substrate_artifacts.environment_id
           AND artifacts.id = runtime_substrate_artifacts.artifact_id
          JOIN deployment_sandboxes
            ON deployment_sandboxes.org_id = runtime_substrate_artifacts.org_id
           AND deployment_sandboxes.cell_id = runtime_substrate_artifacts.cell_id
           AND deployment_sandboxes.project_id = runtime_substrate_artifacts.project_id
           AND deployment_sandboxes.environment_id = runtime_substrate_artifacts.environment_id
           AND deployment_sandboxes.id = runtime_substrate_artifacts.deployment_sandbox_id
         WHERE runtime_substrate_artifacts.org_id = workspace_candidate.org_id
           AND runtime_substrate_artifacts.cell_id = workspace_candidate.cell_id
           AND runtime_substrate_artifacts.project_id = workspace_candidate.project_id
           AND runtime_substrate_artifacts.environment_id = workspace_candidate.environment_id
           AND runtime_substrate_artifacts.id = lease_runtime_instance.runtime_substrate_artifact_id
           AND runtime_substrate_artifacts.retired_at IS NULL
         LIMIT 1
     ) AS runtime_instance_substrate_artifact ON true
     LEFT JOIN LATERAL (
        SELECT runtime_substrate_artifacts.id,
               runtime_substrate_artifacts.substrate_digest,
               runtime_substrate_artifacts.substrate_format,
               runtime_substrate_artifacts.builder_abi,
               runtime_substrate_artifacts.layout_abi,
               runtime_substrate_artifacts.substrate_size_bytes,
               artifacts.digest AS artifact_digest,
               artifacts.size_bytes AS artifact_size_bytes,
               artifacts.media_type AS artifact_media_type
          FROM runtime_substrate_artifacts
          JOIN artifacts
            ON artifacts.org_id = runtime_substrate_artifacts.org_id
           AND artifacts.cell_id = runtime_substrate_artifacts.cell_id
           AND artifacts.project_id = runtime_substrate_artifacts.project_id
           AND artifacts.environment_id = runtime_substrate_artifacts.environment_id
           AND artifacts.id = runtime_substrate_artifacts.artifact_id
          JOIN deployment_sandboxes
            ON deployment_sandboxes.org_id = runtime_substrate_artifacts.org_id
           AND deployment_sandboxes.cell_id = runtime_substrate_artifacts.cell_id
           AND deployment_sandboxes.project_id = runtime_substrate_artifacts.project_id
           AND deployment_sandboxes.environment_id = runtime_substrate_artifacts.environment_id
           AND deployment_sandboxes.id = runtime_substrate_artifacts.deployment_sandbox_id
         WHERE runtime_substrate_artifacts.org_id = workspace_candidate.org_id
           AND runtime_substrate_artifacts.cell_id = workspace_candidate.cell_id
           AND runtime_substrate_artifacts.project_id = workspace_candidate.project_id
           AND runtime_substrate_artifacts.environment_id = workspace_candidate.environment_id
           AND runtime_substrate_artifacts.deployment_sandbox_id = workspace_candidate.deployment_sandbox_id
           AND runtime_substrate_artifacts.retired_at IS NULL
         ORDER BY runtime_substrate_artifacts.last_referenced_at DESC NULLS LAST,
                  runtime_substrate_artifacts.created_at DESC
         LIMIT 1
     ) AS runtime_substrate_candidate ON true
     WHERE workspace_write_lease.id IS NOT NULL
       AND (
           leaseable_capacity.latest_runtime_checkpoint_id IS NULL
           OR EXISTS (SELECT 1 FROM restore_runtime_checkpoint WHERE restore_runtime_checkpoint.run_id = leaseable_capacity.id)
       )
),
selected_restore_runtime_checkpoint AS (
    SELECT runtime_checkpoints.id,
           runtime_checkpoints.run_id
      FROM runtime_checkpoints
      JOIN leaseable_workspace
        ON leaseable_workspace.id = runtime_checkpoints.run_id
     WHERE runtime_checkpoints.org_id = sqlc.arg(org_id)
       AND runtime_checkpoints.id = leaseable_workspace.latest_runtime_checkpoint_id
       AND runtime_checkpoints.base_workspace_version_id = leaseable_workspace.workspace_base_version_id
       AND runtime_checkpoints.state = 'ready'
       AND (runtime_checkpoints.expires_at IS NULL OR runtime_checkpoints.expires_at > now())
),
leased_run_lease AS (
    INSERT INTO run_leases (
        id,
        org_id,
        cell_id,
        route_generation,
        run_id,
        attempt_id,
        worker_instance_id,
        worker_group_id,
        dispatch_message_id,
        dispatch_lease_id,
        dispatch_attempt,
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
           sqlc.arg(org_id),
           candidate.cell_id,
           dispatch.route_generation,
           candidate.id,
           candidate.current_attempt_id,
           sqlc.arg(worker_instance_id),
           dispatch.worker_group_id,
           sqlc.narg(dispatch_message_id)::text,
           sqlc.arg(dispatch_lease_id),
           sqlc.arg(dispatch_attempt)::int,
           'leased',
           sqlc.arg(lease_expires_at),
           candidate.runtime_id,
           dispatch.protocol_version,
           candidate.trace_id,
           sqlc.arg(run_lease_span_id),
           candidate.root_span_id,
           '00-' || candidate.trace_id || '-' || sqlc.arg(run_lease_span_id)::text || '-01',
           (SELECT selected_restore_runtime_checkpoint.id FROM selected_restore_runtime_checkpoint WHERE selected_restore_runtime_checkpoint.run_id = candidate.id)
      FROM leaseable_workspace AS candidate
      JOIN dispatch ON dispatch.run_id = candidate.id
     WHERE candidate.latest_runtime_checkpoint_id IS NULL
        OR EXISTS (SELECT 1 FROM selected_restore_runtime_checkpoint WHERE selected_restore_runtime_checkpoint.run_id = candidate.id)
    RETURNING id, route_generation, worker_instance_id, dispatch_message_id, dispatch_lease_id, dispatch_attempt, attempt_id, lease_expires_at, worker_protocol_version, trace_id, span_id, traceparent, restore_runtime_checkpoint_id
),
runtime_checkpoint_restore AS (
    INSERT INTO runtime_checkpoint_restores (
        id,
        org_id,
        cell_id,
        route_generation,
        project_id,
        environment_id,
        run_id,
        runtime_checkpoint_id,
        run_wait_id,
        run_lease_id,
        worker_instance_id,
        status,
        started_at,
        created_at,
        updated_at
    )
    SELECT uuidv7(),
           sqlc.arg(org_id),
           leaseable_workspace.cell_id,
           leased_run_lease.route_generation,
           leaseable_workspace.project_id,
           leaseable_workspace.environment_id,
           leaseable_workspace.id,
           selected_restore_runtime_checkpoint.id,
           restore_runtime_checkpoint.run_wait_id,
           leased_run_lease.id,
           sqlc.arg(worker_instance_id),
           'restoring',
           now(),
           now(),
           now()
      FROM leased_run_lease
      JOIN leaseable_workspace ON true
      JOIN selected_restore_runtime_checkpoint
        ON selected_restore_runtime_checkpoint.run_id = leaseable_workspace.id
       AND selected_restore_runtime_checkpoint.id = leased_run_lease.restore_runtime_checkpoint_id
      JOIN restore_runtime_checkpoint
        ON restore_runtime_checkpoint.run_id = leaseable_workspace.id
       AND restore_runtime_checkpoint.id = selected_restore_runtime_checkpoint.id
     WHERE leased_run_lease.restore_runtime_checkpoint_id IS NOT NULL
    ON CONFLICT DO NOTHING
    RETURNING id
),
-- Roll back any scheduler/workspace claims if checkpoint restore eligibility
-- disappears after the queue slot and write lease have been claimed.
unleased_runtime_checkpoint_restore AS (
    SELECT selected_restore_runtime_checkpoint.id
      FROM selected_restore_runtime_checkpoint
     WHERE NOT EXISTS (SELECT 1 FROM leased_run_lease)
),
released_workspace_lease_without_run_lease AS (
    UPDATE workspace_leases
       SET state = 'released',
           released_at = now(),
           updated_at = now()
      FROM workspace_write_lease
     WHERE workspace_leases.org_id = sqlc.arg(org_id)
       AND workspace_leases.id = workspace_write_lease.id
       AND workspace_leases.released_at IS NULL
       AND NOT EXISTS (SELECT 1 FROM leased_run_lease)
    RETURNING workspace_leases.id
),
released_concurrency_slot_without_run_lease AS (
    UPDATE run_queue_concurrency_leases
       SET released_at = now()
      FROM concurrency_slot
     WHERE run_queue_concurrency_leases.org_id = sqlc.arg(org_id)
       AND run_queue_concurrency_leases.id = concurrency_slot.id
       AND run_queue_concurrency_leases.released_at IS NULL
       AND NOT EXISTS (SELECT 1 FROM leased_run_lease)
    RETURNING run_queue_concurrency_leases.id
),
lease_cleanup AS (
    SELECT
        (SELECT count(*) FROM unleased_runtime_checkpoint_restore) AS unleased_runtime_checkpoint_restores,
        (SELECT count(*) FROM released_workspace_lease_without_run_lease) AS released_workspace_leases_without_run_lease,
        (SELECT count(*) FROM released_concurrency_slot_without_run_lease) AS released_concurrency_slots_without_run_lease
),
active_time AS (
    SELECT COALESCE(MAX(leaseable_workspace.active_elapsed_ms), 0)::bigint AS active_duration_ms
      FROM leaseable_workspace
),
updated AS (
    UPDATE runs
       SET status = 'running',
           execution_status = 'leased',
           current_run_lease_id = (SELECT id FROM leased_run_lease),
           workspace_mount_id = (SELECT workspace_mount_id FROM workspace_write_lease),
           state_version = state_version + 1,
           updated_at = now()
     WHERE id = (SELECT id FROM leaseable_workspace)
      AND EXISTS (SELECT 1 FROM leased_run_lease)
    RETURNING *
),
activated_runtime_instance AS (
    UPDATE runtime_instances
       SET state = 'running',
           runtime_epoch = runtime_instances.runtime_epoch + 1,
           reserved_cpu_millis = GREATEST(runtime_instances.reserved_cpu_millis, run_runtime_requirements.requested_milli_cpu),
           reserved_memory_mib = GREATEST(runtime_instances.reserved_memory_mib, run_runtime_requirements.requested_memory_mib),
           reserved_disk_mib = GREATEST(runtime_instances.reserved_disk_mib, run_runtime_requirements.requested_disk_mib),
           reserved_execution_slots = GREATEST(runtime_instances.reserved_execution_slots, run_runtime_requirements.requested_execution_slots),
           owner_run_id = updated.id,
           owner_run_lease_id = leased_run_lease.id,
           owner_run_wait_id = NULL,
           owner_workspace_id = workspace_mounts.workspace_id,
           owner_workspace_version_id = workspace_mounts.base_version_id,
           owner_run_state_version = updated.state_version,
           running_at = COALESCE(runtime_instances.running_at, now()),
           last_heartbeat_at = now(),
           updated_at = now()
      FROM updated
      JOIN leased_run_lease ON true
      JOIN workspace_mounts
        ON workspace_mounts.org_id = updated.org_id
       AND workspace_mounts.project_id = updated.project_id
       AND workspace_mounts.environment_id = updated.environment_id
       AND workspace_mounts.id = updated.workspace_mount_id
      JOIN run_runtime_requirements
        ON run_runtime_requirements.org_id = updated.org_id
       AND run_runtime_requirements.run_id = updated.id
     WHERE runtime_instances.org_id = workspace_mounts.org_id
       AND runtime_instances.id = workspace_mounts.runtime_instance_id
       AND runtime_instances.worker_instance_id = leased_run_lease.worker_instance_id
       AND runtime_instances.workspace_mount_id = workspace_mounts.id
       AND runtime_instances.state IN ('binding', 'running', 'waiting_hot')
    RETURNING runtime_instances.id
),
updated_attempt AS (
    UPDATE run_attempts
       SET status = 'running',
           updated_at = now()
      FROM updated
     WHERE run_attempts.org_id = updated.org_id
      AND run_attempts.run_id = updated.id
      AND run_attempts.id = updated.current_attempt_id
      AND EXISTS (SELECT 1 FROM activated_runtime_instance)
    RETURNING run_attempts.id
),
leased_snapshot AS (
    INSERT INTO run_snapshots (org_id, cell_id, run_id, version, status, execution_status, attempt_id, run_lease_id, previous_version, transition, reason)
    SELECT updated.org_id,
           updated.cell_id,
           updated.id,
           updated.state_version,
           updated.status,
           updated.execution_status,
           updated.current_attempt_id,
           leased_run_lease.id,
           updated.state_version - 1,
           'run_lease.leased',
           jsonb_build_object(
               'worker_instance_id', leased_run_lease.worker_instance_id,
               'dispatch_message_id', leased_run_lease.dispatch_message_id,
               'dispatch_attempt', leased_run_lease.dispatch_attempt::int
           )
      FROM updated
      JOIN leased_run_lease ON true
      JOIN updated_attempt ON true
    RETURNING run_snapshots.run_id
)
SELECT
    updated.id,
    updated.org_id,
    updated.cell_id,
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
        updated.current_attempt_id,
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
    run_runtime_requirements.requested_milli_cpu,
    run_runtime_requirements.requested_memory_mib,
    run_runtime_requirements.requested_disk_mib,
    run_runtime_requirements.requested_execution_slots,
    run_runtime_requirements.runtime_id AS requirements_runtime_id,
    run_runtime_requirements.runtime_arch AS requirements_runtime_arch,
    run_runtime_requirements.runtime_abi AS requirements_runtime_abi,
    run_runtime_requirements.kernel_digest AS requirements_kernel_digest,
    run_runtime_requirements.initramfs_digest AS requirements_initramfs_digest,
    run_runtime_requirements.rootfs_digest AS requirements_rootfs_digest,
    run_runtime_requirements.cni_profile AS requirements_cni_profile,
    run_runtime_requirements.network_policy AS requirements_network_policy,
    run_runtime_requirements.placement AS requirements_placement,
    leased_run_lease.id AS run_lease_id,
    leased_run_lease.route_generation AS run_lease_route_generation,
    leased_run_lease.worker_instance_id AS run_lease_worker_instance_id,
    leased_run_lease.dispatch_message_id AS run_lease_dispatch_message_id,
    leased_run_lease.dispatch_lease_id AS run_lease_dispatch_lease_id,
    leased_run_lease.dispatch_attempt AS run_lease_dispatch_attempt,
    run_attempts.attempt_number AS run_lease_attempt_number,
    leased_run_lease.lease_expires_at AS run_lease_expires_at,
    leased_run_lease.worker_protocol_version AS run_lease_worker_protocol_version,
    leased_run_lease.trace_id AS run_lease_trace_id,
    leased_run_lease.span_id AS run_lease_span_id,
    leased_run_lease.traceparent AS run_lease_traceparent,
    leased_run_lease.restore_runtime_checkpoint_id AS run_lease_restore_runtime_checkpoint_id,
    active_time.active_duration_ms AS active_duration_ms,
    leaseable_workspace.workspace_id AS workspace_id,
    leaseable_workspace.workspace_lease_id AS workspace_lease_id,
    leaseable_workspace.workspace_mount_id AS workspace_mount_id,
    leaseable_workspace.workspace_mount_fencing_generation AS workspace_mount_fencing_generation,
    leaseable_workspace.workspace_fencing_token AS workspace_fencing_token,
    leaseable_workspace.deployment_sandbox_id AS workspace_deployment_sandbox_id,
    leaseable_workspace.sandbox_image_artifact_format AS workspace_sandbox_image_artifact_format,
    leaseable_workspace.sandbox_image_artifact_digest AS workspace_sandbox_image_artifact_digest,
    leaseable_workspace.sandbox_image_artifact_size_bytes AS workspace_sandbox_image_artifact_size_bytes,
    leaseable_workspace.sandbox_image_artifact_media_type AS workspace_sandbox_image_artifact_media_type,
    leaseable_workspace.sandbox_image_digest AS workspace_sandbox_image_digest,
    leaseable_workspace.sandbox_image_format AS workspace_sandbox_image_format,
    leaseable_workspace.sandbox_rootfs_digest AS workspace_sandbox_rootfs_digest,
    leaseable_workspace.deployment_sandbox_runtime_abi AS workspace_runtime_abi,
    leaseable_workspace.guestd_abi AS workspace_guestd_abi,
    leaseable_workspace.adapter_abi AS workspace_adapter_abi,
    leaseable_workspace.workspace_base_version_id AS workspace_base_version_id,
    leaseable_workspace.workspace_mount_path AS workspace_mount_path,
    leaseable_workspace.workspace_artifact_digest AS workspace_artifact_digest,
    leaseable_workspace.workspace_artifact_size_bytes AS workspace_artifact_size_bytes,
    leaseable_workspace.workspace_artifact_media_type AS workspace_artifact_media_type,
    leaseable_workspace.workspace_artifact_encoding AS workspace_artifact_encoding,
    leaseable_workspace.workspace_artifact_entry_count AS workspace_artifact_entry_count,
    leaseable_workspace.runtime_substrate_artifact_id AS workspace_runtime_substrate_artifact_id,
    COALESCE(leaseable_workspace.runtime_substrate_digest, '') AS workspace_runtime_substrate_digest,
    COALESCE(leaseable_workspace.runtime_substrate_format, '') AS workspace_runtime_substrate_format,
    COALESCE(leaseable_workspace.runtime_substrate_builder_abi, '') AS workspace_runtime_substrate_builder_abi,
    COALESCE(leaseable_workspace.runtime_substrate_layout_abi, '') AS workspace_runtime_substrate_layout_abi,
    COALESCE(leaseable_workspace.runtime_substrate_size_bytes, 0) AS workspace_runtime_substrate_size_bytes,
    COALESCE(leaseable_workspace.runtime_substrate_artifact_digest, '') AS workspace_runtime_substrate_artifact_digest,
    COALESCE(leaseable_workspace.runtime_substrate_artifact_size_bytes, 0) AS workspace_runtime_substrate_artifact_size_bytes,
    COALESCE(leaseable_workspace.runtime_substrate_artifact_media_type, '') AS workspace_runtime_substrate_artifact_media_type
FROM updated
JOIN lease_cleanup ON true
JOIN leased_run_lease ON true
JOIN leased_snapshot ON true
JOIN active_time ON true
JOIN leaseable_workspace ON leaseable_workspace.id = updated.id
JOIN run_attempts ON run_attempts.org_id = updated.org_id
                 AND run_attempts.run_id = updated.id
                 AND run_attempts.id = leased_run_lease.attempt_id
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
JOIN run_runtime_requirements ON run_runtime_requirements.org_id = updated.org_id
                             AND run_runtime_requirements.run_id = updated.id;

-- name: StartRunLease :one
WITH current_run_lease AS MATERIALIZED (
    SELECT runs.id AS run_id,
           runs.org_id,
           runs.current_attempt_id,
           runs.current_run_lease_id,
           run_leases.status AS run_lease_status
      FROM runs
      JOIN run_leases ON run_leases.id = runs.current_run_lease_id
                          AND run_leases.org_id = runs.org_id
                          AND run_leases.run_id = runs.id
                          AND run_leases.cell_id = runs.cell_id
      JOIN worker_instances
        ON worker_instances.id = run_leases.worker_instance_id
       AND worker_instances.cell_id = runs.cell_id
      JOIN environment_cells
        ON environment_cells.org_id = runs.org_id
       AND environment_cells.project_id = runs.project_id
       AND environment_cells.environment_id = runs.environment_id
       AND environment_cells.cell_id = runs.cell_id
       AND environment_cells.route_generation = run_leases.route_generation
       AND environment_cells.route_state IN ('active', 'draining')
      JOIN org_cells ON org_cells.org_id = environment_cells.org_id
                    AND org_cells.cell_id = environment_cells.cell_id
                    AND org_cells.state = 'active'
      JOIN cells ON cells.id = environment_cells.cell_id
                AND cells.state IN ('active', 'draining')
     WHERE runs.org_id = sqlc.arg(org_id)
       AND runs.id = sqlc.arg(run_id)
       AND runs.status = 'running'
       AND runs.current_run_lease_id = sqlc.arg(run_lease_id)
       AND run_leases.id = sqlc.arg(run_lease_id)
       AND run_leases.worker_instance_id = sqlc.arg(worker_instance_id)
       AND run_leases.cell_id = runs.cell_id
       AND run_leases.status IN ('leased', 'running')
       AND run_leases.lease_expires_at > now()
     FOR UPDATE OF runs, run_leases
),
started_run AS (
    UPDATE runs
       SET status = 'running',
           execution_status = 'executing',
           started_at = COALESCE(runs.started_at, now()),
           active_started_at = COALESCE(runs.active_started_at, now()),
           queued_expires_at = NULL,
           state_version = state_version + CASE WHEN current_run_lease.run_lease_status = 'leased' THEN 1 ELSE 0 END,
           updated_at = now()
      FROM current_run_lease
     WHERE runs.org_id = current_run_lease.org_id
       AND runs.id = current_run_lease.run_id
    RETURNING status, id, runs.org_id, runs.cell_id, runs.current_attempt_id, runs.current_run_lease_id, runs.state_version, current_run_lease.run_lease_status
),
started_run_lease AS (
    UPDATE run_leases
       SET status = 'running',
           started_at = COALESCE(run_leases.started_at, now()),
           renewed_at = now()
      FROM started_run
     WHERE run_leases.id = started_run.current_run_lease_id
       AND run_leases.run_id = started_run.id
       AND run_leases.worker_instance_id = sqlc.arg(worker_instance_id)
     RETURNING run_leases.id, run_leases.restore_runtime_checkpoint_id, started_run.run_lease_status
),
started_attempt AS (
    UPDATE run_attempts
       SET status = 'running',
           started_at = COALESCE(run_attempts.started_at, now()),
           updated_at = now()
      FROM started_run
     WHERE run_attempts.org_id = started_run.org_id
       AND run_attempts.run_id = started_run.id
       AND run_attempts.id = started_run.current_attempt_id
    RETURNING run_attempts.id
),
started_snapshot AS (
    INSERT INTO run_snapshots (org_id, cell_id, run_id, version, status, execution_status, attempt_id, run_lease_id, previous_version, transition, reason)
    SELECT started_run.org_id,
           started_run.cell_id,
           started_run.id,
           started_run.state_version,
           started_run.status,
           'executing',
           started_run.current_attempt_id,
           started_run_lease.id,
           started_run.state_version - 1,
           'run_lease.started',
           jsonb_build_object('worker_instance_id', sqlc.arg(worker_instance_id))
      FROM started_run
      JOIN started_run_lease ON true
      JOIN started_attempt ON true
     WHERE started_run_lease.run_lease_status = 'leased'
    RETURNING run_snapshots.run_id
)
SELECT started_run.status
  FROM started_run
  JOIN started_run_lease ON true
  LEFT JOIN started_snapshot ON true;

-- name: RenewRunLease :one
WITH renewed_session AS (
    UPDATE run_leases
       SET lease_expires_at = sqlc.arg(lease_expires_at),
           renewed_at = now()
      FROM runs
     WHERE runs.org_id = sqlc.arg(org_id)
       AND runs.id = sqlc.arg(run_id)
       AND (
           runs.status = 'running'
           OR (
               runs.status = 'cancelled'
               AND runs.execution_status = 'pending_cancel'
           )
       )
       AND runs.current_run_lease_id = run_leases.id
       AND run_leases.org_id = sqlc.arg(org_id)
       AND run_leases.run_id = sqlc.arg(run_id)
       AND run_leases.id = sqlc.arg(run_lease_id)
       AND run_leases.worker_instance_id = sqlc.arg(worker_instance_id)
       AND run_leases.dispatch_message_id = sqlc.arg(dispatch_message_id)
       AND run_leases.dispatch_lease_id = sqlc.arg(dispatch_lease_id)
       AND run_leases.cell_id = runs.cell_id
       AND run_leases.status IN ('leased', 'running')
       AND run_leases.lease_expires_at > now()
       AND EXISTS (
           SELECT 1
             FROM worker_instances
             JOIN environment_cells
               ON environment_cells.org_id = runs.org_id
              AND environment_cells.project_id = runs.project_id
              AND environment_cells.environment_id = runs.environment_id
              AND environment_cells.cell_id = runs.cell_id
              AND environment_cells.cell_id = worker_instances.cell_id
              AND environment_cells.route_generation = run_leases.route_generation
              AND environment_cells.route_state IN ('active', 'draining')
             JOIN org_cells ON org_cells.org_id = environment_cells.org_id
                           AND org_cells.cell_id = environment_cells.cell_id
                           AND org_cells.state = 'active'
             JOIN cells ON cells.id = environment_cells.cell_id
                       AND cells.state IN ('active', 'draining')
            WHERE worker_instances.id = sqlc.arg(worker_instance_id)
       )
    RETURNING run_leases.id, run_leases.org_id, run_leases.run_id, run_leases.worker_instance_id, run_leases.worker_protocol_version, run_leases.dispatch_message_id, run_leases.dispatch_lease_id, run_leases.dispatch_attempt, run_leases.attempt_id, run_leases.lease_expires_at, run_leases.trace_id, run_leases.span_id, run_leases.traceparent
),
renewed_workspace_lease AS (
    UPDATE workspace_leases
       SET expires_at = sqlc.arg(lease_expires_at),
           renewed_at = now()
      FROM renewed_session
     WHERE workspace_leases.org_id = renewed_session.org_id
       AND workspace_leases.owner_run_id = renewed_session.run_id
       AND workspace_leases.lease_kind = 'write'
       AND workspace_leases.released_at IS NULL
    RETURNING workspace_leases.id
)
SELECT renewed_session.id,
       renewed_session.worker_instance_id,
       renewed_session.worker_protocol_version,
       renewed_session.dispatch_message_id,
       renewed_session.dispatch_lease_id,
       renewed_session.dispatch_attempt,
       run_attempts.attempt_number,
       renewed_session.lease_expires_at,
       renewed_session.trace_id,
       renewed_session.span_id,
       renewed_session.traceparent
  FROM renewed_session
  LEFT JOIN renewed_workspace_lease ON true
  JOIN run_attempts ON run_attempts.org_id = renewed_session.org_id
                   AND run_attempts.run_id = renewed_session.run_id
                   AND run_attempts.id = renewed_session.attempt_id;

-- name: GetRunLeaseQueueLease :one
SELECT run_leases.id,
       run_leases.run_id,
       runs.cell_id,
       runs.project_id,
       runs.environment_id,
       runs.deployment_id,
       runs.task_id,
       runs.session_id,
       run_leases.worker_instance_id,
       run_leases.worker_protocol_version,
       run_leases.dispatch_message_id,
       run_leases.dispatch_lease_id,
       run_leases.dispatch_attempt,
       run_attempts.attempt_number,
       run_leases.lease_expires_at,
       run_leases.route_generation,
       run_queue_items.queue_class,
       run_queue_items.queue_name
  FROM run_leases
  JOIN runs ON runs.org_id = run_leases.org_id
           AND runs.cell_id = run_leases.cell_id
           AND runs.id = run_leases.run_id
  JOIN worker_instances ON worker_instances.id = run_leases.worker_instance_id
                       AND worker_instances.cell_id = runs.cell_id
  JOIN environment_cells
    ON environment_cells.org_id = runs.org_id
   AND environment_cells.project_id = runs.project_id
   AND environment_cells.environment_id = runs.environment_id
   AND environment_cells.cell_id = runs.cell_id
   AND environment_cells.route_generation = run_leases.route_generation
   AND environment_cells.route_state IN ('active', 'draining')
  JOIN org_cells ON org_cells.org_id = environment_cells.org_id
                AND org_cells.cell_id = environment_cells.cell_id
                AND org_cells.state = 'active'
  JOIN cells ON cells.id = environment_cells.cell_id
            AND cells.state IN ('active', 'draining')
  JOIN run_attempts ON run_attempts.org_id = run_leases.org_id
                   AND run_attempts.run_id = run_leases.run_id
                   AND run_attempts.id = run_leases.attempt_id
  JOIN run_queue_items ON run_queue_items.org_id = run_leases.org_id
                     AND run_queue_items.cell_id = run_leases.cell_id
                     AND run_queue_items.run_id = run_leases.run_id
                     AND run_queue_items.route_generation = run_leases.route_generation
                     AND run_queue_items.dispatch_message_id = run_leases.dispatch_message_id
                     AND run_queue_items.reserved_by_worker_instance_id = run_leases.worker_instance_id
 WHERE run_leases.org_id = sqlc.arg(org_id)
   AND run_leases.run_id = sqlc.arg(run_id)
   AND run_leases.cell_id = sqlc.arg(cell_id)
   AND run_leases.id = sqlc.arg(run_lease_id)
   AND run_leases.worker_instance_id = sqlc.arg(worker_instance_id)
   AND run_leases.status IN ('leased', 'running')
   AND run_leases.lease_expires_at > now()
   AND run_queue_items.status = 'reserved'
   AND run_queue_items.reservation_expires_at > now();

-- name: GetCurrentRunningRunLease :one
SELECT run_leases.id,
       run_leases.run_id,
       runs.cell_id,
       runs.project_id,
       runs.environment_id,
       runs.deployment_id,
       runs.task_id,
       runs.session_id,
       run_leases.worker_instance_id,
       run_leases.worker_protocol_version,
       run_leases.dispatch_message_id,
       run_leases.dispatch_lease_id,
       run_leases.dispatch_attempt,
       run_attempts.attempt_number,
       run_leases.lease_expires_at,
       run_queue_items.queue_name
  FROM run_leases
  JOIN runs ON runs.org_id = run_leases.org_id
           AND runs.cell_id = run_leases.cell_id
           AND runs.id = run_leases.run_id
  JOIN worker_instances ON worker_instances.id = run_leases.worker_instance_id
                       AND worker_instances.cell_id = runs.cell_id
  JOIN environment_cells
    ON environment_cells.org_id = runs.org_id
   AND environment_cells.project_id = runs.project_id
   AND environment_cells.environment_id = runs.environment_id
   AND environment_cells.cell_id = runs.cell_id
   AND environment_cells.route_generation = run_leases.route_generation
   AND environment_cells.route_state IN ('active', 'draining')
  JOIN org_cells ON org_cells.org_id = environment_cells.org_id
                AND org_cells.cell_id = environment_cells.cell_id
                AND org_cells.state = 'active'
  JOIN cells ON cells.id = environment_cells.cell_id
            AND cells.state IN ('active', 'draining')
  JOIN run_attempts ON run_attempts.org_id = run_leases.org_id
                   AND run_attempts.run_id = run_leases.run_id
                   AND run_attempts.id = run_leases.attempt_id
  JOIN run_queue_items ON run_queue_items.org_id = run_leases.org_id
                     AND run_queue_items.cell_id = run_leases.cell_id
                     AND run_queue_items.run_id = run_leases.run_id
                     AND run_queue_items.route_generation = run_leases.route_generation
                     AND run_queue_items.dispatch_message_id = run_leases.dispatch_message_id
                     AND run_queue_items.reserved_by_worker_instance_id = run_leases.worker_instance_id
 WHERE run_leases.org_id = sqlc.arg(org_id)
   AND run_leases.run_id = sqlc.arg(run_id)
   AND run_leases.cell_id = sqlc.arg(cell_id)
   AND run_leases.id = sqlc.arg(run_lease_id)
   AND run_leases.worker_instance_id = sqlc.arg(worker_instance_id)
   AND run_leases.status = 'running'
   AND run_leases.lease_expires_at > now()
   AND runs.status = 'running'
   AND runs.execution_status = 'executing'
   AND runs.current_run_lease_id = run_leases.id
   AND run_queue_items.status = 'reserved'
   AND run_queue_items.reservation_expires_at > now();

-- name: GetRunLeaseRuntimeRelease :one
SELECT run_leases.runtime_id,
       runtime_releases.runtime_arch,
       runtime_releases.runtime_abi,
       runtime_releases.kernel_digest,
       runtime_releases.initramfs_digest,
       runtime_releases.rootfs_digest,
       runtime_releases.cni_profile
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
eligible AS (
    SELECT runs.org_id,
           runs.cell_id,
           runs.id AS run_id,
           runs.project_id,
           runs.environment_id,
           runs.session_id,
           runs.current_attempt_id AS previous_attempt_id,
           run_attempts.attempt_number AS previous_attempt_number,
           runs.status AS previous_status,
           runs.execution_status AS previous_execution_status,
           runs.locked_retry_policy
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
       AND worker_instances.cell_id = runs.cell_id
      JOIN environment_cells
        ON environment_cells.org_id = runs.org_id
       AND environment_cells.project_id = runs.project_id
       AND environment_cells.environment_id = runs.environment_id
       AND environment_cells.cell_id = runs.cell_id
       AND environment_cells.route_generation = run_leases.route_generation
       AND environment_cells.route_state IN ('active', 'draining')
      JOIN org_cells ON org_cells.org_id = environment_cells.org_id
                    AND org_cells.cell_id = environment_cells.cell_id
                    AND org_cells.state = 'active'
      JOIN cells ON cells.id = environment_cells.cell_id
                AND cells.state IN ('active', 'draining')
      JOIN run_queue_items
        ON run_queue_items.org_id = runs.org_id
       AND run_queue_items.cell_id = run_leases.cell_id
       AND run_queue_items.run_id = runs.id
       AND run_queue_items.route_generation = run_leases.route_generation
       AND run_queue_items.reserved_by_worker_instance_id = sqlc.arg(worker_instance_id)
       AND run_queue_items.dispatch_message_id = sqlc.arg(dispatch_message_id)
       AND run_queue_items.status = 'reserved'
       AND run_queue_items.reservation_expires_at > now()
      JOIN run_attempts ON run_attempts.org_id = runs.org_id
                       AND run_attempts.run_id = runs.id
                       AND run_attempts.id = runs.current_attempt_id
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
               AND sqlc.narg(workspace_artifact_digest)::text IS NOT NULL
               AND sqlc.narg(workspace_artifact_size_bytes)::bigint IS NOT NULL
                   AND sqlc.narg(workspace_artifact_media_type)::text IS NOT NULL
                   AND sqlc.narg(workspace_artifact_encoding)::text IS NOT NULL
                   AND sqlc.narg(workspace_artifact_entry_count)::int IS NOT NULL
                   AND sqlc.narg(workspace_mount_path)::text IS NOT NULL
                   AND NOT EXISTS (
                   SELECT 1
                     FROM cas_objects
                    WHERE cas_objects.org_id = runs.org_id
                      AND cas_objects.cell_id = runs.cell_id
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
                      AND workspace_leases.workspace_id = workspaces.id
                      AND workspace_leases.owner_run_id = runs.id
                      AND workspace_leases.id = sqlc.narg(workspace_lease_id)::uuid
                      AND workspace_leases.fencing_token = sqlc.narg(workspace_fencing_token)::text
                      AND workspace_leases.lease_kind = 'write'
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
     FOR UPDATE OF runs, run_leases, run_queue_items
),
effective_release AS (
    SELECT eligible.run_id,
           CASE
             WHEN eligible.previous_status = 'cancelled' AND eligible.previous_execution_status = 'pending_cancel' THEN 'cancelled'::run_status
             ELSE sqlc.arg(run_status)::run_status
           END AS run_status,
           CASE
             WHEN eligible.previous_status = 'cancelled' AND eligible.previous_execution_status = 'pending_cancel' THEN 'cancelled'::run_attempt_status
             ELSE sqlc.arg(attempt_status)::run_attempt_status
           END AS attempt_status,
           CASE
             WHEN eligible.previous_status = 'cancelled' AND eligible.previous_execution_status = 'pending_cancel' THEN NULL::int
             ELSE sqlc.narg(exit_code)::int
           END AS exit_code,
           CASE
             WHEN eligible.previous_status = 'cancelled' AND eligible.previous_execution_status = 'pending_cancel' THEN NULL::jsonb
             ELSE sqlc.arg(output)::jsonb
           END AS output,
           CASE
             WHEN eligible.previous_status = 'cancelled' AND eligible.previous_execution_status = 'pending_cancel'
             THEN COALESCE(sqlc.narg(error_message)::text, 'run cancelled')::text
             ELSE sqlc.narg(error_message)::text
           END AS error_message,
           CASE
             WHEN eligible.previous_status = 'cancelled' AND eligible.previous_execution_status = 'pending_cancel' THEN 'run.cancelled'
             ELSE sqlc.arg(terminal_event_kind)::text
           END AS terminal_event_kind,
           CASE
             WHEN eligible.previous_status = 'cancelled' AND eligible.previous_execution_status = 'pending_cancel'
             THEN jsonb_build_object('reason', COALESCE(sqlc.narg(error_message)::text, 'run cancelled'), 'origin', 'cancel_operation')
             ELSE sqlc.arg(terminal_event_payload)::jsonb
           END AS terminal_event_payload
      FROM eligible
),
retry_failure AS (
    SELECT eligible.run_id,
           CASE
             WHEN effective_release.run_status <> 'failed' THEN ''
             WHEN (effective_release.terminal_event_payload ->> 'failure_kind') = 'max_duration' THEN 'timeout'
             WHEN effective_release.exit_code IS NOT NULL AND effective_release.exit_code <> 0 THEN 'non_zero_exit'
             WHEN (effective_release.terminal_event_payload ->> 'failure_kind') IN ('task_not_found', 'duplicate_task_id', 'missing_config', 'task_parse_failed') THEN 'non_retryable'
             ELSE 'transient_error'
           END AS reason
      FROM eligible
      JOIN effective_release ON effective_release.run_id = eligible.run_id
),
retry_plan AS (
    SELECT eligible.run_id,
           eligible.org_id,
           eligible.cell_id,
           eligible.project_id,
           eligible.environment_id,
           eligible.previous_attempt_id,
           eligible.previous_attempt_number,
           uuidv7() AS next_attempt_id,
           eligible.previous_attempt_number + 1 AS next_attempt_number,
           retry_failure.reason,
           delay.delay_ms,
           now() + ((delay.delay_ms::text || ' milliseconds')::interval) AS retry_after,
           eligible.locked_retry_policy
      FROM eligible
      JOIN retry_failure ON retry_failure.run_id = eligible.run_id
      CROSS JOIN LATERAL (
          SELECT (eligible.locked_retry_policy ->> 'maxAttempts')::int AS max_attempts,
                 COALESCE(NULLIF(eligible.locked_retry_policy #>> '{backoff,minMs}', '')::bigint, 1000) AS min_ms,
                 COALESCE(NULLIF(eligible.locked_retry_policy #>> '{backoff,maxMs}', '')::bigint, 30000) AS max_ms,
                 COALESCE(NULLIF(eligible.locked_retry_policy #>> '{backoff,factor}', '')::numeric, 2) AS factor,
                 COALESCE(NULLIF(eligible.locked_retry_policy #>> '{backoff,jitter}', ''), 'full') AS jitter
      ) policy
      CROSS JOIN LATERAL (
          SELECT LEAST(
                     GREATEST(policy.max_ms, 0),
                     GREATEST(
                         0,
                         round(GREATEST(policy.min_ms, 0)::numeric * power(GREATEST(policy.factor, 0), eligible.previous_attempt_number - 1))::bigint
                     )
                 ) AS base_delay_ms
      ) base_delay
      CROSS JOIN LATERAL (
          SELECT CASE
                   WHEN policy.jitter = 'full' THEN floor(random() * GREATEST(base_delay.base_delay_ms, 1))::bigint
                   ELSE base_delay.base_delay_ms
                 END AS delay_ms
      ) delay
      JOIN effective_release ON true
     WHERE effective_release.run_id = eligible.run_id
       AND effective_release.run_status = 'failed'
       AND jsonb_typeof(eligible.locked_retry_policy) = 'object'
       AND COALESCE((eligible.locked_retry_policy ->> 'enabled')::boolean, true)
       AND retry_failure.reason <> 'non_retryable'
       AND eligible.previous_attempt_number < policy.max_attempts
),
retry_attempt AS (
    INSERT INTO run_attempts (id, org_id, cell_id, run_id, attempt_number, status, previous_attempt_id)
    SELECT retry_plan.next_attempt_id,
           retry_plan.org_id,
           retry_plan.cell_id,
           retry_plan.run_id,
           retry_plan.next_attempt_number,
           'queued',
           retry_plan.previous_attempt_id
      FROM retry_plan
    RETURNING id, org_id, run_id, attempt_number
),
completed_queue_entry AS (
    UPDATE run_queue_items
       SET status = CASE WHEN retry_plan.run_id IS NOT NULL THEN 'queued'::run_queue_status ELSE 'completed'::run_queue_status END,
           queue_timestamp = COALESCE(retry_plan.retry_after, run_queue_items.queue_timestamp),
           dispatch_message_id = CASE WHEN retry_plan.run_id IS NOT NULL THEN NULL ELSE run_queue_items.dispatch_message_id END,
           reserved_by_worker_instance_id = CASE WHEN retry_plan.run_id IS NOT NULL THEN NULL ELSE run_queue_items.reserved_by_worker_instance_id END,
           reservation_expires_at = CASE WHEN retry_plan.run_id IS NOT NULL THEN NULL ELSE run_queue_items.reservation_expires_at END,
           dispatch_generation = dispatch_generation + 1,
           last_error = CASE WHEN retry_plan.run_id IS NOT NULL THEN '' ELSE run_queue_items.last_error END,
           enqueued_at = CASE WHEN retry_plan.run_id IS NOT NULL THEN now() ELSE run_queue_items.enqueued_at END,
           updated_at = now(),
           finished_at = CASE WHEN retry_plan.run_id IS NOT NULL THEN NULL ELSE now() END
      FROM eligible
      LEFT JOIN retry_plan ON retry_plan.run_id = eligible.run_id
     WHERE run_queue_items.org_id = eligible.org_id
       AND run_queue_items.run_id = eligible.run_id
       AND run_queue_items.reserved_by_worker_instance_id = sqlc.arg(worker_instance_id)
       AND run_queue_items.dispatch_message_id = sqlc.arg(dispatch_message_id)
    RETURNING run_queue_items.run_id
),
released AS (
    UPDATE runs
       SET status = CASE WHEN retry_plan.run_id IS NOT NULL THEN 'queued'::run_status ELSE effective_release.run_status END,
           execution_status = CASE WHEN retry_plan.run_id IS NOT NULL THEN 'queued'::run_execution_status ELSE 'finished'::run_execution_status END,
           terminal_outcome = CASE WHEN retry_plan.run_id IS NOT NULL THEN NULL ELSE effective_release.run_status::text::run_terminal_outcome END,
           current_run_lease_id = NULL,
           current_attempt_id = COALESCE(retry_attempt.id, runs.current_attempt_id),
           current_attempt_number = COALESCE(retry_attempt.attempt_number, runs.current_attempt_number),
           queue_timestamp = COALESCE(retry_plan.retry_after, runs.queue_timestamp),
           state_version = runs.state_version + CASE WHEN retry_plan.run_id IS NOT NULL THEN 2 ELSE 1 END,
           active_elapsed_ms = LEAST(
               runs.active_elapsed_ms
               +
               CASE
                 WHEN runs.active_started_at IS NULL THEN 0
                 ELSE GREATEST((EXTRACT(EPOCH FROM (now() - runs.active_started_at)) * 1000)::bigint, 0)
               END,
               runs.max_active_duration_ms
           ),
           active_started_at = NULL,
           exit_code = CASE WHEN retry_plan.run_id IS NOT NULL THEN NULL ELSE effective_release.exit_code END,
           output = CASE WHEN retry_plan.run_id IS NOT NULL THEN NULL ELSE effective_release.output END,
           error_message = CASE WHEN retry_plan.run_id IS NOT NULL THEN NULL ELSE effective_release.error_message END,
           finished_at = CASE WHEN retry_plan.run_id IS NOT NULL THEN NULL ELSE now() END,
           updated_at = now()
      FROM eligible
      JOIN effective_release ON effective_release.run_id = eligible.run_id
      JOIN completed_queue_entry ON completed_queue_entry.run_id = eligible.run_id
      JOIN run_leases current_run_lease
        ON current_run_lease.id = sqlc.arg(run_lease_id)
       AND current_run_lease.run_id = eligible.run_id
       AND current_run_lease.worker_instance_id = sqlc.arg(worker_instance_id)
       AND current_run_lease.dispatch_message_id = sqlc.arg(dispatch_message_id)
       AND current_run_lease.dispatch_lease_id = sqlc.arg(dispatch_lease_id)
      LEFT JOIN retry_plan ON retry_plan.run_id = eligible.run_id
      LEFT JOIN retry_attempt ON retry_attempt.org_id = retry_plan.org_id
                             AND retry_attempt.run_id = retry_plan.run_id
     WHERE runs.org_id = eligible.org_id
       AND runs.id = eligible.run_id
    RETURNING runs.*
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
released_with_result_size AS (
    SELECT released.*,
           CASE WHEN released.output IS NULL THEN NULL ELSE octet_length(released.output::text) END AS output_json_bytes
      FROM released
      LEFT JOIN retry_plan ON retry_plan.run_id = released.id
     WHERE retry_plan.run_id IS NULL
),
released_session AS (
    SELECT released_with_result_size.session_id AS id
      FROM released_with_result_size
),
workspace_commit_input AS (
    SELECT released.org_id,
           released.cell_id,
           workspaces.route_generation,
           released.project_id,
           released.environment_id,
           released.id AS run_id,
           released.session_id,
           workspaces.id AS workspace_id,
           workspace_leases.id AS workspace_lease_id,
           workspace_leases.base_version_id AS base_version_id,
           uuidv7() AS artifact_id,
           uuidv7() AS workspace_version_id,
           sqlc.narg(workspace_artifact_digest)::text AS artifact_digest,
           sqlc.narg(workspace_artifact_size_bytes)::bigint AS artifact_size_bytes,
           sqlc.narg(workspace_artifact_media_type)::text AS artifact_media_type,
           sqlc.narg(workspace_artifact_encoding)::text AS artifact_encoding,
           sqlc.narg(workspace_artifact_entry_count)::int AS artifact_entry_count
      FROM released
      JOIN effective_release ON effective_release.run_id = released.id
      JOIN released_session ON released_session.id = released.session_id
      JOIN released_with_result_size ON released_with_result_size.id = released.id
      JOIN workspaces
        ON workspaces.org_id = released.org_id
       AND workspaces.project_id = released.project_id
       AND workspaces.environment_id = released.environment_id
       AND workspaces.id = released.workspace_id
       AND workspaces.state = 'active'
      JOIN workspace_leases
        ON workspace_leases.org_id = workspaces.org_id
       AND workspace_leases.workspace_id = workspaces.id
       AND workspace_leases.owner_run_id = released.id
       AND workspace_leases.id = sqlc.narg(workspace_lease_id)::uuid
       AND workspace_leases.fencing_token = sqlc.narg(workspace_fencing_token)::text
       AND workspace_leases.lease_kind = 'write'
       AND workspace_leases.base_version_id IS NOT DISTINCT FROM sqlc.narg(workspace_base_version_id)::uuid
       AND workspace_leases.released_at IS NULL
     WHERE effective_release.run_status = 'succeeded'
       AND (
           released_with_result_size.output IS NULL
           OR released_with_result_size.output_json_bytes <= 1048576
       )
       AND sqlc.narg(workspace_artifact_digest)::text IS NOT NULL
       AND workspaces.current_version_id IS NOT DISTINCT FROM workspace_leases.base_version_id
),
published_workspace_cas_object AS (
    INSERT INTO cas_objects (org_id, cell_id, digest, size_bytes, media_type)
    SELECT workspace_commit_input.org_id,
           workspace_commit_input.cell_id,
           workspace_commit_input.artifact_digest,
           workspace_commit_input.artifact_size_bytes,
           workspace_commit_input.artifact_media_type
      FROM workspace_commit_input
    ON CONFLICT (org_id, cell_id, digest) DO UPDATE
       SET size_bytes = cas_objects.size_bytes
     WHERE cas_objects.size_bytes = EXCLUDED.size_bytes
       AND cas_objects.media_type = EXCLUDED.media_type
    RETURNING org_id, cell_id, digest
),
inserted_workspace_artifact AS (
    INSERT INTO artifacts (
        id,
        org_id,
        cell_id,
        route_generation,
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
           workspace_commit_input.cell_id,
           workspace_commit_input.route_generation,
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
       AND published_workspace_cas_object.cell_id = workspace_commit_input.cell_id
       AND published_workspace_cas_object.digest = workspace_commit_input.artifact_digest
    RETURNING id
),
published_workspace_version AS (
    INSERT INTO workspace_versions (
        id,
        org_id,
        cell_id,
        project_id,
        environment_id,
        workspace_id,
        parent_version_id,
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
           workspace_commit_input.org_id,
           workspace_commit_input.cell_id,
           workspace_commit_input.project_id,
           workspace_commit_input.environment_id,
           workspace_commit_input.workspace_id,
           workspace_commit_input.base_version_id,
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
    RETURNING id, org_id, workspace_id, parent_version_id
),
advanced_workspace AS (
    UPDATE workspaces
       SET current_version_id = published_workspace_version.id,
           dirty_state = 'clean',
           last_activity_at = now(),
           updated_at = now()
      FROM published_workspace_version
      JOIN workspace_commit_input
        ON workspace_commit_input.workspace_version_id = published_workspace_version.id
     WHERE workspaces.org_id = published_workspace_version.org_id
       AND workspaces.id = published_workspace_version.workspace_id
       AND workspaces.current_version_id IS NOT DISTINCT FROM published_workspace_version.parent_version_id
    RETURNING workspaces.id
),
released_workspace_lease AS (
    UPDATE workspace_leases
       SET state = 'released',
           released_at = now(),
           renewed_at = now(),
           updated_at = now()
      FROM released
     WHERE workspace_leases.org_id = released.org_id
       AND workspace_leases.owner_run_id = released.id
       AND workspace_leases.lease_kind = 'write'
       AND workspace_leases.released_at IS NULL
    RETURNING workspace_leases.id,
              workspace_leases.workspace_id,
              workspace_leases.workspace_mount_id
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
             FROM workspace_leases
            WHERE workspace_leases.org_id = workspace_mounts.org_id
              AND workspace_leases.project_id = workspace_mounts.project_id
              AND workspace_leases.environment_id = workspace_mounts.environment_id
              AND workspace_leases.workspace_id = workspace_mounts.workspace_id
              AND workspace_leases.workspace_mount_id = workspace_mounts.id
              AND workspace_leases.id <> released_workspace_lease.id
              AND workspace_leases.state IN ('active', 'releasing')
              AND workspace_leases.expires_at > now()
       )
       AND NOT EXISTS (
           SELECT 1
             FROM workspace_execs
            WHERE workspace_execs.org_id = workspace_mounts.org_id
              AND workspace_execs.project_id = workspace_mounts.project_id
              AND workspace_execs.environment_id = workspace_mounts.environment_id
              AND workspace_execs.workspace_id = workspace_mounts.workspace_id
              AND (workspace_execs.workspace_mount_id = workspace_mounts.id OR workspace_execs.workspace_mount_id IS NULL)
              AND workspace_execs.state IN ('queued', 'materializing', 'running')
       )
       AND NOT EXISTS (
           SELECT 1
             FROM workspace_pty_sessions
            WHERE workspace_pty_sessions.org_id = workspace_mounts.org_id
              AND workspace_pty_sessions.project_id = workspace_mounts.project_id
              AND workspace_pty_sessions.environment_id = workspace_mounts.environment_id
              AND workspace_pty_sessions.workspace_id = workspace_mounts.workspace_id
              AND (workspace_pty_sessions.workspace_mount_id = workspace_mounts.id OR workspace_pty_sessions.workspace_mount_id IS NULL)
              AND workspace_pty_sessions.state IN ('creating', 'open', 'resizing', 'closing')
       )
    RETURNING runtime_instances.id
),
released_run_lease AS (
    UPDATE run_leases
       SET released_at = now(),
           renewed_at = now(),
           status = 'released',
           -- Store cumulative active time so a restored run can resume from prior usage.
           active_duration_ms = released.active_elapsed_ms
      FROM released
     WHERE run_leases.id = sqlc.arg(run_lease_id)
       AND run_leases.run_id = released.id
       AND run_leases.worker_instance_id = sqlc.arg(worker_instance_id)
       AND run_leases.dispatch_message_id = sqlc.arg(dispatch_message_id)
       AND run_leases.dispatch_lease_id = sqlc.arg(dispatch_lease_id)
    RETURNING run_leases.id,
              run_leases.attempt_id,
              run_leases.trace_id,
              run_leases.span_id,
              run_leases.parent_span_id,
              run_leases.traceparent,
              run_leases.active_duration_ms,
              run_leases.restore_runtime_checkpoint_id
),
released_concurrency_slot AS (
    UPDATE run_queue_concurrency_leases
       SET released_at = now()
      FROM released
     WHERE run_queue_concurrency_leases.org_id = sqlc.arg(org_id)
       AND run_queue_concurrency_leases.run_id = released.id
       AND run_queue_concurrency_leases.run_lease_id = sqlc.arg(run_lease_id)
       AND run_queue_concurrency_leases.released_at IS NULL
    RETURNING run_queue_concurrency_leases.id
),
cancelled_run_waits AS (
    UPDATE run_waits
       SET state = 'cancelled',
           cancelled_at = now(),
           updated_at = now()
      FROM released
     WHERE run_waits.org_id = sqlc.arg(org_id)
       AND run_waits.run_id = released.id
       AND run_waits.state IN ('live_waiting', 'checkpointing', 'checkpointed_waiting', 'resolved_live')
    RETURNING run_waits.org_id, run_waits.run_id, run_waits.id
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
     WHERE runtime_checkpoints.org_id = sqlc.arg(org_id)
       AND runtime_checkpoints.run_id = released.id
       AND runtime_checkpoints.state = 'creating'
    RETURNING runtime_checkpoints.run_id, runtime_checkpoints.id
),
completed_restore_runtime_checkpoint AS (
    SELECT released_run_lease.restore_runtime_checkpoint_id AS id
      FROM released
      JOIN released_run_lease ON true
     WHERE released_run_lease.restore_runtime_checkpoint_id IS NOT NULL
       AND released.error_message IS NULL
),
completed_runtime_checkpoint_restore AS (
    UPDATE runtime_checkpoint_restores
       SET status = 'restored',
           error_message = NULL,
           finished_at = COALESCE(runtime_checkpoint_restores.finished_at, now()),
           updated_at = now()
      FROM released
      JOIN released_run_lease ON true
     WHERE runtime_checkpoint_restores.org_id = sqlc.arg(org_id)
       AND runtime_checkpoint_restores.run_id = released.id
       AND runtime_checkpoint_restores.run_lease_id = released_run_lease.id
       AND runtime_checkpoint_restores.runtime_checkpoint_id = released_run_lease.restore_runtime_checkpoint_id
       AND runtime_checkpoint_restores.status = 'restoring'
       AND released.error_message IS NULL
    RETURNING runtime_checkpoint_restores.id
),
failed_runtime_checkpoint_restore AS (
    UPDATE runtime_checkpoint_restores
       SET status = 'failed',
           error_message = released.error_message,
           finished_at = COALESCE(runtime_checkpoint_restores.finished_at, now()),
           updated_at = now()
      FROM released
      JOIN released_run_lease ON true
     WHERE runtime_checkpoint_restores.org_id = sqlc.arg(org_id)
       AND runtime_checkpoint_restores.run_id = released.id
       AND runtime_checkpoint_restores.run_lease_id = released_run_lease.id
       AND runtime_checkpoint_restores.runtime_checkpoint_id = released_run_lease.restore_runtime_checkpoint_id
       AND runtime_checkpoint_restores.status = 'restoring'
       AND released.error_message IS NOT NULL
    RETURNING runtime_checkpoint_restores.id
),
released_attempt AS (
    UPDATE run_attempts
       SET status = effective_release.attempt_status,
           output = effective_release.output,
           error_message = effective_release.error_message,
           finished_at = now(),
           updated_at = now()
      FROM released
      JOIN released_run_lease ON true
      JOIN effective_release ON true
     WHERE run_attempts.org_id = released.org_id
       AND run_attempts.run_id = released.id
       AND run_attempts.id = released_run_lease.attempt_id
       AND effective_release.run_id = released.id
    RETURNING run_attempts.id, run_attempts.attempt_number
),
active_time_delta AS (
    SELECT GREATEST(
               released_run_lease.active_duration_ms
               - COALESCE((
                   SELECT SUM(usage_facts.quantity)::bigint
                     FROM usage_facts
                    WHERE usage_facts.org_id = released.org_id
                      AND usage_facts.run_id = released.id
                      AND usage_facts.meter = 'active_time'
               ), 0),
               0
           )::bigint AS quantity
      FROM released
      JOIN released_run_lease ON true
),
active_time_usage_event AS (
    INSERT INTO usage_facts (org_id, cell_id, project_id, environment_id, source_kind, source_id, run_id, attempt_id, run_lease_id, trace_id, span_id, snapshot_version, meter, quantity, unit, measured_to, details, idempotency_key)
    SELECT released.org_id,
           released.cell_id,
           released.project_id,
           released.environment_id,
           'run_lease',
           released_run_lease.id,
           released.id,
           released_run_lease.attempt_id,
           released_run_lease.id,
           released_run_lease.trace_id,
           released_run_lease.span_id,
           CASE WHEN retry_plan.run_id IS NOT NULL THEN released.state_version - 1 ELSE released.state_version END,
           'active_time',
           active_time_delta.quantity,
           'ms',
           now(),
           jsonb_build_object('phase', 'final'),
           'active_time:' || released_run_lease.id::text || ':final'
      FROM released
      JOIN released_run_lease ON true
      JOIN active_time_delta ON true
      LEFT JOIN retry_plan ON true
     WHERE active_time_delta.quantity > 0
    ON CONFLICT DO NOTHING
    RETURNING id
),
output_usage_event AS (
    INSERT INTO usage_facts (org_id, cell_id, project_id, environment_id, source_kind, source_id, run_id, attempt_id, run_lease_id, trace_id, span_id, snapshot_version, meter, quantity, unit, measured_to, details, idempotency_key)
    SELECT released.org_id,
           released.cell_id,
           released.project_id,
           released.environment_id,
           'run_lease',
           released_run_lease.id,
           released.id,
           released_run_lease.attempt_id,
           released_run_lease.id,
           released_run_lease.trace_id,
           released_run_lease.span_id,
           CASE WHEN retry_plan.run_id IS NOT NULL THEN released.state_version - 1 ELSE released.state_version END,
           'output_bytes',
           octet_length(effective_release.output::text)::bigint,
           'bytes',
           now(),
           jsonb_build_object('terminal_event_kind', effective_release.terminal_event_kind),
           'output:' || released_run_lease.id::text || ':final'
      FROM released
      JOIN released_run_lease ON true
      JOIN effective_release ON true
      LEFT JOIN retry_plan ON true
     WHERE effective_release.output IS NOT NULL
       AND effective_release.run_id = released.id
       AND octet_length(effective_release.output::text) > 0
    ON CONFLICT DO NOTHING
    RETURNING id
),
released_snapshot AS (
    INSERT INTO run_snapshots (org_id, cell_id, run_id, version, status, execution_status, terminal_outcome, attempt_id, run_lease_id, previous_version, transition, reason)
    SELECT released.org_id,
           released.cell_id,
           released.id,
           CASE WHEN retry_plan.run_id IS NOT NULL THEN released.state_version - 1 ELSE released.state_version END,
           effective_release.run_status,
           'finished',
           effective_release.run_status::text::run_terminal_outcome,
           released_attempt.id,
           released_run_lease.id,
           CASE WHEN retry_plan.run_id IS NOT NULL THEN released.state_version - 2 ELSE released.state_version - 1 END,
           CASE
             WHEN effective_release.run_status = 'succeeded' THEN 'run.completed'
             WHEN effective_release.run_status = 'cancelled' THEN 'run.cancelled'
             ELSE 'run.failed'
           END,
           effective_release.terminal_event_payload
      FROM released
      JOIN released_run_lease ON true
      JOIN released_attempt ON true
      JOIN effective_release ON true
      LEFT JOIN retry_plan ON true
     WHERE effective_release.run_id = released.id
    RETURNING version
),
retry_decision AS (
    INSERT INTO run_retry_decisions (org_id, cell_id, project_id, environment_id, run_id, attempt_id, run_lease_id, snapshot_version, decision, reason, error_class, retry_after, next_attempt_number, policy_snapshot, error)
    SELECT released.org_id,
           released.cell_id,
           released.project_id,
           released.environment_id,
           released.id,
           released_run_lease.attempt_id,
           released_run_lease.id,
           released_snapshot.version,
           CASE
             WHEN retry_plan.run_id IS NOT NULL THEN 'retry'
             WHEN effective_release.run_status = 'cancelled' THEN 'cancel_run'
             ELSE 'fail_run'
           END::run_retry_decision_kind,
           COALESCE(retry_plan.reason, effective_release.error_message, effective_release.terminal_event_kind),
           COALESCE(retry_plan.reason, effective_release.terminal_event_kind),
           retry_plan.retry_after,
           retry_plan.next_attempt_number,
           released.locked_retry_policy,
           effective_release.terminal_event_payload
      FROM released
      JOIN released_run_lease ON true
      JOIN released_snapshot ON true
      JOIN effective_release ON true
      LEFT JOIN retry_plan ON true
     WHERE effective_release.run_id = released.id
       AND effective_release.run_status IN ('failed', 'cancelled')
    ON CONFLICT DO NOTHING
    RETURNING id
),
retry_snapshot AS (
    INSERT INTO run_snapshots (org_id, cell_id, run_id, version, status, execution_status, attempt_id, previous_version, transition, reason)
    SELECT released.org_id,
           released.cell_id,
           released.id,
           released.state_version,
           released.status,
           released.execution_status,
           released.current_attempt_id,
           released.state_version - 1,
           'run.retry_scheduled',
           jsonb_build_object(
               'reason', retry_plan.reason,
               'previous_attempt_id', retry_plan.previous_attempt_id,
               'previous_attempt_number', retry_plan.previous_attempt_number,
               'next_attempt_id', retry_plan.next_attempt_id,
               'next_attempt_number', retry_plan.next_attempt_number,
               'retry_after', retry_plan.retry_after,
               'delay_ms', retry_plan.delay_ms
           )
      FROM released
      JOIN retry_plan ON true
      JOIN completed_queue_entry ON true
     WHERE retry_plan.run_id = released.id
       AND completed_queue_entry.run_id = released.id
    RETURNING run_snapshots.run_id
),
event_inputs AS (
    SELECT 1 AS event_ordinal,
           released.org_id,
           released.cell_id,
           released.project_id,
           released.environment_id,
           released.id AS run_id,
           released_run_lease.attempt_id,
           released_run_lease.id AS run_lease_id,
           released_attempt.attempt_number,
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
      JOIN released_attempt ON true
      JOIN released_snapshot ON true
      JOIN effective_release ON true
     WHERE effective_release.run_id = released.id
    UNION ALL
    SELECT 2 AS event_ordinal,
           released.org_id,
           released.cell_id,
           released.project_id,
           released.environment_id,
           released.id AS run_id,
           released.current_attempt_id,
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
               'previous_attempt_id', retry_plan.previous_attempt_id,
               'previous_attempt_number', retry_plan.previous_attempt_number,
               'next_attempt_id', retry_plan.next_attempt_id,
               'next_attempt_number', retry_plan.next_attempt_number,
               'retry_after', retry_plan.retry_after,
               'delay_ms', retry_plan.delay_ms
           ),
           'internal',
           released.state_version
      FROM released
      JOIN retry_plan ON true
      JOIN retry_snapshot ON true
     WHERE retry_plan.run_id = released.id
),
event_subject_counts AS (
    SELECT org_id, cell_id, run_id, count(*)::bigint AS event_count
      FROM event_inputs
     GROUP BY org_id, cell_id, run_id
),
event_seq AS (
    INSERT INTO event_cursors (org_id, cell_id, subject_kind, subject_id, seq)
    SELECT org_id, cell_id, 'run', run_id, event_count
      FROM event_subject_counts
    ON CONFLICT (org_id, cell_id, subject_kind, subject_id)
    DO UPDATE SET seq = event_cursors.seq + EXCLUDED.seq,
                  observed_at = now()
    RETURNING org_id, subject_kind, subject_id, seq
),
events AS (
    INSERT INTO event_hot_payloads (org_id, cell_id, project_id, environment_id, run_id, seq, attempt_id, run_lease_id, attempt_number, trace_id, span_id, parent_span_id, traceparent, category, severity, source, kind, message, payload, redaction_class, snapshot_version)
    SELECT event_inputs.org_id,
           event_inputs.cell_id,
           event_inputs.project_id,
           event_inputs.environment_id,
           event_inputs.run_id,
           event_seq.seq - event_subject_counts.event_count + row_number() OVER (PARTITION BY event_inputs.org_id, event_inputs.run_id ORDER BY event_inputs.event_ordinal),
           event_inputs.attempt_id,
           event_inputs.run_lease_id,
           event_inputs.attempt_number,
           event_inputs.trace_id,
           event_inputs.span_id,
           event_inputs.parent_span_id,
           event_inputs.traceparent,
           event_inputs.category,
           event_inputs.severity,
           event_inputs.source,
           event_inputs.kind,
           event_inputs.message,
           event_inputs.payload,
           event_inputs.redaction_class,
           event_inputs.snapshot_version
      FROM event_inputs
      JOIN event_subject_counts ON event_subject_counts.org_id = event_inputs.org_id
                               AND event_subject_counts.run_id = event_inputs.run_id
      JOIN event_seq ON event_seq.org_id = event_inputs.org_id
                    AND event_seq.subject_kind = 'run'
                    AND event_seq.subject_id = event_inputs.run_id
    RETURNING *
),
telemetry_outbox AS (
    INSERT INTO telemetry_outbox (org_id, cell_id, stream_kind, source_kind, source_id, seq, idempotency_key)
    SELECT events.org_id,
                  events.cell_id,
                  'event',
                  events.subject_type,
                  events.subject_id,
                  events.seq,
                  'event:' || events.subject_type::text || ':' || events.subject_id::text || ':' || events.seq::text
      FROM events
    RETURNING id
),
cleanup AS (
    SELECT
        (SELECT count(*) FROM cancelled_run_waits) AS cancelled_run_waits,
        (SELECT count(*) FROM invalidated_runtime_checkpoints) AS invalidated_runtime_checkpoints,
        (SELECT count(*) FROM released_concurrency_slot) AS released_concurrency_slots,
        (SELECT count(*) FROM completed_restore_runtime_checkpoint) AS completed_restore_runtime_checkpoints,
        (SELECT count(*) FROM completed_runtime_checkpoint_restore) AS completed_runtime_checkpoint_restores,
        (SELECT count(*) FROM failed_runtime_checkpoint_restore) AS failed_runtime_checkpoint_restores,
        (SELECT count(*) FROM events WHERE kind <> 'run.retry_scheduled') AS terminal_events,
        (SELECT count(*) FROM retry_decision) AS retry_decisions,
        (SELECT count(*) FROM events WHERE kind = 'run.retry_scheduled') AS retry_events,
        (SELECT count(*) FROM telemetry_outbox) AS telemetry_outboxes,
        (SELECT count(*) FROM active_time_usage_event) AS active_time_usage_events,
        (SELECT count(*) FROM output_usage_event) AS output_usage_events,
        (SELECT count(*) FROM published_workspace_version) AS workspace_versions,
        (SELECT count(*) FROM advanced_workspace) AS advanced_workspaces,
        (SELECT count(*) FROM released_workspace_lease) AS released_workspace_leases,
        (SELECT count(*) FROM waiting_runtime_instance) AS waiting_runtime_instances
),
idempotent_released AS (
    SELECT runs.*
      FROM runs
      JOIN run_leases
        ON run_leases.org_id = runs.org_id
       AND run_leases.run_id = runs.id
       AND run_leases.id = sqlc.arg(run_lease_id)
       AND run_leases.worker_instance_id = sqlc.arg(worker_instance_id)
       AND run_leases.dispatch_message_id = sqlc.arg(dispatch_message_id)
       AND run_leases.dispatch_lease_id = sqlc.arg(dispatch_lease_id)
       AND run_leases.status = 'released'
     WHERE runs.org_id = sqlc.arg(org_id)
       AND runs.id = sqlc.arg(run_id)
       AND (
           (
               runs.status = sqlc.arg(run_status)::run_status
               AND runs.exit_code IS NOT DISTINCT FROM sqlc.narg(exit_code)::int
               AND runs.error_message IS NOT DISTINCT FROM sqlc.narg(error_message)
               AND runs.output IS NOT DISTINCT FROM sqlc.arg(output)::jsonb
           )
           OR (
               runs.status = 'queued'
               AND runs.execution_status = 'queued'
               AND EXISTS (
                   SELECT 1
                     FROM run_retry_decisions
                    WHERE run_retry_decisions.org_id = sqlc.arg(org_id)
                      AND run_retry_decisions.run_id = sqlc.arg(run_id)
                      AND run_retry_decisions.run_lease_id = sqlc.arg(run_lease_id)
                      AND run_retry_decisions.decision = 'retry'
               )
           )
           OR (
               runs.status = 'cancelled'
               AND runs.execution_status = 'finished'
               AND EXISTS (
                   SELECT 1
                     FROM run_snapshots
                    WHERE run_snapshots.org_id = sqlc.arg(org_id)
                      AND run_snapshots.run_id = sqlc.arg(run_id)
                      AND run_snapshots.run_lease_id = sqlc.arg(run_lease_id)
                      AND run_snapshots.status = 'cancelled'
                      AND run_snapshots.transition = 'run.cancelled'
               )
           )
       )
       AND runs.current_run_lease_id IS NULL
       AND NOT EXISTS (SELECT 1 FROM released)
)
SELECT released.*
 FROM released
  JOIN released_run_lease ON true
  JOIN completed_queue_entry ON true
  JOIN released_snapshot ON true
 WHERE (SELECT terminal_events FROM cleanup) > 0
   AND (SELECT cancelled_run_waits + invalidated_runtime_checkpoints + released_concurrency_slots + completed_restore_runtime_checkpoints + completed_runtime_checkpoint_restores + failed_runtime_checkpoint_restores + terminal_events + retry_decisions + retry_events + telemetry_outboxes + active_time_usage_events + output_usage_events + workspace_versions + advanced_workspaces + released_workspace_leases + waiting_runtime_instances FROM cleanup) >= 0
UNION ALL
SELECT *
  FROM idempotent_released;
