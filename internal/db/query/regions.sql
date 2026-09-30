-- name: CreateRegion :one
INSERT INTO regions (id, display_name, location)
VALUES (
    sqlc.arg(id),
    sqlc.arg(display_name),
    sqlc.arg(location)::text
)
RETURNING *;

-- name: EnsureRegion :exec
INSERT INTO regions (id, display_name, location)
VALUES (
    sqlc.arg(id),
    sqlc.arg(display_name),
    sqlc.arg(location)::text
)
ON CONFLICT (id) DO NOTHING;

-- name: UpdateRegionMetadata :one
UPDATE regions
   SET display_name = CASE WHEN sqlc.arg(set_display_name)::boolean
                           THEN sqlc.arg(display_name)::text
                           ELSE display_name END,
       location = CASE WHEN sqlc.arg(set_location)::boolean
                       THEN sqlc.arg(location)::text
                       ELSE location END,
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
