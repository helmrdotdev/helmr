-- name: AppendRunEvent :one
WITH event_args AS (
    SELECT sqlc.arg(kind)::text AS event_kind,
           sqlc.arg(payload)::jsonb AS event_payload
),
target_run AS (
    SELECT runs.id,
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
        org_id, stream_kind, source_kind, source_id, project_id,
        environment_id, run_id, attempt_number, trace_id, span_id, traceparent,
        category, severity, source, kind, message, payload, redaction_class,
        snapshot_version, observed_at
    )
    SELECT sqlc.arg(org_id)::uuid,
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
        org_id, stream_kind, source_kind, source_id, project_id,
        environment_id, deployment_id, category, severity, source, kind, message,
        payload, redaction_class, observed_at
    )
    SELECT target_deployment.org_id,
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
              telemetry_outbox.project_id,
              telemetry_outbox.environment_id
)
SELECT *
  FROM appended;
