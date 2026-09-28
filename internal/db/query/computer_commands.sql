-- name: CreateComputerCommand :one
INSERT INTO computer_commands (
    id, environment_id, computer_id, claim_id, argv, cwd, env, stdin,
    timeout_ms, created_by_subject_type, created_by_subject_id
) VALUES (
    sqlc.arg(id), sqlc.arg(environment_id), sqlc.arg(computer_id), sqlc.arg(claim_id),
    sqlc.arg(argv), sqlc.narg(cwd), sqlc.arg(env), sqlc.arg(stdin), sqlc.arg(timeout_ms),
    sqlc.arg(created_by_subject_type), sqlc.arg(created_by_subject_id)
) RETURNING *;

-- name: GetComputerCommandByClaim :one
SELECT command.* FROM computer_commands command
 JOIN environments e ON e.id=command.environment_id
 WHERE e.org_id=sqlc.arg(org_id) AND command.environment_id=sqlc.arg(environment_id)
 AND command.claim_id=sqlc.arg(claim_id);

-- name: GetCommand :one
SELECT command.* FROM computer_commands command
 JOIN environments e ON e.id=command.environment_id
 WHERE e.org_id=sqlc.arg(org_id) AND e.project_id=sqlc.arg(project_id)
 AND command.environment_id=sqlc.arg(environment_id) AND command.id=sqlc.arg(command_id);

-- name: GetComputerCommandTarget :one
SELECT command.environment_id, command.computer_id, e.org_id, e.project_id
 FROM computer_commands command JOIN environments e ON e.id=command.environment_id
 WHERE command.id=sqlc.arg(command_id) AND e.org_id=sqlc.arg(org_id);

-- name: ListPendingComputerCommandCandidates :many
SELECT command.id, command.environment_id, command.computer_id, command.revision, command.created_at,
       e.org_id, e.project_id, computers.region_id, computers.computer_spec_id
 FROM computer_commands command
 JOIN environments e ON e.id=command.environment_id
 JOIN computers ON computers.environment_id=command.environment_id AND computers.id=command.computer_id
 WHERE command.status='pending' AND command.computer_instance_id IS NULL
 AND computers.status='active' AND computers.deleted_at IS NULL
 AND computers.preparation_failure IS NULL AND computers.recovery_failure IS NULL
 ORDER BY command.created_at,command.id LIMIT sqlc.arg(row_limit);

-- Computer and instance locks precede this member lock in admission and cleanup.
-- name: LockComputerCommand :one
SELECT command.* FROM computer_commands command
 WHERE command.environment_id=sqlc.arg(environment_id) AND command.computer_id=sqlc.arg(computer_id)
 AND command.id=sqlc.arg(command_id) FOR UPDATE;

-- Placement is assigned once; reconnect never moves a potentially started command.
-- name: BindComputerCommandInstance :one
WITH admitted AS (
 UPDATE computer_instances i SET membership_revision=membership_revision+1,updated_at=clock_timestamp()
 WHERE i.id=sqlc.arg(computer_instance_id) AND i.environment_id=sqlc.arg(environment_id)
 AND i.computer_id=sqlc.arg(computer_id) AND i.writer_generation=sqlc.arg(writer_generation)
 AND i.admission_state='open' AND i.desired_state='ready' AND i.observed_state='ready'
 AND i.mount_state='mounted' AND i.reclaimed_at IS NULL AND i.writer_expires_at>clock_timestamp()
 RETURNING i.id,i.writer_generation,i.environment_id,i.computer_id
)
UPDATE computer_commands c SET computer_instance_id=i.id,writer_generation=i.writer_generation,
 status='starting',revision=revision+1,updated_at=clock_timestamp()
 FROM admitted i WHERE c.id=sqlc.arg(command_id) AND c.environment_id=i.environment_id
 AND c.computer_id=i.computer_id AND c.status='pending' AND c.computer_instance_id IS NULL
 RETURNING c.*;

-- name: LockComputerCommandWorkerAuthority :one
SELECT sqlc.embed(command),sqlc.embed(i)
 FROM computer_commands command JOIN computer_instances i
 ON i.environment_id=command.environment_id AND i.computer_id=command.computer_id
 AND i.id=command.computer_instance_id AND i.writer_generation=command.writer_generation
 WHERE i.org_id=sqlc.arg(org_id) AND command.id=sqlc.arg(command_id)
 AND i.worker_host_id=sqlc.arg(worker_host_id) AND i.worker_epoch=sqlc.arg(worker_epoch)
 AND i.worker_group_id=sqlc.arg(worker_group_id)
 AND i.reclaimed_at IS NULL AND i.writer_expires_at>clock_timestamp()
 FOR UPDATE OF command,i;

-- name: ListInstanceCommands :many
SELECT * FROM computer_commands WHERE computer_instance_id=sqlc.arg(computer_instance_id)
 AND writer_generation=sqlc.arg(writer_generation)
 AND (terminal_at IS NULL OR process_reconciled_at IS NULL)
 ORDER BY created_at,id;

-- name: StartComputerCommand :one
UPDATE computer_commands SET status='running',started_at=COALESCE(started_at,clock_timestamp()),
 revision=revision+CASE WHEN status='starting' THEN 1 ELSE 0 END,updated_at=clock_timestamp()
 WHERE id=sqlc.arg(command_id) AND computer_instance_id=sqlc.arg(computer_instance_id)
 AND writer_generation=sqlc.arg(writer_generation) AND status IN ('starting','running')
 RETURNING *;

-- Outcome publication does not capture, stop or release the physical instance.
-- The caller validates the final output acknowledgement and process observation.
-- name: CompleteComputerCommand :one
UPDATE computer_commands SET status=sqlc.arg(status),exit_code=sqlc.narg(exit_code),
 failure_reason=sqlc.narg(failure_reason),error=sqlc.narg(error),
 process_exited_at=sqlc.narg(process_exited_at),terminal_at=clock_timestamp(),
 terminal_reason_code=sqlc.arg(reason_code),result_expires_at=clock_timestamp()+interval '30 days',
 revision=revision+1,updated_at=clock_timestamp()
 WHERE id=sqlc.arg(command_id) AND computer_instance_id=sqlc.arg(computer_instance_id)
 AND writer_generation=sqlc.arg(writer_generation) AND terminal_at IS NULL
 RETURNING *;

-- name: ReconcileComputerCommand :one
UPDATE computer_commands SET process_reconciled_at=COALESCE(process_reconciled_at,clock_timestamp()),
 revision=revision+CASE WHEN process_reconciled_at IS NULL THEN 1 ELSE 0 END,updated_at=clock_timestamp()
 WHERE id=sqlc.arg(command_id) AND computer_instance_id=sqlc.arg(computer_instance_id)
 AND writer_generation=sqlc.arg(writer_generation) AND terminal_at IS NOT NULL
 RETURNING *;

-- A pending cancellation conclusively never launched. Placed work is cancelled by
-- its existing process scope, retaining placement until physical reconciliation.
-- name: RequestComputerCommandCancellation :one
UPDATE computer_commands SET cancel_requested_at=COALESCE(cancel_requested_at,clock_timestamp()),
 status=CASE WHEN computer_instance_id IS NULL THEN 'cancelled' ELSE 'stopping' END,
 terminal_at=CASE WHEN computer_instance_id IS NULL THEN clock_timestamp() END,
 terminal_reason_code=CASE WHEN computer_instance_id IS NULL THEN 'computer_command_cancelled' END,
 result_expires_at=CASE WHEN computer_instance_id IS NULL THEN clock_timestamp()+interval '30 days' END,
 revision=revision+1,updated_at=clock_timestamp()
 WHERE id=sqlc.arg(command_id) AND environment_id=sqlc.arg(environment_id) AND terminal_at IS NULL
 RETURNING *;

-- name: FailPendingComputerCommand :one
UPDATE computer_commands SET status='failed',failure_reason='placement_failed',
 error=sqlc.arg(error),terminal_at=clock_timestamp(),terminal_reason_code=sqlc.arg(reason_code),
 result_expires_at=clock_timestamp()+interval '30 days',revision=revision+1,updated_at=clock_timestamp()
 WHERE id=sqlc.arg(command_id) AND environment_id=sqlc.arg(environment_id)
 AND status='pending' AND computer_instance_id IS NULL RETURNING *;

-- name: PruneExpiredComputerCommandResults :execrows
WITH expired AS (
 SELECT id FROM computer_commands WHERE terminal_at IS NOT NULL AND result_pruned_at IS NULL
 AND result_expires_at<=clock_timestamp()
 AND (computer_instance_id IS NULL OR process_reconciled_at IS NOT NULL)
 ORDER BY result_expires_at,id LIMIT sqlc.arg(row_limit) FOR UPDATE SKIP LOCKED
)
UPDATE computer_commands c SET argv=NULL,cwd=NULL,env=NULL,stdin=NULL,error=NULL,
 result_pruned_at=clock_timestamp(),updated_at=clock_timestamp()
 FROM expired WHERE c.id=expired.id;

-- A pending Command has no process. A placed Command must acknowledge stopping
-- before its receipt becomes terminal; revocation does not retire its peers.
-- name: StopSecretRevokedComputerCommand :one
UPDATE computer_commands SET
 status=CASE WHEN computer_instance_id IS NULL THEN 'failed' ELSE 'stopping' END,
 failure_reason=CASE WHEN computer_instance_id IS NULL THEN 'guest_failure' ELSE failure_reason END,
 error=CASE WHEN computer_instance_id IS NULL THEN '{"code":"secret_revoked","retryable":false}'::jsonb ELSE error END,
 terminal_at=CASE WHEN computer_instance_id IS NULL THEN clock_timestamp() ELSE terminal_at END,
 terminal_reason_code=CASE WHEN computer_instance_id IS NULL THEN 'secret_revoked' ELSE terminal_reason_code END,
 result_expires_at=CASE WHEN computer_instance_id IS NULL THEN clock_timestamp()+interval '30 days' ELSE result_expires_at END,
 cancel_requested_at=COALESCE(cancel_requested_at,clock_timestamp()),
 revision=revision+1,updated_at=clock_timestamp()
 WHERE id=sqlc.arg(command_id) AND environment_id=sqlc.arg(environment_id)
 AND revision=sqlc.arg(expected_revision) AND terminal_at IS NULL
 RETURNING *;

-- Physical demand is one Computer, regardless of the number of pending Commands.
-- Unreclaimed instances already consume capacity, even while fenced or draining.
-- name: ListPendingComputerCommandCapacityCandidates :many
SELECT c.id AS computer_id,s.config AS computer_config,
 ARRAY(SELECT h.worker_pool_id FROM computer_instances live
   JOIN worker_hosts h ON h.id=live.worker_host_id
   WHERE live.computer_id=c.id AND live.reclaimed_at IS NULL)::uuid[] AS accounted_pool_ids,
 source.worker_group_id AS required_worker_group_id,
 COALESCE(source.vm_platform_id,'')::text AS required_vm_platform_id,
 COALESCE(source.vm_vcpu_count,0)::integer AS required_vm_vcpu_count,
 COALESCE(source.cpu_config_digest,'')::text AS required_cpu_config_digest,
 COALESCE(source.reserved_cpu_millis,0)::bigint AS required_cpu_millis,
 COALESCE(source.reserved_memory_bytes,0)::bigint AS required_memory_bytes,
 COALESCE(source.reserved_guest_ephemeral_disk_bytes,0)::bigint AS required_guest_ephemeral_disk_bytes
FROM computers c JOIN computer_specs s ON s.id=c.computer_spec_id
LEFT JOIN LATERAL (
 SELECT i.* FROM computer_checkpoints cp
 JOIN computer_instances i ON i.id=cp.source_computer_instance_id
 WHERE cp.computer_id=c.id AND cp.status='ready' AND cp.resume_committed_at IS NULL
 AND (cp.expires_at IS NULL OR cp.expires_at>clock_timestamp())
 ORDER BY cp.created_at DESC,cp.id DESC LIMIT 1
) source ON true
WHERE c.region_id=sqlc.arg(region_id) AND c.status='active' AND c.desired_state='active'
 AND c.deleted_at IS NULL AND c.recovery_failure IS NULL AND c.preparation_failure IS NULL
 AND c.dirty_state NOT IN ('capture_failed','dirty_state_lost')
 AND EXISTS(SELECT 1 FROM computer_commands cmd WHERE cmd.computer_id=c.id
   AND cmd.status='pending' AND cmd.computer_instance_id IS NULL)
ORDER BY c.created_at,c.id LIMIT sqlc.arg(row_limit);

-- Computer lock precedes this exact historical placement lock. A newer instance
-- is never substituted for the command's original process scope.
-- name: LockComputerCommandInstance :one
SELECT i.* FROM computer_instances i JOIN computer_commands c ON c.computer_instance_id=i.id
 AND c.writer_generation=i.writer_generation AND c.environment_id=i.environment_id AND c.computer_id=i.computer_id
WHERE c.environment_id=sqlc.arg(environment_id) AND c.computer_id=sqlc.arg(computer_id)
 AND c.id=sqlc.arg(command_id) FOR UPDATE OF i;

-- name: ListRecoverableComputerCommandCandidates :many
SELECT c.id,c.computer_id,c.revision,i.org_id FROM computer_commands c
JOIN computer_instances i ON i.id=c.computer_instance_id AND i.writer_generation=c.writer_generation
WHERE c.process_reconciled_at IS NULL AND
 (i.reclaimed_at IS NOT NULL OR (c.terminal_at IS NULL AND (i.observed_state IN ('failed','lost')
  OR i.mount_state IN ('lost','failed') OR i.writer_expires_at<=clock_timestamp())))
ORDER BY c.updated_at,c.id LIMIT sqlc.arg(row_limit);
