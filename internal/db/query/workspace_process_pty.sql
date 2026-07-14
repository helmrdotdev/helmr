-- name: CreateWorkspacePtySession :one
INSERT INTO workspace_processes (
    id,
    org_id,
    project_id,
    environment_id,
    workspace_id,
    kind,
    cwd,
    pty_cols,
    pty_rows,
    filesystem_mode,
    state,
    idempotency_key,
    idempotency_expires_at,
    request_fingerprint,
    created_by_subject_type,
    created_by_subject_id
)
SELECT sqlc.arg(id),
       workspaces.org_id,
       workspaces.project_id,
       workspaces.environment_id,
       workspaces.id,
       'pty',
       coalesce(sqlc.arg(cwd)::text, ''),
       sqlc.arg(pty_cols),
       sqlc.arg(pty_rows),
       sqlc.arg(filesystem_mode)::workspace_filesystem_mode,
       sqlc.arg(state)::workspace_process_state,
       coalesce(sqlc.arg(idempotency_key)::text, ''),
       sqlc.narg(idempotency_expires_at),
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

-- name: GetWorkspacePtySessionByIdempotency :one
SELECT *
  FROM workspace_processes
 WHERE org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND workspace_id = sqlc.arg(workspace_id)
   AND kind = 'pty'
   AND idempotency_key = sqlc.arg(idempotency_key)
   AND idempotency_expires_at > now();

-- name: ClearExpiredWorkspacePtyIdempotency :exec
UPDATE workspace_processes
   SET idempotency_key = '',
       idempotency_expires_at = NULL,
       request_fingerprint = '',
       updated_at = now()
 WHERE org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND workspace_id = sqlc.arg(workspace_id)
   AND kind = 'pty'
   AND idempotency_key = sqlc.arg(idempotency_key)
   AND idempotency_expires_at <= now();

-- name: GetWorkspacePtySession :one
SELECT *
  FROM workspace_processes
 WHERE org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND workspace_id = sqlc.arg(workspace_id)
   AND id = sqlc.arg(id)
   AND kind = 'pty';

-- name: ListWorkspacePtySessions :many
SELECT *
  FROM workspace_processes
 WHERE org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND workspace_id = sqlc.arg(workspace_id)
   AND kind = 'pty'
   AND (sqlc.narg(state)::workspace_process_state IS NULL OR state = sqlc.narg(state)::workspace_process_state)
 ORDER BY created_at DESC, id DESC
 LIMIT sqlc.arg(limit_count);

-- name: BindWorkspacePtyWorkspaceMount :one
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
   AND kind = 'pty'
   AND state IN ('starting')
RETURNING *;

-- name: ListWorkspacePtySessionsAwaitingDispatch :many
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
   AND workspace_processes.kind = 'pty'
   AND workspace_processes.state IN ('starting')
   AND workspace_mounts.state = 'mounted'
 ORDER BY workspace_processes.created_at ASC, workspace_processes.id ASC
 LIMIT sqlc.arg(limit_count);

-- name: MarkWorkspacePtyOpen :one
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
       AND workspace_processes.kind = 'pty'
       AND workspace_processes.state IN ('starting', 'running')
       AND workspace_mounts.state = 'mounted'
     FOR UPDATE OF workspace_processes, workspace_mounts
),
updated_pty AS (
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
      JOIN updated_pty ON updated_pty.id = target.id
      JOIN workspace_leases
        ON workspace_leases.org_id = updated_pty.org_id
       AND workspace_leases.project_id = updated_pty.project_id
       AND workspace_leases.environment_id = updated_pty.environment_id
       AND workspace_leases.workspace_id = updated_pty.workspace_id
       AND workspace_leases.id = updated_pty.write_lease_id
       AND workspace_leases.owner_process_id = updated_pty.id
       AND workspace_leases.lease_kind = 'write'
       AND workspace_leases.state = 'active'
     WHERE target.state <> 'running'
       AND updated_pty.filesystem_mode = 'write'
       AND workspace_mounts.org_id = updated_pty.org_id
       AND workspace_mounts.project_id = updated_pty.project_id
       AND workspace_mounts.environment_id = updated_pty.environment_id
       AND workspace_mounts.workspace_id = updated_pty.workspace_id
       AND workspace_mounts.id = updated_pty.workspace_mount_id
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
  FROM updated_pty
 WHERE (SELECT count(*) FROM updated_workspace) >= 0;

-- name: ResizeWorkspacePtySession :one
UPDATE workspace_processes
   SET pending_pty_cols = sqlc.arg(pty_cols),
       pending_pty_rows = sqlc.arg(pty_rows),
       updated_at = now()
 WHERE org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND workspace_id = sqlc.arg(workspace_id)
   AND id = sqlc.arg(id)
   AND kind = 'pty'
   AND state = 'running'
RETURNING *;

-- name: MarkWorkspacePtyResizeApplied :one
UPDATE workspace_processes
   SET pty_cols = pending_pty_cols,
       pty_rows = pending_pty_rows,
       pending_pty_cols = NULL,
       pending_pty_rows = NULL,
       updated_at = now()
 WHERE org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND workspace_id = sqlc.arg(workspace_id)
   AND id = sqlc.arg(id)
   AND workspace_mount_id = sqlc.arg(workspace_mount_id)
   AND kind = 'pty'
   AND state IN ('running', 'closing')
   AND pending_pty_cols = sqlc.arg(pty_cols)
   AND pending_pty_rows = sqlc.arg(pty_rows)
RETURNING *;

-- name: RequestWorkspacePtyClose :one
UPDATE workspace_processes
   SET state = 'closing',
       pending_pty_cols = CASE WHEN state IN ('running', 'closing') THEN pending_pty_cols ELSE NULL END,
       pending_pty_rows = CASE WHEN state IN ('running', 'closing') THEN pending_pty_rows ELSE NULL END,
       updated_at = now()
 WHERE org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND workspace_id = sqlc.arg(workspace_id)
   AND id = sqlc.arg(id)
   AND kind = 'pty'
   AND state IN ('running', 'closing')
RETURNING *;

-- name: RollbackWorkspacePtyControlOperation :one
UPDATE workspace_processes
   SET state = 'running',
       pending_pty_cols = CASE
           WHEN sqlc.arg(operation_kind)::workspace_operation_kind = 'close_process'
               THEN pending_pty_cols
           ELSE NULL
       END,
       pending_pty_rows = CASE
           WHEN sqlc.arg(operation_kind)::workspace_operation_kind = 'close_process'
               THEN pending_pty_rows
           ELSE NULL
       END,
       updated_at = now()
 WHERE workspace_processes.org_id = sqlc.arg(org_id)
   AND workspace_processes.project_id = sqlc.arg(project_id)
   AND workspace_processes.environment_id = sqlc.arg(environment_id)
   AND workspace_processes.workspace_id = sqlc.arg(workspace_id)
   AND workspace_processes.id = sqlc.arg(id)
   AND workspace_processes.workspace_mount_id = sqlc.arg(workspace_mount_id)
   AND workspace_processes.kind = 'pty'
   AND (
       (
           sqlc.arg(operation_kind)::workspace_operation_kind = 'resize_process'
           AND workspace_processes.state = 'running'
           AND workspace_processes.pending_pty_cols = sqlc.narg(pty_cols)
           AND workspace_processes.pending_pty_rows = sqlc.narg(pty_rows)
       )
       OR (
           sqlc.arg(operation_kind)::workspace_operation_kind = 'close_process'
           AND workspace_processes.state = 'closing'
       )
   )
RETURNING *;

-- name: MarkWorkspacePtyClosed :one
WITH updated_pty AS (
    UPDATE workspace_processes
       SET state = 'exited',
           pending_pty_cols = NULL,
           pending_pty_rows = NULL,
           exited_at = coalesce(workspace_processes.exited_at, now()),
           updated_at = now()
     WHERE workspace_processes.org_id = sqlc.arg(org_id)
       AND workspace_processes.project_id = sqlc.arg(project_id)
       AND workspace_processes.environment_id = sqlc.arg(environment_id)
       AND workspace_processes.workspace_id = sqlc.arg(workspace_id)
       AND workspace_processes.id = sqlc.arg(id)
       AND workspace_processes.workspace_mount_id = sqlc.arg(workspace_mount_id)
       AND workspace_processes.kind = 'pty'
       AND workspace_processes.state IN ('starting', 'running', 'closing')
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
       AND workspace_leases.owner_process_id = updated_pty.id
       AND workspace_leases.lease_kind = 'write'
       AND workspace_leases.state IN ('active', 'releasing')
    RETURNING workspace_leases.id
)
SELECT *
  FROM updated_pty;

-- name: MarkWorkspacePtyFailed :one
WITH updated_pty AS (
    UPDATE workspace_processes
       SET state = 'failed',
           pending_pty_cols = NULL,
           pending_pty_rows = NULL,
           error = coalesce(sqlc.arg(error)::jsonb, '{}'::jsonb),
           exited_at = coalesce(workspace_processes.exited_at, now()),
           updated_at = now()
     WHERE workspace_processes.org_id = sqlc.arg(org_id)
       AND workspace_processes.project_id = sqlc.arg(project_id)
       AND workspace_processes.environment_id = sqlc.arg(environment_id)
       AND workspace_processes.workspace_id = sqlc.arg(workspace_id)
       AND workspace_processes.id = sqlc.arg(id)
       AND workspace_processes.workspace_mount_id = sqlc.arg(workspace_mount_id)
       AND workspace_processes.kind = 'pty'
       AND workspace_processes.state IN ('starting', 'running', 'closing')
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
       AND workspace_leases.owner_process_id = updated_pty.id
       AND workspace_leases.lease_kind = 'write'
       AND workspace_leases.state IN ('active', 'releasing')
    RETURNING workspace_leases.id
)
SELECT *
  FROM updated_pty;

-- name: LockWorkspacePtyForStreamAppend :one
SELECT id,
       org_id,
       project_id,
       environment_id,
       workspace_id,
       input_cursor,
       output_cursor,
       state
  FROM workspace_processes
 WHERE org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND workspace_id = sqlc.arg(workspace_id)
   AND id = sqlc.arg(process_id)
   AND kind = 'pty'
 FOR UPDATE;

-- name: InsertWorkspacePtyStreamChunk :one
INSERT INTO workspace_process_stream_chunks (
    org_id,
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
    sqlc.arg(project_id),
    sqlc.arg(environment_id),
    sqlc.arg(workspace_id),
    sqlc.arg(process_id),
    sqlc.arg(stream_name)::text,
    CASE WHEN sqlc.arg(stream_name)::text = 'input' THEN 'input' ELSE 'output' END,
    sqlc.arg(offset_start),
    sqlc.arg(offset_end),
    sqlc.arg(data),
    coalesce(sqlc.narg(observed_at), now())
)
RETURNING *;

-- name: InsertWorkspacePtyOutputStreamChunk :one
WITH inserted AS (
    INSERT INTO workspace_process_stream_chunks (
        org_id,
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
        sqlc.arg(project_id),
        sqlc.arg(environment_id),
        sqlc.arg(workspace_id),
        sqlc.arg(process_id),
        sqlc.arg(stream_name)::text,
        CASE WHEN sqlc.arg(stream_name)::text = 'input' THEN 'input' ELSE 'output' END,
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
    ON CONFLICT (org_id, stream_kind, source_kind, source_id, stream_name, idempotency_key) DO NOTHING
    RETURNING id
)
SELECT *
  FROM inserted
 WHERE (SELECT count(*) FROM terminal_telemetry_outbox) >= 0;

-- name: AdvanceWorkspacePtyStreamCursor :one
UPDATE workspace_processes
   SET input_cursor = CASE WHEN sqlc.arg(stream_name) = 'input' THEN sqlc.arg(offset_end) ELSE input_cursor END,
       output_cursor = CASE WHEN sqlc.arg(stream_name) = 'output' THEN sqlc.arg(offset_end) ELSE output_cursor END,
       updated_at = now()
 WHERE org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND workspace_id = sqlc.arg(workspace_id)
   AND id = sqlc.arg(process_id)
   AND sqlc.arg(offset_start) = CASE sqlc.arg(stream_name)
         WHEN 'input' THEN input_cursor
         WHEN 'output' THEN output_cursor
       END
RETURNING *;

-- name: AdvanceWorkspacePtyOutputCursor :one
WITH RECURSIVE current_cursor AS (
    SELECT CASE sqlc.arg(stream_name)
             WHEN 'output' THEN output_cursor
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
   SET output_cursor = CASE WHEN sqlc.arg(stream_name) = 'output' THEN advanced.cursor ELSE output_cursor END,
       updated_at = now()
  FROM advanced
 WHERE workspace_processes.org_id = sqlc.arg(org_id)
   AND workspace_processes.project_id = sqlc.arg(project_id)
   AND workspace_processes.environment_id = sqlc.arg(environment_id)
   AND workspace_processes.workspace_id = sqlc.arg(workspace_id)
   AND workspace_processes.id = sqlc.arg(process_id)
   AND sqlc.arg(stream_name) = 'output'
RETURNING workspace_processes.*;

-- name: DeleteWorkspacePtyStreamChunksBefore :exec
DELETE FROM workspace_process_stream_chunks
 WHERE workspace_process_stream_chunks.org_id = sqlc.arg(org_id)
   AND workspace_process_stream_chunks.project_id = sqlc.arg(project_id)
   AND workspace_process_stream_chunks.environment_id = sqlc.arg(environment_id)
   AND workspace_process_stream_chunks.workspace_id = sqlc.arg(workspace_id)
   AND workspace_process_stream_chunks.process_id = sqlc.arg(process_id)
   AND workspace_process_stream_chunks.stream_name = sqlc.arg(stream_name)
   AND workspace_process_stream_chunks.offset_end <= sqlc.arg(retain_after_offset);

-- name: GetWorkspacePtyStreamChunkAtOffset :one
SELECT *
  FROM workspace_process_stream_chunks
 WHERE org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND workspace_id = sqlc.arg(workspace_id)
   AND process_id = sqlc.arg(process_id)
   AND stream_name = sqlc.arg(stream_name)
   AND offset_start = sqlc.arg(offset_start);

-- name: InsertWorkspacePtyStreamChunkReceipt :one
INSERT INTO workspace_process_stream_receipts (
    org_id,
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
    sqlc.arg(project_id),
    sqlc.arg(environment_id),
    sqlc.arg(workspace_id),
    sqlc.arg(process_id),
    sqlc.arg(stream_name)::text,
    CASE WHEN sqlc.arg(stream_name)::text = 'input' THEN 'input' ELSE 'output' END,
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
  FROM workspace_process_stream_receipts
 WHERE org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND workspace_id = sqlc.arg(workspace_id)
   AND process_id = sqlc.arg(process_id)
   AND stream_name = sqlc.arg(stream_name)
   AND offset_start = sqlc.arg(offset_start);

-- name: ListWorkspacePtyStreamChunksAfter :many
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

-- name: GetWorkspacePtyStreamBounds :one
SELECT coalesce(min(offset_start), 0)::bigint AS earliest_offset,
       coalesce(max(offset_end), 0)::bigint AS latest_offset
  FROM workspace_process_stream_chunks
 WHERE org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND workspace_id = sqlc.arg(workspace_id)
   AND process_id = sqlc.arg(process_id)
   AND stream_name = sqlc.arg(stream_name);

-- name: ListWorkspacePtyInputChunksAfterDelivered :many
SELECT chunks.*
  FROM workspace_processes
  JOIN workspace_process_stream_chunks AS chunks
    ON chunks.org_id = workspace_processes.org_id
   AND chunks.project_id = workspace_processes.project_id
   AND chunks.environment_id = workspace_processes.environment_id
   AND chunks.workspace_id = workspace_processes.workspace_id
   AND chunks.process_id = workspace_processes.id
   AND chunks.stream_name = 'input'
 WHERE workspace_processes.org_id = sqlc.arg(org_id)
   AND workspace_processes.project_id = sqlc.arg(project_id)
   AND workspace_processes.environment_id = sqlc.arg(environment_id)
   AND workspace_processes.workspace_id = sqlc.arg(workspace_id)
   AND workspace_processes.id = sqlc.arg(process_id)
   AND chunks.offset_end > workspace_processes.input_delivered_cursor
 ORDER BY chunks.offset_start ASC
 LIMIT sqlc.arg(limit_count);

-- name: AdvanceWorkspacePtyInputDeliveredCursor :one
UPDATE workspace_processes
   SET input_delivered_cursor = sqlc.arg(offset_end),
       updated_at = now()
 WHERE workspace_processes.org_id = sqlc.arg(org_id)
   AND workspace_processes.project_id = sqlc.arg(project_id)
   AND workspace_processes.environment_id = sqlc.arg(environment_id)
   AND workspace_processes.workspace_id = sqlc.arg(workspace_id)
   AND workspace_processes.id = sqlc.arg(process_id)
   AND sqlc.arg(offset_start) = workspace_processes.input_delivered_cursor
   AND sqlc.arg(offset_end) <= workspace_processes.input_cursor
   AND EXISTS (
       SELECT 1
         FROM workspace_process_stream_chunks AS chunks
        WHERE chunks.org_id = workspace_processes.org_id
          AND chunks.project_id = workspace_processes.project_id
          AND chunks.environment_id = workspace_processes.environment_id
          AND chunks.workspace_id = workspace_processes.workspace_id
          AND chunks.process_id = workspace_processes.id
          AND chunks.stream_name = 'input'
          AND chunks.offset_start = sqlc.arg(offset_start)
          AND chunks.offset_end = sqlc.arg(offset_end)
   )
RETURNING *;
