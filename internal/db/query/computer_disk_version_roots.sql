-- These operations run only under the owning Computer/Instance fence. Locator
-- framing, authenticated page membership and upload correspondence are prerequisites.

-- name: CreateComputerDiskVersionRoot :exec
INSERT INTO computer_disk_version_roots(environment_id,computer_id,version_id,locator)
VALUES(sqlc.arg(environment_id),sqlc.arg(computer_id),sqlc.arg(version_id),sqlc.arg(locator));

-- name: GetComputerDiskVersionRoot :one
SELECT locator FROM computer_disk_version_roots
 WHERE environment_id=sqlc.arg(environment_id) AND computer_id=sqlc.arg(computer_id)
   AND version_id=sqlc.arg(version_id);

-- name: PinInstanceComputerSource :execrows
UPDATE computer_instances r SET source_disk_version_id=sqlc.arg(version_id)
 WHERE r.id=sqlc.arg(computer_instance_id) AND r.environment_id=sqlc.arg(environment_id)
   AND r.computer_id=sqlc.arg(computer_id) AND r.reclaimed_at IS NULL
   AND EXISTS(SELECT 1 FROM computer_disk_version_roots v WHERE v.environment_id=r.environment_id
      AND v.computer_id=r.computer_id AND v.version_id=sqlc.arg(version_id)
      AND v.logical_bytes=r.reserved_guest_ephemeral_disk_bytes)
   AND (r.source_disk_version_id IS NULL OR r.source_disk_version_id=sqlc.arg(version_id));

-- Derive keys from the Instance's pinned source, not from a caller-supplied root.
-- The broker must revalidate its full live authority before/after provider I/O.
-- name: ListInstanceComputerSourceKeys :many
SELECT k.* FROM computer_instances r
 JOIN computer_disk_version_roots v ON v.environment_id=r.environment_id AND v.computer_id=r.computer_id
   AND v.version_id=r.retained_source_disk_version_id
 JOIN computer_object_keys dependency ON dependency.environment_id=v.environment_id
   AND dependency.computer_id=v.computer_id AND dependency.digest=v.root_pack_digest
 JOIN computer_data_keys k ON k.environment_id=dependency.environment_id
   AND k.computer_id=dependency.computer_id AND k.id=dependency.key_id
 WHERE r.id=sqlc.arg(computer_instance_id)
 ORDER BY k.id;

-- Resolve the retained source, never the current Computer head or reservation.
-- This is retention evidence, not live authorization; callers hold/recheck their
-- Instance and Worker fences before granting source or key access.
-- name: GetInstanceComputerSourceRoot :one
SELECT v.version_id, v.locator, v.logical_bytes
FROM computer_instances r
JOIN computer_disk_version_roots v ON v.environment_id=r.environment_id
 AND v.computer_id=r.computer_id AND v.version_id=r.retained_source_disk_version_id
WHERE r.id=sqlc.arg(computer_instance_id);

-- name: RequireComputerObjectPin :one
SELECT digest FROM computer_object_pins
 WHERE computer_instance_id=sqlc.arg(computer_instance_id)
 AND instance_desired_version=sqlc.arg(instance_desired_version)
 AND publication_key=sqlc.arg(publication_key)
 AND digest=sqlc.arg(digest);

-- Native owners arbitrate retirement with restrictive availability FKs. This
-- discovery only avoids repeatedly selecting retained roots in the bounded sweep.
-- name: ListUnreferencedComputerDiskVersionRoots :many
SELECT r.environment_id,r.computer_id,r.version_id
FROM computer_disk_version_roots r
WHERE NOT EXISTS (SELECT 1 FROM computers owner WHERE computer_payload_required AND head_disk_version_id IS NOT NULL AND owner.id=r.computer_id AND owner.head_disk_version_id=r.version_id)
 AND NOT EXISTS (SELECT 1 FROM computers owner WHERE recovery_payload_required AND recovery_disk_version_id IS NOT NULL AND owner.id=r.computer_id AND owner.recovery_disk_version_id=r.version_id)
 AND NOT EXISTS (SELECT 1 FROM runs owner WHERE computer_payload_required AND base_computer_disk_version_id IS NOT NULL AND owner.computer_id=r.computer_id AND owner.base_computer_disk_version_id=r.version_id)
 AND NOT EXISTS (SELECT 1 FROM run_attempts owner WHERE computer_payload_required AND base_computer_disk_version_id IS NOT NULL AND owner.computer_id=r.computer_id AND owner.base_computer_disk_version_id=r.version_id)
 AND NOT EXISTS (SELECT 1 FROM computer_checkpoints owner WHERE computer_payload_required AND base_computer_disk_version_id IS NOT NULL AND owner.computer_id=r.computer_id AND owner.base_computer_disk_version_id=r.version_id)
 AND NOT EXISTS (SELECT 1 FROM computer_checkpoints owner WHERE computer_payload_required AND private_computer_disk_version_id IS NOT NULL AND owner.computer_id=r.computer_id AND owner.private_computer_disk_version_id=r.version_id)
 AND NOT EXISTS (SELECT 1 FROM computer_instances owner WHERE retained_source_disk_version_id IS NOT NULL AND owner.computer_id=r.computer_id AND owner.retained_source_disk_version_id=r.version_id)
 AND NOT EXISTS (SELECT 1 FROM computer_instances owner WHERE reclaimed_at IS NULL AND save_disk_version_id IS NOT NULL AND owner.computer_id=r.computer_id AND owner.save_disk_version_id=r.version_id)
ORDER BY r.version_id LIMIT sqlc.arg(row_limit);

-- name: DeleteUnreferencedComputerDiskVersionRoot :execrows
DELETE FROM computer_disk_version_roots r
WHERE r.environment_id=sqlc.arg(environment_id) AND r.computer_id=sqlc.arg(computer_id)
 AND r.version_id=sqlc.arg(version_id)
 AND NOT EXISTS (SELECT 1 FROM computers owner WHERE computer_payload_required AND head_disk_version_id IS NOT NULL AND owner.id=r.computer_id AND owner.head_disk_version_id=r.version_id)
 AND NOT EXISTS (SELECT 1 FROM computers owner WHERE recovery_payload_required AND recovery_disk_version_id IS NOT NULL AND owner.id=r.computer_id AND owner.recovery_disk_version_id=r.version_id)
 AND NOT EXISTS (SELECT 1 FROM runs owner WHERE computer_payload_required AND base_computer_disk_version_id IS NOT NULL AND owner.computer_id=r.computer_id AND owner.base_computer_disk_version_id=r.version_id)
 AND NOT EXISTS (SELECT 1 FROM run_attempts owner WHERE computer_payload_required AND base_computer_disk_version_id IS NOT NULL AND owner.computer_id=r.computer_id AND owner.base_computer_disk_version_id=r.version_id)
 AND NOT EXISTS (SELECT 1 FROM computer_checkpoints owner WHERE computer_payload_required AND base_computer_disk_version_id IS NOT NULL AND owner.computer_id=r.computer_id AND owner.base_computer_disk_version_id=r.version_id)
 AND NOT EXISTS (SELECT 1 FROM computer_checkpoints owner WHERE computer_payload_required AND private_computer_disk_version_id IS NOT NULL AND owner.computer_id=r.computer_id AND owner.private_computer_disk_version_id=r.version_id)
 AND NOT EXISTS (SELECT 1 FROM computer_instances owner WHERE retained_source_disk_version_id IS NOT NULL AND owner.computer_id=r.computer_id AND owner.retained_source_disk_version_id=r.version_id)
 AND NOT EXISTS (SELECT 1 FROM computer_instances owner WHERE reclaimed_at IS NULL AND save_disk_version_id IS NOT NULL AND owner.computer_id=r.computer_id AND owner.save_disk_version_id=r.version_id);

-- Called after deleting the exact root in the same transaction. A concurrent
-- owner acquisition rejects this update and rolls root removal back as well.
-- name: RetireComputerDiskVersionPayload :execrows
UPDATE computer_disk_versions SET payload_retired_at=clock_timestamp()
WHERE environment_id=sqlc.arg(environment_id) AND computer_id=sqlc.arg(computer_id)
 AND id=sqlc.arg(version_id) AND payload_not_retired;
