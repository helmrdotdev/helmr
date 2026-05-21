-- name: ListQueueScopes :many
SELECT run_queue_items.org_id,
       run_queue_items.queue_name
  FROM run_queue_items
 WHERE run_queue_items.status IN ('queued', 'published', 'reserved')
 GROUP BY run_queue_items.org_id, run_queue_items.queue_name
 ORDER BY md5(run_queue_items.org_id::text || ':' || run_queue_items.queue_name || ':' || sqlc.arg(scan_seed)::text),
          run_queue_items.org_id ASC,
          run_queue_items.queue_name ASC
 LIMIT sqlc.arg(row_limit)
OFFSET sqlc.arg(row_offset);

-- name: UpsertWorkerInstanceHeartbeat :one
INSERT INTO worker_instances (
    id,
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
    last_seen_at
) VALUES (
    sqlc.arg(id),
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
    now()
)
ON CONFLICT (resource_id) DO UPDATE
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
       last_seen_at = now()
RETURNING *;

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
             FROM run_executions
            WHERE run_executions.worker_instance_id = worker_instances.id
              AND run_executions.status IN ('leased', 'running')
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
        FROM run_executions
        JOIN run_runtime_requirements ON run_runtime_requirements.org_id = run_executions.org_id
                             AND run_runtime_requirements.run_id = run_executions.run_id
       WHERE run_executions.worker_instance_id = worker_instances.id
         AND run_executions.status IN ('leased', 'running')
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
    runtime_arch,
    runtime_abi,
    kernel_digest,
    rootfs_digest,
    cni_profile,
    network_policy,
    placement
)
SELECT sqlc.arg(run_id),
       sqlc.arg(org_id),
       sqlc.arg(requested_milli_cpu),
       sqlc.arg(requested_memory_mib),
       sqlc.arg(requested_disk_mib),
       sqlc.arg(requested_execution_slots),
       sqlc.arg(runtime_arch),
       sqlc.arg(runtime_abi),
       sqlc.arg(kernel_digest),
       sqlc.arg(rootfs_digest),
       sqlc.arg(cni_profile),
       sqlc.arg(network_policy),
       sqlc.arg(placement)
ON CONFLICT (run_id) DO UPDATE
   SET requested_milli_cpu = excluded.requested_milli_cpu,
       requested_memory_mib = excluded.requested_memory_mib,
       requested_disk_mib = excluded.requested_disk_mib,
       requested_execution_slots = excluded.requested_execution_slots,
       runtime_arch = excluded.runtime_arch,
       runtime_abi = excluded.runtime_abi,
       kernel_digest = excluded.kernel_digest,
       rootfs_digest = excluded.rootfs_digest,
       cni_profile = excluded.cni_profile,
       network_policy = excluded.network_policy,
       placement = excluded.placement,
       updated_at = now()
RETURNING *;

-- name: UpsertRunQueueItemQueued :one
INSERT INTO run_queue_items (
    run_id,
    org_id,
    status,
    priority,
    queue_name,
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
           created_at
      FROM runs
     WHERE runs.org_id = sqlc.arg(org_id)
       AND runs.id = sqlc.arg(run_id)
       AND runs.status = 'queued'
       AND runs.current_execution_id IS NULL
),
existing_requirements AS (
    SELECT run_runtime_requirements.*
      FROM run_runtime_requirements
      JOIN target_run ON target_run.org_id = run_runtime_requirements.org_id
                     AND target_run.id = run_runtime_requirements.run_id
     LIMIT 1
),
inserted_requirements AS (
    INSERT INTO run_runtime_requirements (
        run_id,
        org_id,
        requested_milli_cpu,
        requested_memory_mib
    )
    SELECT target_run.id,
           target_run.org_id,
           deployment_tasks.requested_milli_cpu,
           deployment_tasks.requested_memory_mib
      FROM target_run
      JOIN deployment_tasks ON deployment_tasks.org_id = target_run.org_id
                           AND deployment_tasks.deployment_id = target_run.deployment_id
                           AND deployment_tasks.id = target_run.deployment_task_id
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
           sqlc.arg(priority),
           'default',
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
    dispatch.priority,
    dispatch.dispatch_generation,
    dispatch.enqueued_at,
    requirements.requested_milli_cpu,
    requirements.requested_memory_mib,
    requirements.requested_disk_mib,
    requirements.requested_execution_slots,
    requirements.runtime_arch,
    requirements.runtime_abi,
    requirements.kernel_digest,
    requirements.rootfs_digest,
    requirements.cni_profile,
    requirements.network_policy,
    requirements.placement
  FROM target_run
  JOIN requirements ON requirements.org_id = target_run.org_id
                   AND requirements.run_id = target_run.id
  JOIN dispatch ON dispatch.org_id = target_run.org_id
               AND dispatch.run_id = target_run.id;

-- name: ListQueuedRunQueueItemCandidates :many
SELECT runs.org_id,
       runs.id AS run_id,
       COALESCE(run_queue_items.dispatch_message_id, '') AS dispatch_message_id
  FROM runs
  LEFT JOIN run_queue_items ON run_queue_items.org_id = runs.org_id
                           AND run_queue_items.run_id = runs.id
 WHERE runs.org_id = sqlc.arg(org_id)
   AND runs.status = 'queued'
   AND runs.current_execution_id IS NULL
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
 ORDER BY runs.created_at ASC, runs.id ASC
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
       dispatch_generation = dispatch_generation + 1,
       updated_at = now(),
       finished_at = NULL
 WHERE org_id = sqlc.arg(org_id)
   AND run_id = sqlc.arg(run_id)
   AND (
       status = 'published'
       OR (
           status = 'reserved'
           AND reservation_expires_at <= now()
       )
   )
   AND dispatch_message_id = sqlc.arg(dispatch_message_id)
RETURNING *;

-- name: RunExecutionDispatchAttemptsExhausted :one
SELECT count(*) >= sqlc.arg(max_dispatch_attempts)::int AS exhausted
  FROM run_executions
 WHERE org_id = sqlc.arg(org_id)
   AND run_id = sqlc.arg(run_id)
   AND status = 'lost';

-- name: RenewRunQueueReservation :one
UPDATE run_queue_items
   SET reservation_expires_at = sqlc.arg(reservation_expires_at),
       dispatch_generation = dispatch_generation + 1,
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
WITH queue_entry AS (
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
    RETURNING *
),
failed_run AS (
    UPDATE runs
       SET status = 'failed',
           current_execution_id = NULL,
           error_message = sqlc.arg(last_error),
           updated_at = now(),
           finished_at = now()
      FROM queue_entry
     WHERE runs.org_id = queue_entry.org_id
       AND runs.id = queue_entry.run_id
       AND runs.status = 'queued'
       AND runs.current_execution_id IS NULL
    RETURNING runs.org_id, runs.id
),
run_event AS (
    INSERT INTO run_events (org_id, run_id, kind, payload)
    SELECT failed_run.org_id, failed_run.id, sqlc.arg(event_kind), sqlc.arg(event_payload)
      FROM failed_run
    RETURNING id
)
SELECT queue_entry.*
  FROM queue_entry;
