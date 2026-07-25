-- name: GetRunTelemetryFrontier :one
SELECT
    COALESCE(MAX(id), 0)::bigint AS observed_seq,
    COALESCE(MAX(id) FILTER (WHERE state = 'written'), 0)::bigint AS projected_seq,
    COALESCE(
        BOOL_OR(state = 'dead_lettered' AND id > sqlc.arg(after_seq)::bigint),
        false
    )::boolean AS dead_lettered_after
FROM telemetry_outbox
WHERE org_id = sqlc.arg(org_id)
  AND run_id = sqlc.arg(run_id)
  AND stream_kind = sqlc.arg(stream_kind)::telemetry_stream_kind
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
