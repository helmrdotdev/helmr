-- name: UpsertCasObject :one
INSERT INTO cas_objects (digest, size_bytes, media_type)
VALUES ($1, $2, $3)
ON CONFLICT (digest) DO UPDATE SET
    size_bytes = cas_objects.size_bytes
WHERE cas_objects.size_bytes = EXCLUDED.size_bytes
  AND cas_objects.media_type = EXCLUDED.media_type
RETURNING *;

-- name: GetCasObject :one
SELECT *
  FROM cas_objects
 WHERE digest = $1;
