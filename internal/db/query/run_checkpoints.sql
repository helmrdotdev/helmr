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
    sqlc.arg(private_workspace_version_id),
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
       restore_manifest = sqlc.arg(restore_manifest),
       ready_at = now()
 WHERE run_id = sqlc.arg(run_id)
   AND attempt_number = sqlc.arg(attempt_number)
   AND id = sqlc.arg(id)
   AND state = 'creating'
RETURNING *;

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

-- name: ListRunCheckpointArtifacts :many
SELECT *
  FROM run_checkpoint_artifacts
 WHERE run_checkpoint_id = sqlc.arg(run_checkpoint_id)
 ORDER BY role, ordinal;
