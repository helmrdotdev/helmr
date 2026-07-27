-- name: CreateRunCheckpoint :one
INSERT INTO run_checkpoints (
    id,
    kind,
    run_id,
    attempt_number,
    run_wait_id,
    source_run_lease_id,
    source_workspace_lease_id,
    workspace_id,
    base_workspace_version_id,
    private_workspace_version_id,
    actor_speculative_input_sequence,
    state,
    restore_manifest,
    expires_at
)
VALUES (
    sqlc.arg(id),
    sqlc.arg(kind),
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
RETURNING *;

-- name: AddRunCheckpointArtifact :one
INSERT INTO run_checkpoint_artifacts (
    run_checkpoint_id,
    role,
    ordinal,
    artifact_id
)
VALUES (
    sqlc.arg(run_checkpoint_id),
    sqlc.arg(role),
    sqlc.arg(ordinal),
    sqlc.arg(artifact_id)
)
RETURNING *;

-- name: MarkRunCheckpointReady :one
UPDATE run_checkpoints
   SET state = 'ready',
       private_workspace_version_id = sqlc.arg(private_workspace_version_id),
       restore_manifest = sqlc.arg(restore_manifest),
       ready_request_fingerprint = sqlc.arg(ready_request_fingerprint),
       ready_at = now()
 WHERE run_id = sqlc.arg(run_id)
   AND attempt_number = sqlc.arg(attempt_number)
   AND id = sqlc.arg(id)
   AND state = 'creating'
RETURNING *;

-- name: LockCreatingRunCheckpoint :one
SELECT *
  FROM run_checkpoints
 WHERE id = sqlc.arg(id)
   AND kind = 'suspend'
   AND run_id = sqlc.arg(run_id)
   AND attempt_number = sqlc.arg(attempt_number)
   AND run_wait_id = sqlc.arg(run_wait_id)
   AND source_run_lease_id = sqlc.arg(source_run_lease_id)
   AND source_workspace_lease_id = sqlc.arg(source_workspace_lease_id)
   AND workspace_id = sqlc.arg(workspace_id)
   AND state = 'creating'
 FOR UPDATE;

-- name: GetCheckpointReadyReplay :one
SELECT run_id, attempt_number, run_wait_id, source_run_lease_id,
       workspace_id, private_workspace_version_id, ready_request_fingerprint
  FROM run_checkpoints
 WHERE id = sqlc.arg(id)
   AND kind = 'suspend'
   AND state = 'ready'
   AND ready_request_fingerprint IS NOT NULL;

-- name: GetCheckpointFailedReplay :one
SELECT run_id, attempt_number, run_wait_id, source_run_lease_id,
       workspace_id, failed_request_fingerprint
  FROM run_checkpoints
 WHERE id = sqlc.arg(id)
   AND kind = 'suspend'
   AND state = 'invalid'
   AND invalidation_reason_code = 'checkpoint_failed'
   AND failed_request_fingerprint IS NOT NULL;

-- name: GetRuntimeIdentityForCheckpoint :one
SELECT *
  FROM runtime_identities
 WHERE id = sqlc.arg(id);

-- name: GetRuntimeSubstrateForCheckpoint :one
SELECT sqlc.embed(runtime_substrates),
       artifacts.digest AS artifact_digest,
       artifacts.size_bytes AS artifact_size_bytes,
       artifacts.media_type AS artifact_media_type
  FROM runtime_substrates
  JOIN artifacts
    ON artifacts.org_id = runtime_substrates.org_id
   AND artifacts.project_id = runtime_substrates.project_id
   AND artifacts.environment_id = runtime_substrates.environment_id
   AND artifacts.id = runtime_substrates.artifact_id
 WHERE runtime_substrates.id = sqlc.arg(id);

-- name: CreatePrivateCheckpointWorkspaceVersion :one
INSERT INTO workspace_versions (
    id, public_id, environment_id, workspace_id,
    parent_version_id, artifact_id, artifact_kind, kind, content_digest,
    size_bytes, entry_count, state, source_workspace_lease_id,
    ownership_generation, writer_generation
) VALUES (
    sqlc.arg(id), sqlc.arg(public_id), sqlc.arg(environment_id),
    sqlc.arg(workspace_id), sqlc.arg(parent_version_id),
    sqlc.arg(artifact_id), 'workspace_version', 'user', sqlc.arg(content_digest),
    sqlc.arg(size_bytes), sqlc.arg(entry_count), 'private',
    sqlc.arg(source_workspace_lease_id), sqlc.arg(ownership_generation),
    sqlc.arg(writer_generation)
)
RETURNING *;

-- name: CheckpointRunLease :one
UPDATE run_leases
   SET state = 'checkpointed',
       checkpointed_at = sqlc.arg(checkpointed_at),
       terminal_at = sqlc.arg(checkpointed_at),
       terminal_reason_code = 'checkpointed',
       updated_at = sqlc.arg(checkpointed_at)
 WHERE id = sqlc.arg(id)
   AND run_id = sqlc.arg(run_id)
   AND workspace_id = sqlc.arg(workspace_id)
   AND attempt_number = sqlc.arg(attempt_number)
   AND lease_sequence = sqlc.arg(lease_sequence)
   AND state = 'checkpointing'
   AND expires_at > sqlc.arg(checkpointed_at)
RETURNING *;

-- name: ReleaseCheckpointWorkspaceLease :one
UPDATE workspace_leases
   SET state = 'released',
       released_at = sqlc.arg(checkpointed_at),
       terminal_at = sqlc.arg(checkpointed_at),
       updated_at = sqlc.arg(checkpointed_at)
 WHERE id = sqlc.arg(id)
   AND workspace_id = sqlc.arg(workspace_id)
   AND workspace_mount_id = sqlc.arg(workspace_mount_id)
   AND runtime_instance_id = sqlc.arg(runtime_instance_id)
   AND owner_run_lease_id = sqlc.arg(owner_run_lease_id)
   AND owner_process_id IS NULL
   AND base_version_id = sqlc.arg(base_version_id)
   AND ownership_generation = sqlc.arg(ownership_generation)
   AND writer_generation = sqlc.arg(writer_generation)
   AND mount_fencing_generation = sqlc.arg(mount_fencing_generation)
   AND state = 'active'
   AND expires_at > sqlc.arg(checkpointed_at)
RETURNING *;

-- name: CommitPendingCheckpointReady :one
WITH updated_run AS (
    UPDATE runs
       SET current_run_lease_id = NULL,
           state_version = runs.state_version + 1,
           updated_at = sqlc.arg(checkpointed_at)
     WHERE id = sqlc.arg(run_id)
       AND workspace_id = sqlc.arg(workspace_id)
       AND current_attempt_number = sqlc.arg(attempt_number)
       AND current_run_lease_id = sqlc.arg(run_lease_id)
       AND status = 'waiting'
       AND active_started_at IS NULL
       AND runs.state_version = sqlc.arg(expected_run_state_version)
    RETURNING runs.state_version
)
UPDATE run_waits
   SET suspension_state = 'parked',
       expected_run_state_version = updated_run.state_version,
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
   AND run_waits.suspension_state = 'checkpointing'
   AND run_waits.condition_state = 'pending'
   AND run_waits.checkpoint_request_version = sqlc.arg(checkpoint_request_version)
RETURNING run_waits.*;

-- name: CommitSameWorkspaceChildCheckpointReady :one
WITH selected_child AS MATERIALIZED (
    SELECT child.id
      FROM runs AS child
     WHERE child.environment_id = sqlc.arg(environment_id)
       AND child.id = sqlc.arg(child_run_id)
       AND child.parent_run_id = sqlc.arg(parent_run_id)
       AND child.parent_owns_lifecycle IS TRUE
       AND child.workspace_id = sqlc.arg(workspace_id)
       AND child.base_workspace_version_id =
           sqlc.arg(base_workspace_version_id)
       AND child.claim_id = sqlc.arg(child_claim_id)
       AND child.status = 'queued'
), updated_run AS (
    UPDATE runs
       SET current_run_lease_id = NULL,
           state_version = runs.state_version + 1,
           updated_at = sqlc.arg(checkpointed_at)
     WHERE id = sqlc.arg(parent_run_id)
       AND environment_id = sqlc.arg(environment_id)
       AND workspace_id = sqlc.arg(workspace_id)
       AND current_attempt_number = sqlc.arg(parent_attempt_number)
       AND current_run_lease_id = sqlc.arg(parent_run_lease_id)
       AND status = 'waiting'
       AND active_started_at IS NULL
       AND runs.state_version = sqlc.arg(expected_run_state_version)
       AND EXISTS (SELECT 1 FROM selected_child)
    RETURNING runs.state_version
)
UPDATE run_waits
   SET child_run_id = selected_child.id,
       suspension_state = 'parked',
       expected_run_state_version = updated_run.state_version,
       checkpoint_ack_version = sqlc.arg(checkpoint_request_version),
       prior_run_lease_id = current_run_lease_id,
       current_run_lease_id = NULL,
       base_workspace_version_id = sqlc.arg(base_workspace_version_id),
       base_workspace_content_digest =
           sqlc.arg(base_workspace_content_digest),
       handoff_runtime_instance_id = sqlc.arg(runtime_instance_id),
       handoff_workspace_mount_id = sqlc.arg(workspace_mount_id),
       handoff_mount_generation = sqlc.arg(mount_generation),
       ownership_generation = sqlc.arg(ownership_generation),
       parent_writer_generation = sqlc.arg(parent_writer_generation),
       updated_at = sqlc.arg(checkpointed_at)
  FROM updated_run, selected_child
 WHERE run_waits.id = sqlc.arg(run_wait_id)
   AND run_waits.environment_id = sqlc.arg(environment_id)
   AND run_waits.run_id = sqlc.arg(parent_run_id)
   AND run_waits.workspace_id = sqlc.arg(workspace_id)
   AND run_waits.attempt_number = sqlc.arg(parent_attempt_number)
   AND run_waits.kind = 'child'
   AND run_waits.child_run_id IS NULL
   AND run_waits.child_parent_owned IS TRUE
   AND run_waits.child_claim_id = sqlc.arg(child_claim_id)
   AND run_waits.current_run_lease_id = sqlc.arg(parent_run_lease_id)
   AND run_waits.suspend_checkpoint_id =
       sqlc.arg(suspend_checkpoint_id)
   AND run_waits.suspension_state = 'checkpointing'
   AND run_waits.condition_state = 'pending'
   AND run_waits.checkpoint_request_version =
       sqlc.arg(checkpoint_request_version)
RETURNING run_waits.*;

-- name: CommitTerminalCheckpointReady :one
WITH updated_run AS (
    UPDATE runs
       SET status = 'queued',
           current_run_lease_id = NULL,
           state_version = state_version + 1,
           queue_origin_at = sqlc.arg(checkpointed_at),
           queue_score_at = sqlc.arg(checkpointed_at),
           updated_at = sqlc.arg(checkpointed_at)
     WHERE id = sqlc.arg(run_id)
       AND workspace_id = sqlc.arg(workspace_id)
       AND current_attempt_number = sqlc.arg(attempt_number)
       AND current_run_lease_id = sqlc.arg(run_lease_id)
       AND status = 'waiting'
       AND active_started_at IS NULL
       AND state_version = sqlc.arg(expected_run_state_version)
    RETURNING state_version
)
UPDATE run_waits
   SET suspension_state = 'resume_pending',
       expected_run_state_version = updated_run.state_version,
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
   AND run_waits.suspension_state = 'checkpointing'
   AND run_waits.condition_state <> 'pending'
   AND run_waits.checkpoint_request_version = sqlc.arg(checkpoint_request_version)
RETURNING run_waits.*;

-- name: InvalidateFailedRunCheckpoint :one
UPDATE run_checkpoints
   SET state = 'invalid',
       invalidated_at = sqlc.arg(failed_at),
       invalidation_reason_code = 'checkpoint_failed',
       failed_request_fingerprint = sqlc.arg(failed_request_fingerprint)
 WHERE id = sqlc.arg(checkpoint_id)
   AND run_id = sqlc.arg(run_id)
   AND attempt_number = sqlc.arg(attempt_number)
   AND run_wait_id = sqlc.arg(run_wait_id)
   AND source_run_lease_id = sqlc.arg(run_lease_id)
   AND workspace_id = sqlc.arg(workspace_id)
   AND state = 'creating'
RETURNING *;

-- name: FailCheckpointRunLease :one
UPDATE run_leases
   SET state = 'failed',
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
   AND state = 'checkpointing'
   AND terminal_request_fingerprint IS NULL
   AND expires_at > sqlc.arg(failed_at)
RETURNING *;

-- name: FailCheckpointRunWait :one
UPDATE run_waits
   SET condition_state = CASE
           WHEN condition_state = 'pending' THEN 'cancelled'
           ELSE condition_state
       END,
       condition_terminal_at = CASE
           WHEN condition_state = 'pending' THEN sqlc.arg(failed_at)
           ELSE condition_terminal_at
       END,
       condition_reason_code = CASE
           WHEN condition_state = 'pending' THEN 'run_checkpoint_failed'
           ELSE condition_reason_code
       END,
       suspension_state = 'failed',
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
   AND suspension_state = 'checkpointing'
   AND checkpoint_request_version = sqlc.arg(checkpoint_request_version)
RETURNING *;

-- name: RequestCheckpointFailureRuntimeClose :one
WITH closing_runtime AS (
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
   SET state = 'unmounting',
       stopped_at = COALESCE(stopped_at, sqlc.arg(failed_at)),
       updated_at = sqlc.arg(failed_at)
  FROM closing_runtime
 WHERE workspace_mounts.id = sqlc.arg(workspace_mount_id)
   AND workspace_mounts.org_id = sqlc.arg(org_id)
   AND workspace_mounts.project_id = sqlc.arg(project_id)
   AND workspace_mounts.environment_id = sqlc.arg(environment_id)
   AND workspace_mounts.workspace_id = sqlc.arg(workspace_id)
   AND workspace_mounts.runtime_instance_id = closing_runtime.id
   AND workspace_mounts.worker_instance_id = sqlc.arg(worker_instance_id)
   AND workspace_mounts.worker_epoch = sqlc.arg(worker_epoch)
   AND workspace_mounts.fencing_generation = sqlc.arg(mount_fencing_generation)
   AND workspace_mounts.state = 'mounted'
RETURNING workspace_mounts.*;

-- name: RequestHandoffFailureRuntimeClose :one
WITH closing_runtime AS (
    UPDATE runtime_instances
       SET desired_state = 'closed',
           desired_version = desired_version + 1,
           desired_at = sqlc.arg(failed_at),
           desired_reason = 'child_handoff_failed',
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
   SET state = 'unmounting',
       stopped_at = COALESCE(stopped_at, sqlc.arg(failed_at)),
       updated_at = sqlc.arg(failed_at)
  FROM closing_runtime
 WHERE workspace_mounts.id = sqlc.arg(workspace_mount_id)
   AND workspace_mounts.org_id = sqlc.arg(org_id)
   AND workspace_mounts.project_id = sqlc.arg(project_id)
   AND workspace_mounts.environment_id = sqlc.arg(environment_id)
   AND workspace_mounts.workspace_id = sqlc.arg(workspace_id)
   AND workspace_mounts.runtime_instance_id = closing_runtime.id
   AND workspace_mounts.worker_instance_id = sqlc.arg(worker_instance_id)
   AND workspace_mounts.worker_epoch = sqlc.arg(worker_epoch)
   AND workspace_mounts.fencing_generation = sqlc.arg(mount_fencing_generation)
   AND workspace_mounts.state = 'mounted'
RETURNING workspace_mounts.*;

-- name: InvalidateRunCheckpoint :one
UPDATE run_checkpoints
   SET state = 'invalid',
       invalidated_at = now(),
       invalidation_reason_code = sqlc.arg(invalidation_reason_code)
 WHERE run_id = sqlc.arg(run_id)
   AND attempt_number = sqlc.arg(attempt_number)
   AND id = sqlc.arg(id)
   AND state IN ('creating', 'ready')
RETURNING *;

-- name: GetReadyRunCheckpoint :one
SELECT run_checkpoints.*
  FROM run_checkpoints
  JOIN run_waits
    ON run_waits.run_id = run_checkpoints.run_id
   AND run_waits.attempt_number = run_checkpoints.attempt_number
   AND run_waits.workspace_id = run_checkpoints.workspace_id
   AND run_waits.id = run_checkpoints.run_wait_id
 WHERE run_checkpoints.run_id = sqlc.arg(run_id)
   AND run_checkpoints.attempt_number = sqlc.arg(attempt_number)
   AND run_checkpoints.id = sqlc.arg(id)
   AND run_checkpoints.state = 'ready';

-- name: LockRestorableRunCheckpoint :one
SELECT *
  FROM run_checkpoints
 WHERE id = sqlc.arg(id)
   AND kind = 'suspend'
   AND run_id = sqlc.arg(run_id)
   AND attempt_number = sqlc.arg(attempt_number)
   AND run_wait_id = sqlc.arg(run_wait_id)
   AND workspace_id = sqlc.arg(workspace_id)
   AND state = 'ready'
   AND (expires_at IS NULL OR expires_at > transaction_timestamp())
 FOR UPDATE;

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

-- name: ListRunCheckpointArtifacts :many
SELECT *
  FROM run_checkpoint_artifacts
 WHERE run_checkpoint_id = sqlc.arg(run_checkpoint_id)
 ORDER BY role, ordinal;

-- name: ListRunCheckpointArtifactAuthority :many
SELECT members.role,
       members.ordinal,
       artifacts.digest,
       artifacts.size_bytes,
       artifacts.media_type
  FROM run_checkpoint_artifacts AS members
  JOIN run_checkpoints
    ON run_checkpoints.id = members.run_checkpoint_id
  JOIN runs
    ON runs.id = run_checkpoints.run_id
  JOIN artifacts
    ON artifacts.environment_id = runs.environment_id
   AND artifacts.id = members.artifact_id
 WHERE members.run_checkpoint_id = sqlc.arg(run_checkpoint_id)
 ORDER BY members.role, members.ordinal;
