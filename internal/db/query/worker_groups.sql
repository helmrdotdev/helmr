-- name: GetDefaultWorkerGroup :one
SELECT *
  FROM worker_groups
 WHERE name = 'default';

-- name: ListWorkerGroups :many
SELECT *
  FROM worker_groups
 ORDER BY name ASC
 LIMIT sqlc.arg(row_limit);
