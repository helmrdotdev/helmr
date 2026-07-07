-- name: CreateWorkspaceExec :one
INSERT INTO workspace_processes (
    id,
    org_id,
    worker_group_id,
    project_id,
    environment_id,
    workspace_id,
    kind,
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
       workspaces.worker_group_id,
       workspaces.project_id,
       workspaces.environment_id,
       workspaces.id,
       'command',
       sqlc.arg(command)::jsonb,
       coalesce(sqlc.arg(cwd)::text, ''),
       coalesce(sqlc.arg(env_shape)::jsonb, '{}'::jsonb),
       sqlc.arg(filesystem_mode)::workspace_filesystem_mode,
       sqlc.arg(state)::workspace_process_state,
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
  FROM workspace_processes
 WHERE org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND workspace_id = sqlc.arg(workspace_id)
   AND id = sqlc.arg(id)
   AND kind = 'command';

-- name: ListWorkspaceExecs :many
SELECT *
  FROM workspace_processes
 WHERE org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND workspace_id = sqlc.arg(workspace_id)
   AND kind = 'command'
   AND (sqlc.narg(state)::workspace_process_state IS NULL OR state = sqlc.narg(state)::workspace_process_state)
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
      FROM workspace_processes
     WHERE workspace_processes.org_id = sqlc.arg(org_id)
       AND workspace_processes.project_id = sqlc.arg(project_id)
       AND workspace_processes.environment_id = sqlc.arg(environment_id)
       AND workspace_processes.workspace_id = sqlc.arg(workspace_id)
       AND workspace_processes.filesystem_mode = 'write'
       AND workspace_processes.state NOT IN ('exited', 'lost', 'failed')
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

-- name: BindWorkspaceExecWorkspaceMount :one
UPDATE workspace_processes
   SET workspace_mount_id = sqlc.arg(workspace_mount_id),
       instance_lease_id = sqlc.narg(instance_lease_id),
       write_lease_id = sqlc.narg(write_lease_id),
       state = sqlc.arg(state)::workspace_process_state,
       updated_at = now()
 WHERE org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND workspace_id = sqlc.arg(workspace_id)
   AND id = sqlc.arg(id)
   AND kind = 'command'
   AND state IN ('queued', 'starting')
RETURNING *;

-- name: ListWorkspaceExecsAwaitingDispatch :many
SELECT workspace_processes.*
  FROM workspace_processes
  JOIN workspace_mounts
    ON workspace_mounts.org_id = workspace_processes.org_id
   AND workspace_mounts.project_id = workspace_processes.project_id
   AND workspace_mounts.environment_id = workspace_processes.environment_id
   AND workspace_mounts.workspace_id = workspace_processes.workspace_id
   AND workspace_mounts.id = workspace_processes.workspace_mount_id
 WHERE workspace_processes.org_id = sqlc.arg(org_id)
   AND workspace_processes.project_id = sqlc.arg(project_id)
   AND workspace_processes.environment_id = sqlc.arg(environment_id)
   AND workspace_processes.workspace_id = sqlc.arg(workspace_id)
   AND workspace_processes.workspace_mount_id = sqlc.arg(workspace_mount_id)
   AND workspace_processes.kind = 'command'
   AND workspace_processes.state IN ('starting', 'queued')
   AND workspace_mounts.state = 'mounted'
 ORDER BY workspace_processes.created_at ASC, workspace_processes.id ASC
 LIMIT sqlc.arg(limit_count);

-- name: MarkWorkspaceExecStarted :one
WITH target AS MATERIALIZED (
    SELECT workspace_processes.*
      FROM workspace_processes
      JOIN workspace_mounts
        ON workspace_mounts.org_id = workspace_processes.org_id
       AND workspace_mounts.project_id = workspace_processes.project_id
       AND workspace_mounts.environment_id = workspace_processes.environment_id
       AND workspace_mounts.workspace_id = workspace_processes.workspace_id
       AND workspace_mounts.id = workspace_processes.workspace_mount_id
     WHERE workspace_processes.org_id = sqlc.arg(org_id)
       AND workspace_processes.project_id = sqlc.arg(project_id)
       AND workspace_processes.environment_id = sqlc.arg(environment_id)
       AND workspace_processes.workspace_id = sqlc.arg(workspace_id)
       AND workspace_processes.id = sqlc.arg(id)
       AND workspace_processes.workspace_mount_id = sqlc.arg(workspace_mount_id)
       AND workspace_processes.kind = 'command'
       AND workspace_processes.state IN ('queued', 'starting', 'running')
       AND workspace_mounts.state = 'mounted'
     FOR UPDATE OF workspace_processes, workspace_mounts
),
updated_exec AS (
    UPDATE workspace_processes
       SET state = 'running',
           runtime_process_id = sqlc.arg(runtime_process_id),
           started_at = coalesce(workspace_processes.started_at, now()),
           updated_at = now()
      FROM target
     WHERE workspace_processes.org_id = target.org_id
       AND workspace_processes.project_id = target.project_id
       AND workspace_processes.environment_id = target.environment_id
       AND workspace_processes.workspace_id = target.workspace_id
       AND workspace_processes.id = target.id
    RETURNING workspace_processes.*
),
dirtied_mount AS (
    UPDATE workspace_mounts
       SET dirty_generation = workspace_mounts.dirty_generation + 1,
           updated_at = now()
      FROM target
      JOIN updated_exec ON updated_exec.id = target.id
      JOIN workspace_leases
        ON workspace_leases.org_id = updated_exec.org_id
       AND workspace_leases.project_id = updated_exec.project_id
       AND workspace_leases.environment_id = updated_exec.environment_id
       AND workspace_leases.workspace_id = updated_exec.workspace_id
       AND workspace_leases.id = updated_exec.write_lease_id
       AND workspace_leases.owner_process_id = updated_exec.id
       AND workspace_leases.lease_kind = 'write'
       AND workspace_leases.state = 'active'
     WHERE target.state <> 'running'
       AND updated_exec.filesystem_mode = 'write'
       AND workspace_mounts.org_id = updated_exec.org_id
       AND workspace_mounts.project_id = updated_exec.project_id
       AND workspace_mounts.environment_id = updated_exec.environment_id
       AND workspace_mounts.workspace_id = updated_exec.workspace_id
       AND workspace_mounts.id = updated_exec.workspace_mount_id
       AND workspace_mounts.fencing_generation = workspace_leases.acquired_fencing_generation
    RETURNING workspace_mounts.*
),
updated_workspace AS (
    UPDATE workspaces
       SET dirty_state = 'dirty',
           updated_at = now()
      FROM dirtied_mount
     WHERE workspaces.org_id = dirtied_mount.org_id
       AND workspaces.project_id = dirtied_mount.project_id
       AND workspaces.environment_id = dirtied_mount.environment_id
       AND workspaces.id = dirtied_mount.workspace_id
    RETURNING workspaces.id
)
SELECT *
  FROM updated_exec
 WHERE (SELECT count(*) FROM updated_workspace) >= 0;

-- name: CloseWorkspaceExecStdin :one
UPDATE workspace_processes
   SET stdin_closed_at = coalesce(workspace_processes.stdin_closed_at, now()),
       updated_at = now()
 WHERE org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND workspace_id = sqlc.arg(workspace_id)
   AND id = sqlc.arg(id)
   AND kind = 'command'
   AND state IN ('queued', 'starting', 'running')
RETURNING *;

-- name: MarkWorkspaceExecExited :one
WITH updated_exec AS (
    UPDATE workspace_processes
       SET state = sqlc.arg(state)::workspace_process_state,
           exit_code = sqlc.narg(exit_code),
           signal = coalesce(sqlc.arg(signal)::text, workspace_processes.signal),
           error = coalesce(sqlc.arg(error)::jsonb, '{}'::jsonb),
           exited_at = coalesce(workspace_processes.exited_at, now()),
           updated_at = now()
     WHERE workspace_processes.org_id = sqlc.arg(org_id)
       AND workspace_processes.project_id = sqlc.arg(project_id)
       AND workspace_processes.environment_id = sqlc.arg(environment_id)
       AND workspace_processes.workspace_id = sqlc.arg(workspace_id)
       AND workspace_processes.id = sqlc.arg(id)
       AND workspace_processes.workspace_mount_id = sqlc.arg(workspace_mount_id)
       AND workspace_processes.kind = 'command'
       AND workspace_processes.state IN ('queued', 'starting', 'running')
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
       AND workspace_leases.owner_process_id = updated_exec.id
       AND workspace_leases.lease_kind = 'write'
       AND workspace_leases.state IN ('active', 'releasing')
    RETURNING workspace_leases.id
),
stream_wakeups AS (
    INSERT INTO workspace_process_stream_wakeups (org_id, project_id, environment_id, workspace_id, worker_group_id, process_id, stream_name, cursor_offset, notification_kind)
    SELECT updated_exec.org_id,
           updated_exec.project_id,
           updated_exec.environment_id,
           updated_exec.workspace_id,
           updated_exec.worker_group_id,
           updated_exec.id,
           stream_names.stream_name,
           stream_names.cursor_offset,
           'terminal'::workspace_stream_notification_kind
      FROM updated_exec
      CROSS JOIN LATERAL (VALUES ('stdout', updated_exec.stdout_cursor), ('stderr', updated_exec.stderr_cursor)) AS stream_names(stream_name, cursor_offset)
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
  FROM workspace_processes
 WHERE org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND workspace_id = sqlc.arg(workspace_id)
   AND id = sqlc.arg(process_id)
   AND kind = 'command'
 FOR UPDATE;

-- name: InsertWorkspaceExecStreamChunk :one
INSERT INTO workspace_process_stream_chunks (
    org_id,
    worker_group_id,
    project_id,
    environment_id,
    workspace_id,
    process_id,
    stream_name,
    direction,
    offset_start,
    offset_end,
    data,
    observed_at
) VALUES (
    sqlc.arg(org_id),
    sqlc.arg(worker_group_id),
    sqlc.arg(project_id),
    sqlc.arg(environment_id),
    sqlc.arg(workspace_id),
    sqlc.arg(process_id),
    sqlc.arg(stream_name)::text,
    CASE WHEN sqlc.arg(stream_name)::text = 'stdin' THEN 'input' ELSE 'output' END,
    sqlc.arg(offset_start),
    sqlc.arg(offset_end),
    sqlc.arg(data),
    coalesce(sqlc.narg(observed_at), now())
)
RETURNING *;

-- name: InsertWorkspaceExecOutputStreamChunk :one
WITH inserted AS (
    INSERT INTO workspace_process_stream_chunks (
        org_id,
        worker_group_id,
        project_id,
        environment_id,
        workspace_id,
        process_id,
        stream_name,
        direction,
        offset_start,
        offset_end,
        data,
        observed_at
    ) VALUES (
        sqlc.arg(org_id),
        sqlc.arg(worker_group_id),
        sqlc.arg(project_id),
        sqlc.arg(environment_id),
        sqlc.arg(workspace_id),
        sqlc.arg(process_id),
        sqlc.arg(stream_name)::text,
        CASE WHEN sqlc.arg(stream_name)::text = 'stdin' THEN 'input' ELSE 'output' END,
        sqlc.arg(offset_start),
        sqlc.arg(offset_end),
        sqlc.arg(data),
        coalesce(sqlc.narg(observed_at), now())
    )
    ON CONFLICT DO NOTHING
    RETURNING *
),
terminal_telemetry_outbox AS (
    INSERT INTO telemetry_outbox (
        org_id,
        worker_group_id,
        stream_kind,
        source_kind,
        source_id,
        stream_name,
        offset_start,
        idempotency_key,
        project_id,
        environment_id,
        workspace_id,
        resource_kind,
        resource_id,
        content,
        size_bytes,
        offset_end,
        source,
        kind,
        message,
        redaction_class,
        retention_class,
        observed_at
    )
    SELECT inserted.org_id,
           inserted.worker_group_id,
           'terminal_output',
           'workspace_process',
           inserted.process_id,
           inserted.stream_name::text,
           inserted.offset_start,
           'terminal_output:workspace_process:' || inserted.process_id::text || ':' || inserted.stream_name::text || ':' || inserted.offset_start::text || ':' || inserted.offset_end::text,
           inserted.project_id,
           inserted.environment_id,
           inserted.workspace_id,
           'workspace_process',
           inserted.process_id,
           inserted.data,
           octet_length(inserted.data)::bigint,
           inserted.offset_end,
           'worker',
           'terminal.output',
           'terminal.output',
           'standard',
           'standard',
           inserted.observed_at
      FROM inserted
    ON CONFLICT (worker_group_id, stream_kind, idempotency_key) DO NOTHING
    RETURNING id
)
SELECT *
  FROM inserted
 WHERE (SELECT count(*) FROM terminal_telemetry_outbox) >= 0;

-- name: AdvanceWorkspaceExecStreamCursor :one
UPDATE workspace_processes
   SET stdin_cursor = CASE WHEN sqlc.arg(stream_name) = 'stdin' THEN sqlc.arg(offset_end) ELSE stdin_cursor END,
       stdout_cursor = CASE WHEN sqlc.arg(stream_name) = 'stdout' THEN sqlc.arg(offset_end) ELSE stdout_cursor END,
       stderr_cursor = CASE WHEN sqlc.arg(stream_name) = 'stderr' THEN sqlc.arg(offset_end) ELSE stderr_cursor END,
       updated_at = now()
 WHERE org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND workspace_id = sqlc.arg(workspace_id)
   AND id = sqlc.arg(process_id)
   AND sqlc.arg(offset_start) = CASE sqlc.arg(stream_name)
         WHEN 'stdin' THEN stdin_cursor
         WHEN 'stdout' THEN stdout_cursor
         WHEN 'stderr' THEN stderr_cursor
       END
RETURNING *;

-- name: AdvanceWorkspaceExecOutputCursor :one
WITH RECURSIVE current_cursor AS (
    SELECT CASE sqlc.arg(stream_name)
             WHEN 'stdout' THEN stdout_cursor
             WHEN 'stderr' THEN stderr_cursor
             ELSE -1
           END AS cursor
      FROM workspace_processes
     WHERE org_id = sqlc.arg(org_id)
       AND project_id = sqlc.arg(project_id)
       AND environment_id = sqlc.arg(environment_id)
       AND workspace_id = sqlc.arg(workspace_id)
       AND id = sqlc.arg(process_id)
     FOR UPDATE
),
contiguous(end_offset) AS (
    SELECT cursor FROM current_cursor
    UNION
    SELECT chunks.offset_end
      FROM contiguous
      JOIN workspace_process_stream_chunks AS chunks
        ON chunks.org_id = sqlc.arg(org_id)
       AND chunks.project_id = sqlc.arg(project_id)
       AND chunks.environment_id = sqlc.arg(environment_id)
       AND chunks.workspace_id = sqlc.arg(workspace_id)
       AND chunks.process_id = sqlc.arg(process_id)
       AND chunks.stream_name = sqlc.arg(stream_name)
       AND chunks.offset_start = contiguous.end_offset
),
advanced AS (
    SELECT max(end_offset)::bigint AS cursor FROM contiguous
)
UPDATE workspace_processes
   SET stdout_cursor = CASE WHEN sqlc.arg(stream_name) = 'stdout' THEN advanced.cursor ELSE stdout_cursor END,
       stderr_cursor = CASE WHEN sqlc.arg(stream_name) = 'stderr' THEN advanced.cursor ELSE stderr_cursor END,
       updated_at = now()
  FROM advanced
 WHERE workspace_processes.org_id = sqlc.arg(org_id)
   AND workspace_processes.project_id = sqlc.arg(project_id)
   AND workspace_processes.environment_id = sqlc.arg(environment_id)
   AND workspace_processes.workspace_id = sqlc.arg(workspace_id)
   AND workspace_processes.id = sqlc.arg(process_id)
   AND sqlc.arg(stream_name) IN ('stdout', 'stderr')
RETURNING workspace_processes.*;

-- name: DeleteWorkspaceExecStreamChunksBefore :exec
DELETE FROM workspace_process_stream_chunks
 WHERE workspace_process_stream_chunks.org_id = sqlc.arg(org_id)
   AND workspace_process_stream_chunks.project_id = sqlc.arg(project_id)
   AND workspace_process_stream_chunks.environment_id = sqlc.arg(environment_id)
   AND workspace_process_stream_chunks.workspace_id = sqlc.arg(workspace_id)
   AND workspace_process_stream_chunks.process_id = sqlc.arg(process_id)
   AND workspace_process_stream_chunks.stream_name = sqlc.arg(stream_name)
   AND workspace_process_stream_chunks.offset_end <= sqlc.arg(retain_after_offset);

-- name: GetWorkspaceExecStreamChunkAtOffset :one
SELECT *
  FROM workspace_process_stream_chunks
 WHERE org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND workspace_id = sqlc.arg(workspace_id)
   AND process_id = sqlc.arg(process_id)
   AND stream_name = sqlc.arg(stream_name)
   AND offset_start = sqlc.arg(offset_start);

-- name: InsertWorkspaceExecStreamChunkReceipt :one
INSERT INTO workspace_process_stream_receipts (
    org_id,
    worker_group_id,
    project_id,
    environment_id,
    workspace_id,
    process_id,
    stream_name,
    direction,
    offset_start,
    offset_end,
    data_sha256,
    data_size,
    observed_at
) VALUES (
    sqlc.arg(org_id),
    sqlc.arg(worker_group_id),
    sqlc.arg(project_id),
    sqlc.arg(environment_id),
    sqlc.arg(workspace_id),
    sqlc.arg(process_id),
    sqlc.arg(stream_name)::text,
    CASE WHEN sqlc.arg(stream_name)::text = 'stdin' THEN 'input' ELSE 'output' END,
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
  FROM workspace_process_stream_receipts
 WHERE org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND workspace_id = sqlc.arg(workspace_id)
   AND process_id = sqlc.arg(process_id)
   AND stream_name = sqlc.arg(stream_name)
   AND offset_start = sqlc.arg(offset_start);

-- name: ListWorkspaceExecStreamChunksAfter :many
SELECT *
  FROM workspace_process_stream_chunks
 WHERE org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND workspace_id = sqlc.arg(workspace_id)
   AND process_id = sqlc.arg(process_id)
   AND stream_name = sqlc.arg(stream_name)
   AND offset_end > sqlc.arg(cursor_offset)
 ORDER BY offset_start ASC
 LIMIT sqlc.arg(limit_count);

-- name: GetWorkspaceExecStreamBounds :one
SELECT coalesce(min(offset_start), 0)::bigint AS earliest_offset,
       coalesce(max(offset_end), 0)::bigint AS latest_offset
  FROM workspace_process_stream_chunks
 WHERE org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND workspace_id = sqlc.arg(workspace_id)
   AND process_id = sqlc.arg(process_id)
   AND stream_name = sqlc.arg(stream_name);

-- name: ListWorkspaceExecStdinChunksAfterDelivered :many
SELECT chunks.*
  FROM workspace_processes
  JOIN workspace_process_stream_chunks AS chunks
    ON chunks.org_id = workspace_processes.org_id
   AND chunks.project_id = workspace_processes.project_id
   AND chunks.environment_id = workspace_processes.environment_id
   AND chunks.workspace_id = workspace_processes.workspace_id
   AND chunks.process_id = workspace_processes.id
   AND chunks.stream_name = 'stdin'
 WHERE workspace_processes.org_id = sqlc.arg(org_id)
   AND workspace_processes.project_id = sqlc.arg(project_id)
   AND workspace_processes.environment_id = sqlc.arg(environment_id)
   AND workspace_processes.workspace_id = sqlc.arg(workspace_id)
   AND workspace_processes.id = sqlc.arg(process_id)
   AND chunks.offset_end > workspace_processes.stdin_delivered_cursor
 ORDER BY chunks.offset_start ASC
 LIMIT sqlc.arg(limit_count);

-- name: AdvanceWorkspaceExecStdinDeliveredCursor :one
UPDATE workspace_processes
   SET stdin_delivered_cursor = sqlc.arg(offset_end),
       updated_at = now()
 WHERE workspace_processes.org_id = sqlc.arg(org_id)
   AND workspace_processes.project_id = sqlc.arg(project_id)
   AND workspace_processes.environment_id = sqlc.arg(environment_id)
   AND workspace_processes.workspace_id = sqlc.arg(workspace_id)
   AND workspace_processes.id = sqlc.arg(process_id)
   AND sqlc.arg(offset_start) = workspace_processes.stdin_delivered_cursor
   AND sqlc.arg(offset_end) <= workspace_processes.stdin_cursor
   AND EXISTS (
       SELECT 1
         FROM workspace_process_stream_chunks AS chunks
        WHERE chunks.org_id = workspace_processes.org_id
          AND chunks.project_id = workspace_processes.project_id
          AND chunks.environment_id = workspace_processes.environment_id
          AND chunks.workspace_id = workspace_processes.workspace_id
          AND chunks.process_id = workspace_processes.id
          AND chunks.stream_name = 'stdin'
          AND chunks.offset_start = sqlc.arg(offset_start)
          AND chunks.offset_end = sqlc.arg(offset_end)
   )
RETURNING *;
