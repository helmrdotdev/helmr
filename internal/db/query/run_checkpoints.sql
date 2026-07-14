-- name: GetRunRestorePayload :one
SELECT run_checkpoints.*,
       run_waits.reserved_workspace_id,
       run_waits.reserved_workspace_version_id,
       run_waits.resume_request_version,
       run_waits.resume_ack_version
  FROM run_checkpoints
  JOIN run_waits
    ON run_waits.org_id = run_checkpoints.org_id
   AND run_waits.run_id = run_checkpoints.run_id
   AND run_waits.id = run_checkpoints.run_wait_id
 WHERE run_checkpoints.org_id = sqlc.arg(org_id)
   AND run_checkpoints.run_id = sqlc.arg(run_id)
   AND run_checkpoints.id = sqlc.arg(run_checkpoint_id)
   AND run_checkpoints.state = 'ready'
   AND run_waits.state IN ('checkpointed_waiting', 'resuming')
   AND run_waits.resume_request_version = sqlc.arg(resume_request_version)
 FOR UPDATE OF run_waits;

-- name: GetAcknowledgedReadyRunCheckpointForRunWait :one
SELECT run_checkpoints.*
  FROM run_checkpoints
  JOIN run_waits
    ON run_waits.org_id = run_checkpoints.org_id
   AND run_waits.run_id = run_checkpoints.run_id
   AND run_waits.id = run_checkpoints.run_wait_id
 WHERE run_checkpoints.org_id = sqlc.arg(org_id)
   AND run_checkpoints.run_wait_id = sqlc.arg(run_wait_id)
   AND run_checkpoints.state = 'ready'
   AND run_waits.checkpoint_ack_version = run_waits.checkpoint_request_version
 ORDER BY run_checkpoints.created_at DESC, run_checkpoints.id DESC
 LIMIT 1;

-- name: CreateReadyRunCheckpointForRunWait :one
WITH source AS (
    SELECT run_waits.*, run_leases.workspace_id, run_leases.worker_instance_id,
           run_leases.worker_epoch, run_leases.runtime_instance_id
      FROM run_waits
      JOIN run_leases ON run_leases.org_id = run_waits.org_id
                     AND run_leases.run_id = run_waits.run_id
                     AND run_leases.id = run_waits.current_run_lease_id
      JOIN workspace_leases ON workspace_leases.org_id = run_waits.org_id
                           AND workspace_leases.workspace_id = run_leases.workspace_id
                           AND workspace_leases.id = sqlc.arg(source_workspace_lease_id)
                           AND workspace_leases.workspace_mount_id = sqlc.arg(workspace_mount_id)
                           AND workspace_leases.owner_run_id = run_waits.run_id
                           AND workspace_leases.acquired_version_id = sqlc.arg(base_workspace_version_id)
                           AND workspace_leases.worker_instance_id = run_leases.worker_instance_id
                           AND workspace_leases.worker_epoch = run_leases.worker_epoch
                           AND workspace_leases.runtime_instance_id = run_leases.runtime_instance_id
                           AND workspace_leases.lease_kind = 'write'
                           AND workspace_leases.state = 'active'
                           AND workspace_leases.expires_at > now()
     WHERE run_waits.org_id = sqlc.arg(org_id)
       AND run_waits.run_id = sqlc.arg(run_id)
       AND run_waits.id = sqlc.arg(run_wait_id)
       AND run_waits.state = 'checkpointing'
       AND run_waits.checkpoint_request_version = sqlc.arg(checkpoint_request_version)
       AND run_waits.checkpoint_attempt_id = sqlc.arg(id)
     FOR UPDATE OF run_waits, workspace_leases
)
INSERT INTO run_checkpoints (
    id, org_id, project_id, environment_id, workspace_id, run_id, run_wait_id,
    source_run_lease_id, source_runtime_instance_id, source_worker_instance_id,
    source_worker_epoch, source_workspace_lease_id, workspace_mount_id,
    base_workspace_version_id, state, runtime_backend, runtime_identity_id,
    runtime_arch, runtime_abi, kernel_digest, initramfs_digest, rootfs_digest,
    runtime_config_digest, substrate_digest, runtime_substrate_id, runtime_vcpus,
    runtime_memory_mib, runtime_scratch_disk_mib, cni_profile, image_key,
    manifest, expires_at, creation_started_at, creation_expires_at, ready_at
)
SELECT sqlc.arg(id), source.org_id, source.project_id, source.environment_id,
       source.workspace_id, source.run_id, source.id, source.current_run_lease_id,
       source.runtime_instance_id, source.worker_instance_id, source.worker_epoch,
       sqlc.arg(source_workspace_lease_id), sqlc.arg(workspace_mount_id),
       sqlc.arg(base_workspace_version_id), 'creating', sqlc.arg(runtime_backend),
       sqlc.arg(runtime_identity_id), sqlc.arg(runtime_arch), sqlc.arg(runtime_abi),
       sqlc.arg(kernel_digest), sqlc.arg(initramfs_digest), sqlc.arg(rootfs_digest),
       sqlc.arg(runtime_config_digest), sqlc.narg(substrate_digest),
       sqlc.narg(runtime_substrate_id), sqlc.narg(runtime_vcpus),
       sqlc.narg(runtime_memory_mib), sqlc.narg(runtime_scratch_disk_mib),
       sqlc.arg(cni_profile), sqlc.narg(image_key), sqlc.arg(manifest),
       sqlc.narg(expires_at), now(), COALESCE(sqlc.narg(expires_at), now() + interval '5 minutes'), NULL
  FROM source
RETURNING *;

-- name: CreateRunCheckpointArtifact :one
INSERT INTO run_checkpoint_artifacts (
    org_id, project_id, environment_id, run_id, run_checkpoint_id, role, ordinal,
    artifact_id, size_bytes, media_type, digest, encrypt_duration_ms, store_duration_ms
)
SELECT run_checkpoints.org_id, run_checkpoints.project_id, run_checkpoints.environment_id,
       run_checkpoints.run_id, run_checkpoints.id, sqlc.arg(role), sqlc.arg(ordinal),
       sqlc.arg(artifact_id), sqlc.arg(size_bytes), sqlc.arg(media_type),
       sqlc.arg(digest), sqlc.arg(encrypt_duration_ms), sqlc.arg(store_duration_ms)
  FROM run_checkpoints
 WHERE run_checkpoints.org_id = sqlc.arg(org_id)
   AND run_checkpoints.run_id = sqlc.arg(run_id)
   AND run_checkpoints.id = sqlc.arg(run_checkpoint_id)
   AND run_checkpoints.state = 'creating'
RETURNING *;

-- name: FailRunCheckpointAttempt :one
WITH target AS (
    SELECT run_waits.*, run_leases.worker_instance_id, run_leases.worker_epoch,
           run_leases.runtime_instance_id, runs.active_elapsed_ms AS prior_active_elapsed_ms
      FROM run_waits
      JOIN run_leases ON run_leases.org_id = run_waits.org_id
                     AND run_leases.run_id = run_waits.run_id
                     AND run_leases.id = run_waits.current_run_lease_id
      JOIN runs ON runs.org_id = run_waits.org_id AND runs.id = run_waits.run_id
               AND runs.current_run_lease_id = run_leases.id
     WHERE run_waits.org_id = sqlc.arg(org_id)
       AND run_waits.run_id = sqlc.arg(run_id)
       AND run_waits.id = sqlc.arg(run_wait_id)
       AND run_waits.current_run_lease_id = sqlc.arg(run_lease_id)
       AND run_waits.checkpoint_request_version = sqlc.arg(checkpoint_request_version)
       AND run_waits.checkpoint_attempt_id = sqlc.arg(run_checkpoint_id)
       AND run_waits.state = 'checkpointing'
       AND run_leases.state = 'checkpointing'
       AND run_leases.worker_instance_id = sqlc.arg(worker_instance_id)
       AND run_leases.worker_epoch = sqlc.arg(worker_epoch)
     FOR UPDATE OF run_waits, run_leases
), invalidated AS (
    UPDATE run_checkpoints
       SET state = 'invalid', error = sqlc.arg(error), invalidated_at = now()
      FROM target
     WHERE run_checkpoints.org_id = target.org_id
       AND run_checkpoints.run_id = target.run_id
       AND run_checkpoints.id = sqlc.arg(run_checkpoint_id)
       AND run_checkpoints.run_wait_id = target.id
       AND run_checkpoints.state = 'creating'
    RETURNING run_checkpoints.id
), failed_lease AS (
    UPDATE run_leases
       SET state = 'failed', terminal_at = now(), terminal_reason_code = sqlc.arg(reason_code),
           terminal_error = sqlc.arg(error), updated_at = now()
      FROM target
     WHERE run_leases.org_id = target.org_id AND run_leases.run_id = target.run_id
       AND run_leases.id = target.current_run_lease_id
    RETURNING run_leases.*
), released_workspace_leases AS (
    UPDATE workspace_leases
       SET state = 'released', released_at = now(), terminal_at = now(),
           terminal_reason_code = 'checkpoint_failed', updated_at = now()
      FROM target
     WHERE workspace_leases.org_id = target.org_id
       AND workspace_leases.owner_run_id = target.run_id
       AND workspace_leases.state IN ('active','releasing')
    RETURNING workspace_leases.id
), requested_runtime_close AS (
    UPDATE runtime_instances
       SET desired_state = 'closed', desired_version = desired_version + 1,
           desired_at = now(), desired_reason = 'run_wait_checkpoint_failed', updated_at = now()
      FROM failed_lease
     WHERE runtime_instances.org_id = failed_lease.org_id
       AND runtime_instances.id = failed_lease.runtime_instance_id
       AND runtime_instances.worker_instance_id = failed_lease.worker_instance_id
       AND runtime_instances.worker_epoch = failed_lease.worker_epoch
       AND runtime_instances.desired_state <> 'closed'
       AND runtime_instances.observed_state IN ('allocated','preparing','ready')
    RETURNING runtime_instances.id
), cleanup AS (
    SELECT (SELECT count(*) FROM invalidated)
         + (SELECT count(*) FROM released_workspace_leases)
         + (SELECT count(*) FROM requested_runtime_close) AS affected
), failed_wait AS (
UPDATE run_waits
   SET state = 'failed', failed_at = now(), terminal_at = now(),
       terminal_reason_code = sqlc.arg(reason_code), terminal_error = sqlc.arg(error),
       updated_at = now()
  FROM target, failed_lease, cleanup
 WHERE run_waits.org_id = target.org_id AND run_waits.run_id = target.run_id
   AND run_waits.id = target.id
   AND cleanup.affected >= 0
RETURNING run_waits.*
), failed_run AS (
    UPDATE runs
       SET status = 'failed', execution_status = 'finished', terminal_outcome = 'failed',
           current_run_lease_id = NULL, error_message = sqlc.arg(error_message),
           state_version = state_version + 1,
           active_elapsed_ms = GREATEST(active_elapsed_ms, sqlc.arg(active_duration_ms)::bigint),
           active_started_at = NULL, finished_at = now(), updated_at = now()
      FROM target, failed_wait
     WHERE runs.org_id = target.org_id AND runs.id = target.run_id
       AND runs.current_run_lease_id = target.current_run_lease_id
       AND runs.state_version = target.expected_run_state_version
    RETURNING runs.*
), meter_event AS (
    INSERT INTO meter_events (
        org_id, project_id, environment_id, run_id, run_lease_id, attempt_number,
        trace_id, span_id, meter, quantity, unit, measured_from, measured_to,
        details, idempotency_key, idempotency_fingerprint
    )
    SELECT failed_run.org_id, failed_run.project_id, failed_run.environment_id,
           failed_run.id, failed_lease.id, failed_lease.task_attempt_number,
           failed_lease.trace_id, failed_lease.span_id, 'active_time',
           GREATEST(sqlc.arg(active_duration_ms)::bigint - target.prior_active_elapsed_ms, 0),
           'milliseconds', failed_lease.started_at, now(),
           jsonb_build_object('transition','checkpoint_failed',
               'cpu_millis',failed_lease.requested_cpu_millis,
               'memory_bytes',failed_lease.requested_memory_bytes,
               'workload_disk_bytes',failed_lease.requested_workload_disk_bytes,
               'scratch_bytes',failed_lease.requested_scratch_bytes,
               'execution_slots',failed_lease.requested_execution_slots),
           'checkpoint-failed:' || failed_lease.id::text,
           jsonb_build_object('quantity',GREATEST(sqlc.arg(active_duration_ms)::bigint - target.prior_active_elapsed_ms, 0),
               'unit','milliseconds','measured_from',failed_lease.started_at,'measured_to',now(),
               'transition','checkpoint_failed','cpu_millis',failed_lease.requested_cpu_millis,
               'memory_bytes',failed_lease.requested_memory_bytes,
               'workload_disk_bytes',failed_lease.requested_workload_disk_bytes,
               'scratch_bytes',failed_lease.requested_scratch_bytes,
               'execution_slots',failed_lease.requested_execution_slots)::text
      FROM failed_run, failed_lease, target
     WHERE sqlc.arg(active_duration_ms)::bigint > target.prior_active_elapsed_ms
       AND failed_lease.started_at < now()
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
         attempt_number, run_lease_id, worker_instance_id, worker_epoch,
         runtime_instance_id, previous_version, transition, reason, error)
    SELECT failed_run.org_id, failed_run.id, failed_run.state_version,
           failed_run.status, failed_run.execution_status, failed_run.terminal_outcome,
           failed_run.current_attempt_number, failed_lease.id, failed_lease.worker_instance_id,
           failed_lease.worker_epoch, failed_lease.runtime_instance_id,
           failed_run.state_version - 1, 'run.wait_checkpoint_failed',
           jsonb_build_object('run_wait_id', failed_wait.id), sqlc.arg(error)
      FROM failed_run, failed_wait, failed_lease
     WHERE NOT EXISTS (SELECT 1 FROM meter_event) OR EXISTS (SELECT 1 FROM meter_outbox)
    RETURNING run_id
)
SELECT failed_wait.* FROM failed_wait JOIN snapshot ON snapshot.run_id = failed_wait.run_id;
