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
     RETURNING eligible.run_id, eligible.execution_id, eligible.restore_checkpoint_id
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
cleanup AS (
    SELECT count(*) AS restored_checkpoint_count FROM restored_checkpoint
)
UPDATE run_executions
   SET lost_at = COALESCE(lost_at, now()),
       renewed_at = now(),
       status = 'lost'
  FROM updated_runs
 WHERE run_executions.id = updated_runs.execution_id
   AND run_executions.run_id = updated_runs.run_id
   AND (SELECT restored_checkpoint_count FROM cleanup) >= 0;

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
              AND run_executions.worker_group_id = sqlc.arg(worker_group_id)
              AND run_executions.worker_host_id = sqlc.arg(worker_host_id)
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
                         AND run_executions.worker_group_id = sqlc.arg(worker_group_id)
                         AND run_executions.worker_host_id = sqlc.arg(worker_host_id)
     WHERE checkpoints.org_id = sqlc.arg(org_id)
       AND checkpoints.run_id = abandoned.id
       AND checkpoints.id = run_executions.restore_checkpoint_id
       AND checkpoints.status = 'restoring'
    RETURNING checkpoints.id
),
cleanup AS (
    SELECT count(*) AS restored_checkpoint_count FROM restored_checkpoint
)
UPDATE run_executions
   SET lost_at = COALESCE(lost_at, now()),
       renewed_at = now(),
       status = 'lost'
  FROM abandoned
 WHERE run_executions.org_id = sqlc.arg(org_id)
   AND run_executions.run_id = abandoned.id
   AND run_executions.id = sqlc.arg(execution_id)
   AND run_executions.worker_group_id = sqlc.arg(worker_group_id)
   AND run_executions.worker_host_id = sqlc.arg(worker_host_id)
   AND run_executions.status = 'leased'
   AND (SELECT restored_checkpoint_count FROM cleanup) >= 0;

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
cancelled_waitpoints AS (
    UPDATE waitpoints
       SET status = 'cancelled',
           resolution_kind = 'cancelled',
           resolution = jsonb_build_object('reason', 'worker lease expired', 'source', 'lease_sweeper'),
           requested_at = COALESCE(requested_at, now()),
           resolved_at = now()
      FROM updated_runs
     WHERE waitpoints.run_id = updated_runs.run_id
       AND waitpoints.execution_id = updated_runs.execution_id
       AND waitpoints.status IN ('creating', 'pending')
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
    RETURNING checkpoints.id
),
invalidated_restore_checkpoints AS (
    UPDATE checkpoints
       SET status = 'invalid',
           error_message = 'worker lease expired',
           invalidated_at = now()
      FROM updated_runs
     WHERE checkpoints.run_id = updated_runs.run_id
       AND checkpoints.id = updated_runs.restore_checkpoint_id
       AND checkpoints.status = 'restoring'
    RETURNING checkpoints.id
),
completed_queue_entries AS (
    UPDATE run_queue_entries
       SET status = 'completed',
           dispatch_generation = dispatch_generation + 1,
           updated_at = now(),
           finished_at = now()
      FROM updated_runs
      JOIN run_executions ON run_executions.org_id = $1
                         AND run_executions.run_id = updated_runs.run_id
                         AND run_executions.id = updated_runs.execution_id
     WHERE run_queue_entries.org_id = $1
       AND run_queue_entries.run_id = updated_runs.run_id
       AND run_queue_entries.worker_group_id = run_executions.worker_group_id
       AND run_queue_entries.reserved_by_worker_host_id = run_executions.worker_host_id
       AND run_queue_entries.queue_message_id = run_executions.queue_message_id
       AND run_queue_entries.status = 'reserved'
    RETURNING run_queue_entries.run_id
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
        (SELECT count(*) FROM invalidated_restore_checkpoints) AS invalidated_restore_checkpoints,
        (SELECT count(*) FROM completed_queue_entries) AS completed_queue_entries,
        (SELECT count(*) FROM terminal_events) AS terminal_events
)
UPDATE run_executions
   SET lost_at = COALESCE(lost_at, now()),
       renewed_at = now(),
       status = 'lost'
  FROM updated_runs
 WHERE run_executions.id = updated_runs.execution_id
   AND run_executions.run_id = updated_runs.run_id
   AND (SELECT cancelled_waitpoints + invalidated_checkpoints + invalidated_restore_checkpoints + completed_queue_entries + terminal_events FROM cleanup) >= 0;

-- name: LeaseRunExecution :one
WITH dispatch AS (
    SELECT run_queue_entries.run_id,
           run_queue_entries.worker_group_id,
           run_queue_entries.reserved_by_worker_host_id AS worker_host_id,
           run_queue_entries.queue_message_id,
           worker_hosts.available_milli_cpu,
           worker_hosts.available_memory_mib,
           worker_hosts.available_disk_mib,
           worker_hosts.available_execution_slots,
           worker_hosts.total_milli_cpu,
           worker_hosts.total_memory_mib,
           worker_hosts.region,
           worker_hosts.labels,
           worker_hosts.heartbeat->>'runtime_arch' AS runtime_arch,
           worker_hosts.heartbeat->>'runtime_abi' AS runtime_abi,
           worker_hosts.heartbeat->>'kernel_digest' AS kernel_digest,
           worker_hosts.heartbeat->>'rootfs_digest' AS rootfs_digest,
           worker_hosts.heartbeat->>'cni_profile' AS cni_profile,
           active.used_milli_cpu,
           active.used_memory_mib,
           active.used_disk_mib,
           active.used_slots
      FROM run_queue_entries
      JOIN worker_hosts ON worker_hosts.org_id = run_queue_entries.org_id
                       AND worker_hosts.worker_group_id = run_queue_entries.worker_group_id
                       AND worker_hosts.id = run_queue_entries.reserved_by_worker_host_id
      LEFT JOIN LATERAL (
          SELECT COALESCE(sum(run_requirements.requested_milli_cpu), 0)::bigint AS used_milli_cpu,
                 COALESCE(sum(run_requirements.requested_memory_mib), 0)::bigint AS used_memory_mib,
                 COALESCE(sum(run_requirements.requested_disk_mib), 0)::bigint AS used_disk_mib,
                 COALESCE(sum(run_requirements.requested_execution_slots), 0)::int AS used_slots
            FROM run_executions
            JOIN run_requirements ON run_requirements.org_id = run_executions.org_id
                                          AND run_requirements.run_id = run_executions.run_id
                                          AND run_requirements.worker_group_id = run_executions.worker_group_id
           WHERE run_executions.org_id = worker_hosts.org_id
             AND run_executions.worker_group_id = worker_hosts.worker_group_id
             AND run_executions.worker_host_id = worker_hosts.id
             AND run_executions.status IN ('leased', 'running')
      ) active ON true
     WHERE run_queue_entries.org_id = sqlc.arg(org_id)
       AND run_queue_entries.run_id = sqlc.arg(run_id)
       AND run_queue_entries.worker_group_id = sqlc.arg(worker_group_id)
       AND run_queue_entries.reserved_by_worker_host_id = sqlc.arg(worker_host_id)
       AND run_queue_entries.queue_message_id = sqlc.arg(queue_message_id)
       AND run_queue_entries.status = 'reserved'
       AND run_queue_entries.reservation_expires_at > now()
       AND worker_hosts.status = 'active'
     FOR UPDATE OF run_queue_entries, worker_hosts
),
candidate AS (
    SELECT runs.id, runs.latest_checkpoint_id
     FROM runs
      JOIN dispatch ON dispatch.run_id = runs.id
      JOIN run_requirements ON run_requirements.org_id = runs.org_id
                                    AND run_requirements.run_id = runs.id
                                    AND run_requirements.worker_group_id = dispatch.worker_group_id
      JOIN LATERAL (
          SELECT COALESCE(NULLIF(run_requirements.placement->>'region', ''), NULLIF(run_requirements.placement->>'Region', ''), '') AS placement_region,
                 COALESCE(run_requirements.placement->'tags', run_requirements.placement->'Tags') AS placement_tags,
                 COALESCE(NULLIF(run_requirements.placement->>'dedicated_key', ''), NULLIF(run_requirements.placement->>'DedicatedKey', ''), '') AS dedicated_key,
                 COALESCE(NULLIF(run_requirements.placement->>'snapshot_key', ''), NULLIF(run_requirements.placement->>'SnapshotKey', ''), '') AS snapshot_key
      ) placement ON true
     WHERE runs.org_id = sqlc.arg(org_id)
       AND runs.id = sqlc.arg(run_id)
       AND runs.status = 'queued'
       AND runs.current_execution_id IS NULL
       AND run_requirements.requested_milli_cpu <= GREATEST(dispatch.available_milli_cpu - dispatch.used_milli_cpu, 0)
       AND run_requirements.requested_memory_mib <= GREATEST(dispatch.available_memory_mib - dispatch.used_memory_mib, 0)
       AND run_requirements.requested_disk_mib <= GREATEST(dispatch.available_disk_mib - dispatch.used_disk_mib, 0)
       AND run_requirements.requested_execution_slots <= GREATEST(dispatch.available_execution_slots - dispatch.used_slots, 0)
       AND (run_requirements.runtime_arch = '' OR run_requirements.runtime_arch = dispatch.runtime_arch)
       AND (run_requirements.runtime_abi = '' OR run_requirements.runtime_abi = dispatch.runtime_abi)
       AND (run_requirements.kernel_digest = '' OR run_requirements.kernel_digest = dispatch.kernel_digest)
       AND (run_requirements.rootfs_digest = '' OR run_requirements.rootfs_digest = dispatch.rootfs_digest)
       AND (run_requirements.cni_profile = '' OR run_requirements.cni_profile = dispatch.cni_profile)
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
                 JOIN waitpoints ON waitpoints.org_id = sqlc.arg(org_id)
                                AND waitpoints.run_id = runs.id
                                AND waitpoints.checkpoint_id = checkpoints.id
                WHERE checkpoints.org_id = sqlc.arg(org_id)
                  AND checkpoints.run_id = runs.id
                  AND checkpoints.id = runs.latest_checkpoint_id
                  AND checkpoints.status = 'ready'
                  AND waitpoints.status = 'resolved'
                  AND waitpoints.resolution_kind IS NOT NULL
                  AND (checkpoints.runtime_arch IS NULL OR checkpoints.runtime_arch = dispatch.runtime_arch)
                  AND (checkpoints.runtime_abi IS NULL OR checkpoints.runtime_abi = dispatch.runtime_abi)
                  AND (checkpoints.kernel_digest IS NULL OR checkpoints.kernel_digest = dispatch.kernel_digest)
                  AND (checkpoints.rootfs_digest IS NULL OR checkpoints.rootfs_digest = dispatch.rootfs_digest)
                  AND (checkpoints.runtime_vcpus IS NULL OR checkpoints.runtime_vcpus = ((dispatch.total_milli_cpu + 999) / 1000))
                  AND (checkpoints.runtime_memory_mib IS NULL OR checkpoints.runtime_memory_mib = dispatch.total_memory_mib)
                  AND (checkpoints.cni_profile IS NULL OR checkpoints.cni_profile = dispatch.cni_profile)
           )
       )
     FOR UPDATE OF runs
),
restore_checkpoint AS (
    SELECT checkpoints.id
      FROM candidate
      JOIN checkpoints ON checkpoints.org_id = sqlc.arg(org_id)
                      AND checkpoints.run_id = candidate.id
                      AND checkpoints.id = candidate.latest_checkpoint_id
      JOIN waitpoints ON waitpoints.org_id = sqlc.arg(org_id)
                     AND waitpoints.run_id = candidate.id
                     AND waitpoints.checkpoint_id = checkpoints.id
     WHERE checkpoints.status = 'ready'
       AND waitpoints.status = 'resolved'
       AND waitpoints.resolution_kind IS NOT NULL
     ORDER BY waitpoints.resolved_at DESC
     LIMIT 1
),
execution AS (
    INSERT INTO run_executions (id, org_id, run_id, worker_group_id, worker_host_id, queue_message_id, queue_lease_id, delivery_attempt, status, lease_expires_at, restore_checkpoint_id)
    SELECT sqlc.arg(execution_id), sqlc.arg(org_id), candidate.id, sqlc.arg(worker_group_id), sqlc.arg(worker_host_id), sqlc.arg(queue_message_id), sqlc.arg(queue_lease_id), sqlc.arg(delivery_attempt), 'leased', sqlc.arg(lease_expires_at), (SELECT id FROM restore_checkpoint)
      FROM candidate
    RETURNING id, worker_group_id, worker_host_id, queue_message_id, queue_lease_id, delivery_attempt, lease_expires_at
),
active_time AS (
    SELECT COALESCE(MAX(run_executions.active_duration_ms), 0)::bigint AS active_duration_ms
      FROM candidate
      LEFT JOIN run_executions ON run_executions.org_id = sqlc.arg(org_id)
                              AND run_executions.run_id = candidate.id
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
     WHERE id = (SELECT id FROM candidate)
     RETURNING *
)
SELECT
    updated.id,
    updated.org_id,
    updated.task_id,
    updated.status,
    updated.payload,
    updated.secret_bindings,
    deployed_tasks.id AS deployed_task_id,
    deployed_tasks.module_path AS deployed_task_module_path,
    deployed_tasks.export_name AS deployed_task_export_name,
    task_deployments.source_digest AS task_source_digest,
    updated.workspace_repository,
    updated.workspace_installation_id,
    updated.workspace_github_repository_id,
    updated.workspace_ref,
    updated.workspace_sha,
    updated.workspace_subpath,
    updated.max_duration_seconds,
    updated.exit_code,
    updated.error_message,
    updated.created_at,
    updated.updated_at,
    updated.started_at,
    updated.finished_at,
    execution.id AS execution_id,
    execution.worker_group_id AS execution_worker_group_id,
    execution.worker_host_id AS execution_worker_host_id,
    execution.queue_message_id AS execution_queue_message_id,
    execution.queue_lease_id AS execution_queue_lease_id,
    execution.delivery_attempt AS execution_delivery_attempt,
    execution.lease_expires_at AS execution_lease_expires_at,
    active_time.active_duration_ms AS active_duration_ms
FROM updated
JOIN execution ON true
JOIN active_time ON true
JOIN task_deployments ON task_deployments.org_id = updated.org_id
                     AND task_deployments.id = updated.task_deployment_id
JOIN deployed_tasks ON deployed_tasks.org_id = updated.org_id
                   AND deployed_tasks.deployment_id = updated.task_deployment_id
                   AND deployed_tasks.id = updated.deployed_task_id
LEFT JOIN marked_restore_checkpoint ON true;

-- name: StartRunExecution :one
WITH started_run AS (
    UPDATE runs
       SET status = 'running',
           started_at = COALESCE(runs.started_at, now()),
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
              AND run_executions.worker_group_id = sqlc.arg(worker_group_id)
              AND run_executions.worker_host_id = sqlc.arg(worker_host_id)
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
       AND run_executions.worker_group_id = sqlc.arg(worker_group_id)
       AND run_executions.worker_host_id = sqlc.arg(worker_host_id)
     RETURNING run_executions.id
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
   AND run_executions.worker_group_id = sqlc.arg(worker_group_id)
   AND run_executions.worker_host_id = sqlc.arg(worker_host_id)
   AND run_executions.queue_message_id = sqlc.arg(queue_message_id)
   AND run_executions.queue_lease_id = sqlc.arg(queue_lease_id)
   AND run_executions.status IN ('leased', 'running')
   AND run_executions.lease_expires_at > now()
RETURNING run_executions.id, run_executions.worker_host_id, run_executions.queue_message_id, run_executions.queue_lease_id, run_executions.delivery_attempt, run_executions.lease_expires_at;

-- name: GetRunExecutionQueueLease :one
SELECT run_executions.id,
       run_executions.run_id,
       run_executions.worker_group_id,
       run_executions.worker_host_id,
       run_executions.queue_message_id,
       run_executions.queue_lease_id,
       run_executions.delivery_attempt,
       run_executions.lease_expires_at,
       run_queue_entries.queue_name
  FROM run_executions
  JOIN run_queue_entries ON run_queue_entries.org_id = run_executions.org_id
                     AND run_queue_entries.run_id = run_executions.run_id
                     AND run_queue_entries.worker_group_id = run_executions.worker_group_id
                     AND run_queue_entries.queue_message_id = run_executions.queue_message_id
                     AND run_queue_entries.reserved_by_worker_host_id = run_executions.worker_host_id
 WHERE run_executions.org_id = sqlc.arg(org_id)
   AND run_executions.run_id = sqlc.arg(run_id)
   AND run_executions.id = sqlc.arg(execution_id)
   AND run_executions.worker_group_id = sqlc.arg(worker_group_id)
   AND run_executions.worker_host_id = sqlc.arg(worker_host_id)
   AND run_executions.status IN ('leased', 'running')
   AND run_executions.lease_expires_at > now()
   AND run_queue_entries.status = 'reserved'
   AND run_queue_entries.reservation_expires_at > now();

-- name: ReleaseRunExecution :one
WITH eligible AS (
    SELECT runs.org_id, runs.id AS run_id
      FROM runs
      JOIN run_executions
        ON run_executions.org_id = runs.org_id
       AND run_executions.run_id = runs.id
       AND run_executions.id = sqlc.arg(execution_id)
       AND run_executions.worker_group_id = sqlc.arg(worker_group_id)
       AND run_executions.worker_host_id = sqlc.arg(worker_host_id)
       AND run_executions.queue_message_id = sqlc.arg(queue_message_id)
       AND run_executions.queue_lease_id = sqlc.arg(queue_lease_id)
       AND run_executions.status IN ('leased', 'running')
       AND run_executions.lease_expires_at > now()
      JOIN run_queue_entries
        ON run_queue_entries.org_id = runs.org_id
       AND run_queue_entries.run_id = runs.id
       AND run_queue_entries.worker_group_id = sqlc.arg(worker_group_id)
       AND run_queue_entries.reserved_by_worker_host_id = sqlc.arg(worker_host_id)
       AND run_queue_entries.queue_message_id = sqlc.arg(queue_message_id)
       AND run_queue_entries.status = 'reserved'
       AND run_queue_entries.reservation_expires_at > now()
     WHERE runs.org_id = sqlc.arg(org_id)
       AND runs.id = sqlc.arg(run_id)
       AND runs.status = 'running'
       AND runs.current_execution_id = sqlc.arg(execution_id)
     FOR UPDATE OF runs, run_executions, run_queue_entries
),
completed_queue_entry AS (
    UPDATE run_queue_entries
       SET status = 'completed',
           dispatch_generation = dispatch_generation + 1,
           updated_at = now(),
           finished_at = now()
      FROM eligible
     WHERE run_queue_entries.org_id = eligible.org_id
       AND run_queue_entries.run_id = eligible.run_id
       AND run_queue_entries.worker_group_id = sqlc.arg(worker_group_id)
       AND run_queue_entries.reserved_by_worker_host_id = sqlc.arg(worker_host_id)
       AND run_queue_entries.queue_message_id = sqlc.arg(queue_message_id)
    RETURNING run_queue_entries.run_id
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
       AND run_executions.worker_group_id = sqlc.arg(worker_group_id)
       AND run_executions.worker_host_id = sqlc.arg(worker_host_id)
       AND run_executions.queue_message_id = sqlc.arg(queue_message_id)
       AND run_executions.queue_lease_id = sqlc.arg(queue_lease_id)
    RETURNING run_executions.id, run_executions.restore_checkpoint_id
),
cancelled_waitpoints AS (
    UPDATE waitpoints
       SET status = 'cancelled',
           resolution_kind = 'cancelled',
           resolution = jsonb_build_object('reason', COALESCE(sqlc.arg(error_message)::text, 'execution released'), 'source', 'release'),
           requested_at = COALESCE(requested_at, now()),
           resolved_at = now()
      FROM released
     WHERE waitpoints.org_id = sqlc.arg(org_id)
       AND waitpoints.run_id = released.id
       AND waitpoints.execution_id = sqlc.arg(execution_id)
       AND waitpoints.status IN ('creating', 'pending')
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
    RETURNING checkpoints.id
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
    RETURNING checkpoints.id
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
        (SELECT count(*) FROM completed_restore_checkpoint) AS completed_restore_checkpoints,
        (SELECT count(*) FROM failed_restore_checkpoint) AS failed_restore_checkpoints,
        (SELECT count(*) FROM terminal_event) AS terminal_events
),
idempotent_released AS (
    SELECT runs.*
      FROM runs
      JOIN run_executions
        ON run_executions.org_id = runs.org_id
       AND run_executions.run_id = runs.id
       AND run_executions.id = sqlc.arg(execution_id)
       AND run_executions.worker_group_id = sqlc.arg(worker_group_id)
       AND run_executions.worker_host_id = sqlc.arg(worker_host_id)
       AND run_executions.queue_message_id = sqlc.arg(queue_message_id)
       AND run_executions.queue_lease_id = sqlc.arg(queue_lease_id)
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
