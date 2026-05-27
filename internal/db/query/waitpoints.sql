-- name: CreateWaitpointForExecution :one
WITH current_execution AS (
    SELECT runs.id AS run_id,
           run_executions.dispatch_message_id,
           run_executions.dispatch_lease_id,
           run_executions.lease_expires_at
      FROM runs
      JOIN run_executions ON run_executions.id = runs.current_execution_id
                          AND run_executions.org_id = runs.org_id
                          AND run_executions.run_id = runs.id
     WHERE runs.org_id = sqlc.arg(org_id)
       AND runs.id = sqlc.arg(run_id)
       AND runs.status = 'running'
       AND run_executions.id = sqlc.arg(execution_id)
       AND run_executions.worker_instance_id = sqlc.arg(worker_instance_id)
       AND run_executions.status = 'running'
       AND run_executions.lease_expires_at > now()
     FOR UPDATE OF runs, run_executions
),
existing_waitpoint AS (
    SELECT waitpoints.*
      FROM waitpoints
      JOIN current_execution ON current_execution.run_id = waitpoints.run_id
     WHERE waitpoints.org_id = sqlc.arg(org_id)
       AND waitpoints.correlation_id = sqlc.arg(correlation_id)
       AND waitpoints.status = 'opening'
),
checkpoint AS (
    INSERT INTO checkpoints (
        id,
        org_id,
        run_id,
        execution_id,
        reason
    )
    SELECT
        sqlc.arg(checkpoint_id),
        sqlc.arg(org_id),
        current_execution.run_id,
        sqlc.arg(execution_id),
        sqlc.arg(checkpoint_reason)
      FROM current_execution
     WHERE NOT EXISTS (SELECT 1 FROM existing_waitpoint)
    ON CONFLICT (id) DO UPDATE SET
        id = EXCLUDED.id
     WHERE checkpoints.status = 'creating'
    RETURNING *
),
waitpoint AS (
    INSERT INTO waitpoints (
        id,
        org_id,
        run_id,
        execution_id,
        checkpoint_id,
        correlation_id,
        kind,
        request,
        display_text,
        timeout_seconds,
        policy_name,
        policy_snapshot
    )
    SELECT
        sqlc.arg(id),
        sqlc.arg(org_id),
        current_execution.run_id,
        sqlc.arg(execution_id),
        checkpoint.id,
        sqlc.arg(correlation_id),
        sqlc.arg(kind),
        sqlc.arg(request),
        sqlc.arg(display_text),
        sqlc.narg(timeout_seconds),
        sqlc.narg(policy_name),
        sqlc.narg(policy_snapshot)
      FROM current_execution
      JOIN checkpoint ON checkpoint.run_id = current_execution.run_id
    ON CONFLICT (run_id, correlation_id) WHERE status IN ('opening', 'waiting', 'resuming') DO UPDATE SET
        request = waitpoints.request,
        display_text = waitpoints.display_text,
        timeout_seconds = waitpoints.timeout_seconds,
        policy_name = waitpoints.policy_name,
        policy_snapshot = waitpoints.policy_snapshot,
        checkpoint_id = waitpoints.checkpoint_id
     WHERE waitpoints.status = 'opening'
    RETURNING *
),
selected_waitpoint AS (
    SELECT * FROM existing_waitpoint
    UNION ALL
    SELECT * FROM waitpoint
),
hot_availability AS (
    INSERT INTO checkpoint_availability_replicas (
        org_id,
        run_id,
        checkpoint_id,
        state,
        worker_instance_id,
        execution_id,
        dispatch_message_id,
        dispatch_lease_id,
        lease_expires_at,
        metadata
    )
    SELECT sqlc.arg(org_id),
           selected_waitpoint.run_id,
           selected_waitpoint.checkpoint_id,
           'hot',
           sqlc.arg(worker_instance_id),
           sqlc.arg(execution_id),
           current_execution.dispatch_message_id,
           current_execution.dispatch_lease_id,
           current_execution.lease_expires_at,
           jsonb_build_object('source', 'waitpoint_opening')
      FROM selected_waitpoint
      JOIN current_execution ON current_execution.run_id = selected_waitpoint.run_id
    ON CONFLICT (org_id, run_id, checkpoint_id, state, worker_instance_id, execution_id) DO UPDATE
       SET dispatch_message_id = EXCLUDED.dispatch_message_id,
           dispatch_lease_id = EXCLUDED.dispatch_lease_id,
           lease_expires_at = EXCLUDED.lease_expires_at,
           unavailable_at = NULL,
           metadata = EXCLUDED.metadata
    RETURNING id
),
checkpoint_started_event AS (
    INSERT INTO run_events (org_id, run_id, kind, payload)
    SELECT sqlc.arg(org_id),
           selected_waitpoint.run_id,
           'checkpoint.started',
           jsonb_build_object(
               'run_id', selected_waitpoint.run_id,
               'waitpoint_id', selected_waitpoint.id,
               'checkpoint_id', selected_waitpoint.checkpoint_id,
               'kind', selected_waitpoint.kind,
               'display_text', selected_waitpoint.display_text
           )
      FROM selected_waitpoint
      JOIN hot_availability ON true
     WHERE NOT EXISTS (SELECT 1 FROM existing_waitpoint)
    RETURNING id
),
checkpoint_started AS (
    SELECT count(*) AS event_count FROM checkpoint_started_event
)
SELECT selected_waitpoint.*
  FROM selected_waitpoint
  JOIN hot_availability ON true
  JOIN checkpoint_started ON true
 LIMIT 1;

-- name: MarkWaitpointCheckpointDurableReady :one
WITH current_execution AS (
    SELECT runs.id AS run_id,
           run_executions.dispatch_message_id,
           run_executions.dispatch_lease_id,
           run_executions.lease_expires_at
      FROM runs
      JOIN run_executions ON run_executions.id = runs.current_execution_id
                          AND run_executions.org_id = runs.org_id
                          AND run_executions.run_id = runs.id
     WHERE runs.org_id = sqlc.arg(org_id)
       AND runs.id = sqlc.arg(run_id)
       AND runs.status = 'running'
       AND run_executions.id = sqlc.arg(execution_id)
       AND run_executions.worker_instance_id = sqlc.arg(worker_instance_id)
       AND run_executions.status = 'running'
       AND run_executions.lease_expires_at > now()
     FOR UPDATE OF runs, run_executions
),
target_waitpoint AS (
    SELECT waitpoints.*
      FROM waitpoints
      JOIN current_execution ON current_execution.run_id = waitpoints.run_id
     WHERE waitpoints.org_id = sqlc.arg(org_id)
       AND waitpoints.id = sqlc.arg(waitpoint_id)
       AND waitpoints.checkpoint_id = sqlc.arg(checkpoint_id)
       AND waitpoints.execution_id = sqlc.arg(execution_id)
       AND waitpoints.status = 'opening'
     FOR UPDATE OF waitpoints
),
locked_queue_entry AS (
    SELECT run_queue_items.run_id,
           run_queue_items.reserved_by_worker_instance_id,
           run_queue_items.dispatch_message_id
      FROM run_queue_items
      JOIN current_execution ON current_execution.run_id = run_queue_items.run_id
                            AND current_execution.dispatch_message_id = run_queue_items.dispatch_message_id
     WHERE run_queue_items.org_id = sqlc.arg(org_id)
       AND run_queue_items.run_id = sqlc.arg(run_id)
       AND run_queue_items.reserved_by_worker_instance_id = sqlc.arg(worker_instance_id)
       AND run_queue_items.status = 'reserved'
       AND run_queue_items.reservation_expires_at > now()
     FOR UPDATE OF run_queue_items
),
cas_object_input AS (
    SELECT DISTINCT
           artifact.value->>'digest' AS digest,
           (artifact.value->>'size_bytes')::bigint AS size_bytes,
           artifact.value->>'media_type' AS media_type
      FROM jsonb_array_elements(sqlc.arg(checkpoint_artifacts)::jsonb) AS artifact(value)
),
published_cas_objects AS (
    INSERT INTO cas_objects (digest, size_bytes, media_type)
    SELECT digest, size_bytes, media_type
      FROM cas_object_input
      JOIN target_waitpoint ON true
      JOIN locked_queue_entry ON locked_queue_entry.run_id = target_waitpoint.run_id
    ON CONFLICT (digest) DO UPDATE
       SET size_bytes = cas_objects.size_bytes
     WHERE cas_objects.size_bytes = EXCLUDED.size_bytes
       AND cas_objects.media_type = EXCLUDED.media_type
    RETURNING digest
),
cas_objects_ready AS (
    SELECT count(*) = (SELECT count(*) FROM cas_object_input) AS ok
      FROM published_cas_objects
),
ready_checkpoint AS (
    UPDATE checkpoints
       SET status = 'ready',
           manifest = sqlc.arg(manifest),
           runtime_backend = sqlc.arg(runtime_backend),
           runtime_arch = sqlc.arg(runtime_arch),
           runtime_abi = sqlc.arg(runtime_abi),
           kernel_digest = sqlc.narg(kernel_digest),
           rootfs_digest = sqlc.narg(rootfs_digest),
           runtime_vcpus = sqlc.narg(runtime_vcpus),
           runtime_memory_mib = sqlc.narg(runtime_memory_mib),
           runtime_scratch_disk_mib = sqlc.narg(runtime_scratch_disk_mib),
           cni_profile = sqlc.narg(cni_profile),
           image_key = sqlc.narg(image_key),
           runtime_config_digest = sqlc.narg(runtime_config_digest),
           workspace_base_kind = sqlc.narg(workspace_base_kind),
           workspace_repository = sqlc.narg(workspace_repository),
           workspace_ref = sqlc.narg(workspace_ref),
           workspace_sha = sqlc.narg(workspace_sha),
           workspace_subpath = sqlc.narg(workspace_subpath),
           workspace_ref_kind = sqlc.narg(workspace_ref_kind),
           workspace_ref_name = sqlc.narg(workspace_ref_name),
           workspace_full_ref = sqlc.narg(workspace_full_ref),
           workspace_default_branch = sqlc.narg(workspace_default_branch),
           workspace_artifact_digest = sqlc.narg(workspace_artifact_digest),
           workspace_artifact_media_type = sqlc.narg(workspace_artifact_media_type),
           workspace_artifact_encoding = sqlc.narg(workspace_artifact_encoding),
           workspace_mount_path = sqlc.narg(workspace_mount_path),
           workspace_volume_kind = sqlc.narg(workspace_volume_kind),
           ready_at = now()
      FROM target_waitpoint
      JOIN cas_objects_ready ON cas_objects_ready.ok
      JOIN locked_queue_entry ON locked_queue_entry.run_id = target_waitpoint.run_id
     WHERE checkpoints.org_id = sqlc.arg(org_id)
       AND checkpoints.run_id = target_waitpoint.run_id
       AND checkpoints.id = target_waitpoint.checkpoint_id
       AND checkpoints.execution_id = sqlc.arg(execution_id)
       AND checkpoints.status = 'creating'
    RETURNING checkpoints.*
),
ready_requirements AS (
    UPDATE run_runtime_requirements
       SET requested_milli_cpu = COALESCE(ready_checkpoint.runtime_vcpus::bigint * 1000, run_runtime_requirements.requested_milli_cpu),
           requested_memory_mib = COALESCE(ready_checkpoint.runtime_memory_mib::bigint, run_runtime_requirements.requested_memory_mib),
           requested_disk_mib = COALESCE(ready_checkpoint.runtime_scratch_disk_mib::bigint, run_runtime_requirements.requested_disk_mib),
           runtime_arch = COALESCE(ready_checkpoint.runtime_arch, ''),
           runtime_abi = COALESCE(ready_checkpoint.runtime_abi, ''),
           kernel_digest = COALESCE(ready_checkpoint.kernel_digest, ''),
           rootfs_digest = COALESCE(ready_checkpoint.rootfs_digest, ''),
           cni_profile = COALESCE(ready_checkpoint.cni_profile, ''),
           updated_at = now()
      FROM ready_checkpoint
     WHERE run_runtime_requirements.org_id = ready_checkpoint.org_id
       AND run_runtime_requirements.run_id = ready_checkpoint.run_id
    RETURNING run_runtime_requirements.run_id
),
checkpoint_artifact_input AS (
    SELECT (artifact.value->>'role')::checkpoint_artifact_role AS role,
           (artifact.value->>'ordinal')::int AS ordinal,
           artifact.value->>'digest' AS digest,
           (artifact.value->>'size_bytes')::bigint AS size_bytes,
           artifact.value->>'media_type' AS media_type,
           COALESCE((artifact.value->>'encrypt_duration_ms')::bigint, 0) AS encrypt_duration_ms,
           COALESCE((artifact.value->>'store_duration_ms')::bigint, 0) AS store_duration_ms
      FROM jsonb_array_elements(sqlc.arg(checkpoint_artifacts)::jsonb) AS artifact(value)
),
inserted_checkpoint_artifacts AS (
    INSERT INTO checkpoint_artifacts (
        org_id,
        run_id,
        checkpoint_id,
        role,
        ordinal,
        digest,
        size_bytes,
        media_type,
        encrypt_duration_ms,
        store_duration_ms
    )
    SELECT sqlc.arg(org_id),
           ready_checkpoint.run_id,
           ready_checkpoint.id,
           checkpoint_artifact_input.role,
           checkpoint_artifact_input.ordinal,
           checkpoint_artifact_input.digest,
           checkpoint_artifact_input.size_bytes,
           checkpoint_artifact_input.media_type,
           checkpoint_artifact_input.encrypt_duration_ms,
           checkpoint_artifact_input.store_duration_ms
      FROM ready_checkpoint
      JOIN checkpoint_artifact_input ON true
    ON CONFLICT (org_id, run_id, checkpoint_id, role, ordinal) DO UPDATE
       SET digest = EXCLUDED.digest,
           size_bytes = EXCLUDED.size_bytes,
           media_type = EXCLUDED.media_type,
           encrypt_duration_ms = EXCLUDED.encrypt_duration_ms,
           store_duration_ms = EXCLUDED.store_duration_ms
    RETURNING digest
),
checkpoint_artifacts_ready AS (
    SELECT count(*) AS artifact_count FROM inserted_checkpoint_artifacts
),
suspended_queue_entry AS (
    UPDATE run_queue_items
       SET status = 'suspended',
           dispatch_generation = dispatch_generation + 1,
           updated_at = now(),
           finished_at = now()
      FROM ready_checkpoint
      JOIN locked_queue_entry ON locked_queue_entry.run_id = ready_checkpoint.run_id
     WHERE run_queue_items.org_id = sqlc.arg(org_id)
       AND run_queue_items.run_id = ready_checkpoint.run_id
       AND run_queue_items.reserved_by_worker_instance_id = locked_queue_entry.reserved_by_worker_instance_id
       AND run_queue_items.dispatch_message_id = locked_queue_entry.dispatch_message_id
       AND run_queue_items.status = 'reserved'
    RETURNING run_queue_items.run_id
),
durable_availability AS (
    INSERT INTO checkpoint_availability_replicas (
        org_id,
        run_id,
        checkpoint_id,
        state,
        worker_instance_id,
        execution_id,
        dispatch_message_id,
        dispatch_lease_id,
        lease_expires_at,
        metadata
    )
    SELECT ready_checkpoint.org_id,
           ready_checkpoint.run_id,
           ready_checkpoint.id,
           'durable',
           sqlc.arg(worker_instance_id),
           sqlc.arg(execution_id),
           current_execution.dispatch_message_id,
           current_execution.dispatch_lease_id,
           current_execution.lease_expires_at,
           jsonb_build_object('source', 'checkpoint_ready')
      FROM ready_checkpoint
      JOIN current_execution ON current_execution.run_id = ready_checkpoint.run_id
      JOIN checkpoint_artifacts_ready ON true
      JOIN suspended_queue_entry ON suspended_queue_entry.run_id = ready_checkpoint.run_id
    ON CONFLICT (org_id, run_id, checkpoint_id, state, worker_instance_id, execution_id) DO UPDATE
       SET dispatch_message_id = EXCLUDED.dispatch_message_id,
           dispatch_lease_id = EXCLUDED.dispatch_lease_id,
           lease_expires_at = EXCLUDED.lease_expires_at,
           unavailable_at = NULL,
           metadata = EXCLUDED.metadata
    RETURNING checkpoint_id
),
retired_hot_availability AS (
    UPDATE checkpoint_availability_replicas
       SET unavailable_at = COALESCE(unavailable_at, now())
      FROM durable_availability
     WHERE checkpoint_availability_replicas.org_id = sqlc.arg(org_id)
       AND checkpoint_availability_replicas.run_id = sqlc.arg(run_id)
       AND checkpoint_availability_replicas.checkpoint_id = durable_availability.checkpoint_id
       AND checkpoint_availability_replicas.state = 'hot'
       AND checkpoint_availability_replicas.unavailable_at IS NULL
    RETURNING checkpoint_availability_replicas.id
),
retired_hot AS (
    SELECT count(*) AS availability_count FROM retired_hot_availability
),
waitpoint AS (
    UPDATE waitpoints
       SET status = 'waiting',
           requested_at = now()
      FROM ready_checkpoint
      JOIN target_waitpoint ON target_waitpoint.checkpoint_id = ready_checkpoint.id
      JOIN durable_availability ON durable_availability.checkpoint_id = ready_checkpoint.id
     WHERE waitpoints.org_id = sqlc.arg(org_id)
       AND waitpoints.run_id = ready_checkpoint.run_id
       AND waitpoints.id = target_waitpoint.id
       AND waitpoints.checkpoint_id = ready_checkpoint.id
       AND waitpoints.execution_id = sqlc.arg(execution_id)
       AND waitpoints.status = 'opening'
    RETURNING waitpoints.*
),
updated AS (
    UPDATE runs
       SET status = 'waiting',
           latest_checkpoint_id = waitpoint.checkpoint_id,
           current_execution_id = NULL,
           updated_at = now()
      FROM waitpoint
     WHERE runs.org_id = sqlc.arg(org_id)
       AND runs.id = waitpoint.run_id
       AND runs.current_execution_id = sqlc.arg(execution_id)
    RETURNING runs.id
),
detached_execution AS (
    UPDATE run_executions
       SET status = 'detached',
           active_duration_ms = sqlc.arg(active_duration_ms),
           released_at = now(),
           renewed_at = now()
      FROM waitpoint
     WHERE run_executions.org_id = sqlc.arg(org_id)
       AND run_executions.run_id = waitpoint.run_id
       AND run_executions.id = sqlc.arg(execution_id)
       AND run_executions.worker_instance_id = sqlc.arg(worker_instance_id)
       AND run_executions.status = 'running'
    RETURNING run_executions.id, run_executions.restore_checkpoint_id
),
completed_restore_checkpoint AS (
    UPDATE checkpoints
       SET status = 'ready',
           error_message = NULL,
           invalidated_at = NULL
      FROM waitpoint
      JOIN detached_execution ON true
     WHERE checkpoints.org_id = sqlc.arg(org_id)
       AND checkpoints.run_id = waitpoint.run_id
       AND checkpoints.id = detached_execution.restore_checkpoint_id
       AND checkpoints.status = 'restoring'
    RETURNING checkpoints.id
),
checkpoint_event AS (
    INSERT INTO run_events (org_id, run_id, kind, payload)
    SELECT sqlc.arg(org_id), waitpoint.run_id, 'checkpoint.ready', sqlc.arg(checkpoint_payload)
      FROM waitpoint
    RETURNING id
),
waitpoint_event AS (
    INSERT INTO run_events (org_id, run_id, kind, payload)
    SELECT sqlc.arg(org_id), waitpoint.run_id, 'waitpoint.requested',
           jsonb_build_object(
               'run_id', waitpoint.run_id,
               'waitpoint_id', waitpoint.id,
               'checkpoint_id', waitpoint.checkpoint_id,
               'kind', waitpoint.kind,
               'display_text', waitpoint.display_text,
               'request', waitpoint.request,
               'timeout', waitpoint.timeout_seconds
           )
      FROM waitpoint
    RETURNING id
)
SELECT waitpoint.*
  FROM waitpoint
  JOIN updated ON true
  JOIN detached_execution ON true
  LEFT JOIN completed_restore_checkpoint ON true
  JOIN suspended_queue_entry ON true
  JOIN ready_requirements ON true
  JOIN checkpoint_artifacts_ready ON true
  JOIN durable_availability ON durable_availability.checkpoint_id = waitpoint.checkpoint_id
  JOIN retired_hot ON true
  JOIN checkpoint_event ON true
  JOIN waitpoint_event ON true;

-- name: MarkWaitpointCheckpointFailed :one
WITH current_execution AS (
    SELECT runs.id AS run_id
      FROM runs
      JOIN run_executions ON run_executions.id = runs.current_execution_id
                          AND run_executions.org_id = runs.org_id
                          AND run_executions.run_id = runs.id
     WHERE runs.org_id = sqlc.arg(org_id)
       AND runs.id = sqlc.arg(run_id)
       AND runs.status = 'running'
       AND run_executions.id = sqlc.arg(execution_id)
       AND run_executions.worker_instance_id = sqlc.arg(worker_instance_id)
       AND run_executions.status = 'running'
       AND run_executions.lease_expires_at > now()
),
target_waitpoint AS (
    SELECT waitpoints.*
      FROM waitpoints
      JOIN current_execution ON current_execution.run_id = waitpoints.run_id
     WHERE waitpoints.org_id = sqlc.arg(org_id)
       AND waitpoints.id = sqlc.arg(waitpoint_id)
       AND waitpoints.checkpoint_id = sqlc.arg(checkpoint_id)
       AND waitpoints.execution_id = sqlc.arg(execution_id)
       AND waitpoints.status = 'opening'
     FOR UPDATE OF waitpoints
),
failed_checkpoint AS (
    UPDATE checkpoints
       SET status = 'invalid',
           error_message = sqlc.arg(error_message),
           invalidated_at = now()
      FROM target_waitpoint
     WHERE checkpoints.org_id = sqlc.arg(org_id)
       AND checkpoints.run_id = target_waitpoint.run_id
       AND checkpoints.id = target_waitpoint.checkpoint_id
       AND checkpoints.execution_id = sqlc.arg(execution_id)
       AND checkpoints.status = 'creating'
    RETURNING checkpoints.*
),
retired_availability AS (
    UPDATE checkpoint_availability_replicas
       SET unavailable_at = COALESCE(unavailable_at, now())
      FROM failed_checkpoint
     WHERE checkpoint_availability_replicas.org_id = failed_checkpoint.org_id
       AND checkpoint_availability_replicas.run_id = failed_checkpoint.run_id
       AND checkpoint_availability_replicas.checkpoint_id = failed_checkpoint.id
       AND checkpoint_availability_replicas.unavailable_at IS NULL
    RETURNING checkpoint_availability_replicas.id
),
retired AS (
    SELECT count(*) AS availability_count FROM retired_availability
),
cancelled AS (
    UPDATE waitpoints
       SET status = 'cancelled',
           resolution_kind = 'cancelled',
           resolution = jsonb_build_object('reason', sqlc.arg(error_message), 'source', 'checkpoint'),
           requested_at = COALESCE(requested_at, now()),
           resolved_at = now()
      FROM failed_checkpoint
     WHERE waitpoints.org_id = sqlc.arg(org_id)
       AND waitpoints.run_id = failed_checkpoint.run_id
       AND waitpoints.id = sqlc.arg(waitpoint_id)
       AND waitpoints.checkpoint_id = failed_checkpoint.id
       AND waitpoints.execution_id = sqlc.arg(execution_id)
       AND waitpoints.status = 'opening'
    RETURNING waitpoints.*
)
SELECT cancelled.* FROM cancelled JOIN retired ON true;

-- name: GetPendingWaitpointForRun :one
SELECT * FROM waitpoints
 WHERE org_id = sqlc.arg(org_id)
   AND run_id = sqlc.arg(run_id)
   AND status = 'waiting'
 ORDER BY requested_at DESC
 LIMIT 1;

-- name: ResolveWaitpoint :one
WITH resolved AS (
    UPDATE waitpoints
       SET status = 'resuming',
           resolution_kind = sqlc.arg(resolution_kind),
           resolution = sqlc.arg(resolution),
           resolved_at = now()
     WHERE waitpoints.org_id = sqlc.arg(org_id)
       AND waitpoints.run_id = sqlc.arg(run_id)
       AND waitpoints.id = sqlc.arg(id)
       AND waitpoints.kind = sqlc.arg(kind)
       AND waitpoints.status = 'waiting'
       AND EXISTS (
           SELECT 1
             FROM run_queue_items
            WHERE run_queue_items.org_id = waitpoints.org_id
              AND run_queue_items.run_id = waitpoints.run_id
              AND run_queue_items.status = 'suspended'
       )
    RETURNING *
),
updated_run AS (
    UPDATE runs
       SET status = 'queued',
           updated_at = now()
      FROM resolved
     WHERE runs.org_id = sqlc.arg(org_id)
       AND runs.id = resolved.run_id
       AND runs.status = 'waiting'
       AND runs.current_execution_id IS NULL
    RETURNING runs.id
),
continuation_queue_entry AS (
    UPDATE run_queue_items
       SET status = 'queued',
           dispatch_message_id = NULL,
           reserved_by_worker_instance_id = NULL,
           reservation_expires_at = NULL,
           dispatch_generation = dispatch_generation + 1,
           last_error = '',
           enqueued_at = now(),
           updated_at = now(),
           finished_at = NULL
      FROM updated_run
     WHERE run_queue_items.org_id = sqlc.arg(org_id)
       AND run_queue_items.run_id = updated_run.id
       AND run_queue_items.status = 'suspended'
    RETURNING run_queue_items.run_id
),
event AS (
    INSERT INTO run_events (org_id, run_id, kind, payload)
    SELECT sqlc.arg(org_id), resolved.run_id, 'waitpoint.resolved', sqlc.arg(payload)
      FROM resolved
    RETURNING id
)
SELECT resolved.* FROM resolved JOIN updated_run ON true JOIN continuation_queue_entry ON true JOIN event ON true;

-- name: ExpireDuePendingWaitpoints :exec
WITH current_waitpoints AS (
    SELECT waitpoints.*
      FROM waitpoints
      JOIN runs ON runs.org_id = waitpoints.org_id
               AND runs.id = waitpoints.run_id
     WHERE waitpoints.org_id = sqlc.arg(org_id)
       AND waitpoints.status = 'waiting'
       AND waitpoints.timeout_seconds IS NOT NULL
       AND waitpoints.requested_at + (waitpoints.timeout_seconds * interval '1 second') <= now()
       AND runs.status = 'waiting'
       AND runs.current_execution_id IS NULL
       AND EXISTS (
           SELECT 1
             FROM run_queue_items
            WHERE run_queue_items.org_id = waitpoints.org_id
              AND run_queue_items.run_id = waitpoints.run_id
              AND run_queue_items.status = 'suspended'
       )
     FOR UPDATE OF waitpoints
),
expired AS (
    UPDATE waitpoints
       SET status = 'resuming',
           resolution_kind = 'timed_out',
           resolution = jsonb_build_object('at', now()),
           resolved_at = now()
      FROM current_waitpoints
     WHERE waitpoints.org_id = current_waitpoints.org_id
       AND waitpoints.run_id = current_waitpoints.run_id
       AND waitpoints.id = current_waitpoints.id
    RETURNING waitpoints.*
),
updated_runs AS (
    UPDATE runs
       SET status = 'queued',
           updated_at = now()
      FROM expired
     WHERE runs.org_id = expired.org_id
       AND runs.id = expired.run_id
       AND runs.status = 'waiting'
       AND runs.current_execution_id IS NULL
    RETURNING runs.id, runs.org_id
),
continuation_queue_entries AS (
    UPDATE run_queue_items
       SET status = 'queued',
           dispatch_message_id = NULL,
           reserved_by_worker_instance_id = NULL,
           reservation_expires_at = NULL,
           dispatch_generation = dispatch_generation + 1,
           last_error = '',
           enqueued_at = now(),
           updated_at = now(),
           finished_at = NULL
      FROM updated_runs
     WHERE run_queue_items.org_id = updated_runs.org_id
       AND run_queue_items.run_id = updated_runs.id
       AND run_queue_items.status = 'suspended'
    RETURNING run_queue_items.org_id, run_queue_items.run_id
)
INSERT INTO run_events (org_id, run_id, kind, payload)
SELECT expired.org_id,
       expired.run_id,
       'waitpoint.resolved',
       jsonb_build_object(
           'run_id', expired.run_id,
           'waitpoint_id', expired.id,
           'kind', expired.kind,
           'resolution_kind', 'timed_out'
       )
  FROM expired
  JOIN updated_runs ON updated_runs.org_id = expired.org_id
                   AND updated_runs.id = expired.run_id
  JOIN continuation_queue_entries ON continuation_queue_entries.org_id = expired.org_id
                                 AND continuation_queue_entries.run_id = expired.run_id;
