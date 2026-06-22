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

-- name: LockWorkspacePrimitiveWriterScope :one
SELECT id
  FROM workspaces
 WHERE org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND id = sqlc.arg(workspace_id)
   AND state = 'active'
   AND archived_at IS NULL
   AND deleted_at IS NULL
 FOR UPDATE;

-- name: WorkspaceHasActivePrimitiveWriter :one
SELECT EXISTS (
    SELECT 1
      FROM workspace_execs
     WHERE workspace_execs.org_id = sqlc.arg(org_id)
       AND workspace_execs.project_id = sqlc.arg(project_id)
       AND workspace_execs.environment_id = sqlc.arg(environment_id)
       AND workspace_execs.workspace_id = sqlc.arg(workspace_id)
       AND workspace_execs.filesystem_mode = 'write'
       AND workspace_execs.state NOT IN ('exited', 'terminated', 'lost', 'failed')
    UNION ALL
    SELECT 1
      FROM workspace_pty_sessions
     WHERE workspace_pty_sessions.org_id = sqlc.arg(org_id)
       AND workspace_pty_sessions.project_id = sqlc.arg(project_id)
       AND workspace_pty_sessions.environment_id = sqlc.arg(environment_id)
       AND workspace_pty_sessions.workspace_id = sqlc.arg(workspace_id)
       AND workspace_pty_sessions.filesystem_mode = 'write'
       AND workspace_pty_sessions.state NOT IN ('closed', 'lost', 'failed')
    UNION ALL
    SELECT 1
      FROM workspace_leases
     WHERE workspace_leases.org_id = sqlc.arg(org_id)
       AND workspace_leases.project_id = sqlc.arg(project_id)
       AND workspace_leases.environment_id = sqlc.arg(environment_id)
       AND workspace_leases.workspace_id = sqlc.arg(workspace_id)
       AND workspace_leases.lease_kind = 'write'
       AND workspace_leases.state IN ('active', 'releasing')
);

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

-- name: ListWorkspaceExecsAwaitingDispatch :many
SELECT workspace_execs.*
  FROM workspace_execs
  JOIN workspace_materializations
    ON workspace_materializations.org_id = workspace_execs.org_id
   AND workspace_materializations.project_id = workspace_execs.project_id
   AND workspace_materializations.environment_id = workspace_execs.environment_id
   AND workspace_materializations.workspace_id = workspace_execs.workspace_id
   AND workspace_materializations.id = workspace_execs.materialization_id
 WHERE workspace_execs.org_id = sqlc.arg(org_id)
   AND workspace_execs.project_id = sqlc.arg(project_id)
   AND workspace_execs.environment_id = sqlc.arg(environment_id)
   AND workspace_execs.workspace_id = sqlc.arg(workspace_id)
   AND workspace_execs.materialization_id = sqlc.arg(materialization_id)
   AND workspace_execs.state IN ('materializing', 'queued')
   AND workspace_materializations.state = 'running'
 ORDER BY workspace_execs.created_at ASC, workspace_execs.id ASC
 LIMIT sqlc.arg(limit_count);

-- name: MarkWorkspaceExecStarted :one
WITH target AS MATERIALIZED (
    SELECT workspace_execs.*
      FROM workspace_execs
      JOIN workspace_materializations
        ON workspace_materializations.org_id = workspace_execs.org_id
       AND workspace_materializations.project_id = workspace_execs.project_id
       AND workspace_materializations.environment_id = workspace_execs.environment_id
       AND workspace_materializations.workspace_id = workspace_execs.workspace_id
       AND workspace_materializations.id = workspace_execs.materialization_id
     WHERE workspace_execs.org_id = sqlc.arg(org_id)
       AND workspace_execs.project_id = sqlc.arg(project_id)
       AND workspace_execs.environment_id = sqlc.arg(environment_id)
       AND workspace_execs.workspace_id = sqlc.arg(workspace_id)
       AND workspace_execs.id = sqlc.arg(id)
       AND workspace_execs.materialization_id = sqlc.arg(materialization_id)
       AND workspace_execs.state IN ('queued', 'materializing', 'running')
       AND workspace_materializations.state = 'running'
     FOR UPDATE OF workspace_execs, workspace_materializations
),
updated_exec AS (
    UPDATE workspace_execs
       SET state = 'running',
           process_id = sqlc.arg(process_id),
           started_at = coalesce(workspace_execs.started_at, now()),
           updated_at = now()
      FROM target
     WHERE workspace_execs.org_id = target.org_id
       AND workspace_execs.project_id = target.project_id
       AND workspace_execs.environment_id = target.environment_id
       AND workspace_execs.workspace_id = target.workspace_id
       AND workspace_execs.id = target.id
    RETURNING workspace_execs.*
),
dirtied_materialization AS (
    UPDATE workspace_materializations
       SET dirty_generation = workspace_materializations.dirty_generation + 1,
           updated_at = now()
      FROM target
      JOIN updated_exec ON updated_exec.id = target.id
      JOIN workspace_leases
        ON workspace_leases.org_id = updated_exec.org_id
       AND workspace_leases.project_id = updated_exec.project_id
       AND workspace_leases.environment_id = updated_exec.environment_id
       AND workspace_leases.workspace_id = updated_exec.workspace_id
       AND workspace_leases.id = updated_exec.write_lease_id
       AND workspace_leases.owner_exec_id = updated_exec.id
       AND workspace_leases.lease_kind = 'write'
       AND workspace_leases.state = 'active'
     WHERE target.state <> 'running'
       AND updated_exec.filesystem_mode = 'write'
       AND workspace_materializations.org_id = updated_exec.org_id
       AND workspace_materializations.project_id = updated_exec.project_id
       AND workspace_materializations.environment_id = updated_exec.environment_id
       AND workspace_materializations.workspace_id = updated_exec.workspace_id
       AND workspace_materializations.id = updated_exec.materialization_id
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
  FROM updated_exec
 WHERE (SELECT count(*) FROM updated_workspace) >= 0;

-- name: CloseWorkspaceExecStdin :one
UPDATE workspace_execs
   SET stdin_closed_at = coalesce(workspace_execs.stdin_closed_at, now()),
       updated_at = now()
 WHERE org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND workspace_id = sqlc.arg(workspace_id)
   AND id = sqlc.arg(id)
   AND state IN ('queued', 'materializing', 'running')
RETURNING *;

-- name: MarkWorkspaceExecExited :one
WITH updated_exec AS (
    UPDATE workspace_execs
       SET state = sqlc.arg(state)::workspace_exec_state,
           exit_code = sqlc.narg(exit_code),
           signal = coalesce(sqlc.arg(signal)::text, workspace_execs.signal),
           error = coalesce(sqlc.arg(error)::jsonb, '{}'::jsonb),
           exited_at = coalesce(workspace_execs.exited_at, now()),
           updated_at = now()
     WHERE workspace_execs.org_id = sqlc.arg(org_id)
       AND workspace_execs.project_id = sqlc.arg(project_id)
       AND workspace_execs.environment_id = sqlc.arg(environment_id)
       AND workspace_execs.workspace_id = sqlc.arg(workspace_id)
       AND workspace_execs.id = sqlc.arg(id)
       AND workspace_execs.materialization_id = sqlc.arg(materialization_id)
       AND workspace_execs.state IN ('queued', 'materializing', 'running')
    RETURNING *
),
released_write_lease AS (
    UPDATE workspace_leases
       SET state = 'released',
           released_at = now(),
           updated_at = now()
      FROM updated_exec
     WHERE workspace_leases.org_id = updated_exec.org_id
       AND workspace_leases.project_id = updated_exec.project_id
       AND workspace_leases.environment_id = updated_exec.environment_id
       AND workspace_leases.workspace_id = updated_exec.workspace_id
       AND workspace_leases.id = updated_exec.write_lease_id
       AND workspace_leases.owner_exec_id = updated_exec.id
       AND workspace_leases.lease_kind = 'write'
       AND workspace_leases.state IN ('active', 'releasing')
    RETURNING workspace_leases.id
),
stream_wakeups AS (
    INSERT INTO workspace_stream_wakeups (org_id, project_id, environment_id, workspace_id, resource_kind, resource_id, stream, cursor_offset, notification_kind)
    SELECT updated_exec.org_id,
           updated_exec.project_id,
           updated_exec.environment_id,
           updated_exec.workspace_id,
           'workspace_exec'::workspace_resource_kind,
           updated_exec.id,
           stream_names.stream,
           stream_names.cursor_offset,
           'terminal'::workspace_stream_notification_kind
      FROM updated_exec
      CROSS JOIN LATERAL (VALUES ('stdout', updated_exec.stdout_cursor), ('stderr', updated_exec.stderr_cursor)) AS stream_names(stream, cursor_offset)
    RETURNING id
)
SELECT *
  FROM updated_exec
 WHERE (SELECT count(*) FROM stream_wakeups) >= 0;

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

-- name: InsertWorkspaceExecOutputStreamChunk :one
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
ON CONFLICT DO NOTHING
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

-- name: AdvanceWorkspaceExecOutputCursor :one
WITH RECURSIVE current_cursor AS (
    SELECT CASE sqlc.arg(stream)::workspace_exec_stream
             WHEN 'stdout' THEN stdout_cursor
             WHEN 'stderr' THEN stderr_cursor
             ELSE -1
           END AS cursor
      FROM workspace_execs
     WHERE org_id = sqlc.arg(org_id)
       AND project_id = sqlc.arg(project_id)
       AND environment_id = sqlc.arg(environment_id)
       AND workspace_id = sqlc.arg(workspace_id)
       AND id = sqlc.arg(exec_id)
     FOR UPDATE
),
contiguous(end_offset) AS (
    SELECT cursor FROM current_cursor
    UNION
    SELECT chunks.offset_end
      FROM contiguous
      JOIN workspace_exec_stream_chunks AS chunks
        ON chunks.org_id = sqlc.arg(org_id)
       AND chunks.project_id = sqlc.arg(project_id)
       AND chunks.environment_id = sqlc.arg(environment_id)
       AND chunks.workspace_id = sqlc.arg(workspace_id)
       AND chunks.exec_id = sqlc.arg(exec_id)
       AND chunks.stream = sqlc.arg(stream)::workspace_exec_stream
       AND chunks.offset_start = contiguous.end_offset
),
advanced AS (
    SELECT max(end_offset)::bigint AS cursor FROM contiguous
)
UPDATE workspace_execs
   SET stdout_cursor = CASE WHEN sqlc.arg(stream)::workspace_exec_stream = 'stdout' THEN advanced.cursor ELSE stdout_cursor END,
       stderr_cursor = CASE WHEN sqlc.arg(stream)::workspace_exec_stream = 'stderr' THEN advanced.cursor ELSE stderr_cursor END,
       updated_at = now()
  FROM advanced
 WHERE workspace_execs.org_id = sqlc.arg(org_id)
   AND workspace_execs.project_id = sqlc.arg(project_id)
   AND workspace_execs.environment_id = sqlc.arg(environment_id)
   AND workspace_execs.workspace_id = sqlc.arg(workspace_id)
   AND workspace_execs.id = sqlc.arg(exec_id)
   AND sqlc.arg(stream)::workspace_exec_stream IN ('stdout', 'stderr')
RETURNING workspace_execs.*;

-- name: DeleteWorkspaceExecStreamChunksBefore :exec
DELETE FROM workspace_exec_stream_chunks
 WHERE org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND workspace_id = sqlc.arg(workspace_id)
   AND exec_id = sqlc.arg(exec_id)
   AND stream = sqlc.arg(stream)::workspace_exec_stream
   AND offset_end <= sqlc.arg(retain_after_offset);

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

-- name: InsertWorkspaceExecStreamChunkReceipt :one
INSERT INTO workspace_exec_stream_chunk_receipts (
    org_id,
    project_id,
    environment_id,
    workspace_id,
    exec_id,
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
    sqlc.arg(exec_id),
    sqlc.arg(stream)::workspace_exec_stream,
    sqlc.arg(offset_start),
    sqlc.arg(offset_end),
    sqlc.arg(data_sha256),
    sqlc.arg(data_size),
    coalesce(sqlc.narg(observed_at), now())
)
ON CONFLICT DO NOTHING
RETURNING *;

-- name: GetWorkspaceExecStreamChunkReceiptAtOffset :one
SELECT *
  FROM workspace_exec_stream_chunk_receipts
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

-- name: ListWorkspaceExecStdinChunksAfterDelivered :many
SELECT chunks.*
  FROM workspace_execs
  JOIN workspace_exec_stream_chunks AS chunks
    ON chunks.org_id = workspace_execs.org_id
   AND chunks.project_id = workspace_execs.project_id
   AND chunks.environment_id = workspace_execs.environment_id
   AND chunks.workspace_id = workspace_execs.workspace_id
   AND chunks.exec_id = workspace_execs.id
   AND chunks.stream = 'stdin'
 WHERE workspace_execs.org_id = sqlc.arg(org_id)
   AND workspace_execs.project_id = sqlc.arg(project_id)
   AND workspace_execs.environment_id = sqlc.arg(environment_id)
   AND workspace_execs.workspace_id = sqlc.arg(workspace_id)
   AND workspace_execs.id = sqlc.arg(exec_id)
   AND chunks.offset_end > workspace_execs.stdin_delivered_cursor
 ORDER BY chunks.offset_start ASC
 LIMIT sqlc.arg(limit_count);

-- name: AdvanceWorkspaceExecStdinDeliveredCursor :one
UPDATE workspace_execs
   SET stdin_delivered_cursor = sqlc.arg(offset_end),
       updated_at = now()
 WHERE workspace_execs.org_id = sqlc.arg(org_id)
   AND workspace_execs.project_id = sqlc.arg(project_id)
   AND workspace_execs.environment_id = sqlc.arg(environment_id)
   AND workspace_execs.workspace_id = sqlc.arg(workspace_id)
   AND workspace_execs.id = sqlc.arg(exec_id)
   AND sqlc.arg(offset_start) = workspace_execs.stdin_delivered_cursor
   AND sqlc.arg(offset_end) <= workspace_execs.stdin_cursor
   AND EXISTS (
       SELECT 1
         FROM workspace_exec_stream_chunks AS chunks
        WHERE chunks.org_id = workspace_execs.org_id
          AND chunks.project_id = workspace_execs.project_id
          AND chunks.environment_id = workspace_execs.environment_id
          AND chunks.workspace_id = workspace_execs.workspace_id
          AND chunks.exec_id = workspace_execs.id
          AND chunks.stream = 'stdin'
          AND chunks.offset_start = sqlc.arg(offset_start)
          AND chunks.offset_end = sqlc.arg(offset_end)
   )
RETURNING *;
