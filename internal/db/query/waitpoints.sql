-- name: CreateWaitpointForExecution :one
WITH current_execution AS (
    SELECT runs.id AS run_id,
           runs.project_id,
           runs.environment_id,
           run_executions.dispatch_message_id,
           run_executions.dispatch_lease_id
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
existing_run_wait AS (
    SELECT run_waits.*
      FROM run_waits
      JOIN current_execution ON current_execution.run_id = run_waits.run_id
     WHERE run_waits.org_id = sqlc.arg(org_id)
       AND run_waits.correlation_id = sqlc.arg(correlation_id)
       AND run_waits.status = 'opening'
),
checkpoint AS (
    INSERT INTO checkpoints (
        id,
        org_id,
        run_id,
        project_id,
        environment_id,
        execution_id,
        reason
    )
    SELECT
        sqlc.arg(checkpoint_id),
        sqlc.arg(org_id),
        current_execution.run_id,
        current_execution.project_id,
        current_execution.environment_id,
        sqlc.arg(execution_id),
        sqlc.arg(checkpoint_reason)
      FROM current_execution
     WHERE NOT EXISTS (SELECT 1 FROM existing_run_wait)
    ON CONFLICT (id) DO UPDATE SET
        id = EXCLUDED.id
     WHERE checkpoints.status = 'creating'
    RETURNING *
),
created_waitpoint AS (
    INSERT INTO waitpoints (
        id,
        org_id,
        project_id,
        environment_id,
        kind,
        request,
        display_text
    )
    SELECT
        sqlc.arg(id),
        sqlc.arg(org_id),
        current_execution.project_id,
        current_execution.environment_id,
        sqlc.arg(kind),
        sqlc.arg(request),
        sqlc.arg(display_text)
      FROM current_execution
      JOIN checkpoint ON checkpoint.run_id = current_execution.run_id
    ON CONFLICT (id) DO UPDATE SET
        request = waitpoints.request,
        display_text = waitpoints.display_text
	     WHERE waitpoints.status IN ('pending', 'completed')
	       AND waitpoints.org_id = sqlc.arg(org_id)
	       AND waitpoints.project_id = EXCLUDED.project_id
	       AND waitpoints.environment_id = EXCLUDED.environment_id
	       AND waitpoints.kind = sqlc.arg(kind)
	       AND (waitpoints.expires_at IS NULL OR waitpoints.expires_at > now())
    RETURNING *
),
created_run_wait AS (
    INSERT INTO run_waits (
        id,
        org_id,
        run_id,
        project_id,
        environment_id,
        execution_id,
        checkpoint_id,
        correlation_id,
        timeout_seconds,
        policy_name,
        policy_snapshot
    )
    SELECT
        sqlc.arg(run_wait_id),
        sqlc.arg(org_id),
        current_execution.run_id,
        current_execution.project_id,
        current_execution.environment_id,
        sqlc.arg(execution_id),
        checkpoint.id,
        sqlc.arg(correlation_id),
        sqlc.narg(timeout_seconds),
        sqlc.narg(policy_name),
        sqlc.narg(policy_snapshot)
      FROM current_execution
      JOIN checkpoint ON checkpoint.run_id = current_execution.run_id
      JOIN created_waitpoint ON true
    ON CONFLICT (run_id, correlation_id) WHERE status IN ('opening', 'waiting') DO UPDATE SET
        checkpoint_id = run_waits.checkpoint_id
     WHERE run_waits.status = 'opening'
    RETURNING *
),
created_dependency AS (
    INSERT INTO run_wait_dependencies (
        org_id,
        run_id,
        project_id,
        environment_id,
        run_wait_id,
        waitpoint_id
    )
    SELECT
        sqlc.arg(org_id),
        created_run_wait.run_id,
        current_execution.project_id,
        current_execution.environment_id,
        created_run_wait.id,
        created_waitpoint.id
      FROM created_run_wait
      JOIN current_execution ON current_execution.run_id = created_run_wait.run_id
      JOIN created_waitpoint ON true
    ON CONFLICT (org_id, run_wait_id, waitpoint_id) DO NOTHING
    RETURNING *
),
selected AS (
    SELECT waitpoints.id,
           existing_run_wait.id AS run_wait_id,
           waitpoints.org_id,
           existing_run_wait.run_id,
           existing_run_wait.execution_id,
           existing_run_wait.checkpoint_id,
           existing_run_wait.correlation_id,
           waitpoints.kind,
           waitpoints.request,
           waitpoints.display_text,
           existing_run_wait.timeout_seconds,
           existing_run_wait.policy_name,
           existing_run_wait.policy_snapshot,
           existing_run_wait.status,
           existing_run_wait.resolution_kind,
           existing_run_wait.resolution,
           waitpoints.created_at,
           existing_run_wait.waiting_at AS requested_at,
           existing_run_wait.resolved_at
      FROM existing_run_wait
      JOIN run_wait_dependencies ON run_wait_dependencies.org_id = existing_run_wait.org_id
                                AND run_wait_dependencies.run_wait_id = existing_run_wait.id
      JOIN waitpoints ON waitpoints.org_id = run_wait_dependencies.org_id
                     AND waitpoints.id = run_wait_dependencies.waitpoint_id
    UNION ALL
    SELECT created_waitpoint.id,
           created_run_wait.id AS run_wait_id,
           created_waitpoint.org_id,
           created_run_wait.run_id,
           created_run_wait.execution_id,
           created_run_wait.checkpoint_id,
           created_run_wait.correlation_id,
           created_waitpoint.kind,
           created_waitpoint.request,
           created_waitpoint.display_text,
           created_run_wait.timeout_seconds,
           created_run_wait.policy_name,
           created_run_wait.policy_snapshot,
           created_run_wait.status,
           created_run_wait.resolution_kind,
           created_run_wait.resolution,
           created_waitpoint.created_at,
           created_run_wait.waiting_at AS requested_at,
           created_run_wait.resolved_at
      FROM created_run_wait
      JOIN created_dependency ON created_dependency.org_id = created_run_wait.org_id
                             AND created_dependency.run_wait_id = created_run_wait.id
      JOIN created_waitpoint ON created_waitpoint.org_id = created_dependency.org_id
                            AND created_waitpoint.id = created_dependency.waitpoint_id
),
checkpoint_started_event AS (
    INSERT INTO run_events (org_id, run_id, execution_id, attempt_number, kind, payload)
    SELECT sqlc.arg(org_id),
           selected.run_id,
           run_executions.id,
           run_executions.attempt_number,
           'checkpoint.started',
           jsonb_build_object(
               'run_id', selected.run_id,
               'waitpoint_id', selected.id,
               'checkpoint_id', selected.checkpoint_id,
               'kind', selected.kind,
               'display_text', selected.display_text
           )
      FROM selected
      LEFT JOIN run_executions ON run_executions.org_id = selected.org_id
                              AND run_executions.run_id = selected.run_id
                              AND run_executions.id = selected.execution_id
     WHERE NOT EXISTS (SELECT 1 FROM existing_run_wait)
    RETURNING id
),
checkpoint_started AS (
    SELECT count(*) AS event_count FROM checkpoint_started_event
)
SELECT selected.*
  FROM selected
  JOIN checkpoint_started ON true
 LIMIT 1;

-- name: CreateHumanWaitpoint :one
WITH cleared_expired_idempotency_keys AS (
    UPDATE waitpoints
       SET idempotency_key = NULL,
           idempotency_request_hash = NULL,
           idempotency_key_expires_at = NULL,
           idempotency_key_options = '{}'::jsonb
     WHERE waitpoints.org_id = sqlc.arg(org_id)
       AND waitpoints.project_id = sqlc.arg(project_id)
       AND waitpoints.environment_id = sqlc.arg(environment_id)
       AND waitpoints.idempotency_key = sqlc.narg(idempotency_key)
       AND sqlc.narg(idempotency_key)::text IS NOT NULL
       AND waitpoints.idempotency_key_expires_at IS NOT NULL
       AND waitpoints.idempotency_key_expires_at <= now()
    RETURNING id
),
existing_waitpoint AS (
    SELECT *
      FROM waitpoints
     WHERE waitpoints.org_id = sqlc.arg(org_id)
       AND waitpoints.project_id = sqlc.arg(project_id)
       AND waitpoints.environment_id = sqlc.arg(environment_id)
       AND waitpoints.idempotency_key = sqlc.narg(idempotency_key)
       AND sqlc.narg(idempotency_key)::text IS NOT NULL
       AND NOT EXISTS (SELECT 1 FROM cleared_expired_idempotency_keys)
),
inserted_waitpoint AS (
    INSERT INTO waitpoints (
        id,
        org_id,
        project_id,
        environment_id,
        kind,
        request,
        display_text,
        expires_at,
        idempotency_key,
        idempotency_request_hash,
        idempotency_key_expires_at,
        idempotency_key_options
    )
    SELECT
        sqlc.arg(id),
        sqlc.arg(org_id),
        sqlc.arg(project_id),
        sqlc.arg(environment_id),
        'human',
        sqlc.arg(request),
        sqlc.arg(display_text),
        sqlc.arg(expires_at),
        sqlc.narg(idempotency_key),
        sqlc.narg(idempotency_request_hash),
        sqlc.narg(idempotency_key_expires_at),
        sqlc.arg(idempotency_key_options)::jsonb
     WHERE NOT EXISTS (SELECT 1 FROM existing_waitpoint)
    RETURNING *
),
matching_existing_waitpoint AS (
    SELECT *
      FROM existing_waitpoint
     WHERE idempotency_request_hash = sqlc.narg(idempotency_request_hash)
)
SELECT * FROM inserted_waitpoint
UNION ALL
SELECT * FROM matching_existing_waitpoint
LIMIT 1;

-- name: AcknowledgeRestore :one
WITH current_execution AS (
    SELECT runs.id AS run_id,
           run_executions.restore_checkpoint_id
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
       AND run_executions.restore_checkpoint_id = sqlc.arg(checkpoint_id)
       AND run_executions.lease_expires_at > now()
     FOR UPDATE OF runs, run_executions
),
checkpoint AS (
    SELECT checkpoints.id,
           checkpoints.status
      FROM checkpoints
      JOIN current_execution ON current_execution.run_id = checkpoints.run_id
                           AND current_execution.restore_checkpoint_id = checkpoints.id
     WHERE checkpoints.org_id = sqlc.arg(org_id)
       AND checkpoints.status IN ('restoring', 'ready')
     FOR UPDATE OF checkpoints
),
acknowledged_checkpoint AS (
    UPDATE checkpoints
       SET status = 'ready',
           error_message = NULL,
           invalidated_at = NULL
      FROM checkpoint
     WHERE checkpoints.org_id = sqlc.arg(org_id)
       AND checkpoints.id = checkpoint.id
       AND checkpoint.status = 'restoring'
    RETURNING checkpoints.id
),
checkpoint_ready AS (
    SELECT id FROM acknowledged_checkpoint
    UNION ALL
    SELECT id FROM checkpoint WHERE status = 'ready'
),
restored_run_wait AS (
    UPDATE run_waits
       SET status = 'restored',
           restored_at = now(),
           updated_at = now()
      FROM current_execution
      JOIN checkpoint_ready ON checkpoint_ready.id = current_execution.restore_checkpoint_id
     WHERE run_waits.org_id = sqlc.arg(org_id)
       AND run_waits.run_id = current_execution.run_id
       AND run_waits.id = sqlc.arg(run_wait_id)
       AND run_waits.checkpoint_id = current_execution.restore_checkpoint_id
       AND run_waits.status = 'resuming'
    RETURNING run_waits.*
),
current_run_wait AS (
    SELECT run_waits.*
      FROM run_waits
      JOIN current_execution ON current_execution.run_id = run_waits.run_id
      JOIN checkpoint_ready ON checkpoint_ready.id = current_execution.restore_checkpoint_id
     WHERE run_waits.org_id = sqlc.arg(org_id)
       AND run_waits.id = sqlc.arg(run_wait_id)
       AND run_waits.checkpoint_id = current_execution.restore_checkpoint_id
       AND run_waits.status = 'restored'
),
selected_run_wait AS (
    SELECT * FROM restored_run_wait
    UNION ALL
    SELECT * FROM current_run_wait
    WHERE NOT EXISTS (SELECT 1 FROM restored_run_wait)
)
SELECT waitpoints.id,
       selected_run_wait.id AS run_wait_id,
       waitpoints.org_id,
       selected_run_wait.run_id,
       selected_run_wait.execution_id,
       selected_run_wait.checkpoint_id,
       selected_run_wait.correlation_id,
       waitpoints.kind,
       waitpoints.request,
       waitpoints.display_text,
       selected_run_wait.timeout_seconds,
       selected_run_wait.policy_name,
       selected_run_wait.policy_snapshot,
       selected_run_wait.status,
       selected_run_wait.resolution_kind,
       selected_run_wait.resolution,
       waitpoints.created_at,
       selected_run_wait.waiting_at AS requested_at,
       selected_run_wait.resolved_at
  FROM selected_run_wait
  JOIN run_wait_dependencies ON run_wait_dependencies.org_id = selected_run_wait.org_id
                            AND run_wait_dependencies.run_wait_id = selected_run_wait.id
  JOIN waitpoints ON waitpoints.org_id = run_wait_dependencies.org_id
                 AND waitpoints.id = run_wait_dependencies.waitpoint_id
                 AND waitpoints.id = sqlc.arg(waitpoint_id)
 LIMIT 1;

-- name: MarkWaitpointCheckpointDurableReady :one
WITH current_execution AS (
    SELECT runs.id AS run_id,
           run_executions.dispatch_message_id,
           run_executions.dispatch_lease_id,
           run_executions.worker_runtime_id
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
target_run_wait AS (
    SELECT run_waits.*
      FROM run_waits
      JOIN current_execution ON current_execution.run_id = run_waits.run_id
     WHERE run_waits.org_id = sqlc.arg(org_id)
       AND run_waits.id = sqlc.arg(run_wait_id)
       AND run_waits.checkpoint_id = sqlc.arg(checkpoint_id)
       AND run_waits.execution_id = sqlc.arg(execution_id)
       AND run_waits.status = 'opening'
     FOR UPDATE OF run_waits
),
target_waitpoint AS (
    SELECT waitpoints.*
      FROM waitpoints
      JOIN run_wait_dependencies ON run_wait_dependencies.org_id = waitpoints.org_id
                                AND run_wait_dependencies.waitpoint_id = waitpoints.id
      JOIN target_run_wait ON target_run_wait.org_id = run_wait_dependencies.org_id
                          AND target_run_wait.id = run_wait_dependencies.run_wait_id
	     WHERE waitpoints.id = sqlc.arg(waitpoint_id)
	       AND waitpoints.status IN ('pending', 'completed')
	       AND (waitpoints.expires_at IS NULL OR waitpoints.expires_at > now())
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
     FOR UPDATE OF run_queue_items
),
expected_runtime AS (
    -- Checkpoint resume is currently Firecracker-only. Add a backend-specific
    -- identity branch here before accepting other runtime_backends.
    SELECT runtime_releases.runtime_id,
           runtime_releases.runtime_arch,
           runtime_releases.runtime_abi,
           runtime_releases.kernel_digest,
           runtime_releases.initramfs_digest,
           runtime_releases.rootfs_digest,
           runtime_releases.cni_profile
      FROM current_execution
      JOIN runtime_releases ON runtime_releases.runtime_id = current_execution.worker_runtime_id
     WHERE sqlc.arg(runtime_backend)::text = 'firecracker'
       AND runtime_releases.runtime_id = sqlc.arg(runtime_id)
       AND runtime_releases.runtime_arch = sqlc.arg(runtime_arch)
       AND runtime_releases.runtime_abi = sqlc.arg(runtime_abi)
       AND runtime_releases.kernel_digest = sqlc.arg(kernel_digest)
       AND runtime_releases.initramfs_digest = sqlc.arg(initramfs_digest)
       AND runtime_releases.rootfs_digest = sqlc.arg(rootfs_digest)
       AND runtime_releases.cni_profile = sqlc.arg(cni_profile)
),
checkpoint_artifact_input AS (
    SELECT uuidv7() AS artifact_id,
           (artifact.value->>'role')::checkpoint_artifact_role AS role,
           (artifact.value->>'ordinal')::int AS ordinal,
           artifact.value->>'digest' AS digest,
           (artifact.value->>'size_bytes')::bigint AS size_bytes,
           artifact.value->>'media_type' AS media_type,
           CASE (artifact.value->>'role')::checkpoint_artifact_role
             WHEN 'runtime_config' THEN 'checkpoint_runtime_config'::artifact_kind
             WHEN 'runtime_vmstate' THEN 'checkpoint_vmstate'::artifact_kind
             WHEN 'runtime_memory' THEN 'checkpoint_memory'::artifact_kind
             WHEN 'runtime_scratch_disk' THEN 'checkpoint_scratch_disk'::artifact_kind
           END AS kind,
           COALESCE((artifact.value->>'encrypt_duration_ms')::bigint, 0) AS encrypt_duration_ms,
           COALESCE((artifact.value->>'store_duration_ms')::bigint, 0) AS store_duration_ms
      FROM jsonb_array_elements(sqlc.arg(checkpoint_artifacts)::jsonb) AS artifact(value)
),
workspace_artifact_input AS (
    SELECT uuidv7() AS artifact_id,
           sqlc.narg(workspace_artifact_digest)::text AS digest,
           sqlc.narg(workspace_artifact_size_bytes)::bigint AS size_bytes,
           sqlc.narg(workspace_artifact_media_type)::text AS media_type,
           'checkpoint_workspace'::artifact_kind AS kind
     WHERE sqlc.narg(workspace_artifact_digest)::text IS NOT NULL
),
artifact_input AS (
    SELECT artifact_id,
           role,
           ordinal,
           digest,
           size_bytes,
           media_type,
           kind,
           encrypt_duration_ms,
           store_duration_ms
      FROM checkpoint_artifact_input
    UNION ALL
    SELECT artifact_id,
           NULL::checkpoint_artifact_role AS role,
           NULL::int AS ordinal,
           digest,
           size_bytes,
           media_type,
           kind,
           0::bigint AS encrypt_duration_ms,
           0::bigint AS store_duration_ms
      FROM workspace_artifact_input
),
cas_object_input AS (
    SELECT DISTINCT
           digest,
           size_bytes,
           media_type
      FROM artifact_input
),
published_cas_objects AS (
    INSERT INTO cas_objects (digest, size_bytes, media_type)
    SELECT digest, size_bytes, media_type
      FROM cas_object_input
      JOIN target_run_wait ON true
      JOIN locked_queue_entry ON locked_queue_entry.run_id = target_run_wait.run_id
      JOIN expected_runtime ON true
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
           ready_at = now()
      FROM target_run_wait
      JOIN cas_objects_ready ON cas_objects_ready.ok
      JOIN locked_queue_entry ON locked_queue_entry.run_id = target_run_wait.run_id
      JOIN expected_runtime ON true
     WHERE checkpoints.org_id = sqlc.arg(org_id)
       AND checkpoints.run_id = target_run_wait.run_id
       AND checkpoints.id = target_run_wait.checkpoint_id
       AND checkpoints.execution_id = sqlc.arg(execution_id)
       AND checkpoints.status = 'creating'
    RETURNING checkpoints.*
),
inserted_artifacts AS (
    INSERT INTO artifacts (
        id,
        org_id,
        project_id,
        environment_id,
        digest,
        kind,
        size_bytes,
        media_type,
        created_by_worker_instance_id
    )
    SELECT artifact_input.artifact_id,
           ready_checkpoint.org_id,
           ready_checkpoint.project_id,
           ready_checkpoint.environment_id,
           artifact_input.digest,
           artifact_input.kind,
           artifact_input.size_bytes,
           artifact_input.media_type,
           sqlc.arg(worker_instance_id)
      FROM ready_checkpoint
      JOIN artifact_input ON true
    RETURNING id
),
ready_runtime_snapshot AS (
    INSERT INTO checkpoint_runtime_snapshots (
        org_id,
        project_id,
        environment_id,
        run_id,
        checkpoint_id,
        runtime_backend,
        runtime_id,
        runtime_arch,
        runtime_abi,
        kernel_digest,
        initramfs_digest,
        rootfs_digest,
        runtime_vcpus,
        runtime_memory_mib,
        runtime_scratch_disk_mib,
        cni_profile,
        image_key,
        runtime_config_artifact_id
    )
    SELECT ready_checkpoint.org_id,
           ready_checkpoint.project_id,
           ready_checkpoint.environment_id,
           ready_checkpoint.run_id,
           ready_checkpoint.id,
           sqlc.arg(runtime_backend),
           expected_runtime.runtime_id,
           expected_runtime.runtime_arch,
           expected_runtime.runtime_abi,
           expected_runtime.kernel_digest,
           expected_runtime.initramfs_digest,
           expected_runtime.rootfs_digest,
           sqlc.narg(runtime_vcpus),
           sqlc.narg(runtime_memory_mib),
           sqlc.narg(runtime_scratch_disk_mib),
           expected_runtime.cni_profile,
           sqlc.narg(image_key),
           runtime_config_artifact.artifact_id
      FROM ready_checkpoint
      JOIN expected_runtime ON true
      LEFT JOIN artifact_input AS runtime_config_artifact
        ON runtime_config_artifact.role = 'runtime_config'
       AND runtime_config_artifact.ordinal = 0
      LEFT JOIN inserted_artifacts AS inserted_runtime_config_artifact
        ON inserted_runtime_config_artifact.id = runtime_config_artifact.artifact_id
    ON CONFLICT (org_id, run_id, checkpoint_id) DO UPDATE
       SET runtime_backend = EXCLUDED.runtime_backend,
           project_id = EXCLUDED.project_id,
           environment_id = EXCLUDED.environment_id,
           runtime_id = EXCLUDED.runtime_id,
           runtime_arch = EXCLUDED.runtime_arch,
           runtime_abi = EXCLUDED.runtime_abi,
           kernel_digest = EXCLUDED.kernel_digest,
           initramfs_digest = EXCLUDED.initramfs_digest,
           rootfs_digest = EXCLUDED.rootfs_digest,
           runtime_vcpus = EXCLUDED.runtime_vcpus,
           runtime_memory_mib = EXCLUDED.runtime_memory_mib,
           runtime_scratch_disk_mib = EXCLUDED.runtime_scratch_disk_mib,
           cni_profile = EXCLUDED.cni_profile,
           image_key = EXCLUDED.image_key,
           runtime_config_artifact_id = EXCLUDED.runtime_config_artifact_id
    RETURNING *
),
ready_workspace_snapshot AS (
    INSERT INTO checkpoint_workspace_snapshots (
        org_id,
        project_id,
        environment_id,
        run_id,
        checkpoint_id,
        workspace_artifact_id,
        workspace_artifact_encoding,
        workspace_mount_path,
        workspace_volume_kind
    )
    SELECT ready_checkpoint.org_id,
           ready_checkpoint.project_id,
           ready_checkpoint.environment_id,
           ready_checkpoint.run_id,
           ready_checkpoint.id,
           workspace_artifact_input.artifact_id,
           sqlc.narg(workspace_artifact_encoding),
           sqlc.narg(workspace_mount_path),
           sqlc.narg(workspace_volume_kind)
      FROM ready_checkpoint
      LEFT JOIN workspace_artifact_input ON true
      LEFT JOIN inserted_artifacts AS inserted_workspace_artifact
        ON inserted_workspace_artifact.id = workspace_artifact_input.artifact_id
    ON CONFLICT (org_id, run_id, checkpoint_id) DO UPDATE
       SET project_id = EXCLUDED.project_id,
           environment_id = EXCLUDED.environment_id,
           workspace_artifact_id = EXCLUDED.workspace_artifact_id,
           workspace_artifact_encoding = EXCLUDED.workspace_artifact_encoding,
           workspace_mount_path = EXCLUDED.workspace_mount_path,
           workspace_volume_kind = EXCLUDED.workspace_volume_kind
    RETURNING *
),
ready_requirements AS (
    UPDATE run_runtime_requirements
       SET requested_milli_cpu = COALESCE(ready_runtime_snapshot.runtime_vcpus::bigint * 1000, run_runtime_requirements.requested_milli_cpu),
           requested_memory_mib = COALESCE(ready_runtime_snapshot.runtime_memory_mib::bigint, run_runtime_requirements.requested_memory_mib),
           requested_disk_mib = COALESCE(ready_runtime_snapshot.runtime_scratch_disk_mib::bigint, run_runtime_requirements.requested_disk_mib),
           runtime_id = ready_runtime_snapshot.runtime_id,
           runtime_arch = ready_runtime_snapshot.runtime_arch,
           runtime_abi = ready_runtime_snapshot.runtime_abi,
           kernel_digest = ready_runtime_snapshot.kernel_digest,
           initramfs_digest = ready_runtime_snapshot.initramfs_digest,
           rootfs_digest = ready_runtime_snapshot.rootfs_digest,
           cni_profile = ready_runtime_snapshot.cni_profile,
           updated_at = now()
      FROM ready_checkpoint
      JOIN ready_runtime_snapshot ON ready_runtime_snapshot.org_id = ready_checkpoint.org_id
                                 AND ready_runtime_snapshot.run_id = ready_checkpoint.run_id
                                 AND ready_runtime_snapshot.checkpoint_id = ready_checkpoint.id
     WHERE run_runtime_requirements.org_id = ready_checkpoint.org_id
       AND run_runtime_requirements.run_id = ready_checkpoint.run_id
    RETURNING run_runtime_requirements.run_id
),
inserted_checkpoint_artifacts AS (
    INSERT INTO checkpoint_artifacts (
        org_id,
        project_id,
        environment_id,
        run_id,
        checkpoint_id,
        role,
        ordinal,
        artifact_id,
        encrypt_duration_ms,
        store_duration_ms
    )
    SELECT sqlc.arg(org_id),
           ready_checkpoint.project_id,
           ready_checkpoint.environment_id,
           ready_checkpoint.run_id,
           ready_checkpoint.id,
           checkpoint_artifact_input.role,
           checkpoint_artifact_input.ordinal,
           checkpoint_artifact_input.artifact_id,
           checkpoint_artifact_input.encrypt_duration_ms,
           checkpoint_artifact_input.store_duration_ms
      FROM ready_checkpoint
      JOIN checkpoint_artifact_input ON true
      JOIN inserted_artifacts ON inserted_artifacts.id = checkpoint_artifact_input.artifact_id
    ON CONFLICT (org_id, run_id, checkpoint_id, role, ordinal) DO UPDATE
       SET project_id = EXCLUDED.project_id,
           environment_id = EXCLUDED.environment_id,
           artifact_id = EXCLUDED.artifact_id,
           encrypt_duration_ms = EXCLUDED.encrypt_duration_ms,
           store_duration_ms = EXCLUDED.store_duration_ms
    RETURNING artifact_id
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
waiting_run_wait AS (
    UPDATE run_waits
       SET status = 'waiting',
           waiting_at = now(),
           active_duration_ms = sqlc.arg(active_duration_ms),
           updated_at = now()
      FROM ready_checkpoint
      JOIN target_run_wait ON target_run_wait.checkpoint_id = ready_checkpoint.id
     WHERE run_waits.org_id = sqlc.arg(org_id)
       AND run_waits.run_id = ready_checkpoint.run_id
       AND run_waits.id = target_run_wait.id
       AND run_waits.checkpoint_id = ready_checkpoint.id
       AND run_waits.execution_id = sqlc.arg(execution_id)
       AND run_waits.status = 'opening'
    RETURNING run_waits.*
),
updated AS (
    UPDATE runs
       SET status = 'waiting',
           latest_checkpoint_id = waiting_run_wait.checkpoint_id,
           current_execution_id = NULL,
           updated_at = now()
      FROM waiting_run_wait
     WHERE runs.org_id = sqlc.arg(org_id)
       AND runs.id = waiting_run_wait.run_id
       AND runs.current_execution_id = sqlc.arg(execution_id)
    RETURNING runs.id
),
detached_execution AS (
    UPDATE run_executions
       SET status = 'detached',
           active_duration_ms = sqlc.arg(active_duration_ms),
           released_at = now(),
           renewed_at = now()
      FROM waiting_run_wait
     WHERE run_executions.org_id = sqlc.arg(org_id)
       AND run_executions.run_id = waiting_run_wait.run_id
       AND run_executions.id = sqlc.arg(execution_id)
       AND run_executions.worker_instance_id = sqlc.arg(worker_instance_id)
       AND run_executions.status = 'running'
    RETURNING run_executions.id, run_executions.attempt_number, run_executions.restore_checkpoint_id
),
released_concurrency_slot AS (
    UPDATE run_queue_concurrency_leases
       SET released_at = now()
      FROM waiting_run_wait
     WHERE run_queue_concurrency_leases.org_id = sqlc.arg(org_id)
       AND run_queue_concurrency_leases.run_id = waiting_run_wait.run_id
       AND run_queue_concurrency_leases.execution_id = sqlc.arg(execution_id)
       AND run_queue_concurrency_leases.released_at IS NULL
    RETURNING run_queue_concurrency_leases.id
),
completed_restore_checkpoint AS (
    UPDATE checkpoints
       SET status = 'ready',
           error_message = NULL,
           invalidated_at = NULL
      FROM waiting_run_wait
      JOIN detached_execution ON true
     WHERE checkpoints.org_id = sqlc.arg(org_id)
       AND checkpoints.run_id = waiting_run_wait.run_id
       AND checkpoints.id = detached_execution.restore_checkpoint_id
       AND checkpoints.status = 'restoring'
    RETURNING checkpoints.id
),
restored_previous_run_wait AS (
    UPDATE run_waits
       SET status = 'restored',
           restored_at = now(),
           updated_at = now()
      FROM completed_restore_checkpoint
      JOIN detached_execution ON detached_execution.restore_checkpoint_id = completed_restore_checkpoint.id
     WHERE run_waits.org_id = sqlc.arg(org_id)
       AND run_waits.run_id = sqlc.arg(run_id)
       AND run_waits.checkpoint_id = completed_restore_checkpoint.id
       AND run_waits.status = 'resuming'
    RETURNING run_waits.id
),
resolved_restore AS (
    SELECT
        (SELECT count(*) FROM restored_previous_run_wait) AS waitpoint_count,
        (SELECT count(*) FROM released_concurrency_slot) AS concurrency_slot_count
),
checkpoint_event AS (
    INSERT INTO run_events (org_id, run_id, execution_id, attempt_number, kind, payload)
    SELECT sqlc.arg(org_id),
           waiting_run_wait.run_id,
           detached_execution.id,
           detached_execution.attempt_number,
           'checkpoint.ready',
           sqlc.arg(checkpoint_payload)
      FROM waiting_run_wait
      JOIN detached_execution ON true
    RETURNING id
),
waitpoint_event AS (
    INSERT INTO run_events (org_id, run_id, execution_id, attempt_number, kind, payload)
    SELECT sqlc.arg(org_id),
           waiting_run_wait.run_id,
           detached_execution.id,
           detached_execution.attempt_number,
           'waitpoint.requested',
           jsonb_build_object(
               'run_id', waiting_run_wait.run_id,
               'waitpoint_id', waitpoints.id,
               'checkpoint_id', waiting_run_wait.checkpoint_id,
               'kind', waitpoints.kind,
               'display_text', waitpoints.display_text,
               'request', waitpoints.request,
               'timeout', waiting_run_wait.timeout_seconds
           )
      FROM waiting_run_wait
      JOIN detached_execution ON true
      JOIN run_wait_dependencies ON run_wait_dependencies.org_id = waiting_run_wait.org_id
                                AND run_wait_dependencies.run_wait_id = waiting_run_wait.id
      JOIN waitpoints ON waitpoints.org_id = run_wait_dependencies.org_id
                     AND waitpoints.id = run_wait_dependencies.waitpoint_id
    RETURNING id
)
SELECT waitpoints.id,
       waiting_run_wait.id AS run_wait_id,
       waitpoints.org_id,
       waiting_run_wait.run_id,
       waiting_run_wait.execution_id,
       waiting_run_wait.checkpoint_id,
       waiting_run_wait.correlation_id,
       waitpoints.kind,
       waitpoints.request,
       waitpoints.display_text,
       waiting_run_wait.timeout_seconds,
       waiting_run_wait.policy_name,
       waiting_run_wait.policy_snapshot,
       waiting_run_wait.status,
       waiting_run_wait.resolution_kind,
       waiting_run_wait.resolution,
       waitpoints.created_at,
       waiting_run_wait.waiting_at AS requested_at,
       waiting_run_wait.resolved_at
  FROM waiting_run_wait
  JOIN run_wait_dependencies ON run_wait_dependencies.org_id = waiting_run_wait.org_id
                            AND run_wait_dependencies.run_wait_id = waiting_run_wait.id
  JOIN waitpoints ON waitpoints.org_id = run_wait_dependencies.org_id
                 AND waitpoints.id = run_wait_dependencies.waitpoint_id
  JOIN updated ON true
  JOIN detached_execution ON true
  LEFT JOIN completed_restore_checkpoint ON true
  JOIN resolved_restore ON true
  JOIN ready_runtime_snapshot ON true
  JOIN ready_workspace_snapshot ON true
  JOIN suspended_queue_entry ON true
  JOIN ready_requirements ON true
  JOIN checkpoint_artifacts_ready ON true
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
target_run_wait AS (
    SELECT run_waits.*
      FROM run_waits
      JOIN current_execution ON current_execution.run_id = run_waits.run_id
     WHERE run_waits.org_id = sqlc.arg(org_id)
       AND run_waits.id = sqlc.arg(run_wait_id)
       AND run_waits.checkpoint_id = sqlc.arg(checkpoint_id)
       AND run_waits.execution_id = sqlc.arg(execution_id)
       AND run_waits.status = 'opening'
     FOR UPDATE OF run_waits
),
failed_checkpoint AS (
    UPDATE checkpoints
       SET status = 'invalid',
           error_message = sqlc.arg(error_message),
           invalidated_at = now()
      FROM target_run_wait
     WHERE checkpoints.org_id = sqlc.arg(org_id)
       AND checkpoints.run_id = target_run_wait.run_id
       AND checkpoints.id = target_run_wait.checkpoint_id
       AND checkpoints.execution_id = sqlc.arg(execution_id)
       AND checkpoints.status = 'creating'
    RETURNING checkpoints.*
),
failed_run_wait AS (
    UPDATE run_waits
       SET status = 'failed',
           failure = jsonb_build_object('reason', sqlc.arg(error_message), 'source', 'checkpoint'),
           resolution_kind = 'cancelled',
           resolution = jsonb_build_object('reason', sqlc.arg(error_message), 'source', 'checkpoint'),
           failed_at = now(),
           updated_at = now()
      FROM failed_checkpoint
     WHERE run_waits.org_id = sqlc.arg(org_id)
       AND run_waits.run_id = failed_checkpoint.run_id
       AND run_waits.id = sqlc.arg(run_wait_id)
       AND run_waits.checkpoint_id = failed_checkpoint.id
       AND run_waits.execution_id = sqlc.arg(execution_id)
       AND run_waits.status = 'opening'
    RETURNING run_waits.*
),
cancelled_waitpoint AS (
    UPDATE waitpoints
       SET status = 'cancelled',
           resolution_kind = 'cancelled',
           output = 'null'::jsonb,
           resolution = jsonb_build_object('reason', sqlc.arg(error_message), 'source', 'checkpoint'),
           output_is_error = true,
           completed_at = now(),
           updated_at = now()
      FROM failed_run_wait
      JOIN run_wait_dependencies ON run_wait_dependencies.org_id = failed_run_wait.org_id
                                AND run_wait_dependencies.run_wait_id = failed_run_wait.id
     WHERE waitpoints.org_id = run_wait_dependencies.org_id
       AND waitpoints.id = run_wait_dependencies.waitpoint_id
       AND waitpoints.status = 'pending'
    RETURNING waitpoints.*
),
selected_waitpoint AS (
    SELECT waitpoints.*
      FROM failed_run_wait
      JOIN run_wait_dependencies ON run_wait_dependencies.org_id = failed_run_wait.org_id
                                AND run_wait_dependencies.run_wait_id = failed_run_wait.id
      JOIN waitpoints ON waitpoints.org_id = run_wait_dependencies.org_id
                     AND waitpoints.id = run_wait_dependencies.waitpoint_id
                     AND waitpoints.id = sqlc.arg(waitpoint_id)
)
SELECT selected_waitpoint.id,
       failed_run_wait.id AS run_wait_id,
       selected_waitpoint.org_id,
       failed_run_wait.run_id,
       failed_run_wait.execution_id,
       failed_run_wait.checkpoint_id,
       failed_run_wait.correlation_id,
       selected_waitpoint.kind,
       selected_waitpoint.request,
       selected_waitpoint.display_text,
       failed_run_wait.timeout_seconds,
       failed_run_wait.policy_name,
       failed_run_wait.policy_snapshot,
       failed_run_wait.status,
       failed_run_wait.resolution_kind,
       failed_run_wait.resolution,
       selected_waitpoint.created_at,
       failed_run_wait.waiting_at AS requested_at,
       failed_run_wait.resolved_at
  FROM failed_run_wait
  JOIN selected_waitpoint ON true;

-- name: GetPendingWaitpointForRun :one
SELECT waitpoints.id,
       run_waits.id AS run_wait_id,
       waitpoints.org_id,
       run_waits.run_id,
       run_waits.execution_id,
       run_waits.checkpoint_id,
       run_waits.correlation_id,
       waitpoints.kind,
       waitpoints.request,
       waitpoints.display_text,
       run_waits.timeout_seconds,
       run_waits.policy_name,
       run_waits.policy_snapshot,
       run_waits.status,
       run_waits.resolution_kind,
       run_waits.resolution,
       waitpoints.created_at,
       run_waits.waiting_at AS requested_at,
       run_waits.resolved_at
  FROM run_waits
  JOIN run_wait_dependencies ON run_wait_dependencies.org_id = run_waits.org_id
                            AND run_wait_dependencies.run_wait_id = run_waits.id
  JOIN waitpoints ON waitpoints.org_id = run_wait_dependencies.org_id
                 AND waitpoints.id = run_wait_dependencies.waitpoint_id
 WHERE run_waits.org_id = sqlc.arg(org_id)
   AND run_waits.run_id = sqlc.arg(run_id)
   AND run_waits.status = 'waiting'
   AND waitpoints.status = 'pending'
 ORDER BY run_waits.waiting_at DESC, run_wait_dependencies.ordinal ASC
 LIMIT 1;

-- name: GetWaitpointForRespond :one
SELECT *
 FROM waitpoints
 WHERE waitpoints.org_id = sqlc.arg(org_id)
       AND waitpoints.id = sqlc.arg(waitpoint_id)
       AND waitpoints.status IN ('pending', 'completed')
       AND (waitpoints.expires_at IS NULL OR waitpoints.expires_at > now());

-- name: ResolveWaitpoint :one
WITH target_waitpoint AS (
    SELECT *
      FROM waitpoints
     WHERE waitpoints.org_id = sqlc.arg(org_id)
       AND waitpoints.id = sqlc.arg(id)
       AND waitpoints.kind = sqlc.arg(kind)
       AND waitpoints.status IN ('pending', 'completed')
       AND (waitpoints.expires_at IS NULL OR waitpoints.expires_at > now())
     FOR UPDATE
),
completed_waitpoint AS (
    UPDATE waitpoints
       SET status = 'completed',
           resolution_kind = sqlc.arg(resolution_kind),
           output = sqlc.arg(output),
           resolution = sqlc.arg(resolution),
           completed_at = now(),
           updated_at = now()
      FROM target_waitpoint
     WHERE waitpoints.org_id = target_waitpoint.org_id
       AND waitpoints.id = target_waitpoint.id
       AND waitpoints.status = 'pending'
    RETURNING waitpoints.*
),
selected_waitpoint AS (
    SELECT * FROM completed_waitpoint
    UNION ALL
    SELECT target_waitpoint.*
      FROM target_waitpoint
     WHERE target_waitpoint.status = 'completed'
       AND NOT EXISTS (SELECT 1 FROM completed_waitpoint)
)
SELECT * FROM selected_waitpoint
LIMIT 1;

-- name: UnblockRunWaitsForWaitpoint :many
WITH target_waitpoint AS (
    SELECT *
      FROM waitpoints
     WHERE waitpoints.org_id = sqlc.arg(org_id)
       AND waitpoints.id = sqlc.arg(waitpoint_id)
       AND waitpoints.status = 'completed'
     FOR UPDATE
),
eligible_run_waits AS (
    SELECT run_waits.*,
           CASE
               WHEN dependency_state.dependency_count = 1 THEN dependency_state.first_resolution_kind
               ELSE 'waitpoints'
           END AS resume_kind,
           CASE
               WHEN dependency_state.dependency_count = 1 THEN dependency_state.first_resolution
               ELSE jsonb_build_object('waitpoints', COALESCE(dependency_state.resolution_by_waitpoint, '{}'::jsonb))
           END AS resume_output
      FROM target_waitpoint
      JOIN run_wait_dependencies target_dependency
        ON target_dependency.org_id = target_waitpoint.org_id
       AND target_dependency.waitpoint_id = target_waitpoint.id
      JOIN run_waits ON run_waits.org_id = target_dependency.org_id
                    AND run_waits.id = target_dependency.run_wait_id
      JOIN runs ON runs.org_id = run_waits.org_id
               AND runs.id = run_waits.run_id
      JOIN LATERAL (
          SELECT count(*)::int AS dependency_count,
                 count(*) FILTER (
                    WHERE dependency_waitpoints.status = 'completed'
                       OR run_wait_dependencies.waitpoint_id = target_waitpoint.id
                 )::int AS completed_count,
                 (array_agg(
                    CASE
                        WHEN run_wait_dependencies.waitpoint_id = target_waitpoint.id THEN target_waitpoint.resolution_kind
                        ELSE dependency_waitpoints.resolution_kind
                    END
                    ORDER BY run_wait_dependencies.ordinal
                  ) FILTER (
                    WHERE dependency_waitpoints.status = 'completed'
                       OR run_wait_dependencies.waitpoint_id = target_waitpoint.id
                  ))[1] AS first_resolution_kind,
                 (array_agg(
                    CASE
                        WHEN run_wait_dependencies.waitpoint_id = target_waitpoint.id THEN target_waitpoint.resolution
                        ELSE dependency_waitpoints.resolution
                    END
                    ORDER BY run_wait_dependencies.ordinal
                  ) FILTER (
                    WHERE dependency_waitpoints.status = 'completed'
                       OR run_wait_dependencies.waitpoint_id = target_waitpoint.id
                  ))[1] AS first_resolution,
                 jsonb_object_agg(
                    run_wait_dependencies.waitpoint_id::text,
                    CASE
                        WHEN run_wait_dependencies.waitpoint_id = target_waitpoint.id THEN target_waitpoint.resolution
                        ELSE dependency_waitpoints.resolution
                    END
                    ORDER BY run_wait_dependencies.ordinal
                 ) FILTER (
                    WHERE dependency_waitpoints.status = 'completed'
                       OR run_wait_dependencies.waitpoint_id = target_waitpoint.id
                 ) AS resolution_by_waitpoint
            FROM run_wait_dependencies
            JOIN waitpoints dependency_waitpoints
              ON dependency_waitpoints.org_id = run_wait_dependencies.org_id
             AND dependency_waitpoints.id = run_wait_dependencies.waitpoint_id
           WHERE run_wait_dependencies.org_id = run_waits.org_id
             AND run_wait_dependencies.run_wait_id = run_waits.id
      ) dependency_state ON true
     WHERE run_waits.status = 'waiting'
       AND runs.status = 'waiting'
       AND runs.current_execution_id IS NULL
       AND EXISTS (
           SELECT 1
             FROM run_queue_items
            WHERE run_queue_items.org_id = run_waits.org_id
              AND run_queue_items.run_id = run_waits.run_id
              AND run_queue_items.status = 'suspended'
       )
       AND NOT EXISTS (
           SELECT 1
             FROM run_wait_dependencies remaining
             JOIN waitpoints remaining_waitpoints
               ON remaining_waitpoints.org_id = remaining.org_id
              AND remaining_waitpoints.id = remaining.waitpoint_id
            WHERE remaining.org_id = run_waits.org_id
              AND remaining.run_wait_id = run_waits.id
              AND remaining.waitpoint_id <> target_waitpoint.id
              AND remaining_waitpoints.status <> 'completed'
       )
),
resuming_run_waits AS (
    UPDATE run_waits
       SET status = 'resuming',
           resolution_kind = eligible_run_waits.resume_kind,
           resolution = eligible_run_waits.resume_output,
           resolved_at = now(),
           updated_at = now()
      FROM eligible_run_waits
     WHERE run_waits.org_id = eligible_run_waits.org_id
       AND run_waits.id = eligible_run_waits.id
       AND run_waits.status = 'waiting'
    RETURNING run_waits.*
),
updated_runs AS (
    UPDATE runs
       SET status = 'queued',
           updated_at = now()
      FROM resuming_run_waits
     WHERE runs.org_id = resuming_run_waits.org_id
       AND runs.id = resuming_run_waits.run_id
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
),
event AS (
    INSERT INTO run_events (org_id, run_id, execution_id, attempt_number, kind, payload)
    SELECT resuming_run_waits.org_id,
           resuming_run_waits.run_id,
           run_executions.id,
           run_executions.attempt_number,
           'waitpoint.resolved',
           jsonb_build_object(
               'run_id', resuming_run_waits.run_id,
               'waitpoint_id', sqlc.arg(waitpoint_id),
               'kind', target_waitpoint.kind,
               'resolution_kind', resuming_run_waits.resolution_kind,
               'result', resuming_run_waits.resolution
           )
      FROM resuming_run_waits
      JOIN target_waitpoint ON true
      JOIN updated_runs ON updated_runs.org_id = resuming_run_waits.org_id
                       AND updated_runs.id = resuming_run_waits.run_id
      LEFT JOIN run_executions ON run_executions.org_id = resuming_run_waits.org_id
                              AND run_executions.run_id = resuming_run_waits.run_id
                              AND run_executions.id = resuming_run_waits.execution_id
      JOIN continuation_queue_entries ON continuation_queue_entries.org_id = resuming_run_waits.org_id
                                     AND continuation_queue_entries.run_id = resuming_run_waits.run_id
    RETURNING id
)
SELECT waitpoints.id,
       resuming_run_waits.id AS run_wait_id,
       waitpoints.org_id,
       resuming_run_waits.run_id,
       resuming_run_waits.execution_id,
       resuming_run_waits.checkpoint_id,
       resuming_run_waits.correlation_id,
       waitpoints.kind,
       waitpoints.request,
       waitpoints.display_text,
       resuming_run_waits.timeout_seconds,
       resuming_run_waits.policy_name,
       resuming_run_waits.policy_snapshot,
       resuming_run_waits.status,
       resuming_run_waits.resolution_kind,
       resuming_run_waits.resolution,
       waitpoints.created_at,
       resuming_run_waits.waiting_at AS requested_at,
       resuming_run_waits.resolved_at
  FROM resuming_run_waits
  JOIN run_wait_dependencies ON run_wait_dependencies.org_id = resuming_run_waits.org_id
                            AND run_wait_dependencies.run_wait_id = resuming_run_waits.id
  JOIN waitpoints ON waitpoints.org_id = run_wait_dependencies.org_id
                 AND waitpoints.id = run_wait_dependencies.waitpoint_id
  JOIN event ON true
 WHERE waitpoints.id = sqlc.arg(waitpoint_id);

-- name: ExpireDuePendingWaitpoints :exec
WITH current_run_waits AS (
    SELECT run_waits.*
      FROM run_waits
      JOIN runs ON runs.org_id = run_waits.org_id
               AND runs.id = run_waits.run_id
     WHERE run_waits.org_id = sqlc.arg(org_id)
       AND run_waits.status = 'waiting'
       AND run_waits.timeout_seconds IS NOT NULL
       AND run_waits.waiting_at + (run_waits.timeout_seconds * interval '1 second') <= now()
       AND runs.status = 'waiting'
       AND runs.current_execution_id IS NULL
       AND EXISTS (
           SELECT 1
             FROM run_queue_items
            WHERE run_queue_items.org_id = run_waits.org_id
              AND run_queue_items.run_id = run_waits.run_id
              AND run_queue_items.status = 'suspended'
       )
     FOR UPDATE OF run_waits
),
expired_waitpoints AS (
    UPDATE waitpoints
       SET status = 'expired',
           resolution_kind = 'timed_out',
           output = 'null'::jsonb,
           resolution = jsonb_build_object('at', now()),
           output_is_error = true,
           completed_at = now(),
           updated_at = now()
      FROM current_run_waits
      JOIN run_wait_dependencies ON run_wait_dependencies.org_id = current_run_waits.org_id
                                AND run_wait_dependencies.run_wait_id = current_run_waits.id
     WHERE waitpoints.org_id = run_wait_dependencies.org_id
       AND waitpoints.id = run_wait_dependencies.waitpoint_id
       AND waitpoints.status = 'pending'
    RETURNING waitpoints.*
),
expired_run_waits AS (
    UPDATE run_waits
       SET status = 'resuming',
           resolution_kind = 'timed_out',
           resolution = jsonb_build_object('at', now()),
           resolved_at = now(),
           updated_at = now()
      FROM current_run_waits
     WHERE run_waits.org_id = current_run_waits.org_id
       AND run_waits.id = current_run_waits.id
       AND run_waits.status = 'waiting'
    RETURNING run_waits.*
),
updated_runs AS (
    UPDATE runs
       SET status = 'queued',
           updated_at = now()
      FROM expired_run_waits
     WHERE runs.org_id = expired_run_waits.org_id
       AND runs.id = expired_run_waits.run_id
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
INSERT INTO run_events (org_id, run_id, execution_id, attempt_number, kind, payload)
SELECT expired_run_waits.org_id,
       expired_run_waits.run_id,
       run_executions.id,
       run_executions.attempt_number,
       'waitpoint.resolved',
       jsonb_build_object(
           'run_id', expired_run_waits.run_id,
           'waitpoint_id', expired_waitpoints.id,
           'kind', expired_waitpoints.kind,
           'resolution_kind', 'timed_out'
       )
  FROM expired_run_waits
  LEFT JOIN run_executions ON run_executions.org_id = expired_run_waits.org_id
                          AND run_executions.run_id = expired_run_waits.run_id
                          AND run_executions.id = expired_run_waits.execution_id
  JOIN run_wait_dependencies ON run_wait_dependencies.org_id = expired_run_waits.org_id
                            AND run_wait_dependencies.run_wait_id = expired_run_waits.id
  JOIN expired_waitpoints ON expired_waitpoints.org_id = run_wait_dependencies.org_id
                         AND expired_waitpoints.id = run_wait_dependencies.waitpoint_id
  JOIN updated_runs ON updated_runs.org_id = expired_run_waits.org_id
                   AND updated_runs.id = expired_run_waits.run_id
  JOIN continuation_queue_entries ON continuation_queue_entries.org_id = expired_run_waits.org_id
                                 AND continuation_queue_entries.run_id = expired_run_waits.run_id;
