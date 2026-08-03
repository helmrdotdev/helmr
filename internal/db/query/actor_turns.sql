-- name: AdvanceActorTurnWorkspaceLeaseFrontier :one
UPDATE workspace_leases
   SET base_version_id = sqlc.arg(new_version_id),
       updated_at = sqlc.arg(committed_at)
 WHERE id = sqlc.arg(id)
   AND org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND workspace_id = sqlc.arg(workspace_id)
   AND workspace_mount_id = sqlc.arg(workspace_mount_id)
   AND runtime_instance_id = sqlc.arg(runtime_instance_id)
   AND owner_run_lease_id = sqlc.arg(owner_run_lease_id)
   AND owner_process_id IS NULL
   AND base_version_id = sqlc.arg(expected_version_id)
   AND ownership_generation = sqlc.arg(ownership_generation)
   AND writer_generation = sqlc.arg(writer_generation)
   AND mount_fencing_generation = sqlc.arg(mount_fencing_generation)
   AND state = 'active'
   AND expires_at > sqlc.arg(committed_at)
RETURNING *;

-- name: PublishRestoredActorCheckpointWorkspaceVersion :one
UPDATE workspace_versions
   SET state = 'committed',
       published_at = sqlc.arg(committed_at)
 WHERE workspace_versions.id = sqlc.arg(version_id)
   AND workspace_versions.workspace_id = sqlc.arg(workspace_id)
   AND workspace_versions.parent_version_id = sqlc.arg(expected_parent_version_id)
   AND workspace_versions.state = 'private'
   AND workspace_versions.ownership_generation = sqlc.arg(ownership_generation)
   AND workspace_versions.writer_generation = sqlc.arg(writer_generation)
   AND EXISTS (
       SELECT 1
         FROM run_checkpoints
        WHERE run_checkpoints.id = sqlc.arg(restore_checkpoint_id)
          AND run_checkpoints.run_id = sqlc.arg(run_id)
          AND run_checkpoints.attempt_number = sqlc.arg(attempt_number)
          AND run_checkpoints.workspace_id = workspace_versions.workspace_id
          AND run_checkpoints.private_workspace_version_id = workspace_versions.id
          AND run_checkpoints.actor_speculative_input_sequence IS NOT NULL
          AND run_checkpoints.state = 'invalid'
          AND run_checkpoints.invalidation_reason_code = 'actor_turn_committed'
   )
RETURNING workspace_versions.*;

-- name: InvalidateRestoredActorCheckpoint :one
UPDATE run_checkpoints
   SET state = 'invalid',
       invalidated_at = sqlc.arg(committed_at),
       invalidation_reason_code = 'actor_turn_committed'
 WHERE id = sqlc.arg(restore_checkpoint_id)
   AND run_id = sqlc.arg(run_id)
   AND attempt_number = sqlc.arg(attempt_number)
   AND workspace_id = sqlc.arg(workspace_id)
   AND private_workspace_version_id = sqlc.arg(private_workspace_version_id)
   AND actor_speculative_input_sequence BETWEEN sqlc.arg(target_input_sequence)::bigint - 1
                                            AND sqlc.arg(target_input_sequence)::bigint
   AND state = 'ready'
RETURNING run_checkpoints.*;

-- name: AdvanceActorTurnCursor :one
UPDATE actors
   SET committed_input_sequence = sqlc.arg(target_input_sequence),
       state_version = state_version + 1,
       updated_at = sqlc.arg(committed_at)
 WHERE environment_id = sqlc.arg(environment_id)
   AND id = sqlc.arg(actor_id)
   AND workspace_id = sqlc.arg(workspace_id)
   AND current_run_id = sqlc.arg(run_id)
   AND run_generation = sqlc.arg(expected_run_generation)
   AND state IN ('open', 'closing')
   AND committed_input_sequence = sqlc.arg(expected_input_sequence)
   AND sqlc.arg(target_input_sequence) = sqlc.arg(expected_input_sequence) + 1
   AND sqlc.arg(target_input_sequence) < next_input_sequence
RETURNING *;
