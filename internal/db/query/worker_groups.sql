-- name: EnsureDefaultWorkerGroup :one
INSERT INTO worker_groups (cell_id, name, description)
VALUES (sqlc.arg(cell_id), 'default', 'Default worker group')
ON CONFLICT (cell_id, name) DO UPDATE
   SET description = worker_groups.description
RETURNING *;

-- name: ListWorkerGroups :many
SELECT *
  FROM worker_groups
 WHERE cell_id = sqlc.arg(cell_id)
 ORDER BY name ASC
 LIMIT sqlc.arg(row_limit);
