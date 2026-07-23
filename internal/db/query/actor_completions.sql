-- name: GetActorCompletionReplay :one
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
   AND run_attempts.entrypoint_kind = 'actor'
   AND run_attempts.terminal_actor_input_sequence IS NOT NULL
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
   SET terminal_actor_input_sequence = sqlc.arg(terminal_actor_input_sequence),
       terminal_outcome = sqlc.arg(terminal_outcome),
       terminal_reason_code = sqlc.arg(reason_code),
       terminal_error = sqlc.narg(error),
       terminal_at = sqlc.arg(completed_at)
 WHERE run_id = sqlc.arg(run_id)
   AND number = sqlc.arg(number)
   AND workspace_id = sqlc.arg(workspace_id)
   AND entrypoint_kind = 'actor'
   AND actor_start_input_sequence IS NOT NULL
   AND entrypoint_entered_at IS NOT NULL
   AND terminal_at IS NULL
RETURNING *;

-- name: AdvanceActorWorkspaceHead :one
UPDATE workspaces
   SET head_version_id = sqlc.arg(new_head_version_id),
       state_version = state_version + 1,
       last_activity_at = sqlc.arg(completed_at),
       updated_at = sqlc.arg(completed_at)
 WHERE id = sqlc.arg(id)
   AND org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND owner_actor_id = sqlc.arg(actor_id)
   AND owner_run_id IS NULL
   AND ownership_generation = sqlc.arg(ownership_generation)
   AND writer_generation = sqlc.arg(writer_generation)
   AND head_version_id = sqlc.arg(expected_head_version_id)
   AND state = 'active'
   AND desired_state = 'active'
   AND dirty_state = 'clean'
RETURNING *;

-- name: CreateActorRetryAttempt :one
INSERT INTO run_attempts (
    run_id, number, entrypoint_kind, workspace_id,
    actor_start_input_sequence, base_workspace_version_id
)
SELECT runs.id,
       sqlc.arg(number),
       'actor',
       runs.workspace_id,
       actors.committed_input_sequence,
       workspaces.head_version_id
  FROM runs
  JOIN actors
    ON actors.id = runs.actor_id
   AND actors.workspace_id = runs.workspace_id
   AND actors.current_run_id = runs.id
   AND actors.run_generation = sqlc.arg(expected_run_generation)
   AND actors.state IN ('open', 'closing')
  JOIN workspaces
    ON workspaces.id = runs.workspace_id
   AND workspaces.owner_actor_id = actors.id
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
   AND actor_id = sqlc.arg(actor_id)
   AND status = 'running'
   AND current_attempt_number = sqlc.arg(previous_attempt_number)
   AND current_run_lease_id = sqlc.arg(run_lease_id)
   AND active_started_at IS NULL
RETURNING *;

-- name: CreateActorCheckpointFailureRetryAttempt :one
INSERT INTO run_attempts (
    run_id, number, entrypoint_kind, workspace_id,
    actor_start_input_sequence, base_workspace_version_id
)
SELECT runs.id,
       sqlc.arg(number),
       'actor',
       runs.workspace_id,
       actors.committed_input_sequence,
       workspaces.head_version_id
  FROM runs
  JOIN actors
    ON actors.id = runs.actor_id
   AND actors.workspace_id = runs.workspace_id
   AND actors.current_run_id = runs.id
   AND actors.run_generation = sqlc.arg(expected_run_generation)
   AND actors.state IN ('open', 'closing')
  JOIN workspaces
    ON workspaces.id = runs.workspace_id
   AND workspaces.owner_actor_id = actors.id
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
   AND actor_id = sqlc.arg(actor_id)
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
   AND actor_id = sqlc.arg(actor_id)
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
   AND actor_id = sqlc.arg(actor_id)
   AND status = 'running'
   AND current_attempt_number = sqlc.arg(attempt_number)
   AND current_run_lease_id = sqlc.arg(run_lease_id)
   AND active_started_at IS NULL
RETURNING *;

-- name: ReconcileActorTerminalRun :one
UPDATE actors
   SET state = sqlc.arg(state),
       current_run_id = NULL,
       run_generation = run_generation + 1,
       state_version = state_version + 1,
       committed_input_sequence = COALESCE(sqlc.narg(committed_input_sequence), committed_input_sequence),
       failure_code = sqlc.narg(failure_code),
       failure_run_id = sqlc.narg(failure_run_id),
       closed_at = CASE WHEN sqlc.arg(state)::text = 'closed' THEN sqlc.arg(completed_at) ELSE closed_at END,
       failed_at = CASE WHEN sqlc.arg(state)::text = 'failed' THEN sqlc.arg(completed_at) ELSE failed_at END,
       expired_at = CASE WHEN sqlc.arg(state)::text = 'expired' THEN sqlc.arg(completed_at) ELSE expired_at END,
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
   SET owner_actor_id = NULL,
       ownership_generation = ownership_generation + 1,
       state_version = state_version + 1,
       last_activity_at = sqlc.arg(completed_at),
       updated_at = sqlc.arg(completed_at)
 WHERE workspaces.id = sqlc.arg(id)
   AND workspaces.org_id = sqlc.arg(org_id)
   AND workspaces.project_id = sqlc.arg(project_id)
   AND workspaces.environment_id = sqlc.arg(environment_id)
   AND workspaces.owner_actor_id = sqlc.arg(actor_id)
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
        id, public_id, org_id, project_id, environment_id,
        deployment_id, deployment_definition_id, entrypoint_kind,
        entrypoint_declared_id, cause_kind, actor_id,
        actor_start_input_sequence, actor_start_input_high_watermark,
        workspace_id, base_workspace_version_id, metadata, tags,
        queue_name, concurrency_key, queue_concurrency_limit, priority,
        queue_origin_at, queue_score_at, queued_expires_at,
        max_active_duration_ms, retry_policy, trace_id, root_span_id
    )
    SELECT sqlc.arg(run_id), sqlc.arg(public_id), actors.org_id, actors.project_id, actors.environment_id,
           definitions.deployment_id, actors.deployment_definition_id, 'actor',
           actors.actor_declared_id, 'continuation', actors.id,
           actors.committed_input_sequence, actors.next_input_sequence - 1,
           actors.workspace_id, workspaces.head_version_id,
           actors.managed_run_metadata, actors.managed_run_tags,
           actors.managed_queue_name, actors.managed_concurrency_key,
           actors.managed_queue_concurrency_limit, actors.managed_priority,
           sqlc.arg(queue_origin_at)::timestamptz,
           sqlc.arg(queue_origin_at)::timestamptz - (actors.managed_priority::double precision * interval '1 second'),
           CASE WHEN actors.managed_queued_ttl_ms IS NULL THEN NULL
                ELSE sqlc.arg(queue_origin_at)::timestamptz + (actors.managed_queued_ttl_ms::double precision * interval '1 millisecond') END,
           actors.managed_max_active_duration_ms, actors.managed_retry_policy,
           sqlc.narg(trace_id), sqlc.arg(root_span_id)
      FROM actors
      JOIN deployment_definitions AS definitions
        ON definitions.environment_id = actors.environment_id
       AND definitions.id = actors.deployment_definition_id
       AND definitions.kind = 'actor'
       AND definitions.declared_id = actors.actor_declared_id
      JOIN workspaces
        ON workspaces.id = actors.workspace_id
       AND workspaces.owner_actor_id = actors.id
       AND workspaces.owner_run_id IS NULL
       AND workspaces.head_version_id IS NOT NULL
     WHERE actors.environment_id = sqlc.arg(environment_id)
       AND actors.id = sqlc.arg(actor_id)
       AND actors.workspace_id = sqlc.arg(workspace_id)
       AND actors.current_run_id IS NULL
       AND actors.run_generation = sqlc.arg(expected_run_generation)
       AND actors.state IN ('open', 'closing')
       AND actors.manual_run_cancelled = false
       AND (actors.expires_at IS NULL OR actors.expires_at > sqlc.arg(queue_origin_at)::timestamptz)
       AND actors.committed_input_sequence < actors.next_input_sequence - 1
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
        actor_start_input_sequence, base_workspace_version_id
    )
    SELECT created_run.id, 1, 'actor', created_run.workspace_id,
           created_run.actor_start_input_sequence, created_run.base_workspace_version_id
      FROM created_run
    RETURNING run_id
), claimed_actor AS (
    UPDATE actors
       SET current_run_id = created_run.id,
           state_version = actors.state_version + 1,
           updated_at = sqlc.arg(queue_origin_at)
      FROM created_run, created_attempt
     WHERE actors.id = created_run.actor_id
       AND actors.current_run_id IS NULL
       AND actors.run_generation = sqlc.arg(expected_run_generation)
       AND created_attempt.run_id = created_run.id
    RETURNING actors.id
)
SELECT created_run.*
  FROM created_run
  JOIN claimed_actor ON claimed_actor.id = created_run.actor_id;
