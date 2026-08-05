-- name: GetActorCompletionReplay :one
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
   AND run_attempts.entrypoint_kind = 'actor'
   AND run_attempts.terminal_session_input_sequence IS NOT NULL
   AND run_attempts.terminal_at IS NOT NULL
   AND (
       (run_leases.state = 'completed'
        AND run_leases.terminal_reason_code = 'completed'
        AND run_attempts.terminal_outcome = 'succeeded'
        AND run_attempts.terminal_reason_code = 'completed')
       OR
       (run_leases.state = 'failed'
        AND run_leases.terminal_reason_code = 'actor_failed'
        AND run_attempts.terminal_outcome = 'failed'
        AND run_attempts.terminal_reason_code = 'actor_failed')
   );

-- name: CompleteActorAttempt :one
UPDATE run_attempts
   SET terminal_session_input_sequence = sqlc.arg(terminal_session_input_sequence),
       terminal_outcome = sqlc.arg(terminal_outcome),
       terminal_reason_code = sqlc.arg(reason_code),
       terminal_error = sqlc.narg(error),
       terminal_at = sqlc.arg(completed_at)
 WHERE run_id = sqlc.arg(run_id)
   AND number = sqlc.arg(number)
   AND workspace_id = sqlc.arg(workspace_id)
   AND entrypoint_kind = 'actor'
   AND session_input_start_sequence IS NOT NULL
   AND entrypoint_entered_at IS NOT NULL
   AND terminal_at IS NULL
RETURNING *;

-- name: AdvanceActorWorkspaceHead :one
UPDATE workspaces
   SET head_version_id = sqlc.arg(new_head_version_id),
       state_version = state_version + 1,
       last_activity_at = sqlc.arg(completed_at),
       updated_at = sqlc.arg(completed_at)
 WHERE workspaces.id = sqlc.arg(id)
   AND workspaces.environment_id = sqlc.arg(environment_id)
   AND EXISTS (
       SELECT 1 FROM environments
        WHERE environments.id = workspaces.environment_id
          AND environments.org_id = sqlc.arg(org_id)
          AND environments.project_id = sqlc.arg(project_id)
   )
   AND workspaces.owner_session_id = sqlc.arg(session_id)
   AND workspaces.owner_run_id IS NULL
   AND workspaces.ownership_generation = sqlc.arg(ownership_generation)
   AND workspaces.writer_generation = sqlc.arg(writer_generation)
   AND workspaces.head_version_id = sqlc.arg(expected_head_version_id)
   AND workspaces.state = 'active'
   AND workspaces.desired_state = 'active'
   AND workspaces.dirty_state = 'clean'
RETURNING *;

-- name: CreateActorRetryAttempt :one
INSERT INTO run_attempts (
    run_id, number, entrypoint_kind, workspace_id,
    session_input_start_sequence, base_workspace_version_id
)
SELECT runs.id,
       sqlc.arg(number),
       'actor',
       runs.workspace_id,
       sessions.committed_input_sequence,
       workspaces.head_version_id
  FROM runs
  JOIN sessions
    ON sessions.id = runs.session_id
   AND sessions.workspace_id = runs.workspace_id
   AND sessions.current_run_id = runs.id
   AND sessions.run_generation = sqlc.arg(expected_run_generation)
   AND sessions.state IN ('open', 'closing')
  JOIN workspaces
    ON workspaces.id = runs.workspace_id
   AND workspaces.owner_session_id = sessions.id
   AND workspaces.owner_run_id IS NULL
   AND workspaces.head_version_id IS NOT NULL
 WHERE runs.id = sqlc.arg(run_id)
   AND runs.workspace_id = sqlc.arg(workspace_id)
   AND runs.entrypoint_kind = 'actor'
   AND runs.current_attempt_number = sqlc.arg(previous_attempt_number)
   AND runs.current_run_lease_id = sqlc.arg(run_lease_id)
   AND runs.status = 'running'
RETURNING *;

-- name: DelayActorRunRetry :one
UPDATE runs
   SET status = 'retry_delayed',
       state_version = state_version + 1,
       current_attempt_number = sqlc.arg(next_attempt_number),
       current_run_lease_id = NULL,
       retry_at = sqlc.arg(retry_at),
       updated_at = sqlc.arg(completed_at)
 WHERE id = sqlc.arg(id)
   AND workspace_id = sqlc.arg(workspace_id)
   AND entrypoint_kind = 'actor'
   AND session_id = sqlc.arg(session_id)
   AND status = 'running'
   AND current_attempt_number = sqlc.arg(previous_attempt_number)
   AND current_run_lease_id = sqlc.arg(run_lease_id)
   AND active_started_at IS NULL
RETURNING *;

-- name: CreateActorCheckpointFailureRetryAttempt :one
INSERT INTO run_attempts (
    run_id, number, entrypoint_kind, workspace_id,
    session_input_start_sequence, base_workspace_version_id
)
SELECT runs.id,
       sqlc.arg(number),
       'actor',
       runs.workspace_id,
       sessions.committed_input_sequence,
       workspaces.head_version_id
  FROM runs
  JOIN sessions
    ON sessions.id = runs.session_id
   AND sessions.workspace_id = runs.workspace_id
   AND sessions.current_run_id = runs.id
   AND sessions.run_generation = sqlc.arg(expected_run_generation)
   AND sessions.state IN ('open', 'closing')
  JOIN workspaces
    ON workspaces.id = runs.workspace_id
   AND workspaces.owner_session_id = sessions.id
   AND workspaces.owner_run_id IS NULL
   AND workspaces.head_version_id IS NOT NULL
 WHERE runs.id = sqlc.arg(run_id)
   AND runs.workspace_id = sqlc.arg(workspace_id)
   AND runs.entrypoint_kind = 'actor'
   AND runs.current_attempt_number = sqlc.arg(previous_attempt_number)
   AND runs.current_run_lease_id = sqlc.arg(run_lease_id)
   AND runs.status = 'waiting'
   AND runs.active_started_at IS NULL
RETURNING *;

-- name: DelayActorCheckpointFailureRetry :one
UPDATE runs
   SET status = 'retry_delayed',
       state_version = state_version + 1,
       current_attempt_number = sqlc.arg(next_attempt_number),
       current_run_lease_id = NULL,
       retry_at = sqlc.arg(retry_at),
       updated_at = sqlc.arg(failed_at)
 WHERE id = sqlc.arg(id)
   AND workspace_id = sqlc.arg(workspace_id)
   AND entrypoint_kind = 'actor'
   AND session_id = sqlc.arg(session_id)
   AND status = 'waiting'
   AND current_attempt_number = sqlc.arg(previous_attempt_number)
   AND current_run_lease_id = sqlc.arg(run_lease_id)
   AND active_started_at IS NULL
RETURNING *;

-- name: FinishCheckpointFailedActorRun :one
UPDATE runs
   SET status = sqlc.arg(status),
       output = NULL,
       terminal_reason_code = sqlc.narg(reason_code),
       error = sqlc.narg(error),
       state_version = state_version + 1,
       current_run_lease_id = NULL,
       retry_at = NULL,
       terminal_at = sqlc.arg(failed_at),
       updated_at = sqlc.arg(failed_at)
 WHERE id = sqlc.arg(id)
   AND workspace_id = sqlc.arg(workspace_id)
   AND entrypoint_kind = 'actor'
   AND session_id = sqlc.arg(session_id)
   AND status = 'waiting'
   AND current_attempt_number = sqlc.arg(attempt_number)
   AND current_run_lease_id = sqlc.arg(run_lease_id)
   AND active_started_at IS NULL
RETURNING *;

-- name: FinishActorRun :one
UPDATE runs
   SET status = sqlc.arg(status),
       output = NULL,
       terminal_reason_code = sqlc.narg(reason_code),
       error = sqlc.narg(error),
       state_version = state_version + 1,
       current_run_lease_id = NULL,
       retry_at = NULL,
       terminal_at = sqlc.arg(completed_at),
       updated_at = sqlc.arg(completed_at)
 WHERE id = sqlc.arg(id)
   AND workspace_id = sqlc.arg(workspace_id)
   AND entrypoint_kind = 'actor'
   AND session_id = sqlc.arg(session_id)
   AND status = 'running'
   AND current_attempt_number = sqlc.arg(attempt_number)
   AND current_run_lease_id = sqlc.arg(run_lease_id)
   AND active_started_at IS NULL
RETURNING *;

-- name: ReconcileActorTerminalRun :one
UPDATE sessions
   SET state = sqlc.arg(state),
       current_run_id = NULL,
       run_generation = run_generation + 1,
       state_version = state_version + 1,
       committed_input_sequence = COALESCE(sqlc.narg(committed_input_sequence), committed_input_sequence),
       failure_code = sqlc.narg(failure_code),
       failure_run_id = sqlc.narg(failure_run_id),
       closed_at = CASE WHEN sqlc.arg(state)::text = 'closed' THEN sqlc.arg(completed_at) ELSE closed_at END,
       failed_at = CASE WHEN sqlc.arg(state)::text = 'failed' THEN sqlc.arg(completed_at) ELSE failed_at END,
       updated_at = sqlc.arg(completed_at)
 WHERE environment_id = sqlc.arg(environment_id)
   AND id = sqlc.arg(id)
   AND workspace_id = sqlc.arg(workspace_id)
   AND current_run_id = sqlc.arg(run_id)
   AND run_generation = sqlc.arg(expected_run_generation)
   AND state IN ('open', 'closing')
RETURNING *;

-- name: ReleaseActorWorkspaceOwner :one
UPDATE workspaces
   SET owner_session_id = NULL,
       ownership_generation = ownership_generation + 1,
       state_version = state_version + 1,
       last_activity_at = sqlc.arg(completed_at),
       updated_at = sqlc.arg(completed_at)
 WHERE workspaces.id = sqlc.arg(id)
   AND workspaces.environment_id = sqlc.arg(environment_id)
   AND workspaces.owner_session_id = sqlc.arg(session_id)
   AND workspaces.owner_run_id IS NULL
   AND workspaces.ownership_generation = sqlc.arg(ownership_generation)
   AND workspaces.writer_generation = sqlc.arg(writer_generation)
   AND workspaces.state = 'active'
   AND workspaces.desired_state = 'active'
   AND workspaces.dirty_state = 'clean'
   AND NOT EXISTS (
       SELECT 1 FROM workspace_leases
        WHERE workspace_leases.workspace_id = workspaces.id
          AND workspace_leases.state IN ('active', 'releasing')
   )
   AND NOT EXISTS (
       SELECT 1 FROM workspace_processes
        WHERE workspace_processes.workspace_id = workspaces.id
          AND workspace_processes.state IN ('pending', 'starting', 'running', 'exit_requested')
   )
RETURNING *;

-- name: CreateActorContinuationRun :one
WITH created_run AS (
    INSERT INTO runs (
        id, org_id, project_id, environment_id,
        deployment_id, deployment_definition_id, entrypoint_kind,
        entrypoint_declared_id, cause_kind, session_id,
        session_input_start_sequence, session_input_high_watermark,
        workspace_id, base_workspace_version_id, metadata, tags,
        queue_name, concurrency_key, queue_concurrency_limit, priority,
        queue_origin_at, queue_score_at, queued_expires_at,
        max_active_duration_ms, retry_policy, trace_id, root_span_id
    )
    SELECT sqlc.arg(run_id), environments.org_id, environments.project_id, sessions.environment_id,
           definitions.deployment_id, sessions.deployment_definition_id, 'actor',
           sessions.actor_declared_id, 'continuation', sessions.id,
           sessions.committed_input_sequence, sessions.next_input_sequence - 1,
           sessions.workspace_id, workspaces.head_version_id,
           sessions.run_metadata, sessions.run_tags,
           sessions.run_queue_name, sessions.run_concurrency_key,
           sessions.run_queue_concurrency_limit, sessions.run_priority,
           sqlc.arg(queue_origin_at)::timestamptz,
           sqlc.arg(queue_origin_at)::timestamptz - (sessions.run_priority::double precision * interval '1 second'),
           CASE WHEN sessions.run_queue_ttl_ms IS NULL THEN NULL
                ELSE sqlc.arg(queue_origin_at)::timestamptz + (sessions.run_queue_ttl_ms::double precision * interval '1 millisecond') END,
           sessions.run_max_active_duration_ms, sessions.run_retry_policy,
           sqlc.narg(trace_id), sqlc.arg(root_span_id)
      FROM sessions
      JOIN environments ON environments.id = sessions.environment_id
      JOIN deployment_definitions AS definitions
        ON definitions.environment_id = sessions.environment_id
       AND definitions.id = sessions.deployment_definition_id
       AND definitions.kind = 'actor'
       AND definitions.declared_id = sessions.actor_declared_id
      JOIN workspaces
        ON workspaces.id = sessions.workspace_id
       AND workspaces.owner_session_id = sessions.id
       AND workspaces.owner_run_id IS NULL
       AND workspaces.head_version_id IS NOT NULL
     WHERE sessions.environment_id = sqlc.arg(environment_id)
       AND sessions.id = sqlc.arg(session_id)
       AND sessions.workspace_id = sqlc.arg(workspace_id)
       AND sessions.current_run_id IS NULL
       AND sessions.run_generation = sqlc.arg(expected_run_generation)
       AND sessions.state IN ('open', 'closing')
       AND sessions.manual_run_cancelled = false
       AND sessions.committed_input_sequence < sessions.next_input_sequence - 1
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
    RETURNING *
), created_attempt AS (
    INSERT INTO run_attempts (
        run_id, number, entrypoint_kind, workspace_id,
        session_input_start_sequence, base_workspace_version_id
    )
    SELECT created_run.id, 1, 'actor', created_run.workspace_id,
           created_run.session_input_start_sequence, created_run.base_workspace_version_id
      FROM created_run
    RETURNING run_id
), claimed_actor AS (
    UPDATE sessions
       SET current_run_id = created_run.id,
           state_version = sessions.state_version + 1,
           updated_at = sqlc.arg(queue_origin_at)
      FROM created_run, created_attempt
     WHERE sessions.id = created_run.session_id
       AND sessions.current_run_id IS NULL
       AND sessions.run_generation = sqlc.arg(expected_run_generation)
       AND created_attempt.run_id = created_run.id
    RETURNING sessions.id
)
SELECT created_run.*
  FROM created_run
  JOIN claimed_actor ON claimed_actor.id = created_run.session_id;
