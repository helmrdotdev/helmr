-- name: GetRunWait :one
SELECT *
  FROM run_waits
 WHERE run_id = sqlc.arg(run_id)
   AND attempt_number = sqlc.arg(attempt_number)
   AND id = sqlc.arg(id);

-- name: CompleteRunWaitCondition :one
UPDATE run_waits
   SET condition_state = 'completed',
       condition_result = sqlc.narg(condition_result),
       completed_actor_record_id = sqlc.narg(completed_actor_record_id),
       condition_terminal_at = now(),
       updated_at = now()
 WHERE run_id = sqlc.arg(run_id)
   AND attempt_number = sqlc.arg(attempt_number)
   AND id = sqlc.arg(id)
   AND condition_state = 'pending'
RETURNING *;

-- name: FailRunWaitCondition :one
UPDATE run_waits
   SET condition_state = 'failed',
       condition_reason_code = sqlc.arg(condition_reason_code),
       condition_error = sqlc.narg(condition_error),
       condition_terminal_at = now(),
       updated_at = now()
 WHERE run_id = sqlc.arg(run_id)
   AND attempt_number = sqlc.arg(attempt_number)
   AND id = sqlc.arg(id)
   AND condition_state = 'pending'
RETURNING *;

-- name: RequestRunWaitCheckpoint :one
UPDATE run_waits
   SET suspension_state = 'checkpointing',
       checkpoint_request_version = checkpoint_request_version + 1,
       suspend_checkpoint_id = sqlc.arg(suspend_checkpoint_id),
       updated_at = now()
 WHERE run_id = sqlc.arg(run_id)
   AND attempt_number = sqlc.arg(attempt_number)
   AND id = sqlc.arg(id)
   AND current_run_lease_id = sqlc.arg(current_run_lease_id)
   AND suspension_state = 'hot'
   AND condition_state = 'pending'
   AND checkpoint_due_at IS NOT NULL
   AND checkpoint_due_at <= transaction_timestamp()
RETURNING *;

-- name: BeginRunLeaseCheckpoint :one
UPDATE run_leases
   SET state = 'checkpointing',
       updated_at = transaction_timestamp()
 WHERE id = sqlc.arg(id)
   AND run_id = sqlc.arg(run_id)
   AND workspace_id = sqlc.arg(workspace_id)
   AND attempt_number = sqlc.arg(attempt_number)
   AND lease_sequence = sqlc.arg(lease_sequence)
   AND state = 'running'
   AND expires_at > transaction_timestamp()
RETURNING *;

-- name: MarkRunWaitParked :one
UPDATE run_waits
   SET suspension_state = 'parked',
       checkpoint_ack_version = sqlc.arg(checkpoint_ack_version),
       suspend_checkpoint_id = sqlc.arg(suspend_checkpoint_id),
       prior_run_lease_id = current_run_lease_id,
       current_run_lease_id = NULL,
       updated_at = now()
 WHERE run_id = sqlc.arg(run_id)
   AND attempt_number = sqlc.arg(attempt_number)
   AND workspace_id = sqlc.arg(workspace_id)
   AND id = sqlc.arg(id)
   AND suspension_state = 'checkpointing'
   AND condition_state = 'pending'
   AND checkpoint_request_version = sqlc.arg(checkpoint_ack_version)
RETURNING *;

-- name: MarkRunWaitResumePending :one
UPDATE run_waits
   SET suspension_state = 'resume_pending',
       resume_request_version = resume_request_version + 1,
       updated_at = now()
 WHERE run_id = sqlc.arg(run_id)
   AND attempt_number = sqlc.arg(attempt_number)
   AND id = sqlc.arg(id)
   AND condition_state <> 'pending'
   AND suspension_state = 'parked'
RETURNING *;

-- name: ReleaseRunResumeWait :one
UPDATE run_waits
   SET suspension_state = 'released',
       resume_ack_version = sqlc.arg(resume_request_version),
       suspension_terminal_at = transaction_timestamp(),
       updated_at = transaction_timestamp()
 WHERE id = sqlc.arg(id)
   AND environment_id = sqlc.arg(environment_id)
   AND run_id = sqlc.arg(run_id)
   AND attempt_number = sqlc.arg(attempt_number)
   AND workspace_id = sqlc.arg(workspace_id)
   AND current_run_lease_id = sqlc.arg(current_run_lease_id)
   AND suspension_state = 'resuming'
   AND CASE
           WHEN condition_state = 'completed'
                AND handoff_resume_checkpoint_id IS NOT NULL
               THEN handoff_resume_checkpoint_id
           ELSE suspend_checkpoint_id
       END = sqlc.arg(checkpoint_id)::uuid
   AND resume_attach_id = sqlc.arg(resume_attach_id)
   AND resume_request_version = sqlc.arg(resume_request_version)
   AND resume_ack_version < resume_request_version
RETURNING *;

-- name: ListPendingRunWaitTimeouts :many
SELECT *
  FROM run_waits
 WHERE condition_state = 'pending'
   AND timeout_at IS NOT NULL
   AND timeout_at <= now()
 ORDER BY timeout_at, id
 LIMIT sqlc.arg(limit_count)
 FOR UPDATE SKIP LOCKED;

-- name: RegisterActorInputRunWait :one
WITH moved_run AS (
    UPDATE runs
       SET status = 'waiting',
           state_version = state_version + 1,
           updated_at = transaction_timestamp()
     WHERE runs.id = sqlc.arg(run_id)
       AND runs.environment_id = sqlc.arg(environment_id)
       AND runs.actor_id = sqlc.arg(actor_id)
       AND runs.status = 'running'
       AND runs.state_version = sqlc.arg(expected_running_state_version)
       AND runs.current_attempt_number = sqlc.arg(attempt_number)
       AND runs.current_run_lease_id = sqlc.arg(current_run_lease_id)
       AND runs.active_started_at IS NOT NULL
       AND transaction_timestamp() < runs.active_started_at
             + ((runs.max_active_duration_ms - runs.active_elapsed_ms) * interval '1 millisecond')
    RETURNING *
)
INSERT INTO run_waits (
    id, environment_id, run_id, workspace_id, kind, timeout_at,
    idle_timeout_ms, actor_id, after_input_sequence,
    registration_request_fingerprint, expected_run_state_version, attempt_number,
    actor_speculative_input_sequence, current_run_lease_id,
    checkpoint_due_at, resume_attach_id, metadata, tags
)
SELECT sqlc.arg(id), sqlc.arg(environment_id), moved_run.id, moved_run.workspace_id,
       'actor_input', sqlc.narg(timeout_at), sqlc.arg(idle_timeout_ms),
       sqlc.arg(actor_id), sqlc.arg(after_input_sequence),
       sqlc.arg(registration_request_fingerprint), moved_run.state_version,
       sqlc.arg(attempt_number), sqlc.arg(actor_speculative_input_sequence),
       sqlc.arg(current_run_lease_id), sqlc.arg(checkpoint_due_at),
       sqlc.arg(resume_attach_id), sqlc.arg(metadata), sqlc.arg(tags)
  FROM moved_run
RETURNING *;

-- name: GetActorInputRunWaitRegistrationReplay :one
SELECT *
  FROM run_waits
 WHERE id = sqlc.arg(id)
   AND environment_id = sqlc.arg(environment_id)
   AND run_id = sqlc.arg(run_id)
   AND workspace_id = sqlc.arg(workspace_id)
   AND kind = 'actor_input'
   AND actor_id = sqlc.arg(actor_id)
   AND after_input_sequence = sqlc.arg(after_input_sequence)
   AND actor_speculative_input_sequence = sqlc.arg(actor_speculative_input_sequence)
   AND attempt_number = sqlc.arg(attempt_number)
   AND resume_attach_id = sqlc.arg(resume_attach_id)
   AND registration_request_fingerprint = sqlc.arg(registration_request_fingerprint)
   AND metadata = sqlc.arg(metadata)
   AND tags = sqlc.arg(tags)
   AND (current_run_lease_id = sqlc.arg(run_lease_id)
        OR prior_run_lease_id = sqlc.arg(run_lease_id));

-- name: GetPendingActorInputRunWait :one
SELECT *
  FROM run_waits
 WHERE environment_id = sqlc.arg(environment_id)
   AND run_id = sqlc.arg(run_id)
   AND attempt_number = sqlc.arg(attempt_number)
   AND actor_id = sqlc.arg(actor_id)
   AND kind = 'actor_input'
   AND after_input_sequence = sqlc.arg(after_input_sequence)
   AND condition_state = 'pending'
   AND suspension_state IN ('hot', 'checkpointing', 'parked')
 ORDER BY id
 LIMIT 1
 FOR UPDATE;

-- name: CompleteHotActorInputRunWait :one
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
    RETURNING state_version
)
UPDATE run_waits
   SET condition_state = 'completed',
       condition_result = sqlc.arg(condition_result),
       completed_actor_record_id = sqlc.arg(completed_actor_record_id),
       condition_terminal_at = transaction_timestamp(),
       suspension_state = 'released',
       expected_run_state_version = moved_run.state_version,
       suspension_terminal_at = transaction_timestamp(),
       updated_at = transaction_timestamp()
  FROM moved_run
 WHERE run_waits.id = sqlc.arg(id)
   AND run_waits.run_id = sqlc.arg(run_id)
   AND run_waits.condition_state = 'pending'
   AND run_waits.suspension_state = 'hot'
   AND run_waits.expected_run_state_version = sqlc.arg(expected_run_state_version)
   AND run_waits.current_run_lease_id = sqlc.arg(current_run_lease_id)
RETURNING run_waits.*;

-- name: CompleteCheckpointingActorInputRunWait :one
UPDATE run_waits
   SET condition_state = 'completed',
       condition_result = sqlc.arg(condition_result),
       completed_actor_record_id = sqlc.arg(completed_actor_record_id),
       condition_terminal_at = transaction_timestamp(),
       updated_at = transaction_timestamp()
 WHERE id = sqlc.arg(id)
   AND run_id = sqlc.arg(run_id)
   AND condition_state = 'pending'
   AND suspension_state = 'checkpointing'
   AND expected_run_state_version = sqlc.arg(expected_run_state_version)
   AND current_run_lease_id = sqlc.arg(current_run_lease_id)
RETURNING *;

-- name: CompleteParkedActorInputRunWait :one
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
    RETURNING state_version
)
UPDATE run_waits
   SET condition_state = 'completed',
       condition_result = sqlc.arg(condition_result),
       completed_actor_record_id = sqlc.arg(completed_actor_record_id),
       condition_terminal_at = transaction_timestamp(),
       suspension_state = 'resume_pending',
       resume_request_version = run_waits.resume_request_version + 1,
       expected_run_state_version = moved_run.state_version,
       updated_at = transaction_timestamp()
  FROM moved_run
 WHERE run_waits.id = sqlc.arg(id)
   AND run_waits.run_id = sqlc.arg(run_id)
   AND run_waits.condition_state = 'pending'
   AND run_waits.suspension_state = 'parked'
   AND run_waits.expected_run_state_version = sqlc.arg(expected_run_state_version)
   AND run_waits.current_run_lease_id IS NULL
   AND run_waits.prior_run_lease_id = sqlc.arg(prior_run_lease_id)
   AND run_waits.suspend_checkpoint_id = sqlc.arg(suspend_checkpoint_id)
RETURNING run_waits.*;

-- name: ListPendingActorInputWaitTimeouts :many
SELECT *
  FROM run_waits
 WHERE kind = 'actor_input'
   AND condition_state = 'pending'
   AND timeout_at IS NOT NULL
   AND timeout_at <= transaction_timestamp()
 ORDER BY timeout_at, id
 LIMIT sqlc.arg(limit_count);

-- name: FailHotRunWait :one
WITH moved_run AS (
    UPDATE runs
       SET status = 'running', state_version = state_version + 1,
           updated_at = transaction_timestamp()
     WHERE runs.id = sqlc.arg(run_id)
       AND runs.status = 'waiting'
       AND runs.state_version = sqlc.arg(expected_run_state_version)
       AND runs.current_attempt_number = sqlc.arg(attempt_number)
       AND runs.current_run_lease_id = sqlc.arg(current_run_lease_id)
    RETURNING state_version
)
UPDATE run_waits
   SET condition_state = 'failed', condition_reason_code = sqlc.arg(reason_code),
       condition_error = sqlc.arg(condition_error), condition_terminal_at = transaction_timestamp(),
       suspension_state = 'released', expected_run_state_version = moved_run.state_version,
       suspension_terminal_at = transaction_timestamp(), updated_at = transaction_timestamp()
  FROM moved_run
 WHERE run_waits.id = sqlc.arg(id) AND run_waits.run_id = sqlc.arg(run_id)
   AND run_waits.condition_state = 'pending' AND run_waits.suspension_state = 'hot'
   AND run_waits.expected_run_state_version = sqlc.arg(expected_run_state_version)
   AND run_waits.current_run_lease_id = sqlc.arg(current_run_lease_id)
RETURNING run_waits.*;

-- name: FailCheckpointingRunWait :one
UPDATE run_waits
   SET condition_state = 'failed', condition_reason_code = sqlc.arg(reason_code),
       condition_error = sqlc.arg(condition_error), condition_terminal_at = transaction_timestamp(),
       updated_at = transaction_timestamp()
 WHERE id = sqlc.arg(id) AND run_id = sqlc.arg(run_id)
   AND condition_state = 'pending' AND suspension_state = 'checkpointing'
   AND expected_run_state_version = sqlc.arg(expected_run_state_version)
   AND current_run_lease_id = sqlc.arg(current_run_lease_id)
RETURNING *;

-- name: FailParkedRunWait :one
WITH moved_run AS (
    UPDATE runs
       SET status = 'queued', state_version = state_version + 1,
           updated_at = transaction_timestamp()
     WHERE runs.id = sqlc.arg(run_id)
       AND runs.status = 'waiting'
       AND runs.state_version = sqlc.arg(expected_run_state_version)
       AND runs.current_attempt_number = sqlc.arg(attempt_number)
       AND runs.current_run_lease_id IS NULL
    RETURNING state_version
)
UPDATE run_waits
   SET condition_state = 'failed', condition_reason_code = sqlc.arg(reason_code),
       condition_error = sqlc.arg(condition_error), condition_terminal_at = transaction_timestamp(),
       suspension_state = 'resume_pending', resume_request_version = run_waits.resume_request_version + 1,
       expected_run_state_version = moved_run.state_version, updated_at = transaction_timestamp()
  FROM moved_run
 WHERE run_waits.id = sqlc.arg(id) AND run_waits.run_id = sqlc.arg(run_id)
   AND run_waits.condition_state = 'pending' AND run_waits.suspension_state = 'parked'
   AND run_waits.expected_run_state_version = sqlc.arg(expected_run_state_version)
   AND run_waits.current_run_lease_id IS NULL
   AND run_waits.prior_run_lease_id = sqlc.arg(prior_run_lease_id)
   AND run_waits.suspend_checkpoint_id = sqlc.arg(suspend_checkpoint_id)
RETURNING run_waits.*;
