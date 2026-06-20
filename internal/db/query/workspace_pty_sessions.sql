-- name: CreateWorkspacePtySession :one
INSERT INTO workspace_pty_sessions (
    id,
    org_id,
    project_id,
    environment_id,
    workspace_id,
    cwd,
    cols,
    rows,
    filesystem_mode,
    state,
    created_by_subject_type,
    created_by_subject_id
)
SELECT sqlc.arg(id),
       workspaces.org_id,
       workspaces.project_id,
       workspaces.environment_id,
       workspaces.id,
       coalesce(sqlc.arg(cwd)::text, ''),
       sqlc.arg(cols),
       sqlc.arg(rows),
       sqlc.arg(filesystem_mode)::workspace_filesystem_mode,
       sqlc.arg(state)::workspace_pty_state,
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

-- name: GetWorkspacePtySession :one
SELECT *
  FROM workspace_pty_sessions
 WHERE org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND workspace_id = sqlc.arg(workspace_id)
   AND id = sqlc.arg(id);

-- name: ListWorkspacePtySessions :many
SELECT *
  FROM workspace_pty_sessions
 WHERE org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND workspace_id = sqlc.arg(workspace_id)
   AND (sqlc.narg(state)::workspace_pty_state IS NULL OR state = sqlc.narg(state)::workspace_pty_state)
 ORDER BY created_at DESC, id DESC
 LIMIT sqlc.arg(limit_count);

-- name: BindWorkspacePtyMaterialization :one
UPDATE workspace_pty_sessions
   SET materialization_id = sqlc.arg(materialization_id),
       instance_lease_id = sqlc.narg(instance_lease_id),
       write_lease_id = sqlc.narg(write_lease_id),
       state = sqlc.arg(state)::workspace_pty_state,
       updated_at = now()
 WHERE org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND workspace_id = sqlc.arg(workspace_id)
   AND id = sqlc.arg(id)
   AND state IN ('creating')
RETURNING *;

-- name: MarkWorkspacePtyOpen :one
UPDATE workspace_pty_sessions
   SET state = 'open',
       process_id = sqlc.arg(process_id),
       started_at = coalesce(started_at, now()),
       updated_at = now()
 WHERE org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND workspace_id = sqlc.arg(workspace_id)
   AND id = sqlc.arg(id)
   AND state IN ('creating', 'open')
RETURNING *;

-- name: ResizeWorkspacePtySession :one
UPDATE workspace_pty_sessions
   SET cols = sqlc.arg(cols),
       rows = sqlc.arg(rows),
       state = CASE WHEN state = 'open' THEN 'resizing'::workspace_pty_state ELSE state END,
       updated_at = now()
 WHERE org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND workspace_id = sqlc.arg(workspace_id)
   AND id = sqlc.arg(id)
   AND state IN ('creating', 'open', 'resizing')
RETURNING *;

-- name: MarkWorkspacePtyResizeApplied :one
UPDATE workspace_pty_sessions
   SET state = 'open',
       updated_at = now()
 WHERE org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND workspace_id = sqlc.arg(workspace_id)
   AND id = sqlc.arg(id)
   AND state = 'resizing'
RETURNING *;

-- name: RequestWorkspacePtyClose :one
UPDATE workspace_pty_sessions
   SET state = 'closing',
       updated_at = now()
 WHERE org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND workspace_id = sqlc.arg(workspace_id)
   AND id = sqlc.arg(id)
   AND state IN ('creating', 'open', 'resizing')
RETURNING *;

-- name: MarkWorkspacePtyClosed :one
UPDATE workspace_pty_sessions
   SET state = 'closed',
       closed_at = coalesce(closed_at, now()),
       updated_at = now()
 WHERE org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND workspace_id = sqlc.arg(workspace_id)
   AND id = sqlc.arg(id)
   AND state IN ('creating', 'open', 'resizing', 'closing')
RETURNING *;

-- name: MarkWorkspacePtySessionsLostForMaterialization :many
UPDATE workspace_pty_sessions
   SET state = 'lost',
       error = jsonb_build_object('kind', 'workspace_materialization_lost'),
       closed_at = coalesce(closed_at, now()),
       updated_at = now()
 WHERE org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND workspace_id = sqlc.arg(workspace_id)
   AND materialization_id = sqlc.arg(materialization_id)
   AND state IN ('creating', 'open', 'resizing', 'closing')
RETURNING *;

-- name: LockWorkspacePtyForStreamAppend :one
SELECT id,
       org_id,
       project_id,
       environment_id,
       workspace_id,
       input_cursor,
       output_cursor,
       state
  FROM workspace_pty_sessions
 WHERE org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND workspace_id = sqlc.arg(workspace_id)
   AND id = sqlc.arg(pty_session_id)
 FOR UPDATE;

-- name: InsertWorkspacePtyStreamChunk :one
INSERT INTO workspace_pty_stream_chunks (
    org_id,
    project_id,
    environment_id,
    workspace_id,
    pty_session_id,
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
    sqlc.arg(pty_session_id),
    sqlc.arg(stream)::workspace_pty_stream,
    sqlc.arg(offset_start),
    sqlc.arg(offset_end),
    sqlc.arg(data),
    coalesce(sqlc.narg(observed_at), now())
)
RETURNING *;

-- name: AdvanceWorkspacePtyStreamCursor :one
UPDATE workspace_pty_sessions
   SET input_cursor = CASE WHEN sqlc.arg(stream)::workspace_pty_stream = 'input' THEN sqlc.arg(offset_end) ELSE input_cursor END,
       output_cursor = CASE WHEN sqlc.arg(stream)::workspace_pty_stream = 'output' THEN sqlc.arg(offset_end) ELSE output_cursor END,
       updated_at = now()
 WHERE org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND workspace_id = sqlc.arg(workspace_id)
   AND id = sqlc.arg(pty_session_id)
   AND sqlc.arg(offset_start) = CASE sqlc.arg(stream)::workspace_pty_stream
         WHEN 'input' THEN input_cursor
         WHEN 'output' THEN output_cursor
       END
RETURNING *;

-- name: GetWorkspacePtyStreamChunkAtOffset :one
SELECT *
  FROM workspace_pty_stream_chunks
 WHERE org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND workspace_id = sqlc.arg(workspace_id)
   AND pty_session_id = sqlc.arg(pty_session_id)
   AND stream = sqlc.arg(stream)::workspace_pty_stream
   AND offset_start = sqlc.arg(offset_start);

-- name: ListWorkspacePtyStreamChunksAfter :many
SELECT *
  FROM workspace_pty_stream_chunks
 WHERE org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND workspace_id = sqlc.arg(workspace_id)
   AND pty_session_id = sqlc.arg(pty_session_id)
   AND stream = sqlc.arg(stream)::workspace_pty_stream
   AND offset_end > sqlc.arg(cursor_offset)
 ORDER BY offset_start ASC
 LIMIT sqlc.arg(limit_count);

-- name: GetWorkspacePtyStreamBounds :one
SELECT coalesce(min(offset_start), 0)::bigint AS earliest_offset,
       coalesce(max(offset_end), 0)::bigint AS latest_offset
  FROM workspace_pty_stream_chunks
 WHERE org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND workspace_id = sqlc.arg(workspace_id)
   AND pty_session_id = sqlc.arg(pty_session_id)
   AND stream = sqlc.arg(stream)::workspace_pty_stream;
