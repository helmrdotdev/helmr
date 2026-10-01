-- name: GetTaskCompletionReplay :one
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
   AND computer_id = sqlc.arg(computer_id)
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
   AND computer_id = sqlc.arg(computer_id)
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
   AND computer_id = sqlc.arg(computer_id)
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
    computer_id,
    base_computer_disk_version_id
)
SELECT runs.id,
       sqlc.arg(number),
       'task',
       runs.computer_id,
       sqlc.arg(result_computer_disk_version_id)
  FROM runs
 WHERE runs.id = sqlc.arg(run_id)
   AND runs.computer_id = sqlc.arg(computer_id)
   AND runs.entrypoint_kind = 'task'
   AND runs.session_id IS NULL
   AND runs.status = 'running'
   AND runs.current_attempt_number = sqlc.arg(previous_attempt_number)
   AND runs.current_run_lease_id = sqlc.arg(run_lease_id)
RETURNING *;

-- name: DelayTaskRunRetry :one
UPDATE runs
   SET status = 'retry_delayed',
       base_computer_disk_version_id = sqlc.arg(result_computer_disk_version_id),
       revision = revision + 1,
       current_attempt_number = sqlc.arg(next_attempt_number),
       current_run_lease_id = NULL,
       retry_at = sqlc.arg(retry_at),
       updated_at = sqlc.arg(completed_at)
 WHERE id = sqlc.arg(id)
   AND computer_id = sqlc.arg(computer_id)
   AND entrypoint_kind = 'task'
   AND session_id IS NULL
   AND status = 'running'
   AND current_attempt_number = sqlc.arg(previous_attempt_number)
   AND current_run_lease_id = sqlc.arg(run_lease_id)
   AND active_started_at IS NULL
RETURNING *;

-- name: ReadyRunRetries :many
WITH candidates AS (
    SELECT runs.id,
           runs.environment_id,
           runs.computer_id,
           runs.current_attempt_number,
           runs.revision
      FROM runs
      JOIN run_attempts
        ON run_attempts.run_id = runs.id
       AND run_attempts.number = runs.current_attempt_number
       AND run_attempts.entrypoint_kind = runs.entrypoint_kind
       AND run_attempts.computer_id = runs.computer_id
     WHERE runs.status = 'retry_delayed'
       AND runs.retry_at <= now()
       AND runs.current_run_lease_id IS NULL
       AND run_attempts.terminal_outcome IS NULL
       AND run_attempts.terminal_at IS NULL
       AND EXISTS (SELECT 1 FROM computers c WHERE c.id=runs.computer_id AND c.status='active'
         AND c.desired_state='active' AND c.recovery_failure IS NULL
         AND c.dirty_state NOT IN ('dirty_state_lost'))
       AND NOT EXISTS (
            SELECT 1
              FROM run_leases
             WHERE run_leases.run_id = runs.id
               AND run_leases.process_reconciled_at IS NULL
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
       AND runs.computer_id = candidates.computer_id
       AND runs.current_attempt_number = candidates.current_attempt_number
       AND runs.revision = candidates.revision
       AND runs.status = 'retry_delayed'
       AND runs.current_run_lease_id IS NULL
    RETURNING runs.id,
              runs.environment_id,
              runs.computer_id,
              runs.current_attempt_number,
              runs.revision
)
SELECT readied.id,
       readied.environment_id,
       readied.computer_id,
       readied.current_attempt_number,
       readied.revision
  FROM readied;
