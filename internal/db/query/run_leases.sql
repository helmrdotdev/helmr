-- name: DiscoverWorkerRunLeaseWork :many
WITH worker AS (
    SELECT worker_instances.id,
	       worker_instances.current_epoch,
	       worker_instances.status,
	       worker_instances.max_vm_slots,
	       worker_groups.status AS group_status
      FROM worker_instances
      JOIN worker_groups
        ON worker_groups.id = worker_instances.worker_group_id
       AND worker_groups.status IN ('active', 'draining')
     WHERE worker_instances.id = sqlc.arg(worker_instance_id)
	   AND worker_instances.worker_group_id = sqlc.arg(worker_group_id)
	   AND worker_instances.current_epoch = sqlc.arg(worker_epoch)::bigint
	   AND worker_instances.status IN ('active', 'draining')
)
SELECT run_leases.id,
       run_leases.lease_sequence
  FROM worker
  JOIN run_leases
    ON run_leases.worker_instance_id = worker.id
   AND run_leases.worker_epoch = worker.current_epoch
  JOIN runtime_instances
    ON runtime_instances.id = run_leases.runtime_instance_id
   AND runtime_instances.worker_instance_id = run_leases.worker_instance_id
   AND runtime_instances.worker_epoch = run_leases.worker_epoch
   AND runtime_instances.observed_state = 'ready'
   AND runtime_instances.reclaimed_at IS NULL
  JOIN workspace_leases
    ON workspace_leases.owner_run_lease_id = run_leases.id
   AND workspace_leases.runtime_instance_id = run_leases.runtime_instance_id
   AND workspace_leases.status = 'active'
  JOIN workspace_mounts
    ON workspace_mounts.id = workspace_leases.workspace_mount_id
   AND workspace_mounts.runtime_instance_id = run_leases.runtime_instance_id
   AND workspace_mounts.worker_instance_id = run_leases.worker_instance_id
   AND workspace_mounts.worker_epoch = run_leases.worker_epoch
   AND workspace_mounts.status = 'mounted'
 WHERE run_leases.worker_group_id = sqlc.arg(worker_group_id)
   AND run_leases.status IN ('assigned', 'starting')
   AND run_leases.start_deadline_at > transaction_timestamp()
   AND run_leases.expires_at > transaction_timestamp()
 ORDER BY CASE run_leases.status
              WHEN 'starting' THEN 0
              ELSE 1
          END,
          run_leases.created_at,
          run_leases.id
 LIMIT LEAST(sqlc.arg(row_limit)::int, (SELECT max_vm_slots FROM worker));

-- name: GetRunLeaseClaimLocators :one
SELECT run_leases.org_id,
       run_leases.project_id,
       run_leases.environment_id,
       run_leases.run_id,
       run_leases.workspace_id,
       run_leases.attempt_number,
       run_leases.region_id,
       run_leases.runtime_instance_id,
       runtime_instances.restore_checkpoint_id AS runtime_restore_checkpoint_id,
       runs.session_id,
       sessions.run_generation AS actor_run_generation,
       workspace_leases.id AS workspace_lease_id,
       workspace_leases.workspace_mount_id,
       run_waits.id AS run_wait_id,
       run_waits.suspend_checkpoint_id,
       run_waits.resume_attach_id,
       run_waits.resume_request_version,
       suspend_checkpoints.private_workspace_version_id AS checkpoint_private_workspace_version_id,
       runs.parent_run_id,
       runs.parent_owns_lifecycle,
       parent_runs.session_id AS parent_session_id,
       parent_sessions.run_generation AS parent_actor_run_generation,
       coalesce(parent_runs.current_attempt_number, 0)::integer AS parent_attempt_number,
       enclosing_waits.id AS enclosing_wait_id,
       enclosing_waits.suspend_checkpoint_id AS enclosing_suspend_checkpoint_id,
       enclosing_waits.resume_attach_id AS enclosing_resume_attach_id,
       enclosing_waits.base_workspace_version_id AS enclosing_base_workspace_version_id,
       enclosing_waits.ownership_generation AS enclosing_ownership_generation,
       enclosing_waits.parent_writer_generation AS enclosing_parent_writer_generation,
       enclosing_waits.child_writer_generation AS enclosing_child_writer_generation,
       enclosing_waits.resume_writer_generation AS enclosing_resume_writer_generation,
       parent_enclosing_waits.id AS parent_enclosing_wait_id,
       parent_enclosing_waits.run_id AS parent_enclosing_run_id,
       coalesce(parent_enclosing_waits.attempt_number, 0)::integer AS parent_enclosing_attempt_number,
       run_waits.child_run_id AS resume_child_run_id,
       coalesce(resume_child_runs.current_attempt_number, 0)::integer AS resume_child_attempt_number,
       run_waits.resume_workspace_version_id,
       run_waits.ownership_generation AS resume_ownership_generation,
       run_waits.parent_writer_generation AS resume_parent_writer_generation,
       run_waits.child_writer_generation AS resume_child_writer_generation,
       run_waits.resume_writer_generation
  FROM run_leases
  JOIN runs
    ON runs.id = run_leases.run_id
   AND runs.workspace_id = run_leases.workspace_id
   AND runs.current_attempt_number = run_leases.attempt_number
   AND runs.current_run_lease_id = run_leases.id
   AND runs.status = 'queued'
  LEFT JOIN sessions
    ON sessions.id = runs.session_id
   AND sessions.workspace_id = runs.workspace_id
  JOIN worker_groups
    ON worker_groups.id = run_leases.worker_group_id
   AND worker_groups.region_id = run_leases.region_id
   AND worker_groups.status IN ('active', 'draining')
  JOIN worker_instances
    ON worker_instances.id = run_leases.worker_instance_id
   AND worker_instances.worker_group_id = run_leases.worker_group_id
   AND worker_instances.current_epoch = run_leases.worker_epoch
   AND worker_instances.status IN ('active', 'draining')
  JOIN runtime_instances
    ON runtime_instances.id = run_leases.runtime_instance_id
   AND runtime_instances.workspace_id = run_leases.workspace_id
   AND runtime_instances.worker_group_id = run_leases.worker_group_id
   AND runtime_instances.worker_instance_id = run_leases.worker_instance_id
   AND runtime_instances.worker_epoch = run_leases.worker_epoch
  JOIN workspace_leases
    ON workspace_leases.owner_run_lease_id = run_leases.id
   AND workspace_leases.workspace_id = run_leases.workspace_id
   AND workspace_leases.status = 'active'
   AND workspace_leases.expires_at > transaction_timestamp()
  LEFT JOIN run_waits
    ON run_waits.run_id = run_leases.run_id
   AND run_waits.attempt_number = run_leases.attempt_number
   AND run_waits.workspace_id = run_leases.workspace_id
   AND run_waits.current_run_lease_id = run_leases.id
   AND run_waits.suspension_status = 'resuming'
  LEFT JOIN run_checkpoints AS suspend_checkpoints
    ON suspend_checkpoints.run_id = run_waits.run_id
   AND suspend_checkpoints.attempt_number = run_waits.attempt_number
   AND suspend_checkpoints.workspace_id = run_waits.workspace_id
   AND suspend_checkpoints.run_wait_id = run_waits.id
   AND suspend_checkpoints.id = run_waits.suspend_checkpoint_id
   AND suspend_checkpoints.status = 'ready'
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
  LEFT JOIN sessions AS parent_sessions
    ON parent_sessions.id = parent_runs.session_id
   AND parent_sessions.workspace_id = parent_runs.workspace_id
  LEFT JOIN run_waits AS enclosing_waits
    ON enclosing_waits.run_id = parent_runs.id
   AND enclosing_waits.attempt_number = parent_runs.current_attempt_number
   AND enclosing_waits.workspace_id = parent_runs.workspace_id
   AND enclosing_waits.child_run_id = runs.id
   AND enclosing_waits.kind = 'child'
   AND runs.parent_run_id = enclosing_waits.run_id
   AND runs.parent_owns_lifecycle IS TRUE
   AND enclosing_waits.condition_status = 'pending'
   AND enclosing_waits.suspension_status = 'parked'
  LEFT JOIN run_waits AS parent_enclosing_waits
    ON parent_enclosing_waits.workspace_id = parent_runs.workspace_id
   AND parent_enclosing_waits.child_run_id = parent_runs.id
   AND parent_enclosing_waits.kind = 'child'
   AND parent_runs.parent_run_id = parent_enclosing_waits.run_id
   AND parent_runs.parent_owns_lifecycle IS TRUE
   AND parent_enclosing_waits.condition_status = 'pending'
   AND parent_enclosing_waits.suspension_status = 'parked'
 WHERE run_leases.id = sqlc.arg(id)
   AND run_leases.lease_sequence = sqlc.arg(lease_sequence)
   AND run_leases.worker_group_id = sqlc.arg(worker_group_id)
   AND run_leases.worker_instance_id = sqlc.arg(worker_instance_id)
   AND run_leases.worker_epoch = sqlc.arg(worker_epoch)
   AND run_leases.status IN ('assigned', 'starting')
   AND run_leases.start_deadline_at > transaction_timestamp()
   AND run_leases.expires_at > transaction_timestamp();

-- name: GetRunLeaseStartLocators :one
SELECT run_leases.org_id,
       run_leases.project_id,
       run_leases.environment_id,
       run_leases.run_id,
       run_leases.workspace_id,
       run_leases.attempt_number,
       run_leases.region_id,
       run_leases.runtime_instance_id,
       runtime_instances.restore_checkpoint_id AS runtime_restore_checkpoint_id,
       runs.session_id,
       runs.parent_run_id,
       workspace_leases.id AS workspace_lease_id,
       workspace_leases.workspace_mount_id,
       run_waits.id AS run_wait_id,
       run_waits.suspend_checkpoint_id AS run_wait_checkpoint_id,
       run_waits.resume_attach_id,
       run_waits.resume_request_version,
       enclosing_waits.id AS enclosing_wait_id,
       enclosing_waits.suspend_checkpoint_id AS enclosing_checkpoint_id,
       enclosing_waits.resume_attach_id AS enclosing_resume_attach_id
  FROM run_leases
  JOIN runs
    ON runs.id = run_leases.run_id
   AND runs.workspace_id = run_leases.workspace_id
   AND runs.current_attempt_number = run_leases.attempt_number
   AND runs.current_run_lease_id = run_leases.id
   AND (
       (run_leases.status = 'starting' AND runs.status = 'queued')
       OR (run_leases.status = 'running' AND runs.status = 'running')
   )
  JOIN worker_groups
    ON worker_groups.id = run_leases.worker_group_id
   AND worker_groups.region_id = run_leases.region_id
   AND worker_groups.status IN ('active', 'draining')
  JOIN worker_instances
    ON worker_instances.id = run_leases.worker_instance_id
   AND worker_instances.worker_group_id = run_leases.worker_group_id
   AND worker_instances.current_epoch = run_leases.worker_epoch
   AND worker_instances.status IN ('active', 'draining')
  JOIN runtime_instances
    ON runtime_instances.id = run_leases.runtime_instance_id
   AND runtime_instances.workspace_id = run_leases.workspace_id
   AND runtime_instances.worker_group_id = run_leases.worker_group_id
   AND runtime_instances.worker_instance_id = run_leases.worker_instance_id
   AND runtime_instances.worker_epoch = run_leases.worker_epoch
  JOIN workspace_leases
    ON workspace_leases.owner_run_lease_id = run_leases.id
   AND workspace_leases.workspace_id = run_leases.workspace_id
   AND workspace_leases.status = 'active'
   AND workspace_leases.expires_at > transaction_timestamp()
  LEFT JOIN run_waits
    ON run_waits.run_id = runs.id
   AND run_waits.attempt_number = runs.current_attempt_number
   AND run_waits.workspace_id = runs.workspace_id
   AND run_waits.current_run_lease_id = run_leases.id
   AND run_waits.prior_run_lease_id IS NOT NULL
   AND run_waits.prior_run_lease_id IS DISTINCT FROM run_leases.id
   AND run_waits.suspension_status IN ('resuming', 'released')
  LEFT JOIN run_waits AS enclosing_waits
    ON enclosing_waits.run_id = runs.parent_run_id
   AND enclosing_waits.workspace_id = runs.workspace_id
   AND enclosing_waits.child_run_id = runs.id
   AND enclosing_waits.kind = 'child'
   AND runs.parent_run_id = enclosing_waits.run_id
   AND runs.parent_owns_lifecycle IS TRUE
   AND (
       (run_leases.status = 'starting'
        AND enclosing_waits.condition_status = 'pending'
        AND enclosing_waits.suspension_status = 'parked')
       OR run_leases.status = 'running'
   )
 WHERE run_leases.id = sqlc.arg(id)
   AND run_leases.lease_sequence = sqlc.arg(lease_sequence)
   AND run_leases.worker_group_id = sqlc.arg(worker_group_id)
   AND run_leases.worker_instance_id = sqlc.arg(worker_instance_id)
   AND run_leases.worker_epoch = sqlc.arg(worker_epoch)
   AND run_leases.status IN ('starting', 'running')
   AND run_leases.expires_at > transaction_timestamp()
   AND (run_leases.status = 'running'
        OR run_leases.start_deadline_at > transaction_timestamp());

-- name: GetRunLeaseSecretDeliveryLocators :one
SELECT run_leases.environment_id,
       run_leases.run_id,
       run_leases.workspace_id,
       run_leases.attempt_number
  FROM run_leases
  JOIN worker_groups
    ON worker_groups.id = run_leases.worker_group_id
   AND worker_groups.region_id = run_leases.region_id
   AND worker_groups.status IN ('active', 'draining')
  JOIN worker_instances
    ON worker_instances.id = run_leases.worker_instance_id
   AND worker_instances.worker_group_id = run_leases.worker_group_id
   AND worker_instances.current_epoch = run_leases.worker_epoch
   AND worker_instances.status IN ('active', 'draining')
 WHERE run_leases.id = sqlc.arg(id)
   AND run_leases.lease_sequence = sqlc.arg(lease_sequence)
   AND run_leases.worker_group_id = sqlc.arg(worker_group_id)
   AND run_leases.worker_instance_id = sqlc.arg(worker_instance_id)
   AND run_leases.worker_epoch = sqlc.arg(worker_epoch)
   AND run_leases.status IN ('assigned', 'starting')
   AND run_leases.start_deadline_at > transaction_timestamp()
   AND run_leases.expires_at > transaction_timestamp();

-- name: GetRunEntrypointLocators :one
SELECT run_leases.org_id,
       run_leases.project_id,
       run_leases.environment_id,
       run_leases.run_id,
       run_leases.workspace_id,
       run_leases.attempt_number,
       run_leases.region_id,
       run_leases.runtime_instance_id,
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
   AND worker_groups.status IN ('active', 'draining')
  JOIN worker_instances
    ON worker_instances.id = run_leases.worker_instance_id
   AND worker_instances.worker_group_id = run_leases.worker_group_id
   AND worker_instances.current_epoch = run_leases.worker_epoch
   AND worker_instances.status IN ('active', 'draining')
  JOIN workspace_leases
    ON workspace_leases.owner_run_lease_id = run_leases.id
   AND workspace_leases.workspace_id = run_leases.workspace_id
   AND workspace_leases.status = 'active'
   AND workspace_leases.expires_at > transaction_timestamp()
 WHERE run_leases.id = sqlc.arg(id)
   AND run_leases.lease_sequence = sqlc.arg(lease_sequence)
   AND run_leases.worker_group_id = sqlc.arg(worker_group_id)
   AND run_leases.worker_instance_id = sqlc.arg(worker_instance_id)
   AND run_leases.worker_epoch = sqlc.arg(worker_epoch)
   AND run_leases.status = 'running'
   AND run_leases.expires_at > transaction_timestamp()
   AND NOT EXISTS (
       SELECT 1
         FROM run_waits
        WHERE run_waits.run_id = run_leases.run_id
          AND run_waits.attempt_number = run_leases.attempt_number
          AND run_waits.workspace_id = run_leases.workspace_id
          AND run_waits.current_run_lease_id = run_leases.id
   );

-- name: GetLiveRunLeaseLocators :one
SELECT run_leases.org_id,
       run_leases.project_id,
       run_leases.environment_id,
       run_leases.run_id,
       run_leases.workspace_id,
       run_leases.attempt_number,
       runs.session_id,
       runs.parent_run_id,
       runs.parent_owns_lifecycle,
       run_leases.region_id,
       run_leases.runtime_instance_id,
       workspace_leases.id AS workspace_lease_id,
       workspace_leases.workspace_mount_id
  FROM run_leases
  JOIN runs
    ON runs.id = run_leases.run_id
   AND runs.workspace_id = run_leases.workspace_id
   AND runs.current_attempt_number = run_leases.attempt_number
   AND runs.current_run_lease_id = run_leases.id
   AND runs.status IN ('running', 'waiting')
  JOIN worker_groups
    ON worker_groups.id = run_leases.worker_group_id
   AND worker_groups.region_id = run_leases.region_id
   AND worker_groups.status IN ('active', 'draining')
  JOIN worker_instances
    ON worker_instances.id = run_leases.worker_instance_id
   AND worker_instances.worker_group_id = run_leases.worker_group_id
   AND worker_instances.current_epoch = run_leases.worker_epoch
   AND worker_instances.status IN ('active', 'draining')
  JOIN workspace_leases
    ON workspace_leases.owner_run_lease_id = run_leases.id
   AND workspace_leases.workspace_id = run_leases.workspace_id
   AND workspace_leases.status = 'active'
   AND workspace_leases.expires_at > transaction_timestamp()
 WHERE run_leases.id = sqlc.arg(id)
   AND run_leases.lease_sequence = sqlc.arg(lease_sequence)
   AND run_leases.worker_group_id = sqlc.arg(worker_group_id)
   AND run_leases.worker_instance_id = sqlc.arg(worker_instance_id)
   AND run_leases.worker_epoch = sqlc.arg(worker_epoch)
   AND run_leases.status IN ('running', 'checkpointing', 'finalizing')
   AND run_leases.expires_at > transaction_timestamp();

-- name: LockRunLeaseClaimRun :one
SELECT *
  FROM runs
 WHERE id = sqlc.arg(id)
   AND org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND workspace_id = sqlc.arg(workspace_id)
 FOR UPDATE;

-- name: LockRunFinalizationParentRun :one
SELECT *
  FROM runs
 WHERE id = sqlc.arg(id)
   AND org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
 FOR UPDATE;

-- name: LockRunLeaseClaimActor :one
SELECT *
  FROM sessions
 WHERE id = sqlc.arg(id)
   AND workspace_id = sqlc.arg(workspace_id)
 FOR UPDATE;

-- name: LockRunLeaseClaimWorkspace :one
SELECT workspaces.id,
       workspaces.environment_id,
       workspaces.region_id,
       workspaces.sandbox_declared_id,
       workspaces.deployment_definition_id,
       workspaces.key,
       workspaces.revision,
       workspaces.owner_session_id,
       workspaces.owner_run_id,
       workspaces.ownership_generation,
       workspaces.writer_generation,
       workspaces.head_version_id,
       workspaces.status,
       workspaces.desired_state,
       workspaces.dirty_state,
       workspaces.last_activity_at,
       workspaces.created_at,
       workspaces.updated_at,
       workspaces.deleted_at
  FROM workspaces
  JOIN environments ON environments.id = workspaces.environment_id
 WHERE workspaces.id = sqlc.arg(id)
   AND environments.org_id = sqlc.arg(org_id)
   AND environments.project_id = sqlc.arg(project_id)
   AND workspaces.environment_id = sqlc.arg(environment_id)
   AND workspaces.region_id = sqlc.arg(region_id)
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

-- name: LockRunLeaseClaimReadyWorker :one
SELECT sqlc.embed(worker_instances),
       COALESCE((worker_instances.observed_at >= transaction_timestamp()
            - sqlc.arg(observation_freshness_seconds)::bigint * interval '1 second'
        AND worker_instances.run_paused_reason IS NULL), false)::boolean AS run_ready
  FROM worker_instances
 WHERE id = sqlc.arg(id)
   AND worker_group_id = sqlc.arg(worker_group_id)
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
   AND status IN ('assigned', 'starting')
   AND start_deadline_at > transaction_timestamp()
   AND expires_at > transaction_timestamp()
 FOR UPDATE;

-- name: LockRunStartLease :one
SELECT *
  FROM run_leases
 WHERE id = sqlc.arg(id)
   AND run_id = sqlc.arg(run_id)
   AND workspace_id = sqlc.arg(workspace_id)
   AND attempt_number = sqlc.arg(attempt_number)
   AND lease_sequence = sqlc.arg(lease_sequence)
   AND status IN ('starting', 'running')
   AND expires_at > transaction_timestamp()
   AND (status = 'running' OR start_deadline_at > transaction_timestamp())
 FOR UPDATE;

-- name: LockRunEntrypointLease :one
SELECT *
  FROM run_leases
 WHERE id = sqlc.arg(id)
   AND run_id = sqlc.arg(run_id)
   AND workspace_id = sqlc.arg(workspace_id)
   AND attempt_number = sqlc.arg(attempt_number)
   AND lease_sequence = sqlc.arg(lease_sequence)
   AND status = 'running'
   AND expires_at > transaction_timestamp()
 FOR UPDATE;

-- name: LockLiveRunLease :one
SELECT *
  FROM run_leases
 WHERE id = sqlc.arg(id)
   AND run_id = sqlc.arg(run_id)
   AND workspace_id = sqlc.arg(workspace_id)
   AND attempt_number = sqlc.arg(attempt_number)
   AND lease_sequence = sqlc.arg(lease_sequence)
   AND status IN ('running', 'checkpointing', 'finalizing')
   AND expires_at > transaction_timestamp()
 FOR UPDATE;

-- name: GetRunLeaseRenewalTime :one
SELECT clock_timestamp()::timestamptz;

-- name: RenewRunLeaseExpiry :one
UPDATE run_leases
   SET previous_expires_at = expires_at,
       renewed_at = sqlc.arg(renewed_at),
       expires_at = sqlc.arg(expires_at),
       updated_at = sqlc.arg(renewed_at)
 WHERE id = sqlc.arg(id)
   AND run_id = sqlc.arg(run_id)
   AND workspace_id = sqlc.arg(workspace_id)
   AND attempt_number = sqlc.arg(attempt_number)
   AND lease_sequence = sqlc.arg(lease_sequence)
   AND expires_at = sqlc.arg(previous_expires_at)
   AND status IN ('running', 'checkpointing')
 RETURNING *;

-- name: RenewRunWorkspaceLeaseExpiry :one
UPDATE workspace_leases
   SET renewed_at = sqlc.arg(renewed_at),
       expires_at = sqlc.arg(expires_at),
       updated_at = sqlc.arg(renewed_at)
 WHERE id = sqlc.arg(id)
   AND workspace_id = sqlc.arg(workspace_id)
   AND runtime_instance_id = sqlc.arg(runtime_instance_id)
   AND workspace_mount_id = sqlc.arg(workspace_mount_id)
   AND owner_run_lease_id = sqlc.arg(owner_run_lease_id)
   AND ownership_generation = sqlc.arg(ownership_generation)
   AND writer_generation = sqlc.arg(writer_generation)
   AND mount_fencing_generation = sqlc.arg(mount_fencing_generation)
   AND expires_at = sqlc.arg(previous_expires_at)
   AND status = 'active'
 RETURNING *;

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
   AND status = 'active'
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

-- name: LockRunStartWait :one
SELECT *
  FROM run_waits
 WHERE id = sqlc.arg(id)
   AND environment_id = sqlc.arg(environment_id)
   AND run_id = sqlc.arg(run_id)
   AND workspace_id = sqlc.arg(workspace_id)
 FOR UPDATE;

-- name: LockReadyRunCheckpoint :one
SELECT *
  FROM run_checkpoints
 WHERE id = sqlc.arg(id)
   AND run_id = sqlc.arg(run_id)
   AND attempt_number = sqlc.arg(attempt_number)
   AND run_wait_id = sqlc.arg(run_wait_id)
   AND workspace_id = sqlc.arg(workspace_id)
   AND status = 'ready'
   AND (expires_at IS NULL OR expires_at > transaction_timestamp())
 FOR UPDATE;

-- name: MarkRunLeaseStarting :one
UPDATE run_leases
   SET status = 'starting',
       claimed_at = transaction_timestamp(),
       updated_at = transaction_timestamp()
 WHERE id = sqlc.arg(id)
   AND lease_sequence = sqlc.arg(lease_sequence)
   AND worker_group_id = sqlc.arg(worker_group_id)
   AND worker_instance_id = sqlc.arg(worker_instance_id)
   AND worker_epoch = sqlc.arg(worker_epoch)
   AND status = 'assigned'
   AND start_deadline_at > transaction_timestamp()
   AND expires_at > transaction_timestamp()
RETURNING *;

-- name: GetRunFinalizationTime :one
SELECT clock_timestamp()::timestamptz;

-- name: RunFinalizationScopeIsClear :one
SELECT NOT EXISTS (
           SELECT 1
             FROM run_waits
            WHERE run_waits.run_id = sqlc.arg(run_id)
              AND run_waits.attempt_number = sqlc.arg(attempt_number)
              AND run_waits.workspace_id = sqlc.arg(workspace_id)
              AND run_waits.suspension_status NOT IN ('released', 'cancelled', 'failed')
       )
       AND NOT EXISTS (
           SELECT 1
             FROM workspace_processes
            WHERE workspace_processes.workspace_id = sqlc.arg(workspace_id)
              AND workspace_processes.status IN ('pending', 'starting', 'running', 'exit_requested')
       ) AS clear;

-- name: BeginRunLeaseFinalization :one
UPDATE run_leases
   SET status = 'finalizing',
       expires_at = sqlc.arg(expires_at),
       finalization_operation_id = sqlc.arg(finalization_operation_id),
       finalization_kind = sqlc.arg(finalization_kind),
       finalization_started_at = sqlc.arg(finalization_started_at),
       finalization_request_fingerprint = sqlc.arg(finalization_request_fingerprint),
       updated_at = sqlc.arg(finalization_started_at)
 WHERE id = sqlc.arg(id)
   AND run_id = sqlc.arg(run_id)
   AND workspace_id = sqlc.arg(workspace_id)
   AND attempt_number = sqlc.arg(attempt_number)
   AND lease_sequence = sqlc.arg(lease_sequence)
   AND status = 'running'
   AND expires_at = sqlc.arg(previous_expires_at)
   AND sqlc.arg(expires_at)::timestamptz > expires_at
   AND finalization_operation_id IS NULL
   AND finalization_kind IS NULL
   AND finalization_started_at IS NULL
   AND finalization_request_fingerprint IS NULL
RETURNING *;

-- name: BeginRunWorkspaceLeaseFinalization :one
UPDATE workspace_leases
   SET expires_at = sqlc.arg(expires_at),
       updated_at = sqlc.arg(finalization_started_at)
 WHERE id = sqlc.arg(id)
   AND workspace_id = sqlc.arg(workspace_id)
   AND runtime_instance_id = sqlc.arg(runtime_instance_id)
   AND workspace_mount_id = sqlc.arg(workspace_mount_id)
   AND owner_run_lease_id = sqlc.arg(owner_run_lease_id)
   AND ownership_generation = sqlc.arg(ownership_generation)
   AND writer_generation = sqlc.arg(writer_generation)
   AND mount_fencing_generation = sqlc.arg(mount_fencing_generation)
   AND expires_at = sqlc.arg(previous_expires_at)
   AND sqlc.arg(expires_at)::timestamptz > expires_at
   AND status = 'active'
RETURNING *;

-- name: CloseRunActiveIntervalForFinalization :one
UPDATE runs
   SET active_elapsed_ms = active_elapsed_ms
           + floor(extract(epoch FROM (sqlc.arg(finalization_started_at)::timestamptz - active_started_at)) * 1000)::bigint,
       active_started_at = NULL,
       revision = revision + 1,
       updated_at = sqlc.arg(finalization_started_at)
 WHERE id = sqlc.arg(id)
   AND org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND workspace_id = sqlc.arg(workspace_id)
   AND status = 'running'
   AND current_attempt_number = sqlc.arg(attempt_number)
   AND current_run_lease_id = sqlc.arg(run_lease_id)
   AND revision = sqlc.arg(expected_revision)
   AND active_started_at IS NOT NULL
   AND sqlc.arg(finalization_started_at)::timestamptz >= active_started_at
   AND sqlc.arg(finalization_started_at)::timestamptz < active_started_at
       + ((max_active_duration_ms - active_elapsed_ms) * interval '1 millisecond')
RETURNING *;

-- name: MarkRunLeaseRunning :one
UPDATE run_leases
   SET status = 'running',
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
   AND runtime_instance_id = sqlc.arg(runtime_instance_id)
   AND runtime_identity_id = sqlc.arg(runtime_identity_id)
   AND status = 'starting'
   AND start_deadline_at > transaction_timestamp()
   AND expires_at > transaction_timestamp()
RETURNING *;

-- name: MarkRunRunning :one
UPDATE runs
   SET status = 'running',
       started_at = coalesce(started_at, transaction_timestamp()),
       active_started_at = transaction_timestamp(),
       revision = revision + 1,
       updated_at = transaction_timestamp()
 WHERE id = sqlc.arg(id)
   AND org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND workspace_id = sqlc.arg(workspace_id)
   AND revision = sqlc.arg(expected_revision)
   AND status = 'queued'
   AND current_attempt_number = sqlc.arg(attempt_number)
   AND current_run_lease_id = sqlc.arg(run_lease_id)
   AND active_started_at IS NULL
RETURNING *;

-- name: TouchRunWorkspaceActivity :one
UPDATE workspaces
   SET last_activity_at = greatest(last_activity_at, transaction_timestamp()),
       updated_at = transaction_timestamp()
 WHERE workspaces.id = sqlc.arg(id)
   AND workspaces.environment_id = sqlc.arg(environment_id)
   AND EXISTS (
       SELECT 1 FROM environments
        WHERE environments.id = workspaces.environment_id
          AND environments.org_id = sqlc.arg(org_id)
          AND environments.project_id = sqlc.arg(project_id)
   )
   AND workspaces.ownership_generation = sqlc.arg(ownership_generation)
   AND workspaces.writer_generation = sqlc.arg(writer_generation)
   AND workspaces.status = 'active'
   AND workspaces.desired_state = 'active'
RETURNING workspaces.id, workspaces.environment_id, workspaces.region_id, workspaces.sandbox_declared_id, workspaces.deployment_definition_id, workspaces.key, workspaces.revision, workspaces.owner_session_id, workspaces.owner_run_id, workspaces.ownership_generation, workspaces.writer_generation, workspaces.head_version_id, workspaces.status, workspaces.desired_state, workspaces.dirty_state, workspaces.last_activity_at, workspaces.created_at, workspaces.updated_at, workspaces.deleted_at;

-- name: MarkRunEntrypointEntered :one
UPDATE run_attempts
   SET entrypoint_entered_at = transaction_timestamp()
 WHERE run_id = sqlc.arg(run_id)
   AND number = sqlc.arg(number)
   AND workspace_id = sqlc.arg(workspace_id)
   AND entrypoint_entered_at IS NULL
   AND terminal_at IS NULL
RETURNING *;

-- name: ListRunExecutionLeaseRecoveryCandidates :many
SELECT runs.org_id,
       runs.project_id,
       runs.environment_id,
       runs.id AS run_id,
       runs.workspace_id,
       runs.current_attempt_number,
       run_leases.id AS run_lease_id
  FROM run_leases
  JOIN runs
    ON runs.id = run_leases.run_id
   AND runs.workspace_id = run_leases.workspace_id
   AND runs.current_attempt_number = run_leases.attempt_number
   AND runs.current_run_lease_id = run_leases.id
  JOIN worker_instances
    ON worker_instances.id = run_leases.worker_instance_id
  JOIN runtime_instances
    ON runtime_instances.id = run_leases.runtime_instance_id
   AND runtime_instances.worker_instance_id = run_leases.worker_instance_id
   AND runtime_instances.worker_epoch = run_leases.worker_epoch
   AND runtime_instances.workspace_id = run_leases.workspace_id
   AND runtime_instances.reclaimed_at IS NULL
  JOIN workspace_leases
    ON workspace_leases.owner_run_lease_id = run_leases.id
   AND workspace_leases.workspace_id = run_leases.workspace_id
   AND workspace_leases.runtime_instance_id = run_leases.runtime_instance_id
   AND workspace_leases.status IN ('active', 'releasing')
  JOIN workspace_mounts
    ON workspace_mounts.id = workspace_leases.workspace_mount_id
   AND workspace_mounts.runtime_instance_id = run_leases.runtime_instance_id
   AND workspace_mounts.workspace_id = run_leases.workspace_id
   AND workspace_mounts.status IN ('mounting', 'mounted', 'unmounting', 'lost', 'failed')
 WHERE run_leases.status IN ('assigned', 'starting', 'running', 'checkpointing', 'finalizing')
   AND ((run_leases.status IN ('assigned', 'starting')
         AND runs.status = 'queued'
         AND runs.active_started_at IS NULL)
        OR (run_leases.status = 'running'
            AND runs.status = 'running'
            AND runs.active_started_at IS NOT NULL)
        OR (run_leases.status = 'checkpointing'
            AND runs.status = 'waiting'
            AND runs.active_started_at IS NOT NULL)
        OR (run_leases.status = 'finalizing'
            AND runs.status = 'running'
            AND runs.active_started_at IS NULL
            AND run_leases.finalization_operation_id IS NOT NULL
            AND run_leases.finalization_kind IS NOT NULL
            AND run_leases.finalization_started_at IS NOT NULL
            AND run_leases.finalization_request_fingerprint IS NOT NULL))
   -- Recover uncertain Actors once through Session hold and physical cleanup.
   -- Proven continuations remain in the resume lane; they must not consume
   -- this bounded scan only to be rejected later.
   AND (runs.entrypoint_kind = 'task'
        OR EXISTS (SELECT 1 FROM sessions
                    WHERE sessions.id = runs.session_id
                      AND sessions.current_run_id = runs.id
                      AND sessions.status IN ('open', 'closing')))
   AND NOT EXISTS (
       SELECT 1 FROM run_waits
        WHERE run_waits.run_id = runs.id
          AND run_waits.attempt_number = runs.current_attempt_number
          AND run_waits.current_run_lease_id = run_leases.id
          AND run_waits.suspension_status = 'resuming'
          AND run_leases.status IN ('assigned', 'starting')
          AND (runs.entrypoint_kind = 'task' OR EXISTS (
              SELECT 1 FROM sessions
              JOIN run_checkpoints ON run_checkpoints.id = run_waits.suspend_checkpoint_id
               AND run_checkpoints.run_id = runs.id
               AND run_checkpoints.attempt_number = runs.current_attempt_number
               AND run_checkpoints.run_wait_id = run_waits.id
               AND run_checkpoints.workspace_id = runs.workspace_id
              JOIN workspace_versions ON workspace_versions.id = run_checkpoints.private_workspace_version_id
               AND workspace_versions.workspace_id = run_checkpoints.workspace_id
              JOIN run_leases AS source_run_leases ON source_run_leases.id = run_checkpoints.source_run_lease_id
               AND source_run_leases.run_id = runs.id
               AND source_run_leases.attempt_number = runs.current_attempt_number
               AND source_run_leases.workspace_id = runs.workspace_id
              WHERE sessions.id = runs.session_id AND sessions.current_run_id = runs.id
                AND sessions.dispatch_hold_id IS NULL
                AND run_checkpoints.status = 'ready'
                AND (run_checkpoints.expires_at IS NULL OR run_checkpoints.expires_at > transaction_timestamp())
                AND workspace_versions.status = 'private'
                AND source_run_leases.status = 'checkpointed'
                AND run_checkpoints.actor_speculative_input_sequence
                    BETWEEN sessions.committed_input_sequence AND sessions.next_input_sequence - 1

          ))
   )
   AND (run_leases.expires_at <= transaction_timestamp()
        OR (run_leases.status IN ('assigned', 'starting')
            AND run_leases.start_deadline_at <= transaction_timestamp())
        OR (run_leases.status IN ('running', 'checkpointing')
            AND transaction_timestamp() >= runs.active_started_at
                + (GREATEST(runs.max_active_duration_ms - runs.active_elapsed_ms, 0)::text
                   || ' milliseconds')::interval)
        OR worker_instances.lost_at <= transaction_timestamp()
        OR worker_instances.termination_ready_at <= transaction_timestamp()
        OR worker_instances.current_epoch IS DISTINCT FROM run_leases.worker_epoch
        OR (runtime_instances.observed_state = 'lost' AND runtime_instances.terminal_at <= transaction_timestamp())
        OR (runtime_instances.observed_state = 'failed' AND runtime_instances.terminal_at <= transaction_timestamp())
        OR workspace_mounts.lost_at <= transaction_timestamp()
        OR workspace_mounts.failed_at <= transaction_timestamp())
 ORDER BY LEAST(
              run_leases.expires_at,
              CASE
                  WHEN run_leases.status IN ('assigned', 'starting')
                  THEN run_leases.start_deadline_at
                  WHEN run_leases.status IN ('running', 'checkpointing')
                  THEN runs.active_started_at
                       + (GREATEST(runs.max_active_duration_ms - runs.active_elapsed_ms, 0)::text
                          || ' milliseconds')::interval
                  ELSE 'infinity'::timestamptz
              END,
              COALESCE(worker_instances.lost_at, 'infinity'::timestamptz),
              COALESCE(worker_instances.termination_ready_at, 'infinity'::timestamptz),
              CASE WHEN runtime_instances.observed_state = 'lost' THEN runtime_instances.terminal_at ELSE 'infinity'::timestamptz END,
              CASE WHEN runtime_instances.observed_state = 'failed' THEN runtime_instances.terminal_at ELSE 'infinity'::timestamptz END,
              COALESCE(workspace_mounts.lost_at, 'infinity'::timestamptz),
              COALESCE(workspace_mounts.failed_at, 'infinity'::timestamptz)
          ),
          runs.id
 LIMIT sqlc.arg(limit_count);

-- name: RecoverExpiredRunResumes :many
WITH RECURSIVE candidates AS MATERIALIZED (
    SELECT runs.id AS run_id,
           runs.entrypoint_kind,
           runs.session_id,
           run_leases.id AS run_lease_id,
           run_leases.worker_instance_id,
           run_leases.worker_epoch,
           run_leases.runtime_instance_id,
           workspace_leases.id AS workspace_lease_id,
           workspace_mounts.id AS workspace_mount_id,
           run_waits.id AS run_wait_id,
           run_waits.suspend_checkpoint_id AS restore_checkpoint_id,
           run_waits.condition_status
      FROM runs
      JOIN run_leases
        ON run_leases.id = runs.current_run_lease_id
       AND run_leases.run_id = runs.id
       AND run_leases.attempt_number = runs.current_attempt_number
       AND run_leases.workspace_id = runs.workspace_id
       AND run_leases.status IN ('assigned', 'starting')
      JOIN workspace_leases
        ON workspace_leases.owner_run_lease_id = run_leases.id
       AND workspace_leases.workspace_id = runs.workspace_id
       AND workspace_leases.runtime_instance_id = run_leases.runtime_instance_id
       AND workspace_leases.status IN ('active', 'releasing')
      JOIN run_waits
        ON run_waits.run_id = runs.id
       AND run_waits.attempt_number = runs.current_attempt_number
       AND run_waits.workspace_id = runs.workspace_id
       AND run_waits.current_run_lease_id = run_leases.id
       AND run_waits.suspension_status = 'resuming'
      JOIN workspace_mounts
        ON workspace_mounts.id = workspace_leases.workspace_mount_id
       AND workspace_mounts.runtime_instance_id = run_leases.runtime_instance_id
       AND workspace_mounts.workspace_id = runs.workspace_id
       AND workspace_mounts.status IN ('mounting', 'mounted', 'unmounting', 'lost', 'failed')
      JOIN runtime_instances
        ON runtime_instances.id = run_leases.runtime_instance_id
       AND runtime_instances.workspace_id = runs.workspace_id
       AND runtime_instances.restore_checkpoint_id = run_waits.suspend_checkpoint_id
       AND runtime_instances.reclaimed_at IS NULL
      JOIN worker_instances
        ON worker_instances.id = run_leases.worker_instance_id
     WHERE (run_leases.expires_at <= transaction_timestamp()
            OR run_leases.start_deadline_at <= transaction_timestamp()
            OR (runtime_instances.observed_state = 'lost' AND runtime_instances.terminal_at <= transaction_timestamp())
            OR (runtime_instances.observed_state = 'failed' AND runtime_instances.terminal_at <= transaction_timestamp())
            OR worker_instances.lost_at <= transaction_timestamp()
            OR worker_instances.termination_ready_at <= transaction_timestamp()
            OR worker_instances.current_epoch IS DISTINCT FROM run_leases.worker_epoch
            OR workspace_mounts.lost_at <= transaction_timestamp()
            OR workspace_mounts.failed_at <= transaction_timestamp())
       -- This is an eligibility hint before the bounded scan. All execution,
       -- Session and checkpoint authority is checked again under locks below.
       -- An active Turn may continue only from its recoverable same-attempt
       -- checkpoint; held or nonrecoverable Actors must not starve Task recovery.
       AND (runs.entrypoint_kind = 'task'
            OR EXISTS (
                SELECT 1
                  FROM sessions
                 WHERE sessions.id = runs.session_id
                   AND sessions.current_run_id = runs.id
                   AND sessions.status IN ('open', 'closing')
                   AND sessions.dispatch_hold_id IS NULL
                   AND EXISTS (
                            SELECT 1
                              FROM run_checkpoints
                              JOIN workspace_versions
                                ON workspace_versions.id = run_checkpoints.private_workspace_version_id
                               AND workspace_versions.workspace_id = run_checkpoints.workspace_id
                              JOIN run_leases AS source_run_leases
                                ON source_run_leases.id = run_checkpoints.source_run_lease_id
                               AND source_run_leases.run_id = run_checkpoints.run_id
                               AND source_run_leases.attempt_number = run_checkpoints.attempt_number
                               AND source_run_leases.workspace_id = run_checkpoints.workspace_id
                             WHERE run_checkpoints.id = run_waits.suspend_checkpoint_id
                               AND run_checkpoints.run_id = runs.id
                               AND run_checkpoints.attempt_number = runs.current_attempt_number
                               AND run_checkpoints.run_wait_id = run_waits.id
                               AND run_checkpoints.workspace_id = runs.workspace_id
                               AND run_checkpoints.status = 'ready'
                               AND (run_checkpoints.expires_at IS NULL
                                    OR run_checkpoints.expires_at > transaction_timestamp())
                               AND workspace_versions.status = 'private'
                               AND source_run_leases.status = 'checkpointed'
                               AND run_checkpoints.actor_speculative_input_sequence
                                   BETWEEN sessions.committed_input_sequence
                                       AND sessions.next_input_sequence - 1
                        )
            ))
     ORDER BY runs.id
     LIMIT sqlc.arg(limit_count)
), locked_actor_candidates AS MATERIALIZED (
    SELECT candidates.*,
           sessions.run_generation AS actor_run_generation,
           sessions.committed_input_sequence AS actor_committed_input_sequence,
           sessions.next_input_sequence AS actor_next_input_sequence
      FROM candidates
      JOIN sessions
        ON sessions.id = candidates.session_id
       AND sessions.current_run_id = candidates.run_id
       AND sessions.status IN ('open', 'closing')
       AND sessions.dispatch_hold_id IS NULL
     WHERE candidates.entrypoint_kind = 'actor'
     ORDER BY sessions.id
     FOR UPDATE OF sessions SKIP LOCKED
), placement_candidates AS MATERIALIZED (
    SELECT candidates.*,
           NULL::bigint AS actor_run_generation,
           NULL::bigint AS actor_committed_input_sequence,
           NULL::bigint AS actor_next_input_sequence
      FROM candidates
     WHERE candidates.entrypoint_kind = 'task'
       AND candidates.session_id IS NULL
    UNION ALL
    SELECT locked_actor_candidates.*
      FROM locked_actor_candidates
), locked_runs AS MATERIALIZED (
    SELECT runs.org_id,
           runs.project_id,
           runs.environment_id,
           runs.workspace_id,
           runs.id AS run_id,
           runs.revision,
           runs.current_attempt_number,
           runs.session_input_start_sequence,
           runs.session_input_high_watermark,
           placement_candidates.entrypoint_kind,
           placement_candidates.session_id,
           placement_candidates.actor_run_generation,
           placement_candidates.actor_committed_input_sequence,
           placement_candidates.actor_next_input_sequence,
           placement_candidates.run_lease_id,
           placement_candidates.worker_instance_id,
           placement_candidates.worker_epoch,
           placement_candidates.runtime_instance_id,
           placement_candidates.workspace_lease_id,
           placement_candidates.workspace_mount_id,
           placement_candidates.run_wait_id,
           placement_candidates.restore_checkpoint_id,
           placement_candidates.condition_status
      FROM placement_candidates
      JOIN runs ON runs.id = placement_candidates.run_id
     WHERE ((runs.entrypoint_kind = 'task'
             AND runs.session_id IS NULL
             AND placement_candidates.entrypoint_kind = 'task')
            OR (runs.entrypoint_kind = 'actor'
                AND runs.session_id = placement_candidates.session_id
                AND runs.cause_kind IN ('actor_start', 'continuation')
                AND placement_candidates.entrypoint_kind = 'actor'))
       AND runs.status = 'queued' AND runs.active_started_at IS NULL
       AND runs.current_run_lease_id = placement_candidates.run_lease_id
     ORDER BY runs.id
     FOR UPDATE OF runs SKIP LOCKED
), same_workspace_ancestors AS (
    SELECT locked_runs.run_id,
           edge.id AS wait_id,
           parent.id AS parent_run_id,
           parent.environment_id,
           parent.parent_run_id AS next_parent_run_id,
           parent.parent_owns_lifecycle,
           parent.session_id AS parent_session_id,
           edge.attempt_number AS parent_attempt_number,
           edge.expected_run_revision AS expected_parent_revision,
           edge.prior_run_lease_id AS parent_run_lease_id,
           edge.suspend_checkpoint_id,
           edge.base_workspace_version_id,
           edge.ownership_generation,
           edge.parent_writer_generation,
           edge.child_writer_generation,
           0 AS depth
      FROM locked_runs
      JOIN run_waits AS edge
        ON edge.child_run_id = locked_runs.run_id
       AND edge.workspace_id = locked_runs.workspace_id
       AND edge.kind = 'child'
       AND EXISTS (
           SELECT 1 FROM runs AS owned_child
            WHERE owned_child.id = edge.child_run_id
              AND owned_child.parent_run_id = edge.run_id
              AND owned_child.environment_id = edge.environment_id
              AND owned_child.parent_owns_lifecycle IS TRUE
       )
       AND edge.condition_status = 'pending'
       AND edge.suspension_status = 'parked'
       AND edge.ownership_generation IS NOT NULL
       AND edge.parent_writer_generation IS NOT NULL
       AND edge.child_writer_generation IS NOT NULL
       AND edge.resume_writer_generation IS NULL
      JOIN runs AS parent
        ON parent.environment_id = edge.environment_id
       AND parent.id = edge.run_id
       AND parent.workspace_id = edge.workspace_id
       AND parent.status = 'waiting'
       AND parent.current_run_lease_id IS NULL
    UNION ALL
    SELECT child.run_id,
           edge.id,
           parent.id,
           parent.environment_id,
           parent.parent_run_id,
           parent.parent_owns_lifecycle,
           parent.session_id,
           edge.attempt_number,
           edge.expected_run_revision,
           edge.prior_run_lease_id,
           edge.suspend_checkpoint_id,
           edge.base_workspace_version_id,
           edge.ownership_generation,
           edge.parent_writer_generation,
           edge.child_writer_generation,
           child.depth + 1
      FROM same_workspace_ancestors AS child
      JOIN run_waits AS edge
        ON edge.child_run_id = child.parent_run_id
       AND edge.kind = 'child'
       AND edge.run_id = child.next_parent_run_id
       AND edge.environment_id = child.environment_id
       AND edge.condition_status = 'pending'
       AND edge.suspension_status = 'parked'
       AND edge.ownership_generation = child.ownership_generation
       AND edge.child_writer_generation = child.parent_writer_generation
       AND edge.resume_writer_generation IS NULL
      JOIN runs AS parent
        ON parent.environment_id = edge.environment_id
       AND parent.id = edge.run_id
       AND parent.workspace_id = edge.workspace_id
       AND parent.status = 'waiting'
       AND parent.current_run_lease_id IS NULL
     WHERE child.parent_owns_lifecycle IS TRUE
), locked_same_workspace_ancestors AS MATERIALIZED (
    SELECT ancestors.*
      FROM same_workspace_ancestors AS ancestors
      JOIN run_waits AS edge ON edge.id = ancestors.wait_id
      JOIN runs AS parent ON parent.id = ancestors.parent_run_id
     ORDER BY ancestors.run_id, ancestors.depth DESC
     FOR UPDATE OF edge, parent
), locked_workspaces AS MATERIALIZED (
    SELECT locked_runs.*,
           workspaces.ownership_generation,
           workspaces.writer_generation,
           EXISTS (
               SELECT 1
                 FROM locked_same_workspace_ancestors AS nested
                WHERE nested.run_id = locked_runs.run_id
                  AND nested.depth = 0
           ) AS nested_same_workspace,
           (
               SELECT nested.wait_id
                 FROM locked_same_workspace_ancestors AS nested
                WHERE nested.run_id = locked_runs.run_id
                  AND nested.depth = 0
           ) AS enclosing_wait_id,
           (
               SELECT nested.parent_run_id
                 FROM locked_same_workspace_ancestors AS nested
                WHERE nested.run_id = locked_runs.run_id
                  AND nested.depth = 0
           ) AS enclosing_parent_run_id,
           (
               SELECT nested.parent_attempt_number
                 FROM locked_same_workspace_ancestors AS nested
                WHERE nested.run_id = locked_runs.run_id
                  AND nested.depth = 0
           ) AS enclosing_parent_attempt_number,
           (
               SELECT nested.expected_parent_revision
                 FROM locked_same_workspace_ancestors AS nested
                WHERE nested.run_id = locked_runs.run_id
                  AND nested.depth = 0
           ) AS enclosing_expected_parent_revision,
           (
               SELECT nested.parent_run_lease_id
                 FROM locked_same_workspace_ancestors AS nested
                WHERE nested.run_id = locked_runs.run_id
                  AND nested.depth = 0
           ) AS enclosing_parent_run_lease_id,
           (
               SELECT nested.suspend_checkpoint_id
                 FROM locked_same_workspace_ancestors AS nested
                WHERE nested.run_id = locked_runs.run_id
                  AND nested.depth = 0
           ) AS enclosing_suspend_checkpoint_id,
           (
               SELECT nested.base_workspace_version_id
                 FROM locked_same_workspace_ancestors AS nested
                WHERE nested.run_id = locked_runs.run_id
                  AND nested.depth = 0
           ) AS enclosing_base_workspace_version_id,
           (
               SELECT nested.child_writer_generation
                 FROM locked_same_workspace_ancestors AS nested
                WHERE nested.run_id = locked_runs.run_id
                  AND nested.depth = 0
           ) AS enclosing_child_writer_generation
      FROM locked_runs
      JOIN workspaces ON workspaces.id = locked_runs.workspace_id
     WHERE workspaces.environment_id = locked_runs.environment_id
       AND ((locked_runs.entrypoint_kind = 'task'
             AND ((workspaces.owner_run_id = locked_runs.run_id
                   AND workspaces.owner_session_id IS NULL)
                  OR EXISTS (
                          SELECT 1
                            FROM locked_same_workspace_ancestors AS root
                           WHERE root.run_id = locked_runs.run_id
                             AND (root.next_parent_run_id IS NULL
                                  OR root.parent_owns_lifecycle IS NOT TRUE)
                             AND root.ownership_generation = workspaces.ownership_generation
                             AND ((root.parent_session_id IS NULL
                                   AND workspaces.owner_run_id = root.parent_run_id
                                   AND workspaces.owner_session_id IS NULL)
                                  OR (root.parent_session_id IS NOT NULL
                                      AND workspaces.owner_session_id = root.parent_session_id
                                      AND workspaces.owner_run_id IS NULL))
                      )))
            OR (locked_runs.entrypoint_kind = 'actor'
                AND workspaces.owner_session_id = locked_runs.session_id
                AND workspaces.owner_run_id IS NULL))
       AND workspaces.status = 'active'
       AND workspaces.desired_state = 'active'
       AND workspaces.dirty_state = 'clean'
     ORDER BY workspaces.id
     FOR UPDATE OF workspaces
), locked_attempts AS MATERIALIZED (
    SELECT locked_workspaces.*
      FROM locked_workspaces
      JOIN run_attempts
        ON run_attempts.run_id = locked_workspaces.run_id
       AND run_attempts.number = locked_workspaces.current_attempt_number
       AND run_attempts.workspace_id = locked_workspaces.workspace_id
       AND run_attempts.entrypoint_kind = locked_workspaces.entrypoint_kind
       AND run_attempts.terminal_at IS NULL
       AND (locked_workspaces.entrypoint_kind = 'task'
            OR (run_attempts.session_input_start_sequence IS NOT NULL
                AND run_attempts.session_input_start_sequence = locked_workspaces.session_input_start_sequence
                AND locked_workspaces.session_input_start_sequence
                    <= locked_workspaces.session_input_high_watermark
                AND locked_workspaces.actor_committed_input_sequence
                    >= locked_workspaces.session_input_start_sequence
                AND locked_workspaces.actor_committed_input_sequence
                    < locked_workspaces.actor_next_input_sequence))
     ORDER BY run_attempts.run_id, run_attempts.number
     FOR UPDATE OF run_attempts
), locked_workers AS MATERIALIZED (
    SELECT locked_attempts.*,
           LEAST(
               COALESCE(worker_instances.lost_at, 'infinity'::timestamptz),
               COALESCE(worker_instances.termination_ready_at, 'infinity'::timestamptz),
               CASE
                   WHEN worker_instances.current_epoch IS DISTINCT FROM locked_attempts.worker_epoch
                   THEN COALESCE(worker_instances.epoch_started_at, worker_instances.updated_at)
                   ELSE 'infinity'::timestamptz
               END
           ) AS worker_lost_at
      FROM locked_attempts
      JOIN worker_instances
        ON worker_instances.id = locked_attempts.worker_instance_id
     ORDER BY worker_instances.id
     FOR UPDATE OF worker_instances
), locked_runtimes AS MATERIALIZED (
    SELECT locked_workers.*,
           CASE WHEN runtime_instances.observed_state = 'lost' THEN runtime_instances.terminal_at END::timestamptz AS runtime_lost_at,
           CASE WHEN runtime_instances.observed_state = 'failed' THEN runtime_instances.terminal_at END::timestamptz AS runtime_failed_at
      FROM locked_workers
      JOIN runtime_instances
        ON runtime_instances.id = locked_workers.runtime_instance_id
       AND runtime_instances.org_id = locked_workers.org_id
       AND runtime_instances.worker_instance_id = locked_workers.worker_instance_id
       AND runtime_instances.worker_epoch = locked_workers.worker_epoch
       AND runtime_instances.workspace_id = locked_workers.workspace_id
       AND runtime_instances.restore_checkpoint_id = locked_workers.restore_checkpoint_id
       AND runtime_instances.reclaimed_at IS NULL
     ORDER BY runtime_instances.id
     FOR UPDATE OF runtime_instances
), locked_run_leases AS MATERIALIZED (
    SELECT locked_runtimes.*,
           run_leases.status AS run_lease_status,
           run_leases.expires_at AS run_lease_expires_at,
           run_leases.start_deadline_at
      FROM locked_runtimes
      JOIN run_leases
        ON run_leases.id = locked_runtimes.run_lease_id
       AND run_leases.org_id = locked_runtimes.org_id
       AND run_leases.run_id = locked_runtimes.run_id
       AND run_leases.attempt_number = locked_runtimes.current_attempt_number
       AND run_leases.workspace_id = locked_runtimes.workspace_id
       AND run_leases.worker_instance_id = locked_runtimes.worker_instance_id
       AND run_leases.worker_epoch = locked_runtimes.worker_epoch
       AND run_leases.runtime_instance_id = locked_runtimes.runtime_instance_id
       AND run_leases.status IN ('assigned', 'starting')
     ORDER BY run_leases.id
     FOR UPDATE OF run_leases
), locked_mounts AS MATERIALIZED (
    SELECT locked_run_leases.*,
           workspace_mounts.lost_at AS mount_lost_at,
           workspace_mounts.failed_at AS mount_failed_at
      FROM locked_run_leases
      JOIN workspace_mounts
        ON workspace_mounts.id = locked_run_leases.workspace_mount_id
       AND workspace_mounts.runtime_instance_id = locked_run_leases.runtime_instance_id
       AND workspace_mounts.workspace_id = locked_run_leases.workspace_id
       AND workspace_mounts.worker_instance_id = locked_run_leases.worker_instance_id
       AND workspace_mounts.worker_epoch = locked_run_leases.worker_epoch
       AND workspace_mounts.status IN ('mounting', 'mounted', 'unmounting', 'lost', 'failed')
     ORDER BY workspace_mounts.id
     FOR UPDATE OF workspace_mounts
), locked_workspace_leases AS MATERIALIZED (
    SELECT locked_mounts.*,
           workspace_leases.expires_at AS workspace_lease_expires_at,
           workspace_leases.base_workspace_version_id AS restore_workspace_version_id
      FROM locked_mounts
      JOIN workspace_leases
        ON workspace_leases.id = locked_mounts.workspace_lease_id
       AND workspace_leases.owner_run_lease_id = locked_mounts.run_lease_id
       AND workspace_leases.workspace_id = locked_mounts.workspace_id
       AND workspace_leases.workspace_mount_id = locked_mounts.workspace_mount_id
       AND workspace_leases.runtime_instance_id = locked_mounts.runtime_instance_id
       AND workspace_leases.ownership_generation = locked_mounts.ownership_generation
       AND workspace_leases.writer_generation = locked_mounts.writer_generation
       AND workspace_leases.status = 'active'
       AND workspace_leases.expires_at = locked_mounts.run_lease_expires_at
     ORDER BY workspace_leases.id
     FOR UPDATE OF workspace_leases
), locked_waits AS MATERIALIZED (
    SELECT locked_workspace_leases.*,
           run_waits.resume_request_version
      FROM locked_workspace_leases
      JOIN run_waits
        ON run_waits.id = locked_workspace_leases.run_wait_id
       AND run_waits.run_id = locked_workspace_leases.run_id
       AND run_waits.attempt_number = locked_workspace_leases.current_attempt_number
       AND run_waits.workspace_id = locked_workspace_leases.workspace_id
       AND run_waits.current_run_lease_id = locked_workspace_leases.run_lease_id
       AND run_waits.suspension_status = 'resuming'
       AND run_waits.suspend_checkpoint_id = locked_workspace_leases.restore_checkpoint_id
       AND run_waits.condition_status = locked_workspace_leases.condition_status
       AND (run_waits.resume_writer_generation IS NULL
            OR run_waits.resume_writer_generation = locked_workspace_leases.writer_generation)
       AND (run_waits.resume_workspace_version_id IS NULL
            OR run_waits.resume_workspace_version_id
                = locked_workspace_leases.restore_workspace_version_id)
     ORDER BY run_waits.id
     FOR UPDATE OF run_waits
), loss_authority AS MATERIALIZED (
    SELECT locked_waits.*,
           physical_loss.physical_loss_at,
           physical_failure.physical_failure_at,
           LEAST(
               locked_waits.run_lease_expires_at,
               locked_waits.start_deadline_at,
               physical_loss.physical_loss_at,
               physical_failure.physical_failure_at
           ) AS authority_loss_at
      FROM locked_waits
      CROSS JOIN LATERAL (
          SELECT LEAST(
              COALESCE(locked_waits.worker_lost_at, 'infinity'::timestamptz),
              COALESCE(locked_waits.runtime_lost_at, 'infinity'::timestamptz),
              COALESCE(locked_waits.mount_lost_at, 'infinity'::timestamptz)
          ) AS physical_loss_at
      ) AS physical_loss
      CROSS JOIN LATERAL (
          SELECT LEAST(
              COALESCE(locked_waits.runtime_failed_at, 'infinity'::timestamptz),
              COALESCE(locked_waits.mount_failed_at, 'infinity'::timestamptz)
          ) AS physical_failure_at
      ) AS physical_failure
), locked_checkpoints AS MATERIALIZED (
    SELECT loss_authority.*,
           (run_checkpoints.status = 'ready'
            AND (run_checkpoints.expires_at IS NULL
                 OR run_checkpoints.expires_at > transaction_timestamp())
            AND workspace_versions.status = 'private'
            AND source_run_leases.status = 'checkpointed'
            AND ((loss_authority.entrypoint_kind = 'task'
                  AND run_checkpoints.actor_speculative_input_sequence IS NULL)
                 OR (loss_authority.entrypoint_kind = 'actor'
                     AND run_checkpoints.actor_speculative_input_sequence
                         BETWEEN loss_authority.actor_committed_input_sequence
                             AND loss_authority.actor_next_input_sequence - 1))
            ) AS checkpoint_recoverable,
           'restore_checkpoint_unavailable' AS recovery_terminal_reason_code
      FROM loss_authority
      JOIN run_checkpoints
        ON run_checkpoints.id = loss_authority.restore_checkpoint_id
       AND run_checkpoints.run_id = loss_authority.run_id
       AND run_checkpoints.attempt_number = loss_authority.current_attempt_number
       AND run_checkpoints.run_wait_id = loss_authority.run_wait_id
       AND run_checkpoints.workspace_id = loss_authority.workspace_id
      JOIN workspace_versions
        ON workspace_versions.id = run_checkpoints.private_workspace_version_id
       AND workspace_versions.workspace_id = run_checkpoints.workspace_id
      JOIN run_leases AS source_run_leases
        ON source_run_leases.id = run_checkpoints.source_run_lease_id
       AND source_run_leases.run_id = run_checkpoints.run_id
       AND source_run_leases.attempt_number = run_checkpoints.attempt_number
       AND source_run_leases.workspace_id = run_checkpoints.workspace_id
     ORDER BY run_checkpoints.id
     FOR UPDATE OF run_checkpoints, workspace_versions
), expired_run_leases AS (
    UPDATE run_leases
       SET status = 'expired',
           terminal_at = transaction_timestamp(),
           terminal_reason_code = CASE
               WHEN locked_checkpoints.authority_loss_at = locked_checkpoints.physical_failure_at
               THEN 'runtime_failed'
               WHEN locked_checkpoints.authority_loss_at = locked_checkpoints.physical_loss_at
               THEN 'worker_lost'
               ELSE 'lease_expired'
           END,
           updated_at = transaction_timestamp()
      FROM locked_checkpoints
     WHERE run_leases.id = locked_checkpoints.run_lease_id
       AND run_leases.status = locked_checkpoints.run_lease_status
       AND run_leases.expires_at = locked_checkpoints.run_lease_expires_at
       AND run_leases.start_deadline_at = locked_checkpoints.start_deadline_at
       AND locked_checkpoints.authority_loss_at <= transaction_timestamp()
       -- An Actor without continuation proof is held and fenced by the
       -- execution-loss owner; this SQL branch may terminalize only Tasks.
       AND (locked_checkpoints.checkpoint_recoverable
            OR locked_checkpoints.entrypoint_kind = 'task')
    RETURNING run_leases.id, locked_checkpoints.checkpoint_recoverable
), expired_workspace_leases AS (
    UPDATE workspace_leases
       SET status = 'expired',
           terminal_at = transaction_timestamp(),
           terminal_reason_code = CASE
               WHEN locked_checkpoints.authority_loss_at = locked_checkpoints.physical_failure_at
               THEN 'runtime_failed'
               WHEN locked_checkpoints.authority_loss_at = locked_checkpoints.physical_loss_at
               THEN 'worker_lost'
               ELSE 'lease_expired'
           END,
           updated_at = transaction_timestamp()
      FROM locked_checkpoints, expired_run_leases
     WHERE workspace_leases.id = locked_checkpoints.workspace_lease_id
       AND expired_run_leases.id = locked_checkpoints.run_lease_id
       AND workspace_leases.status = 'active'
       AND workspace_leases.expires_at = locked_checkpoints.workspace_lease_expires_at
       AND workspace_leases.expires_at = locked_checkpoints.run_lease_expires_at
    RETURNING workspace_leases.id, expired_run_leases.checkpoint_recoverable
), requeued_runs AS (
    UPDATE runs
       SET status = 'queued',
           current_run_lease_id = NULL,
           revision = runs.revision + 1,
           updated_at = transaction_timestamp()
      FROM locked_checkpoints, expired_workspace_leases
     WHERE runs.id = locked_checkpoints.run_id
       AND runs.org_id = locked_checkpoints.org_id
       AND runs.current_run_lease_id = locked_checkpoints.run_lease_id
       AND runs.revision = locked_checkpoints.revision
       AND runs.status = 'queued'
       AND runs.active_started_at IS NULL
       AND expired_workspace_leases.id = locked_checkpoints.workspace_lease_id
       AND expired_workspace_leases.checkpoint_recoverable
    RETURNING runs.org_id, runs.id, runs.revision
), requeued_waits AS (
    UPDATE run_waits
       SET suspension_status = 'resume_pending',
           current_run_lease_id = NULL,
           resume_writer_generation = NULL,
           resume_request_version = run_waits.resume_request_version + 1,
           expected_run_revision = requeued_runs.revision,
           updated_at = transaction_timestamp()
      FROM locked_checkpoints, requeued_runs
     WHERE run_waits.id = locked_checkpoints.run_wait_id
       AND run_waits.run_id = requeued_runs.id
       AND run_waits.current_run_lease_id = locked_checkpoints.run_lease_id
       AND run_waits.suspension_status = 'resuming'
       AND run_waits.resume_request_version = locked_checkpoints.resume_request_version
    RETURNING run_waits.id, requeued_runs.org_id, requeued_runs.id AS run_id
), failed_attempts AS (
    UPDATE run_attempts
       SET terminal_outcome = 'failed',
           terminal_reason_code = locked_checkpoints.recovery_terminal_reason_code,
           terminal_at = transaction_timestamp()
      FROM locked_checkpoints, expired_workspace_leases
     WHERE run_attempts.run_id = locked_checkpoints.run_id
       AND run_attempts.number = locked_checkpoints.current_attempt_number
       AND run_attempts.workspace_id = locked_checkpoints.workspace_id
       AND run_attempts.terminal_at IS NULL
       AND expired_workspace_leases.id = locked_checkpoints.workspace_lease_id
       AND NOT expired_workspace_leases.checkpoint_recoverable
    RETURNING run_attempts.run_id, run_attempts.number
), failed_runs AS (
    UPDATE runs
       SET status = 'system_failed',
	       failure = jsonb_build_object(
	           'code', locked_checkpoints.recovery_terminal_reason_code,
	           'message', 'Run recovery failed',
	           'details', jsonb_build_object()
	       ),
           current_run_lease_id = NULL,
           revision = runs.revision + 1,
           terminal_at = transaction_timestamp(),
           updated_at = transaction_timestamp()
      FROM locked_checkpoints, failed_attempts
     WHERE runs.id = locked_checkpoints.run_id
       AND runs.org_id = locked_checkpoints.org_id
       AND runs.current_run_lease_id = locked_checkpoints.run_lease_id
       AND runs.revision = locked_checkpoints.revision
       AND runs.status = 'queued'
       AND runs.active_started_at IS NULL
       AND failed_attempts.run_id = locked_checkpoints.run_id
       AND failed_attempts.number = locked_checkpoints.current_attempt_number
    RETURNING runs.id,
              runs.org_id,
              runs.project_id,
              runs.environment_id,
              runs.current_attempt_number,
              runs.revision,
              runs.failure,
              runs.trace_id,
              runs.root_span_id
), failed_waits AS (
    UPDATE run_waits
       SET suspension_status = 'failed',
           current_run_lease_id = NULL,
           suspension_terminal_at = transaction_timestamp(),
           suspension_reason_code = locked_checkpoints.recovery_terminal_reason_code,
           updated_at = transaction_timestamp()
      FROM locked_checkpoints, failed_runs
     WHERE run_waits.id = locked_checkpoints.run_wait_id
       AND run_waits.run_id = failed_runs.id
       AND run_waits.current_run_lease_id = locked_checkpoints.run_lease_id
       AND run_waits.suspension_status = 'resuming'
       AND run_waits.resume_request_version = locked_checkpoints.resume_request_version
    RETURNING run_waits.id, failed_runs.id AS run_id
), failed_enclosing_parents AS (
    UPDATE runs
       SET status = 'queued',
           revision = runs.revision + 1,
           updated_at = transaction_timestamp()
      FROM locked_checkpoints, failed_runs, failed_waits
     WHERE locked_checkpoints.nested_same_workspace
       AND runs.id = locked_checkpoints.enclosing_parent_run_id
       AND runs.environment_id = locked_checkpoints.environment_id
       AND runs.workspace_id = locked_checkpoints.workspace_id
       AND runs.status = 'waiting'
       AND runs.revision = locked_checkpoints.enclosing_expected_parent_revision
       AND runs.current_attempt_number = locked_checkpoints.enclosing_parent_attempt_number
       AND runs.current_run_lease_id IS NULL
       AND failed_runs.id = locked_checkpoints.run_id
       AND failed_waits.run_id = failed_runs.id
    RETURNING runs.id, runs.revision
), failed_enclosing_waits AS (
    UPDATE run_waits
       SET condition_status = 'failed',
           condition_error = failed_runs.failure,
           condition_terminal_at = transaction_timestamp(),
           condition_reason_code = locked_checkpoints.recovery_terminal_reason_code,
           suspension_status = 'resume_pending',
           resume_request_version = run_waits.resume_request_version + 1,
           expected_run_revision = failed_enclosing_parents.revision,
           resume_workspace_version_id = run_waits.base_workspace_version_id,
           updated_at = transaction_timestamp()
      FROM locked_checkpoints, failed_runs, failed_waits, failed_enclosing_parents
     WHERE locked_checkpoints.nested_same_workspace
       AND run_waits.id = locked_checkpoints.enclosing_wait_id
       AND run_waits.environment_id = locked_checkpoints.environment_id
       AND run_waits.run_id = failed_enclosing_parents.id
       AND run_waits.workspace_id = locked_checkpoints.workspace_id
       AND run_waits.attempt_number = locked_checkpoints.enclosing_parent_attempt_number
       AND run_waits.child_run_id = failed_runs.id
       AND run_waits.kind = 'child'
       AND run_waits.condition_status = 'pending'
       AND run_waits.suspension_status = 'parked'
       AND run_waits.expected_run_revision = locked_checkpoints.enclosing_expected_parent_revision
       AND run_waits.current_run_lease_id IS NULL
       AND run_waits.prior_run_lease_id = locked_checkpoints.enclosing_parent_run_lease_id
       AND run_waits.suspend_checkpoint_id = locked_checkpoints.enclosing_suspend_checkpoint_id
       AND run_waits.base_workspace_version_id = locked_checkpoints.enclosing_base_workspace_version_id
       AND run_waits.child_writer_generation = locked_checkpoints.enclosing_child_writer_generation
       AND failed_waits.run_id = failed_runs.id
    RETURNING run_waits.id, failed_runs.id AS run_id
), released_owners AS (
    UPDATE workspaces
       SET owner_run_id = NULL,
           ownership_generation = workspaces.ownership_generation + 1,
           revision = workspaces.revision + 1,
           last_activity_at = transaction_timestamp(),
           updated_at = transaction_timestamp()
      FROM locked_checkpoints
      JOIN failed_waits ON failed_waits.run_id = locked_checkpoints.run_id
     WHERE workspaces.id = locked_checkpoints.workspace_id
       AND NOT locked_checkpoints.nested_same_workspace
       AND locked_checkpoints.entrypoint_kind = 'task'
       AND workspaces.owner_run_id = failed_waits.run_id
       AND workspaces.owner_session_id IS NULL
       AND workspaces.ownership_generation = locked_checkpoints.ownership_generation
       AND workspaces.writer_generation = locked_checkpoints.writer_generation
    RETURNING workspaces.id
), terminal_events AS (
    INSERT INTO telemetry_outbox (
        org_id,
        stream_kind,
        source_kind,
        source_id,
        project_id,
        environment_id,
        run_id,
        run_lease_id,
        attempt_number,
        trace_id,
        span_id,
        category,
        severity,
        source,
        kind,
        message,
        payload,
        redaction_class,
        snapshot_version,
        observed_at
    )
    SELECT failed_runs.org_id,
           'event',
           'run',
           failed_runs.id,
           failed_runs.project_id,
           failed_runs.environment_id,
           failed_runs.id,
           locked_checkpoints.run_lease_id,
           failed_runs.current_attempt_number,
           failed_runs.trace_id,
           failed_runs.root_span_id,
           'lifecycle',
           'error',
           'control',
           'run.system_failed',
           'Run restore Checkpoint became unavailable',
           jsonb_build_object('reasonCode', locked_checkpoints.recovery_terminal_reason_code),
           'internal',
           failed_runs.revision,
           transaction_timestamp()
      FROM failed_runs
      JOIN locked_checkpoints ON locked_checkpoints.run_id = failed_runs.id
      JOIN failed_waits ON failed_waits.run_id = failed_runs.id
      LEFT JOIN released_owners ON released_owners.id = locked_checkpoints.workspace_id
      LEFT JOIN failed_enclosing_waits ON failed_enclosing_waits.run_id = failed_runs.id
     WHERE released_owners.id IS NOT NULL
        OR failed_enclosing_waits.id IS NOT NULL
    RETURNING run_id
), closing_runtimes AS (
    UPDATE runtime_instances
       SET desired_state = 'closed',
           desired_version = desired_version + 1,
           desired_at = transaction_timestamp(),
           desired_reason = 'run_resume_lease_expired',
           updated_at = transaction_timestamp()
      FROM locked_checkpoints
      LEFT JOIN requeued_waits ON requeued_waits.id = locked_checkpoints.run_wait_id
      LEFT JOIN failed_waits ON failed_waits.id = locked_checkpoints.run_wait_id
     WHERE runtime_instances.id = locked_checkpoints.runtime_instance_id
       AND (requeued_waits.id IS NOT NULL OR failed_waits.id IS NOT NULL)
       AND runtime_instances.desired_state = 'ready'
    RETURNING runtime_instances.id
), unmounting AS (
    UPDATE workspace_mounts
       SET status = 'unmounting',
           stopped_at = coalesce(stopped_at, transaction_timestamp()),
           updated_at = transaction_timestamp()
      FROM locked_checkpoints, closing_runtimes
     WHERE workspace_mounts.id = locked_checkpoints.workspace_mount_id
       AND closing_runtimes.id = locked_checkpoints.runtime_instance_id
       AND workspace_mounts.status = 'mounted'
    RETURNING workspace_mounts.id
)
SELECT requeued_waits.id, requeued_waits.org_id, requeued_waits.run_id
  FROM requeued_waits
  JOIN locked_checkpoints ON locked_checkpoints.run_wait_id = requeued_waits.id
  LEFT JOIN closing_runtimes ON closing_runtimes.id = locked_checkpoints.runtime_instance_id
  LEFT JOIN unmounting ON unmounting.id = locked_checkpoints.workspace_mount_id
 ORDER BY requeued_waits.id;
