-- name: EnsureRegion :one
INSERT INTO regions (id, provider, provider_region, display_name, state, visibility, location, static_ips)
VALUES (
    sqlc.arg(id),
    sqlc.arg(provider),
    sqlc.arg(provider_region),
    sqlc.arg(display_name),
    sqlc.arg(state)::region_state,
    sqlc.arg(visibility)::region_visibility,
    sqlc.arg(location),
    sqlc.arg(static_ips)::text[]
)
ON CONFLICT (id) DO UPDATE
   SET provider = EXCLUDED.provider,
       provider_region = EXCLUDED.provider_region,
       display_name = EXCLUDED.display_name
RETURNING *;

-- name: GetRegion :one
SELECT *
  FROM regions
 WHERE id = sqlc.arg(id);

-- name: ListRegions :many
SELECT *
  FROM regions
 ORDER BY lower(display_name), id;
