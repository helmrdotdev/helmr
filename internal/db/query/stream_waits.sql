-- name: CreateStreamWait :one
INSERT INTO stream_waits (
    id,
    org_id,
    cell_id,
    project_id,
    environment_id,
    run_wait_id,
    stream_id,
    after_sequence,
    correlation_id
)
SELECT sqlc.arg(id),
       run_waits.org_id,
       run_waits.cell_id,
       run_waits.project_id,
       run_waits.environment_id,
       run_waits.id,
       streams.id,
       sqlc.arg(after_sequence)::bigint,
       COALESCE(sqlc.arg(correlation_id)::text, '')
  FROM run_waits
  JOIN streams ON streams.org_id = run_waits.org_id
              AND streams.project_id = run_waits.project_id
              AND streams.environment_id = run_waits.environment_id
              AND streams.id = sqlc.arg(stream_id)
 WHERE run_waits.org_id = sqlc.arg(org_id)
   AND run_waits.project_id = sqlc.arg(project_id)
   AND run_waits.environment_id = sqlc.arg(environment_id)
   AND run_waits.id = sqlc.arg(run_wait_id)
   AND run_waits.kind = 'stream'
RETURNING *;

-- name: GetStreamWaitForRunWait :one
SELECT *
  FROM stream_waits
 WHERE org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND run_wait_id = sqlc.arg(run_wait_id);
