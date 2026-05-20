-- name: CreateWorkerGroup :one
INSERT INTO worker_groups (
    id,
    org_id,
    project_id,
    environment_id,
    slug,
    name,
    provisioning_mode,
    queue_name,
    region,
    capabilities,
    metadata
) VALUES (
    sqlc.arg(id),
    sqlc.arg(org_id),
    sqlc.arg(project_id),
    sqlc.arg(environment_id),
    sqlc.arg(slug),
    sqlc.arg(name),
    sqlc.arg(provisioning_mode)::worker_group_provisioning_mode,
    sqlc.arg(queue_name),
    sqlc.arg(region),
    sqlc.arg(capabilities),
    sqlc.arg(metadata)
)
RETURNING *;

-- name: GetWorkerGroup :one
SELECT *
  FROM worker_groups
 WHERE org_id = sqlc.arg(org_id)
   AND id = sqlc.arg(id)
   AND archived_at IS NULL;

-- name: ListWorkerGroupsByScope :many
SELECT *
  FROM worker_groups
 WHERE org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND archived_at IS NULL
 ORDER BY created_at ASC
 LIMIT sqlc.arg(row_limit);

-- name: ArchiveWorkerGroup :one
UPDATE worker_groups
   SET archived_at = COALESCE(archived_at, now()),
       updated_at = now()
 WHERE org_id = sqlc.arg(org_id)
   AND id = sqlc.arg(id)
   AND archived_at IS NULL
RETURNING *;

-- name: UpsertWorkerHostHeartbeat :one
INSERT INTO worker_hosts (
    id,
    org_id,
    worker_group_id,
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
    sqlc.arg(org_id),
    sqlc.arg(worker_group_id),
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
ON CONFLICT (org_id, worker_group_id, external_id) DO UPDATE
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
 WHERE org_id = sqlc.arg(org_id)
   AND worker_group_id = sqlc.arg(worker_group_id)
   AND id = sqlc.arg(id)
RETURNING *;

-- name: ListWorkerHostsByWorkerGroup :many
SELECT *
  FROM worker_hosts
 WHERE org_id = sqlc.arg(org_id)
   AND worker_group_id = sqlc.arg(worker_group_id)
   AND (
       sqlc.arg(status_filter)::text = 'all'
       OR status::text = sqlc.arg(status_filter)::text
   )
 ORDER BY last_seen_at DESC, first_seen_at ASC
 LIMIT sqlc.arg(row_limit);

-- name: GetWorkerHostState :one
SELECT worker_hosts.*,
       (
           SELECT count(*)::int
             FROM run_executions
            WHERE run_executions.org_id = worker_hosts.org_id
              AND run_executions.worker_group_id = worker_hosts.worker_group_id
              AND run_executions.worker_host_id = worker_hosts.id
              AND run_executions.status IN ('leased', 'running')
       ) AS active_executions
  FROM worker_hosts
 WHERE worker_hosts.org_id = sqlc.arg(org_id)
   AND worker_hosts.worker_group_id = sqlc.arg(worker_group_id)
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
                                      AND run_requirements.worker_group_id = run_executions.worker_group_id
       WHERE run_executions.org_id = worker_hosts.org_id
         AND run_executions.worker_group_id = worker_hosts.worker_group_id
         AND run_executions.worker_host_id = worker_hosts.id
         AND run_executions.status IN ('leased', 'running')
  ) active ON true
 WHERE worker_hosts.org_id = sqlc.arg(org_id)
   AND worker_hosts.worker_group_id = sqlc.arg(worker_group_id)
   AND worker_hosts.id = sqlc.arg(id)
   AND worker_hosts.status = 'active';

-- name: UpsertRunRequirements :one
INSERT INTO run_requirements (
    run_id,
    org_id,
    worker_group_id,
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
) VALUES (
    sqlc.arg(run_id),
    sqlc.arg(org_id),
    sqlc.arg(worker_group_id),
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
)
ON CONFLICT (run_id) DO UPDATE
   SET worker_group_id = excluded.worker_group_id,
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
    worker_group_id,
    status,
    priority,
    queue_name,
    queue_message_id,
    lease_expires_at,
    last_error,
    enqueued_at,
    updated_at,
    finished_at
) VALUES (
    sqlc.arg(run_id),
    sqlc.arg(org_id),
    sqlc.arg(worker_group_id),
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
   SET worker_group_id = excluded.worker_group_id,
       status = 'queued',
       priority = excluded.priority,
       queue_name = excluded.queue_name,
       queue_message_id = excluded.queue_message_id,
       leased_by_worker_host_id = NULL,
       lease_expires_at = NULL,
       queue_version = run_queue_entries.queue_version + 1,
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
           task_deployment_id,
           deployed_task_id,
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
      JOIN worker_groups ON worker_groups.org_id = run_requirements.org_id
                        AND worker_groups.id = run_requirements.worker_group_id
                        AND worker_groups.project_id = target_run.project_id
                        AND worker_groups.environment_id = target_run.environment_id
                        AND worker_groups.archived_at IS NULL
     LIMIT 1
),
default_worker_group AS (
    SELECT worker_groups.id
      FROM worker_groups
      JOIN target_run ON target_run.org_id = worker_groups.org_id
                     AND target_run.project_id = worker_groups.project_id
                     AND target_run.environment_id = worker_groups.environment_id
     WHERE worker_groups.archived_at IS NULL
       AND NOT EXISTS (SELECT 1 FROM existing_requirements)
     ORDER BY CASE WHEN worker_groups.provisioning_mode = 'helmr_managed' THEN 0 ELSE 1 END,
              worker_groups.created_at ASC,
              worker_groups.id ASC
     LIMIT 1
),
selected_worker_group AS (
    SELECT worker_group_id AS id FROM existing_requirements
    UNION ALL
    SELECT id FROM default_worker_group
    LIMIT 1
),
inserted_requirements AS (
    INSERT INTO run_requirements (
        run_id,
        org_id,
        worker_group_id,
        requested_milli_cpu,
        requested_memory_mib
    )
    SELECT target_run.id,
           target_run.org_id,
           selected_worker_group.id,
           deployed_tasks.requested_milli_cpu,
           deployed_tasks.requested_memory_mib
      FROM target_run
      JOIN selected_worker_group ON true
      JOIN deployed_tasks ON deployed_tasks.org_id = target_run.org_id
                         AND deployed_tasks.deployment_id = target_run.task_deployment_id
                         AND deployed_tasks.id = target_run.deployed_task_id
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
target_worker_group AS (
    SELECT worker_groups.*
      FROM worker_groups
      JOIN requirements ON requirements.org_id = worker_groups.org_id
                       AND requirements.worker_group_id = worker_groups.id
     WHERE worker_groups.archived_at IS NULL
),
dispatch AS (
    INSERT INTO run_queue_entries (
        run_id,
        org_id,
        worker_group_id,
        status,
        priority,
        queue_name,
        queue_message_id,
        lease_expires_at,
        last_error,
        enqueued_at,
        updated_at,
        finished_at
    )
    SELECT target_run.id,
           target_run.org_id,
           requirements.worker_group_id,
           'queued',
           sqlc.arg(priority),
           target_worker_group.queue_name,
           '',
           NULL,
           '',
           now(),
           now(),
           NULL
      FROM target_run
      JOIN requirements ON requirements.org_id = target_run.org_id
                       AND requirements.run_id = target_run.id
      JOIN target_worker_group ON true
    ON CONFLICT (run_id) DO UPDATE
       SET worker_group_id = excluded.worker_group_id,
           status = 'queued',
           priority = excluded.priority,
           queue_name = excluded.queue_name,
           queue_message_id = '',
           leased_by_worker_host_id = NULL,
           lease_expires_at = NULL,
           queue_version = run_queue_entries.queue_version + 1,
           last_error = '',
           enqueued_at = now(),
           updated_at = now(),
           finished_at = NULL
     WHERE run_queue_entries.status IN ('queued', 'requeued')
        OR (
            run_queue_entries.status = 'leased'
            AND run_queue_entries.lease_expires_at <= now()
        )
    RETURNING *
)
SELECT
    target_run.id AS run_id,
    target_run.org_id,
    target_run.project_id,
    target_run.environment_id,
    dispatch.worker_group_id,
    dispatch.queue_name,
    dispatch.priority,
    dispatch.queue_version,
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
       OR run_queue_entries.status = 'requeued'
       OR (
           run_queue_entries.status = 'queued'
           AND (
               run_queue_entries.queue_message_id = ''
               OR run_queue_entries.last_error <> ''
               OR run_queue_entries.enqueued_at <= now() - interval '1 minute'
           )
       )
       OR (
           run_queue_entries.status = 'leased'
           AND run_queue_entries.lease_expires_at <= now()
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
   AND queue_version = sqlc.arg(expected_queue_version)
RETURNING *;

-- name: MarkRunQueueEntryEnqueued :one
UPDATE run_queue_entries
   SET queue_message_id = sqlc.arg(queue_message_id),
       last_error = '',
       enqueued_at = now(),
       updated_at = now()
 WHERE org_id = sqlc.arg(org_id)
   AND run_id = sqlc.arg(run_id)
   AND status = 'queued'
   AND queue_version = sqlc.arg(expected_queue_version)
RETURNING *;

-- name: MarkRunQueueEntryLeased :one
UPDATE run_queue_entries
   SET status = 'leased',
       queue_message_id = sqlc.arg(queue_message_id),
       leased_by_worker_host_id = sqlc.arg(worker_host_id),
       lease_expires_at = sqlc.arg(lease_expires_at),
       queue_version = queue_version + 1,
       updated_at = now(),
       finished_at = NULL
 WHERE org_id = sqlc.arg(org_id)
   AND run_id = sqlc.arg(run_id)
   AND worker_group_id = sqlc.arg(worker_group_id)
   AND (
       status = 'queued'
       OR (
           status = 'leased'
           AND lease_expires_at <= now()
       )
   )
   AND queue_message_id = sqlc.arg(queue_message_id)
RETURNING *;

-- name: RunExecutionDeliveryAttemptsExhausted :one
SELECT count(*) >= sqlc.arg(max_delivery_attempts)::int AS exhausted
  FROM run_executions
 WHERE org_id = sqlc.arg(org_id)
   AND run_id = sqlc.arg(run_id)
   AND worker_group_id = sqlc.arg(worker_group_id)
   AND status = 'lost';

-- name: RenewRunQueueLease :one
UPDATE run_queue_entries
   SET lease_expires_at = sqlc.arg(lease_expires_at),
       queue_version = queue_version + 1,
       updated_at = now()
 WHERE org_id = sqlc.arg(org_id)
   AND run_id = sqlc.arg(run_id)
   AND worker_group_id = sqlc.arg(worker_group_id)
   AND leased_by_worker_host_id = sqlc.arg(worker_host_id)
   AND queue_message_id = sqlc.arg(queue_message_id)
   AND status = 'leased'
   AND lease_expires_at > now()
RETURNING *;

-- name: CompleteRunQueueEntry :one
UPDATE run_queue_entries
   SET status = 'completed',
       queue_version = queue_version + 1,
       updated_at = now(),
       finished_at = now()
 WHERE org_id = sqlc.arg(org_id)
   AND run_id = sqlc.arg(run_id)
   AND worker_group_id = sqlc.arg(worker_group_id)
   AND leased_by_worker_host_id = sqlc.arg(worker_host_id)
   AND queue_message_id = sqlc.arg(queue_message_id)
   AND status = 'leased'
   AND lease_expires_at > now()
RETURNING *;

-- name: RequeueRunQueueEntry :one
UPDATE run_queue_entries
   SET status = 'requeued',
       leased_by_worker_host_id = NULL,
       lease_expires_at = NULL,
       queue_version = queue_version + 1,
       last_error = sqlc.arg(last_error),
       updated_at = now(),
       finished_at = now()
 WHERE org_id = sqlc.arg(org_id)
   AND run_id = sqlc.arg(run_id)
   AND worker_group_id = sqlc.arg(worker_group_id)
   AND leased_by_worker_host_id = sqlc.arg(worker_host_id)
   AND queue_message_id = sqlc.arg(queue_message_id)
   AND status = 'leased'
   AND lease_expires_at > now()
RETURNING *;

-- name: DeadLetterRunQueueEntry :one
WITH queue_entry AS (
    UPDATE run_queue_entries
       SET status = 'dead_lettered',
           leased_by_worker_host_id = NULL,
           lease_expires_at = NULL,
           queue_version = queue_version + 1,
           last_error = sqlc.arg(last_error),
           updated_at = now(),
           finished_at = now()
     WHERE run_queue_entries.org_id = sqlc.arg(org_id)
       AND run_queue_entries.run_id = sqlc.arg(run_id)
       AND run_queue_entries.worker_group_id = sqlc.arg(worker_group_id)
       AND run_queue_entries.queue_message_id = sqlc.arg(queue_message_id)
       AND run_queue_entries.status IN ('queued', 'leased', 'requeued')
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
