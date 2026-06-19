-- name: ListQueueScopes :many
SELECT run_queue_items.org_id,
       runs.project_id,
       runs.environment_id,
       run_queue_items.queue_name
  FROM run_queue_items
  JOIN runs ON runs.org_id = run_queue_items.org_id
           AND runs.id = run_queue_items.run_id
  JOIN run_runtime_requirements ON run_runtime_requirements.org_id = run_queue_items.org_id
                               AND run_runtime_requirements.run_id = run_queue_items.run_id
 WHERE run_queue_items.status IN ('queued', 'published', 'reserved')
   AND run_runtime_requirements.worker_group_id = sqlc.arg(worker_group_id)
 GROUP BY run_queue_items.org_id,
          runs.project_id,
          runs.environment_id,
          run_queue_items.queue_name
 ORDER BY md5(run_queue_items.org_id::text || ':' || runs.project_id::text || ':' || runs.environment_id::text || ':' || run_queue_items.queue_name || ':' || sqlc.arg(scan_seed)::text),
          run_queue_items.org_id ASC,
          runs.project_id ASC,
          runs.environment_id ASC,
          run_queue_items.queue_name ASC
 LIMIT sqlc.arg(row_limit)
OFFSET sqlc.arg(row_offset);

-- name: UpsertWorkerInstanceHeartbeat :one
WITH observed_runtime AS (
    INSERT INTO runtime_releases (
        runtime_id,
        runtime_arch,
        runtime_abi,
        kernel_digest,
        initramfs_digest,
        rootfs_digest,
        cni_profile,
        last_seen_at
    ) VALUES (
        sqlc.arg(runtime_id),
        sqlc.arg(runtime_arch),
        sqlc.arg(runtime_abi),
        sqlc.arg(kernel_digest),
        sqlc.arg(initramfs_digest),
        sqlc.arg(rootfs_digest),
        sqlc.arg(cni_profile),
        now()
    )
    ON CONFLICT (runtime_id) DO UPDATE
       SET last_seen_at = now()
     WHERE runtime_releases.runtime_arch = EXCLUDED.runtime_arch
       AND runtime_releases.runtime_abi = EXCLUDED.runtime_abi
       AND runtime_releases.kernel_digest = EXCLUDED.kernel_digest
       AND runtime_releases.initramfs_digest = EXCLUDED.initramfs_digest
       AND runtime_releases.rootfs_digest = EXCLUDED.rootfs_digest
       AND runtime_releases.cni_profile = EXCLUDED.cni_profile
    RETURNING *
),
upserted_worker AS (
    INSERT INTO worker_instances (
        id,
        worker_group_id,
        resource_id,
        status,
        region,
        total_milli_cpu,
        total_memory_mib,
        total_disk_mib,
        total_execution_slots,
        available_milli_cpu,
        available_memory_mib,
        available_disk_mib,
        available_execution_slots,
        labels,
        heartbeat,
        worker_version,
        protocol_version,
        runtime_id,
        runtime_arch,
        runtime_abi,
        kernel_digest,
        initramfs_digest,
        rootfs_digest,
        cni_profile,
        last_seen_at
    )
    SELECT sqlc.arg(id),
           sqlc.arg(worker_group_id),
           sqlc.arg(resource_id),
           'active',
           sqlc.arg(region),
           sqlc.arg(total_milli_cpu),
           sqlc.arg(total_memory_mib),
           sqlc.arg(total_disk_mib),
           sqlc.arg(total_execution_slots),
           sqlc.arg(available_milli_cpu),
           sqlc.arg(available_memory_mib),
           sqlc.arg(available_disk_mib),
           sqlc.arg(available_execution_slots),
           sqlc.arg(labels),
           sqlc.arg(heartbeat),
           sqlc.arg(worker_version),
           sqlc.arg(protocol_version),
           observed_runtime.runtime_id,
           observed_runtime.runtime_arch,
           observed_runtime.runtime_abi,
           observed_runtime.kernel_digest,
           observed_runtime.initramfs_digest,
           observed_runtime.rootfs_digest,
           observed_runtime.cni_profile,
           now()
      FROM observed_runtime
    ON CONFLICT (worker_group_id, resource_id) DO UPDATE
       SET status = CASE
               WHEN worker_instances.status IN ('draining', 'unschedulable') THEN worker_instances.status
               ELSE 'active'
           END,
           region = excluded.region,
           total_milli_cpu = excluded.total_milli_cpu,
           total_memory_mib = excluded.total_memory_mib,
           total_disk_mib = excluded.total_disk_mib,
           total_execution_slots = excluded.total_execution_slots,
           available_milli_cpu = excluded.available_milli_cpu,
           available_memory_mib = excluded.available_memory_mib,
           available_disk_mib = excluded.available_disk_mib,
           available_execution_slots = excluded.available_execution_slots,
           labels = excluded.labels,
           heartbeat = excluded.heartbeat,
           worker_version = excluded.worker_version,
           protocol_version = excluded.protocol_version,
           runtime_id = excluded.runtime_id,
           runtime_arch = excluded.runtime_arch,
           runtime_abi = excluded.runtime_abi,
           kernel_digest = excluded.kernel_digest,
           initramfs_digest = excluded.initramfs_digest,
           rootfs_digest = excluded.rootfs_digest,
           cni_profile = excluded.cni_profile,
           last_seen_at = now()
    RETURNING *
)
SELECT upserted_worker.*
  FROM upserted_worker;

-- name: EnsureRuntimeReleaseSelection :exec
WITH selected_runtime AS (
    SELECT runtime_releases.runtime_id
      FROM runtime_releases
     WHERE runtime_releases.runtime_id = sqlc.arg(runtime_id)
),
updated_selection AS (
    UPDATE runtime_release_selections
       SET runtime_id = selected_runtime.runtime_id,
           selected_at = now()
      FROM selected_runtime
    RETURNING runtime_release_selections.runtime_id
)
INSERT INTO runtime_release_selections (runtime_id)
SELECT selected_runtime.runtime_id
  FROM selected_runtime
 WHERE NOT EXISTS (SELECT 1 FROM updated_selection);

-- name: SetWorkerInstanceStatus :one
UPDATE worker_instances
   SET status = sqlc.arg(status)::worker_instance_status,
       drained_at = CASE
           WHEN sqlc.arg(status)::worker_instance_status = 'draining' THEN COALESCE(drained_at, now())
           ELSE drained_at
       END
 WHERE worker_instances.id = sqlc.arg(id)
RETURNING *;

-- name: ListWorkerInstances :many
SELECT worker_instances.*
  FROM worker_instances
 WHERE (
       sqlc.arg(status_filter)::text = 'all'
       OR worker_instances.status::text = sqlc.arg(status_filter)::text
   )
 ORDER BY worker_instances.last_seen_at DESC, worker_instances.first_seen_at ASC
 LIMIT sqlc.arg(row_limit);

-- name: GetWorkerInstanceState :one
SELECT worker_instances.*,
       (
           SELECT count(*)::int
             FROM run_leases
            WHERE run_leases.worker_instance_id = worker_instances.id
              AND run_leases.status IN ('leased', 'running')
       ) AS active_executions
  FROM worker_instances
 WHERE worker_instances.id = sqlc.arg(id);

-- name: GetWorkerInstanceQueueCapacity :one
SELECT GREATEST(worker_instances.available_milli_cpu - active.used_milli_cpu, 0)::bigint AS available_milli_cpu,
       GREATEST(worker_instances.available_memory_mib - active.used_memory_mib, 0)::bigint AS available_memory_mib,
       GREATEST(worker_instances.available_disk_mib - active.used_disk_mib, 0)::bigint AS available_disk_mib,
       GREATEST(worker_instances.available_execution_slots - active.used_slots, 0)::int AS available_execution_slots
  FROM worker_instances
  LEFT JOIN LATERAL (
      SELECT COALESCE(sum(run_runtime_requirements.requested_milli_cpu), 0)::bigint AS used_milli_cpu,
             COALESCE(sum(run_runtime_requirements.requested_memory_mib), 0)::bigint AS used_memory_mib,
             COALESCE(sum(run_runtime_requirements.requested_disk_mib), 0)::bigint AS used_disk_mib,
             COALESCE(sum(run_runtime_requirements.requested_execution_slots), 0)::int AS used_slots
        FROM run_leases
        JOIN run_runtime_requirements ON run_runtime_requirements.org_id = run_leases.org_id
                             AND run_runtime_requirements.run_id = run_leases.run_id
       WHERE run_leases.worker_instance_id = worker_instances.id
         AND run_leases.status IN ('leased', 'running')
  ) active ON true
 WHERE worker_instances.id = sqlc.arg(id)
   AND worker_instances.status = 'active';

-- name: UpsertRunRuntimeRequirements :one
INSERT INTO run_runtime_requirements (
    run_id,
    org_id,
    requested_milli_cpu,
    requested_memory_mib,
    requested_disk_mib,
    requested_execution_slots,
    runtime_id,
    runtime_arch,
    runtime_abi,
    kernel_digest,
    initramfs_digest,
    rootfs_digest,
    cni_profile,
    network_policy,
    placement,
    worker_group_id
)
SELECT sqlc.arg(run_id),
       sqlc.arg(org_id),
       sqlc.arg(requested_milli_cpu),
       sqlc.arg(requested_memory_mib),
       sqlc.arg(requested_disk_mib),
       sqlc.arg(requested_execution_slots),
       sqlc.arg(runtime_id),
       sqlc.arg(runtime_arch),
       sqlc.arg(runtime_abi),
       sqlc.arg(kernel_digest),
       sqlc.arg(initramfs_digest),
       sqlc.arg(rootfs_digest),
       sqlc.arg(cni_profile),
       sqlc.arg(network_policy),
       sqlc.arg(placement),
       sqlc.arg(worker_group_id)
ON CONFLICT (run_id) DO UPDATE
   SET requested_milli_cpu = excluded.requested_milli_cpu,
       requested_memory_mib = excluded.requested_memory_mib,
       requested_disk_mib = excluded.requested_disk_mib,
       requested_execution_slots = excluded.requested_execution_slots,
       runtime_id = excluded.runtime_id,
       runtime_arch = excluded.runtime_arch,
       runtime_abi = excluded.runtime_abi,
       kernel_digest = excluded.kernel_digest,
       initramfs_digest = excluded.initramfs_digest,
       rootfs_digest = excluded.rootfs_digest,
       cni_profile = excluded.cni_profile,
       network_policy = excluded.network_policy,
       placement = excluded.placement,
       worker_group_id = excluded.worker_group_id,
       updated_at = now()
RETURNING *;

-- name: UpsertRunQueueItemQueued :one
INSERT INTO run_queue_items (
    run_id,
    org_id,
    status,
    priority,
    queue_name,
    concurrency_key,
    queue_timestamp,
    queued_expires_at,
    dispatch_message_id,
    reservation_expires_at,
    last_error,
    enqueued_at,
    updated_at,
    finished_at
) VALUES (
    sqlc.arg(run_id),
    sqlc.arg(org_id),
    'queued',
    sqlc.arg(priority),
    sqlc.arg(queue_name),
    sqlc.narg(concurrency_key),
    sqlc.arg(queue_timestamp),
    sqlc.narg(queued_expires_at),
    sqlc.arg(dispatch_message_id),
    NULL,
    '',
    now(),
    now(),
    NULL
)
ON CONFLICT (run_id) DO UPDATE
   SET status = 'queued',
       priority = excluded.priority,
       queue_name = excluded.queue_name,
       concurrency_key = excluded.concurrency_key,
       queue_timestamp = excluded.queue_timestamp,
       queued_expires_at = excluded.queued_expires_at,
       dispatch_message_id = excluded.dispatch_message_id,
       reserved_by_worker_instance_id = NULL,
       reservation_expires_at = NULL,
       dispatch_generation = run_queue_items.dispatch_generation + 1,
       last_error = '',
       enqueued_at = now(),
       updated_at = now(),
       finished_at = NULL
RETURNING *;

-- name: PrepareQueuedRunQueueItem :one
WITH target_run AS (
    SELECT id,
           org_id,
           project_id,
           environment_id,
           deployment_id,
           deployment_task_id,
           queue_name,
           queue_concurrency_limit,
           concurrency_key,
           priority,
           queue_timestamp,
           queued_expires_at,
           created_at
      FROM runs
     WHERE runs.org_id = sqlc.arg(org_id)
       AND runs.id = sqlc.arg(run_id)
       AND runs.status = 'queued'
       AND runs.current_run_lease_id IS NULL
       AND EXISTS (
           SELECT 1
             FROM task_sessions
            WHERE task_sessions.org_id = runs.org_id
              AND task_sessions.project_id = runs.project_id
              AND task_sessions.environment_id = runs.environment_id
              AND task_sessions.id = runs.task_session_id
              AND task_sessions.current_run_id = runs.id
              AND task_sessions.status = 'open'
       )
),
existing_requirements AS (
    SELECT run_runtime_requirements.*
      FROM run_runtime_requirements
      JOIN target_run ON target_run.org_id = run_runtime_requirements.org_id
                     AND target_run.id = run_runtime_requirements.run_id
     LIMIT 1
),
selected_runtime AS (
    SELECT runtime_releases.runtime_id,
           runtime_releases.runtime_arch,
           runtime_releases.runtime_abi,
           runtime_releases.kernel_digest,
           runtime_releases.initramfs_digest,
           runtime_releases.rootfs_digest,
           runtime_releases.cni_profile
      FROM runtime_releases
      JOIN runtime_release_selections ON runtime_release_selections.runtime_id = runtime_releases.runtime_id
     LIMIT 1
),
inserted_requirements AS (
    INSERT INTO run_runtime_requirements (
        run_id,
        org_id,
        requested_milli_cpu,
        requested_memory_mib,
        requested_disk_mib,
        runtime_id,
        runtime_arch,
        runtime_abi,
        kernel_digest,
        initramfs_digest,
        rootfs_digest,
        cni_profile,
        network_policy,
        worker_group_id
    )
    SELECT target_run.id,
           target_run.org_id,
           deployment_tasks.requested_milli_cpu,
           deployment_tasks.requested_memory_mib,
           deployment_tasks.requested_disk_mib,
           selected_runtime.runtime_id,
           selected_runtime.runtime_arch,
           selected_runtime.runtime_abi,
           selected_runtime.kernel_digest,
           selected_runtime.initramfs_digest,
           selected_runtime.rootfs_digest,
           selected_runtime.cni_profile,
           deployment_tasks.network_policy,
           deployments.worker_group_id
      FROM target_run
      JOIN deployment_tasks ON deployment_tasks.org_id = target_run.org_id
                           AND deployment_tasks.deployment_id = target_run.deployment_id
                           AND deployment_tasks.id = target_run.deployment_task_id
      JOIN deployments ON deployments.org_id = target_run.org_id
                      AND deployments.id = target_run.deployment_id
      JOIN selected_runtime ON true
     WHERE NOT EXISTS (SELECT 1 FROM existing_requirements)
    ON CONFLICT (run_id) DO NOTHING
    RETURNING *
),
requirements AS (
    SELECT * FROM existing_requirements
    UNION ALL
    SELECT * FROM inserted_requirements
    LIMIT 1
),
dispatch AS (
    INSERT INTO run_queue_items (
        run_id,
        org_id,
        status,
        priority,
        queue_name,
        concurrency_key,
        queue_timestamp,
        queued_expires_at,
        dispatch_message_id,
        reservation_expires_at,
        last_error,
        enqueued_at,
        updated_at,
        finished_at
    )
    SELECT target_run.id,
           target_run.org_id,
           'queued',
           target_run.priority,
           target_run.queue_name,
           target_run.concurrency_key,
           target_run.queue_timestamp,
           target_run.queued_expires_at,
           NULL,
           NULL,
           '',
           now(),
           now(),
           NULL
      FROM target_run
      JOIN requirements ON requirements.org_id = target_run.org_id
                       AND requirements.run_id = target_run.id
    ON CONFLICT (run_id) DO UPDATE
       SET status = 'queued',
           priority = excluded.priority,
           queue_name = excluded.queue_name,
           concurrency_key = excluded.concurrency_key,
           queue_timestamp = excluded.queue_timestamp,
           queued_expires_at = excluded.queued_expires_at,
           dispatch_message_id = NULL,
           reserved_by_worker_instance_id = NULL,
           reservation_expires_at = NULL,
           dispatch_generation = run_queue_items.dispatch_generation + 1,
           last_error = '',
           enqueued_at = now(),
           updated_at = now(),
           finished_at = NULL
     WHERE run_queue_items.status = 'queued'
        OR (
            run_queue_items.status = 'published'
            AND run_queue_items.enqueued_at <= now() - interval '1 minute'
        )
        OR (
            run_queue_items.status = 'reserved'
            AND run_queue_items.reservation_expires_at <= now()
        )
    RETURNING *
)
SELECT
    target_run.id AS run_id,
    target_run.org_id,
    target_run.project_id,
    target_run.environment_id,
    dispatch.queue_name,
    target_run.queue_concurrency_limit,
    dispatch.priority,
    dispatch.concurrency_key,
    dispatch.queue_timestamp,
    dispatch.queued_expires_at,
    dispatch.dispatch_generation,
    dispatch.enqueued_at,
    requirements.requested_milli_cpu,
    requirements.requested_memory_mib,
    requirements.requested_disk_mib,
    requirements.requested_execution_slots,
    requirements.runtime_id,
    requirements.runtime_arch,
    requirements.runtime_abi,
    requirements.kernel_digest,
    requirements.initramfs_digest,
    requirements.rootfs_digest,
    requirements.cni_profile,
    requirements.network_policy,
    requirements.placement
  FROM target_run
  JOIN requirements ON requirements.org_id = target_run.org_id
                   AND requirements.run_id = target_run.id
  JOIN dispatch ON dispatch.org_id = target_run.org_id
               AND dispatch.run_id = target_run.id;

-- name: ListQueuedRunCandidateScopes :many
WITH candidate_scopes AS (
    SELECT runs.org_id,
           runs.project_id,
           runs.environment_id,
           COALESCE(run_queue_items.queue_name, runs.queue_name) AS queue_name,
           md5(runs.org_id::text || ':' || runs.project_id::text || ':' || runs.environment_id::text || ':' || COALESCE(run_queue_items.queue_name, runs.queue_name) || ':' || sqlc.arg(scan_seed)::text) AS sort_key
      FROM runs
      LEFT JOIN run_queue_items ON run_queue_items.org_id = runs.org_id
                               AND run_queue_items.run_id = runs.id
     WHERE runs.status = 'queued'
       AND runs.current_run_lease_id IS NULL
       AND runs.queue_timestamp <= now()
       AND (runs.queued_expires_at IS NULL OR runs.queued_expires_at > now())
       AND EXISTS (
           SELECT 1
             FROM task_sessions
            WHERE task_sessions.org_id = runs.org_id
              AND task_sessions.project_id = runs.project_id
              AND task_sessions.environment_id = runs.environment_id
              AND task_sessions.id = runs.task_session_id
              AND task_sessions.current_run_id = runs.id
              AND task_sessions.status = 'open'
       )
       AND (
           run_queue_items.run_id IS NULL
           OR (
               run_queue_items.status = 'queued'
               AND (
                   run_queue_items.dispatch_message_id IS NULL
                   OR run_queue_items.last_error <> ''
                   OR run_queue_items.enqueued_at <= now() - interval '1 minute'
               )
           )
           OR (
               run_queue_items.status = 'published'
               AND run_queue_items.enqueued_at <= now() - interval '1 minute'
           )
           OR (
               run_queue_items.status = 'reserved'
               AND run_queue_items.reservation_expires_at <= now()
           )
       )
     GROUP BY runs.org_id,
              runs.project_id,
              runs.environment_id,
              COALESCE(run_queue_items.queue_name, runs.queue_name)
)
SELECT candidate_scopes.org_id,
       candidate_scopes.project_id,
       candidate_scopes.environment_id,
       candidate_scopes.queue_name,
       candidate_scopes.sort_key
  FROM candidate_scopes
 WHERE sqlc.arg(after_sort_key)::text = ''
    OR (candidate_scopes.sort_key, candidate_scopes.org_id, candidate_scopes.project_id, candidate_scopes.environment_id, candidate_scopes.queue_name) > (sqlc.arg(after_sort_key)::text, sqlc.arg(after_org_id)::uuid, sqlc.arg(after_project_id)::uuid, sqlc.arg(after_environment_id)::uuid, sqlc.arg(after_queue_name)::text)
 ORDER BY candidate_scopes.sort_key ASC,
          candidate_scopes.org_id ASC,
          candidate_scopes.project_id ASC,
          candidate_scopes.environment_id ASC,
          candidate_scopes.queue_name ASC
 LIMIT sqlc.arg(row_limit);

-- name: ListQueuedRunQueueItemCandidatesForScope :many
SELECT runs.org_id,
       runs.id AS run_id,
       COALESCE(run_queue_items.dispatch_message_id, '') AS dispatch_message_id
  FROM runs
  LEFT JOIN run_queue_items ON run_queue_items.org_id = runs.org_id
                           AND run_queue_items.run_id = runs.id
 WHERE runs.org_id = sqlc.arg(org_id)
   AND runs.project_id = sqlc.arg(project_id)
   AND runs.environment_id = sqlc.arg(environment_id)
   AND COALESCE(run_queue_items.queue_name, runs.queue_name) = sqlc.arg(queue_name)
   AND runs.status = 'queued'
   AND runs.current_run_lease_id IS NULL
   AND runs.queue_timestamp <= now()
   AND (runs.queued_expires_at IS NULL OR runs.queued_expires_at > now())
   AND EXISTS (
       SELECT 1
         FROM task_sessions
        WHERE task_sessions.org_id = runs.org_id
          AND task_sessions.project_id = runs.project_id
          AND task_sessions.environment_id = runs.environment_id
          AND task_sessions.id = runs.task_session_id
          AND task_sessions.current_run_id = runs.id
          AND task_sessions.status = 'open'
   )
   AND (
       run_queue_items.run_id IS NULL
       OR (
           run_queue_items.status = 'queued'
           AND (
               run_queue_items.dispatch_message_id IS NULL
               OR run_queue_items.last_error <> ''
               OR run_queue_items.enqueued_at <= now() - interval '1 minute'
           )
       )
       OR (
           run_queue_items.status = 'published'
           AND run_queue_items.enqueued_at <= now() - interval '1 minute'
       )
       OR (
           run_queue_items.status = 'reserved'
           AND run_queue_items.reservation_expires_at <= now()
       )
   )
 ORDER BY runs.priority DESC, runs.queue_timestamp ASC, runs.id ASC
 LIMIT sqlc.arg(row_limit);

-- name: MarkRunQueueItemEnqueueError :one
UPDATE run_queue_items
   SET last_error = sqlc.arg(last_error),
       updated_at = now()
 WHERE org_id = sqlc.arg(org_id)
   AND run_id = sqlc.arg(run_id)
   AND status = 'queued'
   AND dispatch_generation = sqlc.arg(expected_dispatch_generation)
RETURNING *;

-- name: MarkRunQueueItemEnqueued :one
UPDATE run_queue_items
   SET status = 'published',
       dispatch_message_id = sqlc.arg(dispatch_message_id),
       last_error = '',
       enqueued_at = now(),
       updated_at = now()
 WHERE org_id = sqlc.arg(org_id)
   AND run_id = sqlc.arg(run_id)
   AND status = 'queued'
   AND dispatch_generation = sqlc.arg(expected_dispatch_generation)
RETURNING *;

-- name: ReserveRunQueueItem :one
UPDATE run_queue_items
   SET status = 'reserved',
       dispatch_message_id = sqlc.arg(dispatch_message_id),
       reserved_by_worker_instance_id = sqlc.arg(worker_instance_id),
       reservation_expires_at = sqlc.arg(reservation_expires_at),
       queued_expires_at = NULL,
       dispatch_generation = dispatch_generation + 1,
       updated_at = now(),
       finished_at = NULL
 WHERE run_queue_items.org_id = sqlc.arg(org_id)
   AND run_queue_items.run_id = sqlc.arg(run_id)
   AND (
       run_queue_items.status = 'published'
       OR (
           run_queue_items.status = 'reserved'
           AND run_queue_items.reservation_expires_at <= now()
       )
   )
   AND run_queue_items.dispatch_message_id = sqlc.arg(dispatch_message_id)
   AND (run_queue_items.queued_expires_at IS NULL OR run_queue_items.queued_expires_at > now())
   AND EXISTS (
       SELECT 1
         FROM runs
        WHERE runs.org_id = run_queue_items.org_id
          AND runs.id = run_queue_items.run_id
          AND runs.status = 'queued'
          AND runs.current_run_lease_id IS NULL
       AND EXISTS (
           SELECT 1
             FROM task_sessions
            WHERE task_sessions.org_id = runs.org_id
              AND task_sessions.project_id = runs.project_id
              AND task_sessions.environment_id = runs.environment_id
              AND task_sessions.id = runs.task_session_id
              AND task_sessions.current_run_id = runs.id
              AND task_sessions.status = 'open'
       )
   )
RETURNING *;

-- name: IsRunQueueLeaseConflict :one
SELECT EXISTS (
    SELECT 1
      FROM run_queue_items
     WHERE org_id = sqlc.arg(org_id)
       AND run_id = sqlc.arg(run_id)
       AND dispatch_message_id = sqlc.arg(dispatch_message_id)
       AND status = 'reserved'
       AND reservation_expires_at > now()
) AS lease_conflict;

-- name: RunLeaseDispatchAttemptsExhausted :one
SELECT count(*) >= sqlc.arg(max_dispatch_attempts)::int AS exhausted
  FROM run_leases
 WHERE org_id = sqlc.arg(org_id)
   AND run_id = sqlc.arg(run_id)
   AND status = 'lost';

-- name: RenewRunQueueReservation :one
UPDATE run_queue_items
   SET reservation_expires_at = sqlc.arg(reservation_expires_at),
       updated_at = now()
 WHERE org_id = sqlc.arg(org_id)
   AND run_id = sqlc.arg(run_id)
   AND reserved_by_worker_instance_id = sqlc.arg(worker_instance_id)
   AND dispatch_message_id = sqlc.arg(dispatch_message_id)
   AND status = 'reserved'
   AND reservation_expires_at > now()
RETURNING *;

-- name: CompleteRunQueueItem :one
UPDATE run_queue_items
   SET status = 'completed',
       dispatch_generation = dispatch_generation + 1,
       updated_at = now(),
       finished_at = now()
 WHERE org_id = sqlc.arg(org_id)
   AND run_id = sqlc.arg(run_id)
   AND reserved_by_worker_instance_id = sqlc.arg(worker_instance_id)
   AND dispatch_message_id = sqlc.arg(dispatch_message_id)
   AND status = 'reserved'
   AND reservation_expires_at > now()
RETURNING *;

-- name: RequeueRunQueueItem :one
UPDATE run_queue_items
   SET status = 'queued',
       dispatch_message_id = NULL,
       reserved_by_worker_instance_id = NULL,
       reservation_expires_at = NULL,
       dispatch_generation = dispatch_generation + 1,
       last_error = sqlc.arg(last_error),
       enqueued_at = now(),
       updated_at = now(),
       finished_at = NULL
 WHERE org_id = sqlc.arg(org_id)
   AND run_id = sqlc.arg(run_id)
   AND reserved_by_worker_instance_id = sqlc.arg(worker_instance_id)
   AND dispatch_message_id = sqlc.arg(dispatch_message_id)
   AND status = 'reserved'
   AND reservation_expires_at > now()
RETURNING *;

-- name: DeadLetterRunQueueItem :one
WITH locked_task_session AS MATERIALIZED (
    SELECT task_sessions.id
      FROM runs
      JOIN task_sessions
        ON task_sessions.org_id = runs.org_id
       AND task_sessions.project_id = runs.project_id
       AND task_sessions.environment_id = runs.environment_id
       AND task_sessions.id = runs.task_session_id
     WHERE runs.org_id = sqlc.arg(org_id)
       AND runs.id = sqlc.arg(run_id)
     FOR UPDATE OF task_sessions
),
queue_entry AS (
    UPDATE run_queue_items
       SET status = 'dead_lettered',
           reserved_by_worker_instance_id = NULL,
           reservation_expires_at = NULL,
           dispatch_generation = dispatch_generation + 1,
           last_error = sqlc.arg(last_error),
           updated_at = now(),
           finished_at = now()
     WHERE run_queue_items.org_id = sqlc.arg(org_id)
       AND run_queue_items.run_id = sqlc.arg(run_id)
       AND run_queue_items.dispatch_message_id = sqlc.arg(dispatch_message_id)
       AND run_queue_items.status IN ('queued', 'published', 'reserved')
       AND (
           NOT EXISTS (
               SELECT 1
                 FROM runs
                WHERE runs.org_id = sqlc.arg(org_id)
                  AND runs.id = sqlc.arg(run_id)
           )
           OR EXISTS (SELECT 1 FROM locked_task_session)
       )
    RETURNING *
),
	failed_run AS (
		    UPDATE runs
		       SET status = 'failed',
		           execution_status = 'finished',
		           terminal_outcome = 'dead_lettered',
		           current_run_lease_id = NULL,
	           error_message = sqlc.arg(last_error),
	           state_version = state_version + 1,
	           updated_at = now(),
	           finished_at = now()
	      FROM queue_entry
	     WHERE runs.org_id = queue_entry.org_id
	       AND runs.id = queue_entry.run_id
	       AND runs.status = 'queued'
	       AND runs.current_run_lease_id IS NULL
	    RETURNING runs.org_id, runs.project_id, runs.environment_id, runs.id, runs.task_session_id, runs.current_attempt_id, runs.current_attempt_number, runs.trace_id, runs.root_span_id, runs.state_version
	),
	failed_attempt AS (
	    UPDATE run_attempts
	       SET status = 'failed',
	           error_message = sqlc.arg(last_error),
	           finished_at = now(),
	           updated_at = now()
	      FROM failed_run
	     WHERE run_attempts.org_id = failed_run.org_id
	       AND run_attempts.run_id = failed_run.id
	       AND run_attempts.id = failed_run.current_attempt_id
	    RETURNING run_attempts.id, run_attempts.run_id
	),
	failed_snapshot AS (
		    INSERT INTO run_snapshots (org_id, run_id, version, status, execution_status, terminal_outcome, attempt_id, transition, reason)
		    SELECT failed_run.org_id,
		           failed_run.id,
		           failed_run.state_version,
		           'failed',
		           'finished',
		           'dead_lettered',
		           failed_run.current_attempt_id,
	           sqlc.arg(event_kind),
	           sqlc.arg(event_payload)
	      FROM failed_run
	      JOIN failed_attempt ON failed_attempt.run_id = failed_run.id
	    RETURNING run_snapshots.run_id
	),
	failed_session_runs AS (
	    UPDATE task_session_runs
	       SET ended_at = now()
      FROM failed_run
     WHERE task_session_runs.org_id = failed_run.org_id
	       AND task_session_runs.project_id = failed_run.project_id
	       AND task_session_runs.environment_id = failed_run.environment_id
	       AND task_session_runs.task_session_id = failed_run.task_session_id
	       AND task_session_runs.run_id = failed_run.id
	    RETURNING task_session_runs.id
	),
	failed_task_sessions AS (
	    UPDATE task_sessions
	       SET status = 'failed',
	           failed_at = now(),
	           result = jsonb_build_object(
	               'ok', false,
	               'error', jsonb_build_object(
	                   'name', 'TaskFailed',
	                   'message', COALESCE(NULLIF(sqlc.arg(last_error)::text, ''), 'run dead-lettered before execution'),
	                   'details', jsonb_build_object('origin', 'dead_letter')
	               )
	           ),
	           terminal_reason = jsonb_build_object('origin', 'dead_letter', 'message', COALESCE(NULLIF(sqlc.arg(last_error)::text, ''), 'run dead-lettered before execution')),
	           current_run_id = NULL,
	           current_run_version = task_sessions.current_run_version + 1,
	           updated_at = now()
      FROM failed_run
     WHERE task_sessions.org_id = failed_run.org_id
	       AND task_sessions.project_id = failed_run.project_id
	       AND task_sessions.environment_id = failed_run.environment_id
	       AND task_sessions.id = failed_run.task_session_id
	       AND task_sessions.current_run_id = failed_run.id
	       AND task_sessions.status = 'open'
	    RETURNING task_sessions.id
	),
	run_event_seq AS (
	    INSERT INTO event_subject_cursors (org_id, subject_type, subject_id, last_seq)
	    SELECT failed_run.org_id, 'run', failed_run.id, 1
	      FROM failed_run
	      JOIN failed_snapshot ON failed_snapshot.run_id = failed_run.id
	    ON CONFLICT (org_id, subject_type, subject_id)
	    DO UPDATE SET last_seq = event_subject_cursors.last_seq + 1,
	                  updated_at = now()
	    RETURNING org_id, subject_type, subject_id, last_seq
	),
	run_event AS (
	    INSERT INTO events (org_id, project_id, environment_id, run_id, seq, attempt_id, attempt_number, trace_id, span_id, traceparent, category, severity, source, kind, message, payload, redaction_class, snapshot_version)
	    SELECT failed_run.org_id,
	           failed_run.project_id,
	           failed_run.environment_id,
	           failed_run.id,
	           run_event_seq.last_seq,
	           failed_run.current_attempt_id,
	           failed_run.current_attempt_number,
	           failed_run.trace_id,
	           failed_run.root_span_id,
	           '00-' || failed_run.trace_id || '-' || failed_run.root_span_id || '-01',
	           'lifecycle',
	           'error',
	           'dispatcher',
	           sqlc.arg(event_kind),
	           sqlc.arg(event_kind),
	           sqlc.arg(event_payload),
	           'internal',
	           failed_run.state_version
	      FROM failed_run
	      JOIN run_event_seq ON run_event_seq.org_id = failed_run.org_id
	                        AND run_event_seq.subject_type = 'run'
	                        AND run_event_seq.subject_id = failed_run.id
	    RETURNING *
	),
	run_event_outbox AS (
	    INSERT INTO event_outbox (event_record_id, stream_key)
	    SELECT run_event.id,
	           'helmr:events:' || run_event.org_id::text || ':' || run_event.subject_type::text || ':' || run_event.subject_id::text
	      FROM run_event
	    RETURNING id
	),
existing_dead_letter AS (
    SELECT run_queue_items.*
      FROM run_queue_items
     WHERE run_queue_items.org_id = sqlc.arg(org_id)
       AND run_queue_items.run_id = sqlc.arg(run_id)
       AND run_queue_items.dispatch_message_id = sqlc.arg(dispatch_message_id)
       AND run_queue_items.status = 'dead_lettered'
)
SELECT queue_entry.*
  FROM queue_entry
  JOIN run_event_outbox ON true
 WHERE (SELECT count(*) FROM failed_session_runs) >= 0
   AND (SELECT count(*) FROM failed_task_sessions) >= 0
UNION ALL
SELECT existing_dead_letter.*
  FROM existing_dead_letter
 WHERE NOT EXISTS (SELECT 1 FROM queue_entry);
