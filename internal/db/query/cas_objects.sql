-- name: UpsertCasObject :one
INSERT INTO cas_objects (org_id, worker_group_id, digest, size_bytes, media_type)
VALUES (sqlc.arg(org_id), sqlc.arg(worker_group_id), sqlc.arg(digest), sqlc.arg(size_bytes), sqlc.arg(media_type))
ON CONFLICT (org_id, worker_group_id, digest) DO UPDATE SET
    size_bytes = cas_objects.size_bytes
WHERE cas_objects.size_bytes = EXCLUDED.size_bytes
  AND cas_objects.media_type = EXCLUDED.media_type
RETURNING *;

-- name: GetCasObject :one
SELECT *
  FROM cas_objects
 WHERE org_id = sqlc.arg(org_id)
   AND worker_group_id = sqlc.arg(worker_group_id)
   AND digest = sqlc.arg(digest);
