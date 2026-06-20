-- name: CreateWorkspaceExec :one
INSERT INTO workspace_execs (
    id,
    org_id,
    project_id,
    environment_id,
    workspace_id,
    command,
    cwd,
    env_shape,
    filesystem_mode,
    state,
    detached,
    idempotency_key,
    request_fingerprint,
    created_by_subject_type,
    created_by_subject_id
)
SELECT sqlc.arg(id),
       workspaces.org_id,
       workspaces.project_id,
       workspaces.environment_id,
       workspaces.id,
       sqlc.arg(command)::jsonb,
       coalesce(sqlc.arg(cwd)::text, ''),
       coalesce(sqlc.arg(env_shape)::jsonb, '{}'::jsonb),
       sqlc.arg(filesystem_mode)::workspace_filesystem_mode,
       sqlc.arg(state)::workspace_exec_state,
       sqlc.arg(detached),
       coalesce(sqlc.arg(idempotency_key)::text, ''),
       coalesce(sqlc.arg(request_fingerprint)::text, ''),
       coalesce(sqlc.arg(created_by_subject_type)::text, ''),
       coalesce(sqlc.arg(created_by_subject_id)::text, '')
  FROM workspaces
 WHERE workspaces.org_id = sqlc.arg(org_id)
   AND workspaces.project_id = sqlc.arg(project_id)
   AND workspaces.environment_id = sqlc.arg(environment_id)
   AND workspaces.id = sqlc.arg(workspace_id)
   AND workspaces.state = 'active'
   AND workspaces.archived_at IS NULL
   AND workspaces.deleted_at IS NULL
RETURNING *;

-- name: GetWorkspaceExec :one
SELECT *
  FROM workspace_execs
 WHERE org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND workspace_id = sqlc.arg(workspace_id)
   AND id = sqlc.arg(id);

-- name: ListWorkspaceExecs :many
SELECT *
  FROM workspace_execs
 WHERE org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND workspace_id = sqlc.arg(workspace_id)
   AND (sqlc.narg(state)::workspace_exec_state IS NULL OR state = sqlc.narg(state)::workspace_exec_state)
 ORDER BY created_at DESC, id DESC
 LIMIT sqlc.arg(limit_count);

-- name: BindWorkspaceExecMaterialization :one
UPDATE workspace_execs
   SET materialization_id = sqlc.arg(materialization_id),
       instance_lease_id = sqlc.narg(instance_lease_id),
       write_lease_id = sqlc.narg(write_lease_id),
       state = sqlc.arg(state)::workspace_exec_state,
       updated_at = now()
 WHERE org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND workspace_id = sqlc.arg(workspace_id)
   AND id = sqlc.arg(id)
   AND state IN ('queued', 'materializing')
RETURNING *;

-- name: MarkWorkspaceExecStarted :one
UPDATE workspace_execs
   SET state = 'running',
       process_id = sqlc.arg(process_id),
       started_at = coalesce(started_at, now()),
       updated_at = now()
 WHERE org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND workspace_id = sqlc.arg(workspace_id)
   AND id = sqlc.arg(id)
   AND state IN ('queued', 'materializing', 'running')
RETURNING *;

-- name: CloseWorkspaceExecStdin :one
UPDATE workspace_execs
   SET stdin_closed_at = coalesce(stdin_closed_at, now()),
       updated_at = now()
 WHERE org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND workspace_id = sqlc.arg(workspace_id)
   AND id = sqlc.arg(id)
   AND state IN ('queued', 'materializing', 'running')
RETURNING *;

-- name: RequestWorkspaceExecKill :one
UPDATE workspace_execs
   SET state = 'kill_requested',
       signal = coalesce(sqlc.arg(signal)::text, ''),
       updated_at = now()
 WHERE org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND workspace_id = sqlc.arg(workspace_id)
   AND id = sqlc.arg(id)
   AND state IN ('queued', 'materializing', 'running')
RETURNING *;

-- name: MarkWorkspaceExecExited :one
UPDATE workspace_execs
   SET state = sqlc.arg(state)::workspace_exec_state,
       exit_code = sqlc.narg(exit_code),
       signal = coalesce(sqlc.arg(signal)::text, workspace_execs.signal),
       error = coalesce(sqlc.arg(error)::jsonb, '{}'::jsonb),
       exited_at = coalesce(exited_at, now()),
       updated_at = now()
 WHERE org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND workspace_id = sqlc.arg(workspace_id)
   AND id = sqlc.arg(id)
   AND state IN ('queued', 'materializing', 'running', 'kill_requested')
RETURNING *;

-- name: MarkWorkspaceExecsLostForMaterialization :many
UPDATE workspace_execs
   SET state = 'lost',
       error = jsonb_build_object('kind', 'workspace_materialization_lost'),
       exited_at = coalesce(exited_at, now()),
       updated_at = now()
 WHERE org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND workspace_id = sqlc.arg(workspace_id)
   AND materialization_id = sqlc.arg(materialization_id)
   AND state IN ('queued', 'materializing', 'running', 'kill_requested')
RETURNING *;

-- name: LockWorkspaceExecForStreamAppend :one
SELECT id,
       org_id,
       project_id,
       environment_id,
       workspace_id,
       stdin_cursor,
       stdout_cursor,
       stderr_cursor,
       stdin_closed_at,
       state
  FROM workspace_execs
 WHERE org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND workspace_id = sqlc.arg(workspace_id)
   AND id = sqlc.arg(exec_id)
 FOR UPDATE;

-- name: InsertWorkspaceExecStreamChunk :one
INSERT INTO workspace_exec_stream_chunks (
    org_id,
    project_id,
    environment_id,
    workspace_id,
    exec_id,
    stream,
    offset_start,
    offset_end,
    data,
    observed_at
) VALUES (
    sqlc.arg(org_id),
    sqlc.arg(project_id),
    sqlc.arg(environment_id),
    sqlc.arg(workspace_id),
    sqlc.arg(exec_id),
    sqlc.arg(stream)::workspace_exec_stream,
    sqlc.arg(offset_start),
    sqlc.arg(offset_end),
    sqlc.arg(data),
    coalesce(sqlc.narg(observed_at), now())
)
RETURNING *;

-- name: AdvanceWorkspaceExecStreamCursor :one
UPDATE workspace_execs
   SET stdin_cursor = CASE WHEN sqlc.arg(stream)::workspace_exec_stream = 'stdin' THEN sqlc.arg(offset_end) ELSE stdin_cursor END,
       stdout_cursor = CASE WHEN sqlc.arg(stream)::workspace_exec_stream = 'stdout' THEN sqlc.arg(offset_end) ELSE stdout_cursor END,
       stderr_cursor = CASE WHEN sqlc.arg(stream)::workspace_exec_stream = 'stderr' THEN sqlc.arg(offset_end) ELSE stderr_cursor END,
       updated_at = now()
 WHERE org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND workspace_id = sqlc.arg(workspace_id)
   AND id = sqlc.arg(exec_id)
   AND sqlc.arg(offset_start) = CASE sqlc.arg(stream)::workspace_exec_stream
         WHEN 'stdin' THEN stdin_cursor
         WHEN 'stdout' THEN stdout_cursor
         WHEN 'stderr' THEN stderr_cursor
       END
RETURNING *;

-- name: GetWorkspaceExecStreamChunkAtOffset :one
SELECT *
  FROM workspace_exec_stream_chunks
 WHERE org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND workspace_id = sqlc.arg(workspace_id)
   AND exec_id = sqlc.arg(exec_id)
   AND stream = sqlc.arg(stream)::workspace_exec_stream
   AND offset_start = sqlc.arg(offset_start);

-- name: ListWorkspaceExecStreamChunksAfter :many
SELECT *
  FROM workspace_exec_stream_chunks
 WHERE org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND workspace_id = sqlc.arg(workspace_id)
   AND exec_id = sqlc.arg(exec_id)
   AND stream = sqlc.arg(stream)::workspace_exec_stream
   AND offset_end > sqlc.arg(cursor_offset)
 ORDER BY offset_start ASC
 LIMIT sqlc.arg(limit_count);

-- name: GetWorkspaceExecStreamBounds :one
SELECT coalesce(min(offset_start), 0)::bigint AS earliest_offset,
       coalesce(max(offset_end), 0)::bigint AS latest_offset
  FROM workspace_exec_stream_chunks
 WHERE org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND workspace_id = sqlc.arg(workspace_id)
   AND exec_id = sqlc.arg(exec_id)
   AND stream = sqlc.arg(stream)::workspace_exec_stream;
