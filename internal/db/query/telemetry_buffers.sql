-- name: UpsertRunLogWatermark :one
INSERT INTO run_log_watermarks (org_id, cell_id, run_id, stream_name, watermark_seq, watermark_observed_at)
VALUES (sqlc.arg(org_id), sqlc.arg(cell_id), sqlc.arg(run_id), sqlc.arg(stream_name), sqlc.arg(watermark_seq), now())
ON CONFLICT (org_id, cell_id, run_id, stream_name)
DO UPDATE SET watermark_seq = GREATEST(run_log_watermarks.watermark_seq, EXCLUDED.watermark_seq),
              watermark_observed_at = CASE
                WHEN EXCLUDED.watermark_seq >= run_log_watermarks.watermark_seq THEN EXCLUDED.watermark_observed_at
                ELSE run_log_watermarks.watermark_observed_at
              END,
              updated_at = now()
RETURNING *;

-- name: GetRunLogWatermark :one
SELECT COALESCE(
    (
        SELECT MIN(telemetry_outbox.seq) - 1
          FROM telemetry_outbox
         WHERE telemetry_outbox.org_id = sqlc.arg(org_id)
           AND telemetry_outbox.cell_id = sqlc.arg(cell_id)
           AND telemetry_outbox.stream_kind = 'run_log'
           AND telemetry_outbox.source_kind = 'run'
           AND telemetry_outbox.source_id = sqlc.arg(run_id)
           AND telemetry_outbox.written_at IS NULL
           AND telemetry_outbox.state <> 'dead_lettered'
    ),
    (
        SELECT MIN(watermark_seq)
          FROM run_log_watermarks
         WHERE org_id = sqlc.arg(org_id)
           AND cell_id = sqlc.arg(cell_id)
           AND run_id = sqlc.arg(run_id)
    ),
    0
)::bigint AS watermark_seq;

-- name: ListRunLogChunksAfterWatermark :many
SELECT chunks.*
  FROM run_log_hot_chunks AS chunks
 WHERE chunks.org_id = sqlc.arg(org_id)
   AND chunks.cell_id = sqlc.arg(cell_id)
   AND chunks.run_id = sqlc.arg(run_id)
   AND chunks.seq > GREATEST(sqlc.arg(watermark_seq)::bigint, sqlc.arg(seq)::bigint)
 ORDER BY chunks.seq
 LIMIT sqlc.arg(row_limit);

-- name: PruneRunLogChunksPastWatermark :many
WITH watermark AS (
    SELECT COALESCE(
        (
            SELECT MIN(telemetry_outbox.seq) - 1
              FROM telemetry_outbox
             WHERE telemetry_outbox.org_id = sqlc.arg(org_id)
               AND telemetry_outbox.cell_id = sqlc.arg(cell_id)
               AND telemetry_outbox.stream_kind = 'run_log'
               AND telemetry_outbox.source_kind = 'run'
               AND telemetry_outbox.source_id = sqlc.arg(run_id)
               AND telemetry_outbox.written_at IS NULL
               AND telemetry_outbox.state <> 'dead_lettered'
        ),
        (
            SELECT MIN(watermark_seq)
              FROM run_log_watermarks
             WHERE org_id = sqlc.arg(org_id)
               AND cell_id = sqlc.arg(cell_id)
               AND run_id = sqlc.arg(run_id)
        ),
        0
    )::bigint AS seq
)
DELETE FROM run_log_hot_chunks
USING watermark
WHERE run_log_hot_chunks.org_id = sqlc.arg(org_id)
  AND run_log_hot_chunks.cell_id = sqlc.arg(cell_id)
  AND run_log_hot_chunks.run_id = sqlc.arg(run_id)
  AND run_log_hot_chunks.seq <= watermark.seq
  AND run_log_hot_chunks.created_at < now() - sqlc.arg(prune_grace)::interval
RETURNING run_log_hot_chunks.seq;

-- name: UpsertEventWatermark :one
INSERT INTO event_watermarks (org_id, cell_id, subject_kind, subject_id, watermark_seq, watermark_observed_at)
VALUES (sqlc.arg(org_id), sqlc.arg(cell_id), sqlc.arg(subject_type)::event_subject_type, sqlc.arg(subject_id), sqlc.arg(watermark_seq), now())
ON CONFLICT (org_id, cell_id, subject_kind, subject_id)
DO UPDATE SET watermark_seq = GREATEST(event_watermarks.watermark_seq, EXCLUDED.watermark_seq),
              watermark_observed_at = CASE
                WHEN EXCLUDED.watermark_seq >= event_watermarks.watermark_seq THEN EXCLUDED.watermark_observed_at
                ELSE event_watermarks.watermark_observed_at
              END,
              updated_at = now()
RETURNING *;

-- name: GetEventWatermark :one
SELECT COALESCE(
    (
        SELECT MIN(telemetry_outbox.seq) - 1
          FROM telemetry_outbox
         WHERE telemetry_outbox.org_id = sqlc.arg(org_id)
           AND telemetry_outbox.cell_id = sqlc.arg(cell_id)
           AND telemetry_outbox.stream_kind = 'event'
           AND telemetry_outbox.source_kind = sqlc.arg(subject_type)
           AND telemetry_outbox.source_id = sqlc.arg(subject_id)
           AND telemetry_outbox.written_at IS NULL
           AND telemetry_outbox.state <> 'dead_lettered'
    ),
    (
        SELECT watermark_seq
          FROM event_watermarks
         WHERE org_id = sqlc.arg(org_id)
           AND cell_id = sqlc.arg(cell_id)
           AND subject_kind = sqlc.arg(subject_type)::event_subject_type
           AND subject_id = sqlc.arg(subject_id)
    ),
    0
)::bigint AS watermark_seq;

-- name: ListSubjectEventsAfterWatermark :many
SELECT events.*
  FROM event_hot_payloads AS events
 WHERE events.org_id = sqlc.arg(org_id)
   AND events.cell_id = sqlc.arg(cell_id)
   AND events.subject_type = sqlc.arg(subject_type)::event_subject_type
   AND events.subject_id = sqlc.arg(subject_id)
   AND events.seq > GREATEST(sqlc.arg(watermark_seq)::bigint, sqlc.arg(seq)::bigint)
 ORDER BY events.seq
 LIMIT sqlc.arg(row_limit);

-- name: PruneEventsPastWatermark :many
DELETE FROM event_hot_payloads
USING event_watermarks
WHERE event_hot_payloads.org_id = sqlc.arg(org_id)
  AND event_hot_payloads.cell_id = sqlc.arg(cell_id)
  AND event_hot_payloads.subject_type = sqlc.arg(subject_type)::event_subject_type
  AND event_hot_payloads.subject_id = sqlc.arg(subject_id)
  AND event_watermarks.org_id = event_hot_payloads.org_id
  AND event_watermarks.cell_id = event_hot_payloads.cell_id
  AND event_watermarks.subject_kind = event_hot_payloads.subject_type
  AND event_watermarks.subject_id = event_hot_payloads.subject_id
  AND event_hot_payloads.seq <= event_watermarks.watermark_seq
  AND event_hot_payloads.created_at < now() - sqlc.arg(prune_grace)::interval
  AND NOT EXISTS (
    SELECT 1
      FROM telemetry_outbox
     WHERE telemetry_outbox.stream_kind = 'event'
       AND telemetry_outbox.org_id = event_hot_payloads.org_id
       AND telemetry_outbox.cell_id = event_hot_payloads.cell_id
       AND telemetry_outbox.source_kind = event_hot_payloads.subject_type::text
       AND telemetry_outbox.source_id = event_hot_payloads.subject_id
       AND telemetry_outbox.seq = event_hot_payloads.seq
       AND telemetry_outbox.state <> 'dead_lettered'
       AND (telemetry_outbox.published_at IS NULL OR telemetry_outbox.written_at IS NULL)
  )
RETURNING event_hot_payloads.seq;

-- name: UpsertTerminalOutputWatermark :one
INSERT INTO terminal_output_watermarks (org_id, cell_id, workspace_id, resource_kind, resource_id, stream_name, watermark_offset, watermark_observed_at)
VALUES (sqlc.arg(org_id), sqlc.arg(cell_id), sqlc.arg(workspace_id), sqlc.arg(resource_kind), sqlc.arg(resource_id), sqlc.arg(stream_name), sqlc.arg(watermark_offset), now())
ON CONFLICT (org_id, cell_id, workspace_id, resource_kind, resource_id, stream_name)
DO UPDATE SET watermark_offset = GREATEST(terminal_output_watermarks.watermark_offset, EXCLUDED.watermark_offset),
              watermark_observed_at = CASE
                WHEN EXCLUDED.watermark_offset >= terminal_output_watermarks.watermark_offset THEN EXCLUDED.watermark_observed_at
                ELSE terminal_output_watermarks.watermark_observed_at
              END,
              updated_at = now()
RETURNING *;

-- name: GetTerminalOutputWatermark :one
SELECT COALESCE(
    (
        SELECT MIN(telemetry_outbox.seq)
          FROM telemetry_outbox
         WHERE telemetry_outbox.org_id = sqlc.arg(org_id)
           AND telemetry_outbox.cell_id = sqlc.arg(cell_id)
           AND telemetry_outbox.stream_kind = 'terminal_output'
           AND telemetry_outbox.source_kind = sqlc.arg(resource_kind)
           AND telemetry_outbox.source_id = sqlc.arg(resource_id)
           AND telemetry_outbox.stream_name = sqlc.arg(stream_name)
           AND telemetry_outbox.written_at IS NULL
           AND telemetry_outbox.state <> 'dead_lettered'
    ),
    (
        SELECT watermark_offset
          FROM terminal_output_watermarks
         WHERE org_id = sqlc.arg(org_id)
           AND cell_id = sqlc.arg(cell_id)
           AND workspace_id = sqlc.arg(workspace_id)
           AND resource_kind = sqlc.arg(resource_kind)
           AND resource_id = sqlc.arg(resource_id)
           AND stream_name = sqlc.arg(stream_name)
    ),
    0
)::bigint AS watermark_offset;

-- name: ListWorkspaceExecStreamChunksAfterWatermark :many
SELECT chunks.*
  FROM workspace_exec_stream_chunks AS chunks
 WHERE chunks.org_id = sqlc.arg(org_id)
   AND chunks.cell_id = sqlc.arg(cell_id)
   AND chunks.project_id = sqlc.arg(project_id)
   AND chunks.environment_id = sqlc.arg(environment_id)
   AND chunks.workspace_id = sqlc.arg(workspace_id)
   AND chunks.exec_id = sqlc.arg(exec_id)
   AND chunks.stream = sqlc.arg(stream)::workspace_exec_stream
   AND chunks.offset_end > GREATEST(sqlc.arg(watermark_offset)::bigint, sqlc.arg(cursor_offset)::bigint)
 ORDER BY chunks.offset_start
 LIMIT sqlc.arg(limit_count);

-- name: ListWorkspacePtyStreamChunksAfterWatermark :many
SELECT chunks.*
  FROM workspace_pty_stream_chunks AS chunks
 WHERE chunks.org_id = sqlc.arg(org_id)
   AND chunks.cell_id = sqlc.arg(cell_id)
   AND chunks.project_id = sqlc.arg(project_id)
   AND chunks.environment_id = sqlc.arg(environment_id)
   AND chunks.workspace_id = sqlc.arg(workspace_id)
   AND chunks.pty_session_id = sqlc.arg(pty_session_id)
   AND chunks.stream = sqlc.arg(stream)::workspace_pty_stream
   AND chunks.offset_end > GREATEST(sqlc.arg(watermark_offset)::bigint, sqlc.arg(cursor_offset)::bigint)
 ORDER BY chunks.offset_start
 LIMIT sqlc.arg(limit_count);

-- name: PruneWorkspaceExecStreamChunksPastWatermark :many
DELETE FROM workspace_exec_stream_chunks
USING terminal_output_watermarks
WHERE workspace_exec_stream_chunks.org_id = sqlc.arg(org_id)
  AND workspace_exec_stream_chunks.cell_id = sqlc.arg(cell_id)
  AND workspace_exec_stream_chunks.workspace_id = sqlc.arg(workspace_id)
  AND workspace_exec_stream_chunks.exec_id = sqlc.arg(exec_id)
  AND terminal_output_watermarks.org_id = workspace_exec_stream_chunks.org_id
  AND terminal_output_watermarks.cell_id = workspace_exec_stream_chunks.cell_id
  AND terminal_output_watermarks.workspace_id = workspace_exec_stream_chunks.workspace_id
  AND terminal_output_watermarks.resource_kind = 'workspace_exec'
  AND terminal_output_watermarks.resource_id = workspace_exec_stream_chunks.exec_id
  AND terminal_output_watermarks.stream_name = workspace_exec_stream_chunks.stream::text
  AND workspace_exec_stream_chunks.offset_end <= terminal_output_watermarks.watermark_offset
  AND workspace_exec_stream_chunks.created_at < now() - sqlc.arg(prune_grace)::interval
  AND NOT EXISTS (
        SELECT 1
          FROM telemetry_outbox
         WHERE telemetry_outbox.org_id = workspace_exec_stream_chunks.org_id
           AND telemetry_outbox.cell_id = workspace_exec_stream_chunks.cell_id
           AND telemetry_outbox.stream_kind = 'terminal_output'
           AND telemetry_outbox.source_kind = 'workspace_exec'
           AND telemetry_outbox.source_id = workspace_exec_stream_chunks.exec_id
           AND telemetry_outbox.stream_name = workspace_exec_stream_chunks.stream::text
           AND telemetry_outbox.seq = workspace_exec_stream_chunks.offset_start
           AND telemetry_outbox.written_at IS NULL
           AND telemetry_outbox.state <> 'dead_lettered'
  )
RETURNING workspace_exec_stream_chunks.offset_end;

-- name: PruneWorkspacePtyStreamChunksPastWatermark :many
DELETE FROM workspace_pty_stream_chunks
USING terminal_output_watermarks
WHERE workspace_pty_stream_chunks.org_id = sqlc.arg(org_id)
  AND workspace_pty_stream_chunks.cell_id = sqlc.arg(cell_id)
  AND workspace_pty_stream_chunks.workspace_id = sqlc.arg(workspace_id)
  AND workspace_pty_stream_chunks.pty_session_id = sqlc.arg(pty_session_id)
  AND terminal_output_watermarks.org_id = workspace_pty_stream_chunks.org_id
  AND terminal_output_watermarks.cell_id = workspace_pty_stream_chunks.cell_id
  AND terminal_output_watermarks.workspace_id = workspace_pty_stream_chunks.workspace_id
  AND terminal_output_watermarks.resource_kind = 'workspace_pty'
  AND terminal_output_watermarks.resource_id = workspace_pty_stream_chunks.pty_session_id
  AND terminal_output_watermarks.stream_name = workspace_pty_stream_chunks.stream::text
  AND workspace_pty_stream_chunks.offset_end <= terminal_output_watermarks.watermark_offset
  AND workspace_pty_stream_chunks.created_at < now() - sqlc.arg(prune_grace)::interval
  AND NOT EXISTS (
        SELECT 1
          FROM telemetry_outbox
         WHERE telemetry_outbox.org_id = workspace_pty_stream_chunks.org_id
           AND telemetry_outbox.cell_id = workspace_pty_stream_chunks.cell_id
           AND telemetry_outbox.stream_kind = 'terminal_output'
           AND telemetry_outbox.source_kind = 'workspace_pty'
           AND telemetry_outbox.source_id = workspace_pty_stream_chunks.pty_session_id
           AND telemetry_outbox.stream_name = workspace_pty_stream_chunks.stream::text
           AND telemetry_outbox.seq = workspace_pty_stream_chunks.offset_start
           AND telemetry_outbox.written_at IS NULL
           AND telemetry_outbox.state <> 'dead_lettered'
  )
RETURNING workspace_pty_stream_chunks.offset_end;
