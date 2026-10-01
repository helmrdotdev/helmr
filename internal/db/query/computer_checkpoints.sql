-- The coordinator holds the Computer and instance locks, followed by all resident
-- Run/wait locks in stable order. Logical wait registration never calls this by
-- itself. Any active or unreconciled non-waiting scope keeps the instance resident.
-- name: BeginComputerCheckpoint :one
WITH barrier AS (
 UPDATE computer_instances i SET admission_state='checkpointing',capture_checkpoint_id=sqlc.arg(checkpoint_id),
 desired_version=desired_version+1,desired_at=clock_timestamp(),desired_reason='checkpoint',updated_at=clock_timestamp()
 FROM computers c
 WHERE i.id=sqlc.arg(computer_instance_id) AND i.environment_id=sqlc.arg(environment_id)
 AND i.writer_generation=sqlc.arg(writer_generation) AND i.membership_revision=sqlc.arg(membership_revision)
 AND i.desired_version=sqlc.arg(desired_version) AND i.desired_state='ready'
 AND i.observed_state='ready' AND i.observed_desired_version=i.desired_version
 AND i.mount_state='mounted' AND i.admission_state IN ('open','draining') AND i.capture_checkpoint_id IS NULL
 AND i.reclaimed_at IS NULL AND i.writer_expires_at>clock_timestamp() AND i.save_disk_version_id IS NULL
 AND c.id=i.computer_id AND c.environment_id=i.environment_id AND c.status='active'
 AND c.writer_generation=i.writer_generation
 AND NOT EXISTS(SELECT 1 FROM computer_commands command WHERE command.computer_id=i.computer_id
                 AND (command.terminal_at IS NULL OR (command.computer_instance_id IS NOT NULL AND command.process_reconciled_at IS NULL)))
 AND NOT EXISTS(
  SELECT 1 FROM run_leases lease WHERE lease.computer_instance_id=i.id AND lease.process_reconciled_at IS NULL
  AND NOT (lease.status='running' AND lease.expires_at>clock_timestamp() AND EXISTS(
   SELECT 1 FROM runs r JOIN run_waits w ON w.run_id=r.id AND w.attempt_number=r.current_attempt_number
   WHERE r.id=lease.run_id AND r.status='waiting' AND r.current_run_lease_id=lease.id
   AND w.current_run_lease_id=lease.id AND w.suspension_status='hot' AND w.condition_status='pending'
   AND w.suspend_checkpoint_id IS NULL)))
 RETURNING i.*,c.head_disk_version_id
)
INSERT INTO computer_checkpoints(id,environment_id,computer_id,computer_spec_id,
 source_computer_instance_id,writer_generation,membership_revision,program_deployment_id,
 base_computer_disk_version_id,expires_at)
SELECT sqlc.arg(checkpoint_id),environment_id,computer_id,computer_spec_id,id,
 writer_generation,membership_revision,program_deployment_id,head_disk_version_id,sqlc.narg(expires_at)
FROM barrier RETURNING *;

-- Capture membership is inserted in the same transaction as the barrier. The
-- coordinator compares the complete resident set before committing capture intent.
-- name: CreateComputerCheckpointRun :one
INSERT INTO computer_checkpoint_runs(checkpoint_id,environment_id,computer_id,run_id,
 attempt_number,run_wait_id,source_run_lease_id,actor_speculative_input_sequence,
 source_computer_instance_id,writer_generation)
SELECT checkpoint.id,checkpoint.environment_id,checkpoint.computer_id,lease.run_id,
 lease.attempt_number,wait.id,lease.id,sqlc.narg(actor_speculative_input_sequence),
 checkpoint.source_computer_instance_id,checkpoint.writer_generation
FROM computer_checkpoints checkpoint JOIN computer_instances instance
 ON instance.id=checkpoint.source_computer_instance_id AND instance.capture_checkpoint_id=checkpoint.id
 AND instance.writer_generation=checkpoint.writer_generation AND instance.membership_revision=checkpoint.membership_revision
JOIN run_leases lease ON lease.computer_instance_id=instance.id AND lease.writer_generation=instance.writer_generation
JOIN runs r ON r.id=lease.run_id AND r.current_run_lease_id=lease.id AND r.status='waiting'
JOIN run_waits wait ON wait.run_id=r.id AND wait.attempt_number=lease.attempt_number AND wait.current_run_lease_id=lease.id
WHERE checkpoint.id=sqlc.arg(checkpoint_id) AND checkpoint.environment_id=sqlc.arg(environment_id)
 AND checkpoint.status='creating' AND instance.admission_state='checkpointing'
 AND lease.id=sqlc.arg(run_lease_id) AND lease.status='running' AND lease.process_reconciled_at IS NULL
 AND wait.id=sqlc.arg(run_wait_id) AND wait.suspension_status='hot' AND wait.condition_status='pending'
RETURNING *;

-- name: LockComputerCheckpoint :one
SELECT * FROM computer_checkpoints WHERE environment_id=sqlc.arg(environment_id)
 AND computer_id=sqlc.arg(computer_id) AND id=sqlc.arg(checkpoint_id) FOR UPDATE;

-- name: ListComputerCheckpointRuns :many
SELECT * FROM computer_checkpoint_runs WHERE environment_id=sqlc.arg(environment_id)
 AND checkpoint_id=sqlc.arg(checkpoint_id) ORDER BY run_id;

-- name: GetCheckpointReadyReplay :one
SELECT * FROM computer_checkpoints WHERE environment_id=sqlc.arg(environment_id)
 AND id=sqlc.arg(checkpoint_id) AND status='ready' AND ready_request_fingerprint IS NOT NULL;


-- name: GetVMPlatformForCheckpoint :one
SELECT * FROM vm_platforms WHERE id=sqlc.arg(id);

-- name: CreatePrivateCheckpointComputerDiskVersion :one
INSERT INTO computer_disk_versions(id,environment_id,computer_id,parent_version_id,
 root_pack_digest,logical_bytes,status,source_computer_instance_id,writer_generation)
SELECT sqlc.arg(id),checkpoint.environment_id,checkpoint.computer_id,checkpoint.base_computer_disk_version_id,
 sqlc.arg(root_pack_digest),sqlc.arg(logical_bytes),'private',checkpoint.source_computer_instance_id,checkpoint.writer_generation
FROM computer_checkpoints checkpoint WHERE checkpoint.id=sqlc.arg(checkpoint_id)
 AND checkpoint.environment_id=sqlc.arg(environment_id) AND checkpoint.status='creating'
RETURNING *;

-- The caller certifies artifact contents and the private disk root, then settles
-- every captured Run/wait and requests physical exclusion in this transaction.
-- name: MarkComputerCheckpointReady :one
UPDATE computer_checkpoints checkpoint SET status='ready',
 private_computer_disk_version_id=sqlc.arg(private_computer_disk_version_id),
 vm_config_artifact_id=sqlc.arg(vm_config_artifact_id),vm_state_artifact_id=sqlc.arg(vm_state_artifact_id),
 memory_artifact_id=sqlc.arg(memory_artifact_id),scratch_disk_artifact_id=sqlc.arg(scratch_disk_artifact_id),
 manifest=sqlc.arg(manifest),phase_timings=sqlc.narg(phase_timings),
 ready_request_fingerprint=sqlc.arg(ready_request_fingerprint),ready_at=clock_timestamp()
FROM computer_instances instance
WHERE checkpoint.id=sqlc.arg(checkpoint_id) AND checkpoint.environment_id=sqlc.arg(environment_id)
 AND checkpoint.status='creating'
 AND (checkpoint.expires_at IS NULL OR checkpoint.expires_at>clock_timestamp())
 AND instance.id=checkpoint.source_computer_instance_id
 AND instance.capture_checkpoint_id=checkpoint.id AND instance.admission_state='checkpointing'
 AND instance.writer_generation=checkpoint.writer_generation AND instance.membership_revision=checkpoint.membership_revision
 AND instance.desired_version=sqlc.arg(desired_version) AND instance.reclaimed_at IS NULL
 AND instance.writer_expires_at>clock_timestamp()
RETURNING checkpoint.*;


-- name: GetReadyComputerCheckpoint :one
SELECT checkpoint.* FROM computer_checkpoint_runs member JOIN computer_checkpoints checkpoint ON checkpoint.id=member.checkpoint_id
JOIN run_waits wait ON wait.id=member.run_wait_id AND wait.run_id=member.run_id
 AND wait.attempt_number=member.attempt_number AND wait.suspend_checkpoint_id=checkpoint.id
WHERE member.environment_id=sqlc.arg(environment_id) AND member.run_id=sqlc.arg(run_id)
 AND member.attempt_number=sqlc.arg(attempt_number) AND member.run_wait_id=sqlc.arg(run_wait_id)
 AND checkpoint.status='ready' AND (checkpoint.expires_at IS NULL OR checkpoint.expires_at>clock_timestamp());

-- One destination is committed before activation. The caller inserts the durable
-- activation outbox record and all fresh Run grants in this same transaction.
-- name: CommitComputerCheckpointRestore :one
UPDATE computer_checkpoints checkpoint SET resume_computer_instance_id=destination.id,resume_committed_at=clock_timestamp()
FROM computer_instances source,computer_instances destination
WHERE checkpoint.id=sqlc.arg(checkpoint_id) AND checkpoint.environment_id=sqlc.arg(environment_id)
 AND checkpoint.status='ready' AND checkpoint.resume_committed_at IS NULL
 AND (checkpoint.expires_at IS NULL OR checkpoint.expires_at>clock_timestamp())
 AND source.id=checkpoint.source_computer_instance_id AND source.writer_generation=checkpoint.writer_generation
 AND source.reclaimed_at IS NOT NULL
 AND destination.id=sqlc.arg(computer_instance_id) AND destination.source_checkpoint_id=checkpoint.id
 AND destination.environment_id=checkpoint.environment_id AND destination.computer_id=checkpoint.computer_id
 AND destination.computer_spec_id=checkpoint.computer_spec_id
 AND destination.program_deployment_id IS NOT DISTINCT FROM checkpoint.program_deployment_id
 AND destination.source_disk_version_id=checkpoint.private_computer_disk_version_id
 AND destination.writer_generation=sqlc.arg(writer_generation) AND destination.writer_generation>source.writer_generation
 AND destination.reclaimed_at IS NULL AND destination.admission_state='restoring'
 AND destination.desired_state='ready' AND destination.desired_version=sqlc.arg(desired_version)
 AND destination.writer_expires_at>clock_timestamp()
RETURNING checkpoint.*;

-- Authenticated activation acknowledgement is accepted only for the committed
-- destination. Losing it after commit cannot make this checkpoint reusable.
-- name: OpenRestoredComputerInstance :one
UPDATE computer_instances instance SET admission_state='open',updated_at=clock_timestamp()
FROM computer_checkpoints checkpoint
WHERE instance.id=sqlc.arg(computer_instance_id) AND instance.environment_id=sqlc.arg(environment_id)
 AND instance.writer_generation=sqlc.arg(writer_generation) AND instance.desired_version=sqlc.arg(desired_version)
 AND instance.worker_host_id=sqlc.arg(worker_host_id) AND instance.worker_epoch=sqlc.arg(worker_epoch)
 AND instance.admission_state='restoring' AND instance.desired_state='ready' AND instance.observed_state='ready'
 AND instance.observed_desired_version=instance.desired_version AND instance.mount_state='mounted'
 AND instance.writer_expires_at>clock_timestamp() AND instance.reclaimed_at IS NULL
 AND checkpoint.id=instance.source_checkpoint_id AND checkpoint.resume_computer_instance_id=instance.id
 AND checkpoint.resume_committed_at IS NOT NULL
RETURNING instance.*;

-- Discovery for a frozen restore is keyed by its destination, not a member Run.
-- Activation still requires the locked one-shot commit and its durable outbox.
-- name: GetComputerInstanceRestoreCheckpoint :one
SELECT sqlc.embed(checkpoint),
 config.digest AS vm_config_digest,config.size_bytes AS vm_config_size_bytes,config.media_type AS vm_config_media_type,
 state.digest AS vm_state_digest,state.size_bytes AS vm_state_size_bytes,state.media_type AS vm_state_media_type,
 memory.digest AS memory_digest,memory.size_bytes AS memory_size_bytes,memory.media_type AS memory_media_type,
 scratch.digest AS scratch_disk_digest,scratch.size_bytes AS scratch_disk_size_bytes,scratch.media_type AS scratch_disk_media_type
FROM computer_instances destination
JOIN computers computer ON computer.id=destination.computer_id AND computer.environment_id=destination.environment_id
JOIN computer_checkpoints checkpoint ON checkpoint.id=destination.source_checkpoint_id
 AND checkpoint.environment_id=destination.environment_id AND checkpoint.computer_id=destination.computer_id
 AND checkpoint.computer_spec_id=destination.computer_spec_id
 AND checkpoint.program_deployment_id IS NOT DISTINCT FROM destination.program_deployment_id
 AND checkpoint.private_computer_disk_version_id=destination.source_disk_version_id
JOIN computer_instances source ON source.id=checkpoint.source_computer_instance_id
 AND source.writer_generation=checkpoint.writer_generation AND source.reclaimed_at IS NOT NULL
 AND source.vm_platform_id=destination.vm_platform_id AND source.vm_vcpu_count=destination.vm_vcpu_count
 AND source.cpu_config_digest=destination.cpu_config_digest
JOIN artifacts config ON config.id=checkpoint.vm_config_artifact_id AND config.environment_id=checkpoint.environment_id AND config.kind='computer_checkpoint_vm_config'
JOIN artifacts state ON state.id=checkpoint.vm_state_artifact_id AND state.environment_id=checkpoint.environment_id AND state.kind='computer_checkpoint_vm_state'
JOIN artifacts memory ON memory.id=checkpoint.memory_artifact_id AND memory.environment_id=checkpoint.environment_id AND memory.kind='computer_checkpoint_memory'
JOIN artifacts scratch ON scratch.id=checkpoint.scratch_disk_artifact_id AND scratch.environment_id=checkpoint.environment_id AND scratch.kind='computer_checkpoint_scratch_disk'
WHERE destination.id=sqlc.arg(computer_instance_id) AND destination.environment_id=sqlc.arg(environment_id)
 AND destination.worker_group_id=sqlc.arg(worker_group_id) AND destination.worker_host_id=sqlc.arg(worker_host_id)
 AND destination.worker_epoch=sqlc.arg(worker_epoch) AND destination.desired_version=sqlc.arg(desired_version)
 AND destination.admission_state='restoring' AND destination.desired_state='ready'
 AND destination.observed_state NOT IN ('failed','lost','closed') AND destination.reclaimed_at IS NULL
 AND destination.writer_generation=computer.writer_generation AND destination.writer_generation>source.writer_generation
 AND destination.writer_expires_at>clock_timestamp() AND computer.status='active'
 AND checkpoint.status='ready' AND (checkpoint.expires_at IS NULL OR checkpoint.expires_at>clock_timestamp())
 AND (checkpoint.resume_computer_instance_id IS NULL OR checkpoint.resume_computer_instance_id=destination.id);

-- Capture discovery identifies a sealed physical operation, never one member's
-- wait. Condition outcomes may change while this barrier remains closed.
-- name: GetComputerInstanceCaptureCheckpoint :one
SELECT checkpoint.* FROM computer_instances instance
JOIN computers computer ON computer.id=instance.computer_id AND computer.environment_id=instance.environment_id
JOIN computer_checkpoints checkpoint ON checkpoint.id=instance.capture_checkpoint_id
 AND checkpoint.environment_id=instance.environment_id AND checkpoint.computer_id=instance.computer_id
 AND checkpoint.source_computer_instance_id=instance.id AND checkpoint.writer_generation=instance.writer_generation
 AND checkpoint.membership_revision=instance.membership_revision AND checkpoint.computer_spec_id=instance.computer_spec_id
 AND checkpoint.program_deployment_id IS NOT DISTINCT FROM instance.program_deployment_id
JOIN worker_hosts worker ON worker.id=instance.worker_host_id AND worker.worker_group_id=instance.worker_group_id
 AND worker.current_epoch=instance.worker_epoch AND worker.vm_platform_id=instance.vm_platform_id
JOIN worker_groups worker_group ON worker_group.id=instance.worker_group_id
WHERE instance.id=sqlc.arg(computer_instance_id) AND instance.environment_id=sqlc.arg(environment_id)
 AND instance.worker_group_id=sqlc.arg(worker_group_id) AND instance.worker_host_id=sqlc.arg(worker_host_id)
 AND instance.worker_epoch=sqlc.arg(worker_epoch) AND instance.desired_version=sqlc.arg(desired_version)
 AND instance.admission_state='checkpointing' AND instance.desired_state='ready'
 AND instance.observed_state='ready' AND instance.mount_state='mounted' AND instance.reclaimed_at IS NULL
 AND instance.writer_generation=computer.writer_generation AND instance.writer_expires_at>clock_timestamp()
 AND computer.status='active' AND computer.desired_state='active'
 AND checkpoint.status='creating' AND (checkpoint.expires_at IS NULL OR checkpoint.expires_at>clock_timestamp())
 AND worker.status IN ('active','draining') AND worker_group.status IN ('active','paused','draining')
 AND worker.observed_at>=clock_timestamp()-sqlc.arg(worker_freshness_seconds)::bigint*interval '1 second';

-- Post-lock time check only. The caller (computer.BeginCapture) must
-- already hold the Worker Group, Worker Host, Computer, Instance and resident
-- owner locks and have validated Group/Host status and epoch. Run and VM pauses
-- do not apply: capture continues resident work. A host that was never
-- observed is not fresh.
-- name: GetComputerCaptureWorkerFresh :one
SELECT COALESCE(observed_at>=clock_timestamp()-sqlc.arg(worker_freshness_seconds)::bigint*interval '1 second'
 AND (sqlc.narg(expires_at)::timestamptz IS NULL OR sqlc.narg(expires_at)>clock_timestamp()),false)::boolean AS fresh
 FROM worker_hosts WHERE id=sqlc.arg(id);


-- Discovery retains the sealed membership while the same source acknowledges abort.
-- name: GetComputerInstanceCaptureAbort :one
SELECT checkpoint.* FROM computer_instances instance
JOIN computers computer ON computer.id=instance.computer_id AND computer.environment_id=instance.environment_id
JOIN computer_checkpoints checkpoint ON checkpoint.id=instance.capture_checkpoint_id
 AND checkpoint.environment_id=instance.environment_id AND checkpoint.computer_id=instance.computer_id
 AND checkpoint.source_computer_instance_id=instance.id AND checkpoint.writer_generation=instance.writer_generation
 AND checkpoint.membership_revision=instance.membership_revision AND checkpoint.computer_spec_id=instance.computer_spec_id
 AND checkpoint.program_deployment_id IS NOT DISTINCT FROM instance.program_deployment_id
JOIN worker_hosts worker ON worker.id=instance.worker_host_id AND worker.worker_group_id=instance.worker_group_id
 AND worker.current_epoch=instance.worker_epoch AND worker.vm_platform_id=instance.vm_platform_id
JOIN worker_groups worker_group ON worker_group.id=instance.worker_group_id
WHERE instance.id=sqlc.arg(computer_instance_id) AND instance.environment_id=sqlc.arg(environment_id)
 AND instance.worker_group_id=sqlc.arg(worker_group_id) AND instance.worker_host_id=sqlc.arg(worker_host_id)
 AND instance.worker_epoch=sqlc.arg(worker_epoch) AND instance.desired_version=sqlc.arg(desired_version)
 AND instance.admission_state='resuming_capture' AND instance.desired_state='ready'
 AND instance.observed_state='ready' AND instance.mount_state='mounted' AND instance.reclaimed_at IS NULL
 AND instance.writer_generation=computer.writer_generation AND instance.writer_expires_at>clock_timestamp()
 AND computer.status='active' AND computer.desired_state='active'
 AND checkpoint.status='aborted' AND checkpoint.abort_desired_version=instance.desired_version AND checkpoint.abort_acknowledged_at IS NULL
 AND worker.status IN ('active','draining') AND worker_group.status IN ('active','paused','draining')
 AND worker.observed_at>=clock_timestamp()-sqlc.arg(worker_freshness_seconds)::bigint*interval '1 second';

-- Actual source exclusion ends unfinished publication; it does not change a
-- ready or aborted decision. The latter already permits abandoned-object GC.
-- name: InvalidateReclaimedComputerCaptures :exec
UPDATE computer_checkpoints cp SET status='invalid',invalidated_at=clock_timestamp(),
 invalidation_reason_code='capture_source_reclaimed'
FROM computer_instances i WHERE i.id=sqlc.arg(instance_id) AND i.reclaimed_at IS NOT NULL
 AND cp.computer_id=i.computer_id AND cp.source_computer_instance_id=i.id
 AND cp.writer_generation=i.writer_generation AND cp.status='creating';
