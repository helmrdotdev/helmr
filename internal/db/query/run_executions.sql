-- name: RequeueExpiredLeasedRunExecutions :exec
WITH eligible AS (
    SELECT runs.id AS run_id,
           run_executions.id AS execution_id,
           run_executions.restore_checkpoint_id
      FROM runs
      JOIN run_executions ON run_executions.id = runs.current_execution_id
                          AND run_executions.org_id = runs.org_id
                          AND run_executions.run_id = runs.id
     WHERE runs.org_id = $1
       AND runs.status = 'running'
       AND run_executions.status = 'leased'
       AND run_executions.lease_expires_at <= now()
     FOR UPDATE OF runs, run_executions
),
updated_runs AS (
    UPDATE runs
       SET status = 'queued',
           current_execution_id = NULL,
           updated_at = now()
      FROM eligible
     WHERE runs.id = eligible.run_id
       AND runs.status = 'running'
       AND runs.current_execution_id = eligible.execution_id
     RETURNING eligible.run_id, eligible.execution_id, eligible.restore_checkpoint_id, runs.queued_expires_at
),
restored_checkpoint AS (
    UPDATE checkpoints
       SET status = 'ready',
           error_message = NULL,
           invalidated_at = NULL
      FROM updated_runs
     WHERE checkpoints.run_id = updated_runs.run_id
       AND checkpoints.id = updated_runs.restore_checkpoint_id
       AND checkpoints.status = 'restoring'
    RETURNING checkpoints.id
),
requeued_queue_entries AS (
    UPDATE run_queue_items
       SET status = 'queued',
           dispatch_message_id = NULL,
           reserved_by_worker_instance_id = NULL,
           reservation_expires_at = NULL,
           queued_expires_at = updated_runs.queued_expires_at,
           dispatch_generation = dispatch_generation + 1,
           last_error = 'worker lease expired before execution started',
           enqueued_at = now(),
           updated_at = now(),
           finished_at = NULL
      FROM updated_runs
      JOIN run_executions ON run_executions.org_id = $1
                         AND run_executions.run_id = updated_runs.run_id
                         AND run_executions.id = updated_runs.execution_id
     WHERE run_queue_items.org_id = $1
       AND run_queue_items.run_id = updated_runs.run_id
       AND run_queue_items.reserved_by_worker_instance_id = run_executions.worker_instance_id
       AND run_queue_items.dispatch_message_id = run_executions.dispatch_message_id
       AND run_queue_items.status = 'reserved'
    RETURNING run_queue_items.run_id
),
released_concurrency_slots AS (
    UPDATE run_concurrency_slots
       SET released_at = now()
      FROM updated_runs
     WHERE run_concurrency_slots.org_id = $1
       AND run_concurrency_slots.run_id = updated_runs.run_id
       AND run_concurrency_slots.execution_id = updated_runs.execution_id
       AND run_concurrency_slots.released_at IS NULL
    RETURNING run_concurrency_slots.id
),
cleanup AS (
    SELECT
        (SELECT count(*) FROM restored_checkpoint) AS restored_checkpoint_count,
        (SELECT count(*) FROM requeued_queue_entries) AS requeued_queue_entry_count,
        (SELECT count(*) FROM released_concurrency_slots) AS released_concurrency_slot_count
)
UPDATE run_executions
   SET lost_at = COALESCE(lost_at, now()),
       renewed_at = now(),
       status = 'lost'
  FROM updated_runs
 WHERE run_executions.id = updated_runs.execution_id
   AND run_executions.run_id = updated_runs.run_id
   AND (SELECT restored_checkpoint_count + requeued_queue_entry_count + released_concurrency_slot_count FROM cleanup) >= 0;

-- name: AbandonLeasedRunExecution :exec
WITH abandoned AS (
    UPDATE runs
       SET status = 'queued',
           current_execution_id = NULL,
           updated_at = now()
     WHERE runs.org_id = sqlc.arg(org_id)
       AND runs.id = sqlc.arg(run_id)
       AND runs.status = 'running'
       AND runs.current_execution_id = sqlc.arg(execution_id)
       AND EXISTS (
           SELECT 1
             FROM run_executions
            WHERE run_executions.org_id = sqlc.arg(org_id)
              AND run_executions.run_id = sqlc.arg(run_id)
              AND run_executions.id = sqlc.arg(execution_id)
              AND run_executions.worker_instance_id = sqlc.arg(worker_instance_id)
              AND run_executions.status = 'leased'
       )
    RETURNING runs.id
),
restored_checkpoint AS (
    UPDATE checkpoints
       SET status = 'ready',
           error_message = NULL,
           invalidated_at = NULL
      FROM abandoned
      JOIN run_executions ON run_executions.org_id = sqlc.arg(org_id)
                         AND run_executions.run_id = abandoned.id
                         AND run_executions.id = sqlc.arg(execution_id)
                         AND run_executions.worker_instance_id = sqlc.arg(worker_instance_id)
     WHERE checkpoints.org_id = sqlc.arg(org_id)
       AND checkpoints.run_id = abandoned.id
       AND checkpoints.id = run_executions.restore_checkpoint_id
       AND checkpoints.status = 'restoring'
    RETURNING checkpoints.id
),
released_concurrency_slots AS (
    UPDATE run_concurrency_slots
       SET released_at = now()
      FROM abandoned
     WHERE run_concurrency_slots.org_id = sqlc.arg(org_id)
       AND run_concurrency_slots.run_id = abandoned.id
       AND run_concurrency_slots.execution_id = sqlc.arg(execution_id)
       AND run_concurrency_slots.released_at IS NULL
    RETURNING run_concurrency_slots.id
),
cleanup AS (
    SELECT
        (SELECT count(*) FROM restored_checkpoint) AS restored_checkpoint_count,
        (SELECT count(*) FROM released_concurrency_slots) AS released_concurrency_slot_count
)
UPDATE run_executions
   SET lost_at = COALESCE(lost_at, now()),
       renewed_at = now(),
       status = 'lost'
  FROM abandoned
 WHERE run_executions.org_id = sqlc.arg(org_id)
   AND run_executions.run_id = abandoned.id
   AND run_executions.id = sqlc.arg(execution_id)
   AND run_executions.worker_instance_id = sqlc.arg(worker_instance_id)
   AND run_executions.status = 'leased'
   AND (SELECT restored_checkpoint_count + released_concurrency_slot_count FROM cleanup) >= 0;

-- name: FailExpiredRunningRunExecutions :exec
WITH eligible AS (
    SELECT runs.id AS run_id,
           run_executions.id AS execution_id,
           run_executions.restore_checkpoint_id
      FROM runs
      JOIN run_executions ON run_executions.id = runs.current_execution_id
                          AND run_executions.org_id = runs.org_id
                          AND run_executions.run_id = runs.id
     WHERE runs.org_id = $1
       AND runs.status = 'running'
       AND run_executions.status = 'running'
       AND run_executions.lease_expires_at <= now()
     FOR UPDATE OF runs, run_executions
),
updated_runs AS (
    UPDATE runs
       SET status = 'failed',
           current_execution_id = NULL,
           error_message = 'worker lease expired',
           finished_at = COALESCE(finished_at, now()),
           updated_at = now()
      FROM eligible
     WHERE runs.id = eligible.run_id
       AND runs.status = 'running'
       AND runs.current_execution_id = eligible.execution_id
     RETURNING eligible.run_id, eligible.execution_id, eligible.restore_checkpoint_id
),
cancelled_run_waits AS (
    UPDATE run_waits
       SET status = 'cancelled',
           failure = jsonb_build_object('reason', 'worker lease expired', 'source', 'lease_sweeper'),
           resolution_kind = 'cancelled',
           resolution = jsonb_build_object('reason', 'worker lease expired', 'source', 'lease_sweeper'),
           failed_at = now(),
           updated_at = now()
      FROM updated_runs
     WHERE run_waits.org_id = $1
       AND run_waits.run_id = updated_runs.run_id
       AND run_waits.execution_id = updated_runs.execution_id
       AND run_waits.status IN ('opening', 'waiting', 'resuming')
    RETURNING run_waits.id, run_waits.org_id
),
cancelled_waitpoints AS (
    UPDATE waitpoints
       SET status = 'cancelled',
           resolution_kind = 'cancelled',
           output = 'null'::jsonb,
           resolution = jsonb_build_object('reason', 'worker lease expired', 'source', 'lease_sweeper'),
           output_is_error = true,
           completed_at = now(),
           updated_at = now()
      FROM cancelled_run_waits
      JOIN run_wait_dependencies ON run_wait_dependencies.org_id = cancelled_run_waits.org_id
                                AND run_wait_dependencies.run_wait_id = cancelled_run_waits.id
     WHERE waitpoints.org_id = run_wait_dependencies.org_id
       AND waitpoints.id = run_wait_dependencies.waitpoint_id
       AND waitpoints.status = 'pending'
    RETURNING waitpoints.id
),
invalidated_checkpoints AS (
    UPDATE checkpoints
       SET status = 'invalid',
           error_message = 'worker lease expired',
           invalidated_at = now()
      FROM updated_runs
     WHERE checkpoints.run_id = updated_runs.run_id
       AND checkpoints.execution_id = updated_runs.execution_id
       AND checkpoints.status IN ('creating', 'restoring')
    RETURNING checkpoints.run_id, checkpoints.id
),
failed_restore_checkpoints AS (
    UPDATE checkpoints
       SET status = 'invalid',
           error_message = 'worker lease expired',
           invalidated_at = now()
      FROM updated_runs
     WHERE checkpoints.run_id = updated_runs.run_id
       AND checkpoints.id = updated_runs.restore_checkpoint_id
       AND checkpoints.status = 'restoring'
    RETURNING checkpoints.run_id, checkpoints.id
),
completed_queue_entries AS (
    UPDATE run_queue_items
       SET status = 'completed',
           dispatch_generation = dispatch_generation + 1,
           updated_at = now(),
           finished_at = now()
      FROM updated_runs
      JOIN run_executions ON run_executions.org_id = $1
                         AND run_executions.run_id = updated_runs.run_id
                         AND run_executions.id = updated_runs.execution_id
     WHERE run_queue_items.org_id = $1
       AND run_queue_items.run_id = updated_runs.run_id
       AND run_queue_items.reserved_by_worker_instance_id = run_executions.worker_instance_id
       AND run_queue_items.dispatch_message_id = run_executions.dispatch_message_id
       AND run_queue_items.status = 'reserved'
    RETURNING run_queue_items.run_id
),
released_concurrency_slots AS (
    UPDATE run_concurrency_slots
       SET released_at = now()
      FROM updated_runs
     WHERE run_concurrency_slots.org_id = $1
       AND run_concurrency_slots.run_id = updated_runs.run_id
       AND run_concurrency_slots.execution_id = updated_runs.execution_id
       AND run_concurrency_slots.released_at IS NULL
    RETURNING run_concurrency_slots.id
),
terminal_events AS (
    INSERT INTO run_events (org_id, run_id, kind, payload)
    SELECT $1,
           updated_runs.run_id,
           'run.failed',
           jsonb_build_object(
               'failure_kind', 'worker_lease_expired',
               'detail', jsonb_build_object('message', 'worker lease expired')
           )
      FROM updated_runs
    RETURNING id
),
cleanup AS (
    SELECT
        (SELECT count(*) FROM cancelled_waitpoints) AS cancelled_waitpoints,
        (SELECT count(*) FROM invalidated_checkpoints) AS invalidated_checkpoints,
        (SELECT count(*) FROM failed_restore_checkpoints) AS failed_restore_checkpoints,
        (SELECT count(*) FROM completed_queue_entries) AS completed_queue_entries,
        (SELECT count(*) FROM released_concurrency_slots) AS released_concurrency_slots,
        (SELECT count(*) FROM terminal_events) AS terminal_events
)
UPDATE run_executions
   SET lost_at = COALESCE(lost_at, now()),
       renewed_at = now(),
       status = 'lost'
 FROM updated_runs
 WHERE run_executions.id = updated_runs.execution_id
   AND run_executions.run_id = updated_runs.run_id
   AND (SELECT cancelled_waitpoints + invalidated_checkpoints + failed_restore_checkpoints + completed_queue_entries + released_concurrency_slots + terminal_events FROM cleanup) >= 0;

-- name: LeaseRunExecution :one
WITH
locked_dispatch AS MATERIALIZED (
    SELECT run_queue_items.run_id,
           run_queue_items.org_id,
           run_queue_items.reserved_by_worker_instance_id,
           run_queue_items.dispatch_message_id
      FROM run_queue_items
     WHERE run_queue_items.org_id = sqlc.arg(org_id)
       AND run_queue_items.run_id = sqlc.arg(run_id)
       AND run_queue_items.reserved_by_worker_instance_id = sqlc.arg(worker_instance_id)
       AND run_queue_items.dispatch_message_id = sqlc.arg(dispatch_message_id)
       AND run_queue_items.status = 'reserved'
       AND run_queue_items.reservation_expires_at > now()
     FOR UPDATE OF run_queue_items
),
locked_worker_instance AS MATERIALIZED (
    SELECT worker_instances.*
      FROM worker_instances
      JOIN locked_dispatch ON locked_dispatch.reserved_by_worker_instance_id = worker_instances.id
     WHERE worker_instances.status = 'active'
     FOR UPDATE OF worker_instances
),
dispatch AS (
    SELECT locked_dispatch.run_id,
           locked_dispatch.reserved_by_worker_instance_id AS worker_instance_id,
           locked_dispatch.dispatch_message_id,
           worker_instances.available_milli_cpu,
           worker_instances.available_memory_mib,
           worker_instances.available_disk_mib,
           worker_instances.available_execution_slots,
           worker_instances.total_milli_cpu,
           worker_instances.total_memory_mib,
           worker_instances.total_disk_mib,
           worker_instances.region,
           worker_instances.labels,
           worker_instances.heartbeat->>'runtime_arch' AS runtime_arch,
           worker_instances.heartbeat->>'runtime_abi' AS runtime_abi,
           worker_instances.heartbeat->>'kernel_digest' AS kernel_digest,
           worker_instances.heartbeat->>'rootfs_digest' AS rootfs_digest,
           worker_instances.heartbeat->>'cni_profile' AS cni_profile,
           active.used_milli_cpu,
           active.used_memory_mib,
           active.used_disk_mib,
           active.used_slots
      FROM locked_dispatch
      JOIN locked_worker_instance AS worker_instances ON worker_instances.id = locked_dispatch.reserved_by_worker_instance_id
      LEFT JOIN LATERAL (
          SELECT COALESCE(sum(run_runtime_requirements.requested_milli_cpu), 0)::bigint AS used_milli_cpu,
                 COALESCE(sum(run_runtime_requirements.requested_memory_mib), 0)::bigint AS used_memory_mib,
                 COALESCE(sum(run_runtime_requirements.requested_disk_mib), 0)::bigint AS used_disk_mib,
                 COALESCE(sum(run_runtime_requirements.requested_execution_slots), 0)::int AS used_slots
            FROM run_executions
            JOIN run_runtime_requirements ON run_runtime_requirements.org_id = run_executions.org_id
                                 AND run_runtime_requirements.run_id = run_executions.run_id
           WHERE run_executions.worker_instance_id = worker_instances.id
             AND run_executions.status IN ('leased', 'running')
      ) active ON true
),
candidate AS (
    SELECT runs.id,
           runs.project_id,
           runs.environment_id,
           runs.latest_checkpoint_id,
           runs.queue_name,
           runs.queue_concurrency_limit,
           runs.concurrency_key
      FROM runs
      JOIN dispatch ON dispatch.run_id = runs.id
      JOIN run_runtime_requirements ON run_runtime_requirements.org_id = runs.org_id
                                    AND run_runtime_requirements.run_id = runs.id
      JOIN LATERAL (
          SELECT COALESCE(NULLIF(run_runtime_requirements.placement->>'region', ''), NULLIF(run_runtime_requirements.placement->>'Region', ''), '') AS placement_region,
                 COALESCE(run_runtime_requirements.placement->'tags', run_runtime_requirements.placement->'Tags') AS placement_tags,
                 COALESCE(NULLIF(run_runtime_requirements.placement->>'dedicated_key', ''), NULLIF(run_runtime_requirements.placement->>'DedicatedKey', ''), '') AS dedicated_key,
                 COALESCE(NULLIF(run_runtime_requirements.placement->>'snapshot_key', ''), NULLIF(run_runtime_requirements.placement->>'SnapshotKey', ''), '') AS snapshot_key
      ) placement ON true
     WHERE runs.org_id = sqlc.arg(org_id)
       AND runs.id = sqlc.arg(run_id)
       AND runs.status = 'queued'
       AND runs.current_execution_id IS NULL
       AND (runs.queued_expires_at IS NULL OR runs.queued_expires_at > now())
       AND run_runtime_requirements.requested_milli_cpu <= GREATEST(dispatch.available_milli_cpu - dispatch.used_milli_cpu, 0)
       AND run_runtime_requirements.requested_memory_mib <= GREATEST(dispatch.available_memory_mib - dispatch.used_memory_mib, 0)
       AND run_runtime_requirements.requested_disk_mib <= GREATEST(dispatch.available_disk_mib - dispatch.used_disk_mib, 0)
       AND run_runtime_requirements.requested_execution_slots <= GREATEST(dispatch.available_execution_slots - dispatch.used_slots, 0)
       AND (run_runtime_requirements.runtime_arch = '' OR run_runtime_requirements.runtime_arch = dispatch.runtime_arch)
       AND (run_runtime_requirements.runtime_abi = '' OR run_runtime_requirements.runtime_abi = dispatch.runtime_abi)
       AND (run_runtime_requirements.kernel_digest = '' OR run_runtime_requirements.kernel_digest = dispatch.kernel_digest)
       AND (run_runtime_requirements.rootfs_digest = '' OR run_runtime_requirements.rootfs_digest = dispatch.rootfs_digest)
       AND (run_runtime_requirements.cni_profile = '' OR run_runtime_requirements.cni_profile = dispatch.cni_profile)
       AND (placement.placement_region = '' OR placement.placement_region = dispatch.region)
       AND (
           placement.placement_tags IS NULL
           OR placement.placement_tags = 'null'::jsonb
           OR (
               jsonb_typeof(placement.placement_tags) = 'object'
               AND dispatch.labels @> placement.placement_tags
           )
       )
       AND (placement.dedicated_key = '' OR dispatch.labels->>'dedicated_key' = placement.dedicated_key)
       AND (placement.snapshot_key = '' OR dispatch.labels->>'snapshot_key' = placement.snapshot_key)
       AND (
           runs.latest_checkpoint_id IS NULL
           OR EXISTS (
               SELECT 1
                 FROM checkpoints
                 JOIN checkpoint_runtime_snapshots
                   ON checkpoint_runtime_snapshots.org_id = checkpoints.org_id
                  AND checkpoint_runtime_snapshots.run_id = checkpoints.run_id
                  AND checkpoint_runtime_snapshots.checkpoint_id = checkpoints.id
                 JOIN run_waits ON run_waits.org_id = sqlc.arg(org_id)
                               AND run_waits.run_id = runs.id
                               AND run_waits.checkpoint_id = checkpoints.id
                 JOIN run_wait_dependencies ON run_wait_dependencies.org_id = run_waits.org_id
                                           AND run_wait_dependencies.run_wait_id = run_waits.id
                 JOIN waitpoints ON waitpoints.org_id = run_wait_dependencies.org_id
                                AND waitpoints.id = run_wait_dependencies.waitpoint_id
                WHERE checkpoints.org_id = sqlc.arg(org_id)
                  AND checkpoints.run_id = runs.id
                  AND checkpoints.id = runs.latest_checkpoint_id
                  AND checkpoints.status = 'ready'
                  AND run_waits.status = 'resuming'
                  AND run_waits.resolution_kind IS NOT NULL
                  AND (checkpoint_runtime_snapshots.runtime_arch IS NULL OR checkpoint_runtime_snapshots.runtime_arch = dispatch.runtime_arch)
                  AND (checkpoint_runtime_snapshots.runtime_abi IS NULL OR checkpoint_runtime_snapshots.runtime_abi = dispatch.runtime_abi)
                  AND (checkpoint_runtime_snapshots.kernel_digest IS NULL OR checkpoint_runtime_snapshots.kernel_digest = dispatch.kernel_digest)
                  AND (checkpoint_runtime_snapshots.rootfs_digest IS NULL OR checkpoint_runtime_snapshots.rootfs_digest = dispatch.rootfs_digest)
                  AND (checkpoint_runtime_snapshots.runtime_vcpus IS NULL OR checkpoint_runtime_snapshots.runtime_vcpus = ((dispatch.total_milli_cpu + 999) / 1000))
                  AND (checkpoint_runtime_snapshots.runtime_memory_mib IS NULL OR checkpoint_runtime_snapshots.runtime_memory_mib = dispatch.total_memory_mib)
                  AND (checkpoint_runtime_snapshots.runtime_scratch_disk_mib IS NULL OR checkpoint_runtime_snapshots.runtime_scratch_disk_mib = dispatch.total_disk_mib)
                  AND (checkpoint_runtime_snapshots.cni_profile IS NULL OR checkpoint_runtime_snapshots.cni_profile = dispatch.cni_profile)
           )
       )
     FOR UPDATE OF runs
),
concurrency_scope_lock AS MATERIALIZED (
    SELECT candidate.id AS run_id,
           true AS locked
      FROM candidate
      CROSS JOIN LATERAL (
          SELECT pg_advisory_xact_lock(
                     hashtext(sqlc.arg(org_id)::text || ':' || candidate.environment_id::text),
                     hashtext(candidate.queue_name || ':' || COALESCE(candidate.concurrency_key, ''))
                 )
      ) lock
     WHERE candidate.queue_concurrency_limit IS NOT NULL
),
concurrency_capacity AS (
    SELECT candidate.*
      FROM candidate
      LEFT JOIN concurrency_scope_lock ON concurrency_scope_lock.run_id = candidate.id
     WHERE candidate.queue_concurrency_limit IS NULL
        OR (
            concurrency_scope_lock.locked
            AND (
                SELECT count(*)::int
                  FROM run_concurrency_slots
                 WHERE run_concurrency_slots.org_id = sqlc.arg(org_id)
                   AND run_concurrency_slots.environment_id = candidate.environment_id
                   AND run_concurrency_slots.queue_name = candidate.queue_name
                   AND COALESCE(run_concurrency_slots.concurrency_key, '') = COALESCE(candidate.concurrency_key, '')
                   AND run_concurrency_slots.released_at IS NULL
            ) < candidate.queue_concurrency_limit
        )
),
restore_checkpoint AS (
    SELECT checkpoints.id
      FROM concurrency_capacity
      JOIN checkpoints ON checkpoints.org_id = sqlc.arg(org_id)
                      AND checkpoints.run_id = concurrency_capacity.id
                      AND checkpoints.id = concurrency_capacity.latest_checkpoint_id
      JOIN run_waits ON run_waits.org_id = sqlc.arg(org_id)
                    AND run_waits.run_id = concurrency_capacity.id
                    AND run_waits.checkpoint_id = checkpoints.id
      JOIN run_wait_dependencies ON run_wait_dependencies.org_id = run_waits.org_id
                                AND run_wait_dependencies.run_wait_id = run_waits.id
      JOIN waitpoints ON waitpoints.org_id = run_wait_dependencies.org_id
                     AND waitpoints.id = run_wait_dependencies.waitpoint_id
     WHERE checkpoints.status = 'ready'
       AND run_waits.status = 'resuming'
       AND run_waits.resolution_kind IS NOT NULL
     ORDER BY run_waits.resolved_at DESC
     LIMIT 1
),
execution AS (
    INSERT INTO run_executions (
        id,
        org_id,
        run_id,
        worker_instance_id,
        dispatch_message_id,
        dispatch_lease_id,
        dispatch_attempt,
        status,
        lease_expires_at,
        restore_checkpoint_id
    )
    SELECT sqlc.arg(execution_id),
           sqlc.arg(org_id),
           candidate.id,
           sqlc.arg(worker_instance_id),
           sqlc.arg(dispatch_message_id),
           sqlc.arg(dispatch_lease_id),
           sqlc.arg(dispatch_attempt),
           'leased',
           sqlc.arg(lease_expires_at),
           (SELECT id FROM restore_checkpoint)
      FROM concurrency_capacity AS candidate
    RETURNING id, worker_instance_id, dispatch_message_id, dispatch_lease_id, dispatch_attempt, lease_expires_at, restore_checkpoint_id
),
concurrency_slot AS (
    INSERT INTO run_concurrency_slots (
        org_id,
        project_id,
        environment_id,
        run_id,
        execution_id,
        queue_name,
        concurrency_key
    )
    SELECT sqlc.arg(org_id),
           concurrency_capacity.project_id,
           concurrency_capacity.environment_id,
           concurrency_capacity.id,
           execution.id,
           concurrency_capacity.queue_name,
           concurrency_capacity.concurrency_key
      FROM concurrency_capacity
      JOIN execution ON execution.id = sqlc.arg(execution_id)
     WHERE concurrency_capacity.queue_concurrency_limit IS NOT NULL
    RETURNING id
),
active_time AS (
    SELECT COALESCE(MAX(run_executions.active_duration_ms), 0)::bigint AS active_duration_ms
      FROM concurrency_capacity
      LEFT JOIN run_executions ON run_executions.org_id = sqlc.arg(org_id)
                              AND run_executions.run_id = concurrency_capacity.id
                              AND run_executions.status IN ('detached', 'released')
),
marked_restore_checkpoint AS (
    UPDATE checkpoints
       SET status = 'restoring',
           error_message = NULL,
           invalidated_at = NULL
      FROM restore_checkpoint
     WHERE checkpoints.org_id = sqlc.arg(org_id)
       AND checkpoints.id = restore_checkpoint.id
       AND checkpoints.status = 'ready'
    RETURNING checkpoints.id
),
updated AS (
    UPDATE runs
       SET status = 'running',
           current_execution_id = (SELECT id FROM execution),
           updated_at = now()
     WHERE id = (SELECT id FROM concurrency_capacity)
       AND EXISTS (SELECT 1 FROM execution)
    RETURNING *
)
SELECT
    updated.id,
    updated.org_id,
    updated.project_id,
    updated.environment_id,
    updated.task_id,
    updated.status,
    updated.payload,
    updated.secret_bindings,
    deployment_tasks.id AS deployment_task_id,
    deployment_tasks.file_path AS deployment_task_file_path,
    deployment_tasks.export_name AS deployment_task_export_name,
    deployment_tasks.handler_entrypoint AS deployment_task_handler_entrypoint,
    deployment_tasks.bundle_digest AS deployment_task_bundle_digest,
    deployments.deployment_source_digest AS deployment_source_digest,
    updated.workspace_repository,
    updated.workspace_installation_id,
    updated.workspace_github_repository_id,
    updated.workspace_ref,
    updated.workspace_sha,
    updated.workspace_subpath,
    updated.workspace_ref_kind,
    updated.workspace_ref_name,
    updated.workspace_full_ref,
    updated.workspace_default_branch,
    updated.workspace_pr_number,
    updated.workspace_pr_base_ref,
    updated.workspace_pr_base_sha,
    updated.workspace_pr_head_ref,
    updated.workspace_pr_head_sha,
    updated.max_duration_seconds,
    updated.exit_code,
    updated.error_message,
    updated.created_at,
    updated.updated_at,
    updated.started_at,
    updated.finished_at,
    execution.id AS execution_id,
    execution.worker_instance_id AS execution_worker_instance_id,
    execution.dispatch_message_id AS execution_dispatch_message_id,
    execution.dispatch_lease_id AS execution_dispatch_lease_id,
    execution.dispatch_attempt AS execution_dispatch_attempt,
    execution.lease_expires_at AS execution_lease_expires_at,
    execution.restore_checkpoint_id AS execution_restore_checkpoint_id,
    active_time.active_duration_ms AS active_duration_ms
FROM updated
JOIN execution ON true
JOIN active_time ON true
JOIN deployments ON deployments.org_id = updated.org_id
                AND deployments.id = updated.deployment_id
JOIN deployment_tasks ON deployment_tasks.org_id = updated.org_id
                     AND deployment_tasks.deployment_id = updated.deployment_id
                     AND deployment_tasks.id = updated.deployment_task_id
LEFT JOIN marked_restore_checkpoint ON true;

-- name: StartRunExecution :one
WITH started_run AS (
    UPDATE runs
       SET status = 'running',
           started_at = COALESCE(runs.started_at, now()),
           queued_expires_at = NULL,
           updated_at = now()
     WHERE runs.org_id = sqlc.arg(org_id)
       AND runs.id = sqlc.arg(run_id)
       AND runs.status = 'running'
       AND runs.current_execution_id = sqlc.arg(execution_id)
       AND EXISTS (
           SELECT 1
            FROM run_executions
            WHERE run_executions.id = sqlc.arg(execution_id)
              AND run_executions.run_id = sqlc.arg(run_id)
              AND run_executions.worker_instance_id = sqlc.arg(worker_instance_id)
              AND run_executions.status IN ('leased', 'running')
              AND run_executions.lease_expires_at > now()
       )
     RETURNING status, id, current_execution_id
),
started_execution AS (
    UPDATE run_executions
       SET status = 'running',
           started_at = COALESCE(run_executions.started_at, now()),
           renewed_at = now()
      FROM started_run
     WHERE run_executions.id = started_run.current_execution_id
       AND run_executions.run_id = started_run.id
       AND run_executions.worker_instance_id = sqlc.arg(worker_instance_id)
     RETURNING run_executions.id, run_executions.restore_checkpoint_id
)
SELECT started_run.status FROM started_run JOIN started_execution ON true;

-- name: RenewRunExecutionLease :one
UPDATE run_executions
   SET lease_expires_at = sqlc.arg(lease_expires_at),
       renewed_at = now()
  FROM runs
 WHERE runs.org_id = sqlc.arg(org_id)
   AND runs.id = sqlc.arg(run_id)
   AND runs.status = 'running'
   AND runs.current_execution_id = run_executions.id
   AND run_executions.org_id = sqlc.arg(org_id)
   AND run_executions.run_id = sqlc.arg(run_id)
   AND run_executions.id = sqlc.arg(execution_id)
   AND run_executions.worker_instance_id = sqlc.arg(worker_instance_id)
   AND run_executions.dispatch_message_id = sqlc.arg(dispatch_message_id)
   AND run_executions.dispatch_lease_id = sqlc.arg(dispatch_lease_id)
   AND run_executions.status IN ('leased', 'running')
   AND run_executions.lease_expires_at > now()
RETURNING run_executions.id, run_executions.worker_instance_id, run_executions.dispatch_message_id, run_executions.dispatch_lease_id, run_executions.dispatch_attempt, run_executions.lease_expires_at;

-- name: GetRunExecutionQueueLease :one
SELECT run_executions.id,
       run_executions.run_id,
       run_executions.worker_instance_id,
       run_executions.dispatch_message_id,
       run_executions.dispatch_lease_id,
       run_executions.dispatch_attempt,
       run_executions.lease_expires_at,
       run_queue_items.queue_name
  FROM run_executions
  JOIN run_queue_items ON run_queue_items.org_id = run_executions.org_id
                     AND run_queue_items.run_id = run_executions.run_id
                     AND run_queue_items.dispatch_message_id = run_executions.dispatch_message_id
                     AND run_queue_items.reserved_by_worker_instance_id = run_executions.worker_instance_id
 WHERE run_executions.org_id = sqlc.arg(org_id)
   AND run_executions.run_id = sqlc.arg(run_id)
   AND run_executions.id = sqlc.arg(execution_id)
   AND run_executions.worker_instance_id = sqlc.arg(worker_instance_id)
   AND run_executions.status IN ('leased', 'running')
   AND run_executions.lease_expires_at > now()
   AND run_queue_items.status = 'reserved'
   AND run_queue_items.reservation_expires_at > now();

-- name: ReleaseRunExecution :one
WITH eligible AS (
    SELECT runs.org_id, runs.id AS run_id
      FROM runs
      JOIN run_executions
        ON run_executions.org_id = runs.org_id
       AND run_executions.run_id = runs.id
       AND run_executions.id = sqlc.arg(execution_id)
       AND run_executions.worker_instance_id = sqlc.arg(worker_instance_id)
       AND run_executions.dispatch_message_id = sqlc.arg(dispatch_message_id)
       AND run_executions.dispatch_lease_id = sqlc.arg(dispatch_lease_id)
       AND run_executions.status IN ('leased', 'running')
       AND run_executions.lease_expires_at > now()
      JOIN run_queue_items
        ON run_queue_items.org_id = runs.org_id
       AND run_queue_items.run_id = runs.id
       AND run_queue_items.reserved_by_worker_instance_id = sqlc.arg(worker_instance_id)
       AND run_queue_items.dispatch_message_id = sqlc.arg(dispatch_message_id)
       AND run_queue_items.status = 'reserved'
       AND run_queue_items.reservation_expires_at > now()
     WHERE runs.org_id = sqlc.arg(org_id)
       AND runs.id = sqlc.arg(run_id)
       AND runs.status = 'running'
       AND runs.current_execution_id = sqlc.arg(execution_id)
     FOR UPDATE OF runs, run_executions, run_queue_items
),
completed_queue_entry AS (
    UPDATE run_queue_items
       SET status = 'completed',
           dispatch_generation = dispatch_generation + 1,
           updated_at = now(),
           finished_at = now()
      FROM eligible
     WHERE run_queue_items.org_id = eligible.org_id
       AND run_queue_items.run_id = eligible.run_id
       AND run_queue_items.reserved_by_worker_instance_id = sqlc.arg(worker_instance_id)
       AND run_queue_items.dispatch_message_id = sqlc.arg(dispatch_message_id)
    RETURNING run_queue_items.run_id
),
released AS (
    UPDATE runs
       SET status = sqlc.arg(status),
           current_execution_id = NULL,
           exit_code = sqlc.arg(exit_code),
           output = sqlc.arg(output),
           error_message = sqlc.arg(error_message),
           finished_at = now(),
           updated_at = now()
      FROM eligible
      JOIN completed_queue_entry ON completed_queue_entry.run_id = eligible.run_id
     WHERE runs.org_id = eligible.org_id
       AND runs.id = eligible.run_id
    RETURNING runs.*
),
released_execution AS (
    UPDATE run_executions
       SET released_at = now(),
           renewed_at = now(),
           status = 'released'
      FROM released
     WHERE run_executions.id = sqlc.arg(execution_id)
       AND run_executions.run_id = released.id
       AND run_executions.worker_instance_id = sqlc.arg(worker_instance_id)
       AND run_executions.dispatch_message_id = sqlc.arg(dispatch_message_id)
       AND run_executions.dispatch_lease_id = sqlc.arg(dispatch_lease_id)
    RETURNING run_executions.id, run_executions.restore_checkpoint_id
),
released_concurrency_slot AS (
    UPDATE run_concurrency_slots
       SET released_at = now()
      FROM released
     WHERE run_concurrency_slots.org_id = sqlc.arg(org_id)
       AND run_concurrency_slots.run_id = released.id
       AND run_concurrency_slots.execution_id = sqlc.arg(execution_id)
       AND run_concurrency_slots.released_at IS NULL
    RETURNING run_concurrency_slots.id
),
cancelled_run_waits AS (
    UPDATE run_waits
       SET status = 'cancelled',
           failure = jsonb_build_object('reason', COALESCE(sqlc.arg(error_message)::text, 'execution released'), 'source', 'release'),
           resolution_kind = 'cancelled',
           resolution = jsonb_build_object('reason', COALESCE(sqlc.arg(error_message)::text, 'execution released'), 'source', 'release'),
           failed_at = now(),
           updated_at = now()
      FROM released
     WHERE run_waits.org_id = sqlc.arg(org_id)
       AND run_waits.run_id = released.id
       AND run_waits.execution_id = sqlc.arg(execution_id)
       AND (
           run_waits.status IN ('opening', 'waiting')
           OR (run_waits.status = 'resuming' AND sqlc.arg(error_message)::text IS NOT NULL)
       )
    RETURNING run_waits.id, run_waits.org_id
),
cancelled_waitpoints AS (
    UPDATE waitpoints
       SET status = 'cancelled',
           resolution_kind = 'cancelled',
           output = 'null'::jsonb,
           resolution = jsonb_build_object('reason', COALESCE(sqlc.arg(error_message)::text, 'execution released'), 'source', 'release'),
           output_is_error = true,
           completed_at = now(),
           updated_at = now()
      FROM cancelled_run_waits
      JOIN run_wait_dependencies ON run_wait_dependencies.org_id = cancelled_run_waits.org_id
                                AND run_wait_dependencies.run_wait_id = cancelled_run_waits.id
     WHERE waitpoints.org_id = run_wait_dependencies.org_id
       AND waitpoints.id = run_wait_dependencies.waitpoint_id
       AND waitpoints.status = 'pending'
    RETURNING waitpoints.id
),
invalidated_checkpoints AS (
    UPDATE checkpoints
       SET status = 'invalid',
           error_message = COALESCE(sqlc.arg(error_message)::text, 'execution released'),
           invalidated_at = now()
      FROM released
     WHERE checkpoints.org_id = sqlc.arg(org_id)
       AND checkpoints.run_id = released.id
       AND checkpoints.execution_id = sqlc.arg(execution_id)
       AND checkpoints.status IN ('creating', 'restoring')
    RETURNING checkpoints.run_id, checkpoints.id
),
completed_restore_checkpoint AS (
    UPDATE checkpoints
       SET status = 'ready',
           error_message = NULL,
           invalidated_at = NULL
      FROM released
      JOIN released_execution ON true
     WHERE checkpoints.org_id = sqlc.arg(org_id)
       AND checkpoints.run_id = released.id
       AND checkpoints.id = released_execution.restore_checkpoint_id
       AND checkpoints.status = 'restoring'
       AND sqlc.arg(error_message)::text IS NULL
    RETURNING checkpoints.id
),
failed_restore_checkpoint AS (
    UPDATE checkpoints
       SET status = 'invalid',
           error_message = sqlc.arg(error_message)::text,
           invalidated_at = now()
      FROM released
      JOIN released_execution ON true
     WHERE checkpoints.org_id = sqlc.arg(org_id)
       AND checkpoints.run_id = released.id
       AND checkpoints.id = released_execution.restore_checkpoint_id
       AND checkpoints.status = 'restoring'
       AND sqlc.arg(error_message)::text IS NOT NULL
    RETURNING checkpoints.run_id, checkpoints.id
),
resolved_restore_waitpoint AS (
    UPDATE run_waits
       SET status = 'restored',
           restored_at = now(),
           updated_at = now()
      FROM released
      JOIN released_execution ON true
      JOIN completed_restore_checkpoint ON completed_restore_checkpoint.id = released_execution.restore_checkpoint_id
     WHERE run_waits.org_id = sqlc.arg(org_id)
       AND run_waits.run_id = released.id
       AND run_waits.checkpoint_id = released_execution.restore_checkpoint_id
       AND run_waits.status = 'resuming'
       AND sqlc.arg(error_message)::text IS NULL
    RETURNING run_waits.id
),
terminal_event AS (
    INSERT INTO run_events (org_id, run_id, kind, payload)
    SELECT released.org_id, released.id, sqlc.arg(terminal_event_kind), sqlc.arg(terminal_event_payload)
      FROM released
    RETURNING id
),
cleanup AS (
    SELECT
        (SELECT count(*) FROM cancelled_waitpoints) AS cancelled_waitpoints,
        (SELECT count(*) FROM invalidated_checkpoints) AS invalidated_checkpoints,
        (SELECT count(*) FROM released_concurrency_slot) AS released_concurrency_slots,
        (SELECT count(*) FROM completed_restore_checkpoint) AS completed_restore_checkpoints,
        (SELECT count(*) FROM resolved_restore_waitpoint) AS resolved_restore_waitpoints,
        (SELECT count(*) FROM terminal_event) AS terminal_events
),
idempotent_released AS (
    SELECT runs.*
      FROM runs
      JOIN run_executions
        ON run_executions.org_id = runs.org_id
       AND run_executions.run_id = runs.id
       AND run_executions.id = sqlc.arg(execution_id)
       AND run_executions.worker_instance_id = sqlc.arg(worker_instance_id)
       AND run_executions.dispatch_message_id = sqlc.arg(dispatch_message_id)
       AND run_executions.dispatch_lease_id = sqlc.arg(dispatch_lease_id)
       AND run_executions.status = 'released'
     WHERE runs.org_id = sqlc.arg(org_id)
       AND runs.id = sqlc.arg(run_id)
       AND runs.status = sqlc.arg(status)
       AND runs.current_execution_id IS NULL
       AND runs.exit_code IS NOT DISTINCT FROM sqlc.arg(exit_code)
       AND runs.error_message IS NOT DISTINCT FROM sqlc.arg(error_message)
       AND runs.output IS NOT DISTINCT FROM sqlc.arg(output)::jsonb
       AND NOT EXISTS (SELECT 1 FROM released)
)
SELECT released.*
  FROM released
  JOIN released_execution ON true
  JOIN completed_queue_entry ON true
  JOIN cleanup ON true
UNION ALL
SELECT *
  FROM idempotent_released;
