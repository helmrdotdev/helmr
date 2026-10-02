-- name: GetRunWait :one
SELECT *
  FROM run_waits
 WHERE run_id = sqlc.arg(run_id)
   AND attempt_number = sqlc.arg(attempt_number)
   AND id = sqlc.arg(id);

-- name: GetTokenWaitRegistrationReplay :one
-- Presence and exact identity must use one statement snapshot, including the
-- read-only replay before Run locks.
WITH replay AS (
    SELECT run_waits.id AS wait_id,
           runs.revision AS run_revision,
           run_waits.condition_status,
           run_waits.suspension_status,
           run_waits.condition_result,
           run_waits.condition_reason_code
      FROM run_waits
      JOIN runs
        ON runs.environment_id = run_waits.environment_id
       AND runs.id = run_waits.run_id
      JOIN run_leases
        ON run_leases.id = sqlc.arg(run_lease_id)
       AND run_leases.run_id = run_waits.run_id
       AND run_leases.attempt_number = run_waits.attempt_number
       AND run_leases.computer_id = run_waits.computer_id
     WHERE run_waits.id = sqlc.arg(wait_id)
       AND run_waits.token_id = sqlc.arg(token_id)
       AND run_waits.kind = 'token'
       AND run_waits.turn_id IS NOT DISTINCT FROM sqlc.narg(turn_id)
       AND run_waits.turn_run_generation IS NOT DISTINCT FROM sqlc.narg(turn_run_generation)
       AND (runs.session_id IS NULL OR EXISTS (SELECT 1 FROM sessions s WHERE s.id=runs.session_id AND s.current_run_id=runs.id AND s.dispatch_hold_id IS NULL AND (run_waits.turn_id IS NULL AND s.active_turn_id IS NULL OR s.active_turn_id=run_waits.turn_id AND s.run_generation=run_waits.turn_run_generation)))
       AND run_waits.registration_request_fingerprint
           = sqlc.arg(request_fingerprint)::text
       AND (
           run_waits.current_run_lease_id = sqlc.arg(run_lease_id)
           OR run_waits.prior_run_lease_id = sqlc.arg(run_lease_id)
       )
       AND run_waits.metadata = sqlc.arg(metadata)::jsonb
       AND run_waits.tags = sqlc.arg(tags)::text[]
       AND run_leases.lease_sequence = sqlc.arg(lease_sequence)
       AND run_leases.worker_group_id = sqlc.arg(worker_group_id)
       AND run_leases.worker_host_id = sqlc.arg(worker_host_id)
       AND run_leases.worker_epoch = sqlc.arg(worker_epoch)
)
SELECT addressed.id AS wait_id,
       (replay.wait_id IS NOT NULL)::boolean AS matches,
       replay.run_revision,
       replay.condition_status,
       replay.suspension_status,
       replay.condition_result,
       replay.condition_reason_code
  FROM run_waits AS addressed
  LEFT JOIN replay ON replay.wait_id = addressed.id
 WHERE addressed.id = sqlc.arg(wait_id);

-- name: GetTokenWaitRegistrationLocator :one
SELECT runs.computer_id,
       runs.session_id,
       runs.org_id,
       runs.project_id
  FROM runs
  JOIN computers
    ON computers.environment_id = runs.environment_id
   AND computers.id = runs.computer_id
 WHERE runs.environment_id = sqlc.arg(environment_id)
   AND runs.id = sqlc.arg(run_id);

-- name: LockTokenWaitSession :one
SELECT *
  FROM sessions
 WHERE id = sqlc.arg(session_id)
 FOR UPDATE;

-- name: LockTokenWaitComputer :one
SELECT * FROM computers WHERE id=sqlc.arg(computer_id) AND environment_id=sqlc.arg(environment_id) FOR UPDATE;

-- name: LockTokenWaitAttempt :one
SELECT entrypoint_kind, session_input_start_sequence, terminal_at
  FROM run_attempts
 WHERE run_id = sqlc.arg(run_id)
   AND number = sqlc.arg(attempt_number)
   AND computer_id = sqlc.arg(computer_id)
 FOR UPDATE;

-- name: LockTokenWaitRunLease :one
SELECT run_leases.status
  FROM run_leases
 WHERE run_leases.id = sqlc.arg(id)
   AND run_leases.run_id = sqlc.arg(run_id)
   AND run_leases.attempt_number = sqlc.arg(attempt_number)
   AND run_leases.computer_id = sqlc.arg(computer_id)
   AND run_leases.lease_sequence = sqlc.arg(lease_sequence)
   AND run_leases.worker_group_id = sqlc.arg(worker_group_id)
   AND run_leases.worker_host_id = sqlc.arg(worker_host_id)
   AND run_leases.worker_epoch = sqlc.arg(worker_epoch)
   AND run_leases.computer_instance_id = sqlc.arg(computer_instance_id)
   AND EXISTS (SELECT 1 FROM computer_instances AS instance
                WHERE instance.id = run_leases.computer_instance_id
                  AND instance.vm_platform_id = sqlc.arg(vm_platform_id)
                  AND instance.writer_generation=run_leases.writer_generation
                  AND instance.writer_expires_at>clock_timestamp() AND instance.reclaimed_at IS NULL
                  AND instance.desired_state='ready' AND instance.observed_state='ready'
                  AND instance.mount_state='mounted' AND instance.admission_state IN ('open','draining'))
   AND run_leases.region_id = sqlc.arg(region_id)
   AND run_leases.status = 'running'
   AND run_leases.expires_at > clock_timestamp()
 FOR UPDATE OF run_leases;

-- name: RegisterTokenWait :one
WITH moved_run AS (
    UPDATE runs
       SET status = 'waiting',
           revision = revision + 1,
           updated_at = transaction_timestamp()
     WHERE runs.id = sqlc.arg(run_id)
       AND runs.status = 'running'
       AND runs.revision = sqlc.arg(expected_running_revision)::bigint
       AND runs.current_attempt_number = sqlc.arg(attempt_number)
       AND runs.current_run_lease_id = sqlc.arg(current_run_lease_id)
       AND runs.active_started_at IS NOT NULL
       AND transaction_timestamp() < runs.active_started_at
             + (
                 (runs.max_active_duration_ms - runs.active_elapsed_ms)
                 * interval '1 millisecond'
             )
    RETURNING runs.*
)
INSERT INTO run_waits (
    id, environment_id, run_id, computer_id, kind, timeout_at,
    idle_timeout_ms, token_id, token_registration_run_revision,
    registration_request_fingerprint, expected_run_revision, attempt_number,
    current_run_lease_id,
    metadata, tags
)
SELECT sqlc.arg(wait_id),
       sqlc.arg(environment_id),
       moved_run.id,
       moved_run.computer_id,
       'token',
       sqlc.narg(timeout_at),
       sqlc.narg(idle_timeout_ms),
       sqlc.arg(token_id),
       sqlc.arg(expected_running_revision)::bigint,
       sqlc.arg(request_fingerprint)::text,
       moved_run.revision,
       sqlc.arg(attempt_number),
       sqlc.arg(current_run_lease_id),
       sqlc.arg(metadata)::jsonb,
       sqlc.arg(tags)::text[]
  FROM moved_run
RETURNING run_waits.*;

-- name: LockTokenWaitCondition :one
SELECT status, result
  FROM tokens
 WHERE environment_id = sqlc.arg(environment_id)
   AND id = sqlc.arg(token_id)
 FOR UPDATE;

-- name: GetTokenWaitLocator :one
SELECT run_waits.id AS wait_id,
       run_waits.run_id,
       run_waits.computer_id,
       run_waits.attempt_number,
       runs.session_id
  FROM run_waits
  JOIN runs
    ON runs.environment_id = run_waits.environment_id
   AND runs.id = run_waits.run_id
  JOIN computers
    ON computers.id = run_waits.computer_id
 WHERE run_waits.id = sqlc.arg(wait_id)
   AND run_waits.environment_id = sqlc.arg(environment_id)
   AND run_waits.run_id = sqlc.arg(run_id)
   AND run_waits.token_id = sqlc.arg(token_id)
   AND run_waits.kind = 'token'
   AND (
       run_waits.condition_status = 'pending'
       OR run_waits.suspension_status = 'checkpointing'
   );

-- name: LockTokenWaitRun :one
SELECT * FROM runs WHERE environment_id=sqlc.arg(environment_id) AND id=sqlc.arg(run_id) FOR UPDATE;

-- name: LockEnclosingRunWaits :many
SELECT id
  FROM run_waits
 WHERE child_run_id = sqlc.arg(run_id)
   AND suspension_status IN (
       'hot',
       'checkpointing',
       'parked',
       'resume_pending',
       'resuming'
   )
 ORDER BY id
 FOR UPDATE;

-- name: LockTokenWait :one
SELECT id,
       run_id,
       computer_id,
       kind,
       condition_status,
       suspension_status,
       expected_run_revision,
       attempt_number,
       current_run_lease_id,
       prior_run_lease_id,
       suspend_checkpoint_id,
       timeout_at,
       coalesce(
           timeout_at <= transaction_timestamp(),
           false
       )::bool AS timed_out
  FROM run_waits
 WHERE id = sqlc.arg(wait_id)
   AND environment_id = sqlc.arg(environment_id)
   AND run_id = sqlc.arg(run_id)
   AND computer_id = sqlc.arg(computer_id)
   AND attempt_number = sqlc.arg(attempt_number)
   AND token_id = sqlc.arg(token_id)
   AND kind = 'token'
   AND (condition_status = 'pending' OR suspension_status = 'checkpointing')
 FOR UPDATE;

-- name: ResolveHotTokenWait :one
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
       condition_reason_code = sqlc.narg(reason_code)::text,
       condition_error = sqlc.arg(condition_error)::jsonb,
       condition_terminal_at = transaction_timestamp(),
       suspension_status = 'released',
       expected_run_revision = moved_run.revision,
       suspension_terminal_at = transaction_timestamp(),
       updated_at = transaction_timestamp()
  FROM moved_run
 WHERE run_waits.id = sqlc.arg(wait_id)
   AND run_waits.run_id = sqlc.arg(run_id)
   AND run_waits.condition_status = 'pending'
   AND run_waits.suspension_status = 'hot'
   AND run_waits.expected_run_revision
       = sqlc.arg(expected_run_revision)
   AND run_waits.current_run_lease_id = sqlc.arg(current_run_lease_id)
RETURNING run_waits.id;

-- name: ResolveCheckpointingTokenWait :one
UPDATE run_waits
   SET condition_status = sqlc.arg(condition_status)::text,
       condition_result = sqlc.arg(condition_result)::jsonb,
       condition_reason_code = sqlc.narg(reason_code)::text,
       condition_error = sqlc.arg(condition_error)::jsonb,
       condition_terminal_at = transaction_timestamp(),
       updated_at = transaction_timestamp()
 WHERE id = sqlc.arg(wait_id)
   AND run_id = sqlc.arg(run_id)
   AND condition_status = 'pending'
   AND suspension_status = 'checkpointing'
   AND expected_run_revision = sqlc.arg(expected_run_revision)
   AND current_run_lease_id = sqlc.arg(current_run_lease_id)
RETURNING id;

-- name: ResolveParkedTokenWait :one
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
),
resolved_wait AS (
    UPDATE run_waits
       SET condition_status = sqlc.arg(condition_status)::text,
           condition_result = sqlc.arg(condition_result)::jsonb,
           condition_reason_code = sqlc.narg(reason_code)::text,
           condition_error = sqlc.arg(condition_error)::jsonb,
           condition_terminal_at = transaction_timestamp(),
           suspension_status = 'resume_pending',
           expected_run_revision = moved_run.revision,
           updated_at = transaction_timestamp()
      FROM moved_run
     WHERE run_waits.id = sqlc.arg(wait_id)
       AND run_waits.run_id = sqlc.arg(run_id)
       AND run_waits.condition_status = 'pending'
       AND run_waits.suspension_status = 'parked'
       AND run_waits.expected_run_revision
           = sqlc.arg(expected_run_revision)
       AND run_waits.current_run_lease_id IS NULL
       AND run_waits.prior_run_lease_id = sqlc.arg(prior_run_lease_id)
       AND run_waits.suspend_checkpoint_id = sqlc.arg(suspend_checkpoint_id)
    RETURNING run_waits.id,
              run_waits.environment_id,
              run_waits.run_id,
              run_waits.computer_id
)
SELECT id FROM resolved_wait;

-- name: ListTokenWaitCandidates :many
SELECT run_waits.id AS wait_id, run_waits.run_id
  FROM run_waits
 WHERE run_waits.environment_id = sqlc.arg(environment_id)
   AND token_id = sqlc.arg(token_id)
   AND (condition_status = 'pending' OR suspension_status = 'checkpointing')
   AND NOT EXISTS(SELECT 1 FROM runs r JOIN sessions s ON s.id=r.session_id WHERE r.id=run_waits.run_id AND (s.dispatch_hold_id IS NOT NULL OR s.current_run_id IS DISTINCT FROM r.id))
 ORDER BY token_id,
          CASE condition_status
              WHEN 'pending' THEN 0
              WHEN 'completed' THEN 1
              WHEN 'failed' THEN 2
              WHEN 'cancelled' THEN 3
          END,
          run_waits.id
 LIMIT sqlc.arg(row_limit);

-- name: ListTimedOutTokenWaitCandidates :many
SELECT run_waits.id AS wait_id, run_waits.run_id, run_waits.environment_id, run_waits.token_id
  FROM run_waits
 WHERE kind = 'token'
   AND condition_status = 'pending'
   AND timeout_at IS NOT NULL
   AND timeout_at <= transaction_timestamp()
   AND NOT EXISTS(SELECT 1 FROM runs r JOIN sessions s ON s.id=r.session_id WHERE r.id=run_waits.run_id AND (s.dispatch_hold_id IS NOT NULL OR s.current_run_id IS DISTINCT FROM r.id))
 ORDER BY timeout_at, run_waits.id
 LIMIT sqlc.arg(row_limit);

-- name: GetChildCallRunWaitReplay :one
SELECT *
  FROM run_waits
 WHERE environment_id = sqlc.arg(environment_id)
   AND run_id = sqlc.arg(run_id)
   AND attempt_number = sqlc.arg(attempt_number)
   AND id = sqlc.arg(id)
   AND kind = 'child'
   AND child_run_id = sqlc.arg(child_run_id)
   AND kind = 'child'
   AND child_claim_id = sqlc.arg(child_claim_id)
   AND registration_request_fingerprint = sqlc.arg(registration_request_fingerprint);

-- name: RegisterChildCall :one
WITH selected_child AS MATERIALIZED (
    SELECT child.id
      FROM runs AS child
     WHERE child.id = sqlc.arg(child_run_id)
       AND child.environment_id = sqlc.arg(environment_id)
       AND child.parent_run_id = sqlc.arg(run_id)
       AND child.parent_owns_lifecycle IS TRUE
       AND child.computer_id = sqlc.arg(child_computer_id)
), moved_run AS (
    UPDATE runs
       SET status = 'waiting',
           revision = revision + 1,
           updated_at = transaction_timestamp()
     WHERE runs.id = sqlc.arg(run_id)
       AND runs.environment_id = sqlc.arg(environment_id)
       AND runs.status = 'running'
       AND runs.revision = sqlc.arg(expected_running_revision)
       AND runs.current_attempt_number = sqlc.arg(attempt_number)
       AND runs.current_run_lease_id = sqlc.arg(current_run_lease_id)
       AND runs.active_started_at IS NOT NULL
       AND EXISTS (SELECT 1 FROM selected_child)
    RETURNING *
)
INSERT INTO run_waits (
    id, environment_id, run_id, computer_id, kind,
    child_run_id, child_target_declared_id,
    child_claim_id, child_request, registration_request_fingerprint,
    expected_run_revision, attempt_number,
    current_run_lease_id,
    metadata, tags
)
SELECT sqlc.arg(id), moved_run.environment_id, moved_run.id, moved_run.computer_id,
       'child', sqlc.arg(child_run_id), sqlc.arg(child_target_declared_id),
       sqlc.arg(child_claim_id), sqlc.arg(child_request),
       sqlc.arg(registration_request_fingerprint), moved_run.revision,
       sqlc.arg(attempt_number), sqlc.arg(current_run_lease_id),
       '{}'::jsonb, '{}'::text[]
  FROM moved_run
RETURNING *;

-- name: RegisterResolvedChildCall :one
INSERT INTO run_waits (
    id, environment_id, run_id, computer_id, kind,
    condition_status, child_run_id,
    child_target_declared_id, child_claim_id, child_request,
    condition_result, condition_terminal_at, suspension_status,
    registration_request_fingerprint, expected_run_revision,
    attempt_number, current_run_lease_id,
    suspension_terminal_at, metadata, tags
)
SELECT sqlc.arg(id), parent.environment_id, parent.id, parent.computer_id,
       'child', 'completed', child.id,
       sqlc.arg(child_target_declared_id), sqlc.arg(child_claim_id),
       sqlc.arg(child_request), sqlc.arg(condition_result),
       transaction_timestamp(), 'released',
       sqlc.arg(registration_request_fingerprint), parent.revision,
       sqlc.arg(attempt_number), sqlc.arg(current_run_lease_id), transaction_timestamp(), '{}'::jsonb, '{}'::text[]
  FROM runs AS parent
  JOIN runs AS child
    ON child.environment_id = parent.environment_id
   AND child.parent_run_id = parent.id
   AND child.parent_owns_lifecycle IS TRUE
   AND child.id = sqlc.arg(child_run_id)
 WHERE parent.environment_id = sqlc.arg(environment_id)
   AND parent.id = sqlc.arg(run_id)
   AND parent.status = 'running'
   AND parent.revision = sqlc.arg(expected_running_revision)
   AND parent.current_attempt_number = sqlc.arg(attempt_number)
   AND parent.current_run_lease_id = sqlc.arg(current_run_lease_id)
   AND child.status IN ('succeeded', 'failed', 'cancelled', 'expired', 'system_failed')
RETURNING *;

-- name: LockParentOwnedChildWait :one
SELECT run_waits.*
  FROM runs AS parent
  JOIN run_waits
    ON run_waits.environment_id = parent.environment_id
   AND run_waits.run_id = parent.id
 WHERE parent.environment_id = sqlc.arg(environment_id)
   AND parent.id = sqlc.arg(parent_run_id)
   AND run_waits.child_run_id = sqlc.arg(child_run_id)
   AND run_waits.kind = 'child'
   AND EXISTS (
       SELECT 1 FROM runs AS child
        WHERE child.id = run_waits.child_run_id
          AND child.parent_run_id = parent.id
          AND child.parent_owns_lifecycle IS TRUE
   )
   AND run_waits.condition_status = 'pending'
   AND run_waits.suspension_status IN ('hot', 'checkpointing', 'parked')
 ORDER BY run_waits.created_at DESC, run_waits.id DESC
 LIMIT 1
 FOR UPDATE OF parent, run_waits;

-- name: CompleteHotChildRunWait :one
WITH moved_run AS (
    UPDATE runs
       SET status = 'running',
           revision = revision + 1,
           updated_at = transaction_timestamp()
     WHERE runs.id = sqlc.arg(run_id)
       AND runs.environment_id = sqlc.arg(environment_id)
       AND runs.status = 'waiting'
       AND runs.revision = sqlc.arg(expected_run_revision)
       AND runs.current_attempt_number = sqlc.arg(attempt_number)
       AND runs.current_run_lease_id = sqlc.arg(current_run_lease_id)
    RETURNING revision
)
UPDATE run_waits
   SET condition_status = 'completed',
       condition_result = sqlc.arg(condition_result),
       condition_terminal_at = transaction_timestamp(),
       suspension_status = 'released',
       expected_run_revision = moved_run.revision,
       suspension_terminal_at = transaction_timestamp(),
       updated_at = transaction_timestamp()
  FROM moved_run
 WHERE run_waits.id = sqlc.arg(id)
   AND run_waits.run_id = sqlc.arg(run_id)
   AND run_waits.child_run_id = sqlc.arg(child_run_id)
   AND run_waits.condition_status = 'pending'
   AND run_waits.suspension_status = 'hot'
   AND run_waits.expected_run_revision = sqlc.arg(expected_run_revision)
   AND run_waits.current_run_lease_id = sqlc.arg(current_run_lease_id)
RETURNING run_waits.*;

-- name: CompleteCheckpointingChildRunWait :one
UPDATE run_waits
   SET condition_status = 'completed',
       condition_result = sqlc.arg(condition_result),
       condition_terminal_at = transaction_timestamp(),
       updated_at = transaction_timestamp()
 WHERE id = sqlc.arg(id)
   AND run_id = sqlc.arg(run_id)
   AND child_run_id = sqlc.arg(child_run_id)
   AND condition_status = 'pending'
   AND suspension_status = 'checkpointing'
   AND expected_run_revision = sqlc.arg(expected_run_revision)
   AND current_run_lease_id = sqlc.arg(current_run_lease_id)
RETURNING *;

-- name: CompleteParkedChildRunWait :one
WITH moved_run AS (
    UPDATE runs
       SET status = 'queued',
           revision = revision + 1,
           updated_at = transaction_timestamp()
     WHERE runs.id = sqlc.arg(run_id)
       AND runs.environment_id = sqlc.arg(environment_id)
       AND runs.status = 'waiting'
       AND runs.revision = sqlc.arg(expected_run_revision)
       AND runs.current_attempt_number = sqlc.arg(attempt_number)
       AND runs.current_run_lease_id IS NULL
    RETURNING revision
)
UPDATE run_waits
   SET condition_status = 'completed',
       condition_result = sqlc.arg(condition_result),
       condition_terminal_at = transaction_timestamp(),
       suspension_status = 'resume_pending',
       expected_run_revision = moved_run.revision,
       updated_at = transaction_timestamp()
  FROM moved_run
 WHERE run_waits.id = sqlc.arg(id)
   AND run_waits.run_id = sqlc.arg(run_id)
   AND run_waits.child_run_id = sqlc.arg(child_run_id)
   AND run_waits.condition_status = 'pending'
   AND run_waits.suspension_status = 'parked'
   AND run_waits.expected_run_revision = sqlc.arg(expected_run_revision)
   AND run_waits.current_run_lease_id IS NULL
   AND run_waits.prior_run_lease_id = sqlc.arg(prior_run_lease_id)
   AND run_waits.suspend_checkpoint_id = sqlc.arg(suspend_checkpoint_id)
RETURNING run_waits.*;

-- Only the instance coordinator may mark members after sealing the complete set.
-- name: MarkCheckpointMemberWaiting :one
UPDATE run_waits w SET suspension_status='checkpointing',suspend_checkpoint_id=m.checkpoint_id,updated_at=clock_timestamp()
FROM computer_checkpoint_runs m,computer_checkpoints c,computer_instances i
WHERE m.checkpoint_id=sqlc.arg(checkpoint_id) AND m.run_wait_id=w.id AND m.source_run_lease_id=w.current_run_lease_id
 AND c.id=m.checkpoint_id AND c.status='creating' AND i.id=c.source_computer_instance_id
 AND i.capture_checkpoint_id=c.id AND i.admission_state='checkpointing'
 AND i.writer_generation=c.writer_generation AND i.membership_revision=c.membership_revision
 AND w.id=sqlc.arg(wait_id) AND w.suspension_status='hot' AND w.condition_status='pending'
RETURNING w.*;

-- name: BeginRunLeaseCheckpoint :one
UPDATE run_leases
   SET status = 'checkpointing',
       updated_at = transaction_timestamp()
 WHERE run_leases.id = sqlc.arg(id)
   AND run_leases.run_id = sqlc.arg(run_id)
   AND run_leases.computer_id = sqlc.arg(computer_id)
   AND run_leases.attempt_number = sqlc.arg(attempt_number)
   AND run_leases.lease_sequence = sqlc.arg(lease_sequence)
   AND run_leases.status = 'running'
   AND run_leases.expires_at > clock_timestamp()
   AND EXISTS(SELECT 1 FROM computer_checkpoint_runs m JOIN computer_instances i ON i.id=m.source_computer_instance_id
               WHERE m.source_run_lease_id=run_leases.id AND i.capture_checkpoint_id=m.checkpoint_id
               AND i.admission_state='checkpointing' AND i.writer_generation=run_leases.writer_generation)
RETURNING run_leases.*;

-- A resumed but still-pending waiter remains resident. Its prior checkpoint is
-- historical; a later whole-instance capture creates a fresh member record.
-- name: AcknowledgeRunWaitResume :one
WITH resumed_run AS (
 UPDATE runs r SET
 status=CASE WHEN w.condition_status='pending' THEN 'waiting' ELSE 'running' END,
 revision=r.revision+CASE WHEN w.condition_status='pending' THEN 0 ELSE 1 END,
 updated_at=clock_timestamp()
 FROM run_waits w,run_leases l,computer_instances i,computer_checkpoints c
 WHERE w.id=sqlc.arg(wait_id) AND w.environment_id=sqlc.arg(environment_id)
 AND w.current_run_lease_id=sqlc.arg(run_lease_id) AND w.suspension_status='resuming'
 AND r.id=w.run_id AND r.environment_id=w.environment_id AND r.status='waiting'
 AND r.revision=w.expected_run_revision AND r.current_attempt_number=w.attempt_number
 AND r.current_run_lease_id=w.current_run_lease_id AND r.active_started_at IS NOT NULL
 AND l.id=w.current_run_lease_id AND l.run_id=w.run_id AND l.attempt_number=w.attempt_number
 AND l.lease_sequence=sqlc.arg(lease_sequence) AND l.status='running' AND l.expires_at>clock_timestamp()
 AND l.process_reconciled_at IS NULL
 AND i.id=l.computer_instance_id AND i.writer_generation=l.writer_generation
 AND i.reclaimed_at IS NULL AND i.writer_expires_at>clock_timestamp()
 AND i.admission_state IN ('restoring','open','draining') AND i.desired_state='ready'
 AND c.id=w.suspend_checkpoint_id AND c.resume_computer_instance_id=i.id AND c.resume_committed_at IS NOT NULL
 AND EXISTS(SELECT 1 FROM computer_checkpoint_runs m WHERE m.checkpoint_id=c.id
 AND m.run_id=r.id AND m.attempt_number=w.attempt_number AND m.run_wait_id=w.id
 AND m.source_run_lease_id=w.prior_run_lease_id)
 RETURNING r.id,r.revision
)
UPDATE run_waits w SET suspension_status=CASE WHEN w.condition_status='pending' THEN 'hot' ELSE 'released' END,
 expected_run_revision=resumed_run.revision,prior_run_lease_id=NULL,suspend_checkpoint_id=NULL,
 suspension_terminal_at=CASE WHEN w.condition_status<>'pending' THEN clock_timestamp() END,updated_at=clock_timestamp()
FROM resumed_run WHERE w.id=sqlc.arg(wait_id) AND w.run_id=resumed_run.id
RETURNING w.*;

-- name: RegisterTimerRunWait :one
WITH moved_run AS (
    UPDATE runs
       SET status = 'waiting',
           revision = revision + 1,
           updated_at = transaction_timestamp()
     WHERE runs.id = sqlc.arg(run_id)
       AND runs.environment_id = sqlc.arg(environment_id)
       AND runs.status = 'running'
       AND runs.revision = sqlc.arg(expected_running_revision)
       AND runs.current_attempt_number = sqlc.arg(attempt_number)
       AND runs.current_run_lease_id = sqlc.arg(current_run_lease_id)
       AND runs.active_started_at IS NOT NULL
       AND transaction_timestamp() < runs.active_started_at
             + ((runs.max_active_duration_ms - runs.active_elapsed_ms) * interval '1 millisecond')
    RETURNING *
)
INSERT INTO run_waits (
    id, environment_id, run_id, computer_id, kind, due_at,
    idle_timeout_ms, registration_request_fingerprint,
    expected_run_revision, attempt_number,
    current_run_lease_id,
    metadata, tags
)
SELECT sqlc.arg(id), moved_run.environment_id, moved_run.id, moved_run.computer_id,
       'timer', sqlc.arg(due_at), sqlc.arg(idle_timeout_ms),
       sqlc.arg(registration_request_fingerprint), moved_run.revision,
       sqlc.arg(attempt_number), sqlc.arg(current_run_lease_id), sqlc.arg(metadata), sqlc.arg(tags)
  FROM moved_run
RETURNING *;

-- name: GetTimerRunWaitRegistrationReplay :one
SELECT *
  FROM run_waits
 WHERE id = sqlc.arg(id)
   AND environment_id = sqlc.arg(environment_id)
   AND run_id = sqlc.arg(run_id)
   AND computer_id = sqlc.arg(computer_id)
   AND kind = 'timer'
   AND attempt_number = sqlc.arg(attempt_number)
   AND registration_request_fingerprint = sqlc.arg(registration_request_fingerprint)
   AND metadata = sqlc.arg(metadata)
   AND tags = sqlc.arg(tags)
   AND (current_run_lease_id = sqlc.arg(run_lease_id)
        OR prior_run_lease_id = sqlc.arg(run_lease_id));

-- name: ListDueTimerRunWaits :many
SELECT run_waits.*
  FROM run_waits
 WHERE kind = 'timer'
   AND condition_status = 'pending'
   AND due_at <= transaction_timestamp()
   AND suspension_status IN ('hot', 'checkpointing', 'parked', 'resuming')
   AND NOT EXISTS(SELECT 1 FROM runs r JOIN sessions s ON s.id=r.session_id WHERE r.id=run_waits.run_id AND (s.dispatch_hold_id IS NOT NULL OR s.current_run_id IS DISTINCT FROM r.id))
 ORDER BY due_at, run_waits.id
 LIMIT sqlc.arg(limit_count);

-- name: RegisterSessionInputRunWait :one
WITH moved_run AS (
    UPDATE runs
       SET status = 'waiting',
           revision = revision + 1,
           updated_at = transaction_timestamp()
     WHERE runs.id = sqlc.arg(run_id)
       AND runs.environment_id = sqlc.arg(environment_id)
       AND runs.session_id = sqlc.arg(session_id)
       AND runs.status = 'running'
       AND runs.revision = sqlc.arg(expected_running_revision)
       AND runs.current_attempt_number = sqlc.arg(attempt_number)
       AND runs.current_run_lease_id = sqlc.arg(current_run_lease_id)
       AND runs.active_started_at IS NOT NULL
       AND transaction_timestamp() < runs.active_started_at
             + ((runs.max_active_duration_ms - runs.active_elapsed_ms) * interval '1 millisecond')
    RETURNING *
)
INSERT INTO run_waits (
    id, environment_id, run_id, computer_id, kind, timeout_at,
    idle_timeout_ms, session_id, after_input_sequence,
    registration_request_fingerprint, expected_run_revision, attempt_number,
    current_run_lease_id,
    metadata, tags
)
SELECT sqlc.arg(id), sqlc.arg(environment_id), moved_run.id, moved_run.computer_id,
       'session_input', sqlc.narg(timeout_at), sqlc.arg(idle_timeout_ms),
       sqlc.arg(session_id), sqlc.arg(after_input_sequence),
       sqlc.arg(registration_request_fingerprint), moved_run.revision,
       sqlc.arg(attempt_number), sqlc.arg(current_run_lease_id), sqlc.arg(metadata), sqlc.arg(tags)
  FROM moved_run
RETURNING *;

-- name: GetSessionInputRunWaitRegistrationReplay :one
SELECT *
  FROM run_waits
 WHERE id = sqlc.arg(id)
   AND environment_id = sqlc.arg(environment_id)
   AND run_id = sqlc.arg(run_id)
   AND computer_id = sqlc.arg(computer_id)
   AND kind = 'session_input'
   AND session_id = sqlc.arg(session_id)
   AND after_input_sequence = sqlc.arg(after_input_sequence)
   AND attempt_number = sqlc.arg(attempt_number)
   AND registration_request_fingerprint = sqlc.arg(registration_request_fingerprint)
   AND metadata = sqlc.arg(metadata)
   AND tags = sqlc.arg(tags)
   AND (current_run_lease_id = sqlc.arg(run_lease_id)
        OR prior_run_lease_id = sqlc.arg(run_lease_id));

-- name: GetPendingSessionInputRunWait :one
SELECT *
  FROM run_waits
 WHERE environment_id = sqlc.arg(environment_id)
   AND run_id = sqlc.arg(run_id)
   AND attempt_number = sqlc.arg(attempt_number)
   AND session_id = sqlc.arg(session_id)
   AND kind = 'session_input'
   AND after_input_sequence = sqlc.arg(after_input_sequence)
   AND condition_status = 'pending'
   AND suspension_status IN ('hot', 'checkpointing', 'parked', 'resuming')
 ORDER BY id
 LIMIT 1
 FOR UPDATE;

-- name: CompleteHotRunWait :one
WITH locked_run AS MATERIALIZED (
    SELECT runs.id AS run_id
      FROM runs
     WHERE runs.id = sqlc.arg(run_id)
       AND runs.status = 'waiting'
       AND runs.revision = sqlc.arg(expected_run_revision)
       AND runs.current_attempt_number = sqlc.arg(attempt_number)
       AND runs.current_run_lease_id = sqlc.arg(current_run_lease_id)
       AND EXISTS (SELECT 1 FROM run_waits AS w WHERE w.id = sqlc.arg(id)
       AND w.run_id = sqlc.arg(run_id)
       AND w.condition_status = 'pending'
       AND w.suspension_status = 'hot'
       AND w.expected_run_revision = sqlc.arg(expected_run_revision)
       AND w.attempt_number = sqlc.arg(attempt_number)
       AND w.current_run_lease_id = sqlc.arg(current_run_lease_id)
       AND (
           (w.kind = 'timer' AND sqlc.narg(completed_turn_id)::uuid IS NULL)
           OR (w.kind = 'session_input' AND EXISTS (
               SELECT 1 FROM session_turns AS record
                WHERE record.id = sqlc.narg(completed_turn_id)
                  AND record.session_id = w.session_id
                  AND record.environment_id = w.environment_id
           ))
       ))
     FOR UPDATE OF runs
), eligible_wait AS MATERIALIZED (
    SELECT w.id, w.run_id
      FROM locked_run
      JOIN run_waits AS w ON w.run_id = locked_run.run_id
     WHERE w.id = sqlc.arg(id)
       AND w.run_id = sqlc.arg(run_id)
       AND w.condition_status = 'pending'
       AND w.suspension_status = 'hot'
       AND w.expected_run_revision = sqlc.arg(expected_run_revision)
       AND w.attempt_number = sqlc.arg(attempt_number)
       AND w.current_run_lease_id = sqlc.arg(current_run_lease_id)
       AND (
           (w.kind = 'timer' AND sqlc.narg(completed_turn_id)::uuid IS NULL)
           OR (w.kind = 'session_input' AND EXISTS (
               SELECT 1 FROM session_turns AS record
                WHERE record.id = sqlc.narg(completed_turn_id)
                  AND record.session_id = w.session_id
                  AND record.environment_id = w.environment_id
           ))
       )
     FOR UPDATE OF w
), moved_run AS (
    UPDATE runs
       SET status = 'running',
           revision = revision + 1,
           updated_at = transaction_timestamp()
      FROM eligible_wait
     WHERE runs.id = sqlc.arg(run_id)
       AND runs.id = eligible_wait.run_id
       AND runs.status = 'waiting'
       AND runs.revision = sqlc.arg(expected_run_revision)
       AND runs.current_attempt_number = sqlc.arg(attempt_number)
       AND runs.current_run_lease_id = sqlc.arg(current_run_lease_id)
    RETURNING revision
)
UPDATE run_waits
   SET condition_status = 'completed',
       condition_result = sqlc.arg(condition_result),
       completed_turn_id = sqlc.narg(completed_turn_id),
       condition_terminal_at = transaction_timestamp(),
       suspension_status = 'released',
       expected_run_revision = moved_run.revision,
       suspension_terminal_at = transaction_timestamp(),
       updated_at = transaction_timestamp()
  FROM moved_run, eligible_wait
 WHERE run_waits.id = eligible_wait.id
RETURNING run_waits.*;

-- name: CompleteCheckpointingRunWait :one
WITH eligible_wait AS MATERIALIZED (
    SELECT w.id
      FROM run_waits AS w
     WHERE w.id = sqlc.arg(id)
       AND w.run_id = sqlc.arg(run_id)
       AND w.condition_status = 'pending'
       AND w.suspension_status = 'checkpointing'
       AND w.expected_run_revision = sqlc.arg(expected_run_revision)
       AND w.current_run_lease_id = sqlc.arg(current_run_lease_id)
       AND (
           (w.kind = 'timer' AND sqlc.narg(completed_turn_id)::uuid IS NULL)
           OR (w.kind = 'session_input' AND EXISTS (
               SELECT 1 FROM session_turns AS record
                WHERE record.id = sqlc.narg(completed_turn_id)
                  AND record.session_id = w.session_id
                  AND record.environment_id = w.environment_id
           ))
       )
     FOR UPDATE OF w
)
UPDATE run_waits
   SET condition_status = 'completed',
       condition_result = sqlc.arg(condition_result),
       completed_turn_id = sqlc.narg(completed_turn_id),
       condition_terminal_at = transaction_timestamp(),
       updated_at = transaction_timestamp()
  FROM eligible_wait
 WHERE run_waits.id = eligible_wait.id
RETURNING run_waits.*;

-- name: CompleteParkedRunWait :one
WITH locked_run AS MATERIALIZED (
    SELECT runs.id AS run_id
      FROM runs
     WHERE runs.id = sqlc.arg(run_id)
       AND runs.status = 'waiting'
       AND runs.revision = sqlc.arg(expected_run_revision)
       AND runs.current_attempt_number = sqlc.arg(attempt_number)
       AND runs.current_run_lease_id IS NULL
       AND EXISTS (SELECT 1 FROM run_waits AS w WHERE w.id = sqlc.arg(id)
       AND w.run_id = sqlc.arg(run_id)
       AND w.condition_status = 'pending'
       AND w.suspension_status = 'parked'
       AND w.expected_run_revision = sqlc.arg(expected_run_revision)
       AND w.attempt_number = sqlc.arg(attempt_number)
       AND w.current_run_lease_id IS NULL
       AND w.prior_run_lease_id = sqlc.arg(prior_run_lease_id)
       AND w.suspend_checkpoint_id = sqlc.arg(suspend_checkpoint_id)
       AND (
           (w.kind = 'timer' AND sqlc.narg(completed_turn_id)::uuid IS NULL)
           OR (w.kind = 'session_input' AND EXISTS (
               SELECT 1 FROM session_turns AS record
                WHERE record.id = sqlc.narg(completed_turn_id)
                  AND record.session_id = w.session_id
                  AND record.environment_id = w.environment_id
           ))
       ))
     FOR UPDATE OF runs
), eligible_wait AS MATERIALIZED (
    SELECT w.id, w.run_id
      FROM locked_run
      JOIN run_waits AS w ON w.run_id = locked_run.run_id
     WHERE w.id = sqlc.arg(id)
       AND w.run_id = sqlc.arg(run_id)
       AND w.condition_status = 'pending'
       AND w.suspension_status = 'parked'
       AND w.expected_run_revision = sqlc.arg(expected_run_revision)
       AND w.attempt_number = sqlc.arg(attempt_number)
       AND w.current_run_lease_id IS NULL
       AND w.prior_run_lease_id = sqlc.arg(prior_run_lease_id)
       AND w.suspend_checkpoint_id = sqlc.arg(suspend_checkpoint_id)
       AND (
           (w.kind = 'timer' AND sqlc.narg(completed_turn_id)::uuid IS NULL)
           OR (w.kind = 'session_input' AND EXISTS (
               SELECT 1 FROM session_turns AS record
                WHERE record.id = sqlc.narg(completed_turn_id)
                  AND record.session_id = w.session_id
                  AND record.environment_id = w.environment_id
           ))
       )
     FOR UPDATE OF w
), moved_run AS (
    UPDATE runs
       SET status = 'queued',
           revision = revision + 1,
           updated_at = transaction_timestamp()
      FROM eligible_wait
     WHERE runs.id = sqlc.arg(run_id)
       AND runs.id = eligible_wait.run_id
       AND runs.status = 'waiting'
       AND runs.revision = sqlc.arg(expected_run_revision)
       AND runs.current_attempt_number = sqlc.arg(attempt_number)
       AND runs.current_run_lease_id IS NULL
    RETURNING revision
)
UPDATE run_waits
   SET condition_status = 'completed',
       condition_result = sqlc.arg(condition_result),
       completed_turn_id = sqlc.narg(completed_turn_id),
       condition_terminal_at = transaction_timestamp(),
       suspension_status = 'resume_pending',
       expected_run_revision = moved_run.revision,
       updated_at = transaction_timestamp()
  FROM moved_run, eligible_wait
 WHERE run_waits.id = eligible_wait.id
RETURNING run_waits.*;

-- name: ListPendingSessionInputWaitTimeouts :many
SELECT run_waits.*
  FROM run_waits
 WHERE kind = 'session_input'
   AND condition_status = 'pending'
   AND timeout_at IS NOT NULL
   AND timeout_at <= transaction_timestamp()
   AND NOT EXISTS(SELECT 1 FROM runs r JOIN sessions s ON s.id=r.session_id WHERE r.id=run_waits.run_id AND (s.dispatch_hold_id IS NOT NULL OR s.current_run_id IS DISTINCT FROM r.id))
 ORDER BY timeout_at, run_waits.id
 LIMIT sqlc.arg(limit_count);

-- name: FailHotRunWait :one
WITH locked_run AS MATERIALIZED (
    SELECT runs.id AS run_id
      FROM runs
     WHERE runs.id = sqlc.arg(run_id)
       AND runs.status = 'waiting'
       AND runs.revision = sqlc.arg(expected_run_revision)
       AND runs.current_attempt_number = sqlc.arg(attempt_number)
       AND runs.current_run_lease_id = sqlc.arg(current_run_lease_id)
       AND EXISTS (SELECT 1 FROM run_waits AS w WHERE w.id = sqlc.arg(id)
       AND w.run_id = sqlc.arg(run_id)
       AND w.condition_status = 'pending'
       AND w.suspension_status = 'hot'
       AND w.expected_run_revision = sqlc.arg(expected_run_revision)
       AND w.attempt_number = sqlc.arg(attempt_number)
       AND w.current_run_lease_id = sqlc.arg(current_run_lease_id))
     FOR UPDATE OF runs
), eligible_wait AS MATERIALIZED (
    SELECT w.id, w.run_id
      FROM locked_run
      JOIN run_waits AS w ON w.run_id = locked_run.run_id
     WHERE w.id = sqlc.arg(id)
       AND w.run_id = sqlc.arg(run_id)
       AND w.condition_status = 'pending'
       AND w.suspension_status = 'hot'
       AND w.expected_run_revision = sqlc.arg(expected_run_revision)
       AND w.attempt_number = sqlc.arg(attempt_number)
       AND w.current_run_lease_id = sqlc.arg(current_run_lease_id)
     FOR UPDATE OF w
), moved_run AS (
    UPDATE runs
       SET status = 'running', revision = revision + 1,
           updated_at = transaction_timestamp()
      FROM eligible_wait
     WHERE runs.id = sqlc.arg(run_id)
       AND runs.id = eligible_wait.run_id
       AND runs.status = 'waiting'
       AND runs.revision = sqlc.arg(expected_run_revision)
       AND runs.current_attempt_number = sqlc.arg(attempt_number)
       AND runs.current_run_lease_id = sqlc.arg(current_run_lease_id)
    RETURNING revision
)
UPDATE run_waits
   SET condition_status = 'failed', condition_reason_code = sqlc.arg(reason_code),
       condition_error = sqlc.arg(condition_error), condition_terminal_at = transaction_timestamp(),
       suspension_status = 'released', expected_run_revision = moved_run.revision,
       suspension_terminal_at = transaction_timestamp(), updated_at = transaction_timestamp()
  FROM moved_run, eligible_wait
 WHERE run_waits.id = eligible_wait.id
RETURNING run_waits.*;

-- name: FailCheckpointingRunWait :one
WITH eligible_wait AS MATERIALIZED (
    SELECT w.id
      FROM run_waits AS w
     WHERE w.id = sqlc.arg(id)
       AND w.run_id = sqlc.arg(run_id)
       AND w.condition_status = 'pending'
       AND w.suspension_status = 'checkpointing'
       AND w.expected_run_revision = sqlc.arg(expected_run_revision)
       AND w.current_run_lease_id = sqlc.arg(current_run_lease_id)
     FOR UPDATE OF w
)
UPDATE run_waits
   SET condition_status = 'failed', condition_reason_code = sqlc.arg(reason_code),
       condition_error = sqlc.arg(condition_error), condition_terminal_at = transaction_timestamp(),
       updated_at = transaction_timestamp()
  FROM eligible_wait
 WHERE run_waits.id = eligible_wait.id
RETURNING run_waits.*;

-- name: FailParkedRunWait :one
WITH locked_run AS MATERIALIZED (
    SELECT runs.id AS run_id
      FROM runs
     WHERE runs.id = sqlc.arg(run_id)
       AND runs.status = 'waiting'
       AND runs.revision = sqlc.arg(expected_run_revision)
       AND runs.current_attempt_number = sqlc.arg(attempt_number)
       AND runs.current_run_lease_id IS NULL
       AND EXISTS (SELECT 1 FROM run_waits AS w WHERE w.id = sqlc.arg(id)
       AND w.run_id = sqlc.arg(run_id)
       AND w.condition_status = 'pending'
       AND w.suspension_status = 'parked'
       AND w.expected_run_revision = sqlc.arg(expected_run_revision)
       AND w.attempt_number = sqlc.arg(attempt_number)
       AND w.current_run_lease_id IS NULL
       AND w.prior_run_lease_id = sqlc.arg(prior_run_lease_id)
       AND w.suspend_checkpoint_id = sqlc.arg(suspend_checkpoint_id))
     FOR UPDATE OF runs
), eligible_wait AS MATERIALIZED (
    SELECT w.id, w.run_id
      FROM locked_run
      JOIN run_waits AS w ON w.run_id = locked_run.run_id
     WHERE w.id = sqlc.arg(id)
       AND w.run_id = sqlc.arg(run_id)
       AND w.condition_status = 'pending'
       AND w.suspension_status = 'parked'
       AND w.expected_run_revision = sqlc.arg(expected_run_revision)
       AND w.attempt_number = sqlc.arg(attempt_number)
       AND w.current_run_lease_id IS NULL
       AND w.prior_run_lease_id = sqlc.arg(prior_run_lease_id)
       AND w.suspend_checkpoint_id = sqlc.arg(suspend_checkpoint_id)
     FOR UPDATE OF w
), moved_run AS (
    UPDATE runs
       SET status = 'queued', revision = revision + 1,
           updated_at = transaction_timestamp()
      FROM eligible_wait
     WHERE runs.id = sqlc.arg(run_id)
       AND runs.id = eligible_wait.run_id
       AND runs.status = 'waiting'
       AND runs.revision = sqlc.arg(expected_run_revision)
       AND runs.current_attempt_number = sqlc.arg(attempt_number)
       AND runs.current_run_lease_id IS NULL
    RETURNING revision
)
UPDATE run_waits
   SET suspension_status='resume_pending', condition_status = 'failed', condition_reason_code = sqlc.arg(reason_code),
       condition_error = sqlc.arg(condition_error), condition_terminal_at = transaction_timestamp(),
       expected_run_revision = moved_run.revision, updated_at = transaction_timestamp()
  FROM moved_run, eligible_wait
 WHERE run_waits.id = eligible_wait.id
RETURNING run_waits.*;

-- name: GetChildCallAttemptWait :one
SELECT * FROM run_waits
WHERE environment_id=sqlc.arg(environment_id) AND run_id=sqlc.arg(run_id)
AND attempt_number=sqlc.arg(attempt_number) AND child_claim_id=sqlc.arg(child_claim_id)
AND kind='child';

-- A pending member may resume because another member woke the shared instance.
-- Record condition changes without crossing its activation acknowledgement.
-- name: ResolveResumingRunWait :one
UPDATE run_waits w SET condition_status=sqlc.arg(condition_status)::text,
 condition_result=sqlc.narg(condition_result)::jsonb,condition_error=sqlc.narg(condition_error)::jsonb,
 condition_reason_code=sqlc.narg(reason_code)::text,completed_turn_id=sqlc.narg(completed_turn_id)::uuid,
 condition_terminal_at=clock_timestamp(),updated_at=clock_timestamp()
FROM runs r,run_leases l,computer_instances i,computer_checkpoints c
WHERE w.id=sqlc.arg(wait_id) AND w.run_id=sqlc.arg(run_id)
 AND w.expected_run_revision=sqlc.arg(expected_run_revision)
 AND w.suspension_status='resuming' AND w.condition_status='pending'
 AND r.id=w.run_id AND r.revision=w.expected_run_revision AND r.status='waiting'
 AND r.current_attempt_number=w.attempt_number AND r.current_run_lease_id=w.current_run_lease_id
 AND l.id=w.current_run_lease_id AND l.run_id=r.id AND l.attempt_number=w.attempt_number
 AND l.status IN ('assigned','starting','running') AND l.process_reconciled_at IS NULL
 AND i.id=l.computer_instance_id AND i.writer_generation=l.writer_generation
 AND i.reclaimed_at IS NULL AND i.admission_state IN ('restoring','open','draining')
 AND c.id=w.suspend_checkpoint_id AND c.resume_computer_instance_id=i.id AND c.resume_committed_at IS NOT NULL
 AND ((w.kind='session_input' AND sqlc.narg(completed_turn_id)::uuid IS NOT NULL AND EXISTS(
    SELECT 1 FROM session_turns t WHERE t.id=sqlc.narg(completed_turn_id) AND t.session_id=w.session_id
    AND t.environment_id=w.environment_id)) OR (w.kind<>'session_input' AND sqlc.narg(completed_turn_id)::uuid IS NULL)
    OR (sqlc.arg(condition_status)::text<>'completed' AND sqlc.narg(completed_turn_id)::uuid IS NULL))
RETURNING w.*;
