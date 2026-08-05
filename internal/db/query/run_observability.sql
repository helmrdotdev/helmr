-- name: UpdateRunMetadata :one
UPDATE runs
   SET metadata = sqlc.arg(metadata)::jsonb,
       state_version = state_version + 1,
       updated_at = now()
 WHERE id = sqlc.arg(run_id)
   AND current_attempt_number = sqlc.arg(attempt_number)
   AND current_run_lease_id = sqlc.arg(run_lease_id)
   AND status = 'running'
RETURNING state_version;

-- name: GetRunMetadataClaimScope :one
SELECT run_leases.environment_id,
       run_leases.run_id,
       run_leases.attempt_number
  FROM run_leases
  JOIN runs
    ON runs.id = run_leases.run_id
   AND runs.environment_id = run_leases.environment_id
 WHERE run_leases.id = sqlc.arg(run_lease_id)
   AND run_leases.lease_sequence = sqlc.arg(lease_sequence)
   AND run_leases.worker_group_id = sqlc.arg(worker_group_id)
   AND run_leases.worker_instance_id = sqlc.arg(worker_instance_id)
   AND run_leases.worker_epoch = sqlc.arg(worker_epoch)
;

-- name: CreateRunMetadataEvent :one
INSERT INTO telemetry_outbox (
    org_id,
    stream_kind,
    source_kind,
    source_id,
    idempotency_key,
    project_id,
    environment_id,
    run_id,
    run_lease_id,
    attempt_number,
    trace_id,
    span_id,
    parent_span_id,
    traceparent,
    category,
    severity,
    source,
    kind,
    message,
    payload,
    redaction_class,
    retention_class,
    snapshot_version,
    observed_at
) VALUES (
    sqlc.arg(org_id),
    'event',
    'run',
    sqlc.arg(run_id),
    sqlc.arg(idempotency_key),
    sqlc.arg(project_id),
    sqlc.arg(environment_id),
    sqlc.arg(run_id),
    sqlc.arg(run_lease_id),
    sqlc.arg(attempt_number),
    sqlc.arg(trace_id),
    sqlc.arg(span_id),
    sqlc.narg(parent_span_id),
    sqlc.arg(traceparent),
    'run',
    'info',
    'runtime',
    'run.metadata.updated',
    'Run metadata updated',
    sqlc.arg(payload)::jsonb,
    'sensitive',
    'standard',
    sqlc.arg(snapshot_version),
    now()
)
RETURNING id;
