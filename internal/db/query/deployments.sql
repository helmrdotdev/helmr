-- name: CreateDeployment :one
INSERT INTO deployments (
    id,
    public_id,
    org_id,
    project_id,
    environment_id,
    build_region_id,
    version,
    api_version,
    sdk_version,
    cli_version,
    bundle_format_version,
    worker_protocol_version,
    content_hash,
    deployment_source_artifact_id,
    status
)
SELECT sqlc.arg(id),
       sqlc.arg(public_id),
       sqlc.arg(org_id),
       sqlc.arg(project_id),
       sqlc.arg(environment_id),
       sqlc.arg(build_region_id),
       sqlc.arg(version),
       sqlc.arg(api_version),
       sqlc.arg(sdk_version),
       sqlc.arg(cli_version),
       sqlc.arg(bundle_format_version),
       sqlc.arg(worker_protocol_version),
       sqlc.arg(content_hash),
       sqlc.arg(deployment_source_artifact_id),
       sqlc.arg(status)::deployment_status
 WHERE EXISTS (
       SELECT 1
         FROM projects
         JOIN environments
           ON environments.org_id = projects.org_id
          AND environments.project_id = projects.id
        WHERE projects.org_id = sqlc.arg(org_id)
          AND projects.id = sqlc.arg(project_id)
          AND environments.id = sqlc.arg(environment_id)
	      AND projects.default_region_id = sqlc.arg(build_region_id)
	 )
RETURNING *;

-- name: LockDeploymentReusableBuildKey :exec
SELECT pg_advisory_xact_lock(
    hashtextextended(
        concat_ws(
            ':',
            sqlc.arg(org_id)::uuid::text,
            sqlc.arg(build_region_id)::text,
            sqlc.arg(project_id)::uuid::text,
            sqlc.arg(environment_id)::uuid::text,
            sqlc.arg(content_hash)::text
        ),
        0
    )
);

-- name: GetReusableDeploymentByContentHash :one
SELECT *
  FROM deployments
 WHERE org_id = sqlc.arg(org_id)
   AND build_region_id = sqlc.arg(build_region_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND content_hash = sqlc.arg(content_hash)
   AND status IN ('queued', 'building');

-- name: AllocateDeploymentVersion :one
WITH allocated AS (
    INSERT INTO deployment_version_counters (
        org_id,
        project_id,
        environment_id,
        prefix,
        next_ordinal
    ) VALUES (
        sqlc.arg(org_id),
        sqlc.arg(project_id),
        sqlc.arg(environment_id),
        sqlc.arg(prefix),
        2
    )
    ON CONFLICT (org_id, project_id, environment_id, prefix)
    DO UPDATE
       SET next_ordinal = deployment_version_counters.next_ordinal + 1,
           updated_at = now()
    RETURNING prefix, next_ordinal
)
SELECT concat(prefix, '.', next_ordinal - 1)::text AS version
  FROM allocated;

-- name: MarkDeploymentFailed :one
UPDATE deployments
   SET status = 'failed',
       failure = sqlc.arg(failure),
       failed_at = now()
 WHERE deployments.org_id = sqlc.arg(org_id)
   AND deployments.project_id = sqlc.arg(project_id)
   AND deployments.environment_id = sqlc.arg(environment_id)
   AND deployments.id = sqlc.arg(id)
   AND deployments.status IN ('queued', 'building')
RETURNING *;

-- name: LeaseQueuedDeploymentBuild :one
WITH candidate AS (
    SELECT deployments.*
      FROM deployments
     WHERE deployments.org_id = sqlc.arg(org_id)
       AND deployments.id = sqlc.arg(deployment_id)
       AND deployments.status IN ('queued', 'building')
       AND deployments.build_region_id = sqlc.arg(build_region_id)
       AND deployments.build_requested_cpu_millis = sqlc.arg(requested_cpu_millis)
       AND deployments.build_requested_memory_bytes = sqlc.arg(requested_memory_bytes)
       AND deployments.build_requested_workload_disk_bytes = sqlc.arg(requested_workload_disk_bytes)
       AND deployments.build_requested_scratch_bytes = sqlc.arg(requested_scratch_bytes)
       AND deployments.build_requested_build_cache_bytes = sqlc.arg(requested_build_cache_bytes)
       AND deployments.build_requested_artifact_cache_bytes = sqlc.arg(requested_artifact_cache_bytes)
       AND deployments.build_requested_executors = sqlc.arg(requested_build_executors)
       AND NOT EXISTS (
           SELECT 1 FROM deployment_build_leases
            WHERE deployment_build_leases.deployment_id = deployments.id
              AND deployment_build_leases.state IN ('assigned', 'starting', 'running')
       )
     FOR UPDATE OF deployments
),
advanced AS (
    UPDATE deployments
       SET status = 'building',
           building_at = COALESCE(deployments.building_at, now()),
           -- queued -> building begins a product attempt; a replacement lease
           -- while already building is delivery replay and retains the attempt.
           build_attempt_number = CASE
               WHEN deployments.status = 'queued' THEN deployments.build_attempt_number + 1
               ELSE deployments.build_attempt_number
           END,
           current_build_lease_id = sqlc.arg(build_lease_id),
           updated_at = now()
      FROM candidate
     WHERE deployments.id = candidate.id
    RETURNING deployments.*
),
inserted AS (
    INSERT INTO deployment_build_leases (
        id, org_id, project_id, environment_id, deployment_id, build_region_id,
        build_attempt_number, lease_sequence, worker_group_id, worker_instance_id,
        worker_epoch, worker_protocol_version, requested_cpu_millis,
        requested_memory_bytes, requested_workload_disk_bytes, requested_scratch_bytes,
        requested_build_cache_bytes, requested_artifact_cache_bytes,
        requested_build_executors, build_snapshot, trace_id, span_id,
        parent_span_id, traceparent, start_deadline_at, expires_at
    )
    SELECT sqlc.arg(build_lease_id), advanced.org_id, advanced.project_id,
           advanced.environment_id, advanced.id, advanced.build_region_id,
           advanced.build_attempt_number, sqlc.arg(lease_sequence),
           sqlc.arg(worker_group_id), sqlc.arg(build_worker_instance_id),
           sqlc.arg(worker_epoch), sqlc.arg(worker_protocol_version),
           sqlc.arg(requested_cpu_millis), sqlc.arg(requested_memory_bytes),
           sqlc.arg(requested_workload_disk_bytes), sqlc.arg(requested_scratch_bytes),
           sqlc.arg(requested_build_cache_bytes), sqlc.arg(requested_artifact_cache_bytes),
           sqlc.arg(requested_build_executors), sqlc.arg(build_snapshot),
           sqlc.narg(trace_id), sqlc.narg(span_id), sqlc.narg(parent_span_id),
           sqlc.narg(traceparent), sqlc.arg(start_deadline_at), sqlc.arg(build_lease_expires_at)
      FROM advanced
    RETURNING *
)
SELECT inserted.*,
       advanced.version,
       advanced.api_version,
       advanced.sdk_version,
       advanced.cli_version,
       advanced.bundle_format_version,
       advanced.content_hash,
       source_artifacts.digest AS deployment_source_digest,
       source_artifacts.size_bytes AS source_size_bytes,
       source_artifacts.media_type AS source_media_type,
       advanced.status AS deployment_status
  FROM inserted
  JOIN advanced ON advanced.id = inserted.deployment_id
  JOIN artifacts AS source_artifacts
    ON source_artifacts.org_id = advanced.org_id
   AND source_artifacts.project_id = advanced.project_id
   AND source_artifacts.environment_id = advanced.environment_id
   AND source_artifacts.id = advanced.deployment_source_artifact_id;

-- name: RequeueExpiredDeploymentBuildLeases :exec
WITH expired AS (
    UPDATE deployment_build_leases
       SET state = 'expired', terminal_at = now(),
           terminal_reason_code = 'lease_expired', updated_at = now()
     WHERE deployment_build_leases.state IN ('assigned','starting','running')
       AND deployment_build_leases.expires_at <= now()
    RETURNING deployment_build_leases.*
), meter_event AS (
    INSERT INTO meter_events (
        org_id, project_id, environment_id, deployment_id,
        deployment_build_lease_id, attempt_number, trace_id, span_id, meter,
        quantity, unit, measured_from, measured_to, details,
        idempotency_key, idempotency_fingerprint
    )
    SELECT expired.org_id, expired.project_id, expired.environment_id,
           expired.deployment_id, expired.id, expired.build_attempt_number,
           expired.trace_id, expired.span_id, 'active_time',
           GREATEST((extract(epoch FROM (expired.expires_at - expired.started_at)) * 1000)::bigint, 0),
           'milliseconds', expired.started_at, expired.expires_at,
           jsonb_build_object('outcome','lease_lost_requeued',
               'cpu_millis',expired.requested_cpu_millis,
               'memory_bytes',expired.requested_memory_bytes,
               'workload_disk_bytes',expired.requested_workload_disk_bytes,
               'scratch_bytes',expired.requested_scratch_bytes,
               'build_cache_bytes',expired.requested_build_cache_bytes,
               'artifact_cache_bytes',expired.requested_artifact_cache_bytes,
               'build_executors',expired.requested_build_executors),
           'build-lease-lost:' || expired.id::text,
           jsonb_build_object('quantity',GREATEST((extract(epoch FROM (expired.expires_at - expired.started_at)) * 1000)::bigint, 0),
               'unit','milliseconds','measured_from',expired.started_at,'measured_to',expired.expires_at,
               'outcome','lease_lost_requeued','cpu_millis',expired.requested_cpu_millis,
               'memory_bytes',expired.requested_memory_bytes,
               'workload_disk_bytes',expired.requested_workload_disk_bytes,
               'scratch_bytes',expired.requested_scratch_bytes,
               'build_cache_bytes',expired.requested_build_cache_bytes,
               'artifact_cache_bytes',expired.requested_artifact_cache_bytes,
               'build_executors',expired.requested_build_executors)::text
      FROM expired
     WHERE expired.started_at IS NOT NULL AND expired.started_at < expired.expires_at
    ON CONFLICT (org_id, source_type, source_id, meter, idempotency_key)
    DO UPDATE SET idempotency_fingerprint = meter_events.idempotency_fingerprint
     WHERE meter_events.idempotency_fingerprint = excluded.idempotency_fingerprint
    RETURNING *
), meter_outbox AS (
    INSERT INTO telemetry_outbox (
        org_id, stream_kind, source_kind, source_id, project_id, environment_id,
        deployment_id, meter_event_id, attempt_number, trace_id, span_id,
        kind, payload, idempotency_key, observed_at
    )
    SELECT org_id, 'meter_event', source_type, source_id, project_id, environment_id,
           deployment_id, id, attempt_number, trace_id, span_id,
           meter, details, idempotency_key, occurred_at
      FROM meter_event
    ON CONFLICT DO NOTHING
    RETURNING meter_event_id
)
UPDATE deployments
   SET current_build_lease_id = NULL, updated_at = now()
  FROM expired
 WHERE deployments.org_id = expired.org_id
   AND deployments.id = expired.deployment_id
   AND deployments.current_build_lease_id = expired.id
   AND deployments.status = 'building'
   AND (expired.started_at IS NULL OR EXISTS (
       SELECT 1 FROM meter_outbox WHERE meter_outbox.meter_event_id = (
           SELECT id FROM meter_event WHERE meter_event.deployment_build_lease_id = expired.id
       )
   ));

-- name: ListQueuedDeploymentBuildCandidates :many
SELECT deployments.org_id,
       deployments.project_id,
       deployments.environment_id,
       deployments.id AS deployment_id,
       deployments.build_region_id,
       deployments.build_requested_cpu_millis,
       deployments.build_requested_memory_bytes,
       deployments.build_requested_workload_disk_bytes,
       deployments.build_requested_scratch_bytes,
       deployments.build_requested_build_cache_bytes,
       deployments.build_requested_artifact_cache_bytes,
       deployments.build_requested_executors,
       CASE WHEN deployments.status = 'queued'
            THEN deployments.build_attempt_number + 1
            ELSE deployments.build_attempt_number
       END::int AS build_attempt_number,
       (COALESCE((
           SELECT max(deployment_build_leases.lease_sequence)
             FROM deployment_build_leases
            WHERE deployment_build_leases.deployment_id = deployments.id
              AND deployment_build_leases.build_attempt_number = CASE
                  WHEN deployments.status = 'queued' THEN deployments.build_attempt_number + 1
                  ELSE deployments.build_attempt_number
              END
       ), 0) + 1)::bigint AS lease_sequence,
       deployments.created_at AS queue_timestamp
  FROM deployments
 WHERE deployments.build_region_id = sqlc.arg(build_region_id)
   AND deployments.status IN ('queued', 'building')
   AND NOT EXISTS (
       SELECT 1 FROM deployment_build_leases
        WHERE deployment_build_leases.deployment_id = deployments.id
          AND deployment_build_leases.state IN ('assigned', 'starting', 'running')
   )
 ORDER BY row_number() OVER (
              PARTITION BY deployments.org_id
              ORDER BY deployments.created_at, deployments.id
          ),
          deployments.created_at, deployments.id
 LIMIT sqlc.arg(limit_count);

-- name: ListQueuedDeploymentBuildRegions :many
SELECT DISTINCT deployments.build_region_id
  FROM deployments
 WHERE deployments.status IN ('queued','building')
   AND NOT EXISTS (
       SELECT 1 FROM deployment_build_leases
        WHERE deployment_build_leases.deployment_id = deployments.id
          AND deployment_build_leases.state IN ('assigned','starting','running')
   )
 ORDER BY deployments.build_region_id
 LIMIT sqlc.arg(limit_count);

-- name: ClaimDeploymentBuildLease :one
UPDATE deployment_build_leases
   SET state = 'starting', claimed_at = COALESCE(claimed_at, now()),
       renewed_at = now(), expires_at = sqlc.arg(expires_at), updated_at = now()
 WHERE org_id = sqlc.arg(org_id) AND deployment_id = sqlc.arg(deployment_id)
   AND id = sqlc.arg(build_lease_id)
   AND build_attempt_number = sqlc.arg(build_attempt_number)
   AND lease_sequence = sqlc.arg(lease_sequence)
   AND worker_group_id = sqlc.arg(worker_group_id)
   AND worker_instance_id = sqlc.arg(worker_instance_id)
   AND worker_epoch = sqlc.arg(worker_epoch)
   AND state = 'assigned' AND start_deadline_at > now() AND expires_at > now()
RETURNING *;

-- name: ClaimNextDeploymentBuildLease :one
WITH candidate AS (
    SELECT deployment_build_leases.*
      FROM deployment_build_leases
     WHERE deployment_build_leases.worker_group_id = sqlc.arg(worker_group_id)
       AND deployment_build_leases.worker_instance_id = sqlc.arg(worker_instance_id)
       AND deployment_build_leases.worker_epoch = sqlc.arg(worker_epoch)
       AND deployment_build_leases.worker_protocol_version = sqlc.arg(worker_protocol_version)
       AND deployment_build_leases.state = 'assigned'
       AND deployment_build_leases.start_deadline_at > now()
       AND deployment_build_leases.expires_at > now()
     ORDER BY deployment_build_leases.assigned_at, deployment_build_leases.id
     LIMIT 1
     FOR UPDATE SKIP LOCKED
), claimed AS (
    UPDATE deployment_build_leases
       SET state = 'starting', claimed_at = now(), renewed_at = now(),
           expires_at = sqlc.arg(expires_at), updated_at = now()
      FROM candidate
     WHERE deployment_build_leases.id = candidate.id
    RETURNING deployment_build_leases.*
)
SELECT claimed.*, deployments.version, deployments.api_version, deployments.sdk_version,
       deployments.cli_version, deployments.bundle_format_version, deployments.content_hash,
       source_artifacts.digest AS deployment_source_digest,
       source_artifacts.size_bytes AS source_size_bytes,
       source_artifacts.media_type AS source_media_type,
       deployments.status AS deployment_status
  FROM claimed
  JOIN deployments ON deployments.org_id = claimed.org_id
                  AND deployments.id = claimed.deployment_id
                  AND deployments.current_build_lease_id = claimed.id
  JOIN artifacts AS source_artifacts
    ON source_artifacts.org_id = deployments.org_id
   AND source_artifacts.project_id = deployments.project_id
   AND source_artifacts.environment_id = deployments.environment_id
   AND source_artifacts.id = deployments.deployment_source_artifact_id;

-- name: StartDeploymentBuildLease :one
UPDATE deployment_build_leases
   SET state = 'running', started_at = COALESCE(started_at, now()),
       renewed_at = now(), expires_at = sqlc.arg(expires_at), updated_at = now()
 WHERE org_id = sqlc.arg(org_id) AND deployment_id = sqlc.arg(deployment_id)
   AND id = sqlc.arg(build_lease_id)
   AND build_attempt_number = sqlc.arg(build_attempt_number)
   AND lease_sequence = sqlc.arg(lease_sequence)
   AND worker_group_id = sqlc.arg(worker_group_id)
   AND worker_instance_id = sqlc.arg(worker_instance_id)
	   AND worker_epoch = sqlc.arg(worker_epoch)
	   AND requested_workload_disk_bytes = sqlc.arg(requested_workload_disk_bytes)
	   AND requested_scratch_bytes = sqlc.arg(requested_scratch_bytes)
	   AND requested_cpu_millis = sqlc.arg(requested_cpu_millis)
	   AND requested_memory_bytes = sqlc.arg(requested_memory_bytes)
	   AND requested_build_executors = sqlc.arg(requested_build_executors)
	   AND state = 'starting' AND start_deadline_at > now() AND expires_at > now()
RETURNING *;

-- name: GetStartedDeploymentBuildLease :one
SELECT *
  FROM deployment_build_leases
 WHERE org_id = sqlc.arg(org_id) AND deployment_id = sqlc.arg(deployment_id)
   AND id = sqlc.arg(build_lease_id)
   AND build_attempt_number = sqlc.arg(build_attempt_number)
   AND lease_sequence = sqlc.arg(lease_sequence)
   AND worker_group_id = sqlc.arg(worker_group_id)
   AND worker_instance_id = sqlc.arg(worker_instance_id)
   AND worker_epoch = sqlc.arg(worker_epoch)
   AND worker_protocol_version = sqlc.arg(worker_protocol_version)
   AND requested_workload_disk_bytes = sqlc.arg(requested_workload_disk_bytes)
   AND requested_scratch_bytes = sqlc.arg(requested_scratch_bytes)
   AND requested_cpu_millis = sqlc.arg(requested_cpu_millis)
   AND requested_memory_bytes = sqlc.arg(requested_memory_bytes)
   AND requested_build_executors = sqlc.arg(requested_build_executors)
   AND state = 'running';

-- name: RenewDeploymentBuildLease :one
UPDATE deployment_build_leases
   SET renewed_at = now(), expires_at = sqlc.arg(expires_at), updated_at = now()
 WHERE org_id = sqlc.arg(org_id) AND deployment_id = sqlc.arg(deployment_id)
   AND id = sqlc.arg(build_lease_id)
   AND build_attempt_number = sqlc.arg(build_attempt_number)
   AND lease_sequence = sqlc.arg(lease_sequence)
   AND worker_group_id = sqlc.arg(worker_group_id)
   AND worker_instance_id = sqlc.arg(worker_instance_id)
   AND worker_epoch = sqlc.arg(worker_epoch)
   AND state = 'running' AND expires_at > now()
RETURNING *;

-- name: RejectDeploymentBuildLease :one
UPDATE deployment_build_leases
   SET state = 'rejected', terminal_at = now(),
       terminal_reason_code = sqlc.arg(reason_code), terminal_error = sqlc.narg(error),
       terminal_request_fingerprint = NULLIF(sqlc.arg(terminal_request_fingerprint)::text, ''),
       updated_at = now()
 WHERE org_id = sqlc.arg(org_id) AND deployment_id = sqlc.arg(deployment_id)
   AND id = sqlc.arg(build_lease_id)
   AND build_attempt_number = sqlc.arg(build_attempt_number)
   AND lease_sequence = sqlc.arg(lease_sequence)
   AND worker_group_id = sqlc.arg(worker_group_id)
   AND worker_instance_id = sqlc.arg(worker_instance_id)
   AND worker_epoch = sqlc.arg(worker_epoch)
   AND state IN ('assigned', 'starting')
RETURNING *;

-- name: CompleteDeploymentBuild :one
WITH completed AS (
    UPDATE deployment_build_leases
       SET state = 'succeeded', committed_artifact_id = sqlc.arg(deployment_manifest_artifact_id),
           terminal_at = now(), terminal_reason_code = 'completed', terminal_error = NULL,
           terminal_request_fingerprint = NULLIF(sqlc.arg(terminal_request_fingerprint)::text, ''),
           updated_at = now()
     WHERE deployment_build_leases.org_id = sqlc.arg(org_id) AND deployment_build_leases.deployment_id = sqlc.arg(id)
       AND deployment_build_leases.id = sqlc.arg(build_lease_id) AND deployment_build_leases.worker_instance_id = sqlc.arg(build_worker_instance_id)
       AND deployment_build_leases.worker_epoch = sqlc.arg(worker_epoch)
       AND deployment_build_leases.build_attempt_number = sqlc.arg(build_attempt_number)
       AND deployment_build_leases.lease_sequence = sqlc.arg(lease_sequence)
       AND deployment_build_leases.state = 'running' AND deployment_build_leases.expires_at > now()
    RETURNING *
), deployed AS (
UPDATE deployments
   SET status = 'deployed', build_manifest_artifact_id = sqlc.arg(build_manifest_artifact_id),
       deployment_manifest_artifact_id = sqlc.arg(deployment_manifest_artifact_id),
       built_at = COALESCE(built_at, now()), deployed_at = now(), updated_at = now()
  FROM completed
 WHERE deployments.id = completed.deployment_id AND deployments.current_build_lease_id = completed.id
RETURNING deployments.*
), meter_event AS (
    INSERT INTO meter_events (
        org_id, project_id, environment_id, deployment_id,
        deployment_build_lease_id, attempt_number, trace_id, span_id, meter,
        quantity, unit, measured_from, measured_to, details,
        idempotency_key, idempotency_fingerprint
    )
    SELECT completed.org_id, completed.project_id, completed.environment_id,
           completed.deployment_id, completed.id, completed.build_attempt_number,
           completed.trace_id, completed.span_id, 'active_time',
           extract(epoch FROM (completed.terminal_at - completed.started_at)) * 1000,
           'milliseconds', completed.started_at, completed.terminal_at,
           jsonb_build_object(
               'outcome','succeeded', 'cpu_millis',completed.requested_cpu_millis,
               'memory_bytes',completed.requested_memory_bytes,
               'workload_disk_bytes',completed.requested_workload_disk_bytes,
               'scratch_bytes',completed.requested_scratch_bytes,
               'build_cache_bytes',completed.requested_build_cache_bytes,
               'artifact_cache_bytes',completed.requested_artifact_cache_bytes,
               'build_executors',completed.requested_build_executors
           ),
           'build-active:' || completed.id::text,
           jsonb_build_object(
               'quantity', extract(epoch FROM (completed.terminal_at - completed.started_at)) * 1000,
               'unit','milliseconds', 'measured_from',completed.started_at,
               'measured_to',completed.terminal_at, 'outcome','succeeded',
               'cpu_millis',completed.requested_cpu_millis,
               'memory_bytes',completed.requested_memory_bytes,
               'workload_disk_bytes',completed.requested_workload_disk_bytes,
               'scratch_bytes',completed.requested_scratch_bytes,
               'build_cache_bytes',completed.requested_build_cache_bytes,
               'artifact_cache_bytes',completed.requested_artifact_cache_bytes,
               'build_executors',completed.requested_build_executors
           )::text
      FROM completed
     WHERE completed.started_at < completed.terminal_at
    ON CONFLICT (org_id, source_type, source_id, meter, idempotency_key)
    DO UPDATE SET idempotency_fingerprint = meter_events.idempotency_fingerprint
     WHERE meter_events.idempotency_fingerprint = excluded.idempotency_fingerprint
    RETURNING *
), meter_outbox AS (
    INSERT INTO telemetry_outbox (
        org_id, stream_kind, source_kind, source_id, project_id, environment_id,
        deployment_id, meter_event_id, attempt_number, trace_id, span_id,
        kind, payload, idempotency_key, observed_at
    )
    SELECT org_id, 'meter_event', source_type, source_id, project_id,
           environment_id, deployment_id, id, attempt_number, trace_id, span_id,
           meter, details, idempotency_key, occurred_at
      FROM meter_event
    ON CONFLICT DO NOTHING
    RETURNING meter_event_id
)
SELECT deployed.* FROM deployed, completed
 WHERE completed.started_at IS NULL OR EXISTS (SELECT 1 FROM meter_outbox);

-- name: GetDeploymentBuildLease :one
SELECT *
 FROM deployment_build_leases
 WHERE org_id = sqlc.arg(org_id) AND deployment_id = sqlc.arg(id)
   AND id = sqlc.arg(build_lease_id) AND worker_instance_id = sqlc.arg(build_worker_instance_id)
   AND worker_epoch = sqlc.arg(worker_epoch)
   AND state IN ('assigned', 'starting', 'running') AND expires_at > now()
 FOR UPDATE;

-- name: GetDeploymentBuildTerminalResult :one
SELECT state, terminal_request_fingerprint
  FROM deployment_build_leases
 WHERE org_id = sqlc.arg(org_id) AND deployment_id = sqlc.arg(deployment_id)
   AND id = sqlc.arg(build_lease_id)
   AND build_attempt_number = sqlc.arg(build_attempt_number)
   AND lease_sequence = sqlc.arg(lease_sequence)
   AND worker_group_id = sqlc.arg(worker_group_id)
   AND worker_instance_id = sqlc.arg(worker_instance_id)
   AND worker_epoch = sqlc.arg(worker_epoch)
   AND worker_protocol_version = sqlc.arg(worker_protocol_version)
   AND state IN ('succeeded', 'failed', 'rejected');

-- name: FailDeploymentBuild :one
WITH failed AS (
    UPDATE deployment_build_leases
       SET state = 'failed', terminal_at = now(), terminal_reason_code = sqlc.arg(reason_code),
           terminal_error = sqlc.arg(failure),
           terminal_request_fingerprint = NULLIF(sqlc.arg(terminal_request_fingerprint)::text, ''),
           updated_at = now()
     WHERE deployment_build_leases.org_id = sqlc.arg(org_id) AND deployment_build_leases.deployment_id = sqlc.arg(id)
       AND deployment_build_leases.id = sqlc.arg(build_lease_id) AND deployment_build_leases.worker_instance_id = sqlc.arg(build_worker_instance_id)
       AND deployment_build_leases.worker_epoch = sqlc.arg(worker_epoch)
       AND deployment_build_leases.build_attempt_number = sqlc.arg(build_attempt_number)
       AND deployment_build_leases.lease_sequence = sqlc.arg(lease_sequence)
       AND deployment_build_leases.state IN ('starting', 'running') AND deployment_build_leases.expires_at > now()
    RETURNING *
), failed_deployment AS (
UPDATE deployments
   SET status = 'failed', failure = sqlc.arg(failure), failed_at = now(), updated_at = now()
  FROM failed
 WHERE deployments.id = failed.deployment_id AND deployments.current_build_lease_id = failed.id
RETURNING deployments.*
), meter_event AS (
    INSERT INTO meter_events (
        org_id, project_id, environment_id, deployment_id,
        deployment_build_lease_id, attempt_number, trace_id, span_id, meter,
        quantity, unit, measured_from, measured_to, details,
        idempotency_key, idempotency_fingerprint
    )
    SELECT failed.org_id, failed.project_id, failed.environment_id,
           failed.deployment_id, failed.id, failed.build_attempt_number,
           failed.trace_id, failed.span_id, 'active_time',
           extract(epoch FROM (failed.terminal_at - failed.started_at)) * 1000,
           'milliseconds', failed.started_at, failed.terminal_at,
           jsonb_build_object(
               'outcome','failed', 'reason_code',failed.terminal_reason_code,
               'cpu_millis',failed.requested_cpu_millis,
               'memory_bytes',failed.requested_memory_bytes,
               'workload_disk_bytes',failed.requested_workload_disk_bytes,
               'scratch_bytes',failed.requested_scratch_bytes,
               'build_cache_bytes',failed.requested_build_cache_bytes,
               'artifact_cache_bytes',failed.requested_artifact_cache_bytes,
               'build_executors',failed.requested_build_executors
           ),
           'build-active:' || failed.id::text,
           jsonb_build_object(
               'quantity', extract(epoch FROM (failed.terminal_at - failed.started_at)) * 1000,
               'unit','milliseconds', 'measured_from',failed.started_at,
               'measured_to',failed.terminal_at, 'outcome','failed',
               'reason_code',failed.terminal_reason_code,
               'cpu_millis',failed.requested_cpu_millis,
               'memory_bytes',failed.requested_memory_bytes,
               'workload_disk_bytes',failed.requested_workload_disk_bytes,
               'scratch_bytes',failed.requested_scratch_bytes,
               'build_cache_bytes',failed.requested_build_cache_bytes,
               'artifact_cache_bytes',failed.requested_artifact_cache_bytes,
               'build_executors',failed.requested_build_executors
           )::text
      FROM failed
     WHERE failed.started_at < failed.terminal_at
    ON CONFLICT (org_id, source_type, source_id, meter, idempotency_key)
    DO UPDATE SET idempotency_fingerprint = meter_events.idempotency_fingerprint
     WHERE meter_events.idempotency_fingerprint = excluded.idempotency_fingerprint
    RETURNING *
), meter_outbox AS (
    INSERT INTO telemetry_outbox (
        org_id, stream_kind, source_kind, source_id, project_id, environment_id,
        deployment_id, meter_event_id, attempt_number, trace_id, span_id,
        kind, payload, idempotency_key, observed_at
    )
    SELECT org_id, 'meter_event', source_type, source_id, project_id,
           environment_id, deployment_id, id, attempt_number, trace_id, span_id,
           meter, details, idempotency_key, occurred_at
      FROM meter_event
    ON CONFLICT DO NOTHING
    RETURNING meter_event_id
)
SELECT failed_deployment.* FROM failed_deployment, failed
 WHERE failed.started_at IS NULL OR EXISTS (SELECT 1 FROM meter_outbox);

-- name: PromoteDeployment :one
WITH target AS (
    SELECT deployments.id,
           deployments.org_id,
           deployments.project_id,
           deployments.environment_id
      FROM deployments
     WHERE deployments.org_id = sqlc.arg(org_id)
       AND deployments.project_id = sqlc.arg(project_id)
       AND deployments.environment_id = sqlc.arg(environment_id)
       AND deployments.id = sqlc.arg(deployment_id)
       AND deployments.status = 'deployed'
),
previous AS (
    SELECT environments.current_deployment_id
      FROM environments
      JOIN target ON target.org_id = environments.org_id
                 AND target.project_id = environments.project_id
                 AND target.environment_id = environments.id
     FOR UPDATE OF environments
),
updated_environment AS (
    UPDATE environments
       SET current_deployment_id = target.id,
           updated_at = now()
      FROM target, previous
     WHERE environments.org_id = target.org_id
       AND environments.project_id = target.project_id
       AND environments.id = target.environment_id
    RETURNING environments.current_deployment_id
),
promotion AS (
    INSERT INTO deployment_promotions (
        id,
        org_id,
        project_id,
        environment_id,
        deployment_id,
        previous_deployment_id,
        promoted_by_principal,
        reason
    )
    SELECT sqlc.arg(id),
           target.org_id,
           target.project_id,
           target.environment_id,
           target.id,
           previous.current_deployment_id,
           sqlc.arg(promoted_by_principal),
           sqlc.arg(reason)
      FROM target
      JOIN previous ON true
      JOIN updated_environment ON true
    RETURNING *
)
SELECT * FROM promotion;

-- name: GetDeployment :one
SELECT *
  FROM deployments
 WHERE org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND id = sqlc.arg(id);

-- name: GetDeploymentForOrg :one
SELECT *
  FROM deployments
 WHERE org_id = sqlc.arg(org_id)
   AND id = sqlc.arg(id);

-- name: GetDeploymentByVersion :one
SELECT *
  FROM deployments
 WHERE org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND version = sqlc.arg(version);

-- name: ListDeploymentsByVersionForOrg :many
SELECT *
  FROM deployments
 WHERE org_id = sqlc.arg(org_id)
   AND version = sqlc.arg(version)
 ORDER BY created_at ASC;

-- name: ListScopedDeployments :many
SELECT deployments.*
  FROM deployments
 WHERE deployments.org_id = sqlc.arg(org_id)
   AND deployments.project_id = sqlc.arg(project_id)
   AND deployments.environment_id = sqlc.arg(environment_id)
 ORDER BY deployments.created_at DESC, deployments.id DESC
 LIMIT sqlc.arg(row_limit);

-- name: CreateDeploymentSandbox :one
INSERT INTO deployment_sandboxes (
    id,
    public_id,
    org_id,
    project_id,
    environment_id,
    deployment_id,
    sandbox_id,
    image_artifact_id,
    image_artifact_format,
    rootfs_digest,
    image_digest,
    image_format,
    workspace_mount_path,
    resource_floor,
    disk_floor_mib,
    network_policy,
    runtime_abi,
    guestd_abi,
    adapter_abi,
    filesystem_format,
    default_uid,
    default_gid,
    default_workdir,
    contract_version,
    fingerprint
) VALUES (
    sqlc.arg(id),
    sqlc.arg(public_id),
    sqlc.arg(org_id),
    sqlc.arg(project_id),
    sqlc.arg(environment_id),
    sqlc.arg(deployment_id),
    sqlc.arg(sandbox_id),
    sqlc.arg(image_artifact_id),
    sqlc.arg(image_artifact_format),
    sqlc.arg(rootfs_digest),
    sqlc.arg(image_digest),
    sqlc.arg(image_format),
    sqlc.arg(workspace_mount_path),
    coalesce(sqlc.arg(resource_floor)::jsonb, '{}'::jsonb),
    sqlc.arg(disk_floor_mib),
    coalesce(sqlc.arg(network_policy)::jsonb, '{}'::jsonb),
    sqlc.arg(runtime_abi),
    sqlc.arg(guestd_abi),
    sqlc.arg(adapter_abi),
    sqlc.arg(filesystem_format),
    sqlc.arg(default_uid),
    sqlc.arg(default_gid),
    sqlc.arg(default_workdir),
    sqlc.arg(contract_version),
    sqlc.arg(fingerprint)
)
RETURNING *;

-- name: CreateDeploymentQueue :one
INSERT INTO deployment_queues (
    id,
    org_id,
    project_id,
    environment_id,
    deployment_id,
    name,
    concurrency_limit
) VALUES (
    sqlc.arg(id),
    sqlc.arg(org_id),
    sqlc.arg(project_id),
    sqlc.arg(environment_id),
    sqlc.arg(deployment_id),
    sqlc.arg(name),
    sqlc.narg(concurrency_limit)
)
RETURNING *;

-- name: CreateDeploymentTask :one
WITH catalog_task AS (
    INSERT INTO tasks (
        org_id,
        public_id,
        project_id,
        environment_id,
        task_id,
        archived_at,
        updated_at
    ) VALUES (
        sqlc.arg(org_id),
        sqlc.arg(task_public_id),
        sqlc.arg(project_id),
        sqlc.arg(environment_id),
        sqlc.arg(task_id),
        NULL,
        now()
    )
    ON CONFLICT (org_id, project_id, environment_id, task_id)
    DO UPDATE SET archived_at = NULL,
                  updated_at = now()
    RETURNING task_id
)
INSERT INTO deployment_tasks (
    id,
    public_id,
    org_id,
    project_id,
    environment_id,
    deployment_id,
    deployment_sandbox_id,
    task_id,
    file_path,
    export_name,
    handler_entrypoint,
    bundle_artifact_id,
    bundle_format_version,
    requested_milli_cpu,
    requested_memory_mib,
    requested_disk_mib,
    secret_declarations,
    resource_requirements,
    network_policy,
    schedule_declarations,
    queue_name,
    queue_concurrency_limit,
    ttl,
    max_active_duration_ms,
    retry_policy
) SELECT
    sqlc.arg(id),
    sqlc.arg(public_id),
    sqlc.arg(org_id),
    sqlc.arg(project_id),
    sqlc.arg(environment_id),
    sqlc.arg(deployment_id),
    sqlc.arg(deployment_sandbox_id),
    sqlc.arg(task_id),
    sqlc.arg(file_path),
    sqlc.arg(export_name),
    sqlc.arg(handler_entrypoint),
    sqlc.arg(bundle_artifact_id),
    sqlc.arg(bundle_format_version),
    sqlc.arg(requested_milli_cpu),
    sqlc.arg(requested_memory_mib),
    sqlc.arg(requested_disk_mib),
    sqlc.arg(secret_declarations),
    sqlc.arg(resource_requirements),
    sqlc.arg(network_policy),
    coalesce(sqlc.narg(schedule_declarations)::jsonb, '[]'::jsonb),
    sqlc.arg(queue_name),
    sqlc.narg(queue_concurrency_limit),
    sqlc.arg(ttl),
    sqlc.arg(max_active_duration_ms),
    coalesce(sqlc.arg(retry_policy)::jsonb, '{"enabled": false}'::jsonb)
  FROM catalog_task
RETURNING *;

-- name: GetCurrentDeployment :one
SELECT deployments.*
  FROM deployments
  JOIN environments ON environments.org_id = deployments.org_id
                   AND environments.project_id = deployments.project_id
                   AND environments.id = deployments.environment_id
                   AND environments.current_deployment_id = deployments.id
 WHERE deployments.org_id = sqlc.arg(org_id)
   AND deployments.project_id = sqlc.arg(project_id)
   AND deployments.environment_id = sqlc.arg(environment_id)
   AND deployments.status = 'deployed'
 LIMIT 1;

-- name: GetCurrentDeploymentForRoute :one
SELECT deployments.*
  FROM deployments
  JOIN environments ON environments.org_id = deployments.org_id
                   AND environments.project_id = deployments.project_id
                   AND environments.id = deployments.environment_id
                   AND environments.current_deployment_id = deployments.id
 WHERE deployments.org_id = sqlc.arg(org_id)
   AND deployments.project_id = sqlc.arg(project_id)
   AND deployments.environment_id = sqlc.arg(environment_id)
   AND deployments.status = 'deployed'
 LIMIT 1;

-- name: ListDeploymentTasks :many
SELECT *
  FROM deployment_tasks
 WHERE org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND deployment_id = sqlc.arg(deployment_id)
 ORDER BY task_id ASC;

-- name: ListCurrentDeploymentTasks :many
SELECT deployment_tasks.*
  FROM deployment_tasks
  JOIN environments ON environments.org_id = deployment_tasks.org_id
                   AND environments.project_id = deployment_tasks.project_id
                   AND environments.id = deployment_tasks.environment_id
                   AND environments.current_deployment_id = deployment_tasks.deployment_id
  JOIN deployments ON deployments.org_id = deployment_tasks.org_id
                  AND deployments.project_id = deployment_tasks.project_id
                  AND deployments.environment_id = deployment_tasks.environment_id
                  AND deployments.id = deployment_tasks.deployment_id
                  AND deployments.status = 'deployed'
 WHERE deployment_tasks.org_id = sqlc.arg(org_id)
   AND deployment_tasks.project_id = sqlc.arg(project_id)
   AND deployment_tasks.environment_id = sqlc.arg(environment_id)
 ORDER BY deployment_tasks.task_id ASC;

-- name: ListCurrentDeploymentSandboxes :many
SELECT deployment_sandboxes.*
  FROM deployment_sandboxes
  JOIN environments ON environments.org_id = deployment_sandboxes.org_id
                   AND environments.project_id = deployment_sandboxes.project_id
                   AND environments.id = deployment_sandboxes.environment_id
                   AND environments.current_deployment_id = deployment_sandboxes.deployment_id
  JOIN deployments ON deployments.org_id = deployment_sandboxes.org_id
                  AND deployments.project_id = deployment_sandboxes.project_id
                  AND deployments.environment_id = deployment_sandboxes.environment_id
                  AND deployments.id = deployment_sandboxes.deployment_id
                  AND deployments.status = 'deployed'
WHERE deployment_sandboxes.org_id = sqlc.arg(org_id)
   AND deployment_sandboxes.project_id = sqlc.arg(project_id)
   AND deployment_sandboxes.environment_id = sqlc.arg(environment_id)
 ORDER BY deployment_sandboxes.sandbox_id ASC;

-- name: GetCurrentDeploymentSandbox :one
SELECT deployment_sandboxes.*
  FROM deployment_sandboxes
  JOIN environments ON environments.org_id = deployment_sandboxes.org_id
                   AND environments.project_id = deployment_sandboxes.project_id
                   AND environments.id = deployment_sandboxes.environment_id
                   AND environments.current_deployment_id = deployment_sandboxes.deployment_id
  JOIN deployments ON deployments.org_id = deployment_sandboxes.org_id
                  AND deployments.project_id = deployment_sandboxes.project_id
                  AND deployments.environment_id = deployment_sandboxes.environment_id
                  AND deployments.id = deployment_sandboxes.deployment_id
                  AND deployments.status = 'deployed'
WHERE deployment_sandboxes.org_id = sqlc.arg(org_id)
   AND deployment_sandboxes.project_id = sqlc.arg(project_id)
   AND deployment_sandboxes.environment_id = sqlc.arg(environment_id)
   AND deployment_sandboxes.sandbox_id = sqlc.arg(sandbox_id)
 LIMIT 1;

-- name: GetDeploymentSandboxByID :one
SELECT *
  FROM deployment_sandboxes
 WHERE id = sqlc.arg(id)
 LIMIT 1;

-- name: GetDeploymentSandboxForWorkerGroup :one
SELECT deployment_sandboxes.*
  FROM deployment_sandboxes
  JOIN deployments
    ON deployments.org_id = deployment_sandboxes.org_id
   AND deployments.project_id = deployment_sandboxes.project_id
   AND deployments.environment_id = deployment_sandboxes.environment_id
   AND deployments.id = deployment_sandboxes.deployment_id
  JOIN worker_groups
    ON worker_groups.id = sqlc.arg(worker_group_id)
   AND worker_groups.state IN ('active', 'draining')
 WHERE deployment_sandboxes.id = sqlc.arg(id)
 LIMIT 1;

-- name: GetCurrentDeploymentTask :one
SELECT deployment_tasks.*,
       deployment_sandboxes.sandbox_id,
       deployment_sandboxes.fingerprint AS sandbox_fingerprint,
       deployment_sandboxes.workspace_mount_path,
       deployment_sandboxes.resource_floor AS deployment_sandbox_resource_floor,
       deployment_sandboxes.disk_floor_mib AS deployment_sandbox_disk_floor_mib,
       deployment_sandboxes.network_policy AS deployment_sandbox_network_policy,
       deployment_sandboxes.rootfs_digest AS deployment_sandbox_rootfs_digest,
       deployment_sandboxes.runtime_abi AS deployment_sandbox_runtime_abi,
       deployment_sandboxes.guestd_abi AS deployment_sandbox_guestd_abi,
       deployment_sandboxes.adapter_abi AS deployment_sandbox_adapter_abi,
       deployment_sandboxes.filesystem_format AS deployment_sandbox_filesystem_format,
       deployment_sandboxes.contract_version AS deployment_sandbox_contract_version,
       deployments.version AS deployment_version,
       deployments.api_version,
       deployments.sdk_version,
       deployments.cli_version,
       deployments.worker_protocol_version,
       task_bundle_artifacts.digest AS bundle_digest,
       source_artifacts.digest AS deployment_source_digest
  FROM deployment_tasks
  JOIN deployments ON deployments.org_id = deployment_tasks.org_id
                  AND deployments.project_id = deployment_tasks.project_id
                  AND deployments.environment_id = deployment_tasks.environment_id
                  AND deployments.id = deployment_tasks.deployment_id
  JOIN deployment_sandboxes
    ON deployment_sandboxes.org_id = deployment_tasks.org_id
   AND deployment_sandboxes.project_id = deployment_tasks.project_id
   AND deployment_sandboxes.environment_id = deployment_tasks.environment_id
   AND deployment_sandboxes.id = deployment_tasks.deployment_sandbox_id
  JOIN artifacts AS task_bundle_artifacts
    ON task_bundle_artifacts.org_id = deployment_tasks.org_id
   AND task_bundle_artifacts.project_id = deployment_tasks.project_id
   AND task_bundle_artifacts.environment_id = deployment_tasks.environment_id
   AND task_bundle_artifacts.id = deployment_tasks.bundle_artifact_id
  JOIN artifacts AS source_artifacts
    ON source_artifacts.org_id = deployments.org_id
   AND source_artifacts.project_id = deployments.project_id
   AND source_artifacts.environment_id = deployments.environment_id
   AND source_artifacts.id = deployments.deployment_source_artifact_id
  JOIN environments ON environments.org_id = deployments.org_id
                   AND environments.project_id = deployments.project_id
                   AND environments.id = deployments.environment_id
                   AND environments.current_deployment_id = deployments.id
 WHERE deployment_tasks.org_id = sqlc.arg(org_id)
   AND deployment_tasks.project_id = sqlc.arg(project_id)
   AND deployment_tasks.environment_id = sqlc.arg(environment_id)
   AND deployment_tasks.task_id = sqlc.arg(task_id)
   AND deployments.status = 'deployed'
 LIMIT 1;

-- name: GetDeploymentQueueConfig :one
SELECT name AS queue_name,
       concurrency_limit AS queue_concurrency_limit
 FROM deployment_queues
 WHERE org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND deployment_id = sqlc.arg(deployment_id)
   AND name = sqlc.arg(queue_name)
 LIMIT 1;

-- name: GetDeploymentTask :one
SELECT deployment_tasks.*,
       deployment_sandboxes.sandbox_id,
       deployment_sandboxes.fingerprint AS sandbox_fingerprint,
       deployment_sandboxes.workspace_mount_path,
       deployment_sandboxes.resource_floor AS deployment_sandbox_resource_floor,
       deployment_sandboxes.disk_floor_mib AS deployment_sandbox_disk_floor_mib,
       deployment_sandboxes.network_policy AS deployment_sandbox_network_policy,
       deployment_sandboxes.rootfs_digest AS deployment_sandbox_rootfs_digest,
       deployment_sandboxes.runtime_abi AS deployment_sandbox_runtime_abi,
       deployment_sandboxes.guestd_abi AS deployment_sandbox_guestd_abi,
       deployment_sandboxes.adapter_abi AS deployment_sandbox_adapter_abi,
       deployment_sandboxes.filesystem_format AS deployment_sandbox_filesystem_format,
       deployment_sandboxes.contract_version AS deployment_sandbox_contract_version,
       deployments.version AS deployment_version,
       deployments.api_version,
       deployments.sdk_version,
       deployments.cli_version,
       deployments.worker_protocol_version,
       task_bundle_artifacts.digest AS bundle_digest,
       source_artifacts.digest AS deployment_source_digest
  FROM deployment_tasks
  JOIN deployments ON deployments.org_id = deployment_tasks.org_id
                  AND deployments.project_id = deployment_tasks.project_id
                  AND deployments.environment_id = deployment_tasks.environment_id
                  AND deployments.id = deployment_tasks.deployment_id
  JOIN deployment_sandboxes
    ON deployment_sandboxes.org_id = deployment_tasks.org_id
   AND deployment_sandboxes.project_id = deployment_tasks.project_id
   AND deployment_sandboxes.environment_id = deployment_tasks.environment_id
   AND deployment_sandboxes.id = deployment_tasks.deployment_sandbox_id
  JOIN artifacts AS task_bundle_artifacts
    ON task_bundle_artifacts.org_id = deployment_tasks.org_id
   AND task_bundle_artifacts.project_id = deployment_tasks.project_id
   AND task_bundle_artifacts.environment_id = deployment_tasks.environment_id
   AND task_bundle_artifacts.id = deployment_tasks.bundle_artifact_id
  JOIN artifacts AS source_artifacts
    ON source_artifacts.org_id = deployments.org_id
   AND source_artifacts.project_id = deployments.project_id
   AND source_artifacts.environment_id = deployments.environment_id
   AND source_artifacts.id = deployments.deployment_source_artifact_id
WHERE deployment_tasks.org_id = sqlc.arg(org_id)
   AND deployment_tasks.project_id = sqlc.arg(project_id)
   AND deployment_tasks.environment_id = sqlc.arg(environment_id)
   AND deployment_tasks.deployment_id = sqlc.arg(deployment_id)
   AND deployment_tasks.task_id = sqlc.arg(task_id)
   AND deployments.status = 'deployed'
 LIMIT 1;
