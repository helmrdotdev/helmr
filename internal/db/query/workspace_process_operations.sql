-- name: RequestWorkspaceOperation :one
INSERT INTO workspace_process_operations (
    id, org_id, project_id, environment_id, workspace_id, workspace_mount_id,
    operation_kind, process_id, request_fingerprint, operation_expires_at,
    priority, instance_lease_id, write_lease_id, fencing_token,
    fencing_generation, request
)
SELECT sqlc.arg(id), workspace_mounts.org_id, workspace_mounts.project_id,
       workspace_mounts.environment_id, workspace_mounts.workspace_id,
       workspace_mounts.id, sqlc.arg(operation_kind), sqlc.arg(process_id),
       sqlc.arg(request_fingerprint), sqlc.arg(operation_expires_at),
       sqlc.arg(priority), sqlc.narg(instance_lease_id), sqlc.narg(write_lease_id),
       COALESCE(sqlc.narg(fencing_token)::text, ''), workspace_mounts.fencing_generation,
       sqlc.arg(request)
  FROM workspace_mounts
 WHERE workspace_mounts.org_id = sqlc.arg(org_id)
   AND workspace_mounts.workspace_id = sqlc.arg(workspace_id)
   AND workspace_mounts.id = sqlc.arg(workspace_mount_id)
   AND workspace_mounts.state = 'mounted'
RETURNING *;

-- name: GetWorkspaceOperation :one
SELECT * FROM workspace_process_operations
 WHERE org_id = sqlc.arg(org_id) AND id = sqlc.arg(id);

-- name: GetActiveWorkspaceOperationByResource :one
SELECT * FROM workspace_process_operations
 WHERE org_id = sqlc.arg(org_id) AND workspace_mount_id = sqlc.arg(workspace_mount_id)
   AND operation_kind = sqlc.arg(operation_kind) AND process_id = sqlc.arg(process_id)
   AND state IN ('queued','claimed','running')
 ORDER BY requested_at, id LIMIT 1;

-- name: ClaimWorkspaceOperation :one
WITH target_group AS MATERIALIZED (
    SELECT worker_groups.id
      FROM worker_groups
      JOIN worker_instances ON worker_instances.worker_group_id = worker_groups.id
     WHERE worker_instances.id = sqlc.arg(worker_instance_id)
       AND worker_groups.state = 'active'
     FOR UPDATE OF worker_groups
), worker_fence AS MATERIALIZED (
    SELECT worker_instances.id, worker_instances.current_epoch
      FROM worker_instances
      JOIN target_group ON target_group.id = worker_instances.worker_group_id
     WHERE worker_instances.id = sqlc.arg(worker_instance_id)
       AND worker_instances.current_epoch = sqlc.arg(worker_epoch)
       AND worker_instances.state = 'active'
     FOR UPDATE OF worker_instances
), candidate AS (
    SELECT workspace_process_operations.id, workspace_mounts.worker_epoch
      FROM workspace_process_operations
      JOIN workspace_mounts ON workspace_mounts.org_id = workspace_process_operations.org_id
                           AND workspace_mounts.workspace_id = workspace_process_operations.workspace_id
                           AND workspace_mounts.id = workspace_process_operations.workspace_mount_id
      JOIN runtime_instances ON runtime_instances.id = workspace_mounts.runtime_instance_id
      JOIN worker_fence ON worker_fence.id = runtime_instances.worker_instance_id
                       AND worker_fence.current_epoch = runtime_instances.worker_epoch
      JOIN workspace_leases ON workspace_leases.org_id = workspace_process_operations.org_id
                            AND workspace_leases.workspace_id = workspace_process_operations.workspace_id
                            AND workspace_leases.workspace_mount_id = workspace_process_operations.workspace_mount_id
                            AND workspace_leases.id = COALESCE(workspace_process_operations.write_lease_id, workspace_process_operations.instance_lease_id)
     WHERE workspace_process_operations.org_id = sqlc.arg(org_id)
       AND workspace_process_operations.workspace_mount_id = sqlc.arg(workspace_mount_id)
       AND workspace_process_operations.state IN ('queued','claimed')
       AND (workspace_process_operations.state = 'queued' OR workspace_process_operations.claim_expires_at <= now())
       AND workspace_process_operations.claim_attempt < sqlc.arg(max_claim_attempts)
       AND workspace_process_operations.operation_expires_at > now()
       AND workspace_mounts.state = 'mounted'
       AND runtime_instances.worker_instance_id = sqlc.arg(worker_instance_id)
       AND runtime_instances.worker_epoch = sqlc.arg(worker_epoch)
       AND runtime_instances.observed_state = 'ready'
       AND workspace_leases.worker_instance_id = sqlc.arg(worker_instance_id)
       AND workspace_leases.worker_epoch = sqlc.arg(worker_epoch)
       AND workspace_leases.runtime_instance_id = workspace_mounts.runtime_instance_id
       AND workspace_leases.state = 'active' AND workspace_leases.expires_at > now()
       AND workspace_leases.fencing_token = workspace_process_operations.fencing_token
       AND workspace_leases.acquired_fencing_generation = workspace_process_operations.fencing_generation
     ORDER BY workspace_process_operations.priority DESC,
              workspace_process_operations.requested_at, workspace_process_operations.id
     LIMIT 1 FOR UPDATE OF workspace_process_operations SKIP LOCKED
)
UPDATE workspace_process_operations
   SET state = 'claimed', claimed_by_worker_instance_id = sqlc.arg(worker_instance_id),
       claimed_worker_epoch = candidate.worker_epoch, claim_token = sqlc.arg(claim_token),
       claim_attempt = claim_attempt + 1, claim_expires_at = sqlc.arg(claim_expires_at),
       claimed_at = now(), updated_at = now()
  FROM candidate WHERE workspace_process_operations.id = candidate.id
RETURNING workspace_process_operations.*;

-- name: StartWorkspaceOperation :one
UPDATE workspace_process_operations
   SET state = 'running', updated_at = now()
 WHERE workspace_process_operations.org_id = sqlc.arg(org_id)
   AND workspace_process_operations.id = sqlc.arg(id)
   AND claimed_by_worker_instance_id = sqlc.arg(worker_instance_id)
   AND claimed_worker_epoch = sqlc.arg(worker_epoch)
   AND claim_token = sqlc.arg(claim_token) AND state = 'claimed'
   AND claim_expires_at > now() AND operation_expires_at > now()
   AND EXISTS (
       SELECT 1 FROM workspace_leases
        WHERE workspace_leases.org_id = workspace_process_operations.org_id
          AND workspace_leases.workspace_id = workspace_process_operations.workspace_id
          AND workspace_leases.workspace_mount_id = workspace_process_operations.workspace_mount_id
          AND workspace_leases.id = COALESCE(workspace_process_operations.write_lease_id, workspace_process_operations.instance_lease_id)
          AND workspace_leases.worker_instance_id = sqlc.arg(worker_instance_id)
          AND workspace_leases.worker_epoch = sqlc.arg(worker_epoch)
          AND workspace_leases.state = 'active' AND workspace_leases.expires_at > now()
          AND workspace_leases.fencing_token = workspace_process_operations.fencing_token
          AND workspace_leases.acquired_fencing_generation = workspace_process_operations.fencing_generation
   )
RETURNING *;

-- name: CompleteWorkspaceOperation :one
UPDATE workspace_process_operations
   SET state = 'completed', result = sqlc.arg(result), completed_at = now(),
       terminal_at = now(), terminal_reason_code = 'completed', terminal_error = NULL,
       claim_expires_at = NULL, updated_at = now()
 WHERE workspace_process_operations.org_id = sqlc.arg(org_id)
   AND workspace_process_operations.id = sqlc.arg(id)
   AND claimed_by_worker_instance_id = sqlc.arg(worker_instance_id)
   AND claimed_worker_epoch = sqlc.arg(worker_epoch)
   AND claim_token = sqlc.arg(claim_token) AND state = 'running'
   AND EXISTS (
       SELECT 1 FROM workspace_leases
        WHERE workspace_leases.org_id = workspace_process_operations.org_id
          AND workspace_leases.workspace_id = workspace_process_operations.workspace_id
          AND workspace_leases.workspace_mount_id = workspace_process_operations.workspace_mount_id
          AND workspace_leases.id = COALESCE(workspace_process_operations.write_lease_id, workspace_process_operations.instance_lease_id)
          AND workspace_leases.worker_instance_id = sqlc.arg(worker_instance_id)
          AND workspace_leases.worker_epoch = sqlc.arg(worker_epoch)
          AND workspace_leases.state = 'active' AND workspace_leases.expires_at > now()
          AND workspace_leases.fencing_token = workspace_process_operations.fencing_token
          AND workspace_leases.acquired_fencing_generation = workspace_process_operations.fencing_generation
   )
RETURNING *;

-- name: FailWorkspaceOperation :one
UPDATE workspace_process_operations
   SET state = 'failed', terminal_at = now(), terminal_reason_code = sqlc.arg(reason_code),
       terminal_error = sqlc.narg(error), claim_expires_at = NULL, updated_at = now()
 WHERE workspace_process_operations.org_id = sqlc.arg(org_id)
   AND workspace_process_operations.id = sqlc.arg(id)
   AND claimed_by_worker_instance_id = sqlc.arg(worker_instance_id)
   AND claimed_worker_epoch = sqlc.arg(worker_epoch)
   AND claim_token = sqlc.arg(claim_token) AND state IN ('claimed','running')
   AND EXISTS (
       SELECT 1 FROM workspace_leases
        WHERE workspace_leases.org_id = workspace_process_operations.org_id
          AND workspace_leases.workspace_id = workspace_process_operations.workspace_id
          AND workspace_leases.workspace_mount_id = workspace_process_operations.workspace_mount_id
          AND workspace_leases.id = COALESCE(workspace_process_operations.write_lease_id, workspace_process_operations.instance_lease_id)
          AND workspace_leases.worker_instance_id = sqlc.arg(worker_instance_id)
          AND workspace_leases.worker_epoch = sqlc.arg(worker_epoch)
          AND workspace_leases.state = 'active' AND workspace_leases.expires_at > now()
          AND workspace_leases.fencing_token = workspace_process_operations.fencing_token
          AND workspace_leases.acquired_fencing_generation = workspace_process_operations.fencing_generation
   )
RETURNING *;
