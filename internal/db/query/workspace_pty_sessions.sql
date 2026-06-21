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

-- name: ListWorkspacePtySessionsAwaitingDispatch :many
SELECT workspace_pty_sessions.*
  FROM workspace_pty_sessions
  JOIN workspace_materializations
    ON workspace_materializations.org_id = workspace_pty_sessions.org_id
   AND workspace_materializations.project_id = workspace_pty_sessions.project_id
   AND workspace_materializations.environment_id = workspace_pty_sessions.environment_id
   AND workspace_materializations.workspace_id = workspace_pty_sessions.workspace_id
   AND workspace_materializations.id = workspace_pty_sessions.materialization_id
 WHERE workspace_pty_sessions.org_id = sqlc.arg(org_id)
   AND workspace_pty_sessions.project_id = sqlc.arg(project_id)
   AND workspace_pty_sessions.environment_id = sqlc.arg(environment_id)
   AND workspace_pty_sessions.workspace_id = sqlc.arg(workspace_id)
   AND workspace_pty_sessions.materialization_id = sqlc.arg(materialization_id)
   AND workspace_pty_sessions.state IN ('creating')
   AND workspace_materializations.state = 'running'
 ORDER BY workspace_pty_sessions.created_at ASC, workspace_pty_sessions.id ASC
 LIMIT sqlc.arg(limit_count);

-- name: MarkWorkspacePtyOpen :one
WITH target AS MATERIALIZED (
    SELECT workspace_pty_sessions.*
      FROM workspace_pty_sessions
      JOIN workspace_materializations
        ON workspace_materializations.org_id = workspace_pty_sessions.org_id
       AND workspace_materializations.project_id = workspace_pty_sessions.project_id
       AND workspace_materializations.environment_id = workspace_pty_sessions.environment_id
       AND workspace_materializations.workspace_id = workspace_pty_sessions.workspace_id
       AND workspace_materializations.id = workspace_pty_sessions.materialization_id
     WHERE workspace_pty_sessions.org_id = sqlc.arg(org_id)
       AND workspace_pty_sessions.project_id = sqlc.arg(project_id)
       AND workspace_pty_sessions.environment_id = sqlc.arg(environment_id)
       AND workspace_pty_sessions.workspace_id = sqlc.arg(workspace_id)
       AND workspace_pty_sessions.id = sqlc.arg(id)
       AND workspace_pty_sessions.materialization_id = sqlc.arg(materialization_id)
       AND workspace_pty_sessions.state IN ('creating', 'open')
       AND workspace_materializations.state = 'running'
     FOR UPDATE OF workspace_pty_sessions, workspace_materializations
),
updated_pty AS (
    UPDATE workspace_pty_sessions
       SET state = 'open',
           process_id = sqlc.arg(process_id),
           started_at = coalesce(workspace_pty_sessions.started_at, now()),
           updated_at = now()
      FROM target
     WHERE workspace_pty_sessions.org_id = target.org_id
       AND workspace_pty_sessions.project_id = target.project_id
       AND workspace_pty_sessions.environment_id = target.environment_id
       AND workspace_pty_sessions.workspace_id = target.workspace_id
       AND workspace_pty_sessions.id = target.id
    RETURNING workspace_pty_sessions.*
),
dirtied_materialization AS (
    UPDATE workspace_materializations
       SET dirty_generation = workspace_materializations.dirty_generation + 1,
           updated_at = now()
      FROM target
      JOIN updated_pty ON updated_pty.id = target.id
      JOIN workspace_leases
        ON workspace_leases.org_id = updated_pty.org_id
       AND workspace_leases.project_id = updated_pty.project_id
       AND workspace_leases.environment_id = updated_pty.environment_id
       AND workspace_leases.workspace_id = updated_pty.workspace_id
       AND workspace_leases.id = updated_pty.write_lease_id
       AND workspace_leases.owner_pty_session_id = updated_pty.id
       AND workspace_leases.lease_kind = 'write'
       AND workspace_leases.state = 'active'
     WHERE target.state <> 'open'
       AND updated_pty.filesystem_mode = 'write'
       AND workspace_materializations.org_id = updated_pty.org_id
       AND workspace_materializations.project_id = updated_pty.project_id
       AND workspace_materializations.environment_id = updated_pty.environment_id
       AND workspace_materializations.workspace_id = updated_pty.workspace_id
       AND workspace_materializations.id = updated_pty.materialization_id
       AND workspace_materializations.fencing_generation = workspace_leases.acquired_fencing_generation
    RETURNING workspace_materializations.*
),
updated_workspace AS (
    UPDATE workspaces
       SET dirty_state = 'dirty',
           updated_at = now()
      FROM dirtied_materialization
     WHERE workspaces.org_id = dirtied_materialization.org_id
       AND workspaces.project_id = dirtied_materialization.project_id
       AND workspaces.environment_id = dirtied_materialization.environment_id
       AND workspaces.id = dirtied_materialization.workspace_id
    RETURNING workspaces.id
)
SELECT *
  FROM updated_pty
 WHERE (SELECT count(*) FROM updated_workspace) >= 0;

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
   AND state IN ('open', 'resizing')
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
   AND materialization_id = sqlc.arg(materialization_id)
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
   AND state IN ('open', 'resizing', 'closing')
RETURNING *;

-- name: MarkWorkspacePtyClosed :one
WITH updated_pty AS (
    UPDATE workspace_pty_sessions
       SET state = 'closed',
           closed_at = coalesce(workspace_pty_sessions.closed_at, now()),
           updated_at = now()
     WHERE workspace_pty_sessions.org_id = sqlc.arg(org_id)
       AND workspace_pty_sessions.project_id = sqlc.arg(project_id)
       AND workspace_pty_sessions.environment_id = sqlc.arg(environment_id)
       AND workspace_pty_sessions.workspace_id = sqlc.arg(workspace_id)
       AND workspace_pty_sessions.id = sqlc.arg(id)
       AND workspace_pty_sessions.materialization_id = sqlc.arg(materialization_id)
       AND workspace_pty_sessions.state IN ('creating', 'open', 'resizing', 'closing')
    RETURNING *
),
released_write_lease AS (
    UPDATE workspace_leases
       SET state = 'released',
           released_at = now(),
           updated_at = now()
      FROM updated_pty
     WHERE workspace_leases.org_id = updated_pty.org_id
       AND workspace_leases.project_id = updated_pty.project_id
       AND workspace_leases.environment_id = updated_pty.environment_id
       AND workspace_leases.workspace_id = updated_pty.workspace_id
       AND workspace_leases.id = updated_pty.write_lease_id
       AND workspace_leases.owner_pty_session_id = updated_pty.id
       AND workspace_leases.lease_kind = 'write'
       AND workspace_leases.state IN ('active', 'releasing')
    RETURNING workspace_leases.id
),
stream_wakeups AS (
    INSERT INTO workspace_stream_wakeups (org_id, project_id, environment_id, workspace_id, resource_kind, resource_id, stream, cursor_offset, notification_kind)
    SELECT updated_pty.org_id,
           updated_pty.project_id,
           updated_pty.environment_id,
           updated_pty.workspace_id,
           'workspace_pty',
           updated_pty.id,
           'output',
           updated_pty.output_cursor,
           'terminal'
      FROM updated_pty
    RETURNING id
)
SELECT *
  FROM updated_pty
 WHERE (SELECT count(*) FROM stream_wakeups) >= 0;

-- name: MarkWorkspacePtyFailed :one
WITH updated_pty AS (
    UPDATE workspace_pty_sessions
       SET state = 'failed',
           error = coalesce(sqlc.arg(error)::jsonb, '{}'::jsonb),
           closed_at = coalesce(workspace_pty_sessions.closed_at, now()),
           updated_at = now()
     WHERE workspace_pty_sessions.org_id = sqlc.arg(org_id)
       AND workspace_pty_sessions.project_id = sqlc.arg(project_id)
       AND workspace_pty_sessions.environment_id = sqlc.arg(environment_id)
       AND workspace_pty_sessions.workspace_id = sqlc.arg(workspace_id)
       AND workspace_pty_sessions.id = sqlc.arg(id)
       AND workspace_pty_sessions.materialization_id = sqlc.arg(materialization_id)
       AND workspace_pty_sessions.state IN ('creating', 'open', 'resizing', 'closing')
    RETURNING *
),
released_write_lease AS (
    UPDATE workspace_leases
       SET state = 'released',
           released_at = now(),
           updated_at = now()
      FROM updated_pty
     WHERE workspace_leases.org_id = updated_pty.org_id
       AND workspace_leases.project_id = updated_pty.project_id
       AND workspace_leases.environment_id = updated_pty.environment_id
       AND workspace_leases.workspace_id = updated_pty.workspace_id
       AND workspace_leases.id = updated_pty.write_lease_id
       AND workspace_leases.owner_pty_session_id = updated_pty.id
       AND workspace_leases.lease_kind = 'write'
       AND workspace_leases.state IN ('active', 'releasing')
    RETURNING workspace_leases.id
),
stream_wakeups AS (
    INSERT INTO workspace_stream_wakeups (org_id, project_id, environment_id, workspace_id, resource_kind, resource_id, stream, cursor_offset, notification_kind)
    SELECT updated_pty.org_id,
           updated_pty.project_id,
           updated_pty.environment_id,
           updated_pty.workspace_id,
           'workspace_pty',
           updated_pty.id,
           'output',
           updated_pty.output_cursor,
           'terminal'
      FROM updated_pty
    RETURNING id
)
SELECT *
  FROM updated_pty
 WHERE (SELECT count(*) FROM stream_wakeups) >= 0;

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

-- name: InsertWorkspacePtyOutputStreamChunk :one
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
ON CONFLICT DO NOTHING
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

-- name: AdvanceWorkspacePtyOutputCursor :one
WITH RECURSIVE current_cursor AS (
    SELECT CASE sqlc.arg(stream)::workspace_pty_stream
             WHEN 'output' THEN output_cursor
             ELSE -1
           END AS cursor
      FROM workspace_pty_sessions
     WHERE org_id = sqlc.arg(org_id)
       AND project_id = sqlc.arg(project_id)
       AND environment_id = sqlc.arg(environment_id)
       AND workspace_id = sqlc.arg(workspace_id)
       AND id = sqlc.arg(pty_session_id)
     FOR UPDATE
),
contiguous(end_offset) AS (
    SELECT cursor FROM current_cursor
    UNION
    SELECT chunks.offset_end
      FROM contiguous
      JOIN workspace_pty_stream_chunks AS chunks
        ON chunks.org_id = sqlc.arg(org_id)
       AND chunks.project_id = sqlc.arg(project_id)
       AND chunks.environment_id = sqlc.arg(environment_id)
       AND chunks.workspace_id = sqlc.arg(workspace_id)
       AND chunks.pty_session_id = sqlc.arg(pty_session_id)
       AND chunks.stream = sqlc.arg(stream)::workspace_pty_stream
       AND chunks.offset_start = contiguous.end_offset
),
advanced AS (
    SELECT max(end_offset)::bigint AS cursor FROM contiguous
)
UPDATE workspace_pty_sessions
   SET output_cursor = CASE WHEN sqlc.arg(stream)::workspace_pty_stream = 'output' THEN advanced.cursor ELSE output_cursor END,
       updated_at = now()
  FROM advanced
 WHERE workspace_pty_sessions.org_id = sqlc.arg(org_id)
   AND workspace_pty_sessions.project_id = sqlc.arg(project_id)
   AND workspace_pty_sessions.environment_id = sqlc.arg(environment_id)
   AND workspace_pty_sessions.workspace_id = sqlc.arg(workspace_id)
   AND workspace_pty_sessions.id = sqlc.arg(pty_session_id)
   AND sqlc.arg(stream)::workspace_pty_stream = 'output'
RETURNING workspace_pty_sessions.*;

-- name: DeleteWorkspacePtyStreamChunksBefore :exec
DELETE FROM workspace_pty_stream_chunks
 WHERE org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND workspace_id = sqlc.arg(workspace_id)
   AND pty_session_id = sqlc.arg(pty_session_id)
   AND stream = sqlc.arg(stream)::workspace_pty_stream
   AND offset_end <= sqlc.arg(retain_after_offset);

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

-- name: InsertWorkspacePtyStreamChunkReceipt :one
INSERT INTO workspace_pty_stream_chunk_receipts (
    org_id,
    project_id,
    environment_id,
    workspace_id,
    pty_session_id,
    stream,
    offset_start,
    offset_end,
    data_sha256,
    data_size,
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
    sqlc.arg(data_sha256),
    sqlc.arg(data_size),
    coalesce(sqlc.narg(observed_at), now())
)
ON CONFLICT DO NOTHING
RETURNING *;

-- name: GetWorkspacePtyStreamChunkReceiptAtOffset :one
SELECT *
  FROM workspace_pty_stream_chunk_receipts
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

-- name: ListWorkspacePtyInputChunksAfterDelivered :many
SELECT chunks.*
  FROM workspace_pty_sessions
  JOIN workspace_pty_stream_chunks AS chunks
    ON chunks.org_id = workspace_pty_sessions.org_id
   AND chunks.project_id = workspace_pty_sessions.project_id
   AND chunks.environment_id = workspace_pty_sessions.environment_id
   AND chunks.workspace_id = workspace_pty_sessions.workspace_id
   AND chunks.pty_session_id = workspace_pty_sessions.id
   AND chunks.stream = 'input'
 WHERE workspace_pty_sessions.org_id = sqlc.arg(org_id)
   AND workspace_pty_sessions.project_id = sqlc.arg(project_id)
   AND workspace_pty_sessions.environment_id = sqlc.arg(environment_id)
   AND workspace_pty_sessions.workspace_id = sqlc.arg(workspace_id)
   AND workspace_pty_sessions.id = sqlc.arg(pty_session_id)
   AND chunks.offset_end > workspace_pty_sessions.input_delivered_cursor
 ORDER BY chunks.offset_start ASC
 LIMIT sqlc.arg(limit_count);

-- name: AdvanceWorkspacePtyInputDeliveredCursor :one
UPDATE workspace_pty_sessions
   SET input_delivered_cursor = sqlc.arg(offset_end),
       updated_at = now()
 WHERE workspace_pty_sessions.org_id = sqlc.arg(org_id)
   AND workspace_pty_sessions.project_id = sqlc.arg(project_id)
   AND workspace_pty_sessions.environment_id = sqlc.arg(environment_id)
   AND workspace_pty_sessions.workspace_id = sqlc.arg(workspace_id)
   AND workspace_pty_sessions.id = sqlc.arg(pty_session_id)
   AND sqlc.arg(offset_start) = workspace_pty_sessions.input_delivered_cursor
   AND sqlc.arg(offset_end) <= workspace_pty_sessions.input_cursor
   AND EXISTS (
       SELECT 1
         FROM workspace_pty_stream_chunks AS chunks
        WHERE chunks.org_id = workspace_pty_sessions.org_id
          AND chunks.project_id = workspace_pty_sessions.project_id
          AND chunks.environment_id = workspace_pty_sessions.environment_id
          AND chunks.workspace_id = workspace_pty_sessions.workspace_id
          AND chunks.pty_session_id = workspace_pty_sessions.id
          AND chunks.stream = 'input'
          AND chunks.offset_start = sqlc.arg(offset_start)
          AND chunks.offset_end = sqlc.arg(offset_end)
   )
RETURNING *;
