-- name: CreateRunRuntimeReservation :one
WITH selected_shape AS MATERIALIZED (
    SELECT worker_pool_cpu_shapes.vcpu_count,
           worker_pool_cpu_shapes.cpu_config_digest
      FROM worker_instances
      JOIN worker_groups
        ON worker_groups.id = worker_instances.worker_group_id
       AND worker_groups.state = 'active'
      JOIN worker_pools
	    ON worker_pools.id = worker_instances.worker_pool_id
	   AND worker_pools.worker_group_id = worker_instances.worker_group_id
	   AND worker_pools.state = 'active'
      JOIN worker_pool_cpu_shapes
        ON worker_pool_cpu_shapes.worker_pool_id = worker_pools.id
       AND worker_pool_cpu_shapes.vcpu_count = ((sqlc.arg(reserved_cpu_millis)::bigint - 1) / 1000 + 1)::integer
     WHERE worker_instances.id = sqlc.arg(worker_instance_id)
       AND worker_instances.worker_group_id = sqlc.arg(worker_group_id)
	   AND worker_instances.current_epoch = sqlc.arg(worker_epoch)
	   AND worker_instances.state = 'active'
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
        reservation_expires_at,
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
        sqlc.arg(reservation_expires_at),
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
UPDATE workspaces
   SET writer_generation = sqlc.arg(writer_generation),
       last_activity_at = transaction_timestamp(),
       updated_at = transaction_timestamp()
 WHERE workspaces.environment_id = sqlc.arg(environment_id)
   AND EXISTS (
       SELECT 1 FROM environments
        WHERE environments.id = workspaces.environment_id
          AND environments.org_id = sqlc.arg(org_id)
          AND environments.project_id = sqlc.arg(project_id)
   )
   AND workspaces.id = sqlc.arg(workspace_id)
   AND workspaces.ownership_generation = sqlc.arg(ownership_generation)
   AND workspaces.writer_generation = sqlc.arg(expected_writer_generation)
   AND workspaces.state = 'active'
   AND workspaces.desired_state = 'active'
RETURNING *;

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
   AND state = 'mounted'
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
    base_version_id,
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
    sqlc.arg(base_version_id),
    sqlc.arg(ownership_generation),
    sqlc.arg(writer_generation),
    sqlc.arg(mount_fencing_generation),
    sqlc.arg(fencing_token_hash),
    sqlc.arg(expires_at)
)
RETURNING *;

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
   AND reservation_expires_at > transaction_timestamp();

-- name: SetRunCurrentLease :one
UPDATE runs
   SET current_run_lease_id = sqlc.arg(run_lease_id),
       first_lease_at = coalesce(first_lease_at, transaction_timestamp()),
       runtime_preparation_count = 0,
       next_runtime_preparation_at = NULL,
       state_version = state_version + 1,
       updated_at = transaction_timestamp()
 WHERE id = sqlc.arg(id)
   AND org_id = sqlc.arg(org_id)
   AND state_version = sqlc.arg(expected_state_version)
   AND status = 'queued'
   AND current_attempt_number = sqlc.arg(attempt_number)
   AND current_run_lease_id IS NULL
   AND (next_runtime_preparation_at IS NULL
        OR next_runtime_preparation_at <= transaction_timestamp())
   AND (first_lease_at IS NOT NULL OR queued_expires_at IS NULL OR queued_expires_at > transaction_timestamp())
RETURNING *;
