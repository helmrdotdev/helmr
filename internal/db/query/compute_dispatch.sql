-- name: ListQueueScopes :many
SELECT run_queue_items.org_id,
       run_queue_items.cell_id,
       runs.project_id,
       runs.environment_id,
       run_queue_items.queue_class,
       run_queue_items.queue_name
  FROM run_queue_items
  JOIN runs ON runs.org_id = run_queue_items.org_id
           AND runs.id = run_queue_items.run_id
           AND runs.cell_id = run_queue_items.cell_id
           AND runs.route_generation = run_queue_items.route_generation
  JOIN environment_cells
    ON environment_cells.org_id = runs.org_id
   AND environment_cells.project_id = runs.project_id
   AND environment_cells.environment_id = runs.environment_id
   AND environment_cells.cell_id = runs.cell_id
   AND environment_cells.route_generation = runs.route_generation
   AND environment_cells.route_state IN ('active', 'draining')
  JOIN cells ON cells.id = environment_cells.cell_id
            AND cells.region_id = environment_cells.region_id
            AND cells.state IN ('active', 'draining')
  JOIN regions ON regions.id = environment_cells.region_id
              AND regions.state = 'available'
  JOIN org_cells ON org_cells.org_id = environment_cells.org_id
                AND org_cells.cell_id = environment_cells.cell_id
                AND org_cells.state = 'active'
  JOIN cell_health ON cell_health.cell_id = environment_cells.cell_id
                  AND cell_health.state IN ('healthy', 'degraded')
                  AND cell_health.routing_fresh_until > now()
  JOIN run_runtime_requirements ON run_runtime_requirements.org_id = run_queue_items.org_id
                               AND run_runtime_requirements.run_id = run_queue_items.run_id
  JOIN worker_groups ON worker_groups.id = sqlc.arg(worker_group_id)
                    AND worker_groups.cell_id = run_queue_items.cell_id
                    AND worker_groups.state = 'active'
 WHERE run_queue_items.status IN ('queued', 'published', 'reserved')
   AND run_runtime_requirements.worker_group_id = sqlc.arg(worker_group_id)
 GROUP BY run_queue_items.org_id,
          run_queue_items.cell_id,
          runs.project_id,
          runs.environment_id,
          run_queue_items.queue_class,
          run_queue_items.queue_name
 ORDER BY md5(run_queue_items.org_id::text || ':' || run_queue_items.cell_id || ':' || runs.project_id::text || ':' || runs.environment_id::text || ':' || run_queue_items.queue_class || ':' || run_queue_items.queue_name || ':' || sqlc.arg(scan_seed)::text),
          run_queue_items.org_id ASC,
          run_queue_items.cell_id ASC,
          runs.project_id ASC,
          runs.environment_id ASC,
          run_queue_items.queue_class ASC,
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
        cell_id,
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
           sqlc.arg(cell_id),
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
     WHERE worker_instances.cell_id = excluded.cell_id
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
   AND worker_instances.cell_id = sqlc.arg(cell_id)
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
 WHERE worker_instances.id = sqlc.arg(id)
   AND worker_instances.cell_id = sqlc.arg(cell_id);

-- name: GetWorkerInstanceQueueCapacity :one
SELECT GREATEST(worker_instances.available_milli_cpu - active.used_milli_cpu - active_runtime_instances.used_milli_cpu, 0)::bigint AS available_milli_cpu,
       GREATEST(worker_instances.available_memory_mib - active.used_memory_mib - active_runtime_instances.used_memory_mib, 0)::bigint AS available_memory_mib,
       GREATEST(worker_instances.available_disk_mib - active.used_disk_mib - active_runtime_instances.used_disk_mib, 0)::bigint AS available_disk_mib,
       GREATEST(worker_instances.available_execution_slots - active.used_slots - active_runtime_instances.used_slots, 0)::int AS available_execution_slots
  FROM worker_instances
  LEFT JOIN LATERAL (
      SELECT COALESCE(sum(run_runtime_requirements.requested_milli_cpu), 0)::bigint AS used_milli_cpu,
             COALESCE(sum(run_runtime_requirements.requested_memory_mib), 0)::bigint AS used_memory_mib,
             COALESCE(sum(run_runtime_requirements.requested_disk_mib), 0)::bigint AS used_disk_mib,
             COALESCE(sum(run_runtime_requirements.requested_execution_slots), 0)::int AS used_slots
        FROM run_leases
        JOIN runs ON runs.org_id = run_leases.org_id
                 AND runs.id = run_leases.run_id
                 AND runs.workspace_mount_id IS NULL
        JOIN run_runtime_requirements ON run_runtime_requirements.org_id = run_leases.org_id
                             AND run_runtime_requirements.run_id = run_leases.run_id
       WHERE run_leases.worker_instance_id = worker_instances.id
         AND run_leases.status IN ('leased', 'running')
  ) active ON true
  LEFT JOIN LATERAL (
      SELECT COALESCE(sum(runtime_instances.reserved_cpu_millis), 0)::bigint AS used_milli_cpu,
             COALESCE(sum(runtime_instances.reserved_memory_mib), 0)::bigint AS used_memory_mib,
             COALESCE(sum(runtime_instances.reserved_disk_mib), 0)::bigint AS used_disk_mib,
             COALESCE(sum(runtime_instances.reserved_execution_slots), 0)::int AS used_slots
        FROM runtime_instances
       WHERE runtime_instances.worker_instance_id = worker_instances.id
         AND runtime_instances.state IN ('preparing', 'ready', 'binding', 'running', 'waiting_hot', 'checkpointing', 'stopping')
         AND (
             runtime_instances.expires_at IS NULL
             OR runtime_instances.expires_at > now()
         )
  ) active_runtime_instances ON true
 WHERE worker_instances.id = sqlc.arg(id)
   AND worker_instances.cell_id = sqlc.arg(cell_id)
   AND worker_instances.status = 'active';

-- name: GetWorkerInstanceRunDispatchCapacity :one
SELECT GREATEST(worker_instances.available_milli_cpu - active.used_milli_cpu - active_runtime_instances.used_milli_cpu, 0)::bigint AS available_milli_cpu,
       GREATEST(worker_instances.available_memory_mib - active.used_memory_mib - active_runtime_instances.used_memory_mib, 0)::bigint AS available_memory_mib,
       GREATEST(worker_instances.available_disk_mib - active.used_disk_mib - active_runtime_instances.used_disk_mib, 0)::bigint AS available_disk_mib,
       GREATEST(worker_instances.available_execution_slots - active.used_slots - active_runtime_instances.used_slots, 0)::int AS available_execution_slots
  FROM worker_instances
  LEFT JOIN LATERAL (
      SELECT COALESCE(sum(run_runtime_requirements.requested_milli_cpu), 0)::bigint AS used_milli_cpu,
             COALESCE(sum(run_runtime_requirements.requested_memory_mib), 0)::bigint AS used_memory_mib,
             COALESCE(sum(run_runtime_requirements.requested_disk_mib), 0)::bigint AS used_disk_mib,
             COALESCE(sum(run_runtime_requirements.requested_execution_slots), 0)::int AS used_slots
        FROM run_leases
        JOIN runs ON runs.org_id = run_leases.org_id
                 AND runs.id = run_leases.run_id
                 AND runs.workspace_mount_id IS NULL
        JOIN run_runtime_requirements ON run_runtime_requirements.org_id = run_leases.org_id
                             AND run_runtime_requirements.run_id = run_leases.run_id
       WHERE run_leases.worker_instance_id = worker_instances.id
         AND run_leases.status IN ('leased', 'running')
  ) active ON true
  LEFT JOIN LATERAL (
      SELECT COALESCE(sum(runtime_instances.reserved_cpu_millis), 0)::bigint AS used_milli_cpu,
             COALESCE(sum(runtime_instances.reserved_memory_mib), 0)::bigint AS used_memory_mib,
             COALESCE(sum(runtime_instances.reserved_disk_mib), 0)::bigint AS used_disk_mib,
             COALESCE(sum(runtime_instances.reserved_execution_slots), 0)::int AS used_slots
        FROM runtime_instances
       WHERE runtime_instances.worker_instance_id = worker_instances.id
         AND runtime_instances.state IN ('preparing', 'ready', 'binding', 'running', 'waiting_hot', 'checkpointing', 'stopping')
         AND (
             runtime_instances.expires_at IS NULL
             OR runtime_instances.expires_at > now()
         )
  ) active_runtime_instances ON true
 WHERE worker_instances.id = sqlc.arg(id)
   AND worker_instances.cell_id = sqlc.arg(cell_id)
   AND worker_instances.status = 'active';

-- name: UpsertRunRuntimeRequirements :one
INSERT INTO run_runtime_requirements (
    run_id,
    org_id,
    cell_id,
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
       runs.cell_id,
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
  FROM runs
 WHERE runs.org_id = sqlc.arg(org_id)
   AND runs.id = sqlc.arg(run_id)
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
WITH upserted AS (
    INSERT INTO run_queue_items (
        run_id,
        org_id,
        cell_id,
        route_generation,
        queue_class,
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
    SELECT sqlc.arg(run_id),
        sqlc.arg(org_id),
        runs.cell_id,
        sqlc.arg(route_generation),
        'default',
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
      FROM runs
     WHERE runs.org_id = sqlc.arg(org_id)
       AND runs.id = sqlc.arg(run_id)
    ON CONFLICT (run_id) DO UPDATE
       SET status = 'queued',
           cell_id = excluded.cell_id,
           route_generation = excluded.route_generation,
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
    RETURNING run_queue_items.*
)
SELECT *
  FROM upserted;

-- name: PrepareQueuedRunQueueItem :one
WITH target_run AS (
    SELECT id,
           org_id,
           cell_id,
           route_generation,
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
           latest_runtime_checkpoint_id,
           created_at
      FROM runs
     WHERE runs.org_id = sqlc.arg(org_id)
       AND runs.id = sqlc.arg(run_id)
       AND runs.status = 'queued'
       AND runs.current_run_lease_id IS NULL
       AND EXISTS (
           SELECT 1
             FROM sessions
            WHERE sessions.org_id = runs.org_id
              AND sessions.project_id = runs.project_id
              AND sessions.environment_id = runs.environment_id
              AND sessions.id = runs.session_id
              AND sessions.current_run_id = runs.id
              AND sessions.status = 'open'
       )
),
record_route AS (
    SELECT environment_cells.org_id,
           environment_cells.project_id,
           environment_cells.environment_id,
           environment_cells.cell_id,
           environment_cells.route_generation
      FROM environment_cells
      JOIN target_run ON target_run.org_id = environment_cells.org_id
                     AND target_run.project_id = environment_cells.project_id
                     AND target_run.environment_id = environment_cells.environment_id
                     AND target_run.cell_id = environment_cells.cell_id
                     AND target_run.route_generation = environment_cells.route_generation
      JOIN cells ON cells.id = environment_cells.cell_id
                AND cells.region_id = environment_cells.region_id
      JOIN regions ON regions.id = environment_cells.region_id
      JOIN cell_health ON cell_health.cell_id = environment_cells.cell_id
     WHERE environment_cells.route_state IN ('active', 'draining')
       AND cells.state IN ('active', 'draining')
       AND regions.state = 'available'
       AND cell_health.state IN ('healthy', 'degraded')
       AND cell_health.routing_fresh_until > now()
       AND EXISTS (
           SELECT 1
             FROM org_cells
            WHERE org_cells.org_id = environment_cells.org_id
              AND org_cells.cell_id = environment_cells.cell_id
              AND org_cells.state = 'active'
       )
     ORDER BY environment_cells.route_generation DESC
     LIMIT 1
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
        cell_id,
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
    SELECT target_run.id,
           target_run.org_id,
           target_run.cell_id,
           deployment_tasks.requested_milli_cpu,
           deployment_tasks.requested_memory_mib,
           deployment_tasks.requested_disk_mib,
           deployment_tasks.requested_execution_slots,
           selected_runtime.runtime_id,
           selected_runtime.runtime_arch,
           selected_runtime.runtime_abi,
           selected_runtime.kernel_digest,
           selected_runtime.initramfs_digest,
           selected_runtime.rootfs_digest,
           selected_runtime.cni_profile,
           deployment_tasks.network_policy,
           deployment_tasks.placement,
           deployments.worker_group_id
      FROM target_run
      JOIN deployment_tasks ON deployment_tasks.org_id = target_run.org_id
                           AND deployment_tasks.project_id = target_run.project_id
                           AND deployment_tasks.environment_id = target_run.environment_id
                           AND deployment_tasks.deployment_id = target_run.deployment_id
                           AND deployment_tasks.id = target_run.deployment_task_id
      JOIN deployments ON deployments.org_id = target_run.org_id
                      AND deployments.project_id = target_run.project_id
                      AND deployments.environment_id = target_run.environment_id
                      AND deployments.id = target_run.deployment_id
                      AND deployments.build_cell_id = target_run.cell_id
                      AND deployments.build_route_generation = target_run.route_generation
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
source_worker_restore_scope AS (
    SELECT 1
      FROM target_run
      JOIN run_waits
        ON run_waits.org_id = target_run.org_id
       AND run_waits.project_id = target_run.project_id
       AND run_waits.environment_id = target_run.environment_id
       AND run_waits.run_id = target_run.id
       AND run_waits.state = 'resuming'
       AND run_waits.resuming_at >= now() - interval '8 seconds'
      JOIN runtime_checkpoints
        ON runtime_checkpoints.org_id = target_run.org_id
       AND runtime_checkpoints.project_id = target_run.project_id
       AND runtime_checkpoints.environment_id = target_run.environment_id
       AND runtime_checkpoints.run_id = target_run.id
       AND runtime_checkpoints.id = target_run.latest_runtime_checkpoint_id
       AND runtime_checkpoints.id = run_waits.runtime_checkpoint_id
       AND runtime_checkpoints.state = 'ready'
       AND (runtime_checkpoints.expires_at IS NULL OR runtime_checkpoints.expires_at > now())
      JOIN worker_instances AS source_worker
        ON source_worker.id = runtime_checkpoints.source_worker_instance_id
       AND source_worker.status = 'active'
     LIMIT 1
),
dispatch AS (
    INSERT INTO run_queue_items (
        run_id,
        org_id,
        cell_id,
        route_generation,
        queue_class,
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
           target_run.cell_id,
           target_run.route_generation,
           'default',
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
      JOIN record_route ON record_route.org_id = target_run.org_id
                       AND record_route.project_id = target_run.project_id
                       AND record_route.environment_id = target_run.environment_id
                       AND record_route.cell_id = target_run.cell_id
      JOIN requirements ON requirements.org_id = target_run.org_id
                       AND requirements.run_id = target_run.id
     WHERE NOT EXISTS (SELECT 1 FROM source_worker_restore_scope)
    ON CONFLICT (run_id) DO UPDATE
       SET status = 'queued',
           cell_id = excluded.cell_id,
           route_generation = excluded.route_generation,
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
    dispatch.cell_id,
    dispatch.route_generation,
    dispatch.queue_class,
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
           runs.cell_id,
           runs.project_id,
           runs.environment_id,
           COALESCE(run_queue_items.queue_class, 'default') AS queue_class,
           COALESCE(run_queue_items.queue_name, runs.queue_name) AS queue_name,
           md5(runs.org_id::text || ':' || runs.cell_id || ':' || runs.project_id::text || ':' || runs.environment_id::text || ':' || COALESCE(run_queue_items.queue_class, 'default') || ':' || COALESCE(run_queue_items.queue_name, runs.queue_name) || ':' || sqlc.arg(scan_seed)::text) AS sort_key
      FROM runs
      LEFT JOIN run_queue_items ON run_queue_items.org_id = runs.org_id
                               AND run_queue_items.run_id = runs.id
                               AND run_queue_items.cell_id = runs.cell_id
                               AND run_queue_items.route_generation = runs.route_generation
      JOIN environment_cells
        ON environment_cells.org_id = runs.org_id
       AND environment_cells.project_id = runs.project_id
       AND environment_cells.environment_id = runs.environment_id
       AND environment_cells.cell_id = runs.cell_id
       AND environment_cells.route_generation = runs.route_generation
       AND environment_cells.route_state IN ('active', 'draining')
      JOIN cells ON cells.id = environment_cells.cell_id
                AND cells.region_id = environment_cells.region_id
                AND cells.state IN ('active', 'draining')
      JOIN regions ON regions.id = environment_cells.region_id
                  AND regions.state = 'available'
      JOIN org_cells ON org_cells.org_id = environment_cells.org_id
                    AND org_cells.cell_id = environment_cells.cell_id
                    AND org_cells.state = 'active'
      JOIN cell_health ON cell_health.cell_id = environment_cells.cell_id
                      AND cell_health.state IN ('healthy', 'degraded')
                      AND cell_health.routing_fresh_until > now()
     WHERE runs.status = 'queued'
       AND runs.cell_id = sqlc.arg(cell_id)
       AND runs.current_run_lease_id IS NULL
       AND runs.queue_timestamp <= now()
       AND (runs.queued_expires_at IS NULL OR runs.queued_expires_at > now())
       AND EXISTS (
           SELECT 1
             FROM sessions
            WHERE sessions.org_id = runs.org_id
              AND sessions.project_id = runs.project_id
              AND sessions.environment_id = runs.environment_id
              AND sessions.id = runs.session_id
              AND sessions.current_run_id = runs.id
              AND sessions.status = 'open'
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
              runs.cell_id,
              runs.project_id,
              runs.environment_id,
              COALESCE(run_queue_items.queue_class, 'default'),
              COALESCE(run_queue_items.queue_name, runs.queue_name)
)
SELECT candidate_scopes.org_id,
       candidate_scopes.cell_id,
       candidate_scopes.project_id,
       candidate_scopes.environment_id,
       candidate_scopes.queue_class,
       candidate_scopes.queue_name,
       candidate_scopes.sort_key
  FROM candidate_scopes
 WHERE sqlc.arg(after_sort_key)::text = ''
    OR (candidate_scopes.sort_key, candidate_scopes.org_id, candidate_scopes.cell_id, candidate_scopes.project_id, candidate_scopes.environment_id, candidate_scopes.queue_class, candidate_scopes.queue_name) > (sqlc.arg(after_sort_key)::text, sqlc.arg(after_org_id)::uuid, sqlc.arg(after_cell_id)::text, sqlc.arg(after_project_id)::uuid, sqlc.arg(after_environment_id)::uuid, sqlc.arg(after_queue_class)::text, sqlc.arg(after_queue_name)::text)
 ORDER BY candidate_scopes.sort_key ASC,
          candidate_scopes.org_id ASC,
          candidate_scopes.cell_id ASC,
          candidate_scopes.project_id ASC,
          candidate_scopes.environment_id ASC,
          candidate_scopes.queue_class ASC,
          candidate_scopes.queue_name ASC
 LIMIT sqlc.arg(row_limit);

-- name: ListQueuedRunQueueItemCandidatesForScope :many
SELECT runs.org_id,
       runs.cell_id,
       runs.id AS run_id,
       COALESCE(run_queue_items.dispatch_message_id, '') AS dispatch_message_id
  FROM runs
  LEFT JOIN run_queue_items ON run_queue_items.org_id = runs.org_id
                           AND run_queue_items.run_id = runs.id
                           AND run_queue_items.cell_id = runs.cell_id
                           AND run_queue_items.route_generation = runs.route_generation
  JOIN environment_cells
    ON environment_cells.org_id = runs.org_id
   AND environment_cells.project_id = runs.project_id
   AND environment_cells.environment_id = runs.environment_id
   AND environment_cells.cell_id = runs.cell_id
   AND environment_cells.route_generation = runs.route_generation
   AND environment_cells.route_state IN ('active', 'draining')
  JOIN cells ON cells.id = environment_cells.cell_id
            AND cells.region_id = environment_cells.region_id
            AND cells.state IN ('active', 'draining')
  JOIN regions ON regions.id = environment_cells.region_id
              AND regions.state = 'available'
  JOIN org_cells ON org_cells.org_id = environment_cells.org_id
                AND org_cells.cell_id = environment_cells.cell_id
                AND org_cells.state = 'active'
  JOIN cell_health ON cell_health.cell_id = environment_cells.cell_id
                  AND cell_health.state IN ('healthy', 'degraded')
                  AND cell_health.routing_fresh_until > now()
 WHERE runs.org_id = sqlc.arg(org_id)
   AND runs.cell_id = sqlc.arg(cell_id)
   AND runs.project_id = sqlc.arg(project_id)
   AND runs.environment_id = sqlc.arg(environment_id)
   AND COALESCE(run_queue_items.queue_class, 'default') = sqlc.arg(queue_class)
   AND COALESCE(run_queue_items.queue_name, runs.queue_name) = sqlc.arg(queue_name)
   AND runs.status = 'queued'
   AND runs.current_run_lease_id IS NULL
   AND runs.queue_timestamp <= now()
   AND (runs.queued_expires_at IS NULL OR runs.queued_expires_at > now())
   AND EXISTS (
       SELECT 1
         FROM sessions
        WHERE sessions.org_id = runs.org_id
          AND sessions.project_id = runs.project_id
          AND sessions.environment_id = runs.environment_id
          AND sessions.id = runs.session_id
          AND sessions.current_run_id = runs.id
          AND sessions.status = 'open'
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
   AND cell_id = sqlc.arg(cell_id)
   AND route_generation = sqlc.arg(route_generation)
   AND queue_class = sqlc.arg(queue_class)
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
   AND cell_id = sqlc.arg(cell_id)
   AND route_generation = sqlc.arg(route_generation)
   AND queue_class = sqlc.arg(queue_class)
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
   AND run_queue_items.cell_id = sqlc.arg(cell_id)
   AND run_queue_items.route_generation = sqlc.arg(route_generation)
   AND run_queue_items.queue_class = sqlc.arg(queue_class)
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
          AND runs.cell_id = run_queue_items.cell_id
          AND runs.status = 'queued'
          AND runs.current_run_lease_id IS NULL
       AND EXISTS (
           SELECT 1
             FROM sessions
            WHERE sessions.org_id = runs.org_id
              AND sessions.project_id = runs.project_id
              AND sessions.environment_id = runs.environment_id
              AND sessions.id = runs.session_id
              AND sessions.current_run_id = runs.id
              AND sessions.status = 'open'
       )
   )
   AND EXISTS (
       SELECT 1
         FROM worker_instances
        WHERE worker_instances.id = sqlc.arg(worker_instance_id)
          AND worker_instances.cell_id = run_queue_items.cell_id
          AND worker_instances.status = 'active'
   )
   AND EXISTS (
       SELECT 1
         FROM runs
         JOIN environment_cells
           ON environment_cells.org_id = runs.org_id
          AND environment_cells.project_id = runs.project_id
          AND environment_cells.environment_id = runs.environment_id
          AND environment_cells.cell_id = runs.cell_id
          AND environment_cells.route_generation = run_queue_items.route_generation
          AND environment_cells.route_state IN ('active', 'draining')
         JOIN org_cells ON org_cells.org_id = environment_cells.org_id
                       AND org_cells.cell_id = environment_cells.cell_id
                       AND org_cells.state = 'active'
         JOIN cells ON cells.id = environment_cells.cell_id
                   AND cells.state IN ('active', 'draining')
         JOIN cell_health ON cell_health.cell_id = environment_cells.cell_id
        WHERE runs.org_id = run_queue_items.org_id
          AND runs.id = run_queue_items.run_id
          AND runs.cell_id = run_queue_items.cell_id
          AND cell_health.state IN ('healthy', 'degraded')
          AND cell_health.routing_fresh_until > now()
   )
RETURNING *;

-- name: ReserveResidentRunQueueItemForWorker :one
WITH candidate AS MATERIALIZED (
    SELECT run_queue_items.*
      FROM run_queue_items
      JOIN runs
        ON runs.org_id = run_queue_items.org_id
       AND runs.id = run_queue_items.run_id
       AND runs.cell_id = run_queue_items.cell_id
      JOIN sessions
        ON sessions.org_id = runs.org_id
       AND sessions.project_id = runs.project_id
       AND sessions.environment_id = runs.environment_id
       AND sessions.id = runs.session_id
       AND sessions.current_run_id = runs.id
       AND sessions.status = 'open'
      JOIN workspace_mounts
        ON workspace_mounts.org_id = runs.org_id
       AND workspace_mounts.id = runs.workspace_mount_id
       AND workspace_mounts.workspace_id = runs.workspace_id
       AND workspace_mounts.state = 'mounted'
      JOIN runtime_instances
        ON runtime_instances.org_id = workspace_mounts.org_id
       AND runtime_instances.id = workspace_mounts.runtime_instance_id
       AND runtime_instances.worker_instance_id = sqlc.arg(worker_instance_id)
       AND runtime_instances.workspace_mount_id = workspace_mounts.id
       AND runtime_instances.state IN ('running', 'waiting_hot')
      JOIN worker_instances
        ON worker_instances.id = runtime_instances.worker_instance_id
       AND worker_instances.cell_id = run_queue_items.cell_id
       AND worker_instances.status = 'active'
      JOIN environment_cells
        ON environment_cells.org_id = runs.org_id
       AND environment_cells.project_id = runs.project_id
       AND environment_cells.environment_id = runs.environment_id
       AND environment_cells.cell_id = runs.cell_id
       AND environment_cells.route_generation = run_queue_items.route_generation
       AND environment_cells.route_state IN ('active', 'draining')
      JOIN org_cells ON org_cells.org_id = environment_cells.org_id
                    AND org_cells.cell_id = environment_cells.cell_id
                    AND org_cells.state = 'active'
      JOIN cells ON cells.id = environment_cells.cell_id
                AND cells.state IN ('active', 'draining')
      JOIN cell_health ON cell_health.cell_id = environment_cells.cell_id
     WHERE runs.status = 'queued'
       AND runs.current_run_lease_id IS NULL
       AND runs.queue_timestamp <= now()
       AND (runs.queued_expires_at IS NULL OR runs.queued_expires_at > now())
       AND cell_health.state IN ('healthy', 'degraded')
       AND cell_health.routing_fresh_until > now()
       AND (
           run_queue_items.status = 'queued'
           OR (
               run_queue_items.status = 'published'
               AND run_queue_items.enqueued_at <= now() - interval '1 second'
           )
           OR (
               run_queue_items.status = 'reserved'
               AND run_queue_items.reservation_expires_at <= now()
           )
       )
     ORDER BY runs.priority DESC,
              runs.queue_timestamp ASC,
              runs.id ASC
     LIMIT 1
     FOR UPDATE OF run_queue_items, runtime_instances SKIP LOCKED
),
reserved AS (
    UPDATE run_queue_items
       SET status = 'reserved',
           dispatch_message_id = 'resident:' || candidate.run_id::text || ':' || (candidate.dispatch_generation + 1)::text,
           reserved_by_worker_instance_id = sqlc.arg(worker_instance_id),
           reservation_expires_at = sqlc.arg(reservation_expires_at),
           queued_expires_at = NULL,
           dispatch_generation = candidate.dispatch_generation + 1,
           last_error = '',
           updated_at = now(),
           finished_at = NULL
      FROM candidate
     WHERE run_queue_items.org_id = candidate.org_id
       AND run_queue_items.cell_id = candidate.cell_id
       AND run_queue_items.run_id = candidate.run_id
       AND run_queue_items.route_generation = candidate.route_generation
    RETURNING run_queue_items.*
)
SELECT *
  FROM reserved;

-- name: ReserveCheckpointRestoreRunQueueItemForWorker :one
WITH worker_scope AS MATERIALIZED (
    SELECT worker_instances.*
      FROM worker_instances
     WHERE worker_instances.id = sqlc.arg(worker_instance_id)
       AND worker_instances.status = 'active'
     FOR UPDATE OF worker_instances
),
candidate AS MATERIALIZED (
    SELECT run_queue_items.*
      FROM worker_scope
      JOIN run_queue_items ON true
      JOIN runs
        ON runs.org_id = run_queue_items.org_id
       AND runs.id = run_queue_items.run_id
       AND runs.cell_id = run_queue_items.cell_id
       AND runs.cell_id = worker_scope.cell_id
      JOIN environment_cells
        ON environment_cells.org_id = runs.org_id
       AND environment_cells.project_id = runs.project_id
       AND environment_cells.environment_id = runs.environment_id
       AND environment_cells.cell_id = runs.cell_id
       AND environment_cells.route_generation = run_queue_items.route_generation
       AND environment_cells.route_state IN ('active', 'draining')
      JOIN org_cells ON org_cells.org_id = environment_cells.org_id
                    AND org_cells.cell_id = environment_cells.cell_id
                    AND org_cells.state = 'active'
      JOIN cells ON cells.id = environment_cells.cell_id
                AND cells.state IN ('active', 'draining')
      JOIN cell_health ON cell_health.cell_id = environment_cells.cell_id
      JOIN sessions
        ON sessions.org_id = runs.org_id
       AND sessions.project_id = runs.project_id
       AND sessions.environment_id = runs.environment_id
       AND sessions.id = runs.session_id
       AND sessions.current_run_id = runs.id
       AND sessions.status = 'open'
      JOIN deployments
        ON deployments.org_id = runs.org_id
       AND deployments.id = runs.deployment_id
       AND deployments.worker_protocol_version = worker_scope.protocol_version
      JOIN run_runtime_requirements
        ON run_runtime_requirements.org_id = runs.org_id
       AND run_runtime_requirements.cell_id = runs.cell_id
       AND run_runtime_requirements.run_id = runs.id
       AND run_runtime_requirements.worker_group_id = worker_scope.worker_group_id
       AND run_runtime_requirements.runtime_id = worker_scope.runtime_id
       AND run_runtime_requirements.runtime_arch = worker_scope.runtime_arch
       AND run_runtime_requirements.runtime_abi = worker_scope.runtime_abi
       AND run_runtime_requirements.kernel_digest = worker_scope.kernel_digest
       AND run_runtime_requirements.initramfs_digest = worker_scope.initramfs_digest
       AND run_runtime_requirements.rootfs_digest = worker_scope.rootfs_digest
       AND run_runtime_requirements.cni_profile = worker_scope.cni_profile
      JOIN LATERAL (
          SELECT COALESCE(run_runtime_requirements.placement->'tags', run_runtime_requirements.placement->'Tags') AS placement_tags,
                 COALESCE(NULLIF(run_runtime_requirements.placement->>'region', ''), NULLIF(run_runtime_requirements.placement->>'Region', ''), '') AS placement_region,
                 COALESCE(NULLIF(run_runtime_requirements.placement->>'dedicated_key', ''), NULLIF(run_runtime_requirements.placement->>'DedicatedKey', ''), '') AS dedicated_key,
                 COALESCE(NULLIF(run_runtime_requirements.placement->>'snapshot_key', ''), NULLIF(run_runtime_requirements.placement->>'SnapshotKey', ''), '') AS snapshot_key
      ) placement ON true
      JOIN run_waits
        ON run_waits.org_id = runs.org_id
       AND run_waits.project_id = runs.project_id
       AND run_waits.environment_id = runs.environment_id
       AND run_waits.run_id = runs.id
       AND run_waits.state = 'resuming'
       AND run_waits.resuming_at >= now() - interval '8 seconds'
      JOIN runtime_checkpoints
        ON runtime_checkpoints.org_id = runs.org_id
       AND runtime_checkpoints.project_id = runs.project_id
       AND runtime_checkpoints.environment_id = runs.environment_id
       AND runtime_checkpoints.run_id = runs.id
       AND runtime_checkpoints.id = runs.latest_runtime_checkpoint_id
       AND runtime_checkpoints.id = run_waits.runtime_checkpoint_id
       AND runtime_checkpoints.source_worker_instance_id = worker_scope.id
       AND runtime_checkpoints.state = 'ready'
       AND (runtime_checkpoints.expires_at IS NULL OR runtime_checkpoints.expires_at > now())
       AND runtime_checkpoints.runtime_id = worker_scope.runtime_id
       AND runtime_checkpoints.runtime_arch = worker_scope.runtime_arch
       AND runtime_checkpoints.runtime_abi = worker_scope.runtime_abi
       AND runtime_checkpoints.kernel_digest = worker_scope.kernel_digest
       AND runtime_checkpoints.initramfs_digest = worker_scope.initramfs_digest
       AND runtime_checkpoints.rootfs_digest = worker_scope.rootfs_digest
       AND (runtime_checkpoints.runtime_vcpus IS NULL OR runtime_checkpoints.runtime_vcpus = ((run_runtime_requirements.requested_milli_cpu + 999) / 1000))
       AND (runtime_checkpoints.runtime_memory_mib IS NULL OR runtime_checkpoints.runtime_memory_mib = run_runtime_requirements.requested_memory_mib)
       AND (
           runtime_checkpoints.runtime_scratch_disk_mib IS NULL
           OR runtime_checkpoints.runtime_scratch_disk_mib = CASE
               WHEN run_runtime_requirements.requested_disk_mib > 0 THEN run_runtime_requirements.requested_disk_mib
               ELSE worker_scope.total_disk_mib
           END
       )
       AND runtime_checkpoints.cni_profile = worker_scope.cni_profile
     WHERE runs.status = 'queued'
       AND run_queue_items.cell_id = worker_scope.cell_id
       AND runs.current_run_lease_id IS NULL
       AND runs.queue_timestamp <= now()
       AND (runs.queued_expires_at IS NULL OR runs.queued_expires_at > now())
       AND cell_health.state IN ('healthy', 'degraded')
       AND cell_health.routing_fresh_until > now()
       AND (
           placement.placement_tags IS NULL
           OR placement.placement_tags = 'null'::jsonb
           OR (
               jsonb_typeof(placement.placement_tags) = 'object'
               AND worker_scope.labels @> placement.placement_tags
           )
       )
       AND (placement.dedicated_key = '' OR worker_scope.labels->>'dedicated_key' = placement.dedicated_key)
       AND (placement.snapshot_key = '' OR worker_scope.labels->>'snapshot_key' = placement.snapshot_key)
       AND (
           run_queue_items.status = 'queued'
           OR (
               run_queue_items.status = 'published'
               AND run_queue_items.enqueued_at <= now() - interval '1 second'
           )
           OR (
               run_queue_items.status = 'reserved'
               AND run_queue_items.reservation_expires_at <= now()
           )
       )
     ORDER BY runs.priority DESC,
              runs.queue_timestamp ASC,
              runs.id ASC
     LIMIT 1
     FOR UPDATE OF run_queue_items SKIP LOCKED
),
reserved AS (
    UPDATE run_queue_items
       SET status = 'reserved',
           dispatch_message_id = 'restore-source:' || candidate.run_id::text || ':' || (candidate.dispatch_generation + 1)::text,
           reserved_by_worker_instance_id = sqlc.arg(worker_instance_id),
           reservation_expires_at = sqlc.arg(reservation_expires_at),
           queued_expires_at = NULL,
           dispatch_generation = candidate.dispatch_generation + 1,
           last_error = '',
           updated_at = now(),
           finished_at = NULL
      FROM candidate
     WHERE run_queue_items.org_id = candidate.org_id
       AND run_queue_items.cell_id = candidate.cell_id
       AND run_queue_items.run_id = candidate.run_id
       AND run_queue_items.route_generation = candidate.route_generation
    RETURNING run_queue_items.*
)
SELECT *
  FROM reserved;

-- name: IsRunQueueLeaseConflict :one
SELECT EXISTS (
    SELECT 1
      FROM run_queue_items
     WHERE org_id = sqlc.arg(org_id)
       AND cell_id = sqlc.arg(cell_id)
       AND route_generation = sqlc.arg(route_generation)
       AND queue_class = sqlc.arg(queue_class)
       AND run_id = sqlc.arg(run_id)
       AND dispatch_message_id = sqlc.arg(dispatch_message_id)
       AND status = 'reserved'
       AND reservation_expires_at > now()
) AS lease_conflict;

-- name: RunLeaseDispatchAttemptsExhausted :one
SELECT count(*) >= sqlc.arg(max_dispatch_attempts)::int AS exhausted
  FROM run_leases
 WHERE org_id = sqlc.arg(org_id)
   AND cell_id = sqlc.arg(cell_id)
   AND route_generation = sqlc.arg(route_generation)
   AND queue_class = sqlc.arg(queue_class)
   AND run_id = sqlc.arg(run_id)
   AND status = 'lost';

-- name: RenewRunQueueReservation :one
UPDATE run_queue_items
   SET reservation_expires_at = sqlc.arg(reservation_expires_at),
       updated_at = now()
 WHERE run_queue_items.org_id = sqlc.arg(org_id)
   AND run_queue_items.cell_id = sqlc.arg(cell_id)
   AND run_queue_items.route_generation = sqlc.arg(route_generation)
   AND run_queue_items.queue_class = sqlc.arg(queue_class)
   AND run_queue_items.run_id = sqlc.arg(run_id)
   AND run_queue_items.reserved_by_worker_instance_id = sqlc.arg(worker_instance_id)
   AND run_queue_items.dispatch_message_id = sqlc.arg(dispatch_message_id)
   AND run_queue_items.status = 'reserved'
   AND run_queue_items.reservation_expires_at > now()
   AND EXISTS (
       SELECT 1
         FROM worker_instances
        WHERE worker_instances.id = sqlc.arg(worker_instance_id)
          AND worker_instances.cell_id = run_queue_items.cell_id
          AND worker_instances.status IN ('active', 'draining')
   )
   AND EXISTS (
       SELECT 1
         FROM runs
        WHERE runs.org_id = run_queue_items.org_id
          AND runs.cell_id = run_queue_items.cell_id
          AND runs.id = run_queue_items.run_id
   )
RETURNING *;

-- name: CompleteRunQueueItem :one
UPDATE run_queue_items
   SET status = 'completed',
       dispatch_generation = dispatch_generation + 1,
       updated_at = now(),
       finished_at = now()
 WHERE run_queue_items.org_id = sqlc.arg(org_id)
   AND run_queue_items.cell_id = sqlc.arg(cell_id)
   AND run_queue_items.route_generation = sqlc.arg(route_generation)
   AND run_queue_items.queue_class = sqlc.arg(queue_class)
   AND run_queue_items.run_id = sqlc.arg(run_id)
   AND run_queue_items.reserved_by_worker_instance_id = sqlc.arg(worker_instance_id)
   AND run_queue_items.dispatch_message_id = sqlc.arg(dispatch_message_id)
   AND run_queue_items.status = 'reserved'
   AND run_queue_items.reservation_expires_at > now()
   AND EXISTS (
       SELECT 1
         FROM worker_instances
        WHERE worker_instances.id = sqlc.arg(worker_instance_id)
          AND worker_instances.cell_id = run_queue_items.cell_id
          AND worker_instances.status IN ('active', 'draining')
   )
   AND EXISTS (
       SELECT 1
         FROM runs
        WHERE runs.org_id = run_queue_items.org_id
          AND runs.cell_id = run_queue_items.cell_id
          AND runs.id = run_queue_items.run_id
   )
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
 WHERE run_queue_items.org_id = sqlc.arg(org_id)
   AND run_queue_items.cell_id = sqlc.arg(cell_id)
   AND run_queue_items.route_generation = sqlc.arg(route_generation)
   AND run_queue_items.queue_class = sqlc.arg(queue_class)
   AND run_queue_items.run_id = sqlc.arg(run_id)
   AND run_queue_items.reserved_by_worker_instance_id = sqlc.arg(worker_instance_id)
   AND run_queue_items.dispatch_message_id = sqlc.arg(dispatch_message_id)
   AND run_queue_items.status = 'reserved'
   AND run_queue_items.reservation_expires_at > now()
   AND EXISTS (
       SELECT 1
         FROM worker_instances
        WHERE worker_instances.id = sqlc.arg(worker_instance_id)
          AND worker_instances.cell_id = run_queue_items.cell_id
          AND worker_instances.status IN ('active', 'draining')
   )
   AND EXISTS (
       SELECT 1
         FROM runs
        WHERE runs.org_id = run_queue_items.org_id
          AND runs.cell_id = run_queue_items.cell_id
          AND runs.id = run_queue_items.run_id
   )
RETURNING *;

-- name: DeadLetterRunQueueItem :one
WITH locked_session AS MATERIALIZED (
    SELECT sessions.id
      FROM runs
      JOIN sessions
        ON sessions.org_id = runs.org_id
       AND sessions.project_id = runs.project_id
       AND sessions.environment_id = runs.environment_id
       AND sessions.id = runs.session_id
     WHERE runs.org_id = sqlc.arg(org_id)
       AND runs.id = sqlc.arg(run_id)
     FOR UPDATE OF sessions
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
       AND run_queue_items.cell_id = sqlc.arg(cell_id)
       AND run_queue_items.route_generation = sqlc.arg(route_generation)
       AND run_queue_items.queue_class = sqlc.arg(queue_class)
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
           OR EXISTS (SELECT 1 FROM locked_session)
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
    RETURNING runs.org_id, runs.cell_id, runs.project_id, runs.environment_id, runs.id, runs.session_id, runs.current_attempt_id, runs.current_attempt_number, runs.trace_id, runs.root_span_id, runs.state_version
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
    INSERT INTO run_snapshots (org_id, cell_id, run_id, version, status, execution_status, terminal_outcome, attempt_id, transition, reason)
    SELECT failed_run.org_id,
           failed_run.cell_id,
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
    UPDATE session_runs
       SET ended_at = now()
      FROM failed_run
     WHERE session_runs.org_id = failed_run.org_id
       AND session_runs.project_id = failed_run.project_id
       AND session_runs.environment_id = failed_run.environment_id
       AND session_runs.session_id = failed_run.session_id
       AND session_runs.run_id = failed_run.id
    RETURNING session_runs.id
),
failed_sessions AS (
    SELECT failed_run.session_id AS id
      FROM failed_run
),
run_event_seq AS (
    INSERT INTO event_cursors (org_id, cell_id, subject_kind, subject_id, seq)
    SELECT failed_run.org_id, failed_run.cell_id, 'run', failed_run.id, 1
      FROM failed_run
      JOIN failed_snapshot ON failed_snapshot.run_id = failed_run.id
    ON CONFLICT (org_id, cell_id, subject_kind, subject_id)
    DO UPDATE SET seq = event_cursors.seq + 1,
                  observed_at = now()
    RETURNING org_id, subject_kind, subject_id, seq
),
	run_event AS (
	    INSERT INTO event_hot_payloads (org_id, cell_id, project_id, environment_id, run_id, seq, attempt_id, attempt_number, trace_id, span_id, traceparent, category, severity, source, kind, message, payload, redaction_class, snapshot_version)
	    SELECT failed_run.org_id,
	           failed_run.cell_id,
	           failed_run.project_id,
	           failed_run.environment_id,
	           failed_run.id,
	           run_event_seq.seq,
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
	                        AND run_event_seq.subject_kind = 'run'
	                        AND run_event_seq.subject_id = failed_run.id
	    RETURNING *
	),
	run_telemetry_outbox AS (
	    INSERT INTO telemetry_outbox (org_id, cell_id, stream_kind, source_kind, source_id, seq, idempotency_key)
	    SELECT run_event.org_id,
	                  run_event.cell_id,
	                  'event',
	                  run_event.subject_type,
	                  run_event.subject_id,
	                  run_event.seq,
	                  'event:' || run_event.subject_type::text || ':' || run_event.subject_id::text || ':' || run_event.seq::text
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
  JOIN run_telemetry_outbox ON true
 WHERE (SELECT count(*) FROM failed_session_runs) >= 0
   AND (SELECT count(*) FROM failed_sessions) >= 0
UNION ALL
SELECT existing_dead_letter.*
  FROM existing_dead_letter
 WHERE NOT EXISTS (SELECT 1 FROM queue_entry);
