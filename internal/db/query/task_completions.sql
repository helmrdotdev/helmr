-- name: GetTaskCompletionReplay :one
SELECT run_leases.terminal_request_fingerprint
  FROM run_leases
  JOIN run_attempts
    ON run_attempts.run_id = run_leases.run_id
   AND run_attempts.number = run_leases.attempt_number
   AND run_attempts.workspace_id = run_leases.workspace_id
 WHERE run_leases.id = sqlc.arg(run_lease_id)
   AND run_leases.run_id = sqlc.arg(run_id)
   AND run_leases.workspace_id = sqlc.arg(workspace_id)
   AND run_leases.attempt_number = sqlc.arg(attempt_number)
   AND run_leases.lease_sequence = sqlc.arg(lease_sequence)
   AND run_leases.worker_group_id = sqlc.arg(worker_group_id)
   AND run_leases.worker_instance_id = sqlc.arg(worker_instance_id)
   AND run_leases.terminal_request_fingerprint IS NOT NULL
   AND run_leases.terminal_at IS NOT NULL
   AND run_attempts.terminal_at IS NOT NULL
   AND (
       (run_leases.state = 'completed'
        AND run_leases.terminal_reason_code = 'completed'
        AND run_attempts.terminal_outcome = 'succeeded'
        AND run_attempts.terminal_reason_code = 'completed')
       OR
       (run_leases.state = 'failed'
        AND run_leases.terminal_reason_code IN ('task_failed', 'task_payload_invalid')
        AND run_attempts.terminal_outcome = 'failed'
        AND run_attempts.terminal_reason_code = run_leases.terminal_reason_code)
   );

-- name: GetTaskCompletionTime :one
SELECT clock_timestamp()::timestamptz;

-- name: GetTaskWorkspaceResetVersion :one
SELECT *
  FROM workspace_versions
 WHERE environment_id = sqlc.arg(environment_id)
   AND workspace_id = sqlc.arg(workspace_id)
   AND id = sqlc.arg(id)
   AND state IN ('committed', 'private');

-- name: PublishTaskWorkspaceVersion :one
INSERT INTO workspace_versions (
    id,
    public_id,
    environment_id,
    workspace_id,
    parent_version_id,
    artifact_id,
    artifact_kind,
    kind,
    content_digest,
    size_bytes,
    entry_count,
    state,
    source_workspace_lease_id,
    ownership_generation,
    writer_generation,
    published_at
)
VALUES (
    sqlc.arg(id),
    sqlc.arg(public_id),
    sqlc.arg(environment_id),
    sqlc.arg(workspace_id),
    sqlc.arg(parent_version_id),
    sqlc.arg(artifact_id),
    'workspace_version',
    'user',
    sqlc.arg(content_digest),
    sqlc.arg(size_bytes),
    sqlc.arg(entry_count),
    'committed',
    sqlc.arg(source_workspace_lease_id),
    sqlc.arg(ownership_generation),
    sqlc.arg(writer_generation),
    sqlc.arg(published_at)
)
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
   AND materialized_version_id = sqlc.arg(base_version_id)
   AND fencing_generation = sqlc.arg(mount_fencing_generation)
   AND state = 'mounted'
RETURNING *;

-- name: ReleaseTaskWorkspaceLease :one
UPDATE workspace_leases
   SET state = 'released',
       released_at = sqlc.arg(completed_at),
       terminal_at = sqlc.arg(completed_at),
       updated_at = sqlc.arg(completed_at)
 WHERE id = sqlc.arg(id)
   AND workspace_id = sqlc.arg(workspace_id)
   AND workspace_mount_id = sqlc.arg(workspace_mount_id)
   AND runtime_instance_id = sqlc.arg(runtime_instance_id)
   AND owner_run_lease_id = sqlc.arg(owner_run_lease_id)
   AND owner_process_id IS NULL
   AND base_version_id = sqlc.arg(base_version_id)
   AND ownership_generation = sqlc.arg(ownership_generation)
   AND writer_generation = sqlc.arg(writer_generation)
   AND mount_fencing_generation = sqlc.arg(mount_fencing_generation)
   AND state = 'active'
   AND expires_at > sqlc.arg(completed_at)
RETURNING *;

-- name: CompleteTaskRunLease :one
UPDATE run_leases
   SET state = sqlc.arg(state),
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
   AND state = 'finalizing'
   AND finalization_operation_id IS NOT NULL
   AND finalization_kind IS NOT NULL
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
       terminal_reason_code = sqlc.narg(reason_code),
       error = sqlc.narg(error),
       state_version = state_version + 1,
       current_run_lease_id = NULL,
       retry_at = NULL,
       terminal_at = sqlc.arg(completed_at),
       updated_at = sqlc.arg(completed_at)
 WHERE id = sqlc.arg(id)
   AND workspace_id = sqlc.arg(workspace_id)
   AND entrypoint_kind = 'task'
   AND actor_id IS NULL
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
       runs.base_workspace_version_id
  FROM runs
 WHERE runs.id = sqlc.arg(run_id)
   AND runs.workspace_id = sqlc.arg(workspace_id)
   AND runs.entrypoint_kind = 'task'
   AND runs.actor_id IS NULL
   AND runs.status = 'running'
   AND runs.current_attempt_number = sqlc.arg(previous_attempt_number)
   AND runs.current_run_lease_id = sqlc.arg(run_lease_id)
RETURNING *;

-- name: DelayTaskRunRetry :one
UPDATE runs
   SET status = 'retry_delayed',
       state_version = state_version + 1,
       current_attempt_number = sqlc.arg(next_attempt_number),
       current_run_lease_id = NULL,
       retry_at = sqlc.arg(retry_at),
       updated_at = sqlc.arg(completed_at)
 WHERE id = sqlc.arg(id)
   AND workspace_id = sqlc.arg(workspace_id)
   AND entrypoint_kind = 'task'
   AND actor_id IS NULL
   AND status = 'running'
   AND current_attempt_number = sqlc.arg(previous_attempt_number)
   AND current_run_lease_id = sqlc.arg(run_lease_id)
   AND active_started_at IS NULL
RETURNING *;

-- name: ClearSameWorkspaceChildWriter :one
UPDATE run_waits
   SET child_writer_generation = NULL,
       updated_at = sqlc.arg(completed_at)
 WHERE id = sqlc.arg(run_wait_id)
   AND environment_id = sqlc.arg(environment_id)
   AND run_id = sqlc.arg(parent_run_id)
   AND workspace_id = sqlc.arg(workspace_id)
   AND child_run_id = sqlc.arg(child_run_id)
   AND child_parent_owned IS TRUE
   AND condition_state = 'pending'
   AND suspension_state = 'parked'
   AND child_writer_generation = sqlc.arg(child_writer_generation)
   AND resume_writer_generation IS NULL
RETURNING *;

-- name: CompleteSameWorkspaceChildSuccess :one
WITH queued_parent AS (
    UPDATE runs
       SET status = 'queued',
           state_version = state_version + 1,
           updated_at = sqlc.arg(completed_at)
     WHERE id = sqlc.arg(parent_run_id)
       AND environment_id = sqlc.arg(environment_id)
       AND workspace_id = sqlc.arg(workspace_id)
       AND status = 'waiting'
       AND state_version = sqlc.arg(expected_parent_state_version)
       AND current_attempt_number = sqlc.arg(parent_attempt_number)
       AND current_run_lease_id IS NULL
    RETURNING state_version
)
UPDATE run_waits
   SET condition_state = 'completed',
       condition_result = sqlc.arg(condition_result),
       condition_terminal_at = sqlc.arg(completed_at),
       suspension_state = 'resume_pending',
       resume_request_version = resume_request_version + 1,
       expected_run_state_version = queued_parent.state_version,
       child_result_version_id = sqlc.arg(child_result_version_id),
       resume_workspace_version_id = sqlc.arg(child_result_version_id),
       handoff_resume_checkpoint_id = sqlc.arg(handoff_resume_checkpoint_id),
       updated_at = sqlc.arg(completed_at)
  FROM queued_parent
 WHERE run_waits.id = sqlc.arg(run_wait_id)
   AND run_waits.environment_id = sqlc.arg(environment_id)
   AND run_waits.run_id = sqlc.arg(parent_run_id)
   AND run_waits.workspace_id = sqlc.arg(workspace_id)
   AND run_waits.attempt_number = sqlc.arg(parent_attempt_number)
   AND run_waits.child_run_id = sqlc.arg(child_run_id)
   AND run_waits.child_parent_owned IS TRUE
   AND run_waits.condition_state = 'pending'
   AND run_waits.suspension_state = 'parked'
   AND run_waits.expected_run_state_version = sqlc.arg(expected_parent_state_version)
   AND run_waits.current_run_lease_id IS NULL
   AND run_waits.prior_run_lease_id = sqlc.arg(parent_run_lease_id)
   AND run_waits.suspend_checkpoint_id = sqlc.arg(suspend_checkpoint_id)
   AND run_waits.child_writer_generation = sqlc.arg(child_writer_generation)
   AND run_waits.handoff_resume_checkpoint_id IS NULL
RETURNING run_waits.*;

-- name: CompleteSameWorkspaceChildFailure :one
WITH queued_parent AS (
    UPDATE runs
       SET status = 'queued',
           state_version = state_version + 1,
           updated_at = sqlc.arg(completed_at)
     WHERE id = sqlc.arg(parent_run_id)
       AND environment_id = sqlc.arg(environment_id)
       AND workspace_id = sqlc.arg(workspace_id)
       AND status = 'waiting'
       AND state_version = sqlc.arg(expected_parent_state_version)
       AND current_attempt_number = sqlc.arg(parent_attempt_number)
       AND current_run_lease_id IS NULL
    RETURNING state_version
)
UPDATE run_waits
   SET condition_state = sqlc.arg(condition_state),
       condition_error = sqlc.arg(condition_error),
       condition_terminal_at = sqlc.arg(completed_at),
       condition_reason_code = sqlc.arg(reason_code),
       suspension_state = 'resume_pending',
       resume_request_version = resume_request_version + 1,
       expected_run_state_version = queued_parent.state_version,
       resume_workspace_version_id = base_workspace_version_id,
       updated_at = sqlc.arg(completed_at)
  FROM queued_parent
 WHERE run_waits.id = sqlc.arg(run_wait_id)
   AND run_waits.environment_id = sqlc.arg(environment_id)
   AND run_waits.run_id = sqlc.arg(parent_run_id)
   AND run_waits.workspace_id = sqlc.arg(workspace_id)
   AND run_waits.attempt_number = sqlc.arg(parent_attempt_number)
   AND run_waits.child_run_id = sqlc.arg(child_run_id)
   AND run_waits.child_parent_owned IS TRUE
   AND run_waits.condition_state = 'pending'
   AND run_waits.suspension_state = 'parked'
   AND run_waits.expected_run_state_version = sqlc.arg(expected_parent_state_version)
   AND run_waits.current_run_lease_id IS NULL
   AND run_waits.prior_run_lease_id = sqlc.arg(parent_run_lease_id)
   AND run_waits.suspend_checkpoint_id = sqlc.arg(suspend_checkpoint_id)
   AND run_waits.child_writer_generation = sqlc.arg(child_writer_generation)
   AND run_waits.handoff_resume_checkpoint_id IS NULL
RETURNING run_waits.*;

-- name: FailNestedSameWorkspaceWait :one
UPDATE run_waits
   SET condition_state = 'failed',
       condition_error = sqlc.arg(error)::jsonb,
       condition_terminal_at = sqlc.arg(failed_at),
       condition_reason_code = sqlc.arg(reason_code),
       suspension_state = 'failed',
       suspension_terminal_at = sqlc.arg(failed_at),
       suspension_reason_code = 'same_workspace_handoff_runtime_lost',
       suspension_error = sqlc.arg(error)::jsonb,
       updated_at = sqlc.arg(failed_at)
 WHERE id = sqlc.arg(run_wait_id)
   AND environment_id = sqlc.arg(environment_id)
   AND run_id = sqlc.arg(run_id)
   AND attempt_number = sqlc.arg(attempt_number)
   AND workspace_id = sqlc.arg(workspace_id)
   AND child_run_id = sqlc.arg(child_run_id)
   AND child_parent_owned IS TRUE
   AND condition_state = 'pending'
   AND suspension_state = 'parked'
   AND current_run_lease_id IS NULL
   AND prior_run_lease_id IS NOT NULL
   AND suspend_checkpoint_id IS NOT NULL
   AND handoff_runtime_instance_id = sqlc.arg(handoff_runtime_instance_id)
   AND handoff_workspace_mount_id = sqlc.arg(handoff_workspace_mount_id)
   AND handoff_mount_generation = sqlc.arg(handoff_mount_generation)
   AND ownership_generation = sqlc.arg(ownership_generation)
   AND child_writer_generation IS NOT NULL
   AND resume_writer_generation IS NULL
RETURNING *;

-- name: FailNestedSameWorkspaceAttempt :one
UPDATE run_attempts
   SET terminal_outcome = 'failed',
       terminal_reason_code = 'same_workspace_handoff_runtime_lost',
       terminal_error = sqlc.arg(error)::jsonb,
       terminal_at = sqlc.arg(failed_at)
 WHERE run_id = sqlc.arg(run_id)
   AND number = sqlc.arg(attempt_number)
   AND workspace_id = sqlc.arg(workspace_id)
   AND entrypoint_kind = 'task'
   AND terminal_at IS NULL
RETURNING *;

-- name: FailNestedSameWorkspaceRun :one
UPDATE runs
   SET status = 'system_failed',
       terminal_reason_code = 'same_workspace_handoff_runtime_lost',
       error = sqlc.arg(error)::jsonb,
       state_version = state_version + 1,
       current_run_lease_id = NULL,
       retry_at = NULL,
       terminal_at = sqlc.arg(failed_at),
       updated_at = sqlc.arg(failed_at)
 WHERE id = sqlc.arg(run_id)
   AND environment_id = sqlc.arg(environment_id)
   AND workspace_id = sqlc.arg(workspace_id)
   AND entrypoint_kind = 'task'
   AND actor_id IS NULL
   AND status = 'waiting'
   AND current_attempt_number = sqlc.arg(attempt_number)
   AND current_run_lease_id IS NULL
RETURNING *;

-- name: CreateCheckpointFailureRetryAttempt :one
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
       runs.base_workspace_version_id
  FROM runs
 WHERE runs.id = sqlc.arg(run_id)
   AND runs.workspace_id = sqlc.arg(workspace_id)
   AND runs.entrypoint_kind = 'task'
   AND runs.actor_id IS NULL
   AND runs.status = 'waiting'
   AND runs.current_attempt_number = sqlc.arg(previous_attempt_number)
   AND runs.current_run_lease_id = sqlc.arg(run_lease_id)
   AND runs.active_started_at IS NULL
RETURNING *;

-- name: DelayCheckpointFailureRetry :one
UPDATE runs
   SET status = 'retry_delayed',
       state_version = state_version + 1,
       current_attempt_number = sqlc.arg(next_attempt_number),
       current_run_lease_id = NULL,
       retry_at = sqlc.arg(retry_at),
       updated_at = sqlc.arg(failed_at)
 WHERE id = sqlc.arg(id)
   AND workspace_id = sqlc.arg(workspace_id)
   AND entrypoint_kind = 'task'
   AND actor_id IS NULL
   AND status = 'waiting'
   AND current_attempt_number = sqlc.arg(previous_attempt_number)
   AND current_run_lease_id = sqlc.arg(run_lease_id)
   AND active_started_at IS NULL
RETURNING *;

-- name: FinishCheckpointFailedTaskRun :one
UPDATE runs
   SET status = sqlc.arg(status),
       terminal_reason_code = sqlc.arg(reason_code),
       error = sqlc.arg(error)::jsonb,
       state_version = state_version + 1,
       current_run_lease_id = NULL,
       retry_at = NULL,
       terminal_at = sqlc.arg(failed_at),
       updated_at = sqlc.arg(failed_at)
 WHERE id = sqlc.arg(id)
   AND workspace_id = sqlc.arg(workspace_id)
   AND entrypoint_kind = 'task'
   AND actor_id IS NULL
   AND status = 'waiting'
   AND current_attempt_number = sqlc.arg(attempt_number)
   AND current_run_lease_id = sqlc.arg(run_lease_id)
   AND active_started_at IS NULL
RETURNING *;

-- name: ReleaseTaskWorkspaceOwner :one
UPDATE workspaces
   SET head_version_id = COALESCE(sqlc.narg(new_head_version_id), workspaces.head_version_id),
       owner_run_id = NULL,
       ownership_generation = workspaces.ownership_generation + 1,
       state_version = workspaces.state_version + 1,
       last_activity_at = sqlc.arg(completed_at),
       updated_at = sqlc.arg(completed_at)
  FROM environments
 WHERE workspaces.id = sqlc.arg(id)
   AND environments.id = workspaces.environment_id
   AND environments.org_id = sqlc.arg(org_id)
   AND environments.project_id = sqlc.arg(project_id)
   AND workspaces.environment_id = sqlc.arg(environment_id)
   AND workspaces.owner_run_id = sqlc.arg(run_id)
   AND workspaces.owner_actor_id IS NULL
   AND workspaces.ownership_generation = sqlc.arg(ownership_generation)
   AND workspaces.writer_generation = sqlc.arg(writer_generation)
   AND workspaces.head_version_id = sqlc.arg(expected_head_version_id)
   AND workspaces.state = 'active'
   AND workspaces.desired_state = 'active'
   AND workspaces.dirty_state = 'clean'
   AND NOT EXISTS (
       SELECT 1
         FROM workspace_leases
        WHERE workspace_leases.workspace_id = workspaces.id
          AND workspace_leases.state IN ('active', 'releasing')
   )
   AND NOT EXISTS (
       SELECT 1
         FROM workspace_processes
        WHERE workspace_processes.workspace_id = workspaces.id
          AND workspace_processes.state IN ('pending', 'starting', 'running', 'exit_requested')
   )
RETURNING *;

-- name: ReadyRunRetries :many
WITH provided_outbox_ids AS MATERIALIZED (
    SELECT id, ordinality
      FROM unnest(sqlc.arg(outbox_message_ids)::uuid[])
           WITH ORDINALITY AS supplied(id, ordinality)
),
candidates AS (
    SELECT runs.id,
           runs.environment_id,
           runs.workspace_id,
           runs.current_attempt_number,
           runs.state_version
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
       AND NOT EXISTS (
            SELECT 1
              FROM run_leases
             WHERE run_leases.run_id = runs.id
               AND run_leases.attempt_number = runs.current_attempt_number
               AND run_leases.state IN ('assigned', 'starting', 'running', 'checkpointing', 'finalizing')
       )
     ORDER BY runs.retry_at, runs.id
     LIMIT sqlc.arg(row_limit)
     FOR UPDATE OF runs, run_attempts SKIP LOCKED
), readied AS (
    UPDATE runs
       SET status = 'queued',
           retry_at = NULL,
           state_version = runs.state_version + 1,
           updated_at = now()
      FROM candidates
     WHERE runs.id = candidates.id
       AND runs.environment_id = candidates.environment_id
       AND runs.workspace_id = candidates.workspace_id
       AND runs.current_attempt_number = candidates.current_attempt_number
       AND runs.state_version = candidates.state_version
       AND runs.status = 'retry_delayed'
       AND runs.current_run_lease_id IS NULL
       AND cardinality(sqlc.arg(outbox_message_ids)::uuid[])
           >= (SELECT count(*) FROM candidates)
    RETURNING runs.id,
              runs.environment_id,
              runs.workspace_id,
              runs.current_attempt_number,
              runs.state_version
), ordered_readied AS MATERIALIZED (
    SELECT readied.id,
           row_number() OVER (ORDER BY readied.id) AS ordinality
      FROM readied
), admission_outbox AS (
    INSERT INTO outbox_messages (
        id,
        lane,
        topic,
        partition_key,
        payload,
        available_at
    )
    SELECT provided_outbox_ids.id,
           'control',
           'run.admit',
           readied.workspace_id::text,
           jsonb_build_object(
               'environmentId', readied.environment_id::text,
               'runId', readied.id::text
           ),
           now()
      FROM readied
      JOIN ordered_readied USING (id)
      JOIN provided_outbox_ids USING (ordinality)
    RETURNING id
)
SELECT readied.id,
       readied.environment_id,
       readied.workspace_id,
       readied.current_attempt_number,
       readied.state_version
  FROM readied
 WHERE EXISTS (SELECT 1 FROM admission_outbox);
