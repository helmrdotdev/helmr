-- name: CreateRunCheckpoint :one
INSERT INTO run_checkpoints (
    id,
    run_id,
    attempt_number,
    run_wait_id,
    source_run_lease_id,
    source_workspace_lease_id,
    workspace_id,
    base_workspace_version_id,
    private_workspace_version_id,
    actor_speculative_input_sequence,
    status,
    restore_manifest,
    expires_at
)
VALUES (
    sqlc.arg(id),
    sqlc.arg(run_id),
    sqlc.arg(attempt_number),
    sqlc.arg(run_wait_id),
    sqlc.arg(source_run_lease_id),
    sqlc.arg(source_workspace_lease_id),
    sqlc.arg(workspace_id),
    sqlc.arg(base_workspace_version_id),
    sqlc.narg(private_workspace_version_id),
    sqlc.narg(actor_speculative_input_sequence),
    'creating',
    sqlc.arg(restore_manifest),
    sqlc.narg(expires_at)
)
RETURNING run_checkpoints.*;

-- name: MarkRunCheckpointReady :one
UPDATE run_checkpoints
   SET status = 'ready',
       private_workspace_version_id = sqlc.arg(private_workspace_version_id),
       runtime_config_artifact_id = sqlc.arg(runtime_config_artifact_id),
       vm_state_artifact_id = sqlc.arg(vm_state_artifact_id),
       memory_artifact_id = sqlc.arg(memory_artifact_id),
       scratch_disk_artifact_id = sqlc.arg(scratch_disk_artifact_id),
       restore_manifest = sqlc.arg(restore_manifest),
       ready_request_fingerprint = sqlc.arg(ready_request_fingerprint),
       ready_at = now()
  FROM runs,
       artifacts AS runtime_config_artifact,
       artifacts AS vm_state_artifact,
       artifacts AS memory_artifact,
       artifacts AS scratch_disk_artifact
 WHERE run_checkpoints.run_id = sqlc.arg(run_id)
   AND run_checkpoints.attempt_number = sqlc.arg(attempt_number)
   AND run_checkpoints.id = sqlc.arg(id)
   AND run_checkpoints.status = 'creating'
   AND runs.id = run_checkpoints.run_id
   AND runtime_config_artifact.id = sqlc.arg(runtime_config_artifact_id)
   AND runtime_config_artifact.environment_id = runs.environment_id
   AND runtime_config_artifact.kind = 'run_checkpoint_config'
   AND vm_state_artifact.id = sqlc.arg(vm_state_artifact_id)
   AND vm_state_artifact.environment_id = runs.environment_id
   AND vm_state_artifact.kind = 'run_checkpoint_vm_state'
   AND memory_artifact.id = sqlc.arg(memory_artifact_id)
   AND memory_artifact.environment_id = runs.environment_id
   AND memory_artifact.kind = 'run_checkpoint_memory'
   AND scratch_disk_artifact.id = sqlc.arg(scratch_disk_artifact_id)
   AND scratch_disk_artifact.environment_id = runs.environment_id
   AND scratch_disk_artifact.kind = 'run_checkpoint_scratch_disk'
RETURNING run_checkpoints.*;

-- name: LockCreatingRunCheckpoint :one
SELECT *
  FROM run_checkpoints
 WHERE id = sqlc.arg(id)
   AND run_id = sqlc.arg(run_id)
   AND attempt_number = sqlc.arg(attempt_number)
   AND run_wait_id = sqlc.arg(run_wait_id)
   AND source_run_lease_id = sqlc.arg(source_run_lease_id)
   AND source_workspace_lease_id = sqlc.arg(source_workspace_lease_id)
   AND workspace_id = sqlc.arg(workspace_id)
   AND status = 'creating'
 FOR UPDATE;

-- name: GetCheckpointReadyReplay :one
SELECT run_id, attempt_number, run_wait_id, source_run_lease_id,
       workspace_id, private_workspace_version_id, ready_request_fingerprint
  FROM run_checkpoints
 WHERE id = sqlc.arg(id)
   AND status = 'ready'
   AND ready_request_fingerprint IS NOT NULL;

-- name: GetCheckpointFailedReplay :one
SELECT run_id, attempt_number, run_wait_id, source_run_lease_id,
       workspace_id, failed_request_fingerprint
  FROM run_checkpoints
 WHERE id = sqlc.arg(id)
   AND status = 'invalid'
   AND invalidation_reason_code = 'checkpoint_failed'
   AND failed_request_fingerprint IS NOT NULL;

-- name: GetRuntimeIdentityForCheckpoint :one
SELECT *
  FROM runtime_identities
 WHERE id = sqlc.arg(id);

-- name: GetRuntimeSubstrateForCheckpoint :one
SELECT *
  FROM runtime_substrates
 WHERE id = sqlc.arg(id);

-- name: CreatePrivateCheckpointWorkspaceVersion :one
INSERT INTO workspace_versions (
    id, environment_id, workspace_id,
    parent_version_id, artifact_id, content_digest,
    size_bytes, entry_count, status, source_workspace_lease_id,
    ownership_generation, writer_generation
)
SELECT
    sqlc.arg(id), sqlc.arg(environment_id),
    sqlc.arg(workspace_id), sqlc.arg(parent_version_id),
    sqlc.arg(artifact_id), sqlc.arg(content_digest),
    sqlc.arg(size_bytes), sqlc.arg(entry_count), 'private',
    sqlc.arg(source_workspace_lease_id), sqlc.arg(ownership_generation),
    sqlc.arg(writer_generation)
  FROM artifacts
 WHERE artifacts.environment_id = sqlc.arg(environment_id)
   AND artifacts.id = sqlc.arg(artifact_id)
   AND artifacts.kind = 'workspace_version'
RETURNING *;

-- name: CheckpointRunLease :one
UPDATE run_leases
   SET status = 'checkpointed',
       checkpointed_at = sqlc.arg(checkpointed_at),
       terminal_at = sqlc.arg(checkpointed_at),
       terminal_reason_code = 'checkpointed',
       updated_at = sqlc.arg(checkpointed_at)
 WHERE id = sqlc.arg(id)
   AND run_id = sqlc.arg(run_id)
   AND workspace_id = sqlc.arg(workspace_id)
   AND attempt_number = sqlc.arg(attempt_number)
   AND lease_sequence = sqlc.arg(lease_sequence)
   AND status = 'checkpointing'
   AND expires_at > sqlc.arg(checkpointed_at)
RETURNING *;

-- name: ReleaseCheckpointWorkspaceLease :one
UPDATE workspace_leases
   SET status = 'released',
       released_at = sqlc.arg(checkpointed_at),
       terminal_at = sqlc.arg(checkpointed_at),
       updated_at = sqlc.arg(checkpointed_at)
 WHERE id = sqlc.arg(id)
   AND workspace_id = sqlc.arg(workspace_id)
   AND workspace_mount_id = sqlc.arg(workspace_mount_id)
   AND runtime_instance_id = sqlc.arg(runtime_instance_id)
   AND owner_run_lease_id = sqlc.arg(owner_run_lease_id)
   AND owner_process_id IS NULL
   AND base_workspace_version_id = sqlc.arg(base_workspace_version_id)
   AND ownership_generation = sqlc.arg(ownership_generation)
   AND writer_generation = sqlc.arg(writer_generation)
   AND mount_fencing_generation = sqlc.arg(mount_fencing_generation)
   AND status = 'active'
   AND expires_at > sqlc.arg(checkpointed_at)
RETURNING *;

-- name: CloseCheckpointSourceRuntime :one
WITH closed_mount AS (
    UPDATE workspace_mounts
       SET status = 'unmounted',
           stopped_at = COALESCE(stopped_at, sqlc.arg(checkpointed_at)),
           unmounted_at = sqlc.arg(checkpointed_at),
           terminal_at = sqlc.arg(checkpointed_at),
           terminal_reason_code = 'checkpointed',
           terminal_error = NULL,
           updated_at = sqlc.arg(checkpointed_at)
     WHERE workspace_mounts.id = sqlc.arg(workspace_mount_id)
       AND workspace_mounts.runtime_instance_id = sqlc.arg(runtime_instance_id)
       AND workspace_mounts.worker_instance_id = sqlc.arg(worker_instance_id)
       AND workspace_mounts.worker_epoch = sqlc.arg(worker_epoch)
       AND workspace_mounts.fencing_generation = sqlc.arg(mount_fencing_generation)
       AND workspace_mounts.status = 'mounted'
    RETURNING workspace_mounts.*
), closed_runtime AS (
    UPDATE runtime_instances
       SET desired_state = 'closed',
           desired_version = runtime_instances.desired_version + 1,
           desired_at = sqlc.arg(checkpointed_at),
           desired_reason = 'checkpointed',
           reserved_run_id = NULL,
           reserved_attempt_number = NULL,
           reserved_process_id = NULL,
           reserved_workspace_version_id = NULL,
           reservation_expires_at = NULL,
           updated_at = sqlc.arg(checkpointed_at)
      FROM closed_mount
     WHERE runtime_instances.id = sqlc.arg(runtime_instance_id)
       AND runtime_instances.id = closed_mount.runtime_instance_id
       AND runtime_instances.worker_instance_id = sqlc.arg(worker_instance_id)
       AND runtime_instances.worker_epoch = sqlc.arg(worker_epoch)
       AND runtime_instances.desired_state = 'ready'
       AND runtime_instances.desired_version = sqlc.arg(expected_desired_version)
       AND runtime_instances.observed_state = 'ready'
       AND runtime_instances.observed_version = sqlc.arg(expected_observed_version)
    RETURNING runtime_instances.id
)
SELECT closed_mount.*
  FROM closed_mount
  JOIN closed_runtime ON closed_runtime.id = closed_mount.runtime_instance_id;

-- name: CommitPendingCheckpointReady :one
WITH updated_run AS (
    UPDATE runs
       SET current_run_lease_id = NULL,
           revision = runs.revision + 1,
           updated_at = sqlc.arg(checkpointed_at)
     WHERE id = sqlc.arg(run_id)
       AND workspace_id = sqlc.arg(workspace_id)
       AND current_attempt_number = sqlc.arg(attempt_number)
       AND current_run_lease_id = sqlc.arg(run_lease_id)
       AND status = 'waiting'
       AND active_started_at IS NULL
       AND runs.revision = sqlc.arg(expected_run_revision)
    RETURNING runs.revision
)
UPDATE run_waits
   SET suspension_status = 'parked',
       expected_run_revision = updated_run.revision,
       checkpoint_ack_version = sqlc.arg(checkpoint_request_version),
       prior_run_lease_id = current_run_lease_id,
       current_run_lease_id = NULL,
       updated_at = sqlc.arg(checkpointed_at)
  FROM updated_run
 WHERE run_waits.id = sqlc.arg(run_wait_id)
   AND run_waits.run_id = sqlc.arg(run_id)
   AND run_waits.workspace_id = sqlc.arg(workspace_id)
   AND run_waits.attempt_number = sqlc.arg(attempt_number)
   AND run_waits.current_run_lease_id = sqlc.arg(run_lease_id)
   AND run_waits.suspend_checkpoint_id = sqlc.arg(checkpoint_id)
   AND run_waits.suspension_status = 'checkpointing'
   AND run_waits.condition_status = 'pending'
   AND run_waits.checkpoint_request_version = sqlc.arg(checkpoint_request_version)
RETURNING run_waits.*;

-- name: CommitSameWorkspaceChildCheckpointReady :one
WITH locked_parent AS MATERIALIZED (
    SELECT runs.id AS run_id
      FROM runs
     WHERE runs.id = sqlc.arg(parent_run_id)
       AND runs.environment_id = sqlc.arg(environment_id)
       AND runs.workspace_id = sqlc.arg(workspace_id)
       AND runs.current_attempt_number = sqlc.arg(parent_attempt_number)
       AND runs.current_run_lease_id = sqlc.arg(parent_run_lease_id)
       AND runs.status = 'waiting'
       AND runs.active_started_at IS NULL
       AND runs.revision = sqlc.arg(expected_run_revision)
     FOR UPDATE OF runs
), locked_wait AS MATERIALIZED (
    SELECT run_waits.id, run_waits.run_id
      FROM locked_parent
      JOIN run_waits ON run_waits.run_id = locked_parent.run_id
     WHERE run_waits.id = sqlc.arg(run_wait_id)
       AND run_waits.environment_id = sqlc.arg(environment_id)
       AND run_waits.run_id = sqlc.arg(parent_run_id)
       AND run_waits.workspace_id = sqlc.arg(workspace_id)
       AND run_waits.attempt_number = sqlc.arg(parent_attempt_number)
       AND run_waits.kind = 'child'
       AND run_waits.child_run_id IS NULL
       AND run_waits.child_claim_id = sqlc.arg(child_claim_id)
       AND run_waits.current_run_lease_id = sqlc.arg(parent_run_lease_id)
       AND run_waits.suspend_checkpoint_id =
           sqlc.arg(suspend_checkpoint_id)
       AND run_waits.suspension_status = 'checkpointing'
       AND run_waits.condition_status = 'pending'
       AND run_waits.expected_run_revision = sqlc.arg(expected_run_revision)
       AND run_waits.checkpoint_request_version =
           sqlc.arg(checkpoint_request_version)
     FOR UPDATE OF run_waits
), selected_child AS MATERIALIZED (
    SELECT child.id, locked_wait.id AS wait_id, locked_wait.run_id AS parent_id
      FROM locked_wait
      JOIN runs AS child ON child.parent_run_id = locked_wait.run_id
     WHERE child.environment_id = sqlc.arg(environment_id)
       AND child.id = sqlc.arg(child_run_id)
       AND child.parent_run_id = sqlc.arg(parent_run_id)
       AND child.parent_owns_lifecycle IS TRUE
       AND child.workspace_id = sqlc.arg(workspace_id)
       AND child.base_workspace_version_id =
           sqlc.arg(base_workspace_version_id)
       AND child.claim_id = sqlc.arg(child_claim_id)
       AND child.status = 'queued'
     FOR UPDATE OF child
), updated_run AS (
    UPDATE runs
       SET current_run_lease_id = NULL,
           revision = runs.revision + 1,
           updated_at = sqlc.arg(checkpointed_at)
      FROM selected_child
     WHERE runs.id = sqlc.arg(parent_run_id)
       AND runs.environment_id = sqlc.arg(environment_id)
       AND runs.workspace_id = sqlc.arg(workspace_id)
       AND runs.current_attempt_number = sqlc.arg(parent_attempt_number)
       AND runs.current_run_lease_id = sqlc.arg(parent_run_lease_id)
       AND runs.status = 'waiting'
       AND runs.active_started_at IS NULL
       AND runs.revision = sqlc.arg(expected_run_revision)
       AND runs.id = selected_child.parent_id
    RETURNING runs.revision
)
UPDATE run_waits
   SET child_run_id = selected_child.id,
       suspension_status = 'parked',
       expected_run_revision = updated_run.revision,
       checkpoint_ack_version = sqlc.arg(checkpoint_request_version),
       prior_run_lease_id = current_run_lease_id,
       current_run_lease_id = NULL,
       base_workspace_version_id = sqlc.arg(base_workspace_version_id),
       base_workspace_content_digest =
           sqlc.arg(base_workspace_content_digest),
       ownership_generation = sqlc.arg(ownership_generation),
       parent_writer_generation = sqlc.arg(parent_writer_generation),
       updated_at = sqlc.arg(checkpointed_at)
  FROM updated_run, selected_child
 WHERE run_waits.id = selected_child.wait_id
RETURNING run_waits.*;

-- name: CommitTerminalCheckpointReady :one
WITH updated_run AS (
    UPDATE runs
       SET status = 'queued',
           current_run_lease_id = NULL,
           revision = revision + 1,
           queue_origin_at = sqlc.arg(checkpointed_at),
           queue_score_at = sqlc.arg(checkpointed_at),
           updated_at = sqlc.arg(checkpointed_at)
     WHERE id = sqlc.arg(run_id)
       AND workspace_id = sqlc.arg(workspace_id)
       AND current_attempt_number = sqlc.arg(attempt_number)
       AND current_run_lease_id = sqlc.arg(run_lease_id)
       AND status = 'waiting'
       AND active_started_at IS NULL
       AND revision = sqlc.arg(expected_run_revision)
    RETURNING revision
)
UPDATE run_waits
   SET suspension_status = 'resume_pending',
       expected_run_revision = updated_run.revision,
       checkpoint_ack_version = sqlc.arg(checkpoint_request_version),
       prior_run_lease_id = current_run_lease_id,
       current_run_lease_id = NULL,
       resume_request_version = resume_request_version + 1,
       updated_at = sqlc.arg(checkpointed_at)
  FROM updated_run
 WHERE run_waits.id = sqlc.arg(run_wait_id)
   AND run_waits.run_id = sqlc.arg(run_id)
   AND run_waits.workspace_id = sqlc.arg(workspace_id)
   AND run_waits.attempt_number = sqlc.arg(attempt_number)
   AND run_waits.current_run_lease_id = sqlc.arg(run_lease_id)
   AND run_waits.suspend_checkpoint_id = sqlc.arg(checkpoint_id)
   AND run_waits.suspension_status = 'checkpointing'
   AND run_waits.condition_status <> 'pending'
   AND run_waits.checkpoint_request_version = sqlc.arg(checkpoint_request_version)
RETURNING run_waits.*;

-- name: InvalidateFailedRunCheckpoint :one
UPDATE run_checkpoints
   SET status = 'invalid',
       invalidated_at = sqlc.arg(failed_at),
       invalidation_reason_code = 'checkpoint_failed',
       failed_request_fingerprint = sqlc.arg(failed_request_fingerprint)
 WHERE id = sqlc.arg(checkpoint_id)
   AND run_id = sqlc.arg(run_id)
   AND attempt_number = sqlc.arg(attempt_number)
   AND run_wait_id = sqlc.arg(run_wait_id)
   AND source_run_lease_id = sqlc.arg(run_lease_id)
   AND workspace_id = sqlc.arg(workspace_id)
   AND status = 'creating'
RETURNING *;

-- name: FailCheckpointRunLease :one
UPDATE run_leases
   SET status = 'failed',
       terminal_at = sqlc.arg(failed_at),
       terminal_reason_code = 'checkpoint_failed',
       terminal_error = sqlc.arg(error)::jsonb,
       terminal_request_fingerprint = sqlc.arg(failed_request_fingerprint),
       updated_at = sqlc.arg(failed_at)
 WHERE id = sqlc.arg(run_lease_id)
   AND run_id = sqlc.arg(run_id)
   AND workspace_id = sqlc.arg(workspace_id)
   AND attempt_number = sqlc.arg(attempt_number)
   AND lease_sequence = sqlc.arg(lease_sequence)
   AND status = 'checkpointing'
   AND terminal_request_fingerprint IS NULL
   AND expires_at > sqlc.arg(failed_at)
RETURNING *;

-- name: FailCheckpointRunWait :one
UPDATE run_waits
   SET condition_status = CASE
           WHEN condition_status = 'pending' THEN 'cancelled'
           ELSE condition_status
       END,
       condition_terminal_at = CASE
           WHEN condition_status = 'pending' THEN sqlc.arg(failed_at)
           ELSE condition_terminal_at
       END,
       condition_reason_code = CASE
           WHEN condition_status = 'pending' THEN 'run_checkpoint_failed'
           ELSE condition_reason_code
       END,
       suspension_status = 'failed',
       checkpoint_ack_version = sqlc.arg(checkpoint_request_version),
       prior_run_lease_id = current_run_lease_id,
       current_run_lease_id = NULL,
       suspension_terminal_at = sqlc.arg(failed_at),
       suspension_reason_code = 'checkpoint_failed',
       suspension_error = sqlc.arg(error)::jsonb,
       updated_at = sqlc.arg(failed_at)
 WHERE id = sqlc.arg(run_wait_id)
   AND run_id = sqlc.arg(run_id)
   AND workspace_id = sqlc.arg(workspace_id)
   AND attempt_number = sqlc.arg(attempt_number)
   AND current_run_lease_id = sqlc.arg(run_lease_id)
   AND suspend_checkpoint_id = sqlc.arg(checkpoint_id)
   AND suspension_status = 'checkpointing'
   AND checkpoint_request_version = sqlc.arg(checkpoint_request_version)
RETURNING *;

-- name: RequestCheckpointFailureRuntimeClose :one
WITH close_runtime AS (
    UPDATE runtime_instances
       SET desired_state = 'closed',
           desired_version = desired_version + 1,
           desired_at = sqlc.arg(failed_at),
           desired_reason = 'checkpoint_failed',
           updated_at = sqlc.arg(failed_at)
     WHERE runtime_instances.id = sqlc.arg(runtime_instance_id)
       AND runtime_instances.org_id = sqlc.arg(org_id)
       AND runtime_instances.project_id = sqlc.arg(project_id)
       AND runtime_instances.environment_id = sqlc.arg(environment_id)
       AND runtime_instances.workspace_id = sqlc.arg(workspace_id)
       AND runtime_instances.worker_instance_id = sqlc.arg(worker_instance_id)
       AND runtime_instances.worker_epoch = sqlc.arg(worker_epoch)
       AND runtime_instances.desired_state = 'ready'
       AND runtime_instances.observed_state = 'ready'
       AND runtime_instances.reclaimed_at IS NULL
    RETURNING id
)
UPDATE workspace_mounts
   SET status = 'unmounting',
       stopped_at = COALESCE(stopped_at, sqlc.arg(failed_at)),
       updated_at = sqlc.arg(failed_at)
  FROM close_runtime
 WHERE workspace_mounts.id = sqlc.arg(workspace_mount_id)
   AND workspace_mounts.org_id = sqlc.arg(org_id)
   AND workspace_mounts.project_id = sqlc.arg(project_id)
   AND workspace_mounts.environment_id = sqlc.arg(environment_id)
   AND workspace_mounts.workspace_id = sqlc.arg(workspace_id)
   AND workspace_mounts.runtime_instance_id = close_runtime.id
   AND workspace_mounts.worker_instance_id = sqlc.arg(worker_instance_id)
   AND workspace_mounts.worker_epoch = sqlc.arg(worker_epoch)
   AND workspace_mounts.fencing_generation = sqlc.arg(mount_fencing_generation)
   AND workspace_mounts.status = 'mounted'
RETURNING workspace_mounts.*;

-- name: GetReadyRunCheckpoint :one
SELECT sqlc.embed(run_checkpoints),
       runtime_config_artifact.digest AS runtime_config_digest,
       runtime_config_artifact.size_bytes AS runtime_config_size_bytes,
       runtime_config_artifact.media_type AS runtime_config_media_type,
       vm_state_artifact.digest AS vm_state_digest,
       vm_state_artifact.size_bytes AS vm_state_size_bytes,
       vm_state_artifact.media_type AS vm_state_media_type,
       memory_artifact.digest AS memory_digest,
       memory_artifact.size_bytes AS memory_size_bytes,
       memory_artifact.media_type AS memory_media_type,
       scratch_disk_artifact.digest AS scratch_disk_digest,
       scratch_disk_artifact.size_bytes AS scratch_disk_size_bytes,
       scratch_disk_artifact.media_type AS scratch_disk_media_type
  FROM run_checkpoints
  JOIN run_waits
    ON run_waits.run_id = run_checkpoints.run_id
   AND run_waits.attempt_number = run_checkpoints.attempt_number
   AND run_waits.workspace_id = run_checkpoints.workspace_id
   AND run_waits.id = run_checkpoints.run_wait_id
  JOIN runs
    ON runs.id = run_checkpoints.run_id
  JOIN artifacts AS runtime_config_artifact
    ON runtime_config_artifact.id = run_checkpoints.runtime_config_artifact_id
   AND runtime_config_artifact.environment_id = runs.environment_id
   AND runtime_config_artifact.kind = 'run_checkpoint_config'
  JOIN artifacts AS vm_state_artifact
    ON vm_state_artifact.id = run_checkpoints.vm_state_artifact_id
   AND vm_state_artifact.environment_id = runs.environment_id
   AND vm_state_artifact.kind = 'run_checkpoint_vm_state'
  JOIN artifacts AS memory_artifact
    ON memory_artifact.id = run_checkpoints.memory_artifact_id
   AND memory_artifact.environment_id = runs.environment_id
   AND memory_artifact.kind = 'run_checkpoint_memory'
  JOIN artifacts AS scratch_disk_artifact
    ON scratch_disk_artifact.id = run_checkpoints.scratch_disk_artifact_id
   AND scratch_disk_artifact.environment_id = runs.environment_id
   AND scratch_disk_artifact.kind = 'run_checkpoint_scratch_disk'
 WHERE run_checkpoints.run_id = sqlc.arg(run_id)
   AND run_checkpoints.attempt_number = sqlc.arg(attempt_number)
   AND run_checkpoints.id = sqlc.arg(id)
   AND run_checkpoints.status = 'ready';

-- name: LockRestorableRunCheckpoint :one
SELECT sqlc.embed(run_checkpoints),
       runtime_config_artifact.digest AS runtime_config_digest,
       runtime_config_artifact.size_bytes AS runtime_config_size_bytes,
       runtime_config_artifact.media_type AS runtime_config_media_type,
       vm_state_artifact.digest AS vm_state_digest,
       vm_state_artifact.size_bytes AS vm_state_size_bytes,
       vm_state_artifact.media_type AS vm_state_media_type,
       memory_artifact.digest AS memory_digest,
       memory_artifact.size_bytes AS memory_size_bytes,
       memory_artifact.media_type AS memory_media_type,
       scratch_disk_artifact.digest AS scratch_disk_digest,
       scratch_disk_artifact.size_bytes AS scratch_disk_size_bytes,
       scratch_disk_artifact.media_type AS scratch_disk_media_type
  FROM run_checkpoints
  JOIN runs
    ON runs.id = run_checkpoints.run_id
  JOIN artifacts AS runtime_config_artifact
    ON runtime_config_artifact.id = run_checkpoints.runtime_config_artifact_id
   AND runtime_config_artifact.environment_id = runs.environment_id
   AND runtime_config_artifact.kind = 'run_checkpoint_config'
  JOIN artifacts AS vm_state_artifact
    ON vm_state_artifact.id = run_checkpoints.vm_state_artifact_id
   AND vm_state_artifact.environment_id = runs.environment_id
   AND vm_state_artifact.kind = 'run_checkpoint_vm_state'
  JOIN artifacts AS memory_artifact
    ON memory_artifact.id = run_checkpoints.memory_artifact_id
   AND memory_artifact.environment_id = runs.environment_id
   AND memory_artifact.kind = 'run_checkpoint_memory'
  JOIN artifacts AS scratch_disk_artifact
    ON scratch_disk_artifact.id = run_checkpoints.scratch_disk_artifact_id
   AND scratch_disk_artifact.environment_id = runs.environment_id
   AND scratch_disk_artifact.kind = 'run_checkpoint_scratch_disk'
 WHERE run_checkpoints.id = sqlc.arg(id)
   AND run_checkpoints.run_id = sqlc.arg(run_id)
   AND run_checkpoints.attempt_number = sqlc.arg(attempt_number)
   AND run_checkpoints.run_wait_id = sqlc.arg(run_wait_id)
   AND run_checkpoints.workspace_id = sqlc.arg(workspace_id)
   AND run_checkpoints.status = 'ready'
   AND (run_checkpoints.expires_at IS NULL OR run_checkpoints.expires_at > transaction_timestamp())
 FOR UPDATE OF run_checkpoints;

-- name: GetRunCheckpointSource :one
SELECT sqlc.embed(run_leases),
       sqlc.embed(workspace_leases),
       sqlc.embed(runtime_instances)
  FROM run_leases
  JOIN workspace_leases
    ON workspace_leases.id = sqlc.arg(source_workspace_lease_id)
   AND workspace_leases.workspace_id = run_leases.workspace_id
   AND workspace_leases.owner_run_lease_id = run_leases.id
  JOIN runtime_instances
    ON runtime_instances.id = run_leases.runtime_instance_id
   AND runtime_instances.org_id = run_leases.org_id
   AND runtime_instances.project_id = run_leases.project_id
   AND runtime_instances.environment_id = run_leases.environment_id
   AND runtime_instances.workspace_id = run_leases.workspace_id
 WHERE run_leases.id = sqlc.arg(source_run_lease_id)
   AND run_leases.run_id = sqlc.arg(run_id)
   AND run_leases.attempt_number = sqlc.arg(attempt_number)
   AND run_leases.workspace_id = sqlc.arg(workspace_id);

-- name: ActorCheckpointLineageIsValid :one
-- Existing checkpoint and acknowledged handback receipts prove the private chain.
-- The source writer strictly decreases on every edge, so cycles cannot qualify.
-- Historical expiry is irrelevant after an acknowledged restore; callers retain
-- the latest candidate's expiry and live execution checks under owner locks.
WITH RECURSIVE proven AS NOT MATERIALIZED (
    SELECT c.id, c.base_workspace_version_id, c.private_workspace_version_id,
           source.writer_generation, runtime.restore_checkpoint_id,
           w.kind, w.child_run_id, w.condition_status, w.suspension_status,
           w.resume_request_version, w.resume_ack_version,
           w.base_workspace_version_id AS handoff_base_version_id,
           w.resume_workspace_version_id, w.ownership_generation AS handoff_ownership_generation,
           w.parent_writer_generation, w.child_writer_generation, w.resume_writer_generation
      FROM run_checkpoints c
      JOIN runs r ON r.id = c.run_id AND r.entrypoint_kind = 'actor'
      JOIN sessions s ON s.id = r.session_id AND s.current_run_id = r.id
      JOIN run_waits w ON w.id = c.run_wait_id AND w.run_id = c.run_id
       AND w.attempt_number = c.attempt_number AND w.workspace_id = c.workspace_id
       AND w.suspend_checkpoint_id = c.id AND w.prior_run_lease_id = c.source_run_lease_id
       AND w.checkpoint_request_version > 0 AND w.checkpoint_ack_version = w.checkpoint_request_version
       AND w.actor_speculative_input_sequence = c.actor_speculative_input_sequence
      JOIN workspace_versions v ON v.id = c.private_workspace_version_id
       AND v.workspace_id = c.workspace_id AND v.status = 'private'
       AND v.parent_version_id = c.base_workspace_version_id
      JOIN workspace_leases source ON source.id = c.source_workspace_lease_id
       AND source.id = v.source_workspace_lease_id AND source.workspace_id = c.workspace_id
       AND source.base_workspace_version_id = c.base_workspace_version_id
       AND source.ownership_generation = sqlc.arg(ownership_generation)::bigint
       AND v.ownership_generation = source.ownership_generation
       AND v.writer_generation = source.writer_generation
       AND source.status IN ('released', 'fenced') AND source.owner_process_id IS NULL
      JOIN run_leases lease ON lease.id = c.source_run_lease_id AND lease.id = source.owner_run_lease_id
       AND lease.run_id = c.run_id AND lease.attempt_number = c.attempt_number
       AND lease.workspace_id = c.workspace_id AND lease.status = 'checkpointed'
      JOIN runtime_instances runtime ON runtime.id = lease.runtime_instance_id
       AND runtime.workspace_id = c.workspace_id AND runtime.runtime_identity_id = lease.runtime_identity_id
       AND runtime.program_deployment_id = r.deployment_id
       AND runtime.desired_state = 'closed' AND runtime.observed_state = 'closed'
     WHERE c.run_id = sqlc.arg(run_id)::uuid AND c.attempt_number = sqlc.arg(attempt_number)::integer
       AND c.workspace_id = sqlc.arg(workspace_id)::uuid AND c.status = 'ready'
       AND c.actor_speculative_input_sequence IS NOT NULL
       AND (w.turn_id IS NULL OR (
           w.turn_session_id = s.id AND w.turn_run_generation = s.run_generation
           AND EXISTS (SELECT 1 FROM session_turns t WHERE t.id = w.turn_id
               AND t.session_id = s.id AND t.run_id = c.run_id
               AND t.attempt_number = c.attempt_number AND t.run_generation = w.turn_run_generation)
       ))
), lineage AS (
    SELECT p.id, p.base_workspace_version_id, p.writer_generation, p.restore_checkpoint_id
      FROM proven p WHERE p.id = sqlc.arg(checkpoint_id)::uuid
    UNION ALL
    SELECT prior.id, prior.base_workspace_version_id, prior.writer_generation, prior.restore_checkpoint_id
      FROM lineage current
      JOIN proven prior ON prior.id = current.restore_checkpoint_id
       AND prior.writer_generation < current.writer_generation
       AND prior.suspension_status = 'released'
       AND prior.resume_request_version > 0 AND prior.resume_ack_version = prior.resume_request_version
     WHERE current.base_workspace_version_id <> sqlc.arg(committed_head_version_id)::uuid
       AND (
           (prior.resume_workspace_version_id IS NULL
            AND prior.handoff_base_version_id IS NULL
            AND current.base_workspace_version_id = prior.private_workspace_version_id)
           OR (
               prior.kind = 'child'
               AND prior.handoff_base_version_id = prior.private_workspace_version_id
               AND prior.resume_workspace_version_id = current.base_workspace_version_id
               AND prior.handoff_ownership_generation = sqlc.arg(ownership_generation)::bigint
               AND prior.parent_writer_generation = prior.writer_generation
               AND prior.parent_writer_generation < prior.resume_writer_generation
               AND prior.resume_writer_generation = current.writer_generation
               AND EXISTS (
                   SELECT 1 FROM runs child
                   WHERE child.id = prior.child_run_id AND child.parent_run_id = sqlc.arg(run_id)::uuid
                     AND child.workspace_id = sqlc.arg(workspace_id)::uuid
                     AND child.parent_owns_lifecycle AND child.entrypoint_kind = 'task'
                     AND child.base_workspace_version_id = prior.private_workspace_version_id
                     AND child.current_run_lease_id IS NULL
                     AND (
                       (prior.condition_status = 'completed' AND child.status = 'succeeded'
                        AND EXISTS (
                          SELECT 1 FROM workspace_versions child_version
                          JOIN workspace_leases child_source ON child_source.id = child_version.source_workspace_lease_id
                           AND child_source.workspace_id = child_version.workspace_id
                           AND child_source.base_workspace_version_id = child_version.parent_version_id
                           AND child_source.ownership_generation = child_version.ownership_generation
                           AND child_source.writer_generation = child_version.writer_generation
                           AND child_source.status IN ('released', 'fenced') AND child_source.owner_process_id IS NULL
                          JOIN run_leases child_lease ON child_lease.id = child_source.owner_run_lease_id
                           AND child_lease.run_id = child.id AND child_lease.attempt_number = child.current_attempt_number
                           AND child_lease.workspace_id = child_version.workspace_id AND child_lease.status = 'completed'
                          JOIN runtime_instances child_runtime ON child_runtime.id = child_lease.runtime_instance_id
                           AND child_runtime.desired_state = 'closed' AND child_runtime.observed_state = 'closed'
                          WHERE child_version.id = current.base_workspace_version_id
                            AND child_version.workspace_id = sqlc.arg(workspace_id)::uuid AND child_version.status = 'private'
                            AND child_version.ownership_generation = sqlc.arg(ownership_generation)::bigint
                            AND child_version.writer_generation = prior.child_writer_generation
                            AND prior.parent_writer_generation < prior.child_writer_generation
                            AND prior.child_writer_generation < prior.resume_writer_generation
                        ))
                       OR (((prior.condition_status = 'cancelled' AND child.status = 'cancelled')
                            OR (prior.condition_status = 'failed' AND child.status IN ('failed', 'expired', 'system_failed')))
                           AND current.base_workspace_version_id = prior.private_workspace_version_id
                           AND (prior.child_writer_generation IS NULL
                                OR (prior.parent_writer_generation < prior.child_writer_generation
                                    AND prior.child_writer_generation < prior.resume_writer_generation))
                           AND EXISTS (
                               SELECT 1 FROM run_attempts terminal_attempt
                               WHERE terminal_attempt.run_id = child.id
                                 AND terminal_attempt.number = child.current_attempt_number
                                 AND terminal_attempt.workspace_id = child.workspace_id
                                 AND terminal_attempt.base_workspace_version_id = child.base_workspace_version_id
                                 AND terminal_attempt.terminal_at IS NOT NULL
                                 AND ((child.status = 'cancelled' AND terminal_attempt.terminal_outcome = 'cancelled')
                                      OR (child.status IN ('failed', 'expired', 'system_failed')
                                          AND terminal_attempt.terminal_outcome = 'failed'))
                           ))
                     )
               )
           )
       )
)
SELECT EXISTS (
    SELECT 1 FROM lineage JOIN workspace_versions head ON head.id = lineage.base_workspace_version_id
     WHERE head.id = sqlc.arg(committed_head_version_id)::uuid
       AND head.workspace_id = sqlc.arg(workspace_id)::uuid AND head.status = 'committed'
);

-- name: SameWorkspaceChildHasNoExecution :one
-- Called under the parent Run/Workspace authority locks; terminal child state
-- prevents a later lease admission. NULL writer alone is not an exclusion proof.
SELECT EXISTS (
    SELECT 1 FROM runs child
    WHERE child.id = sqlc.arg(child_run_id) AND child.parent_run_id = sqlc.arg(parent_run_id)
      AND child.workspace_id = sqlc.arg(workspace_id)
      AND child.base_workspace_version_id = sqlc.arg(base_workspace_version_id)
      AND child.entrypoint_kind = 'task' AND child.parent_owns_lifecycle
      AND child.status IN ('failed', 'cancelled', 'expired', 'system_failed') AND child.current_run_lease_id IS NULL
      AND NOT EXISTS (SELECT 1 FROM run_leases lease WHERE lease.run_id = child.id)
      AND NOT EXISTS (
          SELECT 1 FROM runtime_instances runtime WHERE runtime.reserved_run_id = child.id
          AND (runtime.desired_state <> 'closed' OR runtime.observed_state <> 'closed'
               OR EXISTS (SELECT 1 FROM workspace_mounts mount WHERE mount.runtime_instance_id = runtime.id
                   AND mount.status IN ('mounting', 'mounted', 'unmounting')))
      )
);
