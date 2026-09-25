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
   AND dependency.computer_id=v.computer_id AND dependency.digest=v.root_pack_digest
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

-- name: RequireRuntimeComputerObjectPin :one
SELECT digest FROM runtime_computer_object_pins
 WHERE runtime_instance_id=sqlc.arg(runtime_instance_id)
 AND runtime_desired_version=sqlc.arg(runtime_desired_version)
 AND publication_key=sqlc.arg(publication_key)
 AND digest=sqlc.arg(digest);

-- Native owners arbitrate retirement with restrictive availability FKs. This
-- discovery only avoids repeatedly selecting retained roots in the bounded sweep.
-- name: ListUnreferencedComputerVersionRoots :many
SELECT r.environment_id,r.computer_id,r.version_id
FROM computer_version_roots r
WHERE NOT EXISTS (SELECT 1 FROM computers owner WHERE computer_payload_required AND head_version_id IS NOT NULL AND owner.id=r.computer_id AND owner.head_version_id=r.version_id)
 AND NOT EXISTS (SELECT 1 FROM computers owner WHERE recovery_payload_required AND recovery_version_id IS NOT NULL AND owner.id=r.computer_id AND owner.recovery_version_id=r.version_id)
 AND NOT EXISTS (SELECT 1 FROM runs owner WHERE computer_payload_required AND base_workspace_version_id IS NOT NULL AND owner.workspace_id=r.computer_id AND owner.base_workspace_version_id=r.version_id)
 AND NOT EXISTS (SELECT 1 FROM run_attempts owner WHERE computer_payload_required AND base_workspace_version_id IS NOT NULL AND owner.workspace_id=r.computer_id AND owner.base_workspace_version_id=r.version_id)
 AND NOT EXISTS (SELECT 1 FROM run_checkpoints owner WHERE computer_payload_required AND base_workspace_version_id IS NOT NULL AND owner.workspace_id=r.computer_id AND owner.base_workspace_version_id=r.version_id)
 AND NOT EXISTS (SELECT 1 FROM run_checkpoints owner WHERE computer_payload_required AND private_workspace_version_id IS NOT NULL AND owner.workspace_id=r.computer_id AND owner.private_workspace_version_id=r.version_id)
 AND NOT EXISTS (SELECT 1 FROM run_waits owner WHERE computer_payload_required AND base_workspace_version_id IS NOT NULL AND owner.workspace_id=r.computer_id AND owner.base_workspace_version_id=r.version_id)
 AND NOT EXISTS (SELECT 1 FROM run_waits owner WHERE computer_payload_required AND resume_workspace_version_id IS NOT NULL AND owner.workspace_id=r.computer_id AND owner.resume_workspace_version_id=r.version_id)
 AND NOT EXISTS (SELECT 1 FROM workspace_processes owner WHERE computer_payload_required AND base_workspace_version_id IS NOT NULL AND owner.workspace_id=r.computer_id AND owner.base_workspace_version_id=r.version_id)
 AND NOT EXISTS (SELECT 1 FROM workspace_processes owner WHERE computer_payload_required AND staged_version_id IS NOT NULL AND owner.workspace_id=r.computer_id AND owner.staged_version_id=r.version_id)
 AND NOT EXISTS (SELECT 1 FROM runtime_instances owner WHERE computer_payload_required AND reserved_workspace_version_id IS NOT NULL AND owner.workspace_id=r.computer_id AND owner.reserved_workspace_version_id=r.version_id)
 AND NOT EXISTS (SELECT 1 FROM runtime_instances owner WHERE retained_computer_source_version_id IS NOT NULL AND owner.workspace_id=r.computer_id AND owner.retained_computer_source_version_id=r.version_id)
 AND NOT EXISTS (SELECT 1 FROM runtime_instances owner WHERE reclaimed_at IS NULL AND computer_save_version_id IS NOT NULL AND owner.workspace_id=r.computer_id AND owner.computer_save_version_id=r.version_id)
ORDER BY r.version_id LIMIT sqlc.arg(row_limit);

-- name: DeleteUnreferencedComputerVersionRoot :execrows
DELETE FROM computer_version_roots r
WHERE r.environment_id=sqlc.arg(environment_id) AND r.computer_id=sqlc.arg(computer_id)
 AND r.version_id=sqlc.arg(version_id)
 AND NOT EXISTS (SELECT 1 FROM computers owner WHERE computer_payload_required AND head_version_id IS NOT NULL AND owner.id=r.computer_id AND owner.head_version_id=r.version_id)
 AND NOT EXISTS (SELECT 1 FROM computers owner WHERE recovery_payload_required AND recovery_version_id IS NOT NULL AND owner.id=r.computer_id AND owner.recovery_version_id=r.version_id)
 AND NOT EXISTS (SELECT 1 FROM runs owner WHERE computer_payload_required AND base_workspace_version_id IS NOT NULL AND owner.workspace_id=r.computer_id AND owner.base_workspace_version_id=r.version_id)
 AND NOT EXISTS (SELECT 1 FROM run_attempts owner WHERE computer_payload_required AND base_workspace_version_id IS NOT NULL AND owner.workspace_id=r.computer_id AND owner.base_workspace_version_id=r.version_id)
 AND NOT EXISTS (SELECT 1 FROM run_checkpoints owner WHERE computer_payload_required AND base_workspace_version_id IS NOT NULL AND owner.workspace_id=r.computer_id AND owner.base_workspace_version_id=r.version_id)
 AND NOT EXISTS (SELECT 1 FROM run_checkpoints owner WHERE computer_payload_required AND private_workspace_version_id IS NOT NULL AND owner.workspace_id=r.computer_id AND owner.private_workspace_version_id=r.version_id)
 AND NOT EXISTS (SELECT 1 FROM run_waits owner WHERE computer_payload_required AND base_workspace_version_id IS NOT NULL AND owner.workspace_id=r.computer_id AND owner.base_workspace_version_id=r.version_id)
 AND NOT EXISTS (SELECT 1 FROM run_waits owner WHERE computer_payload_required AND resume_workspace_version_id IS NOT NULL AND owner.workspace_id=r.computer_id AND owner.resume_workspace_version_id=r.version_id)
 AND NOT EXISTS (SELECT 1 FROM workspace_processes owner WHERE computer_payload_required AND base_workspace_version_id IS NOT NULL AND owner.workspace_id=r.computer_id AND owner.base_workspace_version_id=r.version_id)
 AND NOT EXISTS (SELECT 1 FROM workspace_processes owner WHERE computer_payload_required AND staged_version_id IS NOT NULL AND owner.workspace_id=r.computer_id AND owner.staged_version_id=r.version_id)
 AND NOT EXISTS (SELECT 1 FROM runtime_instances owner WHERE computer_payload_required AND reserved_workspace_version_id IS NOT NULL AND owner.workspace_id=r.computer_id AND owner.reserved_workspace_version_id=r.version_id)
 AND NOT EXISTS (SELECT 1 FROM runtime_instances owner WHERE retained_computer_source_version_id IS NOT NULL AND owner.workspace_id=r.computer_id AND owner.retained_computer_source_version_id=r.version_id)
 AND NOT EXISTS (SELECT 1 FROM runtime_instances owner WHERE reclaimed_at IS NULL AND computer_save_version_id IS NOT NULL AND owner.workspace_id=r.computer_id AND owner.computer_save_version_id=r.version_id);

-- Called after deleting the exact root in the same transaction. A concurrent
-- owner acquisition rejects this update and rolls root removal back as well.
-- name: RetireComputerVersionPayload :execrows
UPDATE computer_versions SET payload_retired_at=clock_timestamp()
WHERE environment_id=sqlc.arg(environment_id) AND computer_id=sqlc.arg(computer_id)
 AND id=sqlc.arg(version_id) AND payload_not_retired;
