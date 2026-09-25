-- name: GetTaskCompletionReplay :one
SELECT run_leases.terminal_request_fingerprint
  FROM run_leases
  JOIN run_attempts
    ON run_attempts.run_id = run_leases.run_id
   AND run_attempts.number = run_leases.attempt_number
   AND run_attempts.workspace_id = run_leases.workspace_id
 WHERE run_leases.id = sqlc.arg(run_lease_id)
   AND run_leases.lease_sequence = sqlc.arg(lease_sequence)
   AND run_leases.worker_group_id = sqlc.arg(worker_group_id)
   AND run_leases.worker_instance_id = sqlc.arg(worker_instance_id)
   AND run_leases.terminal_request_fingerprint IS NOT NULL
   AND run_leases.terminal_at IS NOT NULL
   AND run_attempts.terminal_at IS NOT NULL
   AND (
       (run_leases.status = 'completed'
        AND run_leases.terminal_reason_code = 'completed'
        AND run_attempts.terminal_outcome = 'succeeded'
        AND run_attempts.terminal_reason_code = 'completed')
       OR
       (run_leases.status = 'failed'
        AND run_leases.terminal_reason_code IN ('task_failed', 'task_payload_invalid')
        AND run_attempts.terminal_outcome = 'failed'
        AND run_attempts.terminal_reason_code = run_leases.terminal_reason_code)
   );

-- name: GetTaskCompletionTime :one
SELECT clock_timestamp()::timestamptz;

-- name: PublishTaskWorkspaceVersion :one
INSERT INTO computer_versions (
    id,
    environment_id,
    computer_id,
    parent_version_id,
    root_pack_digest,
    logical_bytes,
    status,
    source_workspace_lease_id,
    ownership_generation,
    writer_generation,
    published_at
)
SELECT
    sqlc.arg(id),
    sqlc.arg(environment_id),
    sqlc.arg(workspace_id),
    sqlc.arg(parent_version_id),
    sqlc.arg(root_pack_digest),
    sqlc.arg(logical_bytes),
    'committed',
    sqlc.arg(source_workspace_lease_id),
    sqlc.arg(ownership_generation),
    sqlc.arg(writer_generation),
    sqlc.arg(published_at)
FROM computer_versions predecessor
WHERE predecessor.id=sqlc.arg(parent_version_id)
  AND predecessor.environment_id=sqlc.arg(environment_id)
  AND predecessor.computer_id=sqlc.arg(workspace_id)
  AND predecessor.status='committed'
RETURNING *;

-- name: UpdateTaskWorkspaceMountFrontier :one
UPDATE workspace_mounts
   SET materialized_version_id = sqlc.arg(new_version_id),
       dirty_generation = workspace_mounts.dirty_generation + 1,
       updated_at = sqlc.arg(completed_at)
 WHERE id = sqlc.arg(id)
   AND org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND workspace_id = sqlc.arg(workspace_id)
   AND runtime_instance_id = sqlc.arg(runtime_instance_id)
   AND materialized_version_id = sqlc.arg(base_workspace_version_id)
   AND fencing_generation = sqlc.arg(mount_fencing_generation)
   AND status = 'mounted'
RETURNING *;

-- name: ReleaseTaskWorkspaceLease :one
UPDATE workspace_leases
   SET status = 'released',
       released_at = sqlc.arg(completed_at),
       terminal_at = sqlc.arg(completed_at),
       updated_at = sqlc.arg(completed_at)
 WHERE id = sqlc.arg(id)
   AND workspace_id = sqlc.arg(workspace_id)
   AND workspace_mount_id = sqlc.arg(workspace_mount_id)
   AND runtime_instance_id = sqlc.arg(runtime_instance_id)
   AND owner_run_lease_id = sqlc.arg(owner_run_lease_id)
   AND owner_process_id IS NULL
   AND base_workspace_version_id = sqlc.arg(base_workspace_version_id)
   AND ownership_generation = sqlc.arg(ownership_generation)
   AND writer_generation = sqlc.arg(writer_generation)
   AND mount_fencing_generation = sqlc.arg(mount_fencing_generation)
   AND status = 'active'
   AND expires_at > sqlc.arg(completed_at)
RETURNING *;

-- name: RequestSameWorkspaceChildAttemptRuntimeDiscard :one
WITH authority AS MATERIALIZED (
    SELECT runtime_instances.id AS runtime_instance_id,
           workspace_mounts.id AS workspace_mount_id
      FROM run_leases
      JOIN workspace_leases
        ON workspace_leases.id = sqlc.arg(workspace_lease_id)
       AND workspace_leases.workspace_id = run_leases.workspace_id
       AND workspace_leases.workspace_mount_id = sqlc.arg(workspace_mount_id)
       AND workspace_leases.runtime_instance_id = sqlc.arg(runtime_instance_id)
       AND workspace_leases.owner_run_lease_id = run_leases.id
       AND workspace_leases.owner_process_id IS NULL
       AND workspace_leases.ownership_generation = sqlc.arg(ownership_generation)
       AND workspace_leases.writer_generation = sqlc.arg(writer_generation)
       AND workspace_leases.mount_fencing_generation = sqlc.arg(mount_fencing_generation)
       AND workspace_leases.status = 'released'
      JOIN runtime_instances
        ON runtime_instances.id = workspace_leases.runtime_instance_id
       AND runtime_instances.org_id = sqlc.arg(org_id)
       AND runtime_instances.project_id = sqlc.arg(project_id)
       AND runtime_instances.environment_id = sqlc.arg(environment_id)
       AND runtime_instances.workspace_id = sqlc.arg(workspace_id)
       AND runtime_instances.worker_group_id = sqlc.arg(worker_group_id)
       AND runtime_instances.worker_instance_id = sqlc.arg(worker_instance_id)
       AND runtime_instances.worker_epoch = sqlc.arg(worker_epoch)
       AND runtime_instances.reclaimed_at IS NULL
       AND (runtime_instances.desired_state = 'ready'
            OR (runtime_instances.desired_state = 'closed'
                AND runtime_instances.desired_reason = 'same_workspace_child_attempt_finished'))
       AND runtime_instances.observed_state = 'ready'
      JOIN workspace_mounts
        ON workspace_mounts.id = workspace_leases.workspace_mount_id
       AND workspace_mounts.org_id = runtime_instances.org_id
       AND workspace_mounts.project_id = runtime_instances.project_id
       AND workspace_mounts.environment_id = runtime_instances.environment_id
       AND workspace_mounts.workspace_id = runtime_instances.workspace_id
       AND workspace_mounts.runtime_instance_id = runtime_instances.id
       AND workspace_mounts.worker_group_id = runtime_instances.worker_group_id
       AND workspace_mounts.worker_instance_id = runtime_instances.worker_instance_id
       AND workspace_mounts.worker_epoch = runtime_instances.worker_epoch
       AND workspace_mounts.fencing_generation = sqlc.arg(mount_fencing_generation)
       AND (workspace_mounts.status = 'mounted'
            OR (workspace_mounts.status = 'unmounting'
                AND workspace_mounts.finalization_action = 'discard'
                AND workspace_mounts.finalization_reason_code = 'same_workspace_child_attempt_finished'
                AND workspace_mounts.finalization_error IS NULL))
     WHERE run_leases.id = sqlc.arg(run_lease_id)
       AND run_leases.run_id = sqlc.arg(run_id)
       AND run_leases.workspace_id = sqlc.arg(workspace_id)
       AND run_leases.attempt_number = sqlc.arg(attempt_number)
       AND run_leases.worker_group_id = sqlc.arg(worker_group_id)
       AND run_leases.worker_instance_id = sqlc.arg(worker_instance_id)
       AND run_leases.worker_epoch = sqlc.arg(worker_epoch)
       AND run_leases.runtime_instance_id = runtime_instances.id
       AND run_leases.status IN ('completed', 'failed')
     FOR UPDATE OF runtime_instances, workspace_mounts
), closing_runtime AS (
    UPDATE runtime_instances
       SET desired_state = 'closed',
           desired_version = CASE
               WHEN runtime_instances.desired_state = 'closed'
               THEN runtime_instances.desired_version
               ELSE runtime_instances.desired_version + 1
           END,
           desired_at = sqlc.arg(completed_at),
           desired_reason = 'same_workspace_child_attempt_finished',
           updated_at = sqlc.arg(completed_at)
      FROM authority
     WHERE runtime_instances.id = authority.runtime_instance_id
       AND runtime_instances.reclaimed_at IS NULL
    RETURNING runtime_instances.id
)
UPDATE workspace_mounts
   SET status = 'unmounting',
       finalization_action = 'discard',
       finalization_reason_code = 'same_workspace_child_attempt_finished',
       finalization_error = NULL,
       stopped_at = COALESCE(workspace_mounts.stopped_at, sqlc.arg(completed_at)),
       updated_at = sqlc.arg(completed_at)
  FROM authority, closing_runtime
 WHERE workspace_mounts.id = authority.workspace_mount_id
   AND workspace_mounts.runtime_instance_id = closing_runtime.id
   AND (workspace_mounts.status = 'mounted'
        OR (workspace_mounts.status = 'unmounting'
            AND workspace_mounts.finalization_action = 'discard'
            AND workspace_mounts.finalization_reason_code = 'same_workspace_child_attempt_finished'
            AND workspace_mounts.finalization_error IS NULL))
RETURNING workspace_mounts.*;

-- name: CompleteTaskRunLease :one
UPDATE run_leases
   SET status = sqlc.arg(status),
       terminal_at = sqlc.arg(completed_at),
       terminal_reason_code = sqlc.arg(reason_code),
       terminal_error = sqlc.narg(error),
       terminal_request_fingerprint = sqlc.arg(terminal_request_fingerprint),
       updated_at = sqlc.arg(completed_at)
 WHERE id = sqlc.arg(id)
   AND run_id = sqlc.arg(run_id)
   AND workspace_id = sqlc.arg(workspace_id)
   AND attempt_number = sqlc.arg(attempt_number)
   AND lease_sequence = sqlc.arg(lease_sequence)
   AND status = 'finalizing'
   AND finalization_operation_id IS NOT NULL
   AND finalization_started_at IS NOT NULL
   AND finalization_request_fingerprint IS NOT NULL
   AND terminal_request_fingerprint IS NULL
   AND expires_at > sqlc.arg(completed_at)
RETURNING *;

-- name: CompleteTaskAttempt :one
UPDATE run_attempts
   SET terminal_outcome = sqlc.arg(terminal_outcome),
       terminal_reason_code = sqlc.arg(reason_code),
       terminal_error = sqlc.narg(error),
       terminal_at = sqlc.arg(completed_at)
 WHERE run_id = sqlc.arg(run_id)
   AND number = sqlc.arg(number)
   AND workspace_id = sqlc.arg(workspace_id)
   AND entrypoint_kind = 'task'
   AND entrypoint_entered_at IS NOT NULL
   AND terminal_at IS NULL
RETURNING *;

-- name: FinishTaskRun :one
UPDATE runs
   SET status = sqlc.arg(status),
       output = sqlc.narg(output),
       failure = sqlc.narg(failure),
       revision = revision + 1,
       current_run_lease_id = NULL,
       retry_at = NULL,
       terminal_at = sqlc.arg(completed_at),
       updated_at = sqlc.arg(completed_at)
 WHERE id = sqlc.arg(id)
   AND workspace_id = sqlc.arg(workspace_id)
   AND entrypoint_kind = 'task'
   AND session_id IS NULL
   AND status = 'running'
   AND current_attempt_number = sqlc.arg(attempt_number)
   AND current_run_lease_id = sqlc.arg(run_lease_id)
   AND active_started_at IS NULL
RETURNING *;

-- name: CreateTaskRetryAttempt :one
INSERT INTO run_attempts (
    run_id,
    number,
    entrypoint_kind,
    workspace_id,
    base_workspace_version_id
)
SELECT runs.id,
       sqlc.arg(number),
       'task',
       runs.workspace_id,
       sqlc.arg(result_workspace_version_id)
  FROM runs
 WHERE runs.id = sqlc.arg(run_id)
   AND runs.workspace_id = sqlc.arg(workspace_id)
   AND runs.entrypoint_kind = 'task'
   AND runs.session_id IS NULL
   AND runs.status = 'running'
   AND runs.current_attempt_number = sqlc.arg(previous_attempt_number)
   AND runs.current_run_lease_id = sqlc.arg(run_lease_id)
RETURNING *;

-- name: DelayTaskRunRetry :one
UPDATE runs
   SET status = 'retry_delayed',
       base_workspace_version_id = sqlc.arg(result_workspace_version_id),
       revision = revision + 1,
       current_attempt_number = sqlc.arg(next_attempt_number),
       current_run_lease_id = NULL,
       retry_at = sqlc.arg(retry_at),
       updated_at = sqlc.arg(completed_at)
 WHERE id = sqlc.arg(id)
   AND workspace_id = sqlc.arg(workspace_id)
   AND entrypoint_kind = 'task'
   AND session_id IS NULL
   AND status = 'running'
   AND current_attempt_number = sqlc.arg(previous_attempt_number)
   AND current_run_lease_id = sqlc.arg(run_lease_id)
   AND active_started_at IS NULL
RETURNING *;

-- name: CompleteSameWorkspaceChildSuccess :one
WITH queued_parent AS (
    UPDATE runs
       SET status = 'queued',
           revision = revision + 1,
           updated_at = sqlc.arg(completed_at)
     WHERE id = sqlc.arg(parent_run_id)
       AND environment_id = sqlc.arg(environment_id)
       AND workspace_id = sqlc.arg(workspace_id)
       AND status = 'waiting'
       AND revision = sqlc.arg(expected_parent_revision)
       AND current_attempt_number = sqlc.arg(parent_attempt_number)
       AND current_run_lease_id IS NULL
    RETURNING revision
)
UPDATE run_waits
   SET condition_status = 'completed',
       condition_result = sqlc.arg(condition_result),
       condition_terminal_at = sqlc.arg(completed_at),
       suspension_status = 'resume_pending',
       resume_request_version = resume_request_version + 1,
       expected_run_revision = queued_parent.revision,
       resume_workspace_version_id = sqlc.arg(resume_workspace_version_id),
       updated_at = sqlc.arg(completed_at)
  FROM queued_parent
 WHERE run_waits.id = sqlc.arg(run_wait_id)
   AND run_waits.environment_id = sqlc.arg(environment_id)
   AND run_waits.run_id = sqlc.arg(parent_run_id)
   AND run_waits.workspace_id = sqlc.arg(workspace_id)
   AND run_waits.attempt_number = sqlc.arg(parent_attempt_number)
   AND run_waits.child_run_id = sqlc.arg(child_run_id)
   AND run_waits.kind = 'child'
   AND run_waits.condition_status = 'pending'
   AND run_waits.suspension_status = 'parked'
   AND run_waits.expected_run_revision = sqlc.arg(expected_parent_revision)
   AND run_waits.current_run_lease_id IS NULL
   AND run_waits.prior_run_lease_id = sqlc.arg(parent_run_lease_id)
   AND run_waits.suspend_checkpoint_id = sqlc.arg(suspend_checkpoint_id)
   AND run_waits.child_writer_generation = sqlc.arg(child_writer_generation)
RETURNING run_waits.*;

-- name: CompleteSameWorkspaceChildFailure :one
WITH queued_parent AS (
    UPDATE runs
       SET status = 'queued',
           revision = revision + 1,
           updated_at = sqlc.arg(completed_at)
     WHERE id = sqlc.arg(parent_run_id)
       AND environment_id = sqlc.arg(environment_id)
       AND workspace_id = sqlc.arg(workspace_id)
       AND status = 'waiting'
       AND revision = sqlc.arg(expected_parent_revision)
       AND current_attempt_number = sqlc.arg(parent_attempt_number)
       AND current_run_lease_id IS NULL
    RETURNING revision
)
UPDATE run_waits
   SET condition_status = sqlc.arg(condition_status),
       condition_error = sqlc.arg(condition_error),
       condition_terminal_at = sqlc.arg(completed_at),
       condition_reason_code = sqlc.arg(reason_code),
       suspension_status = 'resume_pending',
       resume_request_version = resume_request_version + 1,
       expected_run_revision = queued_parent.revision,
       resume_workspace_version_id = base_workspace_version_id,
       updated_at = sqlc.arg(completed_at)
  FROM queued_parent
 WHERE run_waits.id = sqlc.arg(run_wait_id)
   AND run_waits.environment_id = sqlc.arg(environment_id)
   AND run_waits.run_id = sqlc.arg(parent_run_id)
   AND run_waits.workspace_id = sqlc.arg(workspace_id)
   AND run_waits.attempt_number = sqlc.arg(parent_attempt_number)
   AND run_waits.child_run_id = sqlc.arg(child_run_id)
   AND run_waits.kind = 'child'
   AND run_waits.condition_status = 'pending'
   AND run_waits.suspension_status = 'parked'
   AND run_waits.expected_run_revision = sqlc.arg(expected_parent_revision)
   AND run_waits.current_run_lease_id IS NULL
   AND run_waits.prior_run_lease_id = sqlc.arg(parent_run_lease_id)
   AND run_waits.suspend_checkpoint_id = sqlc.arg(suspend_checkpoint_id)
   AND run_waits.child_writer_generation = sqlc.arg(child_writer_generation)
RETURNING run_waits.*;

-- name: ReleaseTaskWorkspaceOwner :one
UPDATE computers
   SET head_version_id = COALESCE(sqlc.narg(new_head_version_id), computers.head_version_id),
       owner_run_id = NULL,
       ownership_generation = computers.ownership_generation + 1,
       revision = computers.revision + 1,
       last_activity_at = sqlc.arg(completed_at),
       updated_at = sqlc.arg(completed_at)
  FROM environments
 WHERE computers.id = sqlc.arg(id)
   AND environments.id = computers.environment_id
   AND environments.org_id = sqlc.arg(org_id)
   AND environments.project_id = sqlc.arg(project_id)
   AND computers.environment_id = sqlc.arg(environment_id)
   AND computers.owner_run_id = sqlc.arg(run_id)
   AND computers.owner_session_id IS NULL
   AND computers.ownership_generation = sqlc.arg(ownership_generation)
   AND computers.writer_generation = sqlc.arg(writer_generation)
   AND computers.head_version_id = sqlc.arg(expected_head_version_id)
   AND computers.status = 'active'
   AND computers.desired_state = 'active'
   AND computers.dirty_state = 'clean'
   AND NOT EXISTS (
       SELECT 1
         FROM workspace_leases
        WHERE workspace_leases.workspace_id = computers.id
          AND workspace_leases.status IN ('active', 'releasing')
   )
   AND NOT EXISTS (
       SELECT 1
         FROM workspace_processes
        WHERE workspace_processes.workspace_id = computers.id
          AND workspace_processes.status IN ('pending', 'starting', 'running', 'exit_requested')
   )
RETURNING computers.id, computers.environment_id, computers.region_id, computers.sandbox_declared_id, computers.deployment_definition_id, computers.key, computers.revision, computers.owner_session_id, computers.owner_run_id, computers.ownership_generation, computers.writer_generation, computers.head_version_id, computers.status, computers.desired_state, computers.dirty_state, computers.last_activity_at, computers.created_at, computers.updated_at, computers.deleted_at;

-- name: ReadyRunRetries :many
WITH candidates AS (
    SELECT runs.id,
           runs.environment_id,
           runs.workspace_id,
           runs.current_attempt_number,
           runs.revision
      FROM runs
      JOIN run_attempts
        ON run_attempts.run_id = runs.id
       AND run_attempts.number = runs.current_attempt_number
       AND run_attempts.entrypoint_kind = runs.entrypoint_kind
       AND run_attempts.workspace_id = runs.workspace_id
     WHERE runs.status = 'retry_delayed'
       AND runs.retry_at <= now()
       AND runs.current_run_lease_id IS NULL
       AND run_attempts.terminal_outcome IS NULL
       AND run_attempts.terminal_at IS NULL
       AND EXISTS (SELECT 1 FROM computers c WHERE c.id=runs.workspace_id AND c.status='active' AND c.recovery_failure IS NULL)
       AND (NOT EXISTS(SELECT 1 FROM runs p WHERE p.id=runs.parent_run_id AND p.workspace_id=runs.workspace_id)
         OR EXISTS(SELECT 1 FROM runs p JOIN run_waits w ON w.run_id=p.id AND w.attempt_number=p.current_attempt_number
            WHERE p.id=runs.parent_run_id AND p.status='waiting' AND w.child_run_id=runs.id
              AND w.condition_status='pending' AND w.suspension_status='parked'
              ))
       AND NOT EXISTS (
            SELECT 1
              FROM run_leases
             WHERE run_leases.run_id = runs.id
               AND run_leases.attempt_number = runs.current_attempt_number
               AND run_leases.status IN ('assigned', 'starting', 'running', 'checkpointing', 'finalizing')
       )
     ORDER BY runs.retry_at, runs.id
     LIMIT sqlc.arg(row_limit)
     FOR UPDATE OF runs, run_attempts SKIP LOCKED
), readied AS (
    UPDATE runs
       SET status = 'queued',
           retry_at = NULL,
           revision = runs.revision + 1,
           updated_at = now()
      FROM candidates
     WHERE runs.id = candidates.id
       AND runs.environment_id = candidates.environment_id
       AND runs.workspace_id = candidates.workspace_id
       AND runs.current_attempt_number = candidates.current_attempt_number
       AND runs.revision = candidates.revision
       AND runs.status = 'retry_delayed'
       AND runs.current_run_lease_id IS NULL
    RETURNING runs.id,
              runs.environment_id,
              runs.workspace_id,
              runs.current_attempt_number,
              runs.revision
)
SELECT readied.id,
       readied.environment_id,
       readied.workspace_id,
       readied.current_attempt_number,
       readied.revision
  FROM readied;

-- name: AdvanceTaskRetryWorkspaceHead :one
UPDATE computers
   SET head_version_id = sqlc.arg(result_workspace_version_id),
       revision = revision + 1,
       last_activity_at = sqlc.arg(completed_at),
       updated_at = sqlc.arg(completed_at)
 WHERE id = sqlc.arg(workspace_id)
   AND owner_run_id = sqlc.arg(run_id)
   AND head_version_id = sqlc.arg(expected_head_version_id)
   AND ownership_generation = sqlc.arg(ownership_generation)
   AND writer_generation = sqlc.arg(writer_generation)
RETURNING id;
