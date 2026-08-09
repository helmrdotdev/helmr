-- name: GetRunWait :one
SELECT *
  FROM run_waits
 WHERE run_id = sqlc.arg(run_id)
   AND attempt_number = sqlc.arg(attempt_number)
   AND id = sqlc.arg(id);

-- name: GetTokenWaitRegistrationReplay :one
SELECT run_waits.id AS wait_id,
       runs.state_version AS run_state_version,
       run_waits.condition_state,
       run_waits.suspension_state,
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
   AND run_leases.workspace_id = run_waits.workspace_id
 WHERE run_waits.id = sqlc.arg(wait_id)
   AND run_waits.token_id = sqlc.arg(token_id)
   AND run_waits.kind = 'token'
   AND run_waits.resume_attach_id = sqlc.arg(resume_attach_id)
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
   AND run_leases.worker_instance_id = sqlc.arg(worker_instance_id)
   AND run_leases.worker_epoch = sqlc.arg(worker_epoch)
   AND run_waits.actor_speculative_input_sequence
       IS NOT DISTINCT FROM sqlc.narg(actor_speculative_input_sequence);

-- name: TokenWaitExists :one
SELECT EXISTS (
    SELECT 1
      FROM run_waits
     WHERE id = sqlc.arg(wait_id)
);

-- name: GetTokenWaitRegistrationLocator :one
SELECT runs.workspace_id,
       workspaces.owner_session_id,
       runs.org_id,
       runs.project_id
  FROM runs
  JOIN workspaces
    ON workspaces.environment_id = runs.environment_id
   AND workspaces.id = runs.workspace_id
 WHERE runs.environment_id = sqlc.arg(environment_id)
   AND runs.id = sqlc.arg(run_id);

-- name: LockTokenWaitActor :one
SELECT state,
       current_run_id,
       committed_input_sequence,
       next_input_sequence
  FROM sessions
 WHERE id = sqlc.arg(session_id)
 FOR UPDATE;

-- name: LockTokenWaitWorkspace :one
SELECT owner_session_id, owner_run_id, state, desired_state,
       ownership_generation, writer_generation
  FROM workspaces
 WHERE id = sqlc.arg(workspace_id)
   AND environment_id = sqlc.arg(environment_id)
 FOR UPDATE;

-- name: LockTokenWaitAttempt :one
SELECT entrypoint_kind, session_input_start_sequence, terminal_at
  FROM run_attempts
 WHERE run_id = sqlc.arg(run_id)
   AND number = sqlc.arg(attempt_number)
   AND workspace_id = sqlc.arg(workspace_id)
 FOR UPDATE;

-- name: LockTokenWaitRunLease :one
SELECT state
  FROM run_leases
 WHERE id = sqlc.arg(id)
   AND run_id = sqlc.arg(run_id)
   AND attempt_number = sqlc.arg(attempt_number)
   AND workspace_id = sqlc.arg(workspace_id)
   AND lease_sequence = sqlc.arg(lease_sequence)
   AND worker_group_id = sqlc.arg(worker_group_id)
   AND worker_instance_id = sqlc.arg(worker_instance_id)
   AND worker_epoch = sqlc.arg(worker_epoch)
   AND runtime_instance_id = sqlc.arg(runtime_instance_id)
   AND runtime_identity_id = sqlc.arg(runtime_identity_id)
   AND region_id = sqlc.arg(region_id)
   AND state = 'running'
   AND expires_at > transaction_timestamp()
 FOR UPDATE;

-- name: RegisterTokenWait :one
WITH moved_run AS (
    UPDATE runs
       SET status = 'waiting',
           state_version = state_version + 1,
           updated_at = transaction_timestamp()
     WHERE runs.id = sqlc.arg(run_id)
       AND runs.status = 'running'
       AND runs.state_version = sqlc.arg(expected_running_state_version)::bigint
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
    id, environment_id, run_id, workspace_id, kind, timeout_at,
    idle_timeout_ms, token_id, token_registration_run_state_version,
    registration_request_fingerprint, expected_run_state_version, attempt_number,
    actor_speculative_input_sequence, current_run_lease_id,
    checkpoint_due_at, resume_attach_id, metadata, tags
)
SELECT sqlc.arg(wait_id),
       sqlc.arg(environment_id),
       moved_run.id,
       moved_run.workspace_id,
       'token',
       sqlc.narg(timeout_at),
       sqlc.narg(idle_timeout_ms),
       sqlc.arg(token_id),
       sqlc.arg(expected_running_state_version)::bigint,
       sqlc.arg(request_fingerprint)::text,
       moved_run.state_version,
       sqlc.arg(attempt_number),
       sqlc.narg(actor_speculative_input_sequence),
       sqlc.arg(current_run_lease_id),
       sqlc.narg(checkpoint_due_at),
       sqlc.arg(resume_attach_id),
       sqlc.arg(metadata)::jsonb,
       sqlc.arg(tags)::text[]
  FROM moved_run
RETURNING run_waits.*;

-- name: LockTokenWaitCondition :one
SELECT state, result
  FROM tokens
 WHERE environment_id = sqlc.arg(environment_id)
   AND id = sqlc.arg(token_id)
 FOR UPDATE;

-- name: GetTokenWaitLocator :one
SELECT run_waits.id AS wait_id,
       run_waits.run_id,
       run_waits.workspace_id,
       run_waits.attempt_number,
       workspaces.owner_session_id
  FROM run_waits
  JOIN runs
    ON runs.environment_id = run_waits.environment_id
   AND runs.id = run_waits.run_id
  JOIN workspaces
    ON workspaces.id = run_waits.workspace_id
 WHERE run_waits.id = sqlc.arg(wait_id)
   AND run_waits.environment_id = sqlc.arg(environment_id)
   AND run_waits.run_id = sqlc.arg(run_id)
   AND run_waits.token_id = sqlc.arg(token_id)
   AND run_waits.kind = 'token'
   AND (
       run_waits.condition_state = 'pending'
       OR run_waits.suspension_state = 'checkpointing'
   );

-- name: LockTokenWaitRunLineage :many
WITH RECURSIVE lineage AS (
    SELECT id,
           parent_run_id,
           0::integer AS depth,
           ARRAY[id] AS path,
           false AS cycle
      FROM runs
     WHERE runs.environment_id = sqlc.arg(environment_id)
       AND runs.id = sqlc.arg(run_id)
    UNION ALL
    SELECT parent.id,
           parent.parent_run_id,
           child.depth + 1,
           child.path || parent.id,
           parent.id = ANY(child.path)
      FROM lineage AS child
      JOIN runs AS parent
        ON parent.environment_id = sqlc.arg(environment_id)
       AND parent.id = child.parent_run_id
     WHERE NOT child.cycle
)
SELECT runs.id,
       runs.parent_run_id,
       runs.workspace_id,
       runs.session_id,
       runs.entrypoint_kind,
       runs.status,
       runs.state_version,
       runs.current_attempt_number,
       runs.current_run_lease_id,
       runs.active_started_at,
       lineage.depth,
       lineage.cycle
  FROM lineage
  JOIN runs ON runs.id = lineage.id
 ORDER BY lineage.depth DESC, runs.id
 FOR UPDATE OF runs;

-- name: LockEnclosingRunWaits :many
SELECT id
  FROM run_waits
 WHERE child_run_id = sqlc.arg(run_id)
   AND suspension_state IN (
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
       workspace_id,
       kind,
       condition_state,
       suspension_state,
       expected_run_state_version,
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
   AND workspace_id = sqlc.arg(workspace_id)
   AND attempt_number = sqlc.arg(attempt_number)
   AND token_id = sqlc.arg(token_id)
   AND kind = 'token'
   AND (condition_state = 'pending' OR suspension_state = 'checkpointing')
 FOR UPDATE;

-- name: ResolveHotTokenWait :one
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
    RETURNING runs.state_version
)
UPDATE run_waits
   SET condition_state = sqlc.arg(condition_state)::text,
       condition_result = sqlc.arg(condition_result)::jsonb,
       condition_reason_code = sqlc.narg(reason_code)::text,
       condition_error = sqlc.arg(condition_error)::jsonb,
       condition_terminal_at = transaction_timestamp(),
       suspension_state = 'released',
       expected_run_state_version = moved_run.state_version,
       suspension_terminal_at = transaction_timestamp(),
       updated_at = transaction_timestamp()
  FROM moved_run
 WHERE run_waits.id = sqlc.arg(wait_id)
   AND run_waits.run_id = sqlc.arg(run_id)
   AND run_waits.condition_state = 'pending'
   AND run_waits.suspension_state = 'hot'
   AND run_waits.expected_run_state_version
       = sqlc.arg(expected_run_state_version)
   AND run_waits.current_run_lease_id = sqlc.arg(current_run_lease_id)
RETURNING run_waits.id;

-- name: ResolveCheckpointingTokenWait :one
UPDATE run_waits
   SET condition_state = sqlc.arg(condition_state)::text,
       condition_result = sqlc.arg(condition_result)::jsonb,
       condition_reason_code = sqlc.narg(reason_code)::text,
       condition_error = sqlc.arg(condition_error)::jsonb,
       condition_terminal_at = transaction_timestamp(),
       updated_at = transaction_timestamp()
 WHERE id = sqlc.arg(wait_id)
   AND run_id = sqlc.arg(run_id)
   AND condition_state = 'pending'
   AND suspension_state = 'checkpointing'
   AND expected_run_state_version = sqlc.arg(expected_run_state_version)
   AND current_run_lease_id = sqlc.arg(current_run_lease_id)
RETURNING id;

-- name: ResolveParkedTokenWait :one
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
    RETURNING runs.state_version
),
resolved_wait AS (
    UPDATE run_waits
       SET condition_state = sqlc.arg(condition_state)::text,
           condition_result = sqlc.arg(condition_result)::jsonb,
           condition_reason_code = sqlc.narg(reason_code)::text,
           condition_error = sqlc.arg(condition_error)::jsonb,
           condition_terminal_at = transaction_timestamp(),
           suspension_state = 'resume_pending',
           resume_request_version = run_waits.resume_request_version + 1,
           expected_run_state_version = moved_run.state_version,
           updated_at = transaction_timestamp()
      FROM moved_run
     WHERE run_waits.id = sqlc.arg(wait_id)
       AND run_waits.run_id = sqlc.arg(run_id)
       AND run_waits.condition_state = 'pending'
       AND run_waits.suspension_state = 'parked'
       AND run_waits.expected_run_state_version
           = sqlc.arg(expected_run_state_version)
       AND run_waits.current_run_lease_id IS NULL
       AND run_waits.prior_run_lease_id = sqlc.arg(prior_run_lease_id)
       AND run_waits.suspend_checkpoint_id = sqlc.arg(suspend_checkpoint_id)
    RETURNING run_waits.id,
              run_waits.environment_id,
              run_waits.run_id,
              run_waits.workspace_id,
              run_waits.resume_request_version
)
SELECT id FROM resolved_wait;

-- name: ListTokenWaitCandidates :many
SELECT id AS wait_id, run_id
  FROM run_waits
 WHERE environment_id = sqlc.arg(environment_id)
   AND token_id = sqlc.arg(token_id)
   AND (condition_state = 'pending' OR suspension_state = 'checkpointing')
 ORDER BY token_id,
          CASE condition_state
              WHEN 'pending' THEN 0
              WHEN 'completed' THEN 1
              WHEN 'failed' THEN 2
              WHEN 'cancelled' THEN 3
          END,
          id
 LIMIT sqlc.arg(row_limit);

-- name: ListTimedOutTokenWaitCandidates :many
SELECT id AS wait_id, run_id, environment_id, token_id
  FROM run_waits
 WHERE kind = 'token'
   AND condition_state = 'pending'
   AND timeout_at IS NOT NULL
   AND timeout_at <= transaction_timestamp()
 ORDER BY timeout_at, id
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
   AND child_parent_owned IS TRUE
   AND child_claim_id = sqlc.arg(child_claim_id)
   AND registration_request_fingerprint = sqlc.arg(registration_request_fingerprint)
   AND resume_attach_id = sqlc.arg(resume_attach_id);

-- name: GetSameWorkspaceChildCallReplay :one
SELECT *
  FROM run_waits
 WHERE environment_id = sqlc.arg(environment_id)
   AND run_id = sqlc.arg(run_id)
   AND workspace_id = sqlc.arg(workspace_id)
   AND attempt_number = sqlc.arg(attempt_number)
   AND id = sqlc.arg(id)
   AND kind = 'child'
   AND child_parent_owned IS TRUE
   AND child_target_declared_id = sqlc.arg(child_target_declared_id)
   AND child_claim_id = sqlc.arg(child_claim_id)
   AND registration_request_fingerprint = sqlc.arg(registration_request_fingerprint)
   AND resume_attach_id = sqlc.arg(resume_attach_id)
   AND (current_run_lease_id = sqlc.arg(run_lease_id)
        OR prior_run_lease_id = sqlc.arg(run_lease_id));

-- name: GetBoundSameWorkspaceChildCallReplay :one
SELECT *
  FROM run_waits
 WHERE environment_id = sqlc.arg(environment_id)
   AND run_id = sqlc.arg(run_id)
   AND workspace_id = sqlc.arg(workspace_id)
   AND id = sqlc.arg(id)
   AND kind = 'child'
   AND child_parent_owned IS TRUE
   AND child_target_declared_id = sqlc.arg(child_target_declared_id)
   AND child_claim_id = sqlc.arg(child_claim_id)
   AND registration_request_fingerprint = sqlc.arg(registration_request_fingerprint)
   AND child_run_id = sqlc.arg(child_run_id)
   AND base_workspace_version_id = sqlc.arg(base_workspace_version_id)
   AND base_workspace_content_digest = sqlc.arg(base_workspace_content_digest)
   AND handoff_runtime_instance_id IS NOT NULL
   AND handoff_workspace_mount_id IS NOT NULL
   AND handoff_mount_generation IS NOT NULL
   AND ownership_generation IS NOT NULL
   AND parent_writer_generation IS NOT NULL;

-- name: RegisterSameWorkspaceChildCall :one
WITH moved_run AS (
    UPDATE runs
       SET status = 'waiting',
           state_version = state_version + 1,
           updated_at = transaction_timestamp()
     WHERE runs.id = sqlc.arg(run_id)
       AND runs.environment_id = sqlc.arg(environment_id)
       AND runs.workspace_id = sqlc.arg(workspace_id)
       AND runs.status = 'running'
       AND runs.state_version = sqlc.arg(expected_running_state_version)
       AND runs.current_attempt_number = sqlc.arg(attempt_number)
       AND runs.current_run_lease_id = sqlc.arg(current_run_lease_id)
       AND runs.active_started_at IS NOT NULL
    RETURNING *
)
INSERT INTO run_waits (
    id, environment_id, run_id, workspace_id, kind,
    child_parent_owned, child_target_declared_id, child_claim_id, child_request,
    registration_request_fingerprint, expected_run_state_version,
    attempt_number, actor_speculative_input_sequence, current_run_lease_id,
    checkpoint_due_at, resume_attach_id, metadata, tags
)
SELECT sqlc.arg(id), moved_run.environment_id, moved_run.id,
       moved_run.workspace_id, 'child', TRUE,
       sqlc.arg(child_target_declared_id), sqlc.arg(child_claim_id),
       sqlc.arg(child_request), sqlc.arg(registration_request_fingerprint),
       moved_run.state_version, sqlc.arg(attempt_number),
       sqlc.narg(actor_speculative_input_sequence),
       sqlc.arg(current_run_lease_id), transaction_timestamp(),
       sqlc.arg(resume_attach_id), '{}'::jsonb, '{}'::text[]
  FROM moved_run
RETURNING *;

-- name: RegisterDifferentWorkspaceChildCall :one
WITH moved_run AS (
    UPDATE runs
       SET status = 'waiting',
           state_version = state_version + 1,
           updated_at = transaction_timestamp()
     WHERE runs.id = sqlc.arg(run_id)
       AND runs.environment_id = sqlc.arg(environment_id)
       AND runs.status = 'running'
       AND runs.state_version = sqlc.arg(expected_running_state_version)
       AND runs.current_attempt_number = sqlc.arg(attempt_number)
       AND runs.current_run_lease_id = sqlc.arg(current_run_lease_id)
       AND runs.workspace_id <> sqlc.arg(child_workspace_id)
       AND runs.active_started_at IS NOT NULL
    RETURNING *
)
INSERT INTO run_waits (
    id, environment_id, run_id, workspace_id, kind,
    child_run_id, child_parent_owned, child_target_declared_id,
    child_claim_id, child_request, registration_request_fingerprint,
    expected_run_state_version, attempt_number,
    actor_speculative_input_sequence, current_run_lease_id,
    checkpoint_due_at, resume_attach_id, metadata, tags
)
SELECT sqlc.arg(id), moved_run.environment_id, moved_run.id, moved_run.workspace_id,
       'child', sqlc.arg(child_run_id), TRUE, sqlc.arg(child_target_declared_id),
       sqlc.arg(child_claim_id), sqlc.arg(child_request),
       sqlc.arg(registration_request_fingerprint), moved_run.state_version,
       sqlc.arg(attempt_number), sqlc.narg(actor_speculative_input_sequence),
       sqlc.arg(current_run_lease_id), transaction_timestamp(),
       sqlc.arg(resume_attach_id), '{}'::jsonb, '{}'::text[]
  FROM moved_run
RETURNING *;

-- name: RegisterResolvedDifferentWorkspaceChildCall :one
INSERT INTO run_waits (
    id, environment_id, run_id, workspace_id, kind,
    condition_state, child_run_id, child_parent_owned,
    child_target_declared_id, child_claim_id, child_request,
    condition_result, condition_terminal_at, suspension_state,
    registration_request_fingerprint, expected_run_state_version,
    attempt_number, actor_speculative_input_sequence, current_run_lease_id,
    resume_attach_id, suspension_terminal_at, metadata, tags
)
SELECT sqlc.arg(id), parent.environment_id, parent.id, parent.workspace_id,
       'child', 'completed', child.id, TRUE,
       sqlc.arg(child_target_declared_id), sqlc.arg(child_claim_id),
       sqlc.arg(child_request), sqlc.arg(condition_result),
       transaction_timestamp(), 'released',
       sqlc.arg(registration_request_fingerprint), parent.state_version,
       sqlc.arg(attempt_number), sqlc.narg(actor_speculative_input_sequence),
       sqlc.arg(current_run_lease_id), sqlc.arg(resume_attach_id),
       transaction_timestamp(), '{}'::jsonb, '{}'::text[]
  FROM runs AS parent
  JOIN runs AS child
    ON child.environment_id = parent.environment_id
   AND child.parent_run_id = parent.id
   AND child.parent_owns_lifecycle IS TRUE
   AND child.id = sqlc.arg(child_run_id)
   AND child.workspace_id <> parent.workspace_id
 WHERE parent.environment_id = sqlc.arg(environment_id)
   AND parent.id = sqlc.arg(run_id)
   AND parent.status = 'running'
   AND parent.state_version = sqlc.arg(expected_running_state_version)
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
   AND run_waits.child_parent_owned IS TRUE
   AND run_waits.kind = 'child'
   AND run_waits.condition_state = 'pending'
   AND run_waits.suspension_state IN ('hot', 'checkpointing', 'parked')
 ORDER BY run_waits.created_at DESC, run_waits.id DESC
 LIMIT 1
 FOR UPDATE OF parent, run_waits;

-- name: ListSameWorkspaceHandoffAncestorRuns :many
WITH RECURSIVE ancestors AS (
    SELECT handoff.run_id AS parent_run_id,
           handoff.child_run_id,
           0 AS depth
      FROM run_waits AS handoff
     WHERE handoff.environment_id = sqlc.arg(environment_id)
       AND handoff.child_run_id = sqlc.arg(child_run_id)
       AND handoff.workspace_id = sqlc.arg(workspace_id)
       AND handoff.child_parent_owned IS TRUE
       AND handoff.condition_state = 'pending'
       AND handoff.suspension_state = 'parked'
    UNION ALL
    SELECT outer_wait.run_id,
           outer_wait.child_run_id,
           ancestors.depth + 1
      FROM ancestors
      JOIN runs AS child
        ON child.id = ancestors.parent_run_id
       AND child.workspace_id = sqlc.arg(workspace_id)
       AND child.parent_owns_lifecycle IS TRUE
      JOIN run_waits AS outer_wait
        ON outer_wait.environment_id = sqlc.arg(environment_id)
       AND outer_wait.run_id = child.parent_run_id
       AND outer_wait.child_run_id = child.id
       AND outer_wait.workspace_id = sqlc.arg(workspace_id)
       AND outer_wait.child_parent_owned IS TRUE
       AND outer_wait.condition_state = 'pending'
       AND outer_wait.suspension_state = 'parked'
)
SELECT sqlc.embed(parent),
       ancestors.depth
  FROM ancestors
  JOIN runs AS parent
    ON parent.id = ancestors.parent_run_id
   AND parent.environment_id = sqlc.arg(environment_id)
   AND parent.workspace_id = sqlc.arg(workspace_id)
   AND parent.status = 'waiting'
   AND parent.current_run_lease_id IS NULL
 ORDER BY ancestors.depth DESC;

-- name: LockSameWorkspaceHandoffAncestors :many
WITH RECURSIVE ancestors AS (
    SELECT handoff.id,
           handoff.run_id AS parent_run_id,
           handoff.child_run_id,
           0 AS depth
      FROM run_waits AS handoff
     WHERE handoff.environment_id = sqlc.arg(environment_id)
       AND handoff.child_run_id = sqlc.arg(child_run_id)
       AND handoff.workspace_id = sqlc.arg(workspace_id)
       AND handoff.child_parent_owned IS TRUE
       AND handoff.condition_state = 'pending'
       AND handoff.suspension_state = 'parked'
    UNION ALL
    SELECT outer_wait.id,
           outer_wait.run_id,
           outer_wait.child_run_id,
           ancestors.depth + 1
      FROM ancestors
      JOIN runs AS child
        ON child.id = ancestors.parent_run_id
       AND child.workspace_id = sqlc.arg(workspace_id)
       AND child.parent_owns_lifecycle IS TRUE
      JOIN run_waits AS outer_wait
        ON outer_wait.environment_id = sqlc.arg(environment_id)
       AND outer_wait.run_id = child.parent_run_id
       AND outer_wait.child_run_id = child.id
       AND outer_wait.workspace_id = sqlc.arg(workspace_id)
       AND outer_wait.child_parent_owned IS TRUE
       AND outer_wait.condition_state = 'pending'
       AND outer_wait.suspension_state = 'parked'
)
SELECT sqlc.embed(handoff),
       sqlc.embed(parent),
       sqlc.embed(attempt),
       ancestors.depth
  FROM ancestors
  JOIN run_waits AS handoff
    ON handoff.id = ancestors.id
  JOIN runs AS parent
    ON parent.id = handoff.run_id
   AND parent.environment_id = handoff.environment_id
   AND parent.workspace_id = handoff.workspace_id
   AND parent.status = 'waiting'
   AND parent.current_run_lease_id IS NULL
  JOIN run_attempts AS attempt
    ON attempt.run_id = parent.id
   AND attempt.number = parent.current_attempt_number
   AND attempt.workspace_id = parent.workspace_id
   AND attempt.terminal_at IS NULL
 ORDER BY ancestors.depth DESC
 FOR UPDATE OF parent, attempt, handoff;

-- name: CompleteHotChildRunWait :one
WITH moved_run AS (
    UPDATE runs
       SET status = 'running',
           state_version = state_version + 1,
           updated_at = transaction_timestamp()
     WHERE runs.id = sqlc.arg(run_id)
       AND runs.environment_id = sqlc.arg(environment_id)
       AND runs.status = 'waiting'
       AND runs.state_version = sqlc.arg(expected_run_state_version)
       AND runs.current_attempt_number = sqlc.arg(attempt_number)
       AND runs.current_run_lease_id = sqlc.arg(current_run_lease_id)
    RETURNING state_version
)
UPDATE run_waits
   SET condition_state = 'completed',
       condition_result = sqlc.arg(condition_result),
       condition_terminal_at = transaction_timestamp(),
       suspension_state = 'released',
       expected_run_state_version = moved_run.state_version,
       suspension_terminal_at = transaction_timestamp(),
       updated_at = transaction_timestamp()
  FROM moved_run
 WHERE run_waits.id = sqlc.arg(id)
   AND run_waits.run_id = sqlc.arg(run_id)
   AND run_waits.child_run_id = sqlc.arg(child_run_id)
   AND run_waits.condition_state = 'pending'
   AND run_waits.suspension_state = 'hot'
   AND run_waits.expected_run_state_version = sqlc.arg(expected_run_state_version)
   AND run_waits.current_run_lease_id = sqlc.arg(current_run_lease_id)
RETURNING run_waits.*;

-- name: CompleteCheckpointingChildRunWait :one
UPDATE run_waits
   SET condition_state = 'completed',
       condition_result = sqlc.arg(condition_result),
       condition_terminal_at = transaction_timestamp(),
       updated_at = transaction_timestamp()
 WHERE id = sqlc.arg(id)
   AND run_id = sqlc.arg(run_id)
   AND child_run_id = sqlc.arg(child_run_id)
   AND condition_state = 'pending'
   AND suspension_state = 'checkpointing'
   AND expected_run_state_version = sqlc.arg(expected_run_state_version)
   AND current_run_lease_id = sqlc.arg(current_run_lease_id)
RETURNING *;

-- name: CompleteParkedChildRunWait :one
WITH moved_run AS (
    UPDATE runs
       SET status = 'queued',
           state_version = state_version + 1,
           updated_at = transaction_timestamp()
     WHERE runs.id = sqlc.arg(run_id)
       AND runs.environment_id = sqlc.arg(environment_id)
       AND runs.status = 'waiting'
       AND runs.state_version = sqlc.arg(expected_run_state_version)
       AND runs.current_attempt_number = sqlc.arg(attempt_number)
       AND runs.current_run_lease_id IS NULL
    RETURNING state_version
)
UPDATE run_waits
   SET condition_state = 'completed',
       condition_result = sqlc.arg(condition_result),
       condition_terminal_at = transaction_timestamp(),
       suspension_state = 'resume_pending',
       resume_request_version = run_waits.resume_request_version + 1,
       expected_run_state_version = moved_run.state_version,
       updated_at = transaction_timestamp()
  FROM moved_run
 WHERE run_waits.id = sqlc.arg(id)
   AND run_waits.run_id = sqlc.arg(run_id)
   AND run_waits.child_run_id = sqlc.arg(child_run_id)
   AND run_waits.condition_state = 'pending'
   AND run_waits.suspension_state = 'parked'
   AND run_waits.expected_run_state_version = sqlc.arg(expected_run_state_version)
   AND run_waits.current_run_lease_id IS NULL
   AND run_waits.prior_run_lease_id = sqlc.arg(prior_run_lease_id)
   AND run_waits.suspend_checkpoint_id = sqlc.arg(suspend_checkpoint_id)
RETURNING run_waits.*;

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

-- name: RegisterTimerRunWait :one
WITH moved_run AS (
    UPDATE runs
       SET status = 'waiting',
           state_version = state_version + 1,
           updated_at = transaction_timestamp()
     WHERE runs.id = sqlc.arg(run_id)
       AND runs.environment_id = sqlc.arg(environment_id)
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
    id, environment_id, run_id, workspace_id, kind, due_at,
    idle_timeout_ms, registration_request_fingerprint,
    expected_run_state_version, attempt_number,
    actor_speculative_input_sequence, current_run_lease_id,
    checkpoint_due_at, resume_attach_id, metadata, tags
)
SELECT sqlc.arg(id), moved_run.environment_id, moved_run.id, moved_run.workspace_id,
       'timer', sqlc.arg(due_at), sqlc.arg(idle_timeout_ms),
       sqlc.arg(registration_request_fingerprint), moved_run.state_version,
       sqlc.arg(attempt_number), sqlc.narg(actor_speculative_input_sequence),
       sqlc.arg(current_run_lease_id), sqlc.arg(checkpoint_due_at),
       sqlc.arg(resume_attach_id), sqlc.arg(metadata), sqlc.arg(tags)
  FROM moved_run
RETURNING *;

-- name: GetTimerRunWaitRegistrationReplay :one
SELECT *
  FROM run_waits
 WHERE id = sqlc.arg(id)
   AND environment_id = sqlc.arg(environment_id)
   AND run_id = sqlc.arg(run_id)
   AND workspace_id = sqlc.arg(workspace_id)
   AND kind = 'timer'
   AND attempt_number = sqlc.arg(attempt_number)
   AND actor_speculative_input_sequence IS NOT DISTINCT FROM sqlc.narg(actor_speculative_input_sequence)
   AND resume_attach_id = sqlc.arg(resume_attach_id)
   AND registration_request_fingerprint = sqlc.arg(registration_request_fingerprint)
   AND metadata = sqlc.arg(metadata)
   AND tags = sqlc.arg(tags)
   AND (current_run_lease_id = sqlc.arg(run_lease_id)
        OR prior_run_lease_id = sqlc.arg(run_lease_id));

-- name: ListDueTimerRunWaits :many
SELECT *
  FROM run_waits
 WHERE kind = 'timer'
   AND condition_state = 'pending'
   AND due_at <= transaction_timestamp()
   AND suspension_state IN ('hot', 'checkpointing', 'parked')
 ORDER BY due_at, id
 LIMIT sqlc.arg(limit_count);

-- name: RegisterActorInputRunWait :one
WITH moved_run AS (
    UPDATE runs
       SET status = 'waiting',
           state_version = state_version + 1,
           updated_at = transaction_timestamp()
     WHERE runs.id = sqlc.arg(run_id)
       AND runs.environment_id = sqlc.arg(environment_id)
       AND runs.session_id = sqlc.arg(session_id)
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
    idle_timeout_ms, session_id, after_input_sequence,
    registration_request_fingerprint, expected_run_state_version, attempt_number,
    actor_speculative_input_sequence, current_run_lease_id,
    checkpoint_due_at, resume_attach_id, metadata, tags
)
SELECT sqlc.arg(id), sqlc.arg(environment_id), moved_run.id, moved_run.workspace_id,
       'actor_input', sqlc.narg(timeout_at), sqlc.arg(idle_timeout_ms),
       sqlc.arg(session_id), sqlc.arg(after_input_sequence),
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
   AND session_id = sqlc.arg(session_id)
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
   AND session_id = sqlc.arg(session_id)
   AND kind = 'actor_input'
   AND after_input_sequence = sqlc.arg(after_input_sequence)
   AND condition_state = 'pending'
   AND suspension_state IN ('hot', 'checkpointing', 'parked')
 ORDER BY id
 LIMIT 1
 FOR UPDATE;

-- name: CompleteHotRunWait :one
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
       completed_actor_record_direction = CASE
           WHEN sqlc.arg(completed_actor_record_id)::uuid IS NULL THEN NULL
           ELSE 'input'
       END,
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

-- name: CompleteCheckpointingRunWait :one
UPDATE run_waits
   SET condition_state = 'completed',
       condition_result = sqlc.arg(condition_result),
       completed_actor_record_id = sqlc.arg(completed_actor_record_id),
       completed_actor_record_direction = CASE
           WHEN sqlc.arg(completed_actor_record_id)::uuid IS NULL THEN NULL
           ELSE 'input'
       END,
       condition_terminal_at = transaction_timestamp(),
       updated_at = transaction_timestamp()
 WHERE id = sqlc.arg(id)
   AND run_id = sqlc.arg(run_id)
   AND condition_state = 'pending'
   AND suspension_state = 'checkpointing'
   AND expected_run_state_version = sqlc.arg(expected_run_state_version)
   AND current_run_lease_id = sqlc.arg(current_run_lease_id)
RETURNING *;

-- name: CompleteParkedRunWait :one
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
       completed_actor_record_direction = CASE
           WHEN sqlc.arg(completed_actor_record_id)::uuid IS NULL THEN NULL
           ELSE 'input'
       END,
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
