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
       state_version,
       runtime_preparation_count
  FROM runs
 WHERE id = sqlc.arg(id)
   AND org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
 FOR UPDATE;

-- name: ChargeRunRuntimePreparationFailure :one
UPDATE runs
   SET runtime_preparation_count = runtime_preparation_count + 1,
       next_runtime_preparation_at = transaction_timestamp() + make_interval(
           secs => LEAST(60, power(2, runtime_preparation_count + 1)::integer)
       ),
       updated_at = transaction_timestamp()
 WHERE id = sqlc.arg(id)
   AND status = 'queued'
   AND current_run_lease_id IS NULL
   AND current_attempt_number = sqlc.arg(attempt_number)
   AND runtime_preparation_count = sqlc.arg(expected_count)
   AND runtime_preparation_count < 7
RETURNING *;

-- name: ExhaustRunRuntimePreparation :one
UPDATE runs
   SET runtime_preparation_count = 8,
       next_runtime_preparation_at = NULL,
       updated_at = transaction_timestamp()
 WHERE id = sqlc.arg(id)
   AND status = 'queued'
   AND current_run_lease_id IS NULL
   AND current_attempt_number = sqlc.arg(attempt_number)
   AND runtime_preparation_count = 7
RETURNING *;

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
       base_workspace_version_id,
       child_writer_generation
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

-- name: GetFreshRunLeaseLossAuthority :one
SELECT runs.id AS run_id,
       runs.workspace_id,
       runs.status AS run_status,
       runs.state_version,
       runs.current_attempt_number,
       runs.entrypoint_kind,
       runs.session_id,
       runs.parent_run_id,
       runs.parent_owns_lifecycle,
       runs.retry_policy,
       runs.max_active_duration_ms,
       runs.active_elapsed_ms,
       runs.active_started_at,
       run_leases.id AS run_lease_id,
       run_leases.state AS run_lease_state,
       run_leases.worker_epoch,
       run_leases.start_deadline_at,
       run_leases.expires_at AS run_lease_expires_at,
       worker_instances.state AS worker_state,
       worker_instances.current_epoch AS worker_current_epoch,
       worker_instances.epoch_started_at AS worker_epoch_started_at,
       worker_instances.updated_at AS worker_updated_at,
       worker_instances.lost_at AS worker_lost_at,
       worker_instances.termination_ready_at AS worker_termination_ready_at,
       runtime_instances.desired_state AS runtime_desired_state,
       runtime_instances.observed_state AS runtime_observed_state,
       runtime_instances.lost_at AS runtime_lost_at,
       runtime_instances.failed_at AS runtime_failed_at,
       workspace_leases.state AS workspace_lease_state,
       workspace_mounts.state AS mount_state,
       workspace_mounts.lost_at AS mount_lost_at,
       workspace_mounts.failed_at AS mount_failed_at,
       sessions.run_generation AS actor_run_generation,
       transaction_timestamp()::timestamptz AS observed_at
  FROM runs
  JOIN run_attempts
    ON run_attempts.run_id = runs.id
   AND run_attempts.number = runs.current_attempt_number
   AND run_attempts.workspace_id = runs.workspace_id
   AND run_attempts.terminal_at IS NULL
  JOIN run_leases
    ON run_leases.id = runs.current_run_lease_id
   AND run_leases.run_id = runs.id
   AND run_leases.attempt_number = runs.current_attempt_number
   AND run_leases.workspace_id = runs.workspace_id
  JOIN worker_instances
    ON worker_instances.id = run_leases.worker_instance_id
  JOIN runtime_instances
    ON runtime_instances.id = run_leases.runtime_instance_id
   AND runtime_instances.worker_instance_id = run_leases.worker_instance_id
   AND runtime_instances.worker_epoch = run_leases.worker_epoch
   AND runtime_instances.workspace_id = runs.workspace_id
   AND runtime_instances.reclaimed_at IS NULL
  JOIN workspace_leases
    ON workspace_leases.owner_run_lease_id = run_leases.id
   AND workspace_leases.workspace_id = runs.workspace_id
   AND workspace_leases.runtime_instance_id = run_leases.runtime_instance_id
   AND workspace_leases.state IN ('active', 'releasing')
  JOIN workspace_mounts
    ON workspace_mounts.id = workspace_leases.workspace_mount_id
   AND workspace_mounts.runtime_instance_id = run_leases.runtime_instance_id
   AND workspace_mounts.workspace_id = runs.workspace_id
   AND workspace_mounts.state IN ('mounting', 'mounted', 'unmounting', 'lost', 'failed')
  LEFT JOIN sessions
    ON sessions.id = runs.session_id
   AND sessions.current_run_id = runs.id
   AND sessions.workspace_id = runs.workspace_id
   AND sessions.state IN ('open', 'closing')
 WHERE runs.id = sqlc.arg(run_id)
   AND runs.workspace_id = sqlc.arg(workspace_id)
   AND runs.current_attempt_number = sqlc.arg(attempt_number)
   AND runs.current_run_lease_id = sqlc.arg(run_lease_id)
   AND run_leases.id = sqlc.arg(run_lease_id)
   AND run_leases.state IN ('assigned', 'starting', 'running')
   AND ((run_leases.state IN ('assigned', 'starting')
         AND runs.status = 'queued'
         AND runs.active_started_at IS NULL)
        OR (run_leases.state = 'running'
            AND runs.status = 'running'
            AND runs.active_started_at IS NOT NULL))
   AND NOT EXISTS (
       SELECT 1
         FROM run_waits
        WHERE run_waits.run_id = runs.id
          AND run_waits.attempt_number = runs.current_attempt_number
          AND run_waits.current_run_lease_id = run_leases.id
          AND run_waits.suspension_state = 'resuming'
   );

-- name: StopLostRunActiveInterval :one
UPDATE runs
   SET active_elapsed_ms = LEAST(
           max_active_duration_ms,
           active_elapsed_ms + GREATEST(
               floor(extract(epoch FROM (
                   sqlc.arg(loss_at)::timestamptz - active_started_at
               )) * 1000)::bigint,
               0
           )
       ),
       active_started_at = NULL,
       updated_at = transaction_timestamp()
 WHERE id = sqlc.arg(run_id)
   AND workspace_id = sqlc.arg(workspace_id)
   AND status = 'running'
   AND state_version = sqlc.arg(expected_state_version)
   AND current_attempt_number = sqlc.arg(attempt_number)
   AND current_run_lease_id = sqlc.arg(run_lease_id)
   AND active_started_at IS NOT NULL
   AND sqlc.arg(loss_at)::timestamptz >= active_started_at
RETURNING *;

-- name: ClearFreshPrestartRunLease :one
UPDATE runs
   SET current_run_lease_id = NULL,
       state_version = state_version + 1,
       updated_at = transaction_timestamp()
 WHERE id = sqlc.arg(run_id)
   AND workspace_id = sqlc.arg(workspace_id)
   AND status = 'queued'
   AND state_version = sqlc.arg(expected_state_version)
   AND current_attempt_number = sqlc.arg(attempt_number)
   AND current_run_lease_id = sqlc.arg(run_lease_id)
   AND active_started_at IS NULL
RETURNING *;

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
