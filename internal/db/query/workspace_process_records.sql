-- name: GetWorkspaceProcessRecord :one
SELECT *
FROM workspace_process_records
WHERE process_id = sqlc.arg(process_id)
  AND stream = sqlc.arg(stream)
  AND offset_start = sqlc.arg(offset_start);

-- name: AppendWorkspaceProcessRecord :one
WITH locked AS (
    SELECT workspace_processes.*,
           CASE sqlc.arg(stream)::text
               WHEN 'stdin' THEN stdin_cursor
               WHEN 'stdout' THEN stdout_cursor
               WHEN 'stderr' THEN stderr_cursor
               WHEN 'pty_input' THEN input_cursor
               WHEN 'pty_output' THEN output_cursor
           END AS stream_cursor
      FROM workspace_processes
     WHERE workspace_processes.environment_id = sqlc.arg(environment_id)
       AND workspace_processes.id = sqlc.arg(process_id)
       AND workspace_processes.kind = sqlc.arg(process_kind)
     FOR UPDATE
), replay AS (
    SELECT workspace_process_records.*
      FROM workspace_process_records
      JOIN locked ON locked.id = workspace_process_records.process_id
     WHERE workspace_process_records.process_id = sqlc.arg(process_id)
       AND workspace_process_records.stream = sqlc.arg(stream)
       AND workspace_process_records.offset_start = sqlc.arg(offset_start)
       AND workspace_process_records.offset_end = sqlc.arg(offset_end)
       AND workspace_process_records.content_digest = sqlc.arg(content_digest)
       AND workspace_process_records.size_bytes = sqlc.arg(size_bytes)
), inserted AS (
    INSERT INTO workspace_process_records (
        id,
        environment_id,
        process_id,
        process_kind,
        direction,
        stream,
        offset_start,
        offset_end,
        data,
        artifact_id,
        artifact_kind,
        artifact_digest,
        content_digest,
        size_bytes,
        observed_at,
        payload_expires_at
    )
    SELECT sqlc.arg(id),
           sqlc.arg(environment_id),
           sqlc.arg(process_id),
           sqlc.arg(process_kind),
           sqlc.arg(direction),
           sqlc.arg(stream),
           sqlc.arg(offset_start),
           sqlc.arg(offset_end),
           sqlc.narg(data),
           sqlc.narg(artifact_id),
           sqlc.narg(artifact_kind),
           sqlc.narg(artifact_digest),
           sqlc.arg(content_digest),
           sqlc.arg(size_bytes),
           sqlc.arg(observed_at),
           sqlc.narg(payload_expires_at)
      FROM locked
     WHERE locked.stream_cursor = sqlc.arg(offset_start)
       AND NOT EXISTS (SELECT 1 FROM replay)
    RETURNING *
), advanced AS (
    UPDATE workspace_processes
       SET stdin_cursor = CASE
               WHEN inserted.stream = 'stdin' THEN inserted.offset_end
               ELSE workspace_processes.stdin_cursor
           END,
           stdout_cursor = CASE
               WHEN inserted.stream = 'stdout' THEN inserted.offset_end
               ELSE workspace_processes.stdout_cursor
           END,
           stderr_cursor = CASE
               WHEN inserted.stream = 'stderr' THEN inserted.offset_end
               ELSE workspace_processes.stderr_cursor
           END,
           input_cursor = CASE
               WHEN inserted.stream = 'pty_input' THEN inserted.offset_end
               ELSE workspace_processes.input_cursor
           END,
           output_cursor = CASE
               WHEN inserted.stream = 'pty_output' THEN inserted.offset_end
               ELSE workspace_processes.output_cursor
           END,
           updated_at = now()
      FROM inserted
     WHERE workspace_processes.id = inserted.process_id
    RETURNING workspace_processes.id
)
SELECT replay.* FROM replay
UNION ALL
SELECT inserted.*
  FROM inserted
  JOIN advanced ON advanced.id = inserted.process_id
LIMIT 1;

-- name: ListWorkspaceProcessRecords :many
SELECT *
FROM workspace_process_records
WHERE process_id = sqlc.arg(process_id)
  AND stream = sqlc.arg(stream)
  AND offset_end > sqlc.arg(after_offset)
ORDER BY offset_start, id
LIMIT sqlc.arg(row_limit);

-- name: CollectWorkspaceProcessRecordPayload :one
UPDATE workspace_process_records
SET data = NULL,
    artifact_id = NULL,
    artifact_kind = NULL,
    artifact_digest = NULL,
    payload_collected_at = now()
WHERE id = sqlc.arg(id)
  AND payload_collected_at IS NULL
  AND payload_expires_at IS NOT NULL
  AND payload_expires_at <= now()
RETURNING *;
