-- These operations run only under the owning Computer/Runtime fence. Locator
-- framing, authenticated page membership and upload correspondence are prerequisites.

-- name: CreateComputerVersionRoot :exec
INSERT INTO computer_version_roots(environment_id,computer_id,version_id,locator)
VALUES(sqlc.arg(environment_id),sqlc.arg(computer_id),sqlc.arg(version_id),sqlc.arg(locator));

-- name: GetComputerVersionRoot :one
SELECT locator FROM computer_version_roots
 WHERE environment_id=sqlc.arg(environment_id) AND computer_id=sqlc.arg(computer_id)
   AND version_id=sqlc.arg(version_id);

-- name: PinRuntimeComputerSource :execrows
UPDATE runtime_instances r SET computer_source_version_id=sqlc.arg(version_id)
 WHERE r.id=sqlc.arg(runtime_instance_id) AND r.environment_id=sqlc.arg(environment_id)
   AND r.workspace_id=sqlc.arg(computer_id) AND r.reclaimed_at IS NULL
   AND r.reserved_workspace_version_id=sqlc.arg(version_id)
   AND EXISTS(SELECT 1 FROM computer_version_roots v WHERE v.environment_id=r.environment_id
      AND v.computer_id=r.workspace_id AND v.version_id=sqlc.arg(version_id)
      AND v.logical_bytes=r.reserved_guest_ephemeral_disk_bytes)
   AND (r.computer_source_version_id IS NULL OR r.computer_source_version_id=sqlc.arg(version_id));

-- Derive keys from the Runtime's pinned source, not from a caller-supplied root.
-- The broker must revalidate its full live authority before/after provider I/O.
-- name: ListRuntimeComputerSourceKeys :many
SELECT k.* FROM runtime_instances r
 JOIN computer_version_roots v ON v.environment_id=r.environment_id AND v.computer_id=r.workspace_id
   AND v.version_id=r.retained_computer_source_version_id
 JOIN computer_object_keys dependency ON dependency.environment_id=v.environment_id
   AND dependency.computer_id=v.computer_id AND dependency.digest=v.root_digest
 JOIN computer_data_keys k ON k.environment_id=dependency.environment_id
   AND k.computer_id=dependency.computer_id AND k.id=dependency.key_id
 WHERE r.id=sqlc.arg(runtime_instance_id)
 ORDER BY k.id;

-- Resolve the retained source, never the current Computer head or reservation.
-- This is retention evidence, not live authorization; callers hold/recheck their
-- Runtime and Worker fences before granting source or key access.
-- name: GetRuntimeComputerSourceRoot :one
SELECT v.version_id, v.locator, v.logical_bytes
FROM runtime_instances r
JOIN computer_version_roots v ON v.environment_id=r.environment_id
 AND v.computer_id=r.workspace_id AND v.version_id=r.retained_computer_source_version_id
WHERE r.id=sqlc.arg(runtime_instance_id);
