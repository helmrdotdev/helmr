-- name: GetRunTelemetryFrontier :one
SELECT
    COALESCE(MAX(id), 0)::bigint AS observed_seq,
    COALESCE(
        MIN(id) FILTER (
            WHERE state <> 'written' OR written_at IS NULL
        ),
        0
    )::bigint AS pending_seq,
    COALESCE(
        BOOL_OR(state = 'dead_lettered'),
        false
    )::boolean AS dead_lettered_after
FROM telemetry_outbox
WHERE org_id = sqlc.arg(org_id)
  AND run_id = sqlc.arg(run_id)
  AND stream_kind = sqlc.arg(stream_kind)::telemetry_stream_kind
  AND id > sqlc.arg(after_seq)::bigint
  AND (
      sqlc.arg(through_seq)::bigint = 0
      OR id <= sqlc.arg(through_seq)::bigint
  )
  AND (
      cardinality(sqlc.arg(filter_values)::text[]) = 0
      OR (
          stream_kind = 'event'
          AND severity = ANY(sqlc.arg(filter_values)::text[])
      )
      OR (
          stream_kind = 'run_log'
          AND stream_name = 'structured'
          AND severity = ANY(sqlc.arg(filter_values)::text[])
      )
  );
