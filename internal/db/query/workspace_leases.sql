-- name: GetWorkspaceLease :one
SELECT *
  FROM workspace_leases
 WHERE environment_id = sqlc.arg(environment_id)
   AND workspace_id = sqlc.arg(workspace_id)
   AND id = sqlc.arg(id);
