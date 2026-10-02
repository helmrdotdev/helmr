-- Invalidation ends restore eligibility. Preserve the manifest, lineage and
-- publication receipt while releasing the four storage-owning artifact refs.
-- Ready checkpoints, including committed restores, keep their artifact refs.
-- name: ReleaseInvalidCheckpointArtifacts :execrows
WITH released AS (
 SELECT id FROM computer_checkpoints
 WHERE status IN ('invalid','deleted','aborted') AND vm_config_artifact_id IS NOT NULL
 ORDER BY id LIMIT sqlc.arg(row_limit)
 FOR UPDATE SKIP LOCKED
)
UPDATE computer_checkpoints cp
 SET vm_config_artifact_id=NULL,vm_state_artifact_id=NULL,
     memory_artifact_id=NULL,scratch_disk_artifact_id=NULL
 FROM released WHERE cp.id=released.id;

-- Release commits independently of artifact cleanup. Discovery retries cleanup
-- after interruption; the deletion statement and FKs arbitrate concurrent owners.
-- name: ListUnreferencedCheckpointArtifacts :many
SELECT a.id,a.org_id,a.digest FROM artifacts a
 WHERE a.kind IN ('computer_checkpoint_vm_config','computer_checkpoint_vm_state',
                  'computer_checkpoint_memory','computer_checkpoint_scratch_disk')
 AND NOT EXISTS (SELECT 1 FROM computer_checkpoints cp WHERE cp.vm_config_artifact_id=a.id)
 AND NOT EXISTS (SELECT 1 FROM computer_checkpoints cp WHERE cp.vm_state_artifact_id=a.id)
 AND NOT EXISTS (SELECT 1 FROM computer_checkpoints cp WHERE cp.memory_artifact_id=a.id)
 AND NOT EXISTS (SELECT 1 FROM computer_checkpoints cp WHERE cp.scratch_disk_artifact_id=a.id)
 ORDER BY a.digest,a.id LIMIT sqlc.arg(row_limit);

-- name: DeleteUnreferencedCheckpointArtifact :execrows
DELETE FROM artifacts a WHERE a.id=sqlc.arg(id)
 AND a.kind IN ('computer_checkpoint_vm_config','computer_checkpoint_vm_state',
                'computer_checkpoint_memory','computer_checkpoint_scratch_disk')
 AND NOT EXISTS (SELECT 1 FROM computer_checkpoints cp WHERE cp.vm_config_artifact_id=a.id)
 AND NOT EXISTS (SELECT 1 FROM computer_checkpoints cp WHERE cp.vm_state_artifact_id=a.id)
 AND NOT EXISTS (SELECT 1 FROM computer_checkpoints cp WHERE cp.memory_artifact_id=a.id)
 AND NOT EXISTS (SELECT 1 FROM computer_checkpoints cp WHERE cp.scratch_disk_artifact_id=a.id);
