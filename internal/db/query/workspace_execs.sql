-- name: CreateWorkspaceExec :one
INSERT INTO workspace_processes (
    id,
    org_id,
    project_id,
    environment_id,
    workspace_id,
    kind,
    state,
    request,
    claim_id,
    stdout_cursor,
    stderr_cursor,
    stdin_cursor,
    stdin_delivered_cursor,
    stdin_closed_at,
    created_by_subject_type,
    created_by_subject_id
) VALUES (
    sqlc.arg(id),
    sqlc.arg(org_id),
    sqlc.arg(project_id),
    sqlc.arg(environment_id),
    sqlc.arg(workspace_id),
    'exec',
    'pending',
    sqlc.arg(request),
    sqlc.arg(claim_id),
    0,
    0,
    0,
    0,
    transaction_timestamp(),
    sqlc.arg(created_by_subject_type),
    sqlc.arg(created_by_subject_id)
)
RETURNING *;

-- name: GetWorkspaceExecByClaim :one
SELECT *
  FROM workspace_processes
 WHERE environment_id = sqlc.arg(environment_id)
   AND workspace_id = sqlc.arg(workspace_id)
   AND claim_id = sqlc.arg(claim_id)
   AND kind = 'exec';

-- name: GetWorkspaceExec :one
SELECT *
  FROM workspace_processes
 WHERE org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND workspace_id = sqlc.arg(workspace_id)
   AND id = sqlc.arg(id)
   AND kind = 'exec';

-- name: ListWorkspaceExecTerminalRecords :many
SELECT *
  FROM workspace_process_records
 WHERE environment_id = sqlc.arg(environment_id)
   AND process_id = sqlc.arg(process_id)
   AND process_kind = 'exec'
   AND direction = 'output'
   AND stream IN ('stdout', 'stderr')
 ORDER BY stream, offset_start, id;
