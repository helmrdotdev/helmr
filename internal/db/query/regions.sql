-- name: EnsureRegion :one
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
ON CONFLICT (id) DO UPDATE
   SET provider = EXCLUDED.provider,
       provider_region = EXCLUDED.provider_region,
       display_name = EXCLUDED.display_name,
       updated_at = now()
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
