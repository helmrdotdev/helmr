-- name: AppendRunEventForExecution :one
WITH event_args AS (
    SELECT sqlc.arg(kind)::text AS event_kind,
           sqlc.arg(payload)::jsonb AS event_payload
),
current_run_lease AS (
    SELECT runs.id,
           runs.worker_group_id,
           runs.project_id,
           runs.environment_id,
           runs.trace_id,
           runs.state_version,
           run_leases.id AS run_lease_id,
           run_leases.span_id,
           run_leases.parent_span_id,
           run_leases.traceparent,
           run_leases.attempt_number
      FROM runs
      JOIN run_leases ON run_leases.id = runs.current_run_lease_id
                     AND run_leases.org_id = runs.org_id
                     AND run_leases.run_id = runs.id
      JOIN worker_groups ON worker_groups.id = runs.worker_group_id
                        AND worker_groups.state IN ('active', 'draining')
     WHERE runs.org_id = sqlc.arg(org_id)
       AND runs.worker_group_id = sqlc.arg(worker_group_id)
       AND runs.id = sqlc.arg(run_id)
       AND runs.status = 'running'
       AND run_leases.worker_group_id = sqlc.arg(worker_group_id)
       AND run_leases.id = sqlc.arg(run_lease_id)
       AND run_leases.worker_instance_id = sqlc.arg(worker_instance_id)
       AND run_leases.status IN ('leased', 'running')
       AND run_leases.lease_expires_at > now()
),
appended AS (
    INSERT INTO telemetry_outbox (
        org_id, worker_group_id, stream_kind, source_kind, source_id, project_id,
        environment_id, run_id, run_lease_id, attempt_number, trace_id, span_id,
        parent_span_id, traceparent, category, severity, source, kind, message,
        payload, redaction_class, snapshot_version, observed_at
    )
    SELECT sqlc.arg(org_id)::uuid,
           current_run_lease.worker_group_id,
           'event',
           'run',
           current_run_lease.id,
           current_run_lease.project_id,
           current_run_lease.environment_id,
           current_run_lease.id,
           current_run_lease.run_lease_id,
           current_run_lease.attempt_number,
           current_run_lease.trace_id,
           current_run_lease.span_id,
           current_run_lease.parent_span_id,
           current_run_lease.traceparent,
           CASE WHEN event_args.event_kind = 'log' THEN 'log' ELSE 'guest' END,
           'info',
           'worker',
           event_args.event_kind,
           event_args.event_kind,
           event_args.event_payload,
           'sensitive',
           current_run_lease.state_version,
           now()
      FROM current_run_lease
      CROSS JOIN event_args
    RETURNING telemetry_outbox.run_id AS id,
              telemetry_outbox.worker_group_id,
              telemetry_outbox.project_id,
              telemetry_outbox.environment_id,
              telemetry_outbox.trace_id,
              COALESCE(telemetry_outbox.snapshot_version, 0)::bigint AS state_version,
              telemetry_outbox.run_lease_id,
              COALESCE(telemetry_outbox.span_id, '')::text AS span_id,
              telemetry_outbox.parent_span_id,
              COALESCE(telemetry_outbox.traceparent, '')::text AS traceparent,
              COALESCE(telemetry_outbox.attempt_number, 0)::integer AS attempt_number,
              telemetry_outbox.kind AS event_kind,
              telemetry_outbox.payload AS event_payload
)
SELECT *
  FROM appended;

-- name: AppendRunEvent :one
WITH event_args AS (
    SELECT sqlc.arg(kind)::text AS event_kind,
           sqlc.arg(payload)::jsonb AS event_payload
),
target_run AS (
    SELECT runs.id,
           runs.worker_group_id,
           runs.project_id,
           runs.environment_id,
           runs.current_attempt_number,
           runs.trace_id,
           runs.root_span_id,
           runs.state_version
      FROM runs
     WHERE runs.org_id = sqlc.arg(org_id)
       AND runs.id = sqlc.arg(run_id)
),
appended AS (
    INSERT INTO telemetry_outbox (
        org_id, worker_group_id, stream_kind, source_kind, source_id, project_id,
        environment_id, run_id, attempt_number, trace_id, span_id, traceparent,
        category, severity, source, kind, message, payload, redaction_class,
        snapshot_version, observed_at
    )
    SELECT sqlc.arg(org_id)::uuid,
           target_run.worker_group_id,
           'event',
           'run',
           target_run.id,
           target_run.project_id,
           target_run.environment_id,
           target_run.id,
           target_run.current_attempt_number,
           target_run.trace_id,
           target_run.root_span_id,
           '00-' || target_run.trace_id || '-' || target_run.root_span_id || '-01',
           'system',
           'info',
           'control',
           event_args.event_kind,
           event_args.event_kind,
           event_args.event_payload,
           'internal',
           target_run.state_version,
           now()
      FROM target_run
      CROSS JOIN event_args
    RETURNING telemetry_outbox.run_id AS id,
              telemetry_outbox.worker_group_id,
              telemetry_outbox.project_id,
              telemetry_outbox.environment_id,
              COALESCE(telemetry_outbox.attempt_number, 0)::integer AS current_attempt_number,
              telemetry_outbox.trace_id,
              COALESCE(telemetry_outbox.span_id, '')::text AS root_span_id,
              COALESCE(telemetry_outbox.snapshot_version, 0)::bigint AS state_version,
              telemetry_outbox.kind AS event_kind,
              telemetry_outbox.payload AS event_payload
)
SELECT *
  FROM appended;

-- name: AppendDeploymentEvent :one
WITH target_deployment AS (
    SELECT deployments.id,
           deployments.org_id,
           deployments.build_worker_group_id AS worker_group_id,
           deployments.project_id,
           deployments.environment_id
      FROM deployments
     WHERE deployments.org_id = sqlc.arg(org_id)
       AND deployments.project_id = sqlc.arg(project_id)
       AND deployments.environment_id = sqlc.arg(environment_id)
       AND deployments.id = sqlc.arg(deployment_id)
),
appended AS (
    INSERT INTO telemetry_outbox (
        org_id, worker_group_id, stream_kind, source_kind, source_id, project_id,
        environment_id, deployment_id, category, severity, source, kind, message,
        payload, redaction_class, observed_at
    )
    SELECT target_deployment.org_id,
           target_deployment.worker_group_id,
           'event',
           'deployment',
           target_deployment.id,
           target_deployment.project_id,
           target_deployment.environment_id,
           target_deployment.id,
           COALESCE(NULLIF(sqlc.arg(category)::text, ''), 'system'),
           COALESCE(NULLIF(sqlc.arg(severity)::text, ''), 'info'),
           COALESCE(NULLIF(sqlc.arg(source)::text, ''), 'control'),
           sqlc.arg(kind)::text,
           COALESCE(sqlc.arg(message)::text, ''),
           COALESCE(sqlc.arg(payload)::jsonb, '{}'::jsonb),
           COALESCE(NULLIF(sqlc.arg(redaction_class)::text, ''), 'internal'),
           now()
      FROM target_deployment
    RETURNING telemetry_outbox.deployment_id AS id,
              telemetry_outbox.org_id,
              telemetry_outbox.worker_group_id,
              telemetry_outbox.project_id,
              telemetry_outbox.environment_id
)
SELECT *
  FROM appended;

-- name: ClaimEventOutbox :many
WITH claimed AS (
    SELECT telemetry_outbox.id
      FROM telemetry_outbox
     WHERE telemetry_outbox.stream_kind = 'event'
       AND telemetry_outbox.published_at IS NULL
       AND (telemetry_outbox.publish_locked_until IS NULL OR telemetry_outbox.publish_locked_until < now())
       AND telemetry_outbox.state <> 'dead_lettered'
       AND NOT EXISTS (
            SELECT 1
              FROM telemetry_outbox AS earlier_outbox
             WHERE earlier_outbox.stream_kind = 'event'
               AND earlier_outbox.published_at IS NULL
               AND earlier_outbox.state <> 'dead_lettered'
               AND earlier_outbox.org_id = telemetry_outbox.org_id
               AND earlier_outbox.worker_group_id = telemetry_outbox.worker_group_id
               AND earlier_outbox.source_kind = telemetry_outbox.source_kind
               AND earlier_outbox.source_id = telemetry_outbox.source_id
               AND earlier_outbox.id < telemetry_outbox.id
       )
     ORDER BY telemetry_outbox.id ASC
     LIMIT sqlc.arg(row_limit)
     FOR UPDATE SKIP LOCKED
),
updated AS (
    UPDATE telemetry_outbox
       SET publish_locked_until = now() + sqlc.arg(lease_duration)::interval,
           publish_attempts = telemetry_outbox.publish_attempts + 1,
           updated_at = now(),
           last_error = ''
      FROM claimed
     WHERE telemetry_outbox.id = claimed.id
    RETURNING telemetry_outbox.*
)
SELECT updated.id AS outbox_id,
       ('helmr:events:' || updated.org_id::text || ':' || updated.worker_group_id || ':' || updated.source_kind || ':' || updated.source_id::text)::text AS stream_key,
       updated.publish_attempts AS attempts,
       updated.id AS event_record_id,
       updated.source_kind::event_subject_type AS subject_type,
       updated.source_id AS subject_id,
       updated.id AS seq,
       updated.org_id,
       updated.project_id,
       updated.environment_id,
       updated.run_id,
       updated.deployment_id,
       updated.run_lease_id,
       updated.attempt_number,
       updated.trace_id,
       updated.span_id,
       updated.parent_span_id,
       updated.traceparent,
       updated.category,
       updated.severity,
       updated.source,
       updated.kind,
       updated.message,
       updated.payload,
       updated.redaction_class,
       updated.snapshot_version,
       updated.observed_at AS occurred_at,
       updated.created_at
  FROM updated
 ORDER BY updated.id ASC;

-- name: MarkEventOutboxPublished :exec
UPDATE telemetry_outbox
   SET published_at = now(),
       publish_locked_until = NULL,
       updated_at = now(),
       last_error = ''
 WHERE id = sqlc.arg(id);

-- name: MarkEventOutboxFailed :exec
UPDATE telemetry_outbox
   SET publish_locked_until = now() + sqlc.arg(retry_after)::interval,
       updated_at = now(),
       last_error = sqlc.arg(last_error)
 WHERE id = sqlc.arg(id)
   AND published_at IS NULL;
