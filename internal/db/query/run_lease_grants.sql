-- name: CreateRunRuntimeReservation :one
WITH created_runtime AS (
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
        network_policy,
        reserved_cpu_millis,
        reserved_memory_bytes,
        reserved_workload_disk_bytes,
        reserved_scratch_bytes,
        reserved_execution_slots,
        workspace_id,
        program_deployment_id,
        reserved_run_id,
        reserved_attempt_number,
        reserved_workspace_version_id,
        reservation_expires_at,
        desired_reason
    ) VALUES (
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
        sqlc.arg(network_policy),
        sqlc.arg(reserved_cpu_millis),
        sqlc.arg(reserved_memory_bytes),
        sqlc.arg(reserved_workload_disk_bytes),
        sqlc.arg(reserved_scratch_bytes),
        sqlc.arg(reserved_execution_slots),
        sqlc.arg(workspace_id),
        sqlc.arg(program_deployment_id),
        sqlc.arg(run_id),
        sqlc.arg(attempt_number),
        sqlc.arg(base_workspace_version_id),
        sqlc.arg(reservation_expires_at),
        'run_reservation'
    )
    RETURNING *
), assigned_slot AS (
    UPDATE worker_network_slots
       SET state = 'assigned',
           runtime_instance_id = created_runtime.id,
           assigned_at = transaction_timestamp(),
           updated_at = transaction_timestamp()
      FROM created_runtime
     WHERE worker_network_slots.id = sqlc.arg(network_slot_id)
       AND worker_network_slots.worker_group_id = created_runtime.worker_group_id
       AND worker_network_slots.worker_instance_id = created_runtime.worker_instance_id
       AND worker_network_slots.worker_epoch = created_runtime.worker_epoch
       AND worker_network_slots.generation = sqlc.arg(network_slot_generation)
       AND worker_network_slots.state = 'available'
       AND worker_network_slots.runtime_instance_id IS NULL
    RETURNING worker_network_slots.id
)
SELECT created_runtime.*
  FROM created_runtime
  JOIN assigned_slot ON true;

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
    network_slot_id,
    network_slot_generation,
    runtime_identity_id,
    worker_protocol_version,
    requested_cpu_millis,
    requested_memory_bytes,
    requested_workload_disk_bytes,
    requested_scratch_bytes,
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
    sqlc.arg(network_slot_id),
    sqlc.arg(network_slot_generation),
    sqlc.arg(runtime_identity_id),
    sqlc.arg(worker_protocol_version),
    sqlc.arg(requested_cpu_millis),
    sqlc.arg(requested_memory_bytes),
    sqlc.arg(requested_workload_disk_bytes),
    sqlc.arg(requested_scratch_bytes),
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
 WHERE org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND id = sqlc.arg(workspace_id)
   AND ownership_generation = sqlc.arg(ownership_generation)
   AND writer_generation = sqlc.arg(expected_writer_generation)
   AND state = 'active'
   AND desired_state = 'active'
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
    fencing_key_fingerprint,
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
    sqlc.arg(fencing_key_fingerprint),
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
   AND reservation_expires_at > transaction_timestamp();

-- name: SetRunCurrentLease :one
UPDATE runs
   SET current_run_lease_id = sqlc.arg(run_lease_id),
       first_lease_at = coalesce(first_lease_at, transaction_timestamp()),
       state_version = state_version + 1,
       updated_at = transaction_timestamp()
 WHERE id = sqlc.arg(id)
   AND org_id = sqlc.arg(org_id)
   AND state_version = sqlc.arg(expected_state_version)
   AND status = 'queued'
   AND current_attempt_number = sqlc.arg(attempt_number)
   AND current_run_lease_id IS NULL
   AND (first_lease_at IS NOT NULL OR queued_expires_at IS NULL OR queued_expires_at > transaction_timestamp())
RETURNING *;
