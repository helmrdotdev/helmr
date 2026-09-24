-- name: CreateRunRuntimeReservation :one
WITH selected_shape AS MATERIALIZED (
    SELECT worker_pool_cpu_shapes.vcpu_count,
           worker_pool_cpu_shapes.cpu_config_digest
      FROM worker_instances
      JOIN worker_groups
        ON worker_groups.id = worker_instances.worker_group_id
       AND worker_groups.status = 'active'
      JOIN worker_pools
	    ON worker_pools.id = worker_instances.worker_pool_id
	   AND worker_pools.worker_group_id = worker_instances.worker_group_id
	   AND worker_pools.status = 'active'
      JOIN worker_pool_cpu_shapes
        ON worker_pool_cpu_shapes.worker_pool_id = worker_pools.id
       AND worker_pool_cpu_shapes.vcpu_count = ((sqlc.arg(reserved_cpu_millis)::bigint - 1) / 1000 + 1)::integer
     WHERE worker_instances.id = sqlc.arg(worker_instance_id)
       AND worker_instances.worker_group_id = sqlc.arg(worker_group_id)
	   AND worker_instances.current_epoch = sqlc.arg(worker_epoch)
	   AND worker_instances.status = 'active'
       AND (
           sqlc.narg(restore_checkpoint_id)::uuid IS NOT NULL
           OR worker_groups.primary_pool_id = worker_pools.id
       )
       AND (
           sqlc.narg(required_cpu_config_digest)::text IS NULL
           OR worker_pool_cpu_shapes.cpu_config_digest = sqlc.narg(required_cpu_config_digest)
       )
), created_runtime AS (
    INSERT INTO runtime_instances (
        id,
        org_id,
        worker_group_id,
        project_id,
        environment_id,
        region_id,
        worker_instance_id,
        runtime_identity_id,
        deployment_definition_id,
        worker_epoch,
        vm_vcpu_count,
        cpu_config_digest,
        reserved_cpu_millis,
        reserved_memory_bytes,
        reserved_guest_ephemeral_disk_bytes,
        reserved_execution_slots,
        workspace_id,
        program_deployment_id,
        restore_checkpoint_id,
        reserved_run_id,
        reserved_attempt_number,
        reserved_workspace_version_id,
        preparation_expires_at,
        desired_reason
    ) SELECT
        sqlc.arg(id),
        sqlc.arg(org_id),
        sqlc.arg(worker_group_id),
        sqlc.arg(project_id),
        sqlc.arg(environment_id),
        sqlc.arg(region_id),
        sqlc.arg(worker_instance_id),
        sqlc.arg(runtime_identity_id),
        sqlc.arg(deployment_definition_id),
        sqlc.arg(worker_epoch),
        selected_shape.vcpu_count,
        selected_shape.cpu_config_digest,
        sqlc.arg(reserved_cpu_millis),
        sqlc.arg(reserved_memory_bytes),
        sqlc.arg(reserved_guest_ephemeral_disk_bytes),
        sqlc.arg(reserved_execution_slots),
        sqlc.arg(workspace_id),
        sqlc.arg(program_deployment_id),
        sqlc.narg(restore_checkpoint_id),
        sqlc.arg(run_id),
        sqlc.arg(attempt_number),
        sqlc.arg(base_workspace_version_id),
        transaction_timestamp() + sqlc.arg(preparation_seconds)::bigint * interval '1 second',
        'run_reservation'
      FROM selected_shape
    RETURNING *
)
SELECT created_runtime.*
  FROM created_runtime;

-- name: InsertAssignedRunLease :one
INSERT INTO run_leases (
    id,
    org_id,
    project_id,
    environment_id,
    run_id,
    workspace_id,
    region_id,
    lease_sequence,
    attempt_number,
    worker_group_id,
    worker_instance_id,
    worker_epoch,
    runtime_instance_id,
    runtime_identity_id,
    requested_cpu_millis,
    requested_memory_bytes,
    requested_guest_ephemeral_disk_bytes,
    requested_execution_slots,
    trace_id,
    span_id,
    parent_span_id,
    traceparent,
    start_deadline_at,
    expires_at
) VALUES (
    sqlc.arg(id),
    sqlc.arg(org_id),
    sqlc.arg(project_id),
    sqlc.arg(environment_id),
    sqlc.arg(run_id),
    sqlc.arg(workspace_id),
    sqlc.arg(region_id),
    sqlc.arg(lease_sequence),
    sqlc.arg(attempt_number),
    sqlc.arg(worker_group_id),
    sqlc.arg(worker_instance_id),
    sqlc.arg(worker_epoch),
    sqlc.arg(runtime_instance_id),
    sqlc.arg(runtime_identity_id),
    sqlc.arg(requested_cpu_millis),
    sqlc.arg(requested_memory_bytes),
    sqlc.arg(requested_guest_ephemeral_disk_bytes),
    sqlc.arg(requested_execution_slots),
    sqlc.narg(trace_id),
    sqlc.narg(span_id),
    sqlc.narg(parent_span_id),
    sqlc.narg(traceparent),
    sqlc.arg(start_deadline_at),
    sqlc.arg(expires_at)
)
RETURNING *;

-- name: AdvanceRunWorkspaceWriter :one
UPDATE computers
   SET writer_generation = sqlc.arg(writer_generation),
       last_activity_at = transaction_timestamp(),
       updated_at = transaction_timestamp()
 WHERE computers.environment_id = sqlc.arg(environment_id)
   AND EXISTS (
       SELECT 1 FROM environments
        WHERE environments.id = computers.environment_id
          AND environments.org_id = sqlc.arg(org_id)
          AND environments.project_id = sqlc.arg(project_id)
   )
   AND computers.id = sqlc.arg(workspace_id)
   AND computers.ownership_generation = sqlc.arg(ownership_generation)
   AND computers.writer_generation = sqlc.arg(expected_writer_generation)
   AND computers.status = 'active'
   AND computers.desired_state = 'active'
RETURNING computers.id, computers.environment_id, computers.region_id, computers.sandbox_declared_id, computers.deployment_definition_id, computers.key, computers.revision, computers.owner_session_id, computers.owner_run_id, computers.ownership_generation, computers.writer_generation, computers.head_version_id, computers.status, computers.desired_state, computers.dirty_state, computers.last_activity_at, computers.created_at, computers.updated_at, computers.deleted_at;

-- name: AdvanceRunWorkspaceMountFence :one
UPDATE workspace_mounts
   SET fencing_generation = sqlc.arg(fencing_generation),
       updated_at = transaction_timestamp()
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
   AND materialized_version_id = sqlc.arg(base_workspace_version_id)
   AND fencing_generation = sqlc.arg(expected_fencing_generation)
   AND status = 'mounted'
RETURNING *;

-- name: InsertRunWorkspaceLease :one
INSERT INTO workspace_leases (
    id,
    org_id,
    worker_group_id,
    project_id,
    environment_id,
    region_id,
    worker_instance_id,
    worker_epoch,
    runtime_instance_id,
    workspace_id,
    workspace_mount_id,
    owner_run_lease_id,
    base_workspace_version_id,
    ownership_generation,
    writer_generation,
    mount_fencing_generation,
    fencing_token_hash,
    expires_at
) VALUES (
    sqlc.arg(id),
    sqlc.arg(org_id),
    sqlc.arg(worker_group_id),
    sqlc.arg(project_id),
    sqlc.arg(environment_id),
    sqlc.arg(region_id),
    sqlc.arg(worker_instance_id),
    sqlc.arg(worker_epoch),
    sqlc.arg(runtime_instance_id),
    sqlc.arg(workspace_id),
    sqlc.arg(workspace_mount_id),
    sqlc.arg(owner_run_lease_id),
    sqlc.arg(base_workspace_version_id),
    sqlc.arg(ownership_generation),
    sqlc.arg(writer_generation),
    sqlc.arg(mount_fencing_generation),
    sqlc.arg(fencing_token_hash),
    sqlc.arg(expires_at)
)
RETURNING *;

-- Caller holds the Runtime row lock; recheck time at consumption because other
-- grant operations may have waited since the initial locked authority check.
-- name: ConsumeRunRuntimeReservation :execrows
UPDATE runtime_instances
   SET reserved_run_id = NULL,
       reserved_attempt_number = NULL,
       reserved_workspace_version_id = NULL,
       reservation_expires_at = NULL,
       updated_at = transaction_timestamp()
 WHERE id = sqlc.arg(id)
   AND workspace_id = sqlc.arg(workspace_id)
   AND reserved_run_id = sqlc.arg(run_id)
   AND reserved_attempt_number = sqlc.arg(attempt_number)
   AND reserved_workspace_version_id = sqlc.arg(base_workspace_version_id)
   AND restore_checkpoint_id IS NOT DISTINCT FROM sqlc.narg(restore_checkpoint_id)
   AND reservation_expires_at > clock_timestamp();

-- The grant owner already holds Run and any restore checkpoint locks. Recheck
-- deadlines after potentially blocking grant writes, before publishing the lease.
-- name: SetRunCurrentLease :one
UPDATE runs
   SET current_run_lease_id = sqlc.arg(run_lease_id),
       first_lease_at = coalesce(first_lease_at, transaction_timestamp()),
       runtime_preparation_count = 0,
       next_runtime_preparation_at = NULL,
       revision = revision + 1,
       updated_at = transaction_timestamp()
 WHERE runs.id = sqlc.arg(id)
   AND org_id = sqlc.arg(org_id)
   AND revision = sqlc.arg(expected_revision)
   AND status = 'queued'
   AND current_attempt_number = sqlc.arg(attempt_number)
   AND current_run_lease_id IS NULL
   AND (next_runtime_preparation_at IS NULL
        OR next_runtime_preparation_at <= transaction_timestamp())
   AND (first_lease_at IS NOT NULL OR queued_expires_at IS NULL OR queued_expires_at > clock_timestamp())
   AND (sqlc.narg(restore_checkpoint_id)::uuid IS NULL OR EXISTS (
       SELECT 1 FROM run_checkpoints
        WHERE run_checkpoints.id = sqlc.narg(restore_checkpoint_id)
          AND run_checkpoints.run_id = runs.id
          AND run_checkpoints.attempt_number = runs.current_attempt_number
          AND run_checkpoints.workspace_id = runs.workspace_id
          AND run_checkpoints.status = 'ready'
          AND (run_checkpoints.expires_at IS NULL
               OR run_checkpoints.expires_at > clock_timestamp())
   ))
   AND (sqlc.narg(same_workspace_child_wait_id)::uuid IS NULL OR EXISTS (
       SELECT 1 FROM run_waits AS parent_wait
       JOIN run_checkpoints AS parent_checkpoint
         ON parent_checkpoint.id = parent_wait.suspend_checkpoint_id
        AND parent_checkpoint.run_id = parent_wait.run_id
        AND parent_checkpoint.attempt_number = parent_wait.attempt_number
        AND parent_checkpoint.workspace_id = parent_wait.workspace_id
        AND parent_checkpoint.status = 'ready'
        WHERE parent_wait.id = sqlc.narg(same_workspace_child_wait_id)
          AND parent_wait.child_run_id = runs.id
          AND parent_wait.workspace_id = runs.workspace_id
          AND (parent_checkpoint.expires_at IS NULL
               OR parent_checkpoint.expires_at > clock_timestamp())
   ))
RETURNING runs.*;
