-- The publication owner holds secret, Computer and execution locks and validates
-- live Run/process deadlines before calling these state transitions. These queries
-- do not authenticate Workers or authorize publication of bytes.
-- name: BeginRuntimeComputerSave :one
UPDATE runtime_instances r
   SET computer_save_sequence=sqlc.arg(sequence),
       computer_save_version_id=sqlc.arg(save_id),
       computer_save_lease_id=sqlc.arg(lease_id),
       computer_save_base_version_id=sqlc.arg(predecessor_id),
       updated_at=clock_timestamp()
  FROM computers c, workspace_leases l, computer_versions v
 WHERE r.id=sqlc.arg(runtime_instance_id)
   AND r.worker_instance_id=sqlc.arg(worker_instance_id) AND r.worker_epoch=sqlc.arg(worker_epoch)
   AND r.desired_version=sqlc.arg(desired_version) AND r.desired_state='ready'
   AND r.observed_state='ready' AND r.reclaimed_at IS NULL
   AND c.id=r.workspace_id AND c.environment_id=r.environment_id
   AND c.status='active'
   AND v.id=c.head_version_id AND v.computer_id=c.id AND v.environment_id=c.environment_id
   AND v.status='committed'
   AND l.id=sqlc.arg(lease_id) AND l.workspace_id=c.id AND l.runtime_instance_id=r.id
   AND l.worker_instance_id=r.worker_instance_id AND l.worker_epoch=r.worker_epoch
   AND l.ownership_generation=c.ownership_generation AND l.writer_generation=c.writer_generation
   AND l.status='active' AND l.expires_at>clock_timestamp()
   AND (
       (r.computer_save_version_id IS NULL AND r.computer_save_sequence=sqlc.arg(sequence)::bigint-1
           AND c.head_version_id=sqlc.arg(predecessor_id)
           AND NOT EXISTS(SELECT 1 FROM computer_versions existing WHERE existing.id=sqlc.arg(save_id)))
       OR (r.computer_save_sequence=sqlc.arg(sequence) AND r.computer_save_version_id=sqlc.arg(save_id)
           AND r.computer_save_lease_id=sqlc.arg(lease_id)
           AND r.computer_save_base_version_id=sqlc.arg(predecessor_id))
   )
RETURNING r.computer_save_sequence, r.computer_save_version_id, r.computer_save_lease_id, r.computer_save_base_version_id;

-- Abandon only an unpublished operation after its owner has joined producers. Keep the sequence watermark
-- so a delayed admission cannot resurrect a cleared operation.
-- name: AbandonRuntimeComputerSave :execrows
UPDATE runtime_instances r
   SET computer_save_version_id=NULL, computer_save_lease_id=NULL,
       computer_save_base_version_id=NULL, updated_at=clock_timestamp()
 WHERE r.id=sqlc.arg(runtime_instance_id)
   AND r.worker_instance_id=sqlc.arg(worker_instance_id) AND r.worker_epoch=sqlc.arg(worker_epoch)
   AND r.computer_save_sequence=sqlc.arg(sequence) AND r.computer_save_version_id=sqlc.arg(save_id)
   AND r.computer_save_lease_id=sqlc.arg(lease_id)
   AND NOT EXISTS(SELECT 1 FROM computer_versions v
       WHERE v.publisher_runtime_instance_id=r.id
       AND v.publisher_save_sequence=r.computer_save_sequence);

-- Replay uses immutable publication facts, not the current head or pending slot.
-- name: GetWorkerComputerSave :one
SELECT v.* FROM computer_versions v JOIN runtime_instances r ON r.id=v.publisher_runtime_instance_id
 WHERE v.id=sqlc.arg(save_id) AND v.publisher_save_sequence=sqlc.arg(sequence)
 AND r.worker_instance_id=sqlc.arg(worker_instance_id)
 AND r.worker_group_id=sqlc.arg(worker_group_id) AND r.worker_epoch=sqlc.arg(worker_epoch);

-- All objects are certified and retained by the exact operation before this
-- transaction. The caller holds Computer/execution/Runtime locks and rechecks
-- deadlines before commit. Pending ownership remains until source adoption.
-- name: PublishRuntimeComputerSave :one
WITH created AS (
 INSERT INTO computer_versions(id,environment_id,computer_id,parent_version_id,
 root_pack_digest,logical_bytes,status,source_workspace_lease_id,ownership_generation,writer_generation,
 publisher_runtime_instance_id,publisher_desired_version,publisher_save_sequence,
 publication_request_fingerprint,published_at)
 SELECT r.computer_save_version_id,r.environment_id,r.workspace_id,r.computer_save_base_version_id,
 sqlc.arg(root_pack_digest),sqlc.arg(logical_bytes),'committed',l.id,l.ownership_generation,l.writer_generation,
 r.id,r.desired_version,r.computer_save_sequence,sqlc.arg(fingerprint),clock_timestamp()
 FROM runtime_instances r JOIN workspace_leases l ON l.id=r.computer_save_lease_id
 JOIN computers c ON c.id=r.workspace_id AND c.environment_id=r.environment_id
 WHERE r.id=sqlc.arg(runtime_instance_id) AND r.computer_save_version_id=sqlc.arg(save_id)
 AND r.computer_save_sequence=sqlc.arg(sequence)
 AND r.reclaimed_at IS NULL AND r.desired_state='ready'
 AND c.head_version_id=r.computer_save_base_version_id
 AND c.ownership_generation=l.ownership_generation AND c.writer_generation=l.writer_generation
 AND l.status='active' AND l.expires_at>clock_timestamp()
 RETURNING *
), retained AS (
 INSERT INTO computer_version_roots(environment_id,computer_id,version_id,locator)
 SELECT environment_id,computer_id,id,sqlc.arg(locator) FROM created RETURNING version_id
), advanced AS (
 UPDATE computers c SET head_version_id=v.id,revision=revision+1,updated_at=v.published_at
 FROM created v,retained root WHERE c.id=v.computer_id AND root.version_id=v.id
 AND c.head_version_id=v.parent_version_id
 RETURNING c.id
)
SELECT v.* FROM created v JOIN advanced c ON c.id=v.computer_id;

-- Host acknowledgement follows durable local adoption and producer quiescence.
-- Move only read-source retention; reservation/Run/Attempt/lease origins stay fixed.
-- The caller holds the live owner locks and validates the immutable receipt.
-- name: AdoptRuntimeComputerSave :execrows
UPDATE runtime_instances r
SET computer_source_version_id=v.id,
    computer_save_version_id=NULL, computer_save_lease_id=NULL,
    computer_save_base_version_id=NULL, updated_at=clock_timestamp()
FROM computer_versions v, computer_version_roots root
WHERE r.id=sqlc.arg(runtime_instance_id)
  AND r.worker_instance_id=sqlc.arg(worker_instance_id) AND r.worker_epoch=sqlc.arg(worker_epoch)
  AND r.reclaimed_at IS NULL
  AND r.computer_save_sequence=sqlc.arg(sequence) AND r.computer_save_version_id=sqlc.arg(save_id)
  AND r.computer_save_lease_id=sqlc.arg(lease_id)
  AND v.id=r.computer_save_version_id AND v.publisher_runtime_instance_id=r.id
  AND v.publisher_save_sequence=r.computer_save_sequence AND v.status='committed'
  AND root.environment_id=r.environment_id AND root.computer_id=r.workspace_id
  AND root.version_id=v.id;

-- Historical acknowledgement: a committed operation cannot be abandoned.
-- name: IsRuntimeComputerSaveAdopted :one
SELECT computer_save_sequence > sqlc.arg(sequence)::bigint
   OR (computer_save_sequence = sqlc.arg(sequence)::bigint
       AND computer_save_version_id IS DISTINCT FROM sqlc.arg(save_id)::uuid) AS adopted
FROM runtime_instances WHERE id=sqlc.arg(runtime_instance_id);

-- Stable absence, not a historical receipt: this sequence cannot be admitted
-- again and no committed save at that sequence exists. Read-only history remains
-- scoped to the authenticated Worker and the original execution identity.
-- name: IsComputerSaveAbandoned :one
SELECT (r.computer_save_sequence >= sqlc.arg(sequence)::bigint
        AND (r.computer_save_sequence > sqlc.arg(sequence)::bigint OR r.computer_save_version_id IS NULL)
        AND NOT EXISTS(SELECT 1 FROM computer_versions v WHERE v.publisher_runtime_instance_id=r.id
          AND v.publisher_save_sequence=sqlc.arg(sequence)::bigint)) AS abandoned
FROM runtime_instances r
WHERE r.worker_instance_id=sqlc.arg(worker_instance_id)
  AND r.worker_group_id=sqlc.arg(worker_group_id) AND r.worker_epoch=sqlc.arg(worker_epoch)
  AND ((sqlc.narg(run_lease_id)::uuid IS NOT NULL AND EXISTS(
       SELECT 1 FROM run_leases l WHERE l.id=sqlc.narg(run_lease_id)
       AND l.lease_sequence=sqlc.arg(lease_sequence) AND l.runtime_instance_id=r.id
       AND l.worker_instance_id=r.worker_instance_id AND l.worker_epoch=r.worker_epoch))
    OR (sqlc.narg(mount_id)::uuid IS NOT NULL AND EXISTS(
       SELECT 1 FROM workspace_processes p JOIN workspace_mounts m ON m.id=p.workspace_mount_id
       WHERE m.id=sqlc.narg(mount_id) AND m.org_id=sqlc.narg(org_id)
       AND m.runtime_instance_id=r.id AND m.worker_instance_id=r.worker_instance_id
       AND m.worker_epoch=r.worker_epoch AND p.runtime_instance_id=r.id)));

-- Worker/lease expiry alone is not exclusion evidence. Only physical reclaim
-- abandons an unpublished lost operation. Preserve the sequence. A committed
-- pending slot is not cleared: physical reclaim is not evidence of local adoption.
-- Its object pins are released separately after the same exclusion boundary.
-- name: AbandonReclaimedComputerSaves :execrows
WITH released AS (
 SELECT r.id FROM runtime_instances r
 WHERE r.reclaimed_at IS NOT NULL AND r.computer_save_version_id IS NOT NULL
 AND NOT EXISTS(SELECT 1 FROM computer_versions v WHERE v.publisher_runtime_instance_id=r.id
   AND v.publisher_save_sequence=r.computer_save_sequence)
 ORDER BY r.id LIMIT sqlc.arg(row_limit)
 FOR UPDATE OF r SKIP LOCKED
)
UPDATE runtime_instances r
SET computer_save_version_id=NULL,computer_save_lease_id=NULL,computer_save_base_version_id=NULL,
 updated_at=clock_timestamp()
FROM released WHERE r.id=released.id;
