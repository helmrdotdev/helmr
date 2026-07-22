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
