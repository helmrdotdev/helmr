-- name: CreateWorkerPool :one
INSERT INTO worker_pools (
    id,
    slug,
    name,
    queue_name,
    region,
    capabilities,
    metadata
) VALUES (
    sqlc.arg(id),
    sqlc.arg(slug),
    sqlc.arg(name),
    sqlc.arg(queue_name),
    sqlc.arg(region),
    sqlc.arg(capabilities),
    sqlc.arg(metadata)
)
RETURNING *;

-- name: EnsureDefaultWorkerPool :one
WITH lock AS (
    SELECT pg_advisory_xact_lock(hashtext('worker_pools:default')) AS locked
),
existing AS (
    SELECT worker_pools.*
      FROM worker_pools
      JOIN lock ON true
     WHERE slug = 'default'
       AND archived_at IS NULL
     LIMIT 1
),
inserted AS (
    INSERT INTO worker_pools (
        id,
        slug,
        name,
        queue_name,
        region,
        capabilities,
        metadata
    )
    SELECT sqlc.arg(id),
           'default',
           'Default',
           'default',
           '',
           '{}'::jsonb,
           '{}'::jsonb
      FROM lock
     WHERE NOT EXISTS (SELECT 1 FROM existing)
    RETURNING *
)
SELECT * FROM inserted
UNION ALL
SELECT * FROM existing
LIMIT 1;

-- name: UpsertOrgWorkerPool :one
WITH locked_pool AS (
    SELECT id
      FROM worker_pools
     WHERE worker_pools.id = sqlc.arg(worker_pool_id)
       AND worker_pools.archived_at IS NULL
     FOR UPDATE
)
INSERT INTO org_worker_pools (org_id, worker_pool_id, is_default, concurrency_limit, archived_at)
SELECT sqlc.arg(org_id),
       locked_pool.id,
       sqlc.arg(is_default),
       sqlc.narg(concurrency_limit)::int,
       NULL
  FROM locked_pool
ON CONFLICT (org_id, worker_pool_id) DO UPDATE
   SET is_default = excluded.is_default,
       concurrency_limit = excluded.concurrency_limit,
       archived_at = NULL
RETURNING *;

-- name: SetDefaultOrgWorkerPool :one
WITH lock AS (
    SELECT pg_advisory_xact_lock(hashtext('org_worker_pools:default:' || sqlc.arg(org_id)::uuid::text)) AS locked
),
target AS (
    SELECT org_id,
           worker_pool_id
     FROM org_worker_pools
     JOIN lock ON true
     WHERE org_worker_pools.org_id = sqlc.arg(org_id)::uuid
       AND org_worker_pools.worker_pool_id = sqlc.arg(worker_pool_id)
       AND org_worker_pools.archived_at IS NULL
     FOR UPDATE
),
cleared AS (
    UPDATE org_worker_pools
       SET is_default = false
      FROM target
     WHERE org_worker_pools.org_id = target.org_id
       AND org_worker_pools.worker_pool_id <> target.worker_pool_id
       AND org_worker_pools.is_default
       AND org_worker_pools.archived_at IS NULL
    RETURNING 1
)
UPDATE org_worker_pools
   SET is_default = true
  FROM target
 WHERE org_worker_pools.org_id = target.org_id
   AND org_worker_pools.worker_pool_id = target.worker_pool_id
   AND (SELECT count(*) FROM cleared) >= 0
RETURNING *;

-- name: GetWorkerPool :one
SELECT worker_pools.*
  FROM worker_pools
 JOIN org_worker_pools ON org_worker_pools.worker_pool_id = worker_pools.id
 WHERE org_worker_pools.org_id = sqlc.arg(org_id)
   AND worker_pools.id = sqlc.arg(id)
   AND org_worker_pools.archived_at IS NULL
   AND worker_pools.archived_at IS NULL;

-- name: GetWorkerPoolByID :one
SELECT *
  FROM worker_pools
 WHERE id = sqlc.arg(id)
   AND archived_at IS NULL;

-- name: ListWorkerPools :many
SELECT worker_pools.*
  FROM worker_pools
 JOIN org_worker_pools ON org_worker_pools.worker_pool_id = worker_pools.id
 WHERE org_worker_pools.org_id = sqlc.arg(org_id)
   AND org_worker_pools.archived_at IS NULL
   AND worker_pools.archived_at IS NULL
 ORDER BY org_worker_pools.is_default DESC, worker_pools.created_at ASC
 LIMIT sqlc.arg(row_limit);

-- name: ListWorkerPoolQueueScopes :many
SELECT org_worker_pools.org_id,
       worker_pools.id AS worker_pool_id,
       worker_pools.queue_name
  FROM worker_pools
 JOIN org_worker_pools ON org_worker_pools.worker_pool_id = worker_pools.id
 WHERE worker_pools.id = sqlc.arg(worker_pool_id)
   AND org_worker_pools.archived_at IS NULL
   AND worker_pools.archived_at IS NULL
 ORDER BY md5(org_worker_pools.org_id::text || ':' || sqlc.arg(scan_seed)::text), org_worker_pools.org_id ASC
 LIMIT sqlc.arg(row_limit)
OFFSET sqlc.arg(row_offset);

-- name: ArchiveWorkerPoolForOrg :one
WITH target_grant AS (
    SELECT worker_pools.id
      FROM worker_pools
      JOIN org_worker_pools ON org_worker_pools.worker_pool_id = worker_pools.id
                           AND org_worker_pools.org_id = sqlc.arg(org_id)
     WHERE worker_pools.id = sqlc.arg(id)
       AND worker_pools.archived_at IS NULL
       AND org_worker_pools.archived_at IS NULL
     FOR UPDATE OF worker_pools, org_worker_pools
),
archived_grant AS (
    UPDATE org_worker_pools
       SET is_default = false,
           archived_at = COALESCE(archived_at, now())
      FROM target_grant
     WHERE org_worker_pools.org_id = sqlc.arg(org_id)
       AND org_worker_pools.worker_pool_id = target_grant.id
    RETURNING org_worker_pools.worker_pool_id
),
remaining_grants AS (
    SELECT count(*) AS active_count
      FROM org_worker_pools
      JOIN archived_grant ON archived_grant.worker_pool_id = org_worker_pools.worker_pool_id
     WHERE org_worker_pools.archived_at IS NULL
       AND org_worker_pools.org_id <> sqlc.arg(org_id)
)
UPDATE worker_pools
   SET archived_at = CASE
           WHEN (SELECT active_count FROM remaining_grants) = 0 THEN COALESCE(archived_at, now())
           ELSE archived_at
       END,
       updated_at = now()
  FROM archived_grant
 WHERE worker_pools.id = archived_grant.worker_pool_id
RETURNING worker_pools.*;

-- name: UpsertWorkerHostHeartbeat :one
INSERT INTO worker_hosts (
    id,
    worker_pool_id,
    external_id,
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
    sqlc.arg(worker_pool_id),
    sqlc.arg(external_id),
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
ON CONFLICT (worker_pool_id, external_id) DO UPDATE
   SET status = CASE
           WHEN worker_hosts.status IN ('draining', 'unschedulable') THEN worker_hosts.status
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

-- name: SetWorkerHostStatus :one
UPDATE worker_hosts
   SET status = sqlc.arg(status)::worker_host_status,
       drained_at = CASE
           WHEN sqlc.arg(status)::worker_host_status = 'draining' THEN COALESCE(drained_at, now())
           ELSE drained_at
       END
 WHERE worker_hosts.worker_pool_id = sqlc.arg(worker_pool_id)
   AND worker_hosts.id = sqlc.arg(id)
RETURNING *;

-- name: ListWorkerHostsByWorkerPool :many
SELECT worker_hosts.*
  FROM worker_hosts
  JOIN org_worker_pools ON org_worker_pools.worker_pool_id = worker_hosts.worker_pool_id
 WHERE org_worker_pools.org_id = sqlc.arg(org_id)
   AND org_worker_pools.archived_at IS NULL
   AND worker_hosts.worker_pool_id = sqlc.arg(worker_pool_id)
   AND (
       sqlc.arg(status_filter)::text = 'all'
       OR worker_hosts.status::text = sqlc.arg(status_filter)::text
   )
 ORDER BY worker_hosts.last_seen_at DESC, worker_hosts.first_seen_at ASC
 LIMIT sqlc.arg(row_limit);

-- name: GetWorkerHostState :one
SELECT worker_hosts.*,
       (
           SELECT count(*)::int
             FROM run_executions
            WHERE run_executions.worker_pool_id = worker_hosts.worker_pool_id
              AND run_executions.worker_host_id = worker_hosts.id
              AND run_executions.status IN ('leased', 'running')
       ) AS active_executions
  FROM worker_hosts
 WHERE worker_hosts.worker_pool_id = sqlc.arg(worker_pool_id)
   AND worker_hosts.id = sqlc.arg(id);

-- name: GetWorkerHostQueueCapacity :one
SELECT GREATEST(worker_hosts.available_milli_cpu - active.used_milli_cpu, 0)::bigint AS available_milli_cpu,
       GREATEST(worker_hosts.available_memory_mib - active.used_memory_mib, 0)::bigint AS available_memory_mib,
       GREATEST(worker_hosts.available_disk_mib - active.used_disk_mib, 0)::bigint AS available_disk_mib,
       GREATEST(worker_hosts.available_execution_slots - active.used_slots, 0)::int AS available_execution_slots
  FROM worker_hosts
  LEFT JOIN LATERAL (
      SELECT COALESCE(sum(run_requirements.requested_milli_cpu), 0)::bigint AS used_milli_cpu,
             COALESCE(sum(run_requirements.requested_memory_mib), 0)::bigint AS used_memory_mib,
             COALESCE(sum(run_requirements.requested_disk_mib), 0)::bigint AS used_disk_mib,
             COALESCE(sum(run_requirements.requested_execution_slots), 0)::int AS used_slots
        FROM run_executions
        JOIN run_requirements ON run_requirements.org_id = run_executions.org_id
                                      AND run_requirements.run_id = run_executions.run_id
                                      AND run_requirements.worker_pool_id = run_executions.worker_pool_id
       WHERE run_executions.worker_pool_id = worker_hosts.worker_pool_id
         AND run_executions.worker_host_id = worker_hosts.id
         AND run_executions.status IN ('leased', 'running')
  ) active ON true
 WHERE worker_hosts.worker_pool_id = sqlc.arg(worker_pool_id)
   AND worker_hosts.id = sqlc.arg(id)
   AND worker_hosts.status = 'active';

-- name: UpsertRunRequirements :one
WITH active_worker_pool AS (
    SELECT org_worker_pools.worker_pool_id
      FROM org_worker_pools
      JOIN worker_pools ON worker_pools.id = org_worker_pools.worker_pool_id
     WHERE org_worker_pools.org_id = sqlc.arg(org_id)
       AND org_worker_pools.worker_pool_id = sqlc.arg(worker_pool_id)
       AND org_worker_pools.archived_at IS NULL
       AND worker_pools.archived_at IS NULL
)
INSERT INTO run_requirements (
    run_id,
    org_id,
    worker_pool_id,
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
       active_worker_pool.worker_pool_id,
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
  FROM active_worker_pool
ON CONFLICT (run_id) DO UPDATE
   SET worker_pool_id = excluded.worker_pool_id,
       requested_milli_cpu = excluded.requested_milli_cpu,
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

-- name: UpsertRunQueueEntryQueued :one
INSERT INTO run_queue_entries (
    run_id,
    org_id,
    worker_pool_id,
    status,
    priority,
    queue_name,
    queue_message_id,
    reservation_expires_at,
    last_error,
    enqueued_at,
    updated_at,
    finished_at
) VALUES (
    sqlc.arg(run_id),
    sqlc.arg(org_id),
    sqlc.arg(worker_pool_id),
    'queued',
    sqlc.arg(priority),
    sqlc.arg(queue_name),
    sqlc.arg(queue_message_id),
    NULL,
    '',
    now(),
    now(),
    NULL
)
ON CONFLICT (run_id) DO UPDATE
   SET worker_pool_id = excluded.worker_pool_id,
       status = 'queued',
       priority = excluded.priority,
       queue_name = excluded.queue_name,
       queue_message_id = excluded.queue_message_id,
       reserved_by_worker_host_id = NULL,
       reservation_expires_at = NULL,
       dispatch_generation = run_queue_entries.dispatch_generation + 1,
       last_error = '',
       enqueued_at = now(),
       updated_at = now(),
       finished_at = NULL
RETURNING *;

-- name: PrepareQueuedRunQueueEntry :one
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
    SELECT run_requirements.*
      FROM run_requirements
      JOIN target_run ON target_run.org_id = run_requirements.org_id
                     AND target_run.id = run_requirements.run_id
      JOIN org_worker_pools ON org_worker_pools.org_id = run_requirements.org_id
                           AND org_worker_pools.worker_pool_id = run_requirements.worker_pool_id
      JOIN worker_pools ON worker_pools.id = run_requirements.worker_pool_id
                       AND worker_pools.archived_at IS NULL
     WHERE org_worker_pools.archived_at IS NULL
     LIMIT 1
),
default_worker_pool AS (
    SELECT worker_pools.id
      FROM target_run
      JOIN org_worker_pools ON org_worker_pools.org_id = target_run.org_id
      JOIN worker_pools ON worker_pools.id = org_worker_pools.worker_pool_id
      LEFT JOIN LATERAL (
          SELECT count(*)::int AS active_hosts
            FROM worker_hosts
           WHERE worker_hosts.worker_pool_id = worker_pools.id
             AND worker_hosts.status = 'active'
      ) hosts ON true
     WHERE worker_pools.archived_at IS NULL
       AND org_worker_pools.archived_at IS NULL
       AND NOT EXISTS (SELECT 1 FROM existing_requirements)
     ORDER BY org_worker_pools.is_default DESC,
              CASE WHEN COALESCE(hosts.active_hosts, 0) > 0 THEN 0 ELSE 1 END,
              worker_pools.created_at ASC,
              worker_pools.id ASC
     LIMIT 1
),
selected_worker_pool AS (
    SELECT worker_pool_id AS id FROM existing_requirements
    UNION ALL
    SELECT id FROM default_worker_pool
    LIMIT 1
),
inserted_requirements AS (
    INSERT INTO run_requirements (
        run_id,
        org_id,
        worker_pool_id,
        requested_milli_cpu,
        requested_memory_mib
    )
    SELECT target_run.id,
           target_run.org_id,
           selected_worker_pool.id,
           deployment_tasks.requested_milli_cpu,
           deployment_tasks.requested_memory_mib
      FROM target_run
      JOIN selected_worker_pool ON true
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
target_worker_pool AS (
    SELECT worker_pools.*
      FROM worker_pools
      JOIN requirements ON requirements.worker_pool_id = worker_pools.id
      JOIN org_worker_pools ON org_worker_pools.org_id = requirements.org_id
                           AND org_worker_pools.worker_pool_id = requirements.worker_pool_id
     WHERE worker_pools.archived_at IS NULL
       AND org_worker_pools.archived_at IS NULL
),
dispatch AS (
    INSERT INTO run_queue_entries (
        run_id,
        org_id,
        worker_pool_id,
        status,
        priority,
        queue_name,
        queue_message_id,
        reservation_expires_at,
        last_error,
        enqueued_at,
        updated_at,
        finished_at
    )
    SELECT target_run.id,
           target_run.org_id,
           requirements.worker_pool_id,
           'queued',
           sqlc.arg(priority),
           target_worker_pool.queue_name,
           NULL,
           NULL,
           '',
           now(),
           now(),
           NULL
      FROM target_run
      JOIN requirements ON requirements.org_id = target_run.org_id
                       AND requirements.run_id = target_run.id
      JOIN target_worker_pool ON true
    ON CONFLICT (run_id) DO UPDATE
       SET worker_pool_id = excluded.worker_pool_id,
           status = 'queued',
           priority = excluded.priority,
           queue_name = excluded.queue_name,
           queue_message_id = NULL,
           reserved_by_worker_host_id = NULL,
           reservation_expires_at = NULL,
           dispatch_generation = run_queue_entries.dispatch_generation + 1,
           last_error = '',
           enqueued_at = now(),
           updated_at = now(),
           finished_at = NULL
     WHERE run_queue_entries.status = 'queued'
        OR (
            run_queue_entries.status = 'published'
            AND run_queue_entries.enqueued_at <= now() - interval '1 minute'
        )
        OR (
            run_queue_entries.status = 'reserved'
            AND run_queue_entries.reservation_expires_at <= now()
        )
    RETURNING *
)
SELECT
    target_run.id AS run_id,
    target_run.org_id,
    target_run.project_id,
    target_run.environment_id,
    dispatch.worker_pool_id,
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

-- name: ListQueuedRunQueueEntryCandidates :many
SELECT runs.org_id,
       runs.id AS run_id,
       COALESCE(run_queue_entries.queue_message_id, '') AS queue_message_id
  FROM runs
  LEFT JOIN run_queue_entries ON run_queue_entries.org_id = runs.org_id
                           AND run_queue_entries.run_id = runs.id
 WHERE runs.org_id = sqlc.arg(org_id)
   AND runs.status = 'queued'
   AND runs.current_execution_id IS NULL
   AND (
       run_queue_entries.run_id IS NULL
       OR (
           run_queue_entries.status = 'queued'
           AND (
               run_queue_entries.queue_message_id IS NULL
               OR run_queue_entries.last_error <> ''
               OR run_queue_entries.enqueued_at <= now() - interval '1 minute'
           )
       )
       OR (
           run_queue_entries.status = 'published'
           AND run_queue_entries.enqueued_at <= now() - interval '1 minute'
       )
       OR (
           run_queue_entries.status = 'reserved'
           AND run_queue_entries.reservation_expires_at <= now()
       )
   )
 ORDER BY runs.created_at ASC, runs.id ASC
 LIMIT sqlc.arg(row_limit);

-- name: MarkRunQueueEntryEnqueueError :one
UPDATE run_queue_entries
   SET last_error = sqlc.arg(last_error),
       updated_at = now()
 WHERE org_id = sqlc.arg(org_id)
   AND run_id = sqlc.arg(run_id)
   AND status = 'queued'
   AND dispatch_generation = sqlc.arg(expected_dispatch_generation)
RETURNING *;

-- name: MarkRunQueueEntryEnqueued :one
UPDATE run_queue_entries
   SET status = 'published',
       queue_message_id = sqlc.arg(queue_message_id),
       last_error = '',
       enqueued_at = now(),
       updated_at = now()
 WHERE org_id = sqlc.arg(org_id)
   AND run_id = sqlc.arg(run_id)
   AND status = 'queued'
   AND dispatch_generation = sqlc.arg(expected_dispatch_generation)
RETURNING *;

-- name: ReserveRunQueueEntry :one
UPDATE run_queue_entries
   SET status = 'reserved',
       queue_message_id = sqlc.arg(queue_message_id),
       reserved_by_worker_host_id = sqlc.arg(worker_host_id),
       reservation_expires_at = sqlc.arg(reservation_expires_at),
       dispatch_generation = dispatch_generation + 1,
       updated_at = now(),
       finished_at = NULL
 WHERE org_id = sqlc.arg(org_id)
   AND run_id = sqlc.arg(run_id)
   AND worker_pool_id = sqlc.arg(worker_pool_id)
   AND (
       status = 'published'
       OR (
           status = 'reserved'
           AND reservation_expires_at <= now()
       )
   )
   AND queue_message_id = sqlc.arg(queue_message_id)
RETURNING *;

-- name: RunExecutionDeliveryAttemptsExhausted :one
SELECT count(*) >= sqlc.arg(max_delivery_attempts)::int AS exhausted
  FROM run_executions
 WHERE org_id = sqlc.arg(org_id)
   AND run_id = sqlc.arg(run_id)
   AND worker_pool_id = sqlc.arg(worker_pool_id)
   AND status = 'lost';

-- name: RenewRunQueueReservation :one
UPDATE run_queue_entries
   SET reservation_expires_at = sqlc.arg(reservation_expires_at),
       dispatch_generation = dispatch_generation + 1,
       updated_at = now()
 WHERE org_id = sqlc.arg(org_id)
   AND run_id = sqlc.arg(run_id)
   AND worker_pool_id = sqlc.arg(worker_pool_id)
   AND reserved_by_worker_host_id = sqlc.arg(worker_host_id)
   AND queue_message_id = sqlc.arg(queue_message_id)
   AND status = 'reserved'
   AND reservation_expires_at > now()
RETURNING *;

-- name: CompleteRunQueueEntry :one
UPDATE run_queue_entries
   SET status = 'completed',
       dispatch_generation = dispatch_generation + 1,
       updated_at = now(),
       finished_at = now()
 WHERE org_id = sqlc.arg(org_id)
   AND run_id = sqlc.arg(run_id)
   AND worker_pool_id = sqlc.arg(worker_pool_id)
   AND reserved_by_worker_host_id = sqlc.arg(worker_host_id)
   AND queue_message_id = sqlc.arg(queue_message_id)
   AND status = 'reserved'
   AND reservation_expires_at > now()
RETURNING *;

-- name: RequeueRunQueueEntry :one
UPDATE run_queue_entries
   SET status = 'queued',
       queue_message_id = NULL,
       reserved_by_worker_host_id = NULL,
       reservation_expires_at = NULL,
       dispatch_generation = dispatch_generation + 1,
       last_error = sqlc.arg(last_error),
       enqueued_at = now(),
       updated_at = now(),
       finished_at = NULL
 WHERE org_id = sqlc.arg(org_id)
   AND run_id = sqlc.arg(run_id)
   AND worker_pool_id = sqlc.arg(worker_pool_id)
   AND reserved_by_worker_host_id = sqlc.arg(worker_host_id)
   AND queue_message_id = sqlc.arg(queue_message_id)
   AND status = 'reserved'
   AND reservation_expires_at > now()
RETURNING *;

-- name: DeadLetterRunQueueEntry :one
WITH queue_entry AS (
    UPDATE run_queue_entries
       SET status = 'dead_lettered',
           reserved_by_worker_host_id = NULL,
           reservation_expires_at = NULL,
           dispatch_generation = dispatch_generation + 1,
           last_error = sqlc.arg(last_error),
           updated_at = now(),
           finished_at = now()
     WHERE run_queue_entries.org_id = sqlc.arg(org_id)
       AND run_queue_entries.run_id = sqlc.arg(run_id)
       AND run_queue_entries.worker_pool_id = sqlc.arg(worker_pool_id)
       AND run_queue_entries.queue_message_id = sqlc.arg(queue_message_id)
       AND run_queue_entries.status IN ('queued', 'published', 'reserved')
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
