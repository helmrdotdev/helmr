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
       checkpoint_due_at = sqlc.arg(checkpoint_due_at),
       updated_at = now()
 WHERE run_id = sqlc.arg(run_id)
   AND attempt_number = sqlc.arg(attempt_number)
   AND id = sqlc.arg(id)
   AND current_run_lease_id = sqlc.arg(current_run_lease_id)
   AND suspension_state = 'hot'
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
