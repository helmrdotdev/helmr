-- Caller locks the current checkpoint source and owns the enclosing transaction.
-- Registration never observes remote existence or grants guest execution. The
-- entire four-object set must succeed or roll back; exact replays preserve candidate identity.
-- name: RegisterCheckpointObject :one
WITH lifetime AS (
    INSERT INTO cas_blobs (digest, size_bytes) VALUES (sqlc.arg(digest), sqlc.arg(size_bytes))
    ON CONFLICT DO NOTHING
)
INSERT INTO computer_checkpoint_objects (checkpoint_id, role, digest, size_bytes, media_type, checkpoint_status)
SELECT id, sqlc.arg(role), sqlc.arg(digest), sqlc.arg(size_bytes), sqlc.arg(media_type), status
  FROM computer_checkpoints WHERE id=sqlc.arg(checkpoint_id) AND status='creating'
ON CONFLICT (checkpoint_id, role) DO UPDATE SET role=computer_checkpoint_objects.role
 WHERE computer_checkpoint_objects.digest=EXCLUDED.digest
   AND computer_checkpoint_objects.size_bytes=EXCLUDED.size_bytes
   AND computer_checkpoint_objects.media_type=EXCLUDED.media_type
   AND computer_checkpoint_objects.checkpoint_status='creating'
RETURNING *;

-- name: ListCheckpointObjects :many
SELECT * FROM computer_checkpoint_objects WHERE checkpoint_id=sqlc.arg(checkpoint_id) ORDER BY role;

-- name: RegisterCheckpointManifest :execrows
UPDATE computer_checkpoints SET manifest=sqlc.arg(manifest)
 WHERE id=sqlc.arg(id) AND status='creating'
   AND (manifest IS NULL OR manifest=sqlc.arg(manifest));

-- name: RequireRegisteredCheckpointManifest :one
SELECT id FROM computer_checkpoints
 WHERE id=sqlc.arg(id) AND status='creating'
   AND manifest=sqlc.arg(manifest)
   AND (SELECT count(*) FROM computer_checkpoint_objects WHERE checkpoint_id=computer_checkpoints.id AND checkpoint_status='creating')=4;
