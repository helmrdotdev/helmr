-- name: InsertCommandLogChunk :one
-- Serialize admission with terminalization; producers must flush before completing.
WITH live_command AS MATERIALIZED (
    SELECT e.*, scope.org_id, scope.project_id FROM computer_commands AS e
     JOIN environments AS scope ON scope.id=e.environment_id
     WHERE scope.org_id = sqlc.arg(org_id)
       AND scope.project_id = sqlc.arg(project_id)
       AND e.environment_id = sqlc.arg(environment_id)
       AND e.id = sqlc.arg(command_id)
       AND e.terminal_at IS NULL
     FOR SHARE OF e
)
INSERT INTO telemetry_outbox (
    org_id, project_id, environment_id, stream_kind, source_kind, source_id,
    command_id, stream_name, idempotency_key, content, size_bytes, observed_seq,
    source, observed_at
)
SELECT command.org_id, command.project_id, command.environment_id, 'command_log', 'command', command.id,
       command.id, sqlc.arg(stream_name)::text,
       sqlc.arg(stream_name)::text || ':' || sqlc.arg(observed_seq)::bigint::text,
       sqlc.arg(content)::bytea, octet_length(sqlc.arg(content)::bytea),
       sqlc.arg(observed_seq)::bigint, 'worker', sqlc.arg(observed_at)::timestamptz
  FROM live_command AS command
 WHERE octet_length(sqlc.arg(content)::bytea) <= 196608
ON CONFLICT (environment_id, command_id, stream_name, observed_seq)
    WHERE stream_kind = 'command_log'
DO UPDATE SET content = telemetry_outbox.content
    WHERE telemetry_outbox.content = excluded.content
      AND telemetry_outbox.size_bytes = excluded.size_bytes
      AND telemetry_outbox.observed_at = excluded.observed_at
RETURNING id, created_at;

-- The caller already holds the producer's worker, group, command and lease locks
-- and compared token claims under them. Recheck wall-clock expiry immediately
-- before committing logs.
-- name: CommandLogProducerStillAuthorized :one
SELECT w.status IN ('active', 'draining')
   AND g.status IN ('active', 'paused', 'draining')
   AND clock_timestamp() < sqlc.arg(expires_at)::timestamptz AS authorized
  FROM worker_hosts w JOIN worker_groups g ON g.id = w.worker_group_id
 WHERE w.id = sqlc.arg(worker_host_id)
   AND g.id = sqlc.arg(worker_group_id)
   AND w.current_epoch = sqlc.arg(worker_epoch)::bigint;

-- name: GetCommandLogFrontier :one
-- Read after the sink page. An undelivered chunk in its range must not be exposed
-- as a permanent gap or skipped by a reconnect cursor.
SELECT COALESCE(MAX(observed_seq), -1)::bigint AS observed_seq,
       COALESCE(MIN(observed_seq) FILTER (
           WHERE status <> 'written' OR written_at IS NULL
       ), -1)::bigint AS pending_seq
  FROM telemetry_outbox
 WHERE org_id = sqlc.arg(org_id)
   AND environment_id = sqlc.arg(environment_id)
   AND command_id = sqlc.arg(command_id)
   AND stream_kind = 'command_log'
   AND stream_name = sqlc.arg(stream_name)
   AND observed_seq > sqlc.arg(after_observed_seq)::bigint
   AND (sqlc.narg(through_observed_seq)::bigint IS NULL
        OR observed_seq <= sqlc.narg(through_observed_seq)::bigint)
   AND created_at >= statement_timestamp() - interval '90 days';
