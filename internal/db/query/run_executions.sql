-- name: RequeueExpiredClaimedRunExecutions :exec
WITH eligible AS (
    SELECT runs.id AS run_id,
           run_executions.id AS execution_id,
           run_executions.restore_checkpoint_id
      FROM runs
      JOIN run_executions ON run_executions.id = runs.current_execution_id
                          AND run_executions.org_id = runs.org_id
                          AND run_executions.run_id = runs.id
     WHERE runs.org_id = $1
       AND runs.status = 'claimed'
       AND run_executions.status = 'claimed'
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
       AND runs.status = 'claimed'
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

-- name: AbandonClaimedRunExecution :exec
WITH abandoned AS (
    UPDATE runs
       SET status = 'queued',
           current_execution_id = NULL,
           updated_at = now()
     WHERE runs.org_id = sqlc.arg(org_id)
       AND runs.id = sqlc.arg(run_id)
       AND runs.status = 'claimed'
       AND runs.current_execution_id = sqlc.arg(execution_id)
       AND EXISTS (
           SELECT 1
             FROM run_executions
            WHERE run_executions.org_id = sqlc.arg(org_id)
              AND run_executions.run_id = sqlc.arg(run_id)
              AND run_executions.id = sqlc.arg(execution_id)
              AND run_executions.worker_pool_id = sqlc.arg(worker_pool_id)
              AND run_executions.worker_id = sqlc.arg(worker_id)
              AND run_executions.status = 'claimed'
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
                         AND run_executions.worker_pool_id = sqlc.arg(worker_pool_id)
                         AND run_executions.worker_id = sqlc.arg(worker_id)
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
   AND run_executions.worker_pool_id = sqlc.arg(worker_pool_id)
   AND run_executions.worker_id = sqlc.arg(worker_id)
   AND run_executions.status = 'claimed'
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
       AND runs.status IN ('running', 'waiting')
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
       AND runs.status IN ('running', 'waiting')
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
        (SELECT count(*) FROM terminal_events) AS terminal_events
)
UPDATE run_executions
   SET lost_at = COALESCE(lost_at, now()),
       renewed_at = now(),
       status = 'lost'
  FROM updated_runs
 WHERE run_executions.id = updated_runs.execution_id
   AND run_executions.run_id = updated_runs.run_id
   AND (SELECT cancelled_waitpoints + invalidated_checkpoints + invalidated_restore_checkpoints + terminal_events FROM cleanup) >= 0;

-- name: ClaimRunExecution :one
WITH worker_state AS (
    SELECT workers.*,
           worker_pools.project_id,
           worker_pools.environment_id
      FROM workers
      JOIN worker_pools ON worker_pools.org_id = workers.org_id
                       AND worker_pools.id = workers.worker_pool_id
     WHERE workers.org_id = sqlc.arg(org_id)
       AND workers.worker_pool_id = sqlc.arg(worker_pool_id)
       AND workers.id = sqlc.arg(worker_id)
       AND workers.status = 'active'
       AND worker_pools.archived_at IS NULL
       AND workers.slots_available > (
           SELECT count(*)
            FROM run_executions
            WHERE run_executions.org_id = sqlc.arg(org_id)
              AND run_executions.worker_pool_id = sqlc.arg(worker_pool_id)
              AND run_executions.worker_id = sqlc.arg(worker_id)
              AND run_executions.status IN ('claimed', 'running')
       )
     FOR UPDATE
),
candidate AS (
    SELECT runs.id, runs.latest_checkpoint_id
     FROM runs
      JOIN worker_state ON true
     WHERE runs.org_id = sqlc.arg(org_id)
       AND runs.project_id = worker_state.project_id
       AND runs.environment_id = worker_state.environment_id
       AND runs.status = 'queued'
       AND runs.current_execution_id IS NULL
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
                  AND (checkpoints.runtime_arch IS NULL OR checkpoints.runtime_arch = worker_state.runtime_arch)
                  AND (checkpoints.runtime_abi IS NULL OR checkpoints.runtime_abi = worker_state.runtime_abi)
                  AND (checkpoints.kernel_digest IS NULL OR checkpoints.kernel_digest = worker_state.kernel_digest)
                  AND (checkpoints.rootfs_digest IS NULL OR checkpoints.rootfs_digest = worker_state.rootfs_digest)
                  AND (checkpoints.runtime_vcpus IS NULL OR checkpoints.runtime_vcpus = worker_state.max_vcpus)
                  AND (checkpoints.runtime_memory_mib IS NULL OR checkpoints.runtime_memory_mib = worker_state.max_memory_mib)
                  AND (checkpoints.cni_profile IS NULL OR checkpoints.cni_profile = worker_state.cni_profile)
           )
       )
     ORDER BY runs.created_at ASC
     FOR UPDATE OF runs SKIP LOCKED
     LIMIT 1
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
    INSERT INTO run_executions (id, org_id, run_id, worker_pool_id, worker_id, status, lease_expires_at, restore_checkpoint_id)
    SELECT sqlc.arg(execution_id), sqlc.arg(org_id), id, sqlc.arg(worker_pool_id), sqlc.arg(worker_id), 'claimed', sqlc.arg(lease_expires_at), (SELECT id FROM restore_checkpoint)
      FROM candidate
    RETURNING id, worker_pool_id, worker_id, lease_expires_at
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
       SET status = 'claimed',
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
    execution.worker_pool_id AS execution_worker_pool_id,
    execution.worker_id AS execution_worker_id,
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
       AND runs.status IN ('claimed', 'running')
       AND runs.current_execution_id = sqlc.arg(execution_id)
       AND EXISTS (
           SELECT 1
            FROM run_executions
            WHERE run_executions.id = sqlc.arg(execution_id)
              AND run_executions.run_id = sqlc.arg(run_id)
              AND run_executions.worker_pool_id = sqlc.arg(worker_pool_id)
              AND run_executions.worker_id = sqlc.arg(worker_id)
              AND run_executions.status IN ('claimed', 'running')
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
       AND run_executions.worker_pool_id = sqlc.arg(worker_pool_id)
       AND run_executions.worker_id = sqlc.arg(worker_id)
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
   AND runs.status IN ('claimed', 'running', 'waiting')
   AND runs.current_execution_id = run_executions.id
   AND run_executions.org_id = sqlc.arg(org_id)
   AND run_executions.run_id = sqlc.arg(run_id)
   AND run_executions.id = sqlc.arg(execution_id)
   AND run_executions.worker_pool_id = sqlc.arg(worker_pool_id)
   AND run_executions.worker_id = sqlc.arg(worker_id)
   AND run_executions.status IN ('claimed', 'running')
   AND run_executions.lease_expires_at > now()
RETURNING run_executions.id, run_executions.worker_id, run_executions.lease_expires_at;

-- name: ReleaseRunExecution :one
WITH released AS (
    UPDATE runs
       SET status = sqlc.arg(status),
           current_execution_id = NULL,
           exit_code = sqlc.arg(exit_code),
           output = sqlc.arg(output),
           error_message = sqlc.arg(error_message),
           finished_at = now(),
           updated_at = now()
     WHERE runs.org_id = sqlc.arg(org_id)
       AND runs.id = sqlc.arg(run_id)
       AND runs.status IN ('claimed', 'running', 'waiting')
       AND runs.current_execution_id = sqlc.arg(execution_id)
       AND EXISTS (
           SELECT 1
            FROM run_executions
            WHERE run_executions.id = sqlc.arg(execution_id)
              AND run_executions.run_id = sqlc.arg(run_id)
              AND run_executions.worker_pool_id = sqlc.arg(worker_pool_id)
              AND run_executions.worker_id = sqlc.arg(worker_id)
              AND run_executions.status IN ('claimed', 'running')
              AND run_executions.lease_expires_at > now()
       )
    RETURNING *
),
released_execution AS (
    UPDATE run_executions
       SET released_at = now(),
           renewed_at = now(),
           status = 'released'
      FROM released
     WHERE run_executions.id = sqlc.arg(execution_id)
       AND run_executions.run_id = released.id
       AND run_executions.worker_pool_id = sqlc.arg(worker_pool_id)
       AND run_executions.worker_id = sqlc.arg(worker_id)
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
)
SELECT released.*
  FROM released
  JOIN released_execution ON true
  JOIN cleanup ON true;
