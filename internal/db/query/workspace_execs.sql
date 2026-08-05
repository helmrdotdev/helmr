-- name: CreateWorkspaceExec :one
INSERT INTO workspace_processes (
    id,
    org_id,
    project_id,
    environment_id,
    workspace_id,
    base_version_id,
    restore_desired_state,
    state,
    request,
    stdin,
    claim_id,
    created_by_subject_type,
    created_by_subject_id
) VALUES (
    sqlc.arg(id),
    sqlc.arg(org_id),
    sqlc.arg(project_id),
    sqlc.arg(environment_id),
    sqlc.arg(workspace_id),
    sqlc.arg(base_version_id),
    sqlc.arg(restore_desired_state),
    'pending',
    sqlc.arg(request),
    sqlc.arg(stdin),
    sqlc.arg(claim_id),
    sqlc.arg(created_by_subject_type),
    sqlc.arg(created_by_subject_id)
)
RETURNING *;

-- name: GetWorkspaceExecByClaim :one
SELECT *
  FROM workspace_processes
 WHERE environment_id = sqlc.arg(environment_id)
   AND workspace_id = sqlc.arg(workspace_id)
   AND claim_id = sqlc.arg(claim_id);

-- name: GetWorkspaceExec :one
SELECT *
  FROM workspace_processes
 WHERE org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND workspace_id = sqlc.arg(workspace_id)
   AND id = sqlc.arg(id);

-- name: ListPendingWorkspaceExecCandidates :many
SELECT org_id, id, state_version, created_at
  FROM workspace_processes
 WHERE state = 'pending'
 ORDER BY created_at, id
 LIMIT sqlc.arg(row_limit);

-- name: ListRecoverableWorkspaceExecCandidates :many
SELECT workspace_processes.org_id,
       workspace_processes.id,
       workspace_processes.workspace_id,
       workspace_processes.state_version
  FROM workspace_processes
  JOIN workspace_mounts
    ON workspace_mounts.id = workspace_processes.workspace_mount_id
   AND workspace_mounts.workspace_id = workspace_processes.workspace_id
  JOIN workspace_leases
    ON workspace_leases.workspace_mount_id = workspace_mounts.id
   AND workspace_leases.owner_process_id = workspace_processes.id
 WHERE workspace_processes.state IN ('starting', 'running', 'exit_requested')
   AND (
       workspace_mounts.state = 'lost'
       OR workspace_leases.state = 'fenced'
       OR (
           workspace_leases.state IN ('active', 'releasing')
           AND workspace_leases.expires_at <= transaction_timestamp()
       )
   )
 ORDER BY workspace_processes.updated_at, workspace_processes.id
 LIMIT sqlc.arg(row_limit);

-- name: FailPendingWorkspaceExecProcess :one
UPDATE workspace_processes
   SET state = 'failed',
       state_version = state_version + 1,
       terminal_at = transaction_timestamp(),
       terminal_reason_code = sqlc.arg(reason_code),
       error = sqlc.arg(error),
       updated_at = transaction_timestamp()
 WHERE org_id = sqlc.arg(org_id)
   AND id = sqlc.arg(process_id)
   AND state = 'pending'
   AND state_version = sqlc.arg(expected_state_version)
RETURNING *;

-- name: CloseExpiredWorkspaceExecReservation :execrows
WITH target AS (
    SELECT runtime_instances.id
      FROM runtime_instances
     WHERE runtime_instances.id = sqlc.arg(runtime_instance_id)
       AND runtime_instances.workspace_id = sqlc.arg(workspace_id)
       AND runtime_instances.reserved_process_id = sqlc.arg(process_id)
       AND runtime_instances.reservation_expires_at <= transaction_timestamp()
       AND runtime_instances.reclaimed_at IS NULL
       AND runtime_instances.observed_state IN ('allocated', 'preparing', 'ready')
     FOR UPDATE
), stopped_mount AS (
    UPDATE workspace_mounts
       SET state = 'unmounting',
           finalization_kind = 'discard',
           stopped_at = COALESCE(stopped_at, transaction_timestamp()),
           updated_at = transaction_timestamp()
      FROM target
     WHERE workspace_mounts.runtime_instance_id = target.id
       AND workspace_mounts.state IN ('mounting', 'mounted')
    RETURNING workspace_mounts.id
)
UPDATE runtime_instances
   SET desired_state = 'closed',
       desired_version = CASE
           WHEN desired_state = 'closed' THEN desired_version
           ELSE desired_version + 1
       END,
       desired_at = transaction_timestamp(),
       desired_reason = 'workspace_exec_reservation_expired',
       updated_at = transaction_timestamp()
  FROM target
 WHERE runtime_instances.id = target.id;

-- name: CreateWorkspaceExecRuntimeReservation :one
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
        reserved_cpu_millis,
        reserved_memory_bytes,
        reserved_guest_ephemeral_disk_bytes,
        reserved_execution_slots,
        workspace_id,
        reserved_process_id,
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
        sqlc.arg(reserved_cpu_millis),
        sqlc.arg(reserved_memory_bytes),
        sqlc.arg(reserved_guest_ephemeral_disk_bytes),
        sqlc.arg(reserved_execution_slots),
        sqlc.arg(workspace_id),
        sqlc.arg(process_id),
        sqlc.arg(base_workspace_version_id),
        sqlc.arg(reservation_expires_at),
        'workspace_exec_reservation'
    )
    RETURNING *
)
SELECT created_runtime.*
  FROM created_runtime;

-- name: ReserveReadyRuntimeForWorkspaceExec :one
UPDATE runtime_instances
   SET reserved_process_id = sqlc.arg(process_id),
       reserved_workspace_version_id = sqlc.arg(base_workspace_version_id),
       reservation_expires_at = sqlc.arg(reservation_expires_at),
       desired_reason = 'workspace_exec_reservation',
       updated_at = transaction_timestamp()
 WHERE id = sqlc.arg(id)
   AND workspace_id = sqlc.arg(workspace_id)
   AND deployment_definition_id = sqlc.arg(deployment_definition_id)
   AND observed_state = 'ready'
   AND reclaimed_at IS NULL
   AND reserved_run_id IS NULL
   AND reserved_attempt_number IS NULL
   AND reserved_process_id IS NULL
   AND reserved_workspace_version_id IS NULL
   AND reservation_expires_at IS NULL
RETURNING *;

-- name: AdvanceWorkspaceExecWriter :one
UPDATE workspaces
   SET ownership_generation = sqlc.arg(ownership_generation),
       writer_generation = sqlc.arg(writer_generation),
       desired_state = 'active',
       state_version = state_version + 1,
       last_activity_at = transaction_timestamp(),
       updated_at = transaction_timestamp()
  FROM environments
 WHERE environments.id = workspaces.environment_id
   AND environments.org_id = sqlc.arg(org_id)
   AND environments.project_id = sqlc.arg(project_id)
   AND workspaces.environment_id = sqlc.arg(environment_id)
   AND workspaces.id = sqlc.arg(workspace_id)
   AND workspaces.head_version_id = sqlc.arg(base_workspace_version_id)
   AND workspaces.ownership_generation = sqlc.arg(expected_ownership_generation)
   AND workspaces.writer_generation = sqlc.arg(expected_writer_generation)
   AND workspaces.state = 'active'
   AND workspaces.desired_state IN ('active', 'stopped')
   AND workspaces.dirty_state = 'clean'
   AND workspaces.owner_session_id IS NULL
   AND workspaces.owner_run_id IS NULL
   AND NOT EXISTS (
       SELECT 1
         FROM workspace_leases
        WHERE workspace_leases.workspace_id = workspaces.id
          AND workspace_leases.state IN ('active', 'releasing')
   )
RETURNING *;

-- name: AdvanceWorkspaceExecMountFence :one
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

-- name: BindWorkspaceExecRuntime :one
UPDATE workspace_processes
   SET region_id = sqlc.arg(region_id),
       worker_group_id = sqlc.arg(worker_group_id),
       worker_instance_id = sqlc.arg(worker_instance_id),
       worker_epoch = sqlc.arg(worker_epoch),
       runtime_instance_id = sqlc.arg(runtime_instance_id),
       workspace_mount_id = sqlc.arg(workspace_mount_id),
       state = 'starting',
       state_version = state_version + 1,
       updated_at = transaction_timestamp()
 WHERE org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND workspace_id = sqlc.arg(workspace_id)
   AND id = sqlc.arg(id)
   AND base_version_id = sqlc.arg(base_workspace_version_id)
   AND state = 'pending'
   AND state_version = sqlc.arg(expected_state_version)
RETURNING *;

-- name: InsertWorkspaceExecLease :one
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
    owner_process_id,
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
    sqlc.arg(process_id),
    sqlc.arg(base_workspace_version_id),
    sqlc.arg(ownership_generation),
    sqlc.arg(writer_generation),
    sqlc.arg(mount_fencing_generation),
    sqlc.arg(fencing_token_hash),
    sqlc.arg(expires_at)
)
RETURNING *;

-- name: ConsumeWorkspaceExecRuntimeReservation :execrows
UPDATE runtime_instances
   SET reserved_process_id = NULL,
       reserved_workspace_version_id = NULL,
       reservation_expires_at = NULL,
       updated_at = transaction_timestamp()
 WHERE id = sqlc.arg(id)
   AND workspace_id = sqlc.arg(workspace_id)
   AND reserved_process_id = sqlc.arg(process_id)
   AND reserved_workspace_version_id = sqlc.arg(base_workspace_version_id)
   AND reservation_expires_at > transaction_timestamp();

-- name: LockWorkspaceExecWorkerAuthority :one
SELECT sqlc.embed(workspace_processes),
       sqlc.embed(workspace_mounts),
       sqlc.embed(workspace_leases),
       idempotency_claims.request_fingerprint
  FROM workspace_processes
  JOIN workspace_mounts
    ON workspace_mounts.org_id = workspace_processes.org_id
   AND workspace_mounts.project_id = workspace_processes.project_id
   AND workspace_mounts.environment_id = workspace_processes.environment_id
   AND workspace_mounts.workspace_id = workspace_processes.workspace_id
   AND workspace_mounts.id = workspace_processes.workspace_mount_id
  JOIN workspace_leases
    ON workspace_leases.org_id = workspace_processes.org_id
   AND workspace_leases.project_id = workspace_processes.project_id
   AND workspace_leases.environment_id = workspace_processes.environment_id
   AND workspace_leases.workspace_id = workspace_processes.workspace_id
   AND workspace_leases.workspace_mount_id = workspace_processes.workspace_mount_id
   AND workspace_leases.owner_process_id = workspace_processes.id
  JOIN workspaces
    ON workspaces.id = workspace_processes.workspace_id
  JOIN worker_groups
    ON worker_groups.id = workspace_processes.worker_group_id
   AND worker_groups.region_id = workspace_processes.region_id
  JOIN worker_instances
    ON worker_instances.id = workspace_processes.worker_instance_id
   AND worker_instances.worker_group_id = workspace_processes.worker_group_id
   AND worker_instances.current_epoch = workspace_processes.worker_epoch
  JOIN worker_observations
    ON worker_observations.worker_instance_id = worker_instances.id
   AND worker_observations.worker_epoch = worker_instances.current_epoch
  JOIN runtime_instances
    ON runtime_instances.id = workspace_processes.runtime_instance_id
   AND runtime_instances.org_id = workspace_processes.org_id
   AND runtime_instances.project_id = workspace_processes.project_id
   AND runtime_instances.environment_id = workspace_processes.environment_id
   AND runtime_instances.region_id = workspace_processes.region_id
   AND runtime_instances.worker_group_id = workspace_processes.worker_group_id
   AND runtime_instances.worker_instance_id = workspace_processes.worker_instance_id
   AND runtime_instances.worker_epoch = workspace_processes.worker_epoch
   AND runtime_instances.workspace_id = workspace_processes.workspace_id
  JOIN runtime_identities
    ON runtime_identities.id = runtime_instances.runtime_identity_id
  JOIN runtime_substrates
    ON runtime_substrates.id = runtime_instances.runtime_substrate_id
   AND runtime_substrates.org_id = runtime_instances.org_id
   AND runtime_substrates.project_id = runtime_instances.project_id
   AND runtime_substrates.environment_id = runtime_instances.environment_id
   AND runtime_substrates.deployment_definition_id = runtime_instances.deployment_definition_id
  JOIN idempotency_claims
    ON idempotency_claims.environment_id = workspace_processes.environment_id
   AND idempotency_claims.id = workspace_processes.claim_id
 WHERE workspace_processes.org_id = sqlc.arg(org_id)
   AND workspace_processes.id = sqlc.arg(process_id)
   AND workspace_processes.state IN ('starting', 'running', 'exit_requested')
   AND workspace_mounts.id = sqlc.arg(workspace_mount_id)
   AND workspace_mounts.worker_instance_id = sqlc.arg(worker_instance_id)
   AND workspace_mounts.worker_epoch = sqlc.arg(worker_epoch)
   AND workspace_mounts.state IN ('mounted', 'unmounting')
   AND workspace_leases.worker_instance_id = sqlc.arg(worker_instance_id)
   AND workspace_leases.worker_epoch = sqlc.arg(worker_epoch)
   AND workspace_leases.state IN ('active', 'releasing')
   AND workspace_leases.expires_at > transaction_timestamp()
   AND workspace_leases.ownership_generation = workspaces.ownership_generation
   AND workspace_leases.writer_generation = workspaces.writer_generation
   AND workspace_leases.mount_fencing_generation = workspace_mounts.fencing_generation
   AND (
       workspace_processes.state <> 'starting'
       OR (
           worker_groups.state = 'active'
           AND worker_groups.allows_run
           AND worker_groups.protocol_version = worker_instances.protocol_version
           AND worker_instances.state = 'active'
           AND worker_instances.supports_run
           AND worker_instances.runtime_identity_id = runtime_instances.runtime_identity_id
           AND runtime_identities.network_abi = 'helmr/v0'
           AND worker_observations.observed_at >= transaction_timestamp()
               - worker_groups.observation_ttl_seconds * interval '1 second'
           AND worker_observations.run_paused_reason IS NULL
           AND runtime_instances.desired_state = 'ready'
           AND runtime_instances.observed_state = 'ready'
           AND runtime_instances.observed_desired_version = runtime_instances.desired_version
           AND runtime_instances.reclaimed_at IS NULL
           AND worker_instances.per_vm_cpu_millis >= runtime_instances.reserved_cpu_millis
           AND worker_instances.per_vm_memory_bytes >= runtime_instances.reserved_memory_bytes
           AND worker_instances.per_vm_guest_ephemeral_disk_bytes >=
               runtime_instances.reserved_guest_ephemeral_disk_bytes
           AND runtime_instances.runtime_substrate_id IS NOT NULL
           AND runtime_substrates.substrate_format = worker_instances.substrate_format
           AND runtime_substrates.builder_abi = worker_instances.substrate_builder_abi
           AND runtime_substrates.layout_abi = worker_instances.substrate_layout_abi
       )
   )
 FOR UPDATE OF worker_groups, worker_instances, worker_observations,
               runtime_instances, workspace_processes, workspace_mounts,
               workspace_leases;

-- name: GetWorkspaceExecLocatorForMount :one
SELECT workspace_processes.id,
       workspace_processes.workspace_id,
       workspace_processes.environment_id
  FROM workspace_processes
 WHERE workspace_processes.org_id = sqlc.arg(org_id)
   AND workspace_processes.workspace_mount_id = sqlc.arg(workspace_mount_id)
   AND workspace_processes.state IN ('starting', 'running', 'exit_requested');

-- name: GetWorkspaceExecLocator :one
SELECT workspace_processes.workspace_mount_id,
       workspace_processes.workspace_id,
       workspace_processes.environment_id
  FROM workspace_processes
 WHERE workspace_processes.org_id = sqlc.arg(org_id)
   AND workspace_processes.id = sqlc.arg(process_id)
   AND workspace_processes.state IN ('starting', 'running', 'exit_requested');

-- name: LockWorkspaceExecFailureAuthority :one
SELECT sqlc.embed(workspace_processes),
       sqlc.embed(workspace_mounts),
       sqlc.embed(workspace_leases)
  FROM workspace_processes
  JOIN workspace_mounts
    ON workspace_mounts.id = workspace_processes.workspace_mount_id
   AND workspace_mounts.workspace_id = workspace_processes.workspace_id
  JOIN workspace_leases
    ON workspace_leases.workspace_mount_id = workspace_mounts.id
   AND workspace_leases.owner_process_id = workspace_processes.id
  JOIN workspaces
    ON workspaces.id = workspace_processes.workspace_id
 WHERE workspace_processes.org_id = sqlc.arg(org_id)
   AND workspace_processes.id = sqlc.arg(process_id)
   AND workspace_processes.state IN ('starting', 'running', 'exit_requested')
   AND workspace_mounts.id = sqlc.arg(workspace_mount_id)
   AND workspace_mounts.worker_instance_id = sqlc.arg(worker_instance_id)
   AND workspace_mounts.worker_epoch = sqlc.arg(worker_epoch)
   AND workspace_mounts.state IN ('mounting', 'mounted', 'unmounting')
   AND workspace_leases.worker_instance_id = sqlc.arg(worker_instance_id)
   AND workspace_leases.worker_epoch = sqlc.arg(worker_epoch)
   AND workspace_leases.state IN ('active', 'releasing')
   AND workspace_leases.ownership_generation = workspaces.ownership_generation
   AND workspace_leases.writer_generation = workspaces.writer_generation
   AND workspace_leases.mount_fencing_generation = workspace_mounts.fencing_generation
 FOR UPDATE OF workspace_processes, workspace_mounts, workspace_leases;

-- name: LockWorkspaceExecFailureWorkspace :one
SELECT workspaces.*
  FROM workspaces
  JOIN environments ON environments.id = workspaces.environment_id
 WHERE environments.org_id = sqlc.arg(org_id)
   AND workspaces.id = sqlc.arg(workspace_id)
 FOR UPDATE;

-- name: LockWorkspaceExecRecoveryAuthority :one
SELECT sqlc.embed(workspace_processes),
       sqlc.embed(workspace_mounts),
       sqlc.embed(workspace_leases)
  FROM workspace_processes
  JOIN workspace_mounts
    ON workspace_mounts.id = workspace_processes.workspace_mount_id
   AND workspace_mounts.workspace_id = workspace_processes.workspace_id
  JOIN workspace_leases
    ON workspace_leases.workspace_mount_id = workspace_mounts.id
   AND workspace_leases.owner_process_id = workspace_processes.id
 WHERE workspace_processes.org_id = sqlc.arg(org_id)
   AND workspace_processes.id = sqlc.arg(process_id)
   AND workspace_processes.workspace_id = sqlc.arg(workspace_id)
   AND workspace_processes.state_version = sqlc.arg(expected_state_version)
   AND workspace_processes.state IN ('starting', 'running', 'exit_requested')
   AND (
       workspace_mounts.state = 'lost'
       OR workspace_leases.state = 'fenced'
       OR (
           workspace_leases.state IN ('active', 'releasing')
           AND workspace_leases.expires_at <= transaction_timestamp()
       )
   )
 FOR UPDATE OF workspace_processes, workspace_mounts, workspace_leases;

-- name: LockWorkspaceExecSecretRevocationAuthority :one
SELECT sqlc.embed(workspace_processes),
       sqlc.embed(workspace_mounts),
       sqlc.embed(workspace_leases)
  FROM workspace_processes
  JOIN workspace_mounts
    ON workspace_mounts.id = workspace_processes.workspace_mount_id
   AND workspace_mounts.workspace_id = workspace_processes.workspace_id
  JOIN workspace_leases
    ON workspace_leases.workspace_mount_id = workspace_mounts.id
   AND workspace_leases.owner_process_id = workspace_processes.id
 WHERE workspace_processes.org_id = sqlc.arg(org_id)
   AND workspace_processes.id = sqlc.arg(process_id)
   AND workspace_processes.workspace_id = sqlc.arg(workspace_id)
   AND workspace_processes.state_version = sqlc.arg(expected_state_version)
   AND workspace_processes.state IN ('starting', 'running', 'exit_requested')
   AND workspace_mounts.state IN ('mounting', 'mounted', 'unmounting')
   AND workspace_leases.state IN ('active', 'releasing')
 FOR UPDATE OF workspace_processes, workspace_mounts, workspace_leases;

-- name: FenceWorkspaceExecLeaseForSecretRevocation :one
UPDATE workspace_leases
   SET state = 'fenced',
       terminal_at = transaction_timestamp(),
       terminal_reason_code = 'workspace_exec_secret_revoked',
       terminal_error = '{"code":"workspace_exec_secret_revoked","retryable":false}'::jsonb,
       updated_at = transaction_timestamp()
 WHERE id = sqlc.arg(lease_id)
   AND owner_process_id = sqlc.arg(process_id)
   AND state IN ('active', 'releasing')
RETURNING *;

-- name: LoseWorkspaceExecMount :one
UPDATE workspace_mounts
   SET state = 'lost',
       lost_at = COALESCE(lost_at, transaction_timestamp()),
       terminal_at = COALESCE(terminal_at, transaction_timestamp()),
       terminal_reason_code = sqlc.arg(reason_code),
       updated_at = transaction_timestamp()
 WHERE id = sqlc.arg(workspace_mount_id)
   AND workspace_id = sqlc.arg(workspace_id)
   AND state IN ('mounting', 'mounted', 'unmounting')
RETURNING *;

-- name: CloseWorkspaceExecRuntime :execrows
UPDATE runtime_instances
   SET desired_state = 'closed',
       desired_version = CASE
           WHEN desired_state = 'closed' THEN desired_version
           ELSE desired_version + 1
       END,
       desired_at = transaction_timestamp(),
       desired_reason = sqlc.arg(reason_code),
       updated_at = transaction_timestamp()
 WHERE id = sqlc.arg(runtime_instance_id)
   AND workspace_id = sqlc.arg(workspace_id)
   AND reclaimed_at IS NULL;

-- name: StartWorkspaceExec :one
UPDATE workspace_processes
   SET state = 'running',
       state_version = state_version + 1,
       started_at = COALESCE(started_at, transaction_timestamp()),
       updated_at = transaction_timestamp()
 WHERE id = sqlc.arg(process_id)
   AND workspace_mount_id = sqlc.arg(workspace_mount_id)
   AND state = 'starting'
RETURNING *;

-- name: RenewWorkspaceExecLeaseForMount :execrows
UPDATE workspace_leases
   SET expires_at = sqlc.arg(expires_at),
       renewed_at = transaction_timestamp(),
       updated_at = transaction_timestamp()
 WHERE workspace_mount_id = sqlc.arg(workspace_mount_id)
   AND owner_process_id IS NOT NULL
   AND state = 'active'
   AND expires_at > transaction_timestamp();

-- name: SetWorkspaceExecResult :one
UPDATE workspace_processes
   SET state = 'exit_requested',
       state_version = CASE
           WHEN state = 'running' THEN state_version + 1
           ELSE state_version
       END,
       exit_code = sqlc.narg(exit_code),
       stdout = sqlc.arg(stdout),
       stderr = sqlc.arg(stderr),
       updated_at = transaction_timestamp()
 WHERE id = sqlc.arg(process_id)
   AND workspace_mount_id = sqlc.arg(workspace_mount_id)
   AND (
       state = 'running'
       OR (
           state = 'exit_requested'
           AND exit_code IS NOT DISTINCT FROM sqlc.narg(exit_code)
           AND stdout = sqlc.arg(stdout)
           AND stderr = sqlc.arg(stderr)
       )
   )
RETURNING *;

-- name: RequestWorkspaceExecMountFinalization :one
WITH requested AS (
    UPDATE workspace_mounts
       SET state = 'unmounting',
           finalization_kind = sqlc.arg(finalization_kind),
           finalization_reason_code = sqlc.arg(reason_code),
           finalization_error = sqlc.narg(error),
           stopped_at = COALESCE(stopped_at, transaction_timestamp()),
           updated_at = transaction_timestamp()
     WHERE workspace_mounts.id = sqlc.arg(workspace_mount_id)
       AND workspace_mounts.worker_instance_id = sqlc.arg(worker_instance_id)
       AND workspace_mounts.worker_epoch = sqlc.arg(worker_epoch)
       AND workspace_mounts.state IN ('mounted', 'unmounting')
       AND (
           workspace_mounts.finalization_kind IS NULL
           OR (
               workspace_mounts.finalization_kind = sqlc.arg(finalization_kind)
               AND workspace_mounts.finalization_reason_code = sqlc.arg(reason_code)
               AND workspace_mounts.finalization_error IS NOT DISTINCT FROM sqlc.narg(error)
           )
       )
    RETURNING *
)
UPDATE runtime_instances
   SET desired_state = 'closed',
       desired_version = CASE
           WHEN desired_state = 'closed' THEN desired_version
           ELSE desired_version + 1
       END,
       desired_at = transaction_timestamp(),
       desired_reason = 'workspace_exec_finalization',
       updated_at = transaction_timestamp()
  FROM requested
 WHERE runtime_instances.id = requested.runtime_instance_id
RETURNING requested.*;

-- name: StageWorkspaceExecCapture :one
WITH authority AS (
    SELECT workspace_mounts.*, workspace_processes.base_version_id,
           workspace_leases.id AS source_workspace_lease_id,
           workspace_leases.ownership_generation,
           workspace_leases.writer_generation
      FROM workspace_mounts
      JOIN workspace_processes
        ON workspace_processes.workspace_mount_id = workspace_mounts.id
       AND workspace_processes.workspace_id = workspace_mounts.workspace_id
       AND workspace_processes.state = 'exit_requested'
      JOIN workspace_leases
        ON workspace_leases.workspace_mount_id = workspace_mounts.id
       AND workspace_leases.owner_process_id = workspace_processes.id
       AND workspace_leases.state IN ('active', 'releasing')
     WHERE workspace_mounts.id = sqlc.arg(workspace_mount_id)
       AND workspace_mounts.worker_instance_id = sqlc.arg(worker_instance_id)
       AND workspace_mounts.worker_epoch = sqlc.arg(worker_epoch)
       AND workspace_mounts.state = 'unmounting'
       AND workspace_mounts.finalization_kind = 'capture'
       AND workspace_mounts.staged_version_id IS NULL
     FOR UPDATE OF workspace_mounts, workspace_processes, workspace_leases
), created AS (
    INSERT INTO workspace_versions (
        id, environment_id, workspace_id,
        parent_version_id, artifact_id, artifact_kind, kind, content_digest,
        size_bytes, entry_count, state, source_workspace_lease_id,
        ownership_generation, writer_generation
    )
    SELECT sqlc.arg(workspace_version_id),
           authority.environment_id, authority.workspace_id, authority.base_version_id,
           sqlc.arg(artifact_id), 'workspace_version', 'system',
           sqlc.arg(content_digest), sqlc.arg(size_bytes), sqlc.arg(entry_count),
           'private', authority.source_workspace_lease_id,
           authority.ownership_generation, authority.writer_generation
      FROM authority
    RETURNING *
), staged AS (
    UPDATE workspace_mounts
       SET staged_version_id = created.id,
           updated_at = transaction_timestamp()
      FROM created
     WHERE workspace_mounts.id = sqlc.arg(workspace_mount_id)
    RETURNING workspace_mounts.id
)
SELECT created.*
  FROM created
  JOIN staged ON true;

-- name: GetStagedWorkspaceExecCapture :one
SELECT workspace_versions.*
  FROM workspace_mounts
  JOIN workspace_versions
    ON workspace_versions.workspace_id = workspace_mounts.workspace_id
   AND workspace_versions.id = workspace_mounts.staged_version_id
 WHERE workspace_mounts.id = sqlc.arg(workspace_mount_id)
   AND workspace_mounts.worker_instance_id = sqlc.arg(worker_instance_id)
   AND workspace_mounts.worker_epoch = sqlc.arg(worker_epoch)
   AND workspace_mounts.state = 'unmounting'
   AND workspace_mounts.finalization_kind = 'capture';

-- name: CommitStagedWorkspaceExecVersion :one
UPDATE workspace_versions
   SET state = 'committed',
       published_at = transaction_timestamp()
 WHERE id = sqlc.arg(version_id)
   AND workspace_id = sqlc.arg(workspace_id)
   AND state = 'private'
RETURNING *;

-- name: FinalizeWorkspaceExecWorkspace :one
UPDATE workspaces
   SET head_version_id = COALESCE(sqlc.narg(version_id), head_version_id),
       desired_state = sqlc.arg(restore_desired_state),
       dirty_state = 'clean',
       state_version = state_version + 1,
       updated_at = transaction_timestamp()
 WHERE id = sqlc.arg(workspace_id)
   AND head_version_id = sqlc.arg(base_version_id)
   AND ownership_generation = sqlc.arg(ownership_generation)
   AND writer_generation = sqlc.arg(writer_generation)
RETURNING *;

-- name: MarkWorkspaceExecRecoveryRequired :one
UPDATE workspaces
   SET state = 'recovery_required',
       desired_state = 'stopped',
       dirty_state = 'dirty_state_lost',
       state_version = state_version + 1,
       updated_at = transaction_timestamp()
 WHERE id = sqlc.arg(workspace_id)
   AND head_version_id = sqlc.arg(base_version_id)
   AND ownership_generation = sqlc.arg(ownership_generation)
   AND writer_generation = sqlc.arg(writer_generation)
RETURNING *;

-- name: FinalizeWorkspaceExecProcess :one
UPDATE workspace_processes
   SET state = sqlc.arg(state),
       state_version = state_version + 1,
       exited_at = CASE
           WHEN sqlc.arg(state)::text = 'exited'
           THEN transaction_timestamp()
           ELSE exited_at
       END,
       terminal_at = transaction_timestamp(),
       terminal_reason_code = sqlc.arg(reason_code),
       error = sqlc.narg(error),
       updated_at = transaction_timestamp()
 WHERE id = sqlc.arg(process_id)
   AND workspace_mount_id = sqlc.arg(workspace_mount_id)
   AND state = 'exit_requested'
RETURNING *;

-- name: FailWorkspaceExecProcess :one
UPDATE workspace_processes
   SET state = 'failed',
       state_version = state_version + 1,
       terminal_at = transaction_timestamp(),
       terminal_reason_code = sqlc.arg(reason_code),
       error = sqlc.arg(error),
       updated_at = transaction_timestamp()
 WHERE id = sqlc.arg(process_id)
   AND workspace_mount_id = sqlc.arg(workspace_mount_id)
   AND state IN ('starting', 'running', 'exit_requested')
RETURNING *;

-- name: DiscardStagedWorkspaceExecVersion :execrows
UPDATE workspace_versions
   SET state = 'discarded',
       discarded_at = transaction_timestamp()
 WHERE id = sqlc.arg(version_id)
   AND workspace_id = sqlc.arg(workspace_id)
   AND state = 'private';

-- name: ReleaseWorkspaceExecLease :one
UPDATE workspace_leases
   SET state = 'released',
       released_at = transaction_timestamp(),
       terminal_at = transaction_timestamp(),
       updated_at = transaction_timestamp()
 WHERE id = sqlc.arg(lease_id)
   AND owner_process_id = sqlc.arg(process_id)
   AND state IN ('active', 'releasing')
RETURNING *;

-- name: ExpireWorkspaceExecLease :one
UPDATE workspace_leases
   SET state = 'expired',
       terminal_at = COALESCE(terminal_at, transaction_timestamp()),
       terminal_reason_code = sqlc.arg(reason_code),
       updated_at = transaction_timestamp()
 WHERE id = sqlc.arg(lease_id)
   AND owner_process_id = sqlc.arg(process_id)
   AND state IN ('active', 'releasing')
RETURNING *;
