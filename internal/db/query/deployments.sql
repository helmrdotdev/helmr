-- name: CreateDeployment :one
INSERT INTO deployments (
    id,
    public_id,
    org_id,
    project_id,
    environment_id,
    build_region_id,
    build_architecture,
    build_runtime_digest,
    build_standard_toolchain_digest,
    build_contract_version,
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
       sqlc.arg(build_architecture),
       sqlc.arg(build_runtime_digest),
       sqlc.arg(build_standard_toolchain_digest),
       sqlc.arg(build_contract_version),
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

-- name: LockDeploymentBuildReuseKey :exec
SELECT pg_advisory_xact_lock(
    hashtextextended(sqlc.arg(reuse_key)::text, 0)
);

-- name: GetReusableDeploymentBuild :one
SELECT *
  FROM deployments
 WHERE org_id = sqlc.arg(org_id)
   AND build_region_id = sqlc.arg(build_region_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND content_hash = sqlc.arg(content_hash)
   AND api_version = sqlc.arg(api_version)
   AND sdk_version = sqlc.arg(sdk_version)
   AND cli_version = sqlc.arg(cli_version)
   AND bundle_format_version = sqlc.arg(bundle_format_version)
   AND worker_protocol_version = sqlc.arg(worker_protocol_version)
   AND build_architecture = sqlc.arg(build_architecture)
   AND build_runtime_digest = sqlc.arg(build_runtime_digest)
   AND build_standard_toolchain_digest = sqlc.arg(build_standard_toolchain_digest)
   AND build_contract_version = sqlc.arg(build_contract_version)
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
WITH candidate AS MATERIALIZED (
    SELECT deployments.*
      FROM deployments
     WHERE deployments.org_id = sqlc.arg(org_id)
       AND deployments.id = sqlc.arg(deployment_id)
       AND deployments.status IN ('queued', 'building')
       AND deployments.build_region_id = sqlc.arg(build_region_id)
       AND deployments.build_architecture = sqlc.arg(build_architecture)
       AND deployments.build_requested_cpu_millis = sqlc.arg(requested_cpu_millis)
       AND deployments.build_requested_memory_bytes = sqlc.arg(requested_memory_bytes)
       AND deployments.build_requested_workload_disk_bytes = sqlc.arg(requested_workload_disk_bytes)
       AND deployments.build_requested_scratch_bytes = sqlc.arg(requested_scratch_bytes)
       AND deployments.build_requested_executors = sqlc.arg(requested_build_executors)
       AND deployments.current_build_lease_id IS NULL
       AND sqlc.arg(lease_sequence)::bigint = (
           SELECT COALESCE(max(deployment_build_leases.lease_sequence), 0) + 1
             FROM deployment_build_leases
            WHERE deployment_build_leases.deployment_id = deployments.id
       )
       AND sqlc.arg(lease_sequence)::bigint BETWEEN 1 AND 3
       AND NOT EXISTS (
           SELECT 1 FROM deployment_build_leases
            WHERE deployment_build_leases.deployment_id = deployments.id
              AND deployment_build_leases.state IN ('assigned', 'starting', 'running')
       )
     FOR UPDATE OF deployments
),
inserted AS (
    INSERT INTO deployment_build_leases (
        id, org_id, project_id, environment_id, deployment_id, build_region_id,
        lease_sequence, worker_group_id, worker_instance_id,
        worker_epoch, worker_protocol_version, requested_cpu_millis,
        requested_memory_bytes, requested_workload_disk_bytes, requested_scratch_bytes,
        requested_build_executors, build_snapshot, trace_id, span_id,
        parent_span_id, traceparent, start_deadline_at, expires_at
    )
    SELECT sqlc.arg(build_lease_id), candidate.org_id, candidate.project_id,
           candidate.environment_id, candidate.id, candidate.build_region_id,
           sqlc.arg(lease_sequence), sqlc.arg(worker_group_id),
           sqlc.arg(build_worker_instance_id),
           sqlc.arg(worker_epoch), sqlc.arg(worker_protocol_version),
           sqlc.arg(requested_cpu_millis), sqlc.arg(requested_memory_bytes),
           sqlc.arg(requested_workload_disk_bytes), sqlc.arg(requested_scratch_bytes),
           sqlc.arg(requested_build_executors), sqlc.arg(build_snapshot),
           sqlc.narg(trace_id), sqlc.narg(span_id), sqlc.narg(parent_span_id),
           sqlc.narg(traceparent), sqlc.arg(start_deadline_at), sqlc.arg(build_lease_expires_at)
      FROM candidate
    RETURNING *
),
advanced AS (
    UPDATE deployments
       SET status = 'building',
           building_at = COALESCE(deployments.building_at, now()),
           current_build_lease_id = inserted.id,
           updated_at = now()
      FROM inserted
     WHERE deployments.id = inserted.deployment_id
    RETURNING deployments.*
)
SELECT inserted.*,
       advanced.version,
       advanced.api_version,
       advanced.sdk_version,
       advanced.cli_version,
       advanced.bundle_format_version,
       advanced.content_hash,
       advanced.build_architecture,
       advanced.build_runtime_digest,
       advanced.build_standard_toolchain_digest,
       advanced.build_contract_version,
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
WITH locked_deployments AS MATERIALIZED (
    SELECT deployments.org_id,
           deployments.id,
           deployments.current_build_lease_id
      FROM deployments
      JOIN deployment_build_leases
        ON deployment_build_leases.org_id = deployments.org_id
       AND deployment_build_leases.deployment_id = deployments.id
       AND deployment_build_leases.id = deployments.current_build_lease_id
     WHERE deployments.status = 'building'
       AND deployment_build_leases.state IN ('assigned','starting','running')
       AND deployment_build_leases.expires_at <= now()
     ORDER BY deployments.id
     FOR UPDATE OF deployments SKIP LOCKED
), expired AS (
    UPDATE deployment_build_leases
       SET state = 'expired', terminal_at = now(),
           terminal_reason_code = 'lease_expired', updated_at = now()
      FROM locked_deployments
     WHERE deployment_build_leases.org_id = locked_deployments.org_id
       AND deployment_build_leases.deployment_id = locked_deployments.id
       AND deployment_build_leases.id = locked_deployments.current_build_lease_id
       AND deployment_build_leases.state IN ('assigned','starting','running')
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
           expired.deployment_id, expired.id, NULL::int,
           expired.trace_id, expired.span_id, 'active_time',
           GREATEST((extract(epoch FROM (expired.expires_at - expired.started_at)) * 1000)::bigint, 0),
           'milliseconds', expired.started_at, expired.expires_at,
           jsonb_build_object('outcome','lease_lost_requeued',
               'cpu_millis',expired.requested_cpu_millis,
               'memory_bytes',expired.requested_memory_bytes,
               'workload_disk_bytes',expired.requested_workload_disk_bytes,
               'scratch_bytes',expired.requested_scratch_bytes,
               'build_executors',expired.requested_build_executors),
           'build-lease-lost:' || expired.id::text,
           jsonb_build_object('quantity',GREATEST((extract(epoch FROM (expired.expires_at - expired.started_at)) * 1000)::bigint, 0),
               'unit','milliseconds','measured_from',expired.started_at,'measured_to',expired.expires_at,
               'outcome','lease_lost_requeued','cpu_millis',expired.requested_cpu_millis,
               'memory_bytes',expired.requested_memory_bytes,
               'workload_disk_bytes',expired.requested_workload_disk_bytes,
               'scratch_bytes',expired.requested_scratch_bytes,
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
   SET current_build_lease_id = CASE
           WHEN expired.lease_sequence < 3 THEN NULL
           ELSE deployments.current_build_lease_id
       END,
       status = CASE
           WHEN expired.lease_sequence < 3 THEN 'building'::deployment_status
           ELSE 'failed'::deployment_status
       END,
       failure = CASE
           WHEN expired.lease_sequence < 3 THEN deployments.failure
           ELSE jsonb_build_object('reason_code', 'build_delivery_exhausted')
       END,
       failed_at = CASE
           WHEN expired.lease_sequence < 3 THEN deployments.failed_at
           ELSE now()
       END,
       updated_at = now()
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
       deployments.build_architecture,
       deployments.build_requested_cpu_millis,
       deployments.build_requested_memory_bytes,
       deployments.build_requested_workload_disk_bytes,
       deployments.build_requested_scratch_bytes,
       deployments.build_requested_executors,
       (COALESCE((
           SELECT max(deployment_build_leases.lease_sequence)
             FROM deployment_build_leases
            WHERE deployment_build_leases.deployment_id = deployments.id
       ), 0) + 1)::bigint AS lease_sequence,
       deployments.created_at AS queue_timestamp
  FROM deployments
 WHERE deployments.build_region_id = sqlc.arg(build_region_id)
   AND deployments.status IN ('queued', 'building')
   AND deployments.current_build_lease_id IS NULL
   AND COALESCE((
       SELECT max(deployment_build_leases.lease_sequence)
         FROM deployment_build_leases
        WHERE deployment_build_leases.deployment_id = deployments.id
   ), 0) < 3
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
   AND deployments.current_build_lease_id IS NULL
   AND COALESCE((
       SELECT max(deployment_build_leases.lease_sequence)
         FROM deployment_build_leases
        WHERE deployment_build_leases.deployment_id = deployments.id
   ), 0) < 3
   AND NOT EXISTS (
       SELECT 1 FROM deployment_build_leases
        WHERE deployment_build_leases.deployment_id = deployments.id
          AND deployment_build_leases.state IN ('assigned','starting','running')
   )
 ORDER BY deployments.build_region_id
 LIMIT sqlc.arg(limit_count);

-- name: ClaimNextDeploymentBuildLease :one
WITH candidate AS (
    SELECT deployment_build_leases.*
      FROM deployment_build_leases
      JOIN deployments
        ON deployments.org_id = deployment_build_leases.org_id
       AND deployments.id = deployment_build_leases.deployment_id
       AND deployments.current_build_lease_id = deployment_build_leases.id
      JOIN worker_instances
        ON worker_instances.id = deployment_build_leases.worker_instance_id
       AND worker_instances.worker_group_id = deployment_build_leases.worker_group_id
       AND worker_instances.current_epoch = deployment_build_leases.worker_epoch
       AND worker_instances.state = 'active'
       AND worker_instances.supports_build
       AND worker_instances.toolchain_catalog_digest IS NOT NULL
      JOIN runtime_identities
        ON runtime_identities.id = worker_instances.runtime_identity_id
       AND runtime_identities.runtime_arch = deployments.build_architecture
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
       deployments.build_architecture, deployments.build_runtime_digest,
       deployments.build_standard_toolchain_digest, deployments.build_contract_version,
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
   AND lease_sequence = sqlc.arg(lease_sequence)
   AND worker_group_id = sqlc.arg(worker_group_id)
   AND worker_instance_id = sqlc.arg(worker_instance_id)
   AND worker_epoch = sqlc.arg(worker_epoch)
   AND state = 'running' AND expires_at > now()
RETURNING *;

-- name: RejectDeploymentBuildLease :one
WITH target_deployment AS MATERIALIZED (
    SELECT deployments.id
      FROM deployments
     WHERE deployments.org_id = sqlc.arg(org_id)
       AND deployments.id = sqlc.arg(deployment_id)
       AND deployments.status = 'building'
       AND deployments.current_build_lease_id = sqlc.arg(build_lease_id)
     FOR UPDATE
), rejected AS (
    UPDATE deployment_build_leases
       SET state = 'rejected', terminal_at = now(),
           terminal_reason_code = sqlc.arg(reason_code),
           terminal_error = sqlc.narg(error),
           terminal_request_fingerprint = NULLIF(sqlc.arg(terminal_request_fingerprint)::text, ''),
           updated_at = now()
      FROM target_deployment
     WHERE deployment_build_leases.deployment_id = target_deployment.id
       AND deployment_build_leases.org_id = sqlc.arg(org_id)
       AND deployment_build_leases.id = sqlc.arg(build_lease_id)
       AND deployment_build_leases.lease_sequence = sqlc.arg(lease_sequence)
       AND deployment_build_leases.worker_group_id = sqlc.arg(worker_group_id)
       AND deployment_build_leases.worker_instance_id = sqlc.arg(worker_instance_id)
       AND deployment_build_leases.worker_epoch = sqlc.arg(worker_epoch)
       AND deployment_build_leases.state IN ('assigned', 'starting')
    RETURNING deployment_build_leases.*
), transitioned AS (
    UPDATE deployments
       SET current_build_lease_id = CASE
               WHEN rejected.lease_sequence < 3 THEN NULL
               ELSE deployments.current_build_lease_id
           END,
           status = CASE
               WHEN rejected.lease_sequence < 3 THEN 'building'::deployment_status
               ELSE 'failed'::deployment_status
           END,
           failure = CASE
               WHEN rejected.lease_sequence < 3 THEN deployments.failure
               ELSE jsonb_build_object('reason_code', 'build_delivery_exhausted')
           END,
           failed_at = CASE
               WHEN rejected.lease_sequence < 3 THEN deployments.failed_at
               ELSE now()
           END,
           updated_at = now()
      FROM rejected
     WHERE deployments.org_id = rejected.org_id
       AND deployments.id = rejected.deployment_id
       AND deployments.current_build_lease_id = rejected.id
    RETURNING deployments.id
)
SELECT rejected.*
  FROM rejected
 WHERE EXISTS (SELECT 1 FROM transitioned);

-- name: CompleteDeploymentBuild :one
WITH completed AS (
    UPDATE deployment_build_leases
       SET state = 'succeeded', terminal_at = now(),
           terminal_reason_code = 'completed', terminal_error = NULL,
           terminal_request_fingerprint = NULLIF(sqlc.arg(terminal_request_fingerprint)::text, ''),
           updated_at = now()
     WHERE deployment_build_leases.org_id = sqlc.arg(org_id) AND deployment_build_leases.deployment_id = sqlc.arg(id)
       AND deployment_build_leases.id = sqlc.arg(build_lease_id) AND deployment_build_leases.worker_instance_id = sqlc.arg(build_worker_instance_id)
       AND deployment_build_leases.worker_epoch = sqlc.arg(worker_epoch)
       AND deployment_build_leases.lease_sequence = sqlc.arg(lease_sequence)
       AND deployment_build_leases.state = 'running' AND deployment_build_leases.expires_at > now()
    RETURNING *
), deployed AS (
UPDATE deployments
   SET status = 'deployed',
       program_code_artifact_id = sqlc.narg(program_code_artifact_id),
       program_dependency_artifact_id = sqlc.narg(program_dependency_artifact_id),
       program_runtime_digest = sqlc.narg(program_runtime_digest),
       program_architecture = sqlc.narg(program_architecture),
       queue_config = sqlc.arg(queue_config),
       built_at = COALESCE(built_at, now()), deployed_at = now(), updated_at = now()
  FROM completed
 WHERE deployments.id = completed.deployment_id AND deployments.current_build_lease_id = completed.id
   AND (
       (
           sqlc.narg(program_code_artifact_id)::uuid IS NULL
           AND sqlc.narg(program_dependency_artifact_id)::uuid IS NULL
           AND sqlc.narg(program_runtime_digest)::bytea IS NULL
           AND sqlc.narg(program_architecture)::text IS NULL
       )
       OR (
           sqlc.narg(program_code_artifact_id)::uuid IS NOT NULL
           AND sqlc.narg(program_dependency_artifact_id)::uuid IS NOT NULL
           AND sqlc.narg(program_runtime_digest)::bytea = deployments.build_runtime_digest
           AND sqlc.narg(program_architecture)::text = deployments.build_architecture
       )
   )
RETURNING deployments.*
), meter_event AS (
    INSERT INTO meter_events (
        org_id, project_id, environment_id, deployment_id,
        deployment_build_lease_id, attempt_number, trace_id, span_id, meter,
        quantity, unit, measured_from, measured_to, details,
        idempotency_key, idempotency_fingerprint
    )
    SELECT completed.org_id, completed.project_id, completed.environment_id,
           completed.deployment_id, completed.id, NULL::int,
           completed.trace_id, completed.span_id, 'active_time',
           extract(epoch FROM (completed.terminal_at - completed.started_at)) * 1000,
           'milliseconds', completed.started_at, completed.terminal_at,
           jsonb_build_object(
               'outcome','succeeded', 'cpu_millis',completed.requested_cpu_millis,
               'memory_bytes',completed.requested_memory_bytes,
               'workload_disk_bytes',completed.requested_workload_disk_bytes,
               'scratch_bytes',completed.requested_scratch_bytes,
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

-- name: LockDeploymentBuildTerminalFence :one
WITH locked_deployment AS MATERIALIZED (
    SELECT deployments.*
      FROM deployments
     WHERE deployments.org_id = sqlc.arg(org_id)
       AND deployments.project_id = sqlc.arg(project_id)
       AND deployments.environment_id = sqlc.arg(environment_id)
       AND deployments.id = sqlc.arg(deployment_id)
     FOR UPDATE
), locked_lease AS MATERIALIZED (
    SELECT deployment_build_leases.*
      FROM deployment_build_leases
      JOIN locked_deployment
        ON locked_deployment.org_id = deployment_build_leases.org_id
       AND locked_deployment.project_id = deployment_build_leases.project_id
       AND locked_deployment.environment_id = deployment_build_leases.environment_id
       AND locked_deployment.id = deployment_build_leases.deployment_id
     WHERE deployment_build_leases.id = sqlc.arg(build_lease_id)
       AND deployment_build_leases.lease_sequence = sqlc.arg(lease_sequence)
       AND deployment_build_leases.worker_group_id = sqlc.arg(worker_group_id)
       AND deployment_build_leases.worker_instance_id = sqlc.arg(worker_instance_id)
       AND deployment_build_leases.worker_epoch = sqlc.arg(worker_epoch)
       AND deployment_build_leases.worker_protocol_version = sqlc.arg(worker_protocol_version)
     FOR UPDATE OF deployment_build_leases
)
SELECT locked_lease.*,
       locked_deployment.status AS deployment_status,
       locked_deployment.current_build_lease_id,
       locked_deployment.build_architecture,
       locked_deployment.build_runtime_digest,
       locked_deployment.build_standard_toolchain_digest,
       locked_deployment.build_contract_version,
       source_artifacts.digest AS deployment_source_digest
  FROM locked_lease
  JOIN locked_deployment
    ON locked_deployment.org_id = locked_lease.org_id
   AND locked_deployment.id = locked_lease.deployment_id
  JOIN artifacts AS source_artifacts
    ON source_artifacts.org_id = locked_deployment.org_id
   AND source_artifacts.project_id = locked_deployment.project_id
   AND source_artifacts.environment_id = locked_deployment.environment_id
   AND source_artifacts.id = locked_deployment.deployment_source_artifact_id
   AND source_artifacts.kind = 'deployment_source';

-- name: LockDeploymentBuildWorkerCertification :one
WITH locked_group AS MATERIALIZED (
    SELECT worker_groups.id
      FROM worker_groups
     WHERE worker_groups.id = sqlc.arg(worker_group_id)
       AND worker_groups.state IN ('active', 'draining')
       AND worker_groups.allows_build
     FOR UPDATE
)
SELECT worker_instances.*,
       runtime_identities.rootfs_digest,
       runtime_identities.runtime_abi,
       runtime_identities.runtime_arch
  FROM worker_instances
  JOIN locked_group
    ON locked_group.id = worker_instances.worker_group_id
  LEFT JOIN runtime_identities
    ON runtime_identities.id = worker_instances.runtime_identity_id
 WHERE worker_instances.id = sqlc.arg(worker_instance_id)
   AND worker_instances.current_epoch = sqlc.arg(worker_epoch)::bigint
   AND worker_instances.protocol_version = sqlc.arg(worker_protocol_version)
   AND worker_instances.state IN ('active', 'draining')
   AND worker_instances.certified_at IS NOT NULL
   AND worker_instances.supports_build
   AND worker_instances.toolchain_catalog_digest IS NOT NULL
 FOR UPDATE OF worker_instances;

-- name: GetDeploymentBuildTerminalResult :one
SELECT state, terminal_request_fingerprint
  FROM deployment_build_leases
 WHERE org_id = sqlc.arg(org_id) AND deployment_id = sqlc.arg(deployment_id)
   AND id = sqlc.arg(build_lease_id)
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
       AND deployment_build_leases.lease_sequence = sqlc.arg(lease_sequence)
       AND deployment_build_leases.state = 'running' AND deployment_build_leases.expires_at > now()
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
           failed.deployment_id, failed.id, NULL::int,
           failed.trace_id, failed.span_id, 'active_time',
           extract(epoch FROM (failed.terminal_at - failed.started_at)) * 1000,
           'milliseconds', failed.started_at, failed.terminal_at,
           jsonb_build_object(
               'outcome','failed', 'reason_code',failed.terminal_reason_code,
               'cpu_millis',failed.requested_cpu_millis,
               'memory_bytes',failed.requested_memory_bytes,
               'workload_disk_bytes',failed.requested_workload_disk_bytes,
               'scratch_bytes',failed.requested_scratch_bytes,
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

-- name: FailDeploymentBuildDelivery :one
WITH locked_deployment AS MATERIALIZED (
    SELECT deployments.*
      FROM deployments
     WHERE deployments.org_id = sqlc.arg(org_id)
       AND deployments.project_id = sqlc.arg(project_id)
       AND deployments.environment_id = sqlc.arg(environment_id)
       AND deployments.id = sqlc.arg(deployment_id)
     FOR UPDATE
), locked_lease AS MATERIALIZED (
    SELECT deployment_build_leases.*
      FROM deployment_build_leases
      JOIN locked_deployment
        ON locked_deployment.org_id = deployment_build_leases.org_id
       AND locked_deployment.project_id = deployment_build_leases.project_id
       AND locked_deployment.environment_id = deployment_build_leases.environment_id
       AND locked_deployment.id = deployment_build_leases.deployment_id
     WHERE deployment_build_leases.id = sqlc.arg(build_lease_id)
       AND deployment_build_leases.lease_sequence = sqlc.arg(lease_sequence)
       AND deployment_build_leases.worker_group_id = sqlc.arg(worker_group_id)
       AND deployment_build_leases.worker_instance_id = sqlc.arg(worker_instance_id)
       AND deployment_build_leases.worker_epoch = sqlc.arg(worker_epoch)
       AND deployment_build_leases.worker_protocol_version = sqlc.arg(worker_protocol_version)
       AND (
           (
               deployment_build_leases.state = 'running'
               AND deployment_build_leases.expires_at > now()
               AND locked_deployment.status = 'building'
               AND locked_deployment.current_build_lease_id = deployment_build_leases.id
           )
           OR
           (
               deployment_build_leases.state = 'lost'
               AND deployment_build_leases.terminal_reason_code = sqlc.arg(reason_code)
               AND deployment_build_leases.terminal_request_fingerprint IS NULL
           )
       )
     FOR UPDATE OF deployment_build_leases
), lost AS (
    UPDATE deployment_build_leases
       SET state = 'lost',
           terminal_at = now(),
           terminal_reason_code = sqlc.arg(reason_code),
           terminal_error = NULL,
           terminal_request_fingerprint = NULL,
           updated_at = now()
      FROM locked_lease
     WHERE deployment_build_leases.id = locked_lease.id
       AND locked_lease.state = 'running'
    RETURNING deployment_build_leases.*
), meter_event AS (
    INSERT INTO meter_events (
        org_id, project_id, environment_id, deployment_id,
        deployment_build_lease_id, attempt_number, trace_id, span_id, meter,
        quantity, unit, measured_from, measured_to, details,
        idempotency_key, idempotency_fingerprint
    )
    SELECT lost.org_id, lost.project_id, lost.environment_id,
           lost.deployment_id, lost.id, NULL::int,
           lost.trace_id, lost.span_id, 'active_time',
           extract(epoch FROM (lost.terminal_at - lost.started_at)) * 1000,
           'milliseconds', lost.started_at, lost.terminal_at,
           jsonb_build_object(
               'outcome', 'lease_lost_requeued',
               'reason_code', sqlc.arg(reason_code),
               'cpu_millis', lost.requested_cpu_millis,
               'memory_bytes', lost.requested_memory_bytes,
               'workload_disk_bytes', lost.requested_workload_disk_bytes,
               'scratch_bytes', lost.requested_scratch_bytes,
               'build_executors', lost.requested_build_executors
           ),
           'build-lease-lost:' || lost.id::text,
           jsonb_build_object(
               'quantity', extract(epoch FROM (lost.terminal_at - lost.started_at)) * 1000,
               'unit', 'milliseconds',
               'measured_from', lost.started_at,
               'measured_to', lost.terminal_at,
               'outcome', 'lease_lost_requeued',
               'reason_code', sqlc.arg(reason_code),
               'cpu_millis', lost.requested_cpu_millis,
               'memory_bytes', lost.requested_memory_bytes,
               'workload_disk_bytes', lost.requested_workload_disk_bytes,
               'scratch_bytes', lost.requested_scratch_bytes,
               'build_executors', lost.requested_build_executors
           )::text
      FROM lost
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
           environment_id, deployment_id, id, NULL::int, trace_id, span_id,
           meter, details, idempotency_key, occurred_at
      FROM meter_event
    ON CONFLICT DO NOTHING
    RETURNING meter_event_id
), transitioned_deployment AS (
    UPDATE deployments
       SET current_build_lease_id = CASE
               WHEN lost.lease_sequence < 3 THEN NULL
               ELSE deployments.current_build_lease_id
           END,
           status = CASE
               WHEN lost.lease_sequence < 3 THEN 'building'::deployment_status
               ELSE 'failed'::deployment_status
           END,
           failure = CASE
               WHEN lost.lease_sequence < 3 THEN deployments.failure
               ELSE jsonb_build_object('reason_code', 'build_delivery_exhausted')
           END,
           failed_at = CASE
               WHEN lost.lease_sequence < 3 THEN deployments.failed_at
               ELSE now()
           END,
           updated_at = now()
      FROM lost
     WHERE deployments.org_id = lost.org_id
       AND deployments.id = lost.deployment_id
       AND deployments.current_build_lease_id = lost.id
       AND EXISTS (
           SELECT 1
             FROM meter_outbox
            WHERE meter_outbox.meter_event_id = (
                SELECT id
                  FROM meter_event
                 WHERE meter_event.deployment_build_lease_id = lost.id
            )
       )
    RETURNING deployments.status
), outcome AS (
    SELECT lost.state,
           lost.terminal_reason_code,
           lost.terminal_at,
           lost.lease_sequence,
           false AS replayed
      FROM lost
    UNION ALL
    SELECT locked_lease.state,
           locked_lease.terminal_reason_code,
           locked_lease.terminal_at,
           locked_lease.lease_sequence,
           true AS replayed
      FROM locked_lease
     WHERE locked_lease.state = 'lost'
)
SELECT outcome.state,
       outcome.terminal_reason_code,
       outcome.terminal_at,
       outcome.lease_sequence,
       locked_deployment.id AS deployment_id,
       CASE WHEN outcome.replayed
           THEN locked_deployment.status
           ELSE transitioned_deployment.status
       END::deployment_status AS deployment_status,
       outcome.replayed
  FROM outcome
 CROSS JOIN locked_deployment
  LEFT JOIN transitioned_deployment ON NOT outcome.replayed
 WHERE outcome.replayed OR transitioned_deployment.status IS NOT NULL;

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
