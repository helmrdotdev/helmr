-- name: FindCancellationTarget :one
SELECT id
  FROM runs
 WHERE org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND id = sqlc.arg(id);

-- name: ListCancellationLineage :many
WITH RECURSIVE lineage AS (
    SELECT runs.id,
           runs.parent_run_id,
           runs.parent_owns_lifecycle,
           0 AS depth,
           ARRAY[runs.id] AS path,
           false AS cycle,
           sqlc.arg(max_depth)::integer AS max_depth
      FROM runs
     WHERE runs.id = sqlc.arg(target_id)
    UNION ALL
    SELECT parent.id,
           parent.parent_run_id,
           parent.parent_owns_lifecycle,
           lineage.depth + 1,
           lineage.path || parent.id,
           parent.id = ANY(lineage.path),
           lineage.max_depth
      FROM lineage
      JOIN runs AS parent
        ON parent.id = lineage.parent_run_id
     WHERE lineage.parent_owns_lifecycle IS TRUE
       AND NOT lineage.cycle
       AND lineage.depth < lineage.max_depth
)
SELECT id, depth, cycle
  FROM lineage
 ORDER BY depth DESC;

-- name: ListOwnedCancellationRuns :many
WITH RECURSIVE owned AS (
    SELECT runs.id,
           0 AS depth,
           ARRAY[runs.id] AS path,
           false AS cycle,
           sqlc.arg(max_depth)::integer AS max_depth
      FROM runs
     WHERE runs.id = sqlc.arg(target_id)
       AND runs.org_id = sqlc.arg(org_id)
       AND runs.project_id = sqlc.arg(project_id)
       AND runs.environment_id = sqlc.arg(environment_id)
    UNION ALL
    SELECT child.id,
           owned.depth + 1,
           owned.path || child.id,
           child.id = ANY(owned.path),
           owned.max_depth
      FROM owned
      JOIN runs AS child
        ON child.parent_run_id = owned.id
       AND child.parent_owns_lifecycle IS TRUE
       AND child.org_id = sqlc.arg(org_id)
       AND child.project_id = sqlc.arg(project_id)
       AND child.environment_id = sqlc.arg(environment_id)
       AND child.status IN ('queued', 'running', 'waiting', 'retry_delayed', 'cancel_requested')
     WHERE NOT owned.cycle
       AND owned.depth < owned.max_depth
)
SELECT id, depth, cycle
  FROM owned
 ORDER BY depth, id
 LIMIT sqlc.arg(limit_count);

-- name: LockCancellationActors :many
SELECT sessions.id
  FROM runs
  JOIN sessions
    ON sessions.id = runs.session_id
   AND sessions.environment_id = runs.environment_id
 WHERE runs.id = ANY(sqlc.arg(run_ids)::uuid[])
   AND runs.org_id = sqlc.arg(org_id)
   AND runs.project_id = sqlc.arg(project_id)
   AND runs.environment_id = sqlc.arg(environment_id)
 ORDER BY sessions.id
 FOR UPDATE OF sessions;

-- name: LockCancellationRun :one
SELECT id,
       parent_run_id,
       parent_owns_lifecycle,
       environment_id,
       workspace_id,
       session_id,
       status,
       current_attempt_number,
       current_run_lease_id,
       state_version
  FROM runs
 WHERE id = sqlc.arg(id)
   AND org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
 FOR UPDATE;

-- name: LockCancellationWorkspaces :many
SELECT id
  FROM workspaces
 WHERE id IN (
       SELECT workspace_id
         FROM runs
        WHERE id = ANY(sqlc.arg(run_ids)::uuid[])
 )
 ORDER BY id
 FOR UPDATE;

-- name: LockCancellationAttempts :many
SELECT run_attempts.run_id
  FROM run_attempts
  JOIN runs
    ON runs.id = run_attempts.run_id
   AND runs.current_attempt_number = run_attempts.number
   AND runs.workspace_id = run_attempts.workspace_id
 WHERE runs.id = ANY(sqlc.arg(run_ids)::uuid[])
 ORDER BY array_position(sqlc.arg(run_ids)::uuid[], run_attempts.run_id),
          run_attempts.number
 FOR UPDATE OF run_attempts;

-- name: LockCancellationRuntimes :many
WITH target_runtimes AS (
    SELECT run_leases.runtime_instance_id AS id
      FROM runs
      JOIN run_leases
        ON run_leases.id = runs.current_run_lease_id
       AND run_leases.run_id = runs.id
     WHERE runs.id = ANY(sqlc.arg(cancel_ids)::uuid[])
    UNION
    SELECT runtime_instances.id
      FROM runtime_instances
     WHERE runtime_instances.reserved_run_id = ANY(sqlc.arg(cancel_ids)::uuid[])
    UNION
    SELECT run_waits.handoff_runtime_instance_id
      FROM run_waits
     WHERE run_waits.run_id = ANY(sqlc.arg(run_ids)::uuid[])
       AND run_waits.handoff_runtime_instance_id IS NOT NULL
       AND run_waits.suspension_state IN (
           'hot', 'checkpointing', 'parked', 'resume_pending', 'resuming'
       )
)
SELECT runtime_instances.id
  FROM runtime_instances
  JOIN target_runtimes ON target_runtimes.id = runtime_instances.id
 ORDER BY runtime_instances.id
 FOR UPDATE OF runtime_instances;

-- name: LockCancellationRunLeases :many
SELECT run_leases.id
  FROM runs
  JOIN run_leases
    ON run_leases.id = runs.current_run_lease_id
   AND run_leases.run_id = runs.id
 WHERE runs.id = ANY(sqlc.arg(run_ids)::uuid[])
 ORDER BY run_leases.id
 FOR UPDATE OF run_leases;

-- name: LockCancellationMounts :many
SELECT id
  FROM workspace_mounts
 WHERE runtime_instance_id = ANY(sqlc.arg(runtime_ids)::uuid[])
   AND state IN ('mounting', 'mounted', 'unmounting')
 ORDER BY id
 FOR UPDATE;

-- name: LockCancellationWorkspaceLeases :many
SELECT id
  FROM workspace_leases
 WHERE owner_run_lease_id = ANY(sqlc.arg(run_lease_ids)::uuid[])
   AND state IN ('active', 'releasing')
 ORDER BY id
 FOR UPDATE;

-- name: LockCancellationWaits :many
SELECT id,
       run_id,
       workspace_id,
       child_run_id,
       condition_state,
       suspension_state,
       expected_run_state_version,
       attempt_number,
       current_run_lease_id,
       prior_run_lease_id,
       suspend_checkpoint_id,
       handoff_runtime_instance_id,
       handoff_workspace_mount_id,
       base_workspace_version_id
  FROM run_waits
 WHERE (
       run_id = ANY(sqlc.arg(run_ids)::uuid[])
       OR child_run_id = ANY(sqlc.arg(cancel_ids)::uuid[])
 )
   AND suspension_state IN (
       'hot', 'checkpointing', 'parked', 'resume_pending', 'resuming'
   )
 ORDER BY array_position(sqlc.arg(run_ids)::uuid[], run_id), id
 FOR UPDATE;

-- name: LockCancellationCheckpoints :many
SELECT id
  FROM run_checkpoints
 WHERE run_id = ANY(sqlc.arg(run_ids)::uuid[])
   AND state IN ('creating', 'ready')
 ORDER BY array_position(sqlc.arg(run_ids)::uuid[], run_id), id
 FOR UPDATE;

-- name: ResolveHotTerminalChildWait :one
WITH moved_run AS (
    UPDATE runs
       SET status = 'running',
           state_version = state_version + 1,
           updated_at = transaction_timestamp()
     WHERE runs.id = sqlc.arg(run_id)
       AND runs.status = 'waiting'
       AND runs.state_version = sqlc.arg(expected_run_state_version)
       AND runs.current_attempt_number = sqlc.arg(attempt_number)
       AND runs.current_run_lease_id = sqlc.arg(current_run_lease_id)
    RETURNING runs.state_version
)
UPDATE run_waits
   SET condition_state = sqlc.arg(condition_state)::text,
       condition_result = sqlc.arg(condition_result)::jsonb,
       condition_error = sqlc.arg(condition_error)::jsonb,
       condition_terminal_at = transaction_timestamp(),
       condition_reason_code = sqlc.narg(reason_code)::text,
       suspension_state = 'released',
       expected_run_state_version = moved_run.state_version,
       suspension_terminal_at = transaction_timestamp(),
       updated_at = transaction_timestamp()
  FROM moved_run
 WHERE run_waits.id = sqlc.arg(wait_id)
   AND run_waits.run_id = sqlc.arg(run_id)
   AND run_waits.condition_state = 'pending'
   AND run_waits.suspension_state = 'hot'
RETURNING run_waits.id;

-- name: ResolveCheckpointingTerminalChildWait :one
UPDATE run_waits
   SET condition_state = sqlc.arg(condition_state)::text,
       condition_result = sqlc.arg(condition_result)::jsonb,
       condition_error = sqlc.arg(condition_error)::jsonb,
       condition_terminal_at = transaction_timestamp(),
       condition_reason_code = sqlc.narg(reason_code)::text,
       updated_at = transaction_timestamp()
 WHERE id = sqlc.arg(wait_id)
   AND run_id = sqlc.arg(run_id)
   AND condition_state = 'pending'
   AND suspension_state = 'checkpointing'
RETURNING id;

-- name: ResolveParkedTerminalChildWait :one
WITH moved_run AS (
    UPDATE runs
       SET status = 'queued',
           state_version = state_version + 1,
           updated_at = transaction_timestamp()
     WHERE runs.id = sqlc.arg(run_id)
       AND runs.status = 'waiting'
       AND runs.state_version = sqlc.arg(expected_run_state_version)
       AND runs.current_attempt_number = sqlc.arg(attempt_number)
       AND runs.current_run_lease_id IS NULL
    RETURNING runs.state_version
)
UPDATE run_waits
   SET condition_state = sqlc.arg(condition_state)::text,
       condition_result = sqlc.arg(condition_result)::jsonb,
       condition_error = sqlc.arg(condition_error)::jsonb,
       condition_terminal_at = transaction_timestamp(),
       condition_reason_code = sqlc.narg(reason_code)::text,
       suspension_state = 'resume_pending',
       resume_request_version = run_waits.resume_request_version + 1,
       expected_run_state_version = moved_run.state_version,
       resume_workspace_version_id = COALESCE(
           run_waits.resume_workspace_version_id,
           sqlc.narg(resolved_workspace_version_id)
       ),
       updated_at = transaction_timestamp()
  FROM moved_run
 WHERE run_waits.id = sqlc.arg(wait_id)
   AND run_waits.run_id = sqlc.arg(run_id)
   AND run_waits.condition_state = 'pending'
   AND run_waits.suspension_state = 'parked'
RETURNING run_waits.id,
          run_waits.environment_id,
          run_waits.run_id,
          run_waits.workspace_id,
          run_waits.resume_request_version;

-- name: DetachActorFromCancelledRun :execrows
UPDATE sessions
   SET current_run_id = NULL,
       run_generation = run_generation + 1,
       state_version = state_version + 1,
       manual_run_cancelled = true,
       updated_at = transaction_timestamp()
 WHERE id = sqlc.arg(session_id)
   AND workspace_id = sqlc.arg(workspace_id)
   AND current_run_id = sqlc.arg(run_id)
   AND state IN ('open', 'closing');

-- name: FailActorForRunTermination :execrows
UPDATE sessions
   SET state = 'failed',
       current_run_id = NULL,
       run_generation = run_generation + 1,
       state_version = state_version + 1,
       manual_run_cancelled = false,
       failure = sqlc.arg(failure)::jsonb,
       failure_run_id = sqlc.arg(run_id),
       failed_at = transaction_timestamp(),
       updated_at = transaction_timestamp()
 WHERE id = sqlc.arg(session_id)
   AND workspace_id = sqlc.arg(workspace_id)
   AND current_run_id = sqlc.arg(run_id)
   AND state IN ('open', 'closing');

-- name: TerminalizeRunSuspensions :exec
UPDATE run_waits
   SET condition_state = CASE
           WHEN condition_state = 'pending' THEN sqlc.arg(condition_state)::text
           ELSE condition_state
       END,
       condition_result = CASE
           WHEN condition_state = 'pending' THEN NULL
           ELSE condition_result
       END,
       condition_error = CASE
           WHEN condition_state = 'pending' THEN sqlc.arg(error_payload)::jsonb
           ELSE condition_error
       END,
       condition_terminal_at = CASE
           WHEN condition_state = 'pending' THEN transaction_timestamp()
           ELSE condition_terminal_at
       END,
       condition_reason_code = CASE
           WHEN condition_state = 'pending' THEN sqlc.arg(reason_code)::text
           ELSE condition_reason_code
       END,
       suspension_state = sqlc.arg(suspension_state)::text,
       current_run_lease_id = NULL,
       suspension_terminal_at = transaction_timestamp(),
       suspension_reason_code = sqlc.arg(reason_code)::text,
       suspension_error = sqlc.arg(error_payload)::jsonb,
       updated_at = transaction_timestamp()
 WHERE run_id = sqlc.arg(run_id)
   AND suspension_state IN ('hot', 'checkpointing', 'parked', 'resume_pending', 'resuming');

-- name: InvalidateRunCheckpoints :exec
UPDATE run_checkpoints
   SET state = 'invalid',
       invalidated_at = transaction_timestamp(),
       invalidation_reason_code = sqlc.arg(reason_code)::text
 WHERE run_id = sqlc.arg(run_id)
   AND state IN ('creating', 'ready');

-- name: FenceRunWorkspaceLease :execrows
UPDATE workspace_leases
   SET state = 'fenced',
       terminal_at = transaction_timestamp(),
       terminal_reason_code = sqlc.arg(reason_code)::text,
       terminal_error = sqlc.arg(error_payload)::jsonb,
       updated_at = transaction_timestamp()
 WHERE owner_run_lease_id = sqlc.arg(run_lease_id)
   AND state IN ('active', 'releasing');

-- name: TerminalizeRunLease :execrows
UPDATE run_leases
   SET state = CASE
           WHEN sqlc.arg(state)::text = 'failed' AND started_at IS NULL
           THEN 'rejected'
           ELSE sqlc.arg(state)::text
       END,
       terminal_at = transaction_timestamp(),
       terminal_reason_code = sqlc.arg(reason_code)::text,
       terminal_error = sqlc.arg(error_payload)::jsonb,
       updated_at = transaction_timestamp()
 WHERE id = sqlc.arg(id)
   AND run_id = sqlc.arg(run_id)
   AND state IN ('assigned', 'starting', 'running', 'checkpointing', 'finalizing');

-- name: CloseRunRuntimes :exec
WITH candidate_runtimes AS (
    SELECT run_leases.runtime_instance_id
      FROM run_leases
     WHERE run_leases.id = sqlc.narg(run_lease_id)
       AND run_leases.run_id = sqlc.arg(run_id)
       AND run_leases.runtime_instance_id IS DISTINCT FROM sqlc.narg(retained_runtime_id)
    UNION
    SELECT runtime_instances.id AS runtime_instance_id
      FROM runtime_instances
     WHERE runtime_instances.reserved_run_id = sqlc.arg(run_id)
       AND runtime_instances.id IS DISTINCT FROM sqlc.narg(retained_runtime_id)
    UNION
    SELECT run_waits.handoff_runtime_instance_id AS runtime_instance_id
      FROM run_waits
     WHERE run_waits.run_id = sqlc.arg(run_id)
       AND run_waits.handoff_runtime_instance_id IS NOT NULL
       AND run_waits.handoff_runtime_instance_id IS DISTINCT FROM sqlc.narg(retained_runtime_id)
), closing_runtimes AS (
    UPDATE runtime_instances
       SET desired_state = 'closed',
           desired_version = CASE
               WHEN desired_state = 'closed' THEN desired_version
               ELSE desired_version + 1
           END,
           desired_at = transaction_timestamp(),
           desired_reason = sqlc.arg(reason_code),
           updated_at = transaction_timestamp()
     WHERE runtime_instances.id IN (
           SELECT runtime_instance_id FROM candidate_runtimes
       )
       AND runtime_instances.observed_state IN ('allocated', 'preparing', 'ready', 'closing')
    RETURNING runtime_instances.id
)
UPDATE workspace_mounts
   SET state = 'unmounting',
       stopped_at = COALESCE(stopped_at, transaction_timestamp()),
       updated_at = transaction_timestamp()
 WHERE runtime_instance_id IN (
       SELECT runtime_instance_id FROM candidate_runtimes
       UNION
       SELECT id FROM closing_runtimes
   )
   AND workspace_mounts.id IS DISTINCT FROM sqlc.narg(retained_mount_id)
   AND state IN ('mounting', 'mounted');

-- name: TerminalizeRunAttempt :execrows
UPDATE run_attempts
   SET terminal_outcome = sqlc.arg(outcome)::text,
       terminal_reason_code = sqlc.arg(reason_code)::text,
       terminal_error = sqlc.arg(error_payload)::jsonb,
       terminal_at = transaction_timestamp()
 WHERE run_id = sqlc.arg(run_id)
   AND number = sqlc.arg(attempt_number)
   AND terminal_at IS NULL;

-- name: TerminalizeRun :execrows
UPDATE runs
   SET status = sqlc.arg(status)::text,
       failure = sqlc.arg(failure)::jsonb,
       state_version = state_version + 1,
       current_run_lease_id = NULL,
       retry_at = NULL,
       active_elapsed_ms = LEAST(
           max_active_duration_ms,
           active_elapsed_ms + CASE
               WHEN active_started_at IS NULL THEN 0
               ELSE GREATEST(
                   floor(extract(epoch FROM (
                       transaction_timestamp() - active_started_at
                   )) * 1000)::bigint,
                   0
               )
           END
       ),
       active_started_at = NULL,
       terminal_at = transaction_timestamp(),
       updated_at = transaction_timestamp()
 WHERE id = sqlc.arg(id)
   AND state_version = sqlc.arg(expected_state_version)
   AND status IN ('queued', 'running', 'waiting', 'retry_delayed', 'cancel_requested');

-- name: ReleaseTaskWorkspace :exec
UPDATE workspaces
   SET owner_run_id = NULL,
       ownership_generation = ownership_generation + 1,
       state_version = state_version + 1,
       last_activity_at = transaction_timestamp(),
       updated_at = transaction_timestamp()
 WHERE id = sqlc.arg(workspace_id)
   AND owner_run_id = sqlc.arg(run_id)
   AND owner_session_id IS NULL;

-- name: ReleaseActorWorkspace :exec
UPDATE workspaces
   SET owner_session_id = NULL,
       ownership_generation = ownership_generation + 1,
       state_version = state_version + 1,
       last_activity_at = transaction_timestamp(),
       updated_at = transaction_timestamp()
 WHERE id = sqlc.arg(workspace_id)
   AND owner_session_id = sqlc.arg(session_id)
   AND owner_run_id IS NULL;

-- name: RecordRunTerminalEvent :exec
INSERT INTO telemetry_outbox (
    org_id,
    stream_kind,
    source_kind,
    source_id,
    project_id,
    environment_id,
    run_id,
    run_lease_id,
    attempt_number,
    trace_id,
    span_id,
    category,
    severity,
    source,
    kind,
    message,
    payload,
    redaction_class,
    snapshot_version,
    observed_at
)
SELECT org_id,
       'event',
       'run',
       id,
       project_id,
       environment_id,
       id,
       sqlc.narg(run_lease_id),
       current_attempt_number,
       trace_id,
       root_span_id,
       'lifecycle',
       'info',
       'control',
       sqlc.arg(kind),
       sqlc.arg(message),
       jsonb_build_object('reasonCode', sqlc.arg(reason_code)::text),
       'internal',
       state_version,
       transaction_timestamp()
  FROM runs
 WHERE runs.id = sqlc.arg(run_id);
