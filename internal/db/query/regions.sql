-- name: CreateRegion :one
INSERT INTO regions (id, provider, provider_region, display_name, state, visibility, location)
VALUES (
    sqlc.arg(id),
    sqlc.arg(provider),
    sqlc.arg(provider_region),
    sqlc.arg(display_name),
    sqlc.arg(state)::text,
    sqlc.arg(visibility)::region_visibility,
    sqlc.arg(location)::text
)
RETURNING *;

-- name: UpdateRegionMetadata :one
UPDATE regions
   SET display_name = sqlc.arg(display_name),
       visibility = sqlc.arg(visibility)::region_visibility,
       location = sqlc.arg(location),
       updated_at = now()
 WHERE id = sqlc.arg(id)
RETURNING *;

-- name: GetRegion :one
SELECT *
  FROM regions
 WHERE id = sqlc.arg(id);

-- name: GetRegionByProviderRegion :one
SELECT *
  FROM regions
 WHERE provider = sqlc.arg(provider)
   AND provider_region = sqlc.arg(provider_region);

-- name: ListRegions :many
SELECT *
  FROM regions
 ORDER BY lower(display_name), id;
