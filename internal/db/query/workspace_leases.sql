-- name: GetWorkspaceLease :one
SELECT *
  FROM workspace_leases
 WHERE environment_id = sqlc.arg(environment_id)
   AND workspace_id = sqlc.arg(workspace_id)
   AND id = sqlc.arg(id);

-- name: GetRunOwnedWorkspaceLease :one
SELECT workspace_leases.*
  FROM workspace_leases
  JOIN run_leases
    ON run_leases.workspace_id = workspace_leases.workspace_id
   AND run_leases.id = workspace_leases.owner_run_lease_id
 WHERE run_leases.run_id = sqlc.arg(run_id)
   AND run_leases.attempt_number = sqlc.arg(attempt_number)
   AND workspace_leases.workspace_id = sqlc.arg(workspace_id)
   AND workspace_leases.id = sqlc.arg(id);

-- name: ExpireWorkspaceLeases :many
UPDATE workspace_leases
   SET state = 'expired',
       terminal_at = now(),
       terminal_reason_code = 'lease_expired',
       updated_at = now()
 WHERE state IN ('active', 'releasing')
   AND expires_at <= now()
RETURNING *;
