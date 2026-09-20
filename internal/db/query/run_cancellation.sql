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
SELECT sessions.id, runs.id AS run_id, sessions.active_turn_id, sessions.dispatch_hold_id
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
       revision,
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
   AND status IN ('mounting', 'mounted', 'unmounting')
 ORDER BY id
 FOR UPDATE;

-- name: LockCancellationWorkspaceLeases :many
SELECT id
  FROM workspace_leases
 WHERE owner_run_lease_id = ANY(sqlc.arg(run_lease_ids)::uuid[])
   AND status IN ('active', 'releasing')
 ORDER BY id
 FOR UPDATE;

-- name: LockCancellationWaits :many
SELECT id,
       run_id,
       workspace_id,
       child_run_id,
       condition_status,
       suspension_status,
       expected_run_revision,
       attempt_number,
       current_run_lease_id,
       prior_run_lease_id,
       suspend_checkpoint_id,
       base_workspace_version_id,
       child_writer_generation
  FROM run_waits
 WHERE (
       run_id = ANY(sqlc.arg(run_ids)::uuid[])
       OR child_run_id = ANY(sqlc.arg(cancel_ids)::uuid[])
 )
   AND suspension_status IN (
       'hot', 'checkpointing', 'parked', 'resume_pending', 'resuming'
   )
 ORDER BY array_position(sqlc.arg(run_ids)::uuid[], run_id), id
 FOR UPDATE;

-- name: LockCancellationCheckpoints :many
SELECT id
  FROM run_checkpoints
 WHERE run_id = ANY(sqlc.arg(run_ids)::uuid[])
   AND status IN ('creating', 'ready')
 ORDER BY array_position(sqlc.arg(run_ids)::uuid[], run_id), id
 FOR UPDATE;

-- name: ResolveHotTerminalChildWait :one
WITH moved_run AS (
    UPDATE runs
       SET status = 'running',
           revision = revision + 1,
           updated_at = transaction_timestamp()
     WHERE runs.id = sqlc.arg(run_id)
       AND runs.status = 'waiting'
       AND runs.revision = sqlc.arg(expected_run_revision)
       AND runs.current_attempt_number = sqlc.arg(attempt_number)
       AND runs.current_run_lease_id = sqlc.arg(current_run_lease_id)
    RETURNING runs.revision
)
UPDATE run_waits
   SET condition_status = sqlc.arg(condition_status)::text,
       condition_result = sqlc.arg(condition_result)::jsonb,
       condition_error = sqlc.arg(condition_error)::jsonb,
       condition_terminal_at = transaction_timestamp(),
       condition_reason_code = sqlc.narg(reason_code)::text,
       suspension_status = 'released',
       expected_run_revision = moved_run.revision,
       suspension_terminal_at = transaction_timestamp(),
       updated_at = transaction_timestamp()
  FROM moved_run
 WHERE run_waits.id = sqlc.arg(wait_id)
   AND run_waits.run_id = sqlc.arg(run_id)
   AND run_waits.condition_status = 'pending'
   AND run_waits.suspension_status = 'hot'
RETURNING run_waits.id;

-- name: ResolveCheckpointingTerminalChildWait :one
UPDATE run_waits
   SET condition_status = sqlc.arg(condition_status)::text,
       condition_result = sqlc.arg(condition_result)::jsonb,
       condition_error = sqlc.arg(condition_error)::jsonb,
       condition_terminal_at = transaction_timestamp(),
       condition_reason_code = sqlc.narg(reason_code)::text,
       updated_at = transaction_timestamp()
 WHERE id = sqlc.arg(wait_id)
   AND run_id = sqlc.arg(run_id)
   AND condition_status = 'pending'
   AND suspension_status = 'checkpointing'
RETURNING id;

-- name: ResolveParkedTerminalChildWait :one
WITH moved_run AS (
    UPDATE runs
       SET status = 'queued',
           revision = revision + 1,
           updated_at = transaction_timestamp()
     WHERE runs.id = sqlc.arg(run_id)
       AND runs.status = 'waiting'
       AND runs.revision = sqlc.arg(expected_run_revision)
       AND runs.current_attempt_number = sqlc.arg(attempt_number)
       AND runs.current_run_lease_id IS NULL
    RETURNING runs.revision
)
UPDATE run_waits
   SET condition_status = sqlc.arg(condition_status)::text,
       condition_result = sqlc.arg(condition_result)::jsonb,
       condition_error = sqlc.arg(condition_error)::jsonb,
       condition_terminal_at = transaction_timestamp(),
       condition_reason_code = sqlc.narg(reason_code)::text,
       suspension_status = 'resume_pending',
       resume_request_version = run_waits.resume_request_version + 1,
       expected_run_revision = moved_run.revision,
       resume_workspace_version_id = COALESCE(
           run_waits.resume_workspace_version_id,
           sqlc.narg(resolved_workspace_version_id)
       ),
       updated_at = transaction_timestamp()
  FROM moved_run
 WHERE run_waits.id = sqlc.arg(wait_id)
   AND run_waits.run_id = sqlc.arg(run_id)
   AND run_waits.condition_status = 'pending'
   AND run_waits.suspension_status = 'parked'
RETURNING run_waits.id,
          run_waits.environment_id,
          run_waits.run_id,
          run_waits.workspace_id,
          run_waits.resume_request_version;

-- name: TerminalizeRunSuspensions :exec
UPDATE run_waits
   SET condition_status = CASE
           WHEN condition_status = 'pending' THEN sqlc.arg(condition_status)::text
           ELSE condition_status
       END,
       condition_result = CASE
           WHEN condition_status = 'pending' THEN NULL
           ELSE condition_result
       END,
       condition_error = CASE
           WHEN condition_status = 'pending' THEN sqlc.arg(error_payload)::jsonb
           ELSE condition_error
       END,
       condition_terminal_at = CASE
           WHEN condition_status = 'pending' THEN transaction_timestamp()
           ELSE condition_terminal_at
       END,
       condition_reason_code = CASE
           WHEN condition_status = 'pending' THEN sqlc.arg(reason_code)::text
           ELSE condition_reason_code
       END,
       suspension_status = sqlc.arg(suspension_status)::text,
       current_run_lease_id = NULL,
       suspension_terminal_at = transaction_timestamp(),
       suspension_reason_code = sqlc.arg(reason_code)::text,
       suspension_error = sqlc.arg(error_payload)::jsonb,
       updated_at = transaction_timestamp()
 WHERE run_id = sqlc.arg(run_id)
   AND suspension_status IN ('hot', 'checkpointing', 'parked', 'resume_pending', 'resuming');

-- name: InvalidateRunCheckpoints :exec
UPDATE run_checkpoints
   SET status = 'invalid',
       invalidated_at = transaction_timestamp(),
       invalidation_reason_code = sqlc.arg(reason_code)::text
 WHERE run_id = sqlc.arg(run_id)
   AND status IN ('creating', 'ready');

-- name: GetRunExecutionLeaseLossAuthority :one
SELECT runs.id AS run_id,
       runs.workspace_id,
       runs.status AS run_status,
       runs.revision,
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
       run_leases.status AS run_lease_status,
       run_leases.worker_epoch,
       run_leases.start_deadline_at,
       run_leases.expires_at AS run_lease_expires_at,
       worker_instances.status AS worker_status,
       worker_instances.current_epoch AS worker_current_epoch,
       worker_instances.epoch_started_at AS worker_epoch_started_at,
       worker_instances.updated_at AS worker_updated_at,
       worker_instances.lost_at AS worker_lost_at,
       worker_instances.termination_ready_at AS worker_termination_ready_at,
       runtime_instances.desired_state AS runtime_desired_state,
       runtime_instances.observed_state AS runtime_observed_state,
       CASE WHEN runtime_instances.observed_state = 'lost' THEN runtime_instances.terminal_at END::timestamptz AS runtime_lost_at,
       CASE WHEN runtime_instances.observed_state = 'failed' THEN runtime_instances.terminal_at END::timestamptz AS runtime_failed_at,
       workspace_leases.status AS workspace_lease_status,
       workspace_mounts.status AS mount_status,
       workspace_mounts.lost_at AS mount_lost_at,
       workspace_mounts.failed_at AS mount_failed_at,
       sessions.run_generation AS actor_run_generation,
       sessions.dispatch_hold_id AS actor_dispatch_hold_id,
       EXISTS (SELECT 1 FROM run_waits WHERE run_waits.run_id = runs.id
                AND run_waits.current_run_lease_id = run_leases.id
                AND run_waits.suspension_status = 'resuming') AS has_resume_wait,
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
   AND workspace_leases.status IN ('active', 'releasing')
  JOIN workspace_mounts
    ON workspace_mounts.id = workspace_leases.workspace_mount_id
   AND workspace_mounts.runtime_instance_id = run_leases.runtime_instance_id
   AND workspace_mounts.workspace_id = runs.workspace_id
   AND workspace_mounts.status IN ('mounting', 'mounted', 'unmounting', 'lost', 'failed')
  LEFT JOIN sessions
    ON sessions.id = runs.session_id
   AND sessions.current_run_id = runs.id
   AND sessions.workspace_id = runs.workspace_id
   AND sessions.status IN ('open', 'closing')
 WHERE runs.id = sqlc.arg(run_id)
   AND runs.workspace_id = sqlc.arg(workspace_id)
   AND runs.current_attempt_number = sqlc.arg(attempt_number)
   AND runs.current_run_lease_id = sqlc.arg(run_lease_id)
   AND run_leases.id = sqlc.arg(run_lease_id)
   AND run_leases.status IN ('assigned', 'starting', 'running', 'checkpointing', 'finalizing')
   AND ((run_leases.status IN ('assigned', 'starting')
         AND runs.status = 'queued'
         AND runs.active_started_at IS NULL)
        OR (run_leases.status = 'running'
            AND runs.status = 'running'
            AND runs.active_started_at IS NOT NULL)
        OR (run_leases.status = 'checkpointing'
            AND runs.status = 'waiting'
            AND runs.active_started_at IS NOT NULL)
        OR (run_leases.status = 'finalizing'
            AND runs.status = 'running'
            AND runs.active_started_at IS NULL
            AND run_leases.finalization_operation_id IS NOT NULL
            AND run_leases.finalization_kind IS NOT NULL
            AND run_leases.finalization_started_at IS NOT NULL
            AND run_leases.finalization_request_fingerprint IS NOT NULL))
   AND (runs.entrypoint_kind = 'task'
        OR EXISTS (SELECT 1 FROM sessions
                    WHERE sessions.id = runs.session_id
                      AND sessions.current_run_id = runs.id
                      AND sessions.status IN ('open', 'closing')))
   AND NOT EXISTS (
       SELECT 1 FROM run_waits
        WHERE run_waits.run_id = runs.id
          AND run_waits.attempt_number = runs.current_attempt_number
          AND run_waits.current_run_lease_id = run_leases.id
          AND run_waits.suspension_status = 'resuming'
          AND (runs.entrypoint_kind = 'task' OR EXISTS (
              SELECT 1 FROM sessions
              JOIN run_checkpoints ON run_checkpoints.id = run_waits.suspend_checkpoint_id
               AND run_checkpoints.run_id = runs.id
               AND run_checkpoints.attempt_number = runs.current_attempt_number
               AND run_checkpoints.run_wait_id = run_waits.id
               AND run_checkpoints.workspace_id = runs.workspace_id
              JOIN workspace_versions ON workspace_versions.id = run_checkpoints.private_workspace_version_id
               AND workspace_versions.workspace_id = run_checkpoints.workspace_id
              JOIN run_leases AS source_run_leases ON source_run_leases.id = run_checkpoints.source_run_lease_id
               AND source_run_leases.run_id = runs.id
               AND source_run_leases.attempt_number = runs.current_attempt_number
               AND source_run_leases.workspace_id = runs.workspace_id
              WHERE sessions.id = runs.session_id AND sessions.current_run_id = runs.id
                AND sessions.dispatch_hold_id IS NULL
                AND run_checkpoints.status = 'ready'
                AND (run_checkpoints.expires_at IS NULL OR run_checkpoints.expires_at > transaction_timestamp())
                AND workspace_versions.status = 'private'
                AND source_run_leases.status = 'checkpointed'
                AND run_checkpoints.actor_speculative_input_sequence
                    BETWEEN sessions.committed_input_sequence AND sessions.next_input_sequence - 1
                AND (run_leases.status <> 'running' OR runs.active_started_at IS NULL
                     OR runs.active_started_at
                        + (GREATEST(runs.max_active_duration_ms - runs.active_elapsed_ms, 0)::text || ' milliseconds')::interval
                        > LEAST(run_leases.expires_at,
                            COALESCE(worker_instances.lost_at, 'infinity'::timestamptz),
                            COALESCE(worker_instances.termination_ready_at, 'infinity'::timestamptz),
                            CASE WHEN worker_instances.current_epoch IS DISTINCT FROM run_leases.worker_epoch
                                 THEN COALESCE(worker_instances.epoch_started_at, worker_instances.updated_at)
                                 ELSE 'infinity'::timestamptz END,
                            CASE WHEN runtime_instances.observed_state IN ('lost', 'failed')
                                 THEN runtime_instances.terminal_at ELSE 'infinity'::timestamptz END,
                            COALESCE(workspace_mounts.lost_at, 'infinity'::timestamptz),
                            COALESCE(workspace_mounts.failed_at, 'infinity'::timestamptz)))
          ))
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
   AND status IN ('running', 'waiting')
   AND revision = sqlc.arg(expected_revision)
   AND current_attempt_number = sqlc.arg(attempt_number)
   AND current_run_lease_id = sqlc.arg(run_lease_id)
   AND active_started_at IS NOT NULL
   AND sqlc.arg(loss_at)::timestamptz >= active_started_at
RETURNING *;

-- name: ClearFreshPrestartRunLease :one
UPDATE runs
   SET current_run_lease_id = NULL,
       revision = revision + 1,
       updated_at = transaction_timestamp()
 WHERE id = sqlc.arg(run_id)
   AND workspace_id = sqlc.arg(workspace_id)
   AND status = 'queued'
   AND revision = sqlc.arg(expected_revision)
   AND current_attempt_number = sqlc.arg(attempt_number)
   AND current_run_lease_id = sqlc.arg(run_lease_id)
   AND active_started_at IS NULL
RETURNING *;

-- name: FenceRunWorkspaceLease :execrows
UPDATE workspace_leases
   SET status = 'fenced',
       terminal_at = transaction_timestamp(),
       terminal_reason_code = sqlc.arg(reason_code)::text,
       terminal_error = sqlc.arg(error_payload)::jsonb,
       updated_at = transaction_timestamp()
 WHERE owner_run_lease_id = sqlc.arg(run_lease_id)
   AND status IN ('active', 'releasing');

-- name: TerminalizeRunLease :execrows
UPDATE run_leases
   SET status = CASE
           WHEN sqlc.arg(status)::text = 'failed' AND started_at IS NULL
           THEN 'rejected'
           ELSE sqlc.arg(status)::text
       END,
       terminal_at = transaction_timestamp(),
       terminal_reason_code = sqlc.arg(reason_code)::text,
       terminal_error = sqlc.arg(error_payload)::jsonb,
       updated_at = transaction_timestamp()
 WHERE id = sqlc.arg(id)
   AND run_id = sqlc.arg(run_id)
   AND status IN ('assigned', 'starting', 'running', 'checkpointing', 'finalizing');

-- name: CloseRunRuntimes :exec
WITH candidate_runtimes AS (
    SELECT run_leases.runtime_instance_id
      FROM run_leases
     WHERE run_leases.id = sqlc.narg(run_lease_id)
       AND run_leases.run_id = sqlc.arg(run_id)
    UNION
    SELECT runtime_instances.id AS runtime_instance_id
      FROM runtime_instances
     WHERE runtime_instances.reserved_run_id = sqlc.arg(run_id)
), close_runtimes AS (
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
       AND runtime_instances.observed_state IN ('allocated', 'ready')
    RETURNING runtime_instances.id
)
UPDATE workspace_mounts
   SET status = 'unmounting',
       stopped_at = COALESCE(stopped_at, transaction_timestamp()),
       updated_at = transaction_timestamp()
 WHERE runtime_instance_id IN (
       SELECT runtime_instance_id FROM candidate_runtimes
       UNION
       SELECT id FROM close_runtimes
   )
   AND status IN ('mounting', 'mounted');

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
       revision = revision + 1,
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
   AND revision = sqlc.arg(expected_revision)
   AND status IN ('queued', 'running', 'waiting', 'retry_delayed', 'cancel_requested');

-- name: ReleaseTaskWorkspace :exec
UPDATE workspaces
   SET owner_run_id = NULL,
       ownership_generation = ownership_generation + 1,
       revision = revision + 1,
       last_activity_at = transaction_timestamp(),
       updated_at = transaction_timestamp()
 WHERE id = sqlc.arg(workspace_id)
   AND owner_run_id = sqlc.arg(run_id)
   AND owner_session_id IS NULL;

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
       revision,
       transaction_timestamp()
  FROM runs
 WHERE runs.id = sqlc.arg(run_id);
