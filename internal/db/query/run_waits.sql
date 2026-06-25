-- name: CreateRunWait :one
INSERT INTO run_waits (
    id,
    org_id,
    project_id,
    environment_id,
    run_id,
    kind,
    correlation_id,
    timeout_at,
    workspace_version_id,
    active_elapsed_ms_at_park
)
SELECT sqlc.arg(id),
       runs.org_id,
       runs.project_id,
       runs.environment_id,
       runs.id,
       sqlc.arg(kind)::run_wait_kind,
       COALESCE(sqlc.arg(correlation_id)::text, ''),
       sqlc.narg(timeout_at)::timestamptz,
       sqlc.narg(workspace_version_id)::uuid,
       sqlc.narg(active_elapsed_ms_at_park)::bigint
  FROM runs
 WHERE runs.org_id = sqlc.arg(org_id)
   AND runs.project_id = sqlc.arg(project_id)
   AND runs.environment_id = sqlc.arg(environment_id)
   AND runs.id = sqlc.arg(run_id)
RETURNING *;

-- name: GetRunWait :one
SELECT *
  FROM run_waits
 WHERE org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND id = sqlc.arg(id);

-- name: ListRunWaits :many
WITH cursor_wait AS (
    SELECT parked_at, id
      FROM run_waits
     WHERE org_id = sqlc.arg(org_id)
       AND run_id = sqlc.arg(run_id)
       AND id = sqlc.narg(after_id)::uuid
)
SELECT *
  FROM run_waits
 WHERE run_waits.org_id = sqlc.arg(org_id)
   AND run_waits.run_id = sqlc.arg(run_id)
   AND (
       sqlc.narg(state)::text IS NULL
       OR run_waits.state = sqlc.narg(state)::run_wait_state
   )
   AND (
       sqlc.narg(after_id)::uuid IS NULL
       OR (run_waits.parked_at, run_waits.id) > (SELECT cursor_wait.parked_at, cursor_wait.id FROM cursor_wait)
   )
 ORDER BY run_waits.parked_at ASC, run_waits.id ASC
 LIMIT sqlc.arg(limit_count);

-- name: ResolveRunWait :one
UPDATE run_waits
   SET state = 'resolved',
       resolved_at = COALESCE(run_waits.resolved_at, now()),
       updated_at = now()
 WHERE org_id = sqlc.arg(org_id)
   AND id = sqlc.arg(id)
   AND state = 'waiting'
RETURNING *;

-- name: MarkRunWaitResumed :one
UPDATE run_waits
   SET resumed_at = COALESCE(run_waits.resumed_at, now()),
       state = 'resumed',
       updated_at = now()
  FROM runs
 WHERE run_waits.org_id = sqlc.arg(org_id)
   AND run_waits.id = sqlc.arg(id)
   AND run_waits.run_id = sqlc.arg(run_id)
   AND run_waits.runtime_checkpoint_id = sqlc.arg(runtime_checkpoint_id)
   AND run_waits.state = 'resuming'
   AND runs.org_id = run_waits.org_id
   AND runs.id = run_waits.run_id
   AND runs.current_run_lease_id = sqlc.arg(run_lease_id)
   AND runs.status = 'running'
   AND runs.execution_status = 'executing'
RETURNING *;

-- name: RequeueResolvedRunWaits :many
WITH eligible_waits AS (
    SELECT run_waits.*,
           runs.current_attempt_id,
           runs.queued_expires_at,
           runs.workspace_id,
           runs.priority
      FROM run_waits
      JOIN runs ON runs.org_id = run_waits.org_id
               AND runs.project_id = run_waits.project_id
               AND runs.environment_id = run_waits.environment_id
               AND runs.id = run_waits.run_id
      JOIN runtime_checkpoints ON runtime_checkpoints.org_id = run_waits.org_id
                              AND runtime_checkpoints.project_id = run_waits.project_id
                              AND runtime_checkpoints.environment_id = run_waits.environment_id
                              AND runtime_checkpoints.run_id = run_waits.run_id
                              AND runtime_checkpoints.id = run_waits.runtime_checkpoint_id
      JOIN workspaces ON workspaces.org_id = runs.org_id
                     AND workspaces.project_id = runs.project_id
                     AND workspaces.environment_id = runs.environment_id
                     AND workspaces.id = runs.workspace_id
                     AND workspaces.current_version_id = runtime_checkpoints.base_workspace_version_id
     WHERE run_waits.org_id = sqlc.arg(org_id)
       AND run_waits.state IN ('resolved', 'expired')
       AND run_waits.runtime_checkpoint_id IS NOT NULL
       AND runs.status = 'waiting'
       AND runs.current_run_lease_id IS NULL
       AND runtime_checkpoints.state = 'ready'
       AND (runtime_checkpoints.expires_at IS NULL OR runtime_checkpoints.expires_at > now())
     ORDER BY COALESCE(run_waits.resolved_at, run_waits.timeout_at, run_waits.updated_at), run_waits.id
     LIMIT sqlc.arg(limit_count)
     FOR UPDATE OF run_waits, runs
),
updated_waits AS (
    UPDATE run_waits
       SET state = 'resuming',
           updated_at = now()
      FROM eligible_waits
     WHERE run_waits.org_id = eligible_waits.org_id
       AND run_waits.id = eligible_waits.id
       AND run_waits.state IN ('resolved', 'expired')
    RETURNING run_waits.*
),
updated_runs AS (
    UPDATE runs
       SET status = 'queued',
           execution_status = 'queued',
           state_version = runs.state_version + 1,
           updated_at = now()
      FROM eligible_waits
      JOIN updated_waits ON updated_waits.org_id = eligible_waits.org_id
                        AND updated_waits.id = eligible_waits.id
     WHERE runs.org_id = eligible_waits.org_id
       AND runs.id = eligible_waits.run_id
       AND runs.status = 'waiting'
       AND runs.current_run_lease_id IS NULL
    RETURNING runs.id, runs.org_id, runs.current_attempt_id, runs.queued_expires_at
),
updated_attempts AS (
    UPDATE run_attempts
       SET status = 'queued',
           updated_at = now()
      FROM updated_runs
     WHERE run_attempts.org_id = updated_runs.org_id
       AND run_attempts.run_id = updated_runs.id
       AND run_attempts.id = updated_runs.current_attempt_id
       AND run_attempts.status = 'waiting'
    RETURNING run_attempts.run_id
),
updated_queue AS (
    UPDATE run_queue_items
       SET status = 'queued',
           dispatch_message_id = NULL,
           reserved_by_worker_instance_id = NULL,
           reservation_expires_at = NULL,
           queued_expires_at = updated_runs.queued_expires_at,
           dispatch_generation = run_queue_items.dispatch_generation + 1,
           last_error = '',
           enqueued_at = now(),
           updated_at = now(),
           finished_at = NULL
      FROM updated_runs
     WHERE run_queue_items.org_id = updated_runs.org_id
       AND run_queue_items.run_id = updated_runs.id
       AND run_queue_items.status = 'parked'
    RETURNING run_queue_items.run_id
)
SELECT updated_waits.*,
       eligible_waits.workspace_id,
       eligible_waits.priority
  FROM updated_waits
  JOIN eligible_waits ON eligible_waits.org_id = updated_waits.org_id
                     AND eligible_waits.id = updated_waits.id
  JOIN updated_runs ON updated_runs.org_id = updated_waits.org_id
                   AND updated_runs.id = updated_waits.run_id
  JOIN updated_attempts ON updated_attempts.run_id = updated_waits.run_id
  JOIN updated_queue ON updated_queue.run_id = updated_waits.run_id;

-- name: FailStaleResolvedRunWaits :many
WITH stale_waits AS MATERIALIZED (
    SELECT run_waits.id AS run_wait_id,
           run_waits.org_id,
           run_waits.project_id,
           run_waits.environment_id,
           run_waits.run_id,
           runs.task_session_id,
           runs.current_attempt_id,
           runs.current_attempt_number,
           runs.trace_id,
           runs.root_span_id,
           runs.state_version + 1 AS next_state_version,
           runtime_checkpoints.id AS runtime_checkpoint_id,
           runtime_checkpoints.base_workspace_version_id,
           runtime_checkpoints.expires_at AS runtime_checkpoint_expires_at,
           workspaces.current_version_id,
           run_waits.state AS run_wait_state,
           runs.status AS run_status,
           CASE
             WHEN runtime_checkpoints.expires_at <= now()
             THEN 'runtime_checkpoint_expired'
             ELSE 'workspace_version_mismatch'
           END AS failure_reason,
           CASE
             WHEN runtime_checkpoints.expires_at <= now()
             THEN 'runtime checkpoint expired while run was parked'
             ELSE 'workspace advanced while run was parked'
           END AS failure_message
      FROM run_waits
      JOIN runs ON runs.org_id = run_waits.org_id
               AND runs.project_id = run_waits.project_id
               AND runs.environment_id = run_waits.environment_id
               AND runs.id = run_waits.run_id
      JOIN task_sessions ON task_sessions.org_id = runs.org_id
                        AND task_sessions.project_id = runs.project_id
                        AND task_sessions.environment_id = runs.environment_id
                        AND task_sessions.id = runs.task_session_id
      JOIN runtime_checkpoints ON runtime_checkpoints.org_id = run_waits.org_id
                              AND runtime_checkpoints.project_id = run_waits.project_id
                              AND runtime_checkpoints.environment_id = run_waits.environment_id
                              AND runtime_checkpoints.run_id = run_waits.run_id
                              AND runtime_checkpoints.id = run_waits.runtime_checkpoint_id
      JOIN workspaces ON workspaces.org_id = runs.org_id
                     AND workspaces.project_id = runs.project_id
                     AND workspaces.environment_id = runs.environment_id
                     AND workspaces.id = runs.workspace_id
     WHERE run_waits.org_id = sqlc.arg(org_id)
       AND (
           (run_waits.state IN ('resolved', 'expired') AND runs.status = 'waiting')
           OR (run_waits.state = 'resuming' AND runs.status = 'queued')
       )
       AND run_waits.runtime_checkpoint_id IS NOT NULL
       AND runs.current_run_lease_id IS NULL
       AND runtime_checkpoints.state = 'ready'
       AND runs.latest_runtime_checkpoint_id = runtime_checkpoints.id
       AND (
           workspaces.current_version_id IS DISTINCT FROM runtime_checkpoints.base_workspace_version_id
           OR runtime_checkpoints.expires_at <= now()
       )
     ORDER BY COALESCE(run_waits.resolved_at, run_waits.timeout_at, run_waits.updated_at), run_waits.id
     LIMIT sqlc.arg(limit_count)
     FOR UPDATE OF run_waits, runs, task_sessions
),
failed_waits AS (
    UPDATE run_waits
       SET state = 'failed',
           updated_at = now()
      FROM stale_waits
     WHERE run_waits.org_id = stale_waits.org_id
       AND run_waits.id = stale_waits.run_wait_id
       AND run_waits.state = stale_waits.run_wait_state
    RETURNING run_waits.*
),
failed_runs AS (
    UPDATE runs
       SET status = 'failed',
           execution_status = 'finished',
           terminal_outcome = 'failed',
           error_message = stale_waits.failure_message,
           state_version = stale_waits.next_state_version,
           finished_at = now(),
           updated_at = now()
      FROM stale_waits
      JOIN failed_waits ON failed_waits.org_id = stale_waits.org_id
                       AND failed_waits.id = stale_waits.run_wait_id
     WHERE runs.org_id = stale_waits.org_id
       AND runs.id = stale_waits.run_id
       AND runs.status = stale_waits.run_status
       AND runs.current_run_lease_id IS NULL
    RETURNING runs.id, runs.org_id, runs.project_id, runs.environment_id, runs.task_session_id,
              runs.current_attempt_id, runs.current_attempt_number, runs.trace_id, runs.root_span_id,
              runs.state_version, runs.error_message, stale_waits.runtime_checkpoint_id,
              stale_waits.base_workspace_version_id, stale_waits.current_version_id,
              stale_waits.runtime_checkpoint_expires_at, stale_waits.failure_reason
),
invalidated_checkpoints AS (
    UPDATE runtime_checkpoints
       SET state = 'invalid',
           error_message = failed_runs.error_message,
           invalidated_at = now()
      FROM failed_runs
     WHERE runtime_checkpoints.org_id = failed_runs.org_id
       AND runtime_checkpoints.project_id = failed_runs.project_id
       AND runtime_checkpoints.environment_id = failed_runs.environment_id
       AND runtime_checkpoints.run_id = failed_runs.id
       AND runtime_checkpoints.id = failed_runs.runtime_checkpoint_id
       AND runtime_checkpoints.state = 'ready'
    RETURNING runtime_checkpoints.id
),
ended_session_runs AS (
    UPDATE task_session_runs
       SET ended_at = now()
      FROM failed_runs
     WHERE task_session_runs.org_id = failed_runs.org_id
       AND task_session_runs.project_id = failed_runs.project_id
       AND task_session_runs.environment_id = failed_runs.environment_id
       AND task_session_runs.task_session_id = failed_runs.task_session_id
       AND task_session_runs.run_id = failed_runs.id
    RETURNING task_session_runs.id
),
failed_task_sessions AS (
    SELECT failed_runs.task_session_id AS id
      FROM failed_runs
),
failed_attempts AS (
    UPDATE run_attempts
       SET status = 'failed',
           error_message = failed_runs.error_message,
           finished_at = now(),
           updated_at = now()
      FROM failed_runs
     WHERE run_attempts.org_id = failed_runs.org_id
       AND run_attempts.run_id = failed_runs.id
       AND run_attempts.id = failed_runs.current_attempt_id
    RETURNING run_attempts.id, run_attempts.run_id
),
completed_queue_entries AS (
    UPDATE run_queue_items
       SET status = 'completed',
           dispatch_generation = dispatch_generation + 1,
           updated_at = now(),
           finished_at = now()
     FROM failed_runs
     WHERE run_queue_items.org_id = failed_runs.org_id
       AND run_queue_items.run_id = failed_runs.id
       AND run_queue_items.status IN ('parked', 'queued', 'published', 'reserved')
    RETURNING run_queue_items.run_id
),
failed_snapshots AS (
    INSERT INTO run_snapshots (org_id, run_id, version, status, execution_status, terminal_outcome, attempt_id, transition, reason)
    SELECT failed_runs.org_id,
           failed_runs.id,
           failed_runs.state_version,
           'failed',
           'finished',
           'failed',
           failed_runs.current_attempt_id,
           'run.failed',
           jsonb_build_object(
               'origin', 'run_wait_resume',
               'reason', failed_runs.failure_reason,
               'message', failed_runs.error_message,
               'runtime_checkpoint_id', failed_runs.runtime_checkpoint_id,
               'base_workspace_version_id', failed_runs.base_workspace_version_id,
               'current_workspace_version_id', failed_runs.current_version_id,
               'runtime_checkpoint_expires_at', failed_runs.runtime_checkpoint_expires_at
           )
      FROM failed_runs
      JOIN failed_attempts ON failed_attempts.run_id = failed_runs.id
    RETURNING run_snapshots.run_id
),
failed_event_seq AS (
    INSERT INTO event_subject_cursors (org_id, subject_type, subject_id, last_seq)
    SELECT failed_runs.org_id, 'run', failed_runs.id, 1
      FROM failed_runs
      JOIN failed_snapshots ON failed_snapshots.run_id = failed_runs.id
    ON CONFLICT (org_id, subject_type, subject_id)
    DO UPDATE SET last_seq = event_subject_cursors.last_seq + 1,
                  updated_at = now()
    RETURNING org_id, subject_type, subject_id, last_seq
),
failed_events AS (
    INSERT INTO events (org_id, project_id, environment_id, run_id, seq, attempt_id, attempt_number, trace_id, span_id, traceparent, category, severity, source, kind, message, payload, redaction_class, snapshot_version)
    SELECT failed_runs.org_id,
           failed_runs.project_id,
           failed_runs.environment_id,
           failed_runs.id,
           failed_event_seq.last_seq,
           failed_runs.current_attempt_id,
           failed_runs.current_attempt_number,
           failed_runs.trace_id,
           failed_runs.root_span_id,
           '00-' || failed_runs.trace_id || '-' || failed_runs.root_span_id || '-01',
           'lifecycle',
           'error',
           'control',
           'run.failed',
           'run.failed',
           jsonb_build_object(
               'origin', 'run_wait_resume',
               'reason', failed_runs.failure_reason,
               'message', failed_runs.error_message,
               'runtime_checkpoint_id', failed_runs.runtime_checkpoint_id,
               'base_workspace_version_id', failed_runs.base_workspace_version_id,
               'current_workspace_version_id', failed_runs.current_version_id,
               'runtime_checkpoint_expires_at', failed_runs.runtime_checkpoint_expires_at
           ),
           'internal',
           failed_runs.state_version
      FROM failed_runs
      JOIN failed_snapshots ON failed_snapshots.run_id = failed_runs.id
      JOIN failed_event_seq ON failed_event_seq.org_id = failed_runs.org_id
                           AND failed_event_seq.subject_type = 'run'
                           AND failed_event_seq.subject_id = failed_runs.id
    RETURNING *
),
failed_event_outbox AS (
    INSERT INTO event_outbox (event_record_id, stream_key)
    SELECT failed_events.id,
           'helmr:events:' || failed_events.org_id::text || ':' || failed_events.subject_type::text || ':' || failed_events.subject_id::text
      FROM failed_events
    RETURNING id
),
cleanup AS (
    SELECT
        (SELECT count(*) FROM invalidated_checkpoints) AS invalidated_checkpoints,
        (SELECT count(*) FROM failed_event_outbox) AS failed_event_outboxes
)
SELECT failed_waits.*
  FROM failed_waits
  JOIN failed_runs ON failed_runs.org_id = failed_waits.org_id
                  AND failed_runs.id = failed_waits.run_id
 WHERE (SELECT invalidated_checkpoints + failed_event_outboxes FROM cleanup) >= 0;

-- name: SetRunWaitWorkspaceVersion :one
UPDATE run_waits
   SET workspace_version_id = workspace_versions.id,
       updated_at = now()
  FROM runs
  JOIN workspace_versions
    ON workspace_versions.org_id = runs.org_id
   AND workspace_versions.project_id = runs.project_id
   AND workspace_versions.environment_id = runs.environment_id
   AND workspace_versions.workspace_id = runs.workspace_id
   AND workspace_versions.id = sqlc.arg(workspace_version_id)
   AND workspace_versions.state = 'ready'
 WHERE run_waits.org_id = sqlc.arg(org_id)
   AND run_waits.project_id = sqlc.arg(project_id)
   AND run_waits.environment_id = sqlc.arg(environment_id)
   AND run_waits.id = sqlc.arg(id)
   AND run_waits.run_id = sqlc.arg(run_id)
   AND run_waits.state = 'parking'
   AND run_waits.workspace_version_id IS NULL
   AND runs.org_id = run_waits.org_id
   AND runs.project_id = run_waits.project_id
   AND runs.environment_id = run_waits.environment_id
   AND runs.id = run_waits.run_id
RETURNING run_waits.*;

-- name: CancelRunWait :one
UPDATE run_waits
   SET state = 'cancelled',
       cancelled_at = COALESCE(run_waits.cancelled_at, now()),
       updated_at = now()
 WHERE org_id = sqlc.arg(org_id)
   AND id = sqlc.arg(id)
   AND state = 'waiting'
RETURNING *;

-- name: CancelRunWaitsForRun :many
UPDATE run_waits
   SET state = 'cancelled',
       cancelled_at = COALESCE(run_waits.cancelled_at, now()),
       updated_at = now()
 WHERE org_id = sqlc.arg(org_id)
   AND run_id = sqlc.arg(run_id)
   AND state = 'waiting'
RETURNING *;

-- name: ExpireDueRunWaits :many
UPDATE run_waits
   SET state = 'expired',
       resolved_at = COALESCE(run_waits.resolved_at, now()),
       updated_at = now()
 WHERE org_id = sqlc.arg(org_id)
   AND state = 'waiting'
   AND timeout_at IS NOT NULL
   AND timeout_at <= now()
RETURNING *;

-- name: GetWorkerRunWaitScope :one
SELECT runs.org_id,
       runs.project_id,
       runs.environment_id,
       runs.id AS run_id,
       runs.task_session_id,
       runs.workspace_id,
       runs.current_run_lease_id,
       run_leases.worker_instance_id,
       workspace_leases.id AS workspace_lease_id,
       workspace_leases.fencing_token AS workspace_fencing_token,
       workspace_leases.materialization_id,
       workspace_leases.base_version_id AS workspace_base_version_id,
       workspaces.current_version_id AS workspace_current_version_id,
       workspace_materializations.dirty_generation,
       worker_instances.cni_profile AS worker_cni_profile
  FROM runs
  JOIN run_leases ON run_leases.org_id = runs.org_id
                 AND run_leases.run_id = runs.id
                 AND run_leases.id = runs.current_run_lease_id
  JOIN worker_instances ON worker_instances.id = run_leases.worker_instance_id
                       AND worker_instances.worker_group_id = run_leases.worker_group_id
  JOIN workspaces ON workspaces.org_id = runs.org_id
                 AND workspaces.project_id = runs.project_id
                 AND workspaces.environment_id = runs.environment_id
                 AND workspaces.id = runs.workspace_id
  JOIN workspace_leases ON workspace_leases.org_id = runs.org_id
                       AND workspace_leases.project_id = runs.project_id
                       AND workspace_leases.environment_id = runs.environment_id
                       AND workspace_leases.workspace_id = runs.workspace_id
                       AND workspace_leases.owner_run_id = runs.id
                       AND workspace_leases.lease_kind = 'write'
                       AND workspace_leases.state = 'active'
                       AND workspace_leases.released_at IS NULL
                       AND workspace_leases.expires_at > now()
  JOIN workspace_materializations ON workspace_materializations.org_id = workspace_leases.org_id
                                 AND workspace_materializations.project_id = workspace_leases.project_id
                                 AND workspace_materializations.environment_id = workspace_leases.environment_id
                                 AND workspace_materializations.workspace_id = workspace_leases.workspace_id
                                 AND workspace_materializations.id = workspace_leases.materialization_id
 WHERE runs.org_id = sqlc.arg(org_id)
   AND runs.id = sqlc.arg(run_id)
   AND runs.current_run_lease_id = sqlc.arg(run_lease_id)
   AND runs.status = 'running'
   AND run_leases.worker_instance_id = sqlc.arg(worker_instance_id)
   AND run_leases.status IN ('leased', 'running')
   AND run_leases.lease_expires_at > now();
