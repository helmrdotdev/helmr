-- name: CreateRegion :one
INSERT INTO regions (id, display_name, location)
VALUES (
    sqlc.arg(id),
    sqlc.arg(display_name),
    sqlc.arg(location)::text
)
RETURNING *;

-- name: UpdateRegionMetadata :one
UPDATE regions
   SET display_name = sqlc.arg(display_name),
       location = sqlc.arg(location),
       updated_at = now()
 WHERE id = sqlc.arg(id)
RETURNING *;

-- name: GetRegion :one
SELECT *
  FROM regions
 WHERE id = sqlc.arg(id);

-- name: ListRegions :many
SELECT *
  FROM regions
 ORDER BY lower(display_name), id;
