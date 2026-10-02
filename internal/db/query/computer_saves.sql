-- The caller authenticates the Worker and locks the Computer before its instance.
-- Disk publication belongs to the physical writer, independently of Run lifetimes.
-- name: BeginComputerInstanceSave :one
UPDATE computer_instances i
SET save_sequence=sqlc.arg(sequence),save_disk_version_id=sqlc.arg(save_id),
    save_base_disk_version_id=sqlc.arg(predecessor_id),updated_at=clock_timestamp()
FROM computers c,computer_disk_versions v
WHERE i.id=sqlc.arg(computer_instance_id) AND i.environment_id=sqlc.arg(environment_id)
 AND i.worker_host_id=sqlc.arg(worker_host_id) AND i.worker_epoch=sqlc.arg(worker_epoch)
 AND i.writer_generation=sqlc.arg(writer_generation) AND i.writer_token_hash=sqlc.arg(writer_token_hash)
 AND i.writer_expires_at>clock_timestamp() AND i.reclaimed_at IS NULL
 AND i.desired_version=sqlc.arg(desired_version) AND i.desired_state='ready'
 AND i.observed_state='ready' AND i.mount_state='mounted'
 AND c.id=i.computer_id AND c.environment_id=i.environment_id AND c.status='active'
 AND c.writer_generation=i.writer_generation
 AND v.id=c.head_disk_version_id AND v.computer_id=c.id AND v.environment_id=c.environment_id
 AND v.status='committed'
 AND ((i.save_disk_version_id IS NULL AND i.save_sequence=sqlc.arg(sequence)::bigint-1
       AND c.head_disk_version_id=sqlc.arg(predecessor_id)
       AND NOT EXISTS(SELECT 1 FROM computer_disk_versions existing WHERE existing.id=sqlc.arg(save_id)))
   OR (i.save_sequence=sqlc.arg(sequence) AND i.save_disk_version_id=sqlc.arg(save_id)
       AND i.save_base_disk_version_id=sqlc.arg(predecessor_id)))
RETURNING i.save_sequence,i.save_disk_version_id,i.save_base_disk_version_id;

-- The host joins producers before abandoning an unpublished operation. The
-- sequence watermark remains, preventing a delayed admission from resurrecting it.
-- name: AbandonComputerInstanceSave :execrows
UPDATE computer_instances i
SET save_disk_version_id=NULL,save_base_disk_version_id=NULL,updated_at=clock_timestamp()
WHERE i.id=sqlc.arg(computer_instance_id) AND i.environment_id=sqlc.arg(environment_id)
 AND i.worker_host_id=sqlc.arg(worker_host_id) AND i.worker_epoch=sqlc.arg(worker_epoch)
 AND i.writer_generation=sqlc.arg(writer_generation) AND i.writer_token_hash=sqlc.arg(writer_token_hash)
 AND i.save_sequence=sqlc.arg(sequence) AND i.save_disk_version_id=sqlc.arg(save_id)
 AND NOT EXISTS(SELECT 1 FROM computer_disk_versions v
                WHERE v.publisher_computer_instance_id=i.id AND v.publisher_save_sequence=i.save_sequence);

-- Immutable receipts remain queryable after lease expiry and physical reclaim.
-- name: GetWorkerComputerSave :one
SELECT v.* FROM computer_disk_versions v JOIN computer_instances i ON i.id=v.publisher_computer_instance_id
WHERE v.id=sqlc.arg(save_id) AND v.publisher_save_sequence=sqlc.arg(sequence)
 AND i.id=sqlc.arg(computer_instance_id) AND i.environment_id=sqlc.arg(environment_id)
 AND i.worker_host_id=sqlc.arg(worker_host_id) AND i.worker_group_id=sqlc.arg(worker_group_id)
 AND i.worker_epoch=sqlc.arg(worker_epoch) AND i.writer_generation=sqlc.arg(writer_generation);

-- Certified objects and their exact operation pins are checked under the same
-- Computer/instance locks. Head publication and root retention commit atomically.
-- name: PublishComputerInstanceSave :one
WITH created AS (
 INSERT INTO computer_disk_versions(id,environment_id,computer_id,parent_version_id,
 status,source_computer_instance_id,writer_generation,
 publisher_computer_instance_id,publisher_desired_version,publisher_save_sequence,
 publication_request_fingerprint,published_at)
 SELECT i.save_disk_version_id,i.environment_id,i.computer_id,i.save_base_disk_version_id,
 'committed',i.id,i.writer_generation,
 i.id,i.desired_version,i.save_sequence,sqlc.arg(fingerprint),clock_timestamp()
 FROM computer_instances i JOIN computers c ON c.id=i.computer_id AND c.environment_id=i.environment_id
 WHERE i.id=sqlc.arg(computer_instance_id) AND i.environment_id=sqlc.arg(environment_id)
 AND i.worker_host_id=sqlc.arg(worker_host_id) AND i.worker_epoch=sqlc.arg(worker_epoch)
 AND i.writer_generation=sqlc.arg(writer_generation) AND i.writer_token_hash=sqlc.arg(writer_token_hash)
 AND i.writer_expires_at>clock_timestamp() AND i.reclaimed_at IS NULL
 AND i.desired_version=sqlc.arg(desired_version) AND i.desired_state='ready'
 AND i.observed_state='ready' AND i.mount_state='mounted'
 AND i.save_disk_version_id=sqlc.arg(save_id) AND i.save_sequence=sqlc.arg(sequence)
 AND c.status='active' AND c.head_disk_version_id=i.save_base_disk_version_id
 AND c.writer_generation=i.writer_generation
 RETURNING *
), retained AS (
 INSERT INTO computer_disk_version_roots(environment_id,computer_id,version_id,root_id)
 SELECT environment_id,computer_id,id,sqlc.arg(root_id) FROM created RETURNING version_id
), advanced AS (
 UPDATE computers c SET head_disk_version_id=v.id,revision=revision+1,updated_at=v.published_at
 FROM created v,retained root WHERE c.id=v.computer_id AND root.version_id=v.id
 AND c.head_disk_version_id=v.parent_version_id AND c.writer_generation=v.writer_generation
 RETURNING c.id
)
SELECT v.* FROM created v JOIN advanced c ON c.id=v.computer_id;

-- Host acknowledgement follows durable local source adoption and producer
-- quiescence. Historical Run origins remain unchanged.
-- name: AdoptComputerInstanceSave :execrows
UPDATE computer_instances i
SET source_disk_version_id=v.id,save_disk_version_id=NULL,
    save_base_disk_version_id=NULL,updated_at=clock_timestamp()
FROM computer_disk_versions v,computer_disk_version_roots root
WHERE i.id=sqlc.arg(computer_instance_id) AND i.environment_id=sqlc.arg(environment_id)
 AND i.worker_host_id=sqlc.arg(worker_host_id) AND i.worker_epoch=sqlc.arg(worker_epoch)
 AND i.writer_generation=sqlc.arg(writer_generation) AND i.writer_token_hash=sqlc.arg(writer_token_hash)
 AND i.reclaimed_at IS NULL
 AND i.save_sequence=sqlc.arg(sequence) AND i.save_disk_version_id=sqlc.arg(save_id)
 AND v.id=i.save_disk_version_id AND v.publisher_computer_instance_id=i.id
 AND v.publisher_save_sequence=i.save_sequence AND v.status='committed'
 AND root.environment_id=i.environment_id AND root.computer_id=i.computer_id AND root.version_id=v.id;

-- name: IsComputerInstanceSaveAdopted :one
SELECT EXISTS(SELECT 1 FROM computer_disk_versions v
              WHERE v.id=sqlc.arg(save_id) AND v.publisher_computer_instance_id=i.id
              AND v.publisher_save_sequence=sqlc.arg(sequence))
 AND (i.save_sequence>sqlc.arg(sequence)::bigint
      OR (i.save_sequence=sqlc.arg(sequence)::bigint AND i.save_disk_version_id IS NULL)) AS adopted
FROM computer_instances i
WHERE i.id=sqlc.arg(computer_instance_id) AND i.environment_id=sqlc.arg(environment_id)
 AND i.worker_host_id=sqlc.arg(worker_host_id) AND i.worker_group_id=sqlc.arg(worker_group_id)
 AND i.worker_epoch=sqlc.arg(worker_epoch) AND i.writer_generation=sqlc.arg(writer_generation);

-- Stable absence is distinct from a committed save's historical receipt.
-- name: IsComputerSaveAbandoned :one
SELECT (i.save_sequence>=sqlc.arg(sequence)::bigint
        AND (i.save_sequence>sqlc.arg(sequence)::bigint OR i.save_disk_version_id IS NULL)
        AND NOT EXISTS(SELECT 1 FROM computer_disk_versions v WHERE v.publisher_computer_instance_id=i.id
                       AND v.publisher_save_sequence=sqlc.arg(sequence)::bigint)) AS abandoned
FROM computer_instances i
WHERE i.id=sqlc.arg(computer_instance_id) AND i.environment_id=sqlc.arg(environment_id)
 AND i.worker_host_id=sqlc.arg(worker_host_id) AND i.worker_group_id=sqlc.arg(worker_group_id)
 AND i.worker_epoch=sqlc.arg(worker_epoch) AND i.writer_generation=sqlc.arg(writer_generation);

-- Only observed physical reclaim abandons an unpublished lost operation. Expiry
-- is not exclusion, and reclaim is not evidence of local source adoption.
-- name: AbandonReclaimedComputerSaves :execrows
WITH released AS (
 SELECT i.id FROM computer_instances i WHERE i.reclaimed_at IS NOT NULL AND i.save_disk_version_id IS NOT NULL
 AND NOT EXISTS(SELECT 1 FROM computer_disk_versions v WHERE v.publisher_computer_instance_id=i.id
                AND v.publisher_save_sequence=i.save_sequence)
 ORDER BY i.id LIMIT sqlc.arg(row_limit) FOR UPDATE OF i SKIP LOCKED
)
UPDATE computer_instances i SET save_disk_version_id=NULL,save_base_disk_version_id=NULL,updated_at=clock_timestamp()
FROM released WHERE i.id=released.id;
