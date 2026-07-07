-- name: EnsureSessionContinuationRequestForStreamRecord :one
INSERT INTO session_continuation_requests (
    id,
    org_id,
    worker_group_id,
    project_id,
    environment_id,
    session_id,
    stream_record_id,
    stream_id
)
SELECT
    sqlc.arg(id),
    stream_records.org_id,
    stream_records.worker_group_id,
    stream_records.project_id,
    stream_records.environment_id,
    stream_records.session_id,
    stream_records.id,
    stream_records.stream_id
  FROM stream_records
  JOIN streams
    ON streams.org_id = stream_records.org_id
   AND streams.project_id = stream_records.project_id
   AND streams.environment_id = stream_records.environment_id
   AND streams.id = stream_records.stream_id
   AND streams.worker_group_id = stream_records.worker_group_id
   AND streams.session_id = stream_records.session_id
  JOIN sessions
    ON sessions.org_id = stream_records.org_id
   AND sessions.project_id = stream_records.project_id
   AND sessions.environment_id = stream_records.environment_id
   AND sessions.id = stream_records.session_id
   AND sessions.worker_group_id = stream_records.worker_group_id
 WHERE stream_records.org_id = sqlc.arg(org_id)
   AND stream_records.project_id = sqlc.arg(project_id)
   AND stream_records.environment_id = sqlc.arg(environment_id)
   AND stream_records.session_id = sqlc.arg(session_id)
   AND stream_records.stream_id = sqlc.arg(stream_id)
   AND stream_records.id = sqlc.arg(stream_record_id)
ON CONFLICT (org_id, project_id, environment_id, stream_record_id)
DO UPDATE SET updated_at = session_continuation_requests.updated_at
RETURNING *;

-- name: GetSessionContinuationRequest :one
SELECT *
 FROM session_continuation_requests
 WHERE org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND id = sqlc.arg(id);

-- name: ClaimDueSessionContinuationRequests :many
WITH eligible AS (
    SELECT id
     FROM session_continuation_requests
     WHERE status IN ('accepted', 'claimed')
       AND worker_group_id = sqlc.arg(worker_group_id)
       AND (
           status = 'accepted'
           OR claim_expires_at IS NULL
           OR claim_expires_at <= now()
       )
       AND next_attempt_at <= now()
       AND (
           sqlc.narg(org_id)::uuid IS NULL
           OR org_id = sqlc.narg(org_id)
       )
       AND (
           sqlc.narg(project_id)::uuid IS NULL
           OR project_id = sqlc.narg(project_id)
       )
       AND (
           sqlc.narg(environment_id)::uuid IS NULL
           OR environment_id = sqlc.narg(environment_id)
       )
       AND (
           sqlc.narg(session_id)::uuid IS NULL
           OR session_id = sqlc.narg(session_id)
       )
     ORDER BY next_attempt_at ASC, created_at ASC, id ASC
     LIMIT sqlc.arg(limit_count)
     FOR UPDATE SKIP LOCKED
)
UPDATE session_continuation_requests
   SET status = 'claimed',
       attempts = attempts + 1,
       claimed_at = now(),
       claim_expires_at = now() + sqlc.arg(claim_ttl)::interval,
       claim_owner = sqlc.arg(claim_owner),
       updated_at = now()
 FROM eligible
 WHERE session_continuation_requests.id = eligible.id
   AND session_continuation_requests.worker_group_id = sqlc.arg(worker_group_id)
RETURNING session_continuation_requests.*;

-- name: ReleaseSessionContinuationRequestForRetry :one
UPDATE session_continuation_requests
   SET status = 'accepted',
       next_attempt_at = now() + sqlc.arg(retry_after)::interval,
       status_reason = '',
       last_error_code = sqlc.arg(last_error_code),
       last_error_message = sqlc.arg(last_error_message),
       claimed_at = NULL,
       claim_expires_at = NULL,
       claim_owner = '',
       updated_at = now()
 WHERE org_id = sqlc.arg(org_id)
	   AND worker_group_id = sqlc.arg(worker_group_id)
	   AND project_id = sqlc.arg(project_id)
	   AND environment_id = sqlc.arg(environment_id)
	   AND id = sqlc.arg(id)
	   AND status = 'claimed'
	   AND claim_owner = sqlc.arg(claim_owner)
	RETURNING *;

-- name: MarkSessionContinuationRequestCreated :one
UPDATE session_continuation_requests
   SET status = 'created',
       status_reason = '',
       created_run_id = sqlc.arg(created_run_id),
       consumed_by_run_id = NULL,
       last_error_code = '',
       last_error_message = '',
       claimed_at = NULL,
       claim_expires_at = NULL,
       claim_owner = '',
       updated_at = now()
 WHERE org_id = sqlc.arg(org_id)
	   AND worker_group_id = sqlc.arg(worker_group_id)
	   AND project_id = sqlc.arg(project_id)
	   AND environment_id = sqlc.arg(environment_id)
	   AND id = sqlc.arg(id)
	   AND status = 'claimed'
	   AND claim_owner = sqlc.arg(claim_owner)
	RETURNING *;

-- name: MarkSessionContinuationRequestSkipped :one
UPDATE session_continuation_requests
   SET status = 'skipped',
       status_reason = sqlc.arg(reason),
       last_error_code = '',
       last_error_message = '',
       claimed_at = NULL,
       claim_expires_at = NULL,
       claim_owner = '',
       updated_at = now()
 WHERE org_id = sqlc.arg(org_id)
	   AND worker_group_id = sqlc.arg(worker_group_id)
	   AND project_id = sqlc.arg(project_id)
	   AND environment_id = sqlc.arg(environment_id)
	   AND id = sqlc.arg(id)
	   AND status = 'claimed'
	   AND claim_owner = sqlc.arg(claim_owner)
	RETURNING *;

-- name: MarkSessionContinuationRequestConsumedByActiveRun :one
WITH target AS MATERIALIZED (
    SELECT *
     FROM session_continuation_requests
     WHERE session_continuation_requests.org_id = sqlc.arg(org_id)
       AND session_continuation_requests.worker_group_id = sqlc.arg(worker_group_id)
       AND session_continuation_requests.project_id = sqlc.arg(project_id)
       AND session_continuation_requests.environment_id = sqlc.arg(environment_id)
       AND session_continuation_requests.stream_record_id = sqlc.arg(stream_record_id)
       AND session_continuation_requests.status IN ('accepted', 'claimed', 'created')
       AND (
           session_continuation_requests.status <> 'created'
           OR session_continuation_requests.created_run_id IS DISTINCT FROM sqlc.arg(active_run_id)
       )
     FOR UPDATE
),
cancelled_runs AS (
    UPDATE runs
       SET status = 'cancelled',
           execution_status = CASE
             WHEN runs.execution_status = 'executing' THEN 'pending_cancel'::run_execution_status
             ELSE 'finished'::run_execution_status
           END,
           terminal_outcome = 'cancelled',
           current_run_lease_id = CASE
             WHEN runs.execution_status = 'executing' THEN runs.current_run_lease_id
             ELSE NULL
	           END,
	           error_message = 'stream record consumed by active run',
	           dispatch_generation = CASE
	             WHEN runs.execution_status = 'executing' THEN runs.dispatch_generation
	             ELSE runs.dispatch_generation + 1
	           END,
	           state_version = runs.state_version + 1,
           finished_at = CASE
             WHEN runs.execution_status = 'executing' THEN runs.finished_at
             ELSE COALESCE(runs.finished_at, now())
           END,
           updated_at = now()
      FROM target
     WHERE target.status = 'created'
       AND target.created_run_id IS NOT NULL
       AND runs.org_id = target.org_id
       AND runs.worker_group_id = target.worker_group_id
       AND runs.project_id = target.project_id
       AND runs.environment_id = target.environment_id
       AND runs.id = target.created_run_id
       AND runs.status NOT IN ('succeeded', 'failed', 'cancelled', 'expired')
    RETURNING runs.*
),
ended_session_runs AS (
    UPDATE session_runs
       SET ended_at = COALESCE(session_runs.ended_at, now())
      FROM cancelled_runs
     WHERE session_runs.org_id = cancelled_runs.org_id
       AND session_runs.worker_group_id = cancelled_runs.worker_group_id
       AND session_runs.project_id = cancelled_runs.project_id
       AND session_runs.environment_id = cancelled_runs.environment_id
       AND session_runs.session_id = cancelled_runs.session_id
       AND session_runs.run_id = cancelled_runs.id
       AND cancelled_runs.execution_status <> 'pending_cancel'
    RETURNING session_runs.id
),
restored_session_current AS (
    UPDATE sessions
       SET current_run_id = sqlc.arg(active_run_id),
           updated_at = now()
      FROM target
     WHERE sessions.org_id = target.org_id
       AND sessions.worker_group_id = target.worker_group_id
       AND sessions.project_id = target.project_id
       AND sessions.environment_id = target.environment_id
       AND sessions.id = target.session_id
       AND sessions.current_run_id = target.created_run_id
       AND target.status = 'created'
    RETURNING sessions.id
)
UPDATE session_continuation_requests
   SET status = 'skipped',
       status_reason = 'consumed_by_active_run',
       consumed_by_run_id = sqlc.arg(active_run_id),
       last_error_code = '',
       last_error_message = '',
       claimed_at = NULL,
       claim_expires_at = NULL,
       claim_owner = '',
       updated_at = now()
  FROM target
 WHERE session_continuation_requests.org_id = target.org_id
   AND session_continuation_requests.worker_group_id = target.worker_group_id
   AND session_continuation_requests.project_id = target.project_id
   AND session_continuation_requests.environment_id = target.environment_id
   AND session_continuation_requests.id = target.id
RETURNING session_continuation_requests.*;

-- name: MarkSessionContinuationRequestFailed :one
UPDATE session_continuation_requests
   SET status = 'failed',
       status_reason = sqlc.arg(reason),
       last_error_code = '',
       last_error_message = '',
       claimed_at = NULL,
       claim_expires_at = NULL,
       claim_owner = '',
       updated_at = now()
 WHERE org_id = sqlc.arg(org_id)
	   AND worker_group_id = sqlc.arg(worker_group_id)
	   AND project_id = sqlc.arg(project_id)
	   AND environment_id = sqlc.arg(environment_id)
	   AND id = sqlc.arg(id)
	   AND status = 'claimed'
	   AND claim_owner = sqlc.arg(claim_owner)
	RETURNING *;
