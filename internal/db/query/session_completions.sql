-- name: GetActorCompletionReplay :one
SELECT run_leases.terminal_request_fingerprint
  FROM run_leases
  JOIN run_attempts
    ON run_attempts.run_id = run_leases.run_id
   AND run_attempts.number = run_leases.attempt_number
   AND run_attempts.computer_id = run_leases.computer_id
 WHERE run_leases.id = sqlc.arg(run_lease_id)
   AND run_leases.lease_sequence = sqlc.arg(lease_sequence)
   AND run_leases.worker_group_id = sqlc.arg(worker_group_id)
   AND run_leases.worker_host_id = sqlc.arg(worker_host_id)
   AND run_leases.terminal_request_fingerprint IS NOT NULL
   AND run_leases.terminal_at IS NOT NULL
   AND run_attempts.entrypoint_kind = 'actor'
   AND run_attempts.terminal_session_input_sequence IS NOT NULL
   AND run_attempts.terminal_at IS NOT NULL
   AND (
       (run_leases.status = 'completed'
        AND run_leases.terminal_reason_code = 'completed'
        AND run_attempts.terminal_outcome = 'succeeded'
        AND run_attempts.terminal_reason_code = 'completed')
       OR
       (run_leases.status = 'cancelled' AND run_leases.terminal_reason_code = 'session_interrupted'
        AND run_attempts.terminal_outcome = 'cancelled' AND run_attempts.terminal_reason_code = 'session_interrupted')
       OR
       (run_leases.status = 'failed'
        AND run_leases.terminal_reason_code IN ('actor_failed', 'no_progress')
        AND run_attempts.terminal_outcome = 'failed'
        AND run_attempts.terminal_reason_code = run_leases.terminal_reason_code)
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
   AND computer_id = sqlc.arg(computer_id)
   AND entrypoint_kind = 'actor'
   AND session_input_start_sequence IS NOT NULL
   AND entrypoint_entered_at IS NOT NULL
   AND terminal_at IS NULL
RETURNING *;

-- name: FinishActorRun :one
UPDATE runs
   SET status = sqlc.arg(status),
       output = CASE
           WHEN sqlc.arg(status)::text = 'succeeded' THEN 'null'::jsonb
           ELSE NULL
       END,
       failure = sqlc.narg(failure),
       revision = revision + 1,
       current_run_lease_id = NULL,
       retry_at = NULL,
       terminal_at = sqlc.arg(completed_at),
       updated_at = sqlc.arg(completed_at)
 WHERE id = sqlc.arg(id)
   AND computer_id = sqlc.arg(computer_id)
   AND entrypoint_kind = 'actor'
   AND session_id = sqlc.arg(session_id)
   AND status = 'running'
   AND current_attempt_number = sqlc.arg(attempt_number)
   AND current_run_lease_id = sqlc.arg(run_lease_id)
   AND active_started_at IS NULL
RETURNING *;

-- name: ReconcileSessionTerminalRun :one
UPDATE sessions
   SET status = sqlc.arg(status),
       current_run_id = NULL,
       run_generation = run_generation + 1,
       revision = revision + 1,
       closed_at = CASE WHEN sqlc.arg(status)::text = 'closed' THEN sqlc.arg(completed_at) ELSE closed_at END,
       updated_at = sqlc.arg(completed_at)
 WHERE environment_id = sqlc.arg(environment_id)
   AND id = sqlc.arg(id)
   AND computer_id = sqlc.arg(computer_id)
   AND current_run_id = sqlc.arg(run_id)
   AND run_generation = sqlc.arg(expected_run_generation)
   AND status IN ('open', 'closing')
   AND active_turn_id IS NULL AND dispatch_hold_id IS NULL
RETURNING *;

-- name: CreateActorContinuationRun :one
WITH created_run AS (
    INSERT INTO runs (
        id, org_id, project_id, environment_id,
        deployment_id, deployment_definition_id, entrypoint_kind,
        entrypoint_declared_id, cause_kind, session_id,
        session_input_start_sequence, session_input_high_watermark,
        computer_id, base_computer_disk_version_id, metadata, tags,
        queue_name, concurrency_key, queue_concurrency_limit, priority,
        queue_origin_at, queue_score_at, queued_expires_at,
        max_active_duration_ms, retry_policy, trace_id, root_span_id
    )
    SELECT sqlc.arg(run_id), environments.org_id, environments.project_id, sessions.environment_id,
           definitions.deployment_id, sessions.deployment_definition_id, 'actor',
           sessions.actor_declared_id, 'continuation', sessions.id,
           sessions.committed_input_sequence, sessions.next_input_sequence - 1,
           sessions.computer_id, computers.head_disk_version_id,
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
      JOIN computers
        ON computers.id = sessions.computer_id
       AND computers.environment_id = sessions.environment_id
       AND computers.head_disk_version_id IS NOT NULL
     WHERE sessions.environment_id = sqlc.arg(environment_id)
       AND sessions.id = sqlc.arg(session_id)
       AND sessions.computer_id = sqlc.arg(computer_id)
       AND sessions.current_run_id IS NULL
       AND sessions.run_generation = sqlc.arg(expected_run_generation)
       AND sessions.status IN ('open', 'closing') AND sessions.cancel_requested_at IS NULL
       AND sessions.active_turn_id IS NULL AND sessions.dispatch_hold_id IS NULL
       AND (sessions.status = 'open' OR sessions.committed_input_sequence < sessions.close_sequence)
       AND computers.status='active' AND computers.desired_state='active'
       AND computers.deleted_at IS NULL AND computers.recovery_failure IS NULL
       AND computers.dirty_state NOT IN ('dirty_state_lost')
       AND NOT EXISTS(SELECT 1 FROM run_leases l JOIN runs r ON r.id=l.run_id
         WHERE r.session_id=sessions.id AND l.process_reconciled_at IS NULL)
	ON CONFLICT (session_id)
	    WHERE session_id IS NOT NULL
	      AND status IN ('queued', 'running', 'waiting', 'retry_delayed', 'cancel_requested')
	DO NOTHING
    RETURNING *
), created_attempt AS (
    INSERT INTO run_attempts (
        run_id, number, entrypoint_kind, computer_id,
        session_input_start_sequence, base_computer_disk_version_id
    )
    SELECT created_run.id, 1, 'actor', created_run.computer_id,
           created_run.session_input_start_sequence, created_run.base_computer_disk_version_id
      FROM created_run
    RETURNING run_id
), claimed_actor AS (
    UPDATE sessions
       SET current_run_id = created_run.id,
           revision = sessions.revision + 1,
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

-- name: FailSession :one
UPDATE sessions
   SET status = 'failed',
       failure = sqlc.arg(failure)::jsonb,
       failure_run_id = sqlc.arg(run_id)::uuid,
       failed_at = sqlc.arg(completed_at)::timestamptz,
       current_run_id = NULL, active_turn_id = NULL,
       dispatch_hold_id = NULL, dispatch_hold_reason = NULL,
       dispatch_hold_run_id = NULL, dispatch_hold_attempt_number = NULL,
       dispatch_hold_run_generation = NULL,
       committed_input_sequence = coalesce(sqlc.narg(input_sequence), committed_input_sequence),
       run_generation = run_generation + 1, revision = revision + 1,
       updated_at = sqlc.arg(completed_at)
 WHERE environment_id = sqlc.arg(environment_id) AND id = sqlc.arg(session_id)
   AND current_run_id = sqlc.arg(run_id) AND run_generation = sqlc.arg(run_generation)
   AND status IN ('open', 'closing')
   AND cancel_requested_at IS NULL
RETURNING *;
