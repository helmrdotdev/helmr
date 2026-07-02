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

-- name: PruneRunLogChunksPastWatermark :many
DELETE FROM run_log_hot_chunks
USING run_log_watermarks
WHERE run_log_hot_chunks.org_id = sqlc.arg(org_id)
  AND run_log_hot_chunks.cell_id = sqlc.arg(cell_id)
  AND run_log_hot_chunks.run_id = sqlc.arg(run_id)
  AND run_log_watermarks.org_id = run_log_hot_chunks.org_id
  AND run_log_watermarks.cell_id = run_log_hot_chunks.cell_id
  AND run_log_watermarks.run_id = run_log_hot_chunks.run_id
  AND run_log_watermarks.stream_name = run_log_hot_chunks.stream::text
  AND run_log_hot_chunks.seq <= run_log_watermarks.watermark_seq
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
  AND NOT EXISTS (
    SELECT 1
      FROM telemetry_outbox
     WHERE telemetry_outbox.event_record_id = event_hot_payloads.id
       AND telemetry_outbox.cell_id = event_hot_payloads.cell_id
       AND telemetry_outbox.written_at IS NULL
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
RETURNING workspace_pty_stream_chunks.offset_end;
