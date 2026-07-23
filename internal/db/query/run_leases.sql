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

-- name: GetRunLeaseStartLocators :one
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
       runs.parent_run_id,
       workspace_leases.id AS workspace_lease_id,
       workspace_leases.workspace_mount_id,
       run_waits.id AS run_wait_id,
       CASE
           WHEN run_waits.condition_state = 'completed'
               THEN run_waits.handoff_resume_checkpoint_id
           ELSE run_waits.suspend_checkpoint_id
       END::uuid AS run_wait_checkpoint_id,
       run_waits.resume_attach_id,
       run_waits.resume_request_version,
       run_waits.child_run_id AS resume_child_run_id,
       run_waits.child_parent_owned AS resume_child_parent_owned,
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
       (run_leases.state = 'starting' AND runs.status = 'queued')
       OR (run_leases.state = 'running' AND runs.status = 'running')
   )
  JOIN worker_groups
    ON worker_groups.id = run_leases.worker_group_id
   AND worker_groups.region_id = run_leases.region_id
   AND worker_groups.state IN ('active', 'draining')
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
    ON run_waits.run_id = runs.id
   AND run_waits.attempt_number = runs.current_attempt_number
   AND run_waits.workspace_id = runs.workspace_id
   AND run_waits.current_run_lease_id = run_leases.id
   AND run_waits.prior_run_lease_id IS NOT NULL
   AND run_waits.prior_run_lease_id IS DISTINCT FROM run_leases.id
   AND run_waits.suspension_state IN ('resuming', 'released')
  LEFT JOIN run_waits AS enclosing_waits
    ON enclosing_waits.run_id = runs.parent_run_id
   AND enclosing_waits.workspace_id = runs.workspace_id
   AND enclosing_waits.child_run_id = runs.id
   AND enclosing_waits.child_parent_owned IS TRUE
   AND (
       (run_leases.state = 'starting'
        AND enclosing_waits.condition_state = 'pending'
        AND enclosing_waits.suspension_state = 'parked')
       OR run_leases.state = 'running'
   )
 WHERE run_leases.id = sqlc.arg(id)
   AND run_leases.lease_sequence = sqlc.arg(lease_sequence)
   AND run_leases.worker_group_id = sqlc.arg(worker_group_id)
   AND run_leases.worker_instance_id = sqlc.arg(worker_instance_id)
   AND run_leases.worker_epoch = sqlc.arg(worker_epoch)
   AND run_leases.worker_protocol_version = sqlc.arg(worker_protocol_version)
   AND run_leases.state IN ('starting', 'running')
   AND run_leases.expires_at > transaction_timestamp()
   AND (run_leases.state = 'running'
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

-- name: GetLiveRunLeaseLocators :one
SELECT run_leases.org_id,
       run_leases.project_id,
       run_leases.environment_id,
       run_leases.run_id,
       run_leases.workspace_id,
       run_leases.attempt_number,
       runs.actor_id,
       runs.parent_run_id,
       runs.parent_owns_lifecycle,
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
   AND runs.status IN ('running', 'waiting')
  JOIN worker_groups
    ON worker_groups.id = run_leases.worker_group_id
   AND worker_groups.region_id = run_leases.region_id
   AND worker_groups.state IN ('active', 'draining')
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
   AND run_leases.state IN ('running', 'checkpointing', 'finalizing')
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

-- name: LockRunStartLease :one
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

-- name: LockLiveRunLease :one
SELECT *
  FROM run_leases
 WHERE id = sqlc.arg(id)
   AND run_id = sqlc.arg(run_id)
   AND workspace_id = sqlc.arg(workspace_id)
   AND attempt_number = sqlc.arg(attempt_number)
   AND lease_sequence = sqlc.arg(lease_sequence)
   AND state IN ('running', 'checkpointing', 'finalizing')
   AND expires_at > transaction_timestamp()
 FOR UPDATE;

-- name: GetActorInputSendSource :one
SELECT environment_id, run_id
  FROM run_leases
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
   AND requested_cpu_millis = sqlc.arg(requested_cpu_millis)
   AND requested_memory_bytes = sqlc.arg(requested_memory_bytes)
   AND requested_workload_disk_bytes = sqlc.arg(requested_workload_disk_bytes)
   AND requested_scratch_bytes = sqlc.arg(requested_scratch_bytes)
   AND requested_execution_slots = sqlc.arg(requested_execution_slots)
   AND start_deadline_at = sqlc.arg(start_deadline_at)
   AND expires_at = sqlc.arg(expires_at);

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
   AND state IN ('running', 'checkpointing')
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
   AND state = 'active'
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

-- name: LockRunStartWait :one
SELECT *
  FROM run_waits
 WHERE id = sqlc.arg(id)
   AND environment_id = sqlc.arg(environment_id)
   AND run_id = sqlc.arg(run_id)
   AND workspace_id = sqlc.arg(workspace_id)
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

-- name: GetRunFinalizationTime :one
SELECT clock_timestamp()::timestamptz;

-- name: RunFinalizationScopeIsClear :one
SELECT NOT EXISTS (
           SELECT 1
             FROM run_waits
            WHERE run_waits.run_id = sqlc.arg(run_id)
              AND run_waits.attempt_number = sqlc.arg(attempt_number)
              AND run_waits.workspace_id = sqlc.arg(workspace_id)
              AND run_waits.suspension_state NOT IN ('released', 'cancelled', 'failed')
       )
       AND NOT EXISTS (
           SELECT 1
             FROM workspace_processes
            WHERE workspace_processes.workspace_id = sqlc.arg(workspace_id)
              AND workspace_processes.state IN ('pending', 'starting', 'running', 'exit_requested')
       ) AS clear;

-- name: BeginRunLeaseFinalization :one
UPDATE run_leases
   SET state = 'finalizing',
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
   AND state = 'running'
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
   AND state = 'active'
RETURNING *;

-- name: CloseRunActiveIntervalForFinalization :one
UPDATE runs
   SET active_elapsed_ms = active_elapsed_ms
           + floor(extract(epoch FROM (sqlc.arg(finalization_started_at)::timestamptz - active_started_at)) * 1000)::bigint,
       active_started_at = NULL,
       state_version = state_version + 1,
       updated_at = sqlc.arg(finalization_started_at)
 WHERE id = sqlc.arg(id)
   AND org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND workspace_id = sqlc.arg(workspace_id)
   AND status = 'running'
   AND current_attempt_number = sqlc.arg(attempt_number)
   AND current_run_lease_id = sqlc.arg(run_lease_id)
   AND state_version = sqlc.arg(expected_state_version)
   AND active_started_at IS NOT NULL
   AND sqlc.arg(finalization_started_at)::timestamptz >= active_started_at
   AND sqlc.arg(finalization_started_at)::timestamptz < active_started_at
       + ((max_active_duration_ms - active_elapsed_ms) * interval '1 millisecond')
RETURNING *;

-- name: MarkRunLeaseRunning :one
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

-- name: MarkRunRunning :one
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

-- name: TouchRunWorkspaceActivity :one
UPDATE workspaces
   SET last_activity_at = greatest(last_activity_at, transaction_timestamp()),
       updated_at = transaction_timestamp()
 WHERE id = sqlc.arg(id)
   AND org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND ownership_generation = sqlc.arg(ownership_generation)
   AND writer_generation = sqlc.arg(writer_generation)
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
-- name: RecoverExpiredRecreatedRunResumes :many
WITH candidates AS MATERIALIZED (
    SELECT runs.id AS run_id,
           runs.entrypoint_kind,
           runs.actor_id,
           run_leases.id AS run_lease_id,
           run_leases.worker_instance_id,
           run_leases.worker_epoch,
           run_leases.network_slot_id,
           run_leases.network_slot_generation,
           run_leases.runtime_instance_id,
           workspace_leases.id AS workspace_lease_id,
           workspace_mounts.id AS workspace_mount_id,
           run_waits.id AS run_wait_id,
           runtime_instances.restore_checkpoint_id
      FROM runs
      JOIN run_leases
        ON run_leases.id = runs.current_run_lease_id
       AND run_leases.run_id = runs.id
       AND run_leases.attempt_number = runs.current_attempt_number
       AND run_leases.workspace_id = runs.workspace_id
       AND run_leases.state IN ('assigned', 'starting', 'running')
      JOIN workspace_leases
        ON workspace_leases.owner_run_lease_id = run_leases.id
       AND workspace_leases.workspace_id = runs.workspace_id
       AND workspace_leases.runtime_instance_id = run_leases.runtime_instance_id
       AND workspace_leases.state IN ('active', 'releasing')
      JOIN workspace_mounts
        ON workspace_mounts.id = workspace_leases.workspace_mount_id
       AND workspace_mounts.runtime_instance_id = run_leases.runtime_instance_id
       AND workspace_mounts.workspace_id = runs.workspace_id
       AND workspace_mounts.state IN ('mounting', 'mounted', 'unmounting', 'lost', 'failed')
      JOIN runtime_instances
        ON runtime_instances.id = run_leases.runtime_instance_id
       AND runtime_instances.workspace_id = runs.workspace_id
       AND runtime_instances.restore_checkpoint_id IS NOT NULL
       AND runtime_instances.reclaimed_at IS NULL
      JOIN worker_instances
        ON worker_instances.id = run_leases.worker_instance_id
      JOIN worker_network_slots
        ON worker_network_slots.id = run_leases.network_slot_id
       AND worker_network_slots.worker_instance_id = run_leases.worker_instance_id
       AND worker_network_slots.worker_epoch = run_leases.worker_epoch
       AND worker_network_slots.runtime_instance_id = run_leases.runtime_instance_id
       AND ((worker_network_slots.generation = run_leases.network_slot_generation
             AND worker_network_slots.state IN ('bound', 'reclaiming', 'quarantined'))
            OR (worker_network_slots.state = 'lost'
                AND worker_network_slots.generation = run_leases.network_slot_generation + 1))
      JOIN run_waits
        ON run_waits.run_id = runs.id
       AND run_waits.attempt_number = runs.current_attempt_number
       AND run_waits.workspace_id = runs.workspace_id
       AND run_waits.current_run_lease_id = run_leases.id
       AND run_waits.suspension_state = 'resuming'
       AND run_waits.suspend_checkpoint_id = runtime_instances.restore_checkpoint_id
       AND run_waits.handoff_runtime_instance_id IS NULL
       AND run_waits.handoff_workspace_mount_id IS NULL
       AND run_waits.handoff_resume_checkpoint_id IS NULL
     WHERE (run_leases.expires_at <= transaction_timestamp()
            OR (run_leases.state IN ('assigned', 'starting')
                AND run_leases.start_deadline_at <= transaction_timestamp())
            OR (run_leases.state = 'running'
                AND runs.active_started_at IS NOT NULL
                AND transaction_timestamp() >= runs.active_started_at
                    + (GREATEST(runs.max_active_duration_ms - runs.active_elapsed_ms, 0)::text
                       || ' milliseconds')::interval)
            OR runtime_instances.lost_at <= transaction_timestamp()
            OR runtime_instances.failed_at <= transaction_timestamp()
            OR worker_instances.lost_at <= transaction_timestamp()
            OR worker_instances.disabled_at <= transaction_timestamp()
            OR worker_instances.current_epoch IS DISTINCT FROM run_leases.worker_epoch
            OR workspace_mounts.lost_at <= transaction_timestamp()
            OR workspace_mounts.failed_at <= transaction_timestamp()
            OR worker_network_slots.lost_at <= transaction_timestamp())
     ORDER BY runs.id
     LIMIT sqlc.arg(limit_count)
), locked_actor_candidates AS MATERIALIZED (
    SELECT candidates.*,
           actors.run_generation AS actor_run_generation,
           actors.committed_input_sequence AS actor_committed_input_sequence,
           actors.next_input_sequence AS actor_next_input_sequence
      FROM candidates
      JOIN actors
        ON actors.id = candidates.actor_id
       AND actors.current_run_id = candidates.run_id
       AND actors.state IN ('open', 'closing')
     WHERE candidates.entrypoint_kind = 'actor'
     ORDER BY actors.id
     FOR UPDATE OF actors SKIP LOCKED
), placement_candidates AS MATERIALIZED (
    SELECT candidates.*,
           NULL::bigint AS actor_run_generation,
           NULL::bigint AS actor_committed_input_sequence,
           NULL::bigint AS actor_next_input_sequence
      FROM candidates
     WHERE candidates.entrypoint_kind = 'task'
       AND candidates.actor_id IS NULL
    UNION ALL
    SELECT locked_actor_candidates.*
      FROM locked_actor_candidates
), locked_runs AS MATERIALIZED (
    SELECT runs.org_id,
           runs.project_id,
           runs.environment_id,
           runs.workspace_id,
           runs.id AS run_id,
           runs.state_version,
           runs.current_attempt_number,
           runs.actor_start_input_sequence,
           runs.actor_start_input_high_watermark,
           runs.max_active_duration_ms,
           runs.active_elapsed_ms,
           runs.active_started_at,
           placement_candidates.entrypoint_kind,
           placement_candidates.actor_id,
           placement_candidates.actor_run_generation,
           placement_candidates.actor_committed_input_sequence,
           placement_candidates.actor_next_input_sequence,
           placement_candidates.run_lease_id,
           placement_candidates.worker_instance_id,
           placement_candidates.worker_epoch,
           placement_candidates.network_slot_id,
           placement_candidates.network_slot_generation,
           placement_candidates.runtime_instance_id,
           placement_candidates.workspace_lease_id,
           placement_candidates.workspace_mount_id,
           placement_candidates.run_wait_id,
           placement_candidates.restore_checkpoint_id
      FROM placement_candidates
      JOIN runs ON runs.id = placement_candidates.run_id
     WHERE ((runs.entrypoint_kind = 'task'
             AND runs.actor_id IS NULL
             AND placement_candidates.entrypoint_kind = 'task')
            OR (runs.entrypoint_kind = 'actor'
                AND runs.actor_id = placement_candidates.actor_id
                AND runs.cause_kind IN ('actor_start', 'continuation')
                AND placement_candidates.entrypoint_kind = 'actor'))
       AND runs.parent_run_id IS NULL
       AND ((runs.status = 'queued' AND runs.active_started_at IS NULL)
            OR (runs.status = 'running' AND runs.active_started_at IS NOT NULL))
       AND runs.current_run_lease_id = placement_candidates.run_lease_id
     ORDER BY runs.id
     FOR UPDATE OF runs SKIP LOCKED
), locked_workspaces AS MATERIALIZED (
    SELECT locked_runs.*,
           workspaces.ownership_generation,
           workspaces.writer_generation
      FROM locked_runs
      JOIN workspaces ON workspaces.id = locked_runs.workspace_id
     WHERE workspaces.org_id = locked_runs.org_id
       AND workspaces.project_id = locked_runs.project_id
       AND workspaces.environment_id = locked_runs.environment_id
       AND ((locked_runs.entrypoint_kind = 'task'
             AND workspaces.owner_run_id = locked_runs.run_id
             AND workspaces.owner_actor_id IS NULL)
            OR (locked_runs.entrypoint_kind = 'actor'
                AND workspaces.owner_actor_id = locked_runs.actor_id
                AND workspaces.owner_run_id IS NULL))
       AND workspaces.state = 'active'
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
            OR (run_attempts.actor_start_input_sequence IS NOT NULL
                AND run_attempts.actor_start_input_sequence = locked_workspaces.actor_start_input_sequence
                AND locked_workspaces.actor_start_input_sequence
                    <= locked_workspaces.actor_start_input_high_watermark
                AND locked_workspaces.actor_committed_input_sequence
                    >= locked_workspaces.actor_start_input_sequence
                AND locked_workspaces.actor_committed_input_sequence
                    < locked_workspaces.actor_next_input_sequence))
     ORDER BY run_attempts.run_id, run_attempts.number
     FOR UPDATE OF run_attempts
), locked_workers AS MATERIALIZED (
    SELECT locked_attempts.*,
           LEAST(
               COALESCE(worker_instances.lost_at, 'infinity'::timestamptz),
               COALESCE(worker_instances.disabled_at, 'infinity'::timestamptz),
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
), locked_slots AS MATERIALIZED (
    SELECT locked_workers.*,
           worker_network_slots.lost_at AS slot_lost_at
      FROM locked_workers
      JOIN worker_network_slots
        ON worker_network_slots.id = locked_workers.network_slot_id
       AND worker_network_slots.worker_instance_id = locked_workers.worker_instance_id
       AND worker_network_slots.worker_epoch = locked_workers.worker_epoch
       AND worker_network_slots.runtime_instance_id = locked_workers.runtime_instance_id
       AND ((worker_network_slots.generation = locked_workers.network_slot_generation
             AND worker_network_slots.state IN ('bound', 'reclaiming', 'quarantined'))
            OR (worker_network_slots.state = 'lost'
                AND worker_network_slots.generation = locked_workers.network_slot_generation + 1))
     ORDER BY worker_network_slots.id
     FOR UPDATE OF worker_network_slots
), locked_runtimes AS MATERIALIZED (
    SELECT locked_slots.*,
           runtime_instances.lost_at AS runtime_lost_at,
           runtime_instances.failed_at AS runtime_failed_at
      FROM locked_slots
      JOIN runtime_instances
        ON runtime_instances.id = locked_slots.runtime_instance_id
       AND runtime_instances.org_id = locked_slots.org_id
       AND runtime_instances.worker_instance_id = locked_slots.worker_instance_id
       AND runtime_instances.worker_epoch = locked_slots.worker_epoch
       AND runtime_instances.workspace_id = locked_slots.workspace_id
       AND runtime_instances.restore_checkpoint_id = locked_slots.restore_checkpoint_id
       AND runtime_instances.reclaimed_at IS NULL
     ORDER BY runtime_instances.id
     FOR UPDATE OF runtime_instances
), locked_run_leases AS MATERIALIZED (
    SELECT locked_runtimes.*,
           run_leases.state AS run_lease_state,
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
       AND run_leases.network_slot_id = locked_runtimes.network_slot_id
       AND run_leases.network_slot_generation = locked_runtimes.network_slot_generation
       AND run_leases.runtime_instance_id = locked_runtimes.runtime_instance_id
       AND run_leases.state IN ('assigned', 'starting', 'running')
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
       AND workspace_mounts.state IN ('mounting', 'mounted', 'unmounting', 'lost', 'failed')
     ORDER BY workspace_mounts.id
     FOR UPDATE OF workspace_mounts
), locked_workspace_leases AS MATERIALIZED (
    SELECT locked_mounts.*,
           workspace_leases.expires_at AS workspace_lease_expires_at,
           workspace_leases.base_version_id AS restore_workspace_version_id
      FROM locked_mounts
      JOIN workspace_leases
        ON workspace_leases.id = locked_mounts.workspace_lease_id
       AND workspace_leases.owner_run_lease_id = locked_mounts.run_lease_id
       AND workspace_leases.workspace_id = locked_mounts.workspace_id
       AND workspace_leases.workspace_mount_id = locked_mounts.workspace_mount_id
       AND workspace_leases.runtime_instance_id = locked_mounts.runtime_instance_id
       AND workspace_leases.ownership_generation = locked_mounts.ownership_generation
       AND workspace_leases.writer_generation = locked_mounts.writer_generation
       AND workspace_leases.state = 'active'
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
       AND run_waits.suspension_state = 'resuming'
       AND run_waits.suspend_checkpoint_id = locked_workspace_leases.restore_checkpoint_id
       AND run_waits.handoff_runtime_instance_id IS NULL
       AND run_waits.handoff_workspace_mount_id IS NULL
       AND run_waits.handoff_resume_checkpoint_id IS NULL
     ORDER BY run_waits.id
     FOR UPDATE OF run_waits
), loss_authority AS MATERIALIZED (
    SELECT locked_waits.*,
           hard_deadline.hard_deadline_at,
           physical_loss.physical_loss_at,
           physical_failure.physical_failure_at,
           CASE
               WHEN locked_waits.run_lease_state = 'running'
               THEN LEAST(
                   locked_waits.run_lease_expires_at,
                   hard_deadline.hard_deadline_at,
                   physical_loss.physical_loss_at,
                   physical_failure.physical_failure_at
               )
               ELSE LEAST(
                   locked_waits.run_lease_expires_at,
                   locked_waits.start_deadline_at,
                   physical_loss.physical_loss_at,
                   physical_failure.physical_failure_at
               )
           END AS authority_loss_at
      FROM locked_waits
      CROSS JOIN LATERAL (
          SELECT CASE
              WHEN locked_waits.run_lease_state = 'running'
               AND locked_waits.active_started_at IS NOT NULL
              THEN locked_waits.active_started_at
                   + (GREATEST(
                          locked_waits.max_active_duration_ms - locked_waits.active_elapsed_ms,
                          0
                      )::text || ' milliseconds')::interval
              ELSE 'infinity'::timestamptz
          END AS hard_deadline_at
      ) AS hard_deadline
      CROSS JOIN LATERAL (
          SELECT LEAST(
              COALESCE(locked_waits.worker_lost_at, 'infinity'::timestamptz),
              COALESCE(locked_waits.slot_lost_at, 'infinity'::timestamptz),
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
           (loss_authority.run_lease_state = 'running'
            AND loss_authority.authority_loss_at = loss_authority.hard_deadline_at) AS active_budget_exhausted,
           (run_checkpoints.state = 'ready'
            AND (run_checkpoints.expires_at IS NULL
                 OR run_checkpoints.expires_at > transaction_timestamp())
            AND workspace_versions.state = 'private'
            AND source_run_leases.state = 'checkpointed'
            AND ((loss_authority.entrypoint_kind = 'task'
                  AND run_checkpoints.actor_speculative_input_sequence IS NULL)
                 OR (loss_authority.entrypoint_kind = 'actor'
                     AND run_checkpoints.actor_speculative_input_sequence
                         BETWEEN loss_authority.actor_committed_input_sequence
                             AND loss_authority.actor_next_input_sequence - 1))
            AND NOT (
                loss_authority.run_lease_state = 'running'
                AND loss_authority.authority_loss_at = loss_authority.hard_deadline_at
            )) AS checkpoint_recoverable,
           CASE
               WHEN loss_authority.run_lease_state = 'running'
                AND loss_authority.authority_loss_at = loss_authority.hard_deadline_at
               THEN 'max_active_duration_exceeded'
               ELSE 'restore_checkpoint_unavailable'
           END AS recovery_terminal_reason_code
      FROM loss_authority
      JOIN run_checkpoints
        ON run_checkpoints.id = loss_authority.restore_checkpoint_id
       AND run_checkpoints.kind = 'suspend'
       AND run_checkpoints.run_id = loss_authority.run_id
       AND run_checkpoints.attempt_number = loss_authority.current_attempt_number
       AND run_checkpoints.run_wait_id = loss_authority.run_wait_id
       AND run_checkpoints.workspace_id = loss_authority.workspace_id
      JOIN workspace_versions
        ON workspace_versions.id = run_checkpoints.private_workspace_version_id
       AND workspace_versions.workspace_id = run_checkpoints.workspace_id
       AND workspace_versions.id = loss_authority.restore_workspace_version_id
      JOIN run_leases AS source_run_leases
        ON source_run_leases.id = run_checkpoints.source_run_lease_id
       AND source_run_leases.run_id = run_checkpoints.run_id
       AND source_run_leases.attempt_number = run_checkpoints.attempt_number
       AND source_run_leases.workspace_id = run_checkpoints.workspace_id
     ORDER BY run_checkpoints.id
     FOR UPDATE OF run_checkpoints, workspace_versions
), expired_run_leases AS (
    UPDATE run_leases
       SET state = 'expired',
           terminal_at = transaction_timestamp(),
           terminal_reason_code = CASE
               WHEN locked_checkpoints.active_budget_exhausted
               THEN 'max_active_duration_exceeded'
               WHEN locked_checkpoints.authority_loss_at = locked_checkpoints.physical_failure_at
               THEN 'runtime_failed'
               WHEN locked_checkpoints.authority_loss_at = locked_checkpoints.physical_loss_at
               THEN 'worker_lost'
               ELSE 'lease_expired'
           END,
           updated_at = transaction_timestamp()
      FROM locked_checkpoints
     WHERE run_leases.id = locked_checkpoints.run_lease_id
       AND run_leases.state = locked_checkpoints.run_lease_state
       AND run_leases.expires_at = locked_checkpoints.run_lease_expires_at
       AND run_leases.start_deadline_at = locked_checkpoints.start_deadline_at
       AND locked_checkpoints.authority_loss_at <= transaction_timestamp()
    RETURNING run_leases.id, locked_checkpoints.checkpoint_recoverable
), expired_workspace_leases AS (
    UPDATE workspace_leases
       SET state = 'expired',
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
       AND workspace_leases.state = 'active'
       AND workspace_leases.expires_at = locked_checkpoints.workspace_lease_expires_at
       AND workspace_leases.expires_at = locked_checkpoints.run_lease_expires_at
    RETURNING workspace_leases.id, expired_run_leases.checkpoint_recoverable
), requeued_runs AS (
    UPDATE runs
       SET status = 'queued',
           current_run_lease_id = NULL,
           active_elapsed_ms = LEAST(runs.max_active_duration_ms, runs.active_elapsed_ms + CASE
               WHEN runs.active_started_at IS NULL THEN 0
               ELSE GREATEST(
                   floor(extract(epoch FROM (locked_checkpoints.authority_loss_at - runs.active_started_at)) * 1000)::bigint,
                   0
               )
           END),
           active_started_at = NULL,
           state_version = runs.state_version + 1,
           updated_at = transaction_timestamp()
      FROM locked_checkpoints, expired_workspace_leases
     WHERE runs.id = locked_checkpoints.run_id
       AND runs.org_id = locked_checkpoints.org_id
       AND runs.current_run_lease_id = locked_checkpoints.run_lease_id
       AND runs.state_version = locked_checkpoints.state_version
       AND ((locked_checkpoints.run_lease_state IN ('assigned', 'starting')
             AND runs.status = 'queued'
             AND runs.active_started_at IS NULL)
            OR (locked_checkpoints.run_lease_state = 'running'
                AND runs.status = 'running'
                AND runs.active_started_at IS NOT NULL))
       AND expired_workspace_leases.id = locked_checkpoints.workspace_lease_id
       AND expired_workspace_leases.checkpoint_recoverable
    RETURNING runs.org_id, runs.id, runs.state_version
), requeued_waits AS (
    UPDATE run_waits
       SET suspension_state = 'resume_pending',
           current_run_lease_id = NULL,
           resume_request_version = run_waits.resume_request_version + 1,
           expected_run_state_version = requeued_runs.state_version,
           updated_at = transaction_timestamp()
      FROM locked_checkpoints, requeued_runs
     WHERE run_waits.id = locked_checkpoints.run_wait_id
       AND run_waits.run_id = requeued_runs.id
       AND run_waits.current_run_lease_id = locked_checkpoints.run_lease_id
       AND run_waits.suspension_state = 'resuming'
       AND run_waits.resume_request_version = locked_checkpoints.resume_request_version
    RETURNING run_waits.id, requeued_runs.org_id, requeued_runs.id AS run_id
), admission_outbox AS (
    INSERT INTO outbox_messages (
        lane,
        topic,
        partition_key,
        payload,
        available_at
    )
    SELECT 'control',
           'run.admit',
           locked_checkpoints.workspace_id::text,
           jsonb_build_object(
               'environmentId', locked_checkpoints.environment_id::text,
               'runId', requeued_waits.run_id::text
           ),
           transaction_timestamp()
      FROM requeued_waits
      JOIN locked_checkpoints ON locked_checkpoints.run_wait_id = requeued_waits.id
    RETURNING payload
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
       SET status = CASE
               WHEN locked_checkpoints.active_budget_exhausted THEN 'expired'::run_status
               ELSE 'system_failed'::run_status
           END,
           terminal_reason_code = locked_checkpoints.recovery_terminal_reason_code,
           current_run_lease_id = NULL,
           active_elapsed_ms = CASE
               WHEN locked_checkpoints.active_budget_exhausted THEN runs.max_active_duration_ms
               ELSE LEAST(runs.max_active_duration_ms, runs.active_elapsed_ms + CASE
                   WHEN runs.active_started_at IS NULL THEN 0
                   ELSE GREATEST(
                       floor(extract(epoch FROM (locked_checkpoints.authority_loss_at - runs.active_started_at)) * 1000)::bigint,
                       0
                   )
               END)
           END,
           active_started_at = NULL,
           state_version = runs.state_version + 1,
           terminal_at = transaction_timestamp(),
           updated_at = transaction_timestamp()
      FROM locked_checkpoints, failed_attempts
     WHERE runs.id = locked_checkpoints.run_id
       AND runs.org_id = locked_checkpoints.org_id
       AND runs.current_run_lease_id = locked_checkpoints.run_lease_id
       AND runs.state_version = locked_checkpoints.state_version
       AND ((locked_checkpoints.run_lease_state IN ('assigned', 'starting')
             AND runs.status = 'queued'
             AND runs.active_started_at IS NULL)
            OR (locked_checkpoints.run_lease_state = 'running'
                AND runs.status = 'running'
                AND runs.active_started_at IS NOT NULL))
       AND failed_attempts.run_id = locked_checkpoints.run_id
       AND failed_attempts.number = locked_checkpoints.current_attempt_number
    RETURNING runs.id,
              runs.org_id,
              runs.project_id,
              runs.environment_id,
              runs.current_attempt_number,
              runs.state_version,
              runs.trace_id,
              runs.root_span_id
), failed_waits AS (
    UPDATE run_waits
       SET suspension_state = 'failed',
           current_run_lease_id = NULL,
           suspension_terminal_at = transaction_timestamp(),
           suspension_reason_code = locked_checkpoints.recovery_terminal_reason_code,
           updated_at = transaction_timestamp()
      FROM locked_checkpoints, failed_runs
     WHERE run_waits.id = locked_checkpoints.run_wait_id
       AND run_waits.run_id = failed_runs.id
       AND run_waits.current_run_lease_id = locked_checkpoints.run_lease_id
       AND run_waits.suspension_state = 'resuming'
       AND run_waits.resume_request_version = locked_checkpoints.resume_request_version
    RETURNING run_waits.id, failed_runs.id AS run_id
), failed_actors AS (
    UPDATE actors
       SET state = 'failed',
           current_run_id = NULL,
           run_generation = actors.run_generation + 1,
           state_version = actors.state_version + 1,
           manual_run_cancelled = false,
           failure_code = CASE
               WHEN locked_checkpoints.active_budget_exhausted THEN 'run-expired'
               ELSE 'platform-failure'
           END,
           failure_run_id = failed_runs.id,
           failed_at = transaction_timestamp(),
           updated_at = transaction_timestamp()
      FROM locked_checkpoints, failed_runs, failed_waits
     WHERE locked_checkpoints.entrypoint_kind = 'actor'
       AND actors.id = locked_checkpoints.actor_id
       AND actors.current_run_id = failed_runs.id
       AND actors.run_generation = locked_checkpoints.actor_run_generation
       AND actors.state IN ('open', 'closing')
       AND failed_waits.run_id = failed_runs.id
    RETURNING actors.id, failed_runs.id AS run_id
), released_owners AS (
    UPDATE workspaces
       SET owner_run_id = CASE
               WHEN locked_checkpoints.entrypoint_kind = 'task' THEN NULL
               ELSE workspaces.owner_run_id
           END,
           owner_actor_id = CASE
               WHEN locked_checkpoints.entrypoint_kind = 'actor' THEN NULL
               ELSE workspaces.owner_actor_id
           END,
           ownership_generation = workspaces.ownership_generation + 1,
           state_version = workspaces.state_version + 1,
           last_activity_at = transaction_timestamp(),
           updated_at = transaction_timestamp()
      FROM locked_checkpoints
      JOIN failed_waits ON failed_waits.run_id = locked_checkpoints.run_id
      LEFT JOIN failed_actors
        ON failed_actors.run_id = locked_checkpoints.run_id
       AND failed_actors.id = locked_checkpoints.actor_id
     WHERE workspaces.id = locked_checkpoints.workspace_id
       AND ((locked_checkpoints.entrypoint_kind = 'task'
             AND workspaces.owner_run_id = failed_waits.run_id
             AND workspaces.owner_actor_id IS NULL)
            OR (locked_checkpoints.entrypoint_kind = 'actor'
                AND failed_actors.id IS NOT NULL
                AND workspaces.owner_actor_id = failed_actors.id
                AND workspaces.owner_run_id IS NULL))
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
           CASE
               WHEN locked_checkpoints.active_budget_exhausted THEN 'run.expired'
               ELSE 'run.system_failed'
           END,
           CASE
               WHEN locked_checkpoints.active_budget_exhausted THEN 'Run maximum active duration exceeded'
               ELSE 'Run restore Checkpoint became unavailable'
           END,
           jsonb_build_object('reasonCode', locked_checkpoints.recovery_terminal_reason_code),
           'internal',
           failed_runs.state_version,
           transaction_timestamp()
      FROM failed_runs
      JOIN locked_checkpoints ON locked_checkpoints.run_id = failed_runs.id
      JOIN failed_waits ON failed_waits.run_id = failed_runs.id
      JOIN released_owners ON released_owners.id = locked_checkpoints.workspace_id
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
       SET state = 'unmounting',
           stopped_at = coalesce(stopped_at, transaction_timestamp()),
           updated_at = transaction_timestamp()
      FROM locked_checkpoints, closing_runtimes
     WHERE workspace_mounts.id = locked_checkpoints.workspace_mount_id
       AND closing_runtimes.id = locked_checkpoints.runtime_instance_id
       AND workspace_mounts.state = 'mounted'
    RETURNING workspace_mounts.id
)
SELECT requeued_waits.id, requeued_waits.org_id, requeued_waits.run_id
  FROM requeued_waits
  JOIN locked_checkpoints ON locked_checkpoints.run_wait_id = requeued_waits.id
  JOIN admission_outbox
    ON admission_outbox.payload ->> 'runId' = requeued_waits.run_id::text
  LEFT JOIN closing_runtimes ON closing_runtimes.id = locked_checkpoints.runtime_instance_id
  LEFT JOIN unmounting ON unmounting.id = locked_checkpoints.workspace_mount_id
 ORDER BY requeued_waits.id;
