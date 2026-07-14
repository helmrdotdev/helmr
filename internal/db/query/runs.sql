-- name: CreateScopedRun :one
WITH scope AS (
    SELECT sessions.org_id, sessions.project_id, sessions.environment_id,
           sessions.id AS session_id, sessions.workspace_id, workspaces.region_id
      FROM sessions
      JOIN workspaces ON workspaces.org_id = sessions.org_id
                     AND workspaces.project_id = sessions.project_id
                     AND workspaces.environment_id = sessions.environment_id
                     AND workspaces.id = sessions.workspace_id
     WHERE sessions.org_id = sqlc.arg(org_id) AND sessions.project_id = sqlc.arg(project_id)
       AND sessions.environment_id = sqlc.arg(environment_id)
       AND sessions.id = sqlc.arg(session_id) AND sessions.workspace_id = sqlc.arg(workspace_id)
), task AS (
    SELECT deployment_tasks.*,
           deployment_sandboxes.runtime_abi AS sandbox_runtime_abi,
           deployment_sandboxes.rootfs_digest AS sandbox_rootfs_digest
      FROM deployment_tasks
      JOIN deployments ON deployments.org_id = deployment_tasks.org_id
                      AND deployments.project_id = deployment_tasks.project_id
                      AND deployments.environment_id = deployment_tasks.environment_id
                      AND deployments.id = deployment_tasks.deployment_id
                      AND deployments.status = 'deployed'
      JOIN deployment_sandboxes
        ON deployment_sandboxes.org_id = deployment_tasks.org_id
       AND deployment_sandboxes.project_id = deployment_tasks.project_id
       AND deployment_sandboxes.environment_id = deployment_tasks.environment_id
       AND deployment_sandboxes.deployment_id = deployment_tasks.deployment_id
       AND deployment_sandboxes.id = deployment_tasks.deployment_sandbox_id
     WHERE deployment_tasks.org_id = sqlc.arg(org_id)
       AND deployment_tasks.project_id = sqlc.arg(project_id)
       AND deployment_tasks.environment_id = sqlc.arg(environment_id)
       AND deployment_tasks.deployment_id = sqlc.arg(deployment_id)
       AND deployment_tasks.id = sqlc.arg(deployment_task_id)
       AND deployment_tasks.task_id = sqlc.arg(task_id)
), runtime_target AS (
    SELECT runtime_identities.*
      FROM scope
      JOIN task ON true
      JOIN worker_groups
        ON worker_groups.region_id = scope.region_id
       AND worker_groups.state = 'active'
       AND worker_groups.allows_run
      JOIN worker_instances
        ON worker_instances.worker_group_id = worker_groups.id
       AND worker_instances.supports_run
       AND worker_instances.runtime_identity_id IS NOT NULL
       AND worker_instances.certified_at IS NOT NULL
      JOIN runtime_identities
        ON runtime_identities.id = worker_instances.runtime_identity_id
       AND runtime_identities.runtime_abi = task.sandbox_runtime_abi
       AND runtime_identities.rootfs_digest = task.sandbox_rootfs_digest
     ORDER BY (worker_instances.state = 'active') DESC,
              worker_instances.certified_at DESC,
              runtime_identities.id
     LIMIT 1
), created AS (
    INSERT INTO runs (
        id, public_id, org_id, project_id, environment_id, deployment_id,
        deployment_task_id, workspace_id, deployment_version, api_version,
        sdk_version, cli_version, task_id, session_id, payload, metadata, tags,
        locked_retry_policy, queue_name, queue_concurrency_limit, concurrency_key,
        priority, queue_timestamp, ttl, queued_expires_at, requested_milli_cpu,
        requested_memory_mib, requested_disk_mib, requested_execution_slots,
        runtime_identity_id, runtime_arch, runtime_abi, kernel_digest,
        initramfs_digest, rootfs_digest, cni_profile, network_policy,
        resource_placement_policy, max_active_duration_ms, trace_id, root_span_id,
        current_attempt_number, schedule_id, schedule_instance_id, scheduled_at
    )
    SELECT sqlc.arg(id), sqlc.arg(public_id), scope.org_id, scope.project_id,
           scope.environment_id, sqlc.arg(deployment_id), sqlc.arg(deployment_task_id),
           scope.workspace_id, sqlc.arg(deployment_version), sqlc.arg(api_version),
           sqlc.arg(sdk_version), sqlc.arg(cli_version), sqlc.arg(task_id), scope.session_id,
           sqlc.arg(payload), COALESCE(sqlc.arg(metadata)::jsonb, '{}'::jsonb),
           COALESCE(sqlc.arg(tags)::text[], '{}'::text[]),
           COALESCE(sqlc.arg(locked_retry_policy)::jsonb, '{"enabled":false}'::jsonb),
           sqlc.arg(queue_name), sqlc.narg(queue_concurrency_limit), sqlc.narg(concurrency_key),
           sqlc.arg(priority), sqlc.arg(queue_timestamp), sqlc.arg(ttl),
           sqlc.narg(queued_expires_at), task.requested_milli_cpu,
           task.requested_memory_mib, task.requested_disk_mib,
           task.requested_execution_slots, runtime_target.id,
           runtime_target.runtime_arch, runtime_target.runtime_abi,
           runtime_target.kernel_digest, runtime_target.initramfs_digest,
           runtime_target.rootfs_digest, runtime_target.cni_profile,
           task.network_policy, task.resource_placement_policy,
           sqlc.arg(max_active_duration_ms), sqlc.arg(trace_id), sqlc.arg(root_span_id),
           1, sqlc.narg(schedule_id), sqlc.narg(schedule_instance_id), sqlc.narg(scheduled_at)
      FROM scope JOIN task ON true JOIN runtime_target ON true
     WHERE sqlc.narg(schedule_instance_id)::uuid IS NULL
        OR EXISTS (
            SELECT 1 FROM task_schedule_instances
             WHERE task_schedule_instances.id = sqlc.narg(schedule_instance_id)::uuid
               AND task_schedule_instances.schedule_id = sqlc.narg(schedule_id)::uuid
               AND task_schedule_instances.generation = sqlc.arg(schedule_generation)
        )
    RETURNING *
), snap AS (
    INSERT INTO run_state_snapshots
        (org_id, run_id, version, status, execution_status, attempt_number,
         operation_id, transition, reason)
    SELECT org_id, id, state_version, status, execution_status,
           current_attempt_number, NULL, 'run.created', sqlc.arg(event_payload)
      FROM created RETURNING run_id
)
SELECT created.* FROM created JOIN snap ON snap.run_id = created.id;

-- name: GetRun :one
SELECT * FROM runs WHERE org_id = sqlc.arg(org_id) AND id = sqlc.arg(id);

-- name: ExpireQueuedRuns :exec
WITH expired AS (
    UPDATE runs SET status = 'expired', execution_status = 'finished',
                    terminal_outcome = 'expired', state_version = state_version + 1,
                    error_message = 'run ttl expired before execution started',
                    finished_at = now(), updated_at = now()
     WHERE runs.org_id = sqlc.arg(org_id) AND runs.status = 'queued'
       AND runs.current_run_lease_id IS NULL AND runs.queued_expires_at <= now()
    RETURNING *
)
INSERT INTO run_state_snapshots
    (org_id, run_id, version, status, execution_status, terminal_outcome,
     attempt_number, previous_version, transition, reason)
SELECT expired.org_id, expired.id, expired.state_version, expired.status,
       expired.execution_status, expired.terminal_outcome,
       expired.current_attempt_number, expired.state_version - 1, 'run.expired',
       jsonb_build_object('message','run ttl expired before execution started')
  FROM expired;

-- name: FailQueuedRun :exec
WITH failed AS (
    UPDATE runs SET status = 'failed', execution_status = 'finished',
                    terminal_outcome = 'failed', state_version = state_version + 1,
                    error_message = sqlc.arg(error_message), finished_at = now(), updated_at = now()
     WHERE runs.org_id = sqlc.arg(org_id) AND runs.id = sqlc.arg(run_id)
       AND runs.status = 'queued' AND runs.current_run_lease_id IS NULL
       AND runs.state_version = sqlc.arg(expected_run_state_version)
    RETURNING *
)
INSERT INTO run_state_snapshots
    (org_id, run_id, version, status, execution_status, terminal_outcome,
     attempt_number, previous_version, transition, reason, error)
SELECT failed.org_id, failed.id, failed.state_version, failed.status,
       failed.execution_status, failed.terminal_outcome,
       failed.current_attempt_number, failed.state_version - 1, 'run.failed',
       jsonb_build_object('message', sqlc.arg(error_message)::text),
       jsonb_build_object('message', sqlc.arg(error_message)::text)
  FROM failed;

-- name: GetRunSummary :one
SELECT runs.* FROM runs
 WHERE org_id = sqlc.arg(org_id) AND id = sqlc.arg(id);

-- name: CountScopedRunsByStatus :one
SELECT count(*) FILTER (WHERE status = 'queued')::bigint AS queued,
       count(*) FILTER (WHERE status = 'running')::bigint AS running,
       count(*) FILTER (WHERE status = 'waiting')::bigint AS waiting,
       count(*) FILTER (WHERE status = 'succeeded')::bigint AS succeeded,
       count(*) FILTER (WHERE status = 'failed')::bigint AS failed,
       count(*) FILTER (WHERE status = 'cancelled')::bigint AS cancelled,
       count(*) FILTER (WHERE status = 'expired')::bigint AS expired
  FROM runs
 WHERE org_id = sqlc.arg(org_id) AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id);

-- name: ListScopedRunSummaries :many
SELECT * FROM runs
 WHERE org_id = sqlc.arg(org_id) AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND (sqlc.arg(status_filter)::text = 'all' OR status::text = sqlc.arg(status_filter)::text)
   AND (sqlc.narg(session_id)::uuid IS NULL OR session_id = sqlc.narg(session_id)::uuid)
 ORDER BY created_at DESC, id DESC LIMIT sqlc.arg(row_limit);

-- name: CreateRunOperation :one
INSERT INTO run_operations (
    id, public_id, org_id, project_id, environment_id, run_id,
    expected_run_state_version, kind, actor_kind, actor_id, api_key_id,
    reason, request, idempotency_key
)
SELECT sqlc.arg(id), sqlc.arg(public_id), runs.org_id, runs.project_id,
       runs.environment_id, runs.id, runs.state_version, sqlc.arg(kind),
       sqlc.arg(actor_kind), sqlc.arg(actor_id), sqlc.narg(api_key_id),
       sqlc.arg(reason), sqlc.arg(request), sqlc.arg(idempotency_key)
 FROM runs
 WHERE runs.org_id = sqlc.arg(org_id) AND runs.id = sqlc.arg(run_id)
ON CONFLICT (org_id, project_id, environment_id, run_id, kind, idempotency_key)
    WHERE idempotency_key <> ''
DO UPDATE SET idempotency_key = excluded.idempotency_key
RETURNING *;

-- name: GetRunOperation :one
SELECT * FROM run_operations
 WHERE org_id = sqlc.arg(org_id) AND run_id = sqlc.arg(run_id) AND id = sqlc.arg(id);

-- name: MarkRunOperationApplied :one
UPDATE run_operations SET status = 'applied', result = sqlc.arg(result), applied_at = now()
 WHERE org_id = sqlc.arg(org_id) AND run_id = sqlc.arg(run_id) AND id = sqlc.arg(id)
   AND status = 'requested'
RETURNING *;

-- name: MarkRunOperationRejected :one
UPDATE run_operations SET status = 'rejected', result = sqlc.arg(result), rejected_at = now()
 WHERE org_id = sqlc.arg(org_id) AND run_id = sqlc.arg(run_id) AND id = sqlc.arg(id)
   AND status = 'requested'
RETURNING *;

-- name: CancelRun :one
WITH operation AS (
    SELECT * FROM run_operations
     WHERE run_operations.org_id = sqlc.arg(org_id) AND run_operations.run_id = sqlc.arg(run_id)
       AND run_operations.id = sqlc.arg(operation_id) AND run_operations.kind = 'cancel' AND run_operations.status = 'requested'
     FOR UPDATE
), target AS (
    SELECT runs.* FROM runs, operation
     WHERE runs.org_id = operation.org_id AND runs.id = operation.run_id
       AND runs.state_version = operation.expected_run_state_version
       AND runs.status NOT IN ('succeeded','failed','cancelled','expired')
     FOR UPDATE OF runs
), cancelled_lease AS (
    UPDATE run_leases
       SET state = 'cancelled', terminal_at = now(),
           terminal_reason_code = 'cancel_operation', updated_at = now()
      FROM target
     WHERE run_leases.org_id = target.org_id AND run_leases.run_id = target.id
       AND run_leases.id = target.current_run_lease_id
       AND run_leases.state IN ('assigned','starting','running','checkpointing')
    RETURNING run_leases.*
), released_workspace_leases AS (
    UPDATE workspace_leases
       SET state = 'released', released_at = now(), terminal_at = now(),
           terminal_reason_code = 'run_cancelled', updated_at = now()
      FROM target
     WHERE workspace_leases.org_id = target.org_id
       AND workspace_leases.owner_run_id = target.id
       AND workspace_leases.lease_kind = 'write'
       AND workspace_leases.state IN ('active','releasing')
    RETURNING workspace_leases.id
), requested_runtime_close AS (
    UPDATE runtime_instances
       SET desired_state = 'closed', desired_version = desired_version + 1,
           desired_at = now(), desired_reason = 'run_cancelled', updated_at = now()
      FROM cancelled_lease
     WHERE runtime_instances.org_id = cancelled_lease.org_id
       AND runtime_instances.id = cancelled_lease.runtime_instance_id
       AND runtime_instances.worker_group_id = cancelled_lease.worker_group_id
       AND runtime_instances.worker_instance_id = cancelled_lease.worker_instance_id
       AND runtime_instances.worker_epoch = cancelled_lease.worker_epoch
       AND runtime_instances.desired_state <> 'closed'
       AND runtime_instances.observed_state IN ('allocated','preparing','ready')
    RETURNING runtime_instances.id
), cancelled_run_waits AS (
    UPDATE run_waits
       SET state = 'cancelled', cancelled_at = now(), terminal_at = now(),
           terminal_reason_code = 'run_cancelled', updated_at = now()
      FROM target
     WHERE run_waits.org_id = target.org_id AND run_waits.run_id = target.id
       AND run_waits.state IN ('hot_waiting','checkpointing','checkpointed_waiting','resuming')
    RETURNING run_waits.id
), cancelled AS (
    UPDATE runs SET status = 'cancelled', execution_status = 'finished',
                    terminal_outcome = 'cancelled', state_version = runs.state_version + 1,
                    current_run_lease_id = NULL, active_started_at = NULL,
                    active_elapsed_ms = runs.active_elapsed_ms + COALESCE((
                        SELECT GREATEST((extract(epoch FROM (now() - cancelled_lease.started_at)) * 1000)::bigint, 0)
                          FROM cancelled_lease WHERE cancelled_lease.started_at IS NOT NULL
                    ), 0),
                    finished_at = COALESCE(runs.finished_at, now()), updated_at = now()
      FROM target
     WHERE runs.org_id = target.org_id AND runs.id = target.id
    RETURNING runs.*
), meter_event AS (
    INSERT INTO meter_events (
        org_id, project_id, environment_id, run_id, run_lease_id, attempt_number,
        trace_id, span_id, meter, quantity, unit, measured_from, measured_to,
        details, idempotency_key, idempotency_fingerprint
    )
    SELECT cancelled.org_id, cancelled.project_id, cancelled.environment_id,
           cancelled.id, cancelled_lease.id, cancelled_lease.task_attempt_number,
           cancelled_lease.trace_id, cancelled_lease.span_id, 'active_time',
           GREATEST((extract(epoch FROM (now() - cancelled_lease.started_at)) * 1000)::bigint, 0),
           'milliseconds', cancelled_lease.started_at, now(),
           jsonb_build_object('transition','cancelled','cpu_millis',cancelled_lease.requested_cpu_millis,
               'memory_bytes',cancelled_lease.requested_memory_bytes,
               'workload_disk_bytes',cancelled_lease.requested_workload_disk_bytes,
               'scratch_bytes',cancelled_lease.requested_scratch_bytes,
               'execution_slots',cancelled_lease.requested_execution_slots),
           'cancel:' || cancelled_lease.id::text,
           jsonb_build_object('quantity',GREATEST((extract(epoch FROM (now() - cancelled_lease.started_at)) * 1000)::bigint, 0),
               'unit','milliseconds','measured_from',cancelled_lease.started_at,'measured_to',now(),
               'transition','cancelled','cpu_millis',cancelled_lease.requested_cpu_millis,
               'memory_bytes',cancelled_lease.requested_memory_bytes,
               'workload_disk_bytes',cancelled_lease.requested_workload_disk_bytes,
               'scratch_bytes',cancelled_lease.requested_scratch_bytes,
               'execution_slots',cancelled_lease.requested_execution_slots)::text
      FROM cancelled, cancelled_lease
     WHERE cancelled_lease.started_at IS NOT NULL AND cancelled_lease.started_at < now()
    ON CONFLICT (org_id, source_type, source_id, meter, idempotency_key)
    DO UPDATE SET idempotency_fingerprint = meter_events.idempotency_fingerprint
     WHERE meter_events.idempotency_fingerprint = excluded.idempotency_fingerprint
    RETURNING *
), meter_outbox AS (
    INSERT INTO telemetry_outbox (
        org_id, stream_kind, source_kind, source_id, project_id, environment_id,
        run_id, run_lease_id, meter_event_id, attempt_number, trace_id, span_id,
        kind, payload, idempotency_key, observed_at
    )
    SELECT org_id, 'meter_event', source_type, source_id, project_id, environment_id,
           run_id, run_lease_id, id, attempt_number, trace_id, span_id,
           meter, details, idempotency_key, occurred_at
      FROM meter_event
    ON CONFLICT DO NOTHING
    RETURNING meter_event_id
), snapshot AS (
    INSERT INTO run_state_snapshots
        (org_id, run_id, version, status, execution_status, terminal_outcome,
         attempt_number, run_lease_id, operation_id, previous_version,
         transition, reason)
    SELECT cancelled.org_id, cancelled.id, cancelled.state_version,
           cancelled.status, cancelled.execution_status, cancelled.terminal_outcome,
           cancelled.current_attempt_number, target.current_run_lease_id,
           operation.id, cancelled.state_version - 1, 'run.cancelled',
           jsonb_build_object('message', operation.reason)
      FROM cancelled JOIN operation ON operation.run_id = cancelled.id
                     JOIN target ON target.id = cancelled.id
     WHERE NOT EXISTS (SELECT 1 FROM meter_event) OR EXISTS (SELECT 1 FROM meter_outbox)
    RETURNING run_id
)
UPDATE run_operations SET status = 'applied', applied_at = now(),
                          result = jsonb_build_object('state_version', cancelled.state_version)
  FROM cancelled, snapshot
 WHERE run_operations.id = sqlc.arg(operation_id)
   AND snapshot.run_id = cancelled.id
RETURNING cancelled.*;

-- name: UpdateRunMetadataForExecution :one
UPDATE runs
   SET metadata = CASE sqlc.arg(operation)::text
         WHEN 'set' THEN jsonb_set(runs.metadata, ARRAY[sqlc.arg(key)::text], sqlc.arg(value)::jsonb, true)
         WHEN 'patch' THEN runs.metadata || sqlc.arg(patch)::jsonb
         WHEN 'increment' THEN jsonb_set(
             runs.metadata, ARRAY[sqlc.arg(key)::text],
             to_jsonb(COALESCE((runs.metadata ->> sqlc.arg(key)::text)::numeric, 0) + sqlc.arg(amount)::numeric), true)
         ELSE runs.metadata
       END,
       updated_at = now()
  FROM run_leases
 WHERE runs.org_id = sqlc.arg(org_id) AND runs.id = sqlc.arg(run_id)
   AND runs.current_run_lease_id = sqlc.arg(run_lease_id)
   AND run_leases.org_id = runs.org_id AND run_leases.run_id = runs.id
   AND run_leases.id = runs.current_run_lease_id
   AND runs.status IN ('running','waiting')
   AND run_leases.state IN ('starting','running','checkpointing')
   AND run_leases.expires_at > now()
   AND octet_length((CASE sqlc.arg(operation)::text
         WHEN 'set' THEN jsonb_set(runs.metadata, ARRAY[sqlc.arg(key)::text], sqlc.arg(value)::jsonb, true)
         WHEN 'patch' THEN runs.metadata || sqlc.arg(patch)::jsonb
         WHEN 'increment' THEN jsonb_set(
             runs.metadata, ARRAY[sqlc.arg(key)::text],
             to_jsonb(COALESCE((runs.metadata ->> sqlc.arg(key)::text)::numeric, 0) + sqlc.arg(amount)::numeric), true)
         ELSE runs.metadata END)::text) <= sqlc.arg(max_metadata_bytes)::integer
RETURNING runs.*, false AS metadata_too_large;
