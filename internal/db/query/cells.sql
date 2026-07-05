-- name: EnsureCell :one
INSERT INTO cells (id, region_id, environment_class, state)
VALUES (
    sqlc.arg(id),
    sqlc.arg(region_id),
    sqlc.arg(environment_class),
    sqlc.arg(state)::cell_state
)
ON CONFLICT (id) DO UPDATE
   SET environment_class = EXCLUDED.environment_class
 WHERE cells.region_id = EXCLUDED.region_id
RETURNING *;

-- name: GetCell :one
SELECT *
  FROM cells
 WHERE id = sqlc.arg(id);
