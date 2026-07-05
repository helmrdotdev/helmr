-- name: EnsureOrgCell :one
INSERT INTO org_cells (org_id, cell_id, role, state)
VALUES (
    sqlc.arg(org_id),
    sqlc.arg(cell_id),
    sqlc.arg(role)::org_cell_role,
    sqlc.arg(state)::org_cell_state
)
ON CONFLICT (org_id, cell_id, role) DO UPDATE
   SET state = EXCLUDED.state
RETURNING *;

-- name: GetOrgCell :one
SELECT *
  FROM org_cells
 WHERE org_id = sqlc.arg(org_id)
   AND cell_id = sqlc.arg(cell_id)
   AND role = sqlc.arg(role)::org_cell_role;

-- name: ListOrgCells :many
SELECT *
  FROM org_cells
 WHERE org_id = sqlc.arg(org_id)
 ORDER BY cell_id, role;
