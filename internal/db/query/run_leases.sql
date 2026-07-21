-- name: GetRunLease :one
SELECT *
  FROM run_leases
 WHERE run_id = sqlc.arg(run_id)
   AND attempt_number = sqlc.arg(attempt_number)
   AND workspace_id = sqlc.arg(workspace_id)
   AND id = sqlc.arg(id);

-- name: GetCurrentRunLease :one
SELECT run_leases.*
  FROM runs
  JOIN run_leases
    ON run_leases.run_id = runs.id
   AND run_leases.attempt_number = runs.current_attempt_number
   AND run_leases.workspace_id = runs.workspace_id
   AND run_leases.id = runs.current_run_lease_id
 WHERE runs.environment_id = sqlc.arg(environment_id)
   AND runs.id = sqlc.arg(run_id);

-- name: DiscoverWorkerRunLeaseWork :many
WITH worker AS (
    SELECT worker_instances.id,
           worker_instances.current_epoch,
           worker_instances.state,
           worker_instances.max_run_consumers
      FROM worker_instances
      JOIN worker_groups
        ON worker_groups.id = worker_instances.worker_group_id
       AND worker_groups.state = 'active'
       AND worker_groups.allows_run
       AND worker_groups.protocol_version = worker_instances.protocol_version
     WHERE worker_instances.id = sqlc.arg(worker_instance_id)
       AND worker_instances.worker_group_id = sqlc.arg(worker_group_id)
       AND worker_instances.current_epoch = sqlc.arg(worker_epoch)::bigint
       AND worker_instances.protocol_version = sqlc.arg(worker_protocol_version)
       AND worker_instances.state IN ('active', 'draining')
       AND worker_instances.supports_run
)
SELECT run_leases.id,
       run_leases.lease_sequence
  FROM worker
  JOIN run_leases
    ON run_leases.worker_instance_id = worker.id
   AND run_leases.worker_epoch = worker.current_epoch
 WHERE run_leases.worker_group_id = sqlc.arg(worker_group_id)
   AND run_leases.worker_protocol_version = sqlc.arg(worker_protocol_version)
   AND run_leases.state IN ('assigned', 'starting')
   AND run_leases.start_deadline_at > transaction_timestamp()
   AND run_leases.expires_at > transaction_timestamp()
   AND (run_leases.state = 'starting' OR worker.state = 'active')
 ORDER BY CASE run_leases.state
              WHEN 'starting' THEN 0
              ELSE 1
          END,
          run_leases.assigned_at,
          run_leases.id
 LIMIT LEAST(sqlc.arg(row_limit)::int, (SELECT max_run_consumers FROM worker));

-- name: GetRunLeaseClaimLocators :one
SELECT run_leases.org_id,
       run_leases.project_id,
       run_leases.environment_id,
       run_leases.run_id,
       run_leases.workspace_id,
       run_leases.attempt_number,
       run_leases.region_id,
       run_leases.runtime_instance_id,
       run_leases.network_slot_id,
       run_leases.network_slot_generation,
       runs.actor_id,
       actors.run_generation AS actor_run_generation,
       workspace_leases.id AS workspace_lease_id,
       workspace_leases.workspace_mount_id,
       run_waits.id AS run_wait_id,
       run_waits.suspend_checkpoint_id,
       run_waits.handoff_resume_checkpoint_id,
       run_waits.resume_attach_id,
       run_waits.resume_request_version,
       suspend_checkpoints.private_workspace_version_id AS checkpoint_private_workspace_version_id,
       runs.parent_run_id,
       runs.parent_owns_lifecycle,
       parent_runs.actor_id AS parent_actor_id,
       parent_actors.run_generation AS parent_actor_run_generation,
       coalesce(parent_runs.current_attempt_number, 0)::integer AS parent_attempt_number,
       enclosing_waits.id AS enclosing_wait_id,
       enclosing_waits.suspend_checkpoint_id AS enclosing_suspend_checkpoint_id,
       enclosing_waits.resume_attach_id AS enclosing_resume_attach_id,
       enclosing_waits.base_workspace_version_id AS enclosing_base_workspace_version_id,
       enclosing_waits.handoff_runtime_instance_id AS enclosing_runtime_instance_id,
       enclosing_waits.handoff_workspace_mount_id AS enclosing_workspace_mount_id,
       enclosing_waits.handoff_mount_generation AS enclosing_mount_generation,
       enclosing_waits.ownership_generation AS enclosing_ownership_generation,
       enclosing_waits.parent_writer_generation AS enclosing_parent_writer_generation,
       enclosing_waits.child_writer_generation AS enclosing_child_writer_generation,
       enclosing_waits.resume_writer_generation AS enclosing_resume_writer_generation,
       parent_enclosing_waits.id AS parent_enclosing_wait_id,
       parent_enclosing_waits.run_id AS parent_enclosing_run_id,
       coalesce(parent_enclosing_waits.attempt_number, 0)::integer AS parent_enclosing_attempt_number,
       run_waits.child_run_id AS resume_child_run_id,
       coalesce(resume_child_runs.current_attempt_number, 0)::integer AS resume_child_attempt_number,
       run_waits.resume_workspace_version_id AS handoff_resume_workspace_version_id,
       run_waits.handoff_runtime_instance_id AS resume_handoff_runtime_instance_id,
       run_waits.handoff_workspace_mount_id AS resume_handoff_workspace_mount_id,
       run_waits.handoff_mount_generation AS resume_handoff_mount_generation,
       run_waits.ownership_generation AS resume_handoff_ownership_generation,
       run_waits.parent_writer_generation AS resume_handoff_parent_writer_generation,
       run_waits.child_writer_generation AS resume_handoff_child_writer_generation,
       run_waits.resume_writer_generation AS resume_handoff_resume_writer_generation
  FROM run_leases
  JOIN runs
    ON runs.id = run_leases.run_id
   AND runs.workspace_id = run_leases.workspace_id
   AND runs.current_attempt_number = run_leases.attempt_number
   AND runs.current_run_lease_id = run_leases.id
   AND runs.status = 'queued'
  LEFT JOIN actors
    ON actors.id = runs.actor_id
   AND actors.workspace_id = runs.workspace_id
  JOIN worker_groups
    ON worker_groups.id = run_leases.worker_group_id
   AND worker_groups.region_id = run_leases.region_id
   AND worker_groups.state = 'active'
   AND worker_groups.allows_run
   AND worker_groups.protocol_version = run_leases.worker_protocol_version
  JOIN worker_instances
    ON worker_instances.id = run_leases.worker_instance_id
   AND worker_instances.worker_group_id = run_leases.worker_group_id
   AND worker_instances.current_epoch = run_leases.worker_epoch
   AND worker_instances.protocol_version = run_leases.worker_protocol_version
   AND worker_instances.state IN ('active', 'draining')
   AND worker_instances.supports_run
  JOIN workspace_leases
    ON workspace_leases.owner_run_lease_id = run_leases.id
   AND workspace_leases.workspace_id = run_leases.workspace_id
   AND workspace_leases.state = 'active'
   AND workspace_leases.expires_at > transaction_timestamp()
  LEFT JOIN run_waits
    ON run_waits.run_id = run_leases.run_id
   AND run_waits.attempt_number = run_leases.attempt_number
   AND run_waits.workspace_id = run_leases.workspace_id
   AND run_waits.current_run_lease_id = run_leases.id
   AND run_waits.suspension_state = 'resuming'
  LEFT JOIN run_checkpoints AS suspend_checkpoints
    ON suspend_checkpoints.run_id = run_waits.run_id
   AND suspend_checkpoints.attempt_number = run_waits.attempt_number
   AND suspend_checkpoints.workspace_id = run_waits.workspace_id
   AND suspend_checkpoints.run_wait_id = run_waits.id
   AND suspend_checkpoints.id = run_waits.suspend_checkpoint_id
   AND suspend_checkpoints.kind = 'suspend'
   AND suspend_checkpoints.state = 'ready'
   AND (suspend_checkpoints.expires_at IS NULL
        OR suspend_checkpoints.expires_at > transaction_timestamp())
  LEFT JOIN runs AS resume_child_runs
    ON resume_child_runs.id = run_waits.child_run_id
   AND resume_child_runs.parent_run_id = run_waits.run_id
   AND resume_child_runs.workspace_id = run_waits.workspace_id
  LEFT JOIN runs AS parent_runs
    ON parent_runs.id = runs.parent_run_id
   AND parent_runs.environment_id = runs.environment_id
   AND parent_runs.workspace_id = runs.workspace_id
  LEFT JOIN actors AS parent_actors
    ON parent_actors.id = parent_runs.actor_id
   AND parent_actors.workspace_id = parent_runs.workspace_id
  LEFT JOIN run_waits AS enclosing_waits
    ON enclosing_waits.run_id = parent_runs.id
   AND enclosing_waits.attempt_number = parent_runs.current_attempt_number
   AND enclosing_waits.workspace_id = parent_runs.workspace_id
   AND enclosing_waits.child_run_id = runs.id
   AND enclosing_waits.child_parent_owned IS TRUE
   AND enclosing_waits.condition_state = 'pending'
   AND enclosing_waits.suspension_state = 'parked'
  LEFT JOIN run_waits AS parent_enclosing_waits
    ON parent_enclosing_waits.workspace_id = parent_runs.workspace_id
   AND parent_enclosing_waits.child_run_id = parent_runs.id
   AND parent_enclosing_waits.child_parent_owned IS TRUE
   AND parent_enclosing_waits.condition_state = 'pending'
   AND parent_enclosing_waits.suspension_state = 'parked'
 WHERE run_leases.id = sqlc.arg(id)
   AND run_leases.lease_sequence = sqlc.arg(lease_sequence)
   AND run_leases.worker_group_id = sqlc.arg(worker_group_id)
   AND run_leases.worker_instance_id = sqlc.arg(worker_instance_id)
   AND run_leases.worker_epoch = sqlc.arg(worker_epoch)
   AND run_leases.worker_protocol_version = sqlc.arg(worker_protocol_version)
   AND run_leases.state IN ('assigned', 'starting')
   AND run_leases.start_deadline_at > transaction_timestamp()
   AND run_leases.expires_at > transaction_timestamp()
   AND (run_leases.state = 'starting' OR worker_instances.state = 'active');

-- name: GetFreshRunLeaseStartLocators :one
SELECT run_leases.org_id,
       run_leases.project_id,
       run_leases.environment_id,
       run_leases.run_id,
       run_leases.workspace_id,
       run_leases.attempt_number,
       run_leases.region_id,
       run_leases.runtime_instance_id,
       run_leases.network_slot_id,
       run_leases.network_slot_generation,
       workspace_leases.id AS workspace_lease_id,
       workspace_leases.workspace_mount_id
  FROM run_leases
  JOIN runs
    ON runs.id = run_leases.run_id
   AND runs.workspace_id = run_leases.workspace_id
   AND runs.current_attempt_number = run_leases.attempt_number
   AND runs.current_run_lease_id = run_leases.id
   AND runs.entrypoint_kind = 'task'
   AND runs.actor_id IS NULL
   AND runs.parent_run_id IS NULL
   AND (
       (run_leases.state = 'starting' AND runs.status = 'queued')
       OR (run_leases.state = 'running' AND runs.status = 'running')
   )
  JOIN worker_groups
    ON worker_groups.id = run_leases.worker_group_id
   AND worker_groups.region_id = run_leases.region_id
   AND worker_groups.state = 'active'
   AND worker_groups.allows_run
   AND worker_groups.protocol_version = run_leases.worker_protocol_version
  JOIN worker_instances
    ON worker_instances.id = run_leases.worker_instance_id
   AND worker_instances.worker_group_id = run_leases.worker_group_id
   AND worker_instances.current_epoch = run_leases.worker_epoch
   AND worker_instances.protocol_version = run_leases.worker_protocol_version
   AND worker_instances.state IN ('active', 'draining')
   AND worker_instances.supports_run
  JOIN workspace_leases
    ON workspace_leases.owner_run_lease_id = run_leases.id
   AND workspace_leases.workspace_id = run_leases.workspace_id
   AND workspace_leases.state = 'active'
   AND workspace_leases.expires_at > transaction_timestamp()
 WHERE run_leases.id = sqlc.arg(id)
   AND run_leases.lease_sequence = sqlc.arg(lease_sequence)
   AND run_leases.worker_group_id = sqlc.arg(worker_group_id)
   AND run_leases.worker_instance_id = sqlc.arg(worker_instance_id)
   AND run_leases.worker_epoch = sqlc.arg(worker_epoch)
   AND run_leases.worker_protocol_version = sqlc.arg(worker_protocol_version)
   AND run_leases.state IN ('starting', 'running')
   AND run_leases.expires_at > transaction_timestamp()
   AND (run_leases.state = 'running'
        OR run_leases.start_deadline_at > transaction_timestamp())
   AND NOT EXISTS (
       SELECT 1
         FROM run_waits
        WHERE run_waits.run_id = run_leases.run_id
          AND run_waits.attempt_number = run_leases.attempt_number
          AND run_waits.workspace_id = run_leases.workspace_id
          AND run_waits.current_run_lease_id = run_leases.id
          AND run_waits.suspension_state = 'resuming'
   );

-- name: GetRunLeaseSecretDeliveryLocators :one
SELECT run_leases.environment_id,
       run_leases.run_id,
       run_leases.workspace_id,
       run_leases.attempt_number
  FROM run_leases
  JOIN worker_groups
    ON worker_groups.id = run_leases.worker_group_id
   AND worker_groups.region_id = run_leases.region_id
   AND worker_groups.state = 'active'
   AND worker_groups.allows_run
   AND worker_groups.protocol_version = run_leases.worker_protocol_version
  JOIN worker_instances
    ON worker_instances.id = run_leases.worker_instance_id
   AND worker_instances.worker_group_id = run_leases.worker_group_id
   AND worker_instances.current_epoch = run_leases.worker_epoch
   AND worker_instances.protocol_version = run_leases.worker_protocol_version
   AND worker_instances.state IN ('active', 'draining')
   AND worker_instances.supports_run
 WHERE run_leases.id = sqlc.arg(id)
   AND run_leases.lease_sequence = sqlc.arg(lease_sequence)
   AND run_leases.worker_group_id = sqlc.arg(worker_group_id)
   AND run_leases.worker_instance_id = sqlc.arg(worker_instance_id)
   AND run_leases.worker_epoch = sqlc.arg(worker_epoch)
   AND run_leases.worker_protocol_version = sqlc.arg(worker_protocol_version)
   AND run_leases.state IN ('assigned', 'starting')
   AND run_leases.start_deadline_at > transaction_timestamp()
   AND run_leases.expires_at > transaction_timestamp()
   AND (run_leases.state = 'starting' OR worker_instances.state = 'active');

-- name: GetRunEntrypointLocators :one
SELECT run_leases.org_id,
       run_leases.project_id,
       run_leases.environment_id,
       run_leases.run_id,
       run_leases.workspace_id,
       run_leases.attempt_number,
       run_leases.region_id,
       run_leases.runtime_instance_id,
       run_leases.network_slot_id,
       run_leases.network_slot_generation,
       workspace_leases.id AS workspace_lease_id,
       workspace_leases.workspace_mount_id
  FROM run_leases
  JOIN runs
    ON runs.id = run_leases.run_id
   AND runs.workspace_id = run_leases.workspace_id
   AND runs.current_attempt_number = run_leases.attempt_number
   AND runs.current_run_lease_id = run_leases.id
   AND runs.status = 'running'
  JOIN worker_groups
    ON worker_groups.id = run_leases.worker_group_id
   AND worker_groups.region_id = run_leases.region_id
   AND worker_groups.state = 'active'
   AND worker_groups.allows_run
   AND worker_groups.protocol_version = run_leases.worker_protocol_version
  JOIN worker_instances
    ON worker_instances.id = run_leases.worker_instance_id
   AND worker_instances.worker_group_id = run_leases.worker_group_id
   AND worker_instances.current_epoch = run_leases.worker_epoch
   AND worker_instances.protocol_version = run_leases.worker_protocol_version
   AND worker_instances.state IN ('active', 'draining')
   AND worker_instances.supports_run
  JOIN workspace_leases
    ON workspace_leases.owner_run_lease_id = run_leases.id
   AND workspace_leases.workspace_id = run_leases.workspace_id
   AND workspace_leases.state = 'active'
   AND workspace_leases.expires_at > transaction_timestamp()
 WHERE run_leases.id = sqlc.arg(id)
   AND run_leases.lease_sequence = sqlc.arg(lease_sequence)
   AND run_leases.worker_group_id = sqlc.arg(worker_group_id)
   AND run_leases.worker_instance_id = sqlc.arg(worker_instance_id)
   AND run_leases.worker_epoch = sqlc.arg(worker_epoch)
   AND run_leases.worker_protocol_version = sqlc.arg(worker_protocol_version)
   AND run_leases.state = 'running'
   AND run_leases.expires_at > transaction_timestamp()
   AND NOT EXISTS (
       SELECT 1
         FROM run_waits
        WHERE run_waits.run_id = run_leases.run_id
          AND run_waits.attempt_number = run_leases.attempt_number
          AND run_waits.workspace_id = run_leases.workspace_id
          AND run_waits.current_run_lease_id = run_leases.id
   );

-- name: LockRunLeaseClaimRun :one
SELECT *
  FROM runs
 WHERE id = sqlc.arg(id)
   AND org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND workspace_id = sqlc.arg(workspace_id)
 FOR UPDATE;

-- name: LockRunLeaseClaimActor :one
SELECT *
  FROM actors
 WHERE id = sqlc.arg(id)
   AND workspace_id = sqlc.arg(workspace_id)
 FOR UPDATE;

-- name: LockRunLeaseClaimWorkspace :one
SELECT *
  FROM workspaces
 WHERE id = sqlc.arg(id)
   AND org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND region_id = sqlc.arg(region_id)
 FOR UPDATE;

-- name: LockRunLeaseClaimAttempt :one
SELECT *
  FROM run_attempts
 WHERE run_id = sqlc.arg(run_id)
   AND number = sqlc.arg(number)
   AND workspace_id = sqlc.arg(workspace_id)
 FOR UPDATE;

-- name: LockRunLeaseClaimWorkerGroup :one
SELECT *
  FROM worker_groups
 WHERE id = sqlc.arg(id)
   AND region_id = sqlc.arg(region_id)
 FOR UPDATE;

-- name: LockRunLeaseClaimWorker :one
SELECT *
  FROM worker_instances
 WHERE id = sqlc.arg(id)
   AND worker_group_id = sqlc.arg(worker_group_id)
 FOR UPDATE;

-- name: LockRunLeaseClaimNetworkSlot :one
SELECT *
  FROM worker_network_slots
 WHERE id = sqlc.arg(id)
   AND worker_group_id = sqlc.arg(worker_group_id)
   AND worker_instance_id = sqlc.arg(worker_instance_id)
   AND worker_epoch = sqlc.arg(worker_epoch)
   AND generation = sqlc.arg(generation)
   AND runtime_instance_id = sqlc.arg(runtime_instance_id)
 FOR UPDATE;

-- name: LockRunLeaseClaimRuntime :one
SELECT *
  FROM runtime_instances
 WHERE id = sqlc.arg(id)
   AND org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND region_id = sqlc.arg(region_id)
   AND worker_group_id = sqlc.arg(worker_group_id)
   AND worker_instance_id = sqlc.arg(worker_instance_id)
   AND worker_epoch = sqlc.arg(worker_epoch)
   AND workspace_id = sqlc.arg(workspace_id)
 FOR UPDATE;

-- name: LockRunLeaseClaimLease :one
SELECT *
  FROM run_leases
 WHERE id = sqlc.arg(id)
   AND run_id = sqlc.arg(run_id)
   AND workspace_id = sqlc.arg(workspace_id)
   AND attempt_number = sqlc.arg(attempt_number)
   AND lease_sequence = sqlc.arg(lease_sequence)
   AND state IN ('assigned', 'starting')
   AND start_deadline_at > transaction_timestamp()
   AND expires_at > transaction_timestamp()
 FOR UPDATE;

-- name: LockFreshRunStartLease :one
SELECT *
  FROM run_leases
 WHERE id = sqlc.arg(id)
   AND run_id = sqlc.arg(run_id)
   AND workspace_id = sqlc.arg(workspace_id)
   AND attempt_number = sqlc.arg(attempt_number)
   AND lease_sequence = sqlc.arg(lease_sequence)
   AND state IN ('starting', 'running')
   AND expires_at > transaction_timestamp()
   AND (state = 'running' OR start_deadline_at > transaction_timestamp())
 FOR UPDATE;

-- name: LockRunEntrypointLease :one
SELECT *
  FROM run_leases
 WHERE id = sqlc.arg(id)
   AND run_id = sqlc.arg(run_id)
   AND workspace_id = sqlc.arg(workspace_id)
   AND attempt_number = sqlc.arg(attempt_number)
   AND lease_sequence = sqlc.arg(lease_sequence)
   AND state = 'running'
   AND expires_at > transaction_timestamp()
 FOR UPDATE;

-- name: LockRunLeaseClaimMount :one
SELECT *
  FROM workspace_mounts
 WHERE id = sqlc.arg(id)
   AND org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND region_id = sqlc.arg(region_id)
   AND worker_group_id = sqlc.arg(worker_group_id)
   AND worker_instance_id = sqlc.arg(worker_instance_id)
   AND worker_epoch = sqlc.arg(worker_epoch)
   AND runtime_instance_id = sqlc.arg(runtime_instance_id)
   AND workspace_id = sqlc.arg(workspace_id)
 FOR UPDATE;

-- name: LockRunLeaseClaimWorkspaceLease :one
SELECT *
  FROM workspace_leases
 WHERE id = sqlc.arg(id)
   AND org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND region_id = sqlc.arg(region_id)
   AND worker_group_id = sqlc.arg(worker_group_id)
   AND worker_instance_id = sqlc.arg(worker_instance_id)
   AND worker_epoch = sqlc.arg(worker_epoch)
   AND runtime_instance_id = sqlc.arg(runtime_instance_id)
   AND workspace_id = sqlc.arg(workspace_id)
   AND workspace_mount_id = sqlc.arg(workspace_mount_id)
   AND state = 'active'
   AND expires_at > transaction_timestamp()
 FOR UPDATE;

-- name: LockRunLeaseClaimWait :one
SELECT *
  FROM run_waits
 WHERE id = sqlc.arg(id)
   AND environment_id = sqlc.arg(environment_id)
   AND run_id = sqlc.arg(run_id)
   AND attempt_number = sqlc.arg(attempt_number)
   AND workspace_id = sqlc.arg(workspace_id)
   AND current_run_lease_id = sqlc.arg(current_run_lease_id)
 FOR UPDATE;

-- name: LockSameWorkspaceHandoffWait :one
SELECT *
  FROM run_waits
 WHERE id = sqlc.arg(id)
   AND environment_id = sqlc.arg(environment_id)
   AND run_id = sqlc.arg(parent_run_id)
   AND attempt_number = sqlc.arg(parent_attempt_number)
   AND workspace_id = sqlc.arg(workspace_id)
   AND child_run_id = sqlc.arg(child_run_id)
   AND child_parent_owned IS TRUE
   AND condition_state = 'pending'
   AND suspension_state = 'parked'
 FOR UPDATE;

-- name: LockReadyRunCheckpoint :one
SELECT *
  FROM run_checkpoints
 WHERE id = sqlc.arg(id)
   AND kind = sqlc.arg(kind)
   AND run_id = sqlc.arg(run_id)
   AND attempt_number = sqlc.arg(attempt_number)
   AND run_wait_id = sqlc.arg(run_wait_id)
   AND workspace_id = sqlc.arg(workspace_id)
   AND state = 'ready'
   AND (expires_at IS NULL OR expires_at > transaction_timestamp())
 FOR UPDATE;

-- name: MarkRunLeaseStarting :one
UPDATE run_leases
   SET state = 'starting',
       claimed_at = transaction_timestamp(),
       updated_at = transaction_timestamp()
 WHERE id = sqlc.arg(id)
   AND lease_sequence = sqlc.arg(lease_sequence)
   AND worker_group_id = sqlc.arg(worker_group_id)
   AND worker_instance_id = sqlc.arg(worker_instance_id)
   AND worker_epoch = sqlc.arg(worker_epoch)
   AND worker_protocol_version = sqlc.arg(worker_protocol_version)
   AND state = 'assigned'
   AND start_deadline_at > transaction_timestamp()
   AND expires_at > transaction_timestamp()
RETURNING *;

-- name: MarkFreshRunLeaseRunning :one
UPDATE run_leases
   SET state = 'running',
       started_at = transaction_timestamp(),
       updated_at = transaction_timestamp()
 WHERE id = sqlc.arg(id)
   AND run_id = sqlc.arg(run_id)
   AND workspace_id = sqlc.arg(workspace_id)
   AND attempt_number = sqlc.arg(attempt_number)
   AND lease_sequence = sqlc.arg(lease_sequence)
   AND worker_group_id = sqlc.arg(worker_group_id)
   AND worker_instance_id = sqlc.arg(worker_instance_id)
   AND worker_epoch = sqlc.arg(worker_epoch)
   AND worker_protocol_version = sqlc.arg(worker_protocol_version)
   AND runtime_instance_id = sqlc.arg(runtime_instance_id)
   AND network_slot_id = sqlc.arg(network_slot_id)
   AND network_slot_generation = sqlc.arg(network_slot_generation)
   AND runtime_identity_id = sqlc.arg(runtime_identity_id)
   AND state = 'starting'
   AND start_deadline_at > transaction_timestamp()
   AND expires_at > transaction_timestamp()
RETURNING *;

-- name: MarkFreshRunRunning :one
UPDATE runs
   SET status = 'running',
       started_at = coalesce(started_at, transaction_timestamp()),
       active_started_at = transaction_timestamp(),
       state_version = state_version + 1,
       updated_at = transaction_timestamp()
 WHERE id = sqlc.arg(id)
   AND org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND workspace_id = sqlc.arg(workspace_id)
   AND state_version = sqlc.arg(expected_state_version)
   AND status = 'queued'
   AND current_attempt_number = sqlc.arg(attempt_number)
   AND current_run_lease_id = sqlc.arg(run_lease_id)
   AND active_started_at IS NULL
RETURNING *;

-- name: TouchFreshRunWorkspace :one
UPDATE workspaces
   SET last_activity_at = greatest(last_activity_at, transaction_timestamp()),
       updated_at = transaction_timestamp()
 WHERE id = sqlc.arg(id)
   AND org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND ownership_generation = sqlc.arg(ownership_generation)
   AND writer_generation = sqlc.arg(writer_generation)
   AND owner_run_id = sqlc.arg(run_id)
   AND owner_actor_id IS NULL
   AND state = 'active'
   AND desired_state = 'active'
RETURNING *;

-- name: MarkRunEntrypointEntered :one
UPDATE run_attempts
   SET entrypoint_entered_at = transaction_timestamp()
 WHERE run_id = sqlc.arg(run_id)
   AND number = sqlc.arg(number)
   AND workspace_id = sqlc.arg(workspace_id)
   AND entrypoint_entered_at IS NULL
   AND terminal_at IS NULL
RETURNING *;
