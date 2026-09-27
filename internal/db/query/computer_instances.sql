-- The caller locks Worker capacity, then the Computer and instance.
-- A physical allocation is the only operation that advances writer_generation.
-- name: AllocateComputerInstance :one
WITH advanced AS (
 UPDATE computers c SET writer_generation=sqlc.arg(writer_generation),revision=revision+1,updated_at=clock_timestamp()
 FROM computer_disk_versions head
 LEFT JOIN computer_checkpoints checkpoint ON checkpoint.id=sqlc.narg(source_checkpoint_id)
 WHERE c.id=sqlc.arg(computer_id) AND c.environment_id=sqlc.arg(environment_id)
 AND c.computer_spec_id=sqlc.arg(computer_spec_id)
 AND c.writer_generation=sqlc.arg(writer_generation)::bigint-1
 AND c.status='active' AND c.deleted_at IS NULL
 AND head.id=c.head_disk_version_id AND head.environment_id=c.environment_id AND head.computer_id=c.id
 AND head.status IN ('initializing','committed')
 AND (sqlc.narg(source_checkpoint_id)::uuid IS NULL OR (
      checkpoint.environment_id=c.environment_id AND checkpoint.computer_id=c.id
      AND checkpoint.computer_spec_id=c.computer_spec_id AND checkpoint.status='ready'
      AND checkpoint.private_computer_disk_version_id IS NOT NULL
      AND checkpoint.program_deployment_id IS NOT DISTINCT FROM sqlc.narg(program_deployment_id)::uuid
      AND checkpoint.resume_committed_at IS NULL
      AND (checkpoint.expires_at IS NULL OR checkpoint.expires_at>clock_timestamp())))
 AND NOT EXISTS(SELECT 1 FROM computer_instances i WHERE i.computer_id=c.id AND i.reclaimed_at IS NULL)
 RETURNING c.*,CASE WHEN checkpoint.id IS NOT NULL THEN checkpoint.private_computer_disk_version_id
                   WHEN head.status='committed' THEN head.id END AS allocation_source_disk_version_id
)
INSERT INTO computer_instances (
 id,org_id,project_id,environment_id,region_id,worker_group_id,worker_host_id,worker_epoch,
 vm_platform_id,vm_vcpu_count,cpu_config_digest,reserved_cpu_millis,reserved_memory_bytes,
 reserved_guest_ephemeral_disk_bytes,reserved_execution_slots,computer_id,computer_spec_id,
 program_deployment_id,source_checkpoint_id,source_disk_version_id,preparation_expires_at,
 desired_reason,writer_generation,writer_token_hash,writer_expires_at,admission_state
)
SELECT sqlc.arg(id),e.org_id,e.project_id,c.environment_id,c.region_id,
 sqlc.arg(worker_group_id),sqlc.arg(worker_host_id),sqlc.arg(worker_epoch),
 sqlc.arg(vm_platform_id),sqlc.arg(vm_vcpu_count),sqlc.arg(cpu_config_digest),
 sqlc.arg(reserved_cpu_millis),sqlc.arg(reserved_memory_bytes),sqlc.arg(reserved_guest_ephemeral_disk_bytes),
 sqlc.arg(reserved_execution_slots),c.id,c.computer_spec_id,sqlc.narg(program_deployment_id),
 sqlc.narg(source_checkpoint_id),c.allocation_source_disk_version_id,
 clock_timestamp()+sqlc.arg(preparation_seconds)::bigint*interval '1 second',
 sqlc.arg(reason),c.writer_generation,sqlc.arg(writer_token_hash),
 clock_timestamp()+sqlc.arg(writer_ttl_seconds)::bigint*interval '1 second',
 CASE WHEN sqlc.narg(source_checkpoint_id)::uuid IS NULL THEN 'open' ELSE 'restoring' END
 FROM advanced c JOIN environments e ON e.id=c.environment_id
RETURNING *;

-- name: LockComputerInstance :one
SELECT * FROM computer_instances WHERE computer_id=sqlc.arg(computer_id)
 AND environment_id=sqlc.arg(environment_id) AND reclaimed_at IS NULL FOR UPDATE;

-- name: GetComputerInstance :one
SELECT * FROM computer_instances WHERE id=sqlc.arg(id) AND environment_id=sqlc.arg(environment_id);

-- name: LockWorkerComputerInstance :one
SELECT * FROM computer_instances WHERE id=sqlc.arg(id) AND org_id=sqlc.arg(org_id)
 AND worker_host_id=sqlc.arg(worker_host_id) AND worker_epoch=sqlc.arg(worker_epoch)
 AND worker_group_id=sqlc.arg(worker_group_id) FOR UPDATE;

-- name: RenewComputerInstanceWriter :one
UPDATE computer_instances SET writer_expires_at=clock_timestamp()+sqlc.arg(ttl_seconds)::bigint*interval '1 second',
 guest_channel_token_expires_at=CASE WHEN guest_channel_token_hash IS NOT NULL
 THEN clock_timestamp()+sqlc.arg(ttl_seconds)::bigint*interval '1 second' END,
 updated_at=clock_timestamp()
 WHERE id=sqlc.arg(id) AND writer_generation=sqlc.arg(writer_generation)
 AND writer_token_hash=sqlc.arg(writer_token_hash)
 AND worker_host_id=sqlc.arg(worker_host_id) AND worker_epoch=sqlc.arg(worker_epoch)
 AND writer_expires_at>clock_timestamp() AND reclaimed_at IS NULL
 AND desired_state='ready' AND admission_state<>'closed'
 RETURNING *;

-- name: ListComputerInstanceReconcileTargets :many
SELECT i.*,spec.config AS computer_config,spec.digest AS computer_spec_digest,
 spec.seed_digest AS computer_image_digest,spec.seed_size_bytes AS computer_image_size_bytes,
 spec.seed_media_type AS computer_image_media_type,
 source.id AS preparation_disk_version_id,
 c.initial_config AS computer_initial_config,root.locator AS computer_generation_locator,
 source.status AS computer_disk_version_status,source.root_pack_digest AS computer_content_digest,
 source.logical_bytes AS computer_logical_size_bytes,
 platform.arch AS computer_architecture,platform.rootfs_digest,platform.contract AS vm_contract,
 program.runtime_artifact_digest AS program_runtime_digest,program.program_index_digest,
 artifact.digest AS program_artifact_digest,artifact.size_bytes AS program_artifact_size_bytes,
 artifact.media_type AS program_artifact_media_type
 FROM computer_instances i JOIN computers c ON c.id=i.computer_id AND c.environment_id=i.environment_id
 JOIN computer_specs spec ON spec.id=i.computer_spec_id AND spec.environment_id=i.environment_id
 JOIN vm_platforms platform ON platform.id=i.vm_platform_id
 LEFT JOIN computer_disk_versions source ON source.id=COALESCE(i.source_disk_version_id,c.head_disk_version_id) AND source.computer_id=i.computer_id
 LEFT JOIN computer_disk_version_roots root ON root.version_id=source.id AND root.computer_id=i.computer_id
 LEFT JOIN deployments program ON program.id=i.program_deployment_id AND program.environment_id=i.environment_id
 LEFT JOIN artifacts artifact ON artifact.id=program.program_artifact_id AND artifact.environment_id=i.environment_id
 WHERE i.worker_host_id=sqlc.arg(worker_host_id) AND i.worker_epoch=sqlc.arg(worker_epoch)
 AND i.worker_group_id=sqlc.arg(worker_group_id) AND i.reclaimed_at IS NULL
 AND (i.observed_desired_version<i.desired_version OR i.observed_state IN ('failed','lost')
      OR i.admission_state IN ('checkpointing','restoring'))
 ORDER BY i.desired_at,i.id LIMIT sqlc.arg(row_limit);

-- name: MarkComputerInstanceMounting :one
UPDATE computer_instances SET mount_state='mounting',updated_at=clock_timestamp()
 WHERE id=sqlc.arg(id) AND writer_generation=sqlc.arg(writer_generation)
 AND desired_state='ready' AND mount_state IN ('pending','mounting') AND reclaimed_at IS NULL
 RETURNING *;

-- name: MarkComputerInstanceReady :one
UPDATE computer_instances SET observed_state='ready',observed_version=observed_version+1,
 observed_desired_version=sqlc.arg(desired_version),observed_at=clock_timestamp(),
 ready_at=COALESCE(ready_at,clock_timestamp()),mount_state='mounted',mounted_at=COALESCE(mounted_at,clock_timestamp()),
 updated_at=clock_timestamp()
 WHERE id=sqlc.arg(id) AND worker_host_id=sqlc.arg(worker_host_id) AND worker_epoch=sqlc.arg(worker_epoch)
 AND writer_generation=sqlc.arg(writer_generation) AND desired_version=sqlc.arg(desired_version)
 AND writer_expires_at>clock_timestamp() AND desired_state='ready'
 AND observed_version=sqlc.arg(expected_observed_version)
 AND vm_vcpu_count=sqlc.arg(vm_vcpu_count) AND cpu_config_digest=sqlc.arg(cpu_config_digest)
 AND (observed_state='ready' OR (observed_state='allocated' AND preparation_expires_at>clock_timestamp()))
 AND reclaimed_at IS NULL
 RETURNING *;

-- Program preparation is acknowledged through the Instance desired version.
-- It cannot change a resident Program or authorize any member by itself.
-- name: PrepareComputerInstanceProgram :one
UPDATE computer_instances SET program_deployment_id=sqlc.arg(deployment_id),
 desired_version=desired_version+1,desired_at=clock_timestamp(),updated_at=clock_timestamp()
 WHERE id=sqlc.arg(id) AND environment_id=sqlc.arg(environment_id)
 AND writer_generation=sqlc.arg(writer_generation) AND admission_state='open'
 AND desired_state='ready' AND observed_state IN ('allocated','ready')
 AND writer_expires_at>clock_timestamp() AND reclaimed_at IS NULL
 AND program_deployment_id IS NULL
 RETURNING *;

-- name: AdvanceComputerInstanceMembership :one
UPDATE computer_instances SET membership_revision=membership_revision+1,updated_at=clock_timestamp()
 WHERE id=sqlc.arg(id) AND writer_generation=sqlc.arg(writer_generation) AND reclaimed_at IS NULL
 RETURNING *;

-- Logical completion cannot call this operation. Its owner must establish idle,
-- deletion, loss or whole-instance failure under the Computer and instance locks.
-- name: RequestComputerInstanceClose :one
UPDATE computer_instances SET desired_state='closed',desired_version=desired_version+1,
 desired_at=clock_timestamp(),desired_reason=sqlc.arg(reason),admission_state='closed',
 finalization_action=sqlc.arg(finalization_action),finalization_reason_code=sqlc.arg(reason),
 finalization_error=sqlc.narg(error),mount_state=CASE WHEN mount_state='mounted' THEN 'unmounting' ELSE mount_state END,
 updated_at=clock_timestamp()
 WHERE id=sqlc.arg(id) AND writer_generation=sqlc.arg(writer_generation)
 AND desired_state='ready' AND reclaimed_at IS NULL RETURNING *;

-- Caller has validated signed Worker claims and actual exclusion evidence.
-- name: ReclaimComputerInstance :one
UPDATE computer_instances SET observed_state=sqlc.arg(observed_state),observed_version=observed_version+1,
 observed_desired_version=sqlc.arg(desired_version),observed_at=clock_timestamp(),
 terminal_at=COALESCE(terminal_at,clock_timestamp()),terminal_reason_code=sqlc.arg(reason),
 terminal_error=sqlc.narg(error),reclaimed_at=clock_timestamp(),reclaim_evidence=sqlc.arg(evidence),
 admission_state='closed',mount_state=sqlc.arg(mount_state),unmounted_at=clock_timestamp(),updated_at=clock_timestamp()
 WHERE id=sqlc.arg(id) AND worker_host_id=sqlc.arg(worker_host_id) AND worker_epoch=sqlc.arg(worker_epoch)
 AND writer_generation=sqlc.arg(writer_generation) AND desired_version=sqlc.arg(desired_version)
 AND observed_version=sqlc.arg(expected_observed_version)
 AND desired_state='closed' AND reclaimed_at IS NULL
 AND (NOT sqlc.arg(require_failure)::boolean OR observed_state IN ('failed','lost')) RETURNING *;

-- Warm idle residence has no reservation deadline. Only unfinished preparation
-- and the physical writer's authority have deadlines here.
-- name: ListExpiredComputerInstances :many
SELECT * FROM computer_instances
 WHERE reclaimed_at IS NULL AND desired_state='ready'
 AND (writer_expires_at<=clock_timestamp()
      OR ((observed_state='allocated' OR admission_state='restoring') AND preparation_expires_at<=clock_timestamp()))
 ORDER BY LEAST(writer_expires_at,CASE WHEN (observed_state='allocated' OR admission_state='restoring') THEN preparation_expires_at ELSE writer_expires_at END),id
 LIMIT sqlc.arg(row_limit);

-- Caller holds Computer then Instance locks. Expiry requests physical teardown;
-- it never fabricates exclusion evidence or releases reserved capacity/pins.
-- name: ExpireComputerInstance :one
UPDATE computer_instances SET desired_state='closed',desired_version=desired_version+1,
 desired_at=clock_timestamp(),
 desired_reason=CASE WHEN (observed_state='allocated' OR admission_state='restoring') AND preparation_expires_at<=clock_timestamp()
                     THEN 'computer_preparation_expired' ELSE 'computer_writer_expired' END,
 admission_state='closed',finalization_action='discard',
 finalization_reason_code=CASE WHEN (observed_state='allocated' OR admission_state='restoring') AND preparation_expires_at<=clock_timestamp()
                              THEN 'computer_preparation_expired' ELSE 'computer_writer_expired' END,
 mount_state=CASE WHEN mount_state='mounted' THEN 'unmounting' ELSE mount_state END,
 updated_at=clock_timestamp()
 WHERE id=sqlc.arg(id) AND environment_id=sqlc.arg(environment_id)
 AND writer_generation=sqlc.arg(writer_generation) AND desired_version=sqlc.arg(desired_version)
 AND desired_state='ready' AND reclaimed_at IS NULL
 AND (writer_expires_at<=clock_timestamp()
      OR ((observed_state='allocated' OR admission_state='restoring') AND preparation_expires_at<=clock_timestamp()))
 RETURNING *;

-- name: GetWorkerComputerInstanceTarget :one
SELECT i.org_id,i.environment_id,i.computer_id,h.worker_pool_id
FROM computer_instances i JOIN worker_hosts h ON h.id=i.worker_host_id AND h.worker_group_id=i.worker_group_id
WHERE i.id=sqlc.arg(id) AND i.worker_group_id=sqlc.arg(worker_group_id)
 AND i.worker_host_id=sqlc.arg(worker_host_id) AND i.worker_epoch=sqlc.arg(worker_epoch);

-- Worker failure is a physical fact, not a Run retry or proof of exclusion.
-- name: MarkComputerInstanceFailed :one
UPDATE computer_instances SET observed_state='failed',observed_version=observed_version+1,
 observed_desired_version=sqlc.arg(desired_version),observed_at=clock_timestamp(),
 terminal_at=clock_timestamp(),terminal_reason_code=sqlc.arg(reason_code),terminal_error=sqlc.narg(error),
 desired_state='closed',desired_version=desired_version+CASE WHEN desired_state='ready' THEN 1 ELSE 0 END,
 desired_at=clock_timestamp(),desired_reason=sqlc.arg(reason_code),admission_state='closed',
 finalization_action='discard',finalization_reason_code=sqlc.arg(reason_code),finalization_error=sqlc.narg(error),
 mount_state=CASE WHEN mount_state='mounted' THEN 'unmounting' ELSE mount_state END,
 updated_at=clock_timestamp()
WHERE id=sqlc.arg(id) AND worker_host_id=sqlc.arg(worker_host_id) AND worker_epoch=sqlc.arg(worker_epoch)
 AND desired_version=sqlc.arg(desired_version) AND observed_version=sqlc.arg(expected_observed_version)
 AND observed_state IN ('allocated','ready') AND reclaimed_at IS NULL
RETURNING *;

-- name: ListUnclaimedWorkerComputerInstances :many
SELECT id,environment_id,computer_id FROM computer_instances
 WHERE worker_group_id=sqlc.arg(worker_group_id) AND worker_host_id=sqlc.arg(worker_host_id)
 AND worker_epoch=sqlc.arg(worker_epoch) AND observed_state='ready' AND desired_state='ready'
 AND writer_expires_at>clock_timestamp() AND reclaimed_at IS NULL
 AND guest_channel_token_hash IS NULL AND admission_state IN ('open','restoring')
 ORDER BY id LIMIT 64;

-- Caller holds Secret bindings, Worker Group/Host, Computer and Instance locks.
-- A channel is issued once per physical Instance. Lost delivery is reconciled by
-- Instance expiry; it cannot rotate a live Guest's authority behind its owner.
-- name: ClaimComputerInstanceChannel :one
UPDATE computer_instances AS i SET guest_channel_token_hash=sqlc.arg(token_hash),
 guest_channel_token_expires_at=writer_expires_at,updated_at=clock_timestamp()
 WHERE i.id=sqlc.arg(id) AND worker_host_id=sqlc.arg(worker_host_id) AND worker_epoch=sqlc.arg(worker_epoch)
 AND writer_generation=sqlc.arg(writer_generation) AND guest_channel_token_hash IS NULL
 AND writer_expires_at>clock_timestamp() AND reclaimed_at IS NULL
 AND desired_state='ready' AND observed_state='ready' AND observed_desired_version=desired_version AND mount_state='mounted'
 AND EXISTS(SELECT 1 FROM worker_hosts h WHERE h.id=i.worker_host_id
 AND h.observed_at>=clock_timestamp()-sqlc.arg(worker_freshness_seconds)::bigint*interval '1 second' AND h.run_paused_reason IS NULL)
 AND admission_state IN ('open','restoring')
 RETURNING i.*;

-- name: GetComputerInstanceAssignmentSource :one
SELECT spec.seed_digest,spec.seed_size_bytes,spec.seed_media_type,platform.rootfs_digest,platform.contract
 FROM computer_specs spec JOIN computer_instances i ON i.computer_spec_id=spec.id AND i.environment_id=spec.environment_id
 JOIN vm_platforms platform ON platform.id=i.vm_platform_id WHERE i.id=sqlc.arg(id);
