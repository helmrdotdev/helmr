-- name: RequeueExpiredLeasedRunLeases :exec
WITH expired AS (
    UPDATE run_leases
       SET state = 'expired', terminal_at = now(), terminal_reason_code = 'lease_expired',
           updated_at = now()
     WHERE state IN ('assigned', 'starting') AND expires_at <= now()
    RETURNING *
), released_workspace_leases AS (
    UPDATE workspace_leases
       SET state = 'released', released_at = now(), terminal_at = now(),
           terminal_reason_code = 'run_lease_expired', updated_at = now()
      FROM expired
     WHERE workspace_leases.org_id = expired.org_id
       AND workspace_leases.owner_run_id = expired.run_id
       AND workspace_leases.lease_kind = 'write'
       AND workspace_leases.state IN ('active','releasing')
    RETURNING workspace_leases.id
), requested_mount_stop AS (
    UPDATE workspace_mounts
       SET state = 'unmounting', stopped_at = COALESCE(stopped_at, now()), updated_at = now()
      FROM expired
     WHERE workspace_mounts.org_id = expired.org_id
       AND workspace_mounts.workspace_id = expired.workspace_id
       AND workspace_mounts.runtime_instance_id = expired.runtime_instance_id
       AND workspace_mounts.state IN ('mounting','mounted')
    RETURNING workspace_mounts.id
), requested_runtime_close AS (
    UPDATE runtime_instances
       SET desired_state = 'closed', desired_version = desired_version + 1,
           desired_at = now(), desired_reason = 'run_lease_expired', updated_at = now()
      FROM expired
     WHERE runtime_instances.org_id = expired.org_id
       AND runtime_instances.id = expired.runtime_instance_id
       AND runtime_instances.worker_group_id = expired.worker_group_id
       AND runtime_instances.worker_instance_id = expired.worker_instance_id
       AND runtime_instances.worker_epoch = expired.worker_epoch
       AND runtime_instances.desired_state <> 'closed'
       AND runtime_instances.observed_state IN ('allocated','preparing','ready')
    RETURNING runtime_instances.id
), transitioned AS (
UPDATE runs
   SET current_run_lease_id = NULL, status = 'queued', execution_status = 'queued',
       state_version = state_version + 1, queue_timestamp = now(), updated_at = now()
  FROM expired
 WHERE runs.org_id = expired.org_id AND runs.id = expired.run_id
   AND runs.current_run_lease_id = expired.id
RETURNING runs.*, expired.id AS expired_run_lease_id
), requeued_resume_wait AS (
    UPDATE run_waits
       SET state = 'checkpointed_waiting', current_run_lease_id = NULL,
           resuming_at = NULL,
           expected_run_state_version = transitioned.state_version,
           updated_at = now()
      FROM transitioned
     WHERE run_waits.org_id = transitioned.org_id
       AND run_waits.run_id = transitioned.id
       AND run_waits.current_run_lease_id = transitioned.expired_run_lease_id
       AND run_waits.state = 'resuming'
       AND run_waits.run_checkpoint_id IS NOT NULL
       AND run_waits.resume_ack_version < run_waits.resume_request_version
    RETURNING run_waits.id, run_waits.org_id, run_waits.run_id
)
INSERT INTO run_state_snapshots
    (org_id, run_id, version, status, execution_status, terminal_outcome,
     attempt_number, run_lease_id, previous_version, transition, reason)
SELECT transitioned.org_id, transitioned.id, transitioned.state_version,
       transitioned.status, transitioned.execution_status, transitioned.terminal_outcome,
       current_attempt_number, expired_run_lease_id, state_version - 1,
       'run.lease_expired_requeued', jsonb_build_object('reason_code','lease_expired')
  FROM transitioned
  LEFT JOIN requeued_resume_wait
         ON requeued_resume_wait.org_id = transitioned.org_id
        AND requeued_resume_wait.run_id = transitioned.id;

-- name: LockRunLeaseConcurrencyScope :exec
SELECT pg_advisory_xact_lock(hashtextextended(concat_ws(':',
    sqlc.arg(org_id)::uuid::text, sqlc.arg(project_id)::uuid::text,
    sqlc.arg(environment_id)::uuid::text, sqlc.arg(queue_class)::text,
    sqlc.arg(queue_name)::text, COALESCE(sqlc.narg(concurrency_key)::text, '')), 0));

-- name: AbandonLeasedRunLease :exec
WITH rejected AS (
    UPDATE run_leases
       SET state = 'rejected', terminal_at = now(), terminal_reason_code = sqlc.arg(reason_code),
           terminal_error = sqlc.narg(error), updated_at = now()
     WHERE run_leases.org_id = sqlc.arg(org_id) AND run_leases.run_id = sqlc.arg(run_id)
       AND run_leases.id = sqlc.arg(run_lease_id) AND run_leases.worker_instance_id = sqlc.arg(worker_instance_id)
       AND run_leases.worker_group_id = sqlc.arg(worker_group_id)
       AND run_leases.worker_epoch = sqlc.arg(worker_epoch)
       AND run_leases.task_attempt_number = sqlc.arg(attempt_number)::integer
       AND run_leases.lease_sequence = sqlc.arg(lease_sequence)
       AND run_leases.state IN ('assigned', 'starting')
    RETURNING run_leases.*
), transitioned AS (
    UPDATE runs
       SET current_run_lease_id = NULL, status = 'queued', execution_status = 'queued',
           state_version = state_version + 1, queue_timestamp = now(), updated_at = now()
      FROM rejected
     WHERE runs.org_id = rejected.org_id AND runs.id = rejected.run_id
       AND runs.current_run_lease_id = rejected.id
	RETURNING runs.*, rejected.id AS rejected_run_lease_id
), requeued_resume_wait AS (
	UPDATE run_waits
	   SET state = 'checkpointed_waiting', current_run_lease_id = NULL,
	       resuming_at = NULL,
	       expected_run_state_version = transitioned.state_version,
	       updated_at = now()
	  FROM transitioned
	 WHERE run_waits.org_id = transitioned.org_id
	   AND run_waits.run_id = transitioned.id
	   AND run_waits.current_run_lease_id = transitioned.rejected_run_lease_id
	   AND run_waits.state = 'resuming'
	   AND run_waits.run_checkpoint_id IS NOT NULL
	   AND run_waits.resume_ack_version < run_waits.resume_request_version
	RETURNING run_waits.id, run_waits.org_id, run_waits.run_id
), released_workspace_leases AS (
	UPDATE workspace_leases
	   SET state = 'released', released_at = now(), terminal_at = now(),
	       terminal_reason_code = 'run_lease_rejected', updated_at = now()
	  FROM rejected
	 WHERE workspace_leases.org_id = rejected.org_id
	   AND workspace_leases.owner_run_id = rejected.run_id
	   AND workspace_leases.lease_kind = 'write'
	   AND workspace_leases.state IN ('active','releasing')
	RETURNING workspace_leases.id
), requested_mount_stop AS (
	UPDATE workspace_mounts
	   SET state = 'unmounting', stopped_at = COALESCE(stopped_at, now()), updated_at = now()
	  FROM rejected
	 WHERE workspace_mounts.org_id = rejected.org_id
	   AND workspace_mounts.workspace_id = rejected.workspace_id
	   AND workspace_mounts.runtime_instance_id = rejected.runtime_instance_id
	   AND workspace_mounts.state IN ('mounting','mounted')
	RETURNING workspace_mounts.id
), requested_runtime_close AS (
	UPDATE runtime_instances
	   SET desired_state = 'closed', desired_version = desired_version + 1,
	       desired_at = now(), desired_reason = 'run_lease_rejected', updated_at = now()
	  FROM rejected
	 WHERE runtime_instances.org_id = rejected.org_id
	   AND runtime_instances.id = rejected.runtime_instance_id
	   AND runtime_instances.worker_group_id = rejected.worker_group_id
	   AND runtime_instances.worker_instance_id = rejected.worker_instance_id
	   AND runtime_instances.worker_epoch = rejected.worker_epoch
	   AND runtime_instances.desired_state <> 'closed'
	   AND runtime_instances.observed_state IN ('allocated','preparing','ready')
	RETURNING runtime_instances.id
)
INSERT INTO run_state_snapshots
    (org_id, run_id, version, status, execution_status, terminal_outcome,
     attempt_number, run_lease_id, previous_version, transition, reason, error)
SELECT transitioned.org_id, transitioned.id, transitioned.state_version, transitioned.status,
       transitioned.execution_status, transitioned.terminal_outcome,
       transitioned.current_attempt_number, transitioned.rejected_run_lease_id, transitioned.state_version - 1,
       'run.lease_rejected_requeued', jsonb_build_object('reason_code', sqlc.arg(reason_code)::text),
       COALESCE(sqlc.narg(error)::jsonb, '{}'::jsonb)
  FROM transitioned
  LEFT JOIN requeued_resume_wait
         ON requeued_resume_wait.org_id = transitioned.org_id
        AND requeued_resume_wait.run_id = transitioned.id;

-- name: RequeueExpiredRunningRunLeases :exec
WITH expired AS (
    UPDATE run_leases
       SET state = 'lost', terminal_at = now(), terminal_reason_code = 'lease_expired',
           updated_at = now()
     WHERE state IN ('running', 'checkpointing') AND expires_at <= now()
    RETURNING *
), released_workspace_leases AS (
    UPDATE workspace_leases
       SET state = 'released', released_at = now(), terminal_at = now(),
           terminal_reason_code = 'run_lease_expired', updated_at = now()
      FROM expired
     WHERE workspace_leases.org_id = expired.org_id
       AND workspace_leases.owner_run_id = expired.run_id
       AND workspace_leases.lease_kind = 'write'
       AND workspace_leases.state IN ('active','releasing')
    RETURNING workspace_leases.id
), requested_mount_stop AS (
    UPDATE workspace_mounts
       SET state = 'unmounting', stopped_at = COALESCE(stopped_at, now()), updated_at = now()
      FROM expired
     WHERE workspace_mounts.org_id = expired.org_id
       AND workspace_mounts.workspace_id = expired.workspace_id
       AND workspace_mounts.runtime_instance_id = expired.runtime_instance_id
       AND workspace_mounts.state IN ('mounting','mounted')
    RETURNING workspace_mounts.id
), requested_runtime_close AS (
    UPDATE runtime_instances
       SET desired_state = 'closed', desired_version = desired_version + 1,
           desired_at = now(), desired_reason = 'run_lease_expired', updated_at = now()
      FROM expired
     WHERE runtime_instances.org_id = expired.org_id
       AND runtime_instances.id = expired.runtime_instance_id
       AND runtime_instances.worker_group_id = expired.worker_group_id
       AND runtime_instances.worker_instance_id = expired.worker_instance_id
       AND runtime_instances.worker_epoch = expired.worker_epoch
       AND runtime_instances.desired_state <> 'closed'
       AND runtime_instances.observed_state IN ('allocated','preparing','ready')
    RETURNING runtime_instances.id
), transitioned AS (
UPDATE runs
   SET status = 'queued', execution_status = 'queued', terminal_outcome = NULL,
       current_run_lease_id = NULL, state_version = state_version + 1,
       active_elapsed_ms = active_elapsed_ms + GREATEST(
           (extract(epoch FROM (expired.expires_at - expired.started_at)) * 1000)::bigint, 0
       ),
       active_started_at = NULL, error_message = NULL, queue_timestamp = now(), updated_at = now()
  FROM expired
 WHERE runs.org_id = expired.org_id AND runs.id = expired.run_id
   AND runs.current_run_lease_id = expired.id
RETURNING runs.*, expired.id AS expired_run_lease_id
), requeued_resume_wait AS (
    UPDATE run_waits
       SET state = 'checkpointed_waiting', current_run_lease_id = NULL,
           resuming_at = NULL,
           expected_run_state_version = transitioned.state_version,
           updated_at = now()
      FROM transitioned
     WHERE run_waits.org_id = transitioned.org_id
       AND run_waits.run_id = transitioned.id
       AND run_waits.current_run_lease_id = transitioned.expired_run_lease_id
       AND run_waits.state = 'resuming'
       AND run_waits.run_checkpoint_id IS NOT NULL
       AND run_waits.resume_ack_version < run_waits.resume_request_version
    RETURNING run_waits.id, run_waits.org_id, run_waits.run_id
), meter_event AS (
    INSERT INTO meter_events (
        org_id, project_id, environment_id, run_id, run_lease_id, attempt_number,
        trace_id, span_id, meter, quantity, unit, measured_from, measured_to,
        details, idempotency_key, idempotency_fingerprint
    )
    SELECT transitioned.org_id, transitioned.project_id, transitioned.environment_id,
           transitioned.id, expired.id, expired.task_attempt_number,
           expired.trace_id, expired.span_id, 'active_time',
           GREATEST((extract(epoch FROM (expired.expires_at - expired.started_at)) * 1000)::bigint, 0),
           'milliseconds', expired.started_at, expired.expires_at,
           jsonb_build_object(
               'transition','lease_lost_requeued',
               'cpu_millis',expired.requested_cpu_millis,
               'memory_bytes',expired.requested_memory_bytes,
               'workload_disk_bytes',expired.requested_workload_disk_bytes,
               'scratch_bytes',expired.requested_scratch_bytes,
               'execution_slots',expired.requested_execution_slots
           ),
           'lease-lost:' || expired.id::text,
           jsonb_build_object(
               'quantity',GREATEST((extract(epoch FROM (expired.expires_at - expired.started_at)) * 1000)::bigint, 0),
               'unit','milliseconds','measured_from',expired.started_at,'measured_to',expired.expires_at,
               'transition','lease_lost_requeued','cpu_millis',expired.requested_cpu_millis,
               'memory_bytes',expired.requested_memory_bytes,
               'workload_disk_bytes',expired.requested_workload_disk_bytes,
               'scratch_bytes',expired.requested_scratch_bytes,
               'execution_slots',expired.requested_execution_slots
           )::text
      FROM transitioned, expired
     WHERE expired.started_at IS NOT NULL AND expired.started_at < expired.expires_at
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
)
INSERT INTO run_state_snapshots
    (org_id, run_id, version, status, execution_status, terminal_outcome,
     attempt_number, run_lease_id, previous_version, transition, reason, error)
SELECT transitioned.org_id, transitioned.id, transitioned.state_version,
       transitioned.status, transitioned.execution_status,
       transitioned.terminal_outcome, transitioned.current_attempt_number,
       transitioned.expired_run_lease_id, transitioned.state_version - 1,
       'run.lease_lost_requeued', jsonb_build_object('reason_code','lease_expired'),
       jsonb_build_object('message','run lease expired and was redriven')
  FROM transitioned
  LEFT JOIN requeued_resume_wait
         ON requeued_resume_wait.org_id = transitioned.org_id
        AND requeued_resume_wait.run_id = transitioned.id
 WHERE NOT EXISTS (SELECT 1 FROM meter_event) OR EXISTS (SELECT 1 FROM meter_outbox);

-- name: LeaseRunLease :one
WITH target AS (
    SELECT runs.*, workspaces.region_id
      FROM runs
      JOIN workspaces ON workspaces.org_id = runs.org_id
                     AND workspaces.project_id = runs.project_id
                     AND workspaces.environment_id = runs.environment_id
                     AND workspaces.id = runs.workspace_id
     WHERE runs.org_id = sqlc.arg(org_id) AND runs.id = sqlc.arg(run_id)
       AND runs.status = 'queued' AND runs.current_run_lease_id IS NULL
       AND runs.state_version = sqlc.arg(expected_run_state_version)
     FOR UPDATE OF runs
), inserted AS (
    INSERT INTO run_leases (
        id, org_id, project_id, environment_id, run_id, workspace_id, region_id,
        lease_sequence, task_attempt_number, worker_group_id, worker_instance_id,
        worker_epoch, runtime_instance_id, network_slot_id, network_slot_generation,
        queue_name, queue_class, concurrency_key, queue_concurrency_limit,
        runtime_identity_id, worker_protocol_version, requested_cpu_millis,
        requested_memory_bytes, requested_workload_disk_bytes, requested_scratch_bytes,
        requested_execution_slots, resource_snapshot, trace_id, span_id,
        parent_span_id, traceparent, start_deadline_at, expires_at
    )
    SELECT sqlc.arg(run_lease_id), target.org_id, target.project_id,
           target.environment_id, target.id, target.workspace_id, target.region_id,
           sqlc.arg(lease_sequence), target.current_attempt_number,
           sqlc.arg(worker_group_id), sqlc.arg(worker_instance_id), sqlc.arg(worker_epoch),
           sqlc.arg(runtime_instance_id), sqlc.arg(network_slot_id),
           sqlc.arg(network_slot_generation), target.queue_name, target.queue_class,
           target.concurrency_key, target.queue_concurrency_limit, target.runtime_identity_id,
           sqlc.arg(worker_protocol_version), target.requested_milli_cpu,
           target.requested_memory_mib * 1024 * 1024,
           target.requested_disk_mib * 1024 * 1024, sqlc.arg(requested_scratch_bytes),
           target.requested_execution_slots, sqlc.arg(resource_snapshot), target.trace_id,
           sqlc.narg(span_id), sqlc.narg(parent_span_id), sqlc.narg(traceparent),
           sqlc.arg(start_deadline_at), sqlc.arg(expires_at)
      FROM target
    RETURNING run_leases.*
), pointed AS (
    UPDATE runs SET current_run_lease_id = inserted.id, status = 'queued',
                    execution_status = 'queued', state_version = state_version + 1,
                    updated_at = now()
      FROM inserted WHERE runs.id = inserted.run_id
    RETURNING runs.*
), bound_resume_wait AS (
    UPDATE run_waits
       SET current_run_lease_id = inserted.id,
           state = 'resuming', resuming_at = now(),
           expected_run_state_version = pointed.state_version,
           updated_at = now()
      FROM target, inserted, pointed
     WHERE run_waits.org_id = target.org_id
       AND run_waits.run_id = target.id
       AND run_waits.state = 'checkpointed_waiting'
       AND run_waits.current_run_lease_id IS NULL
       AND run_waits.run_checkpoint_id IS NOT NULL
       AND run_waits.resume_ack_version < run_waits.resume_request_version
       AND run_waits.expected_run_state_version = target.state_version
    RETURNING run_waits.id, run_waits.org_id, run_waits.run_id
), snapshot AS (
    INSERT INTO run_state_snapshots
        (org_id, run_id, version, status, execution_status, terminal_outcome,
         attempt_number, run_lease_id, worker_instance_id, worker_epoch,
         runtime_instance_id, previous_version, transition, reason)
    SELECT pointed.org_id, pointed.id, pointed.state_version, pointed.status,
           pointed.execution_status, pointed.terminal_outcome,
           pointed.current_attempt_number, inserted.id, inserted.worker_instance_id,
           inserted.worker_epoch, inserted.runtime_instance_id,
           pointed.state_version - 1, 'run.lease_assigned',
           jsonb_build_object('lease_sequence', inserted.lease_sequence)
      FROM pointed
      JOIN inserted ON inserted.run_id = pointed.id
      LEFT JOIN bound_resume_wait
             ON bound_resume_wait.org_id = pointed.org_id
            AND bound_resume_wait.run_id = pointed.id
    RETURNING run_id
)
SELECT inserted.* FROM inserted JOIN snapshot ON snapshot.run_id = inserted.run_id;

-- name: ClaimAssignedRunLease :one
WITH candidate AS MATERIALIZED (
    SELECT run_leases.id
      FROM run_leases
      JOIN runs ON runs.org_id = run_leases.org_id
               AND runs.id = run_leases.run_id
               AND runs.current_run_lease_id = run_leases.id
      JOIN worker_instances ON worker_instances.id = run_leases.worker_instance_id
                           AND worker_instances.worker_group_id = run_leases.worker_group_id
                           AND worker_instances.current_epoch = run_leases.worker_epoch
                           AND worker_instances.state IN ('active', 'draining')
      JOIN worker_groups ON worker_groups.id = run_leases.worker_group_id
                        AND worker_groups.state IN ('active', 'draining')
      JOIN runtime_instances ON runtime_instances.id = run_leases.runtime_instance_id
                            AND runtime_instances.worker_group_id = run_leases.worker_group_id
                            AND runtime_instances.worker_instance_id = run_leases.worker_instance_id
                            AND runtime_instances.worker_epoch = run_leases.worker_epoch
      JOIN worker_network_slots ON worker_network_slots.id = run_leases.network_slot_id
                        AND worker_network_slots.worker_instance_id = run_leases.worker_instance_id
                        AND worker_network_slots.worker_epoch = run_leases.worker_epoch
                        AND worker_network_slots.generation = run_leases.network_slot_generation
                        AND worker_network_slots.runtime_instance_id = run_leases.runtime_instance_id
                        AND worker_network_slots.state = 'bound'
      JOIN workspace_mounts ON workspace_mounts.org_id = run_leases.org_id
                           AND workspace_mounts.project_id = run_leases.project_id
                           AND workspace_mounts.environment_id = run_leases.environment_id
                           AND workspace_mounts.workspace_id = run_leases.workspace_id
                           AND workspace_mounts.worker_group_id = run_leases.worker_group_id
                           AND workspace_mounts.worker_instance_id = run_leases.worker_instance_id
                           AND workspace_mounts.worker_epoch = run_leases.worker_epoch
                           AND workspace_mounts.runtime_instance_id = run_leases.runtime_instance_id
                           AND workspace_mounts.state = 'mounted'
      JOIN workspace_leases ON workspace_leases.org_id = run_leases.org_id
                           AND workspace_leases.project_id = run_leases.project_id
                           AND workspace_leases.environment_id = run_leases.environment_id
                           AND workspace_leases.workspace_id = run_leases.workspace_id
                           AND workspace_leases.workspace_mount_id = workspace_mounts.id
                           AND workspace_leases.worker_group_id = run_leases.worker_group_id
                           AND workspace_leases.worker_instance_id = run_leases.worker_instance_id
                           AND workspace_leases.worker_epoch = run_leases.worker_epoch
                           AND workspace_leases.runtime_instance_id = run_leases.runtime_instance_id
                           AND workspace_leases.owner_run_id = run_leases.run_id
                           AND workspace_leases.lease_kind = 'write'
                           AND workspace_leases.state = 'active'
                           AND workspace_leases.expires_at > now()
     WHERE run_leases.worker_group_id = sqlc.arg(worker_group_id)
       AND run_leases.worker_instance_id = sqlc.arg(worker_instance_id)
       AND run_leases.worker_epoch = sqlc.arg(worker_epoch)
       AND run_leases.worker_protocol_version = sqlc.arg(worker_protocol_version)
       AND run_leases.state = 'assigned'
       AND run_leases.start_deadline_at > now()
       AND run_leases.expires_at > now()
     ORDER BY run_leases.assigned_at, run_leases.id
     LIMIT 1
     FOR UPDATE OF run_leases SKIP LOCKED
), claimed AS (
    UPDATE run_leases
       SET state = 'starting', claimed_at = now(), updated_at = now()
      FROM candidate
     WHERE run_leases.id = candidate.id
       AND run_leases.state = 'assigned'
    RETURNING run_leases.*
)
SELECT
    runs.id,
    runs.org_id,
    runs.project_id,
    runs.environment_id,
    runs.session_id,
    runs.task_id,
    runs.deployment_version AS run_deployment_version,
    runs.api_version AS run_api_version,
    runs.sdk_version AS run_sdk_version,
    runs.cli_version AS run_cli_version,
    runs.status,
    runs.payload,
    runs.current_attempt_number,
    runs.state_version,
    deployment_tasks.id AS deployment_task_id,
    deployment_tasks.file_path AS deployment_task_file_path,
    deployment_tasks.export_name AS deployment_task_export_name,
    deployment_tasks.handler_entrypoint AS deployment_task_handler_entrypoint,
    task_bundle_artifacts.digest AS deployment_task_bundle_digest,
    deployment_tasks.bundle_format_version AS deployment_task_bundle_format_version,
    deployment_tasks.secret_declarations AS deployment_task_secret_declarations,
    deployments.version AS deployment_version,
    deployments.api_version AS deployment_api_version,
    deployments.sdk_version AS deployment_sdk_version,
    deployments.cli_version AS deployment_cli_version,
    deployments.worker_protocol_version AS deployment_worker_protocol_version,
    source_artifacts.digest AS deployment_source_digest,
    runs.max_active_duration_ms,
    runs.exit_code,
    runs.error_message,
    runs.created_at,
    runs.updated_at,
    runs.started_at,
    runs.finished_at,
    claimed.requested_cpu_millis AS requested_milli_cpu,
    claimed.requested_memory_bytes / 1048576 AS requested_memory_mib,
    claimed.requested_workload_disk_bytes / 1048576 AS requested_disk_mib,
    claimed.requested_execution_slots,
    runtime_identities.id AS requirements_runtime_id,
    runtime_identities.runtime_arch AS requirements_runtime_arch,
    runtime_identities.runtime_abi AS requirements_runtime_abi,
    runtime_identities.kernel_digest AS requirements_kernel_digest,
    runtime_identities.initramfs_digest AS requirements_initramfs_digest,
    runtime_identities.rootfs_digest AS requirements_rootfs_digest,
    runtime_identities.cni_profile AS requirements_cni_profile,
    runtime_instances.network_policy AS requirements_network_policy,
    runs.resource_placement_policy AS requirements_placement,
    claimed.id AS run_lease_id,
    claimed.worker_group_id AS run_lease_worker_group_id,
    claimed.worker_instance_id AS run_lease_worker_instance_id,
    claimed.worker_epoch AS run_lease_worker_epoch,
    claimed.lease_sequence AS run_lease_sequence,
    claimed.runtime_instance_id AS run_lease_runtime_instance_id,
    claimed.network_slot_id AS run_lease_network_slot_id,
    claimed.network_slot_generation AS run_lease_network_slot_generation,
    claimed.task_attempt_number AS run_lease_attempt_number,
    claimed.expires_at AS run_lease_expires_at,
    claimed.worker_protocol_version AS run_lease_worker_protocol_version,
    claimed.trace_id AS run_lease_trace_id,
    claimed.span_id AS run_lease_span_id,
    claimed.traceparent AS run_lease_traceparent,
    run_waits.run_checkpoint_id AS run_lease_restore_run_checkpoint_id,
    run_waits.resume_request_version AS run_lease_restore_resume_request_version,
    runs.active_elapsed_ms AS active_duration_ms,
    workspaces.id AS workspace_id,
    workspace_leases.id AS workspace_lease_id,
    workspace_mounts.id AS workspace_mount_id,
    workspace_mounts.fencing_generation AS workspace_mount_fencing_generation,
    workspace_leases.fencing_token AS workspace_fencing_token,
    workspaces.deployment_sandbox_id AS workspace_deployment_sandbox_id,
    workspace_mounts.image_artifact_format AS workspace_sandbox_image_artifact_format,
    image_artifacts.digest AS workspace_sandbox_image_artifact_digest,
    image_artifacts.size_bytes AS workspace_sandbox_image_artifact_size_bytes,
    image_artifacts.media_type AS workspace_sandbox_image_artifact_media_type,
    workspace_mounts.image_digest AS workspace_sandbox_image_digest,
    workspace_mounts.image_format AS workspace_sandbox_image_format,
    workspace_mounts.rootfs_digest AS workspace_sandbox_rootfs_digest,
    workspace_mounts.runtime_abi AS workspace_runtime_abi,
    workspace_mounts.guestd_abi AS workspace_guestd_abi,
    workspace_mounts.adapter_abi AS workspace_adapter_abi,
    workspace_mounts.base_version_id AS workspace_base_version_id,
    workspace_mounts.workspace_mount_path AS workspace_mount_path,
    workspace_mounts.workspace_artifact_digest AS workspace_artifact_digest,
    workspace_mounts.workspace_artifact_size_bytes AS workspace_artifact_size_bytes,
    workspace_mounts.workspace_artifact_media_type AS workspace_artifact_media_type,
    workspace_mounts.workspace_artifact_encoding AS workspace_artifact_encoding,
    workspace_mounts.workspace_artifact_entry_count AS workspace_artifact_entry_count,
    runtime_substrates.id AS workspace_runtime_substrate_id,
    runtime_substrates.substrate_digest AS workspace_runtime_substrate_digest,
    runtime_substrates.substrate_format AS workspace_runtime_substrate_format,
    runtime_substrates.builder_abi AS workspace_runtime_substrate_builder_abi,
    runtime_substrates.layout_abi AS workspace_runtime_substrate_layout_abi,
    runtime_substrates.substrate_size_bytes AS workspace_runtime_substrate_size_bytes,
    substrate_artifacts.digest AS workspace_runtime_substrate_blob_digest,
    substrate_artifacts.size_bytes AS workspace_runtime_substrate_blob_size_bytes,
    substrate_artifacts.media_type AS workspace_runtime_substrate_blob_media_type
  FROM claimed
  JOIN runs ON runs.org_id = claimed.org_id
           AND runs.id = claimed.run_id
           AND runs.current_run_lease_id = claimed.id
  JOIN deployments ON deployments.org_id = runs.org_id
                  AND deployments.project_id = runs.project_id
                  AND deployments.environment_id = runs.environment_id
                  AND deployments.id = runs.deployment_id
  JOIN deployment_tasks ON deployment_tasks.org_id = runs.org_id
                       AND deployment_tasks.project_id = runs.project_id
                       AND deployment_tasks.environment_id = runs.environment_id
                       AND deployment_tasks.deployment_id = runs.deployment_id
                       AND deployment_tasks.id = runs.deployment_task_id
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
  JOIN runtime_instances ON runtime_instances.id = claimed.runtime_instance_id
                        AND runtime_instances.worker_group_id = claimed.worker_group_id
                        AND runtime_instances.worker_instance_id = claimed.worker_instance_id
                        AND runtime_instances.worker_epoch = claimed.worker_epoch
  JOIN runtime_identities ON runtime_identities.id = claimed.runtime_identity_id
  JOIN workspaces ON workspaces.org_id = claimed.org_id
                 AND workspaces.project_id = claimed.project_id
                 AND workspaces.environment_id = claimed.environment_id
                 AND workspaces.id = claimed.workspace_id
  JOIN workspace_mounts ON workspace_mounts.org_id = claimed.org_id
                       AND workspace_mounts.project_id = claimed.project_id
                       AND workspace_mounts.environment_id = claimed.environment_id
                       AND workspace_mounts.workspace_id = claimed.workspace_id
                       AND workspace_mounts.worker_group_id = claimed.worker_group_id
                       AND workspace_mounts.worker_instance_id = claimed.worker_instance_id
                       AND workspace_mounts.worker_epoch = claimed.worker_epoch
                       AND workspace_mounts.runtime_instance_id = claimed.runtime_instance_id
                       AND workspace_mounts.state = 'mounted'
  JOIN workspace_leases ON workspace_leases.org_id = claimed.org_id
                       AND workspace_leases.project_id = claimed.project_id
                       AND workspace_leases.environment_id = claimed.environment_id
                       AND workspace_leases.workspace_id = claimed.workspace_id
                       AND workspace_leases.workspace_mount_id = workspace_mounts.id
                       AND workspace_leases.owner_run_id = claimed.run_id
                       AND workspace_leases.lease_kind = 'write'
                       AND workspace_leases.state = 'active'
  JOIN artifacts AS image_artifacts
    ON image_artifacts.org_id = workspace_mounts.org_id
   AND image_artifacts.project_id = workspace_mounts.project_id
   AND image_artifacts.environment_id = workspace_mounts.environment_id
   AND image_artifacts.id = workspace_mounts.image_artifact_id
  LEFT JOIN run_waits ON run_waits.org_id = claimed.org_id
                     AND run_waits.run_id = claimed.run_id
                     AND run_waits.current_run_lease_id = claimed.id
                     AND run_waits.state = 'resuming'
  LEFT JOIN runtime_substrates ON runtime_substrates.org_id = runtime_instances.org_id
                              AND runtime_substrates.project_id = runtime_instances.project_id
                              AND runtime_substrates.environment_id = runtime_instances.environment_id
                              AND runtime_substrates.id = runtime_instances.runtime_substrate_id
  LEFT JOIN artifacts AS substrate_artifacts
    ON substrate_artifacts.org_id = runtime_substrates.org_id
   AND substrate_artifacts.project_id = runtime_substrates.project_id
   AND substrate_artifacts.environment_id = runtime_substrates.environment_id
   AND substrate_artifacts.id = runtime_substrates.artifact_id;

-- name: StartRunLease :one
WITH started AS (
    UPDATE run_leases
       SET state = 'running', claimed_at = COALESCE(claimed_at, now()),
           started_at = COALESCE(started_at, now()), renewed_at = now(),
           expires_at = sqlc.arg(expires_at), updated_at = now()
      FROM worker_network_slots, workspace_leases
     WHERE run_leases.org_id = sqlc.arg(org_id) AND run_leases.run_id = sqlc.arg(run_id)
       AND run_leases.id = sqlc.arg(run_lease_id) AND run_leases.lease_sequence = sqlc.arg(lease_sequence)
       AND run_leases.worker_instance_id = sqlc.arg(worker_instance_id)
       AND run_leases.worker_epoch = sqlc.arg(worker_epoch) AND run_leases.runtime_instance_id = sqlc.arg(runtime_instance_id)
       AND run_leases.network_slot_id = sqlc.arg(network_slot_id)
       AND run_leases.network_slot_generation = sqlc.arg(network_slot_generation)
       AND worker_network_slots.id = run_leases.network_slot_id
       AND worker_network_slots.worker_instance_id = run_leases.worker_instance_id
       AND worker_network_slots.worker_epoch = run_leases.worker_epoch
       AND worker_network_slots.generation = run_leases.network_slot_generation
       AND worker_network_slots.runtime_instance_id = run_leases.runtime_instance_id
       AND worker_network_slots.state = 'bound'
       AND workspace_leases.org_id = run_leases.org_id
       AND workspace_leases.owner_run_id = run_leases.run_id
       AND workspace_leases.workspace_mount_id IN (
           SELECT id FROM workspace_mounts
            WHERE runtime_instance_id = run_leases.runtime_instance_id
              AND worker_instance_id = run_leases.worker_instance_id
              AND worker_epoch = run_leases.worker_epoch AND state = 'mounted')
       AND workspace_leases.lease_kind = 'write'
       AND workspace_leases.state = 'active' AND workspace_leases.expires_at > now()
       AND run_leases.state IN ('assigned', 'starting') AND run_leases.start_deadline_at > now()
    RETURNING run_leases.*
), renewed_workspace AS (
    UPDATE workspace_leases
       SET renewed_at = now(), expires_at = sqlc.arg(expires_at), updated_at = now()
      FROM started
     WHERE workspace_leases.org_id = started.org_id
       AND workspace_leases.owner_run_id = started.run_id
       AND workspace_leases.lease_kind = 'write' AND workspace_leases.state = 'active'
    RETURNING workspace_leases.id
), transitioned AS (
UPDATE runs SET status = 'running', execution_status = 'executing',
                state_version = state_version + 1,
                started_at = COALESCE(runs.started_at, now()), active_started_at = now(),
                updated_at = now()
  FROM started, renewed_workspace
 WHERE runs.org_id = started.org_id AND runs.id = started.run_id
   AND runs.current_run_lease_id = started.id
   AND runs.state_version = sqlc.arg(expected_run_state_version)
RETURNING runs.*, started.id AS started_run_lease_id,
          started.worker_instance_id AS started_worker_instance_id,
          started.worker_epoch AS started_worker_epoch,
          started.runtime_instance_id AS started_runtime_instance_id
), aligned_resume_wait AS (
    UPDATE run_waits
       SET expected_run_state_version = transitioned.state_version,
           updated_at = now()
      FROM transitioned
     WHERE run_waits.org_id = transitioned.org_id
       AND run_waits.run_id = transitioned.id
       AND run_waits.current_run_lease_id = transitioned.started_run_lease_id
       AND run_waits.state = 'resuming'
       AND run_waits.resume_ack_version < run_waits.resume_request_version
    RETURNING run_waits.id, run_waits.org_id, run_waits.run_id
), snapshot AS (
INSERT INTO run_state_snapshots
    (org_id, run_id, version, status, execution_status, terminal_outcome,
     attempt_number, run_lease_id, worker_instance_id, worker_epoch,
     runtime_instance_id, previous_version, transition, reason)
SELECT transitioned.org_id, transitioned.id, transitioned.state_version,
       transitioned.status, transitioned.execution_status,
       transitioned.terminal_outcome, transitioned.current_attempt_number,
       transitioned.started_run_lease_id, transitioned.started_worker_instance_id,
       transitioned.started_worker_epoch, transitioned.started_runtime_instance_id,
       transitioned.state_version - 1,
       'run.lease_started', '{}'::jsonb
  FROM transitioned
  LEFT JOIN aligned_resume_wait
         ON aligned_resume_wait.org_id = transitioned.org_id
        AND aligned_resume_wait.run_id = transitioned.id
  RETURNING run_id
)
SELECT started.* FROM started
  JOIN snapshot ON snapshot.run_id = started.run_id
  JOIN renewed_workspace ON true;

-- name: GetStartedRunLease :one
SELECT run_leases.*
  FROM run_leases
  JOIN runs ON runs.org_id = run_leases.org_id
           AND runs.id = run_leases.run_id
           AND runs.current_run_lease_id = run_leases.id
 WHERE run_leases.org_id = sqlc.arg(org_id)
   AND run_leases.run_id = sqlc.arg(run_id)
   AND run_leases.id = sqlc.arg(run_lease_id)
   AND run_leases.lease_sequence = sqlc.arg(lease_sequence)
   AND run_leases.task_attempt_number = sqlc.arg(task_attempt_number)
   AND run_leases.worker_group_id = sqlc.arg(worker_group_id)
   AND run_leases.worker_instance_id = sqlc.arg(worker_instance_id)
   AND run_leases.worker_epoch = sqlc.arg(worker_epoch)
   AND run_leases.runtime_instance_id = sqlc.arg(runtime_instance_id)
   AND run_leases.network_slot_id = sqlc.arg(network_slot_id)
   AND run_leases.network_slot_generation = sqlc.arg(network_slot_generation)
   AND run_leases.worker_protocol_version = sqlc.arg(worker_protocol_version)
   AND run_leases.state = 'running'
   AND runs.state_version = sqlc.arg(expected_run_state_version)::bigint + 1;

-- name: RenewRunLease :one
WITH renewed AS (
UPDATE run_leases
   SET renewed_at = now(), expires_at = sqlc.arg(expires_at), updated_at = now()
  FROM worker_network_slots, workspace_leases
 WHERE run_leases.org_id = sqlc.arg(org_id) AND run_leases.run_id = sqlc.arg(run_id)
   AND run_leases.id = sqlc.arg(run_lease_id) AND run_leases.lease_sequence = sqlc.arg(lease_sequence)
   AND run_leases.worker_instance_id = sqlc.arg(worker_instance_id) AND run_leases.worker_epoch = sqlc.arg(worker_epoch)
   AND run_leases.runtime_instance_id = sqlc.arg(runtime_instance_id)
   AND run_leases.network_slot_id = sqlc.arg(network_slot_id)
   AND run_leases.network_slot_generation = sqlc.arg(network_slot_generation)
   AND worker_network_slots.id = run_leases.network_slot_id
   AND worker_network_slots.worker_instance_id = run_leases.worker_instance_id
   AND worker_network_slots.worker_epoch = run_leases.worker_epoch
   AND worker_network_slots.generation = run_leases.network_slot_generation
   AND worker_network_slots.runtime_instance_id = run_leases.runtime_instance_id
   AND worker_network_slots.state = 'bound'
   AND workspace_leases.org_id = run_leases.org_id
   AND workspace_leases.owner_run_id = run_leases.run_id
   AND workspace_leases.lease_kind = 'write'
   AND workspace_leases.state = 'active' AND workspace_leases.expires_at > now()
   AND run_leases.state IN ('running', 'checkpointing') AND run_leases.expires_at > now()
RETURNING run_leases.*
), renewed_workspace AS (
UPDATE workspace_leases
   SET renewed_at = now(), expires_at = sqlc.arg(expires_at), updated_at = now()
  FROM renewed
 WHERE workspace_leases.org_id = renewed.org_id
   AND workspace_leases.owner_run_id = renewed.run_id
   AND workspace_leases.lease_kind = 'write' AND workspace_leases.state = 'active'
RETURNING workspace_leases.id
)
SELECT renewed.* FROM renewed JOIN renewed_workspace ON true;

-- name: GetStartingRunLease :one
SELECT * FROM run_leases
 WHERE org_id = sqlc.arg(org_id) AND run_id = sqlc.arg(run_id)
   AND id = sqlc.arg(run_lease_id) AND state IN ('assigned', 'starting')
   AND worker_group_id = sqlc.arg(worker_group_id)
   AND worker_instance_id = sqlc.arg(worker_instance_id)
   AND worker_epoch = sqlc.arg(worker_epoch)
   AND lease_sequence = sqlc.arg(lease_sequence)
   AND runtime_instance_id = sqlc.arg(runtime_instance_id)
   AND network_slot_id = sqlc.arg(network_slot_id)
   AND network_slot_generation = sqlc.arg(network_slot_generation)
   AND worker_protocol_version = sqlc.arg(worker_protocol_version)
   AND expires_at > now()
 FOR UPDATE;

-- name: GetCurrentRunningRunLease :one
SELECT run_leases.* FROM run_leases
 JOIN runs ON runs.org_id = run_leases.org_id AND runs.id = run_leases.run_id
          AND runs.current_run_lease_id = run_leases.id
 WHERE run_leases.org_id = sqlc.arg(org_id) AND run_leases.run_id = sqlc.arg(run_id)
   AND run_leases.id = sqlc.arg(run_lease_id)
   AND run_leases.worker_group_id = sqlc.arg(worker_group_id)
   AND run_leases.worker_instance_id = sqlc.arg(worker_instance_id)
   AND run_leases.worker_epoch = sqlc.arg(worker_epoch)
   AND run_leases.lease_sequence = sqlc.arg(lease_sequence)
   AND run_leases.runtime_instance_id = sqlc.arg(runtime_instance_id)
   AND run_leases.network_slot_id = sqlc.arg(network_slot_id)
   AND run_leases.network_slot_generation = sqlc.arg(network_slot_generation)
   AND run_leases.worker_protocol_version = sqlc.arg(worker_protocol_version)
   AND run_leases.state IN ('running', 'checkpointing') AND run_leases.expires_at > now();

-- name: GetRunLeaseRuntimeIdentity :one
SELECT runtime_identities.* FROM runtime_identities
 JOIN run_leases ON run_leases.runtime_identity_id = runtime_identities.id
 WHERE run_leases.org_id = sqlc.arg(org_id) AND run_leases.run_id = sqlc.arg(run_id)
   AND run_leases.id = sqlc.arg(run_lease_id);

-- name: ReleaseRunLease :one
WITH run_before AS (
    SELECT runs.id, runs.active_elapsed_ms
      FROM runs
     WHERE runs.org_id = sqlc.arg(org_id) AND runs.id = sqlc.arg(run_id)
       AND runs.current_run_lease_id = sqlc.arg(run_lease_id)
     FOR UPDATE
), workspace_source AS (
    SELECT workspace_leases.*
      FROM workspace_leases
     WHERE workspace_leases.org_id = sqlc.arg(org_id)
       AND workspace_leases.owner_run_id = sqlc.arg(run_id)
       AND workspace_leases.lease_kind = 'write'
       AND workspace_leases.state = 'active'
       AND workspace_leases.expires_at > now()
       AND (
           (sqlc.arg(run_status)::run_status = 'succeeded'
            AND workspace_leases.id = sqlc.narg(workspace_lease_id)
            AND workspace_leases.fencing_token = sqlc.narg(workspace_fencing_token)
            AND workspace_leases.acquired_fencing_generation = sqlc.narg(workspace_fencing_generation))
           OR sqlc.arg(run_status)::run_status <> 'succeeded'
       )
     FOR UPDATE
), released AS (
    UPDATE run_leases
       SET state = sqlc.arg(state)::run_lease_state, terminal_at = now(),
           terminal_reason_code = sqlc.arg(reason_code), terminal_error = sqlc.narg(error),
           terminal_request_fingerprint = NULLIF(sqlc.arg(terminal_request_fingerprint)::text, ''),
           updated_at = now()
      FROM worker_network_slots, run_before
     WHERE run_leases.org_id = sqlc.arg(org_id) AND run_leases.run_id = sqlc.arg(run_id)
       AND run_leases.id = sqlc.arg(run_lease_id) AND run_leases.lease_sequence = sqlc.arg(lease_sequence)
       AND run_leases.worker_group_id = sqlc.arg(worker_group_id)
       AND run_leases.worker_instance_id = sqlc.arg(worker_instance_id) AND run_leases.worker_epoch = sqlc.arg(worker_epoch)
       AND run_leases.worker_protocol_version = sqlc.arg(worker_protocol_version)
       AND run_leases.runtime_instance_id = sqlc.arg(runtime_instance_id)
       AND run_leases.network_slot_id = sqlc.arg(network_slot_id)
       AND run_leases.network_slot_generation = sqlc.arg(network_slot_generation)
       AND worker_network_slots.id = run_leases.network_slot_id
       AND worker_network_slots.worker_instance_id = run_leases.worker_instance_id
       AND worker_network_slots.worker_epoch = run_leases.worker_epoch
       AND worker_network_slots.generation = run_leases.network_slot_generation
       AND worker_network_slots.runtime_instance_id = run_leases.runtime_instance_id
       AND worker_network_slots.state IN ('bound', 'reclaiming')
       AND run_leases.state IN ('running', 'checkpointing')
       AND (sqlc.arg(run_status)::run_status <> 'succeeded'
            OR sqlc.narg(workspace_lease_id) IS NULL
            OR EXISTS (SELECT 1 FROM workspace_source))
    RETURNING run_leases.*
), workspace_cas AS (
    INSERT INTO cas_objects (org_id, digest, size_bytes, media_type)
    SELECT released.org_id, sqlc.narg(workspace_artifact_digest),
           sqlc.narg(workspace_artifact_size_bytes), sqlc.narg(workspace_artifact_media_type)
      FROM released, workspace_source
     WHERE sqlc.arg(run_status)::run_status = 'succeeded'
    ON CONFLICT (org_id, digest) DO UPDATE
       SET size_bytes = EXCLUDED.size_bytes, media_type = EXCLUDED.media_type
     WHERE cas_objects.size_bytes = EXCLUDED.size_bytes
       AND cas_objects.media_type = EXCLUDED.media_type
    RETURNING digest
), workspace_artifact AS (
    INSERT INTO artifacts (
        id, org_id, project_id, environment_id, digest, kind,
        size_bytes, media_type, created_by_worker_instance_id
    )
    SELECT uuidv7(), released.org_id, released.project_id, released.environment_id,
           workspace_cas.digest, 'workspace_version', sqlc.narg(workspace_artifact_size_bytes),
           sqlc.narg(workspace_artifact_media_type), released.worker_instance_id
      FROM released, workspace_cas
    RETURNING *
), workspace_version AS (
    INSERT INTO workspace_versions (
        id, public_id, org_id, project_id, environment_id, workspace_id,
        parent_version_id, source_workspace_mount_id, source_write_lease_id,
        produced_by_run_id, kind, state, artifact_id, artifact_encoding,
        artifact_entry_count, content_digest, size_bytes, message, promoted_at
    )
    SELECT uuidv7(), sqlc.narg(workspace_version_public_id), workspace_source.org_id,
           workspace_source.project_id, workspace_source.environment_id,
           workspace_source.workspace_id, sqlc.narg(workspace_base_version_id),
           workspace_source.workspace_mount_id, workspace_source.id,
           workspace_source.owner_run_id, 'user', 'ready', workspace_artifact.id,
           sqlc.narg(workspace_artifact_encoding), sqlc.narg(workspace_artifact_entry_count),
           workspace_artifact.digest, workspace_artifact.size_bytes, 'run completion', now()
      FROM workspace_source, workspace_artifact
    RETURNING *
), committed_workspace AS (
    UPDATE workspaces
       SET current_version_id = workspace_version.id, dirty_state = 'clean', updated_at = now()
      FROM workspace_version
     WHERE workspaces.org_id = workspace_version.org_id
       AND workspaces.id = workspace_version.workspace_id
    RETURNING workspaces.id
), released_workspace_lease AS (
    UPDATE workspace_leases
       SET acquired_version_id = workspace_version.id, state = 'released',
           released_at = now(), terminal_at = now(), terminal_reason_code = 'run_terminal',
           updated_at = now()
      FROM workspace_version, committed_workspace
     WHERE workspace_leases.org_id = workspace_version.org_id
       AND workspace_leases.id = workspace_version.source_write_lease_id
    RETURNING workspace_leases.id
), released_terminal_workspace_lease AS (
    UPDATE workspace_leases
       SET state = 'released', released_at = now(), terminal_at = now(),
           terminal_reason_code = 'run_terminal_without_commit', updated_at = now()
      FROM workspace_source, released
     WHERE sqlc.arg(run_status)::run_status <> 'succeeded'
       AND workspace_leases.org_id = workspace_source.org_id
       AND workspace_leases.id = workspace_source.id
    RETURNING workspace_leases.id
), requested_mount_stop AS (
    UPDATE workspace_mounts
       SET state = 'unmounting', stopped_at = COALESCE(stopped_at, now()), updated_at = now()
      FROM released
     WHERE workspace_mounts.org_id = released.org_id
       AND workspace_mounts.workspace_id = released.workspace_id
       AND workspace_mounts.runtime_instance_id = released.runtime_instance_id
       AND workspace_mounts.state IN ('mounting','mounted')
    RETURNING workspace_mounts.id
), requested_runtime_close AS (
    UPDATE runtime_instances
       SET desired_state = 'closed', desired_version = desired_version + 1,
           desired_at = now(), desired_reason = 'run_terminal', updated_at = now()
      FROM released
     WHERE runtime_instances.org_id = released.org_id
       AND runtime_instances.id = released.runtime_instance_id
       AND runtime_instances.worker_group_id = released.worker_group_id
       AND runtime_instances.worker_instance_id = released.worker_instance_id
       AND runtime_instances.worker_epoch = released.worker_epoch
       AND runtime_instances.desired_state <> 'closed'
       AND runtime_instances.observed_state IN ('allocated','preparing','ready')
    RETURNING runtime_instances.id
), workspace_guard AS (
    SELECT CASE WHEN sqlc.arg(run_status)::run_status = 'succeeded'
                     AND sqlc.narg(workspace_lease_id) IS NOT NULL
                THEN (SELECT count(*) FROM released_workspace_lease)
                ELSE 1::bigint END AS committed
), transitioned AS (
UPDATE runs
   SET status = sqlc.arg(run_status)::run_status,
       execution_status = 'finished', terminal_outcome = sqlc.narg(terminal_outcome)::run_terminal_outcome,
       current_run_lease_id = NULL, state_version = state_version + 1,
       output = sqlc.narg(output), exit_code = sqlc.narg(exit_code),
       error_message = sqlc.narg(error_message), finished_at = now(), updated_at = now(),
       active_elapsed_ms = GREATEST(active_elapsed_ms, sqlc.arg(active_duration_ms)::bigint),
       active_started_at = NULL
  FROM released, workspace_guard
 WHERE runs.org_id = released.org_id AND runs.id = released.run_id
   AND runs.current_run_lease_id = released.id
   AND runs.state_version = sqlc.arg(expected_run_state_version)
   AND workspace_guard.committed = 1
RETURNING runs.*, released.id AS released_run_lease_id,
          released.worker_instance_id AS released_worker_instance_id,
          released.worker_epoch AS released_worker_epoch,
          released.runtime_instance_id AS released_runtime_instance_id
), meter_event AS (
    INSERT INTO meter_events (
        org_id, project_id, environment_id, run_id, run_lease_id, attempt_number,
        trace_id, span_id, meter, quantity, unit, measured_from, measured_to,
        details, idempotency_key, idempotency_fingerprint
    )
    SELECT transitioned.org_id, transitioned.project_id, transitioned.environment_id,
           transitioned.id, released.id, released.task_attempt_number,
           released.trace_id, released.span_id, 'active_time',
           GREATEST(sqlc.arg(active_duration_ms)::bigint - run_before.active_elapsed_ms, 0),
           'milliseconds', released.started_at, now(),
           jsonb_build_object(
               'terminal_status', transitioned.status,
               'cpu_millis',released.requested_cpu_millis,
               'memory_bytes',released.requested_memory_bytes,
               'workload_disk_bytes',released.requested_workload_disk_bytes,
               'scratch_bytes',released.requested_scratch_bytes,
               'execution_slots',released.requested_execution_slots
           ),
           'terminal:' || released.id::text,
           jsonb_build_object(
               'quantity', GREATEST(sqlc.arg(active_duration_ms)::bigint - run_before.active_elapsed_ms, 0),
               'unit', 'milliseconds', 'measured_from',released.started_at,'measured_to',now(),
               'terminal_status', transitioned.status,
               'cpu_millis',released.requested_cpu_millis,
               'memory_bytes',released.requested_memory_bytes,
               'workload_disk_bytes',released.requested_workload_disk_bytes,
               'scratch_bytes',released.requested_scratch_bytes,
               'execution_slots',released.requested_execution_slots
           )::text
      FROM transitioned, released, run_before
     WHERE sqlc.arg(active_duration_ms)::bigint > run_before.active_elapsed_ms
       AND released.started_at < now()
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
    SELECT meter_event.org_id, 'meter_event', meter_event.source_type, meter_event.source_id,
           meter_event.project_id, meter_event.environment_id, meter_event.run_id,
           meter_event.run_lease_id, meter_event.id, meter_event.attempt_number,
           meter_event.trace_id, meter_event.span_id, meter_event.meter,
           meter_event.details, meter_event.idempotency_key, meter_event.occurred_at
      FROM meter_event
    ON CONFLICT DO NOTHING
    RETURNING meter_event_id
), snapshot AS (
INSERT INTO run_state_snapshots
    (org_id, run_id, version, status, execution_status, terminal_outcome,
     attempt_number, run_lease_id, worker_instance_id, worker_epoch,
     runtime_instance_id, previous_version, transition, reason, error)
SELECT transitioned.org_id, transitioned.id, transitioned.state_version,
       transitioned.status, transitioned.execution_status, transitioned.terminal_outcome,
       transitioned.current_attempt_number, transitioned.released_run_lease_id,
       transitioned.released_worker_instance_id,
       released_worker_epoch, released_runtime_instance_id, state_version - 1,
       'run.lease_released', jsonb_build_object('reason_code', sqlc.arg(reason_code)::text),
       COALESCE(sqlc.narg(error)::jsonb, '{}'::jsonb)
  FROM transitioned, run_before
 WHERE sqlc.arg(active_duration_ms)::bigint <= run_before.active_elapsed_ms OR EXISTS (SELECT 1 FROM meter_outbox)
 RETURNING run_id
)
SELECT transitioned.* FROM transitioned JOIN snapshot ON snapshot.run_id = transitioned.id;

-- name: GetRunLeaseTerminalResult :one
SELECT (CASE run_leases.state
            WHEN 'completed' THEN 'succeeded'
            WHEN 'failed' THEN 'failed'
            WHEN 'cancelled' THEN 'cancelled'
        END)::run_status AS run_status,
       run_leases.terminal_request_fingerprint
  FROM run_leases
 WHERE run_leases.org_id = sqlc.arg(org_id)
   AND run_leases.run_id = sqlc.arg(run_id)
   AND run_leases.id = sqlc.arg(run_lease_id)
   AND run_leases.lease_sequence = sqlc.arg(lease_sequence)
   AND run_leases.task_attempt_number = sqlc.arg(task_attempt_number)
   AND run_leases.worker_group_id = sqlc.arg(worker_group_id)
   AND run_leases.worker_instance_id = sqlc.arg(worker_instance_id)
   AND run_leases.worker_epoch = sqlc.arg(worker_epoch)
   AND run_leases.runtime_instance_id = sqlc.arg(runtime_instance_id)
   AND run_leases.network_slot_id = sqlc.arg(network_slot_id)
   AND run_leases.network_slot_generation = sqlc.arg(network_slot_generation)
   AND run_leases.worker_protocol_version = sqlc.arg(worker_protocol_version)
   AND run_leases.state IN ('completed', 'failed', 'cancelled');

-- name: GetClaimedRunRestorePayload :one
SELECT run_checkpoints.id AS run_checkpoint_id,
       run_checkpoints.manifest,
       run_waits.id AS run_wait_id,
       run_waits.resume_request_version,
       waits.kind AS run_wait_kind,
       waits.state AS wait_state,
       waits.result AS wait_result,
       streams.name AS stream_name,
       stream_records.sequence AS stream_record_sequence,
       stream_records.data AS stream_record_data,
       tokens.state AS token_state,
       tokens.completion_data AS token_completion_data
  FROM run_leases
  JOIN runs ON runs.org_id = run_leases.org_id
           AND runs.id = run_leases.run_id
           AND runs.current_run_lease_id = run_leases.id
  JOIN run_waits ON run_waits.org_id = run_leases.org_id
                AND run_waits.run_id = run_leases.run_id
                AND run_waits.current_run_lease_id = run_leases.id
                AND run_waits.run_checkpoint_id = sqlc.arg(run_checkpoint_id)
                AND run_waits.resume_request_version = sqlc.arg(resume_request_version)
                AND run_waits.state = 'resuming'
  JOIN run_checkpoints ON run_checkpoints.org_id = run_waits.org_id
                      AND run_checkpoints.run_id = run_waits.run_id
                      AND run_checkpoints.id = run_waits.run_checkpoint_id
                      AND run_checkpoints.run_wait_id = run_waits.id
                      AND run_checkpoints.state = 'ready'
                      AND (run_checkpoints.expires_at IS NULL OR run_checkpoints.expires_at > now())
  JOIN waits ON waits.org_id = run_waits.org_id
            AND waits.project_id = run_waits.project_id
            AND waits.environment_id = run_waits.environment_id
            AND waits.id = run_waits.wait_id
            AND waits.state IN ('completed', 'expired', 'cancelled')
  LEFT JOIN streams ON streams.org_id = waits.org_id
                   AND streams.project_id = waits.project_id
                   AND streams.environment_id = waits.environment_id
                   AND streams.id = waits.stream_id
  LEFT JOIN stream_records ON stream_records.org_id = waits.org_id
                          AND stream_records.stream_id = waits.stream_id
                          AND stream_records.id = waits.stream_record_id
  LEFT JOIN tokens ON tokens.org_id = waits.org_id
                  AND tokens.project_id = waits.project_id
                  AND tokens.environment_id = waits.environment_id
                  AND tokens.id = waits.token_id
 WHERE run_leases.org_id = sqlc.arg(org_id)
   AND run_leases.run_id = sqlc.arg(run_id)
   AND run_leases.id = sqlc.arg(run_lease_id)
   AND run_leases.worker_instance_id = sqlc.arg(worker_instance_id)
   AND run_leases.worker_epoch = sqlc.arg(worker_epoch)
   AND run_leases.state IN ('starting', 'running')
   AND run_leases.expires_at > now()
 LIMIT 1;
