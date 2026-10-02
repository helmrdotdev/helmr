-- These operations run only under the owning Computer/Instance fence. Locator
-- framing, authenticated page membership and upload correspondence are prerequisites.

-- name: RetainComputerDiskRoot :one
INSERT INTO computer_disk_roots(id,environment_id,locator)
VALUES(sqlc.arg(id),sqlc.arg(environment_id),sqlc.arg(locator))
ON CONFLICT (environment_id,root_pack_digest,root_page_offset) DO UPDATE
 SET locator=EXCLUDED.locator
 WHERE computer_disk_roots.locator=EXCLUDED.locator
RETURNING id;

-- name: CreateComputerDiskVersionRoot :exec
INSERT INTO computer_disk_version_roots(environment_id,computer_id,version_id,root_id)
VALUES(sqlc.arg(environment_id),sqlc.arg(computer_id),sqlc.arg(version_id),sqlc.arg(root_id));

-- name: GetComputerDiskVersionRoot :one
SELECT r.locator FROM computer_disk_version_roots v
 JOIN computer_disk_roots r ON r.environment_id=v.environment_id AND r.id=v.root_id
 WHERE v.environment_id=sqlc.arg(environment_id) AND v.computer_id=sqlc.arg(computer_id)
   AND v.version_id=sqlc.arg(version_id);

-- name: PinInstanceComputerSource :execrows
UPDATE computer_instances r SET source_disk_version_id=sqlc.arg(version_id)
 WHERE r.id=sqlc.arg(computer_instance_id) AND r.environment_id=sqlc.arg(environment_id)
   AND r.computer_id=sqlc.arg(computer_id) AND r.reclaimed_at IS NULL
   AND EXISTS(SELECT 1 FROM computer_disk_version_roots v JOIN computer_disk_roots root ON root.environment_id=v.environment_id AND root.id=v.root_id WHERE v.environment_id=r.environment_id
      AND v.computer_id=r.computer_id AND v.version_id=sqlc.arg(version_id)
      AND root.logical_bytes=r.reserved_guest_ephemeral_disk_bytes)
   AND (r.source_disk_version_id IS NULL OR r.source_disk_version_id=sqlc.arg(version_id));

-- Derive keys from the Instance's pinned source, not from a caller-supplied root.
-- The broker must revalidate its full live authority before/after provider I/O.
-- name: ListInstanceComputerSourceKeys :many
SELECT k.* FROM computer_instances r
 JOIN computer_disk_version_roots v ON v.environment_id=r.environment_id AND v.computer_id=r.computer_id
   AND v.version_id=r.retained_source_disk_version_id
 JOIN computer_disk_roots root ON root.environment_id=v.environment_id AND root.id=v.root_id
 JOIN computer_object_keys dependency ON dependency.environment_id=root.environment_id
   AND dependency.digest=root.root_pack_digest
 JOIN computer_data_keys k ON k.environment_id=dependency.environment_id
   AND k.id=dependency.key_id
 WHERE r.id=sqlc.arg(computer_instance_id)
 ORDER BY k.id;

-- Resolve the retained source, never the current Computer head or reservation.
-- This is retention evidence, not live authorization; callers hold/recheck their
-- Instance and Worker fences before granting source or key access.
-- name: GetInstanceComputerSourceRoot :one
SELECT v.version_id, root.locator, root.logical_bytes
FROM computer_instances r
JOIN computer_disk_version_roots v ON v.environment_id=r.environment_id
 AND v.computer_id=r.computer_id AND v.version_id=r.retained_source_disk_version_id
JOIN computer_disk_roots root ON root.environment_id=v.environment_id AND root.id=v.root_id
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

-- The shared descriptor may be removed only after all independent owners release it.
-- name: DeleteUnreferencedComputerDiskRoots :execrows
WITH candidates AS (
 SELECT r.id FROM computer_disk_roots r
 WHERE NOT EXISTS (SELECT 1 FROM computer_disk_version_roots v WHERE v.root_id=r.id)
 AND NOT EXISTS (SELECT 1 FROM computer_seeds s WHERE s.root_id=r.id)
 AND NOT EXISTS (SELECT 1 FROM computer_snapshots s WHERE s.root_id=r.id)
 ORDER BY r.id LIMIT sqlc.arg(row_limit) FOR UPDATE SKIP LOCKED
)
DELETE FROM computer_disk_roots r USING candidates c WHERE r.id=c.id;
