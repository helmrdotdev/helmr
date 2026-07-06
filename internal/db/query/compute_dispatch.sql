-- name: ListQueueScopes :many
SELECT runs.org_id,
       runs.worker_group_id,
       runs.project_id,
       runs.environment_id,
       runs.queue_class,
       runs.queue_name
  FROM runs
  JOIN worker_groups
    ON worker_groups.id = runs.worker_group_id
   AND worker_groups.state = 'active'
   AND worker_groups.health_state IN ('healthy', 'degraded')
   AND worker_groups.routing_fresh_until > now()
  JOIN regions
    ON regions.id = worker_groups.region_id
   AND regions.state = 'available'
 WHERE runs.status = 'queued'
   AND runs.current_run_lease_id IS NULL
   AND runs.worker_group_id = sqlc.arg(worker_group_id)
   AND runs.queue_timestamp <= now()
   AND (runs.queued_expires_at IS NULL OR runs.queued_expires_at > now())
 GROUP BY runs.org_id,
          runs.worker_group_id,
          runs.project_id,
          runs.environment_id,
          runs.queue_class,
          runs.queue_name
 ORDER BY md5(runs.org_id::text || ':' || runs.worker_group_id || ':' || runs.project_id::text || ':' || runs.environment_id::text || ':' || runs.queue_class || ':' || runs.queue_name || ':' || sqlc.arg(scan_seed)::text),
          runs.org_id ASC,
          runs.worker_group_id ASC,
          runs.project_id ASC,
          runs.environment_id ASC,
          runs.queue_class ASC,
          runs.queue_name ASC
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
     WHERE worker_instances.worker_group_id = excluded.worker_group_id
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
   AND worker_instances.worker_group_id = sqlc.arg(worker_group_id)
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
   AND worker_instances.worker_group_id = sqlc.arg(worker_group_id);

-- name: GetWorkerInstanceQueueCapacity :one
SELECT GREATEST(worker_instances.available_milli_cpu - active.used_milli_cpu - active_runtime_instances.used_milli_cpu, 0)::bigint AS available_milli_cpu,
       GREATEST(worker_instances.available_memory_mib - active.used_memory_mib - active_runtime_instances.used_memory_mib, 0)::bigint AS available_memory_mib,
       GREATEST(worker_instances.available_disk_mib - active.used_disk_mib - active_runtime_instances.used_disk_mib, 0)::bigint AS available_disk_mib,
       GREATEST(worker_instances.available_execution_slots - active.used_slots - active_runtime_instances.used_slots, 0)::int AS available_execution_slots
  FROM worker_instances
  LEFT JOIN LATERAL (
      SELECT COALESCE(sum(runs.requested_milli_cpu), 0)::bigint AS used_milli_cpu,
             COALESCE(sum(runs.requested_memory_mib), 0)::bigint AS used_memory_mib,
             COALESCE(sum(runs.requested_disk_mib), 0)::bigint AS used_disk_mib,
             COALESCE(sum(runs.requested_execution_slots), 0)::int AS used_slots
        FROM run_leases
        JOIN runs ON runs.org_id = run_leases.org_id
                 AND runs.id = run_leases.run_id
                 AND runs.workspace_mount_id IS NULL
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
   AND worker_instances.worker_group_id = sqlc.arg(worker_group_id)
   AND worker_instances.status = 'active';

-- name: GetWorkerInstanceRunDispatchCapacity :one
SELECT * FROM (
    SELECT GREATEST(worker_instances.available_milli_cpu - active.used_milli_cpu - active_runtime_instances.used_milli_cpu, 0)::bigint AS available_milli_cpu,
           GREATEST(worker_instances.available_memory_mib - active.used_memory_mib - active_runtime_instances.used_memory_mib, 0)::bigint AS available_memory_mib,
           GREATEST(worker_instances.available_disk_mib - active.used_disk_mib - active_runtime_instances.used_disk_mib, 0)::bigint AS available_disk_mib,
           GREATEST(worker_instances.available_execution_slots - active.used_slots - active_runtime_instances.used_slots, 0)::int AS available_execution_slots
      FROM worker_instances
      LEFT JOIN LATERAL (
          SELECT COALESCE(sum(runs.requested_milli_cpu), 0)::bigint AS used_milli_cpu,
                 COALESCE(sum(runs.requested_memory_mib), 0)::bigint AS used_memory_mib,
                 COALESCE(sum(runs.requested_disk_mib), 0)::bigint AS used_disk_mib,
                 COALESCE(sum(runs.requested_execution_slots), 0)::int AS used_slots
            FROM run_leases
            JOIN runs ON runs.org_id = run_leases.org_id
                     AND runs.id = run_leases.run_id
                     AND runs.workspace_mount_id IS NULL
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
       AND worker_instances.worker_group_id = sqlc.arg(worker_group_id)
       AND worker_instances.status = 'active'
) capacity;

-- name: PrepareQueuedRunDispatch :one
SELECT runs.id AS run_id,
       runs.org_id,
       runs.worker_group_id,
       runs.queue_class,
       runs.project_id,
       runs.environment_id,
       runs.queue_name,
       runs.queue_concurrency_limit,
       runs.priority,
       runs.concurrency_key,
       runs.queue_timestamp,
       runs.queued_expires_at,
       runs.dispatch_generation,
       COALESCE(runs.last_enqueued_at, runs.created_at) AS enqueued_at,
       runs.requested_milli_cpu,
       runs.requested_memory_mib,
       runs.requested_disk_mib,
       runs.requested_execution_slots,
       runs.runtime_id,
       runs.runtime_arch,
       runs.runtime_abi,
       runs.kernel_digest,
       runs.initramfs_digest,
       runs.rootfs_digest,
       runs.cni_profile,
       runs.network_policy,
       runs.placement
  FROM runs
  JOIN worker_groups
    ON worker_groups.id = runs.worker_group_id
   AND worker_groups.state = 'active'
   AND worker_groups.health_state IN ('healthy', 'degraded')
   AND worker_groups.routing_fresh_until > now()
  JOIN regions
    ON regions.id = worker_groups.region_id
   AND regions.state = 'available'
 WHERE runs.org_id = sqlc.arg(org_id)
   AND runs.id = sqlc.arg(run_id)
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
   );

-- name: ListQueuedRunCandidateScopes :many
WITH candidate_scopes AS (
    SELECT runs.org_id,
           runs.worker_group_id,
           runs.project_id,
           runs.environment_id,
           runs.queue_class,
           runs.queue_name,
           md5(runs.org_id::text || ':' || runs.worker_group_id || ':' || runs.project_id::text || ':' || runs.environment_id::text || ':' || runs.queue_class || ':' || runs.queue_name || ':' || sqlc.arg(scan_seed)::text) AS sort_key
      FROM runs
      JOIN worker_groups
        ON worker_groups.id = runs.worker_group_id
       AND worker_groups.state = 'active'
       AND worker_groups.health_state IN ('healthy', 'degraded')
       AND worker_groups.routing_fresh_until > now()
      JOIN regions
        ON regions.id = worker_groups.region_id
       AND regions.state = 'available'
     WHERE runs.status = 'queued'
       AND runs.worker_group_id = sqlc.arg(worker_group_id)
       AND runs.current_run_lease_id IS NULL
       AND runs.queue_timestamp <= now()
       AND (runs.queued_expires_at IS NULL OR runs.queued_expires_at > now())
       AND (
           runs.last_enqueued_at IS NULL
           OR runs.last_enqueue_error <> ''
           OR runs.last_enqueued_at <= now() - interval '1 minute'
       )
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
     GROUP BY runs.org_id,
              runs.worker_group_id,
              runs.project_id,
              runs.environment_id,
              runs.queue_class,
              runs.queue_name
)
SELECT candidate_scopes.org_id,
       candidate_scopes.worker_group_id,
       candidate_scopes.project_id,
       candidate_scopes.environment_id,
       candidate_scopes.queue_class,
       candidate_scopes.queue_name,
       candidate_scopes.sort_key
  FROM candidate_scopes
 WHERE sqlc.arg(after_sort_key)::text = ''
    OR (candidate_scopes.sort_key, candidate_scopes.org_id, candidate_scopes.worker_group_id, candidate_scopes.project_id, candidate_scopes.environment_id, candidate_scopes.queue_class, candidate_scopes.queue_name) > (sqlc.arg(after_sort_key)::text, sqlc.arg(after_org_id)::uuid, sqlc.arg(after_worker_group_id)::text, sqlc.arg(after_project_id)::uuid, sqlc.arg(after_environment_id)::uuid, sqlc.arg(after_queue_class)::text, sqlc.arg(after_queue_name)::text)
 ORDER BY candidate_scopes.sort_key ASC,
          candidate_scopes.org_id ASC,
          candidate_scopes.worker_group_id ASC,
          candidate_scopes.project_id ASC,
          candidate_scopes.environment_id ASC,
          candidate_scopes.queue_class ASC,
          candidate_scopes.queue_name ASC
 LIMIT sqlc.arg(row_limit);

-- name: ListQueuedRunDispatchCandidatesForScope :many
SELECT runs.org_id,
       runs.worker_group_id,
       runs.id AS run_id,
       (runs.id::text || ':' || runs.dispatch_generation::text) AS dispatch_message_id
  FROM runs
  JOIN worker_groups
    ON worker_groups.id = runs.worker_group_id
   AND worker_groups.state = 'active'
   AND worker_groups.health_state IN ('healthy', 'degraded')
   AND worker_groups.routing_fresh_until > now()
  JOIN regions
    ON regions.id = worker_groups.region_id
   AND regions.state = 'available'
 WHERE runs.org_id = sqlc.arg(org_id)
   AND runs.worker_group_id = sqlc.arg(worker_group_id)
   AND runs.project_id = sqlc.arg(project_id)
   AND runs.environment_id = sqlc.arg(environment_id)
   AND runs.queue_class = sqlc.arg(queue_class)
   AND runs.queue_name = sqlc.arg(queue_name)
   AND runs.status = 'queued'
   AND runs.current_run_lease_id IS NULL
   AND runs.queue_timestamp <= now()
   AND (runs.queued_expires_at IS NULL OR runs.queued_expires_at > now())
   AND (
       runs.last_enqueued_at IS NULL
       OR runs.last_enqueue_error <> ''
       OR runs.last_enqueued_at <= now() - interval '1 minute'
   )
 ORDER BY runs.priority DESC, runs.queue_timestamp ASC, runs.id ASC
 LIMIT sqlc.arg(row_limit);

-- name: MarkRunDispatchEnqueueError :one
UPDATE runs
   SET last_enqueue_error = sqlc.arg(last_error),
       updated_at = now()
 WHERE org_id = sqlc.arg(org_id)
   AND worker_group_id = sqlc.arg(worker_group_id)
   AND queue_class = sqlc.arg(queue_class)
   AND id = sqlc.arg(run_id)
   AND status = 'queued'
   AND dispatch_generation = sqlc.arg(expected_dispatch_generation)
RETURNING *;

-- name: MarkRunDispatchEnqueued :one
UPDATE runs
   SET last_enqueue_error = '',
       last_enqueued_at = now(),
       updated_at = now()
 WHERE org_id = sqlc.arg(org_id)
   AND worker_group_id = sqlc.arg(worker_group_id)
   AND queue_class = sqlc.arg(queue_class)
   AND id = sqlc.arg(run_id)
   AND status = 'queued'
   AND dispatch_generation = sqlc.arg(expected_dispatch_generation)
RETURNING *;

-- name: RunLeaseDispatchAttemptsExhausted :one
SELECT runs.dispatch_attempt_count >= sqlc.arg(max_dispatch_attempts)::int AS exhausted
  FROM runs
WHERE runs.org_id = sqlc.arg(org_id)
   AND runs.worker_group_id = sqlc.arg(worker_group_id)
   AND runs.queue_class = sqlc.arg(queue_class)
   AND runs.id = sqlc.arg(run_id)
   AND runs.dispatch_generation = sqlc.arg(dispatch_generation)
   AND runs.status = 'queued';

-- name: ValidateRunLeaseDispatchRenewal :one
SELECT runs.*
  FROM runs
  JOIN run_leases
    ON run_leases.org_id = runs.org_id
   AND run_leases.run_id = runs.id
   AND run_leases.id = runs.current_run_lease_id
   AND run_leases.worker_group_id = runs.worker_group_id
   AND run_leases.worker_instance_id = sqlc.arg(worker_instance_id)
   AND run_leases.dispatch_message_id = sqlc.arg(dispatch_message_id)
   AND run_leases.dispatch_generation = runs.dispatch_generation
   AND run_leases.status IN ('leased', 'running')
   AND run_leases.lease_expires_at > now()
 WHERE runs.org_id = sqlc.arg(org_id)
   AND runs.worker_group_id = sqlc.arg(worker_group_id)
   AND runs.queue_class = sqlc.arg(queue_class)
   AND runs.id = sqlc.arg(run_id)
   AND runs.status = 'running';

-- name: CompleteRunDispatch :one
SELECT runs.*
  FROM runs
 WHERE runs.org_id = sqlc.arg(org_id)
   AND runs.worker_group_id = sqlc.arg(worker_group_id)
   AND runs.queue_class = sqlc.arg(queue_class)
   AND runs.id = sqlc.arg(run_id);

-- name: RequeueRunDispatch :one
UPDATE runs
   SET dispatch_generation = dispatch_generation + 1,
       dispatch_attempt_count = dispatch_attempt_count + 1,
       last_enqueue_error = sqlc.arg(last_error),
       last_enqueued_at = NULL,
       updated_at = now()
 WHERE runs.org_id = sqlc.arg(org_id)
   AND runs.worker_group_id = sqlc.arg(worker_group_id)
   AND runs.queue_class = sqlc.arg(queue_class)
   AND runs.id = sqlc.arg(run_id)
   AND runs.status = 'queued'
   AND runs.dispatch_generation = sqlc.arg(expected_dispatch_generation)
   AND runs.current_run_lease_id IS NULL
RETURNING *;

-- name: ReserveResidentRunForWorker :one
SELECT runs.org_id,
       runs.worker_group_id,
       runs.id AS run_id,
       runs.queue_class,
       runs.queue_name,
       runs.priority,
       runs.queue_timestamp,
       runs.queued_expires_at,
       runs.dispatch_generation,
       (runs.id::text || ':' || runs.dispatch_generation::text) AS dispatch_message_id
  FROM runs
  JOIN worker_instances
    ON worker_instances.id = sqlc.arg(worker_instance_id)
   AND worker_instances.worker_group_id = runs.worker_group_id
   AND worker_instances.status = 'active'
 WHERE runs.status = 'queued'
   AND runs.current_run_lease_id IS NULL
   AND runs.latest_runtime_checkpoint_id IS NULL
   AND EXISTS (
       SELECT 1
         FROM runtime_instances
        WHERE runtime_instances.org_id = runs.org_id
          AND runtime_instances.project_id = runs.project_id
          AND runtime_instances.environment_id = runs.environment_id
          AND runtime_instances.workspace_id = runs.workspace_id
          AND runtime_instances.worker_instance_id = worker_instances.id
          AND runtime_instances.state IN ('ready', 'waiting_hot')
   )
 ORDER BY runs.priority DESC, runs.queue_timestamp ASC, runs.id ASC
 LIMIT 1;

-- name: ReserveCheckpointRestoreRunForWorker :one
SELECT runs.org_id,
       runs.worker_group_id,
       runs.id AS run_id,
       runs.queue_class,
       runs.queue_name,
       runs.priority,
       runs.queue_timestamp,
       runs.queued_expires_at,
       runs.dispatch_generation,
       (runs.id::text || ':' || runs.dispatch_generation::text) AS dispatch_message_id
  FROM runs
  JOIN worker_instances
    ON worker_instances.id = sqlc.arg(worker_instance_id)
   AND worker_instances.worker_group_id = runs.worker_group_id
   AND worker_instances.status = 'active'
  JOIN runtime_checkpoints
    ON runtime_checkpoints.org_id = runs.org_id
   AND runtime_checkpoints.run_id = runs.id
   AND runtime_checkpoints.id = runs.latest_runtime_checkpoint_id
   AND runtime_checkpoints.source_worker_instance_id = worker_instances.id
   AND runtime_checkpoints.state = 'ready'
   AND (runtime_checkpoints.expires_at IS NULL OR runtime_checkpoints.expires_at > now())
 WHERE runs.status = 'queued'
   AND runs.current_run_lease_id IS NULL
 ORDER BY runs.priority DESC, runs.queue_timestamp ASC, runs.id ASC
 LIMIT 1;

-- name: DeadLetterRunDispatch :one
WITH terminalized AS (
    UPDATE runs
       SET status = 'failed',
           execution_status = 'finished',
           terminal_outcome = 'dead_lettered',
           current_run_lease_id = NULL,
           dispatch_generation = dispatch_generation + 1,
           last_enqueue_error = sqlc.arg(last_error),
           state_version = state_version + 1,
           finished_at = COALESCE(finished_at, now()),
           updated_at = now()
     WHERE runs.org_id = sqlc.arg(org_id)
       AND runs.worker_group_id = sqlc.arg(worker_group_id)
       AND runs.queue_class = sqlc.arg(queue_class)
       AND runs.id = sqlc.arg(run_id)
       AND runs.dispatch_generation = sqlc.arg(dispatch_generation)
       AND runs.status = 'queued'
    RETURNING *
),
ended_session_run AS (
    UPDATE session_runs
       SET ended_at = COALESCE(session_runs.ended_at, terminalized.finished_at)
      FROM terminalized
     WHERE session_runs.org_id = terminalized.org_id
       AND session_runs.project_id = terminalized.project_id
       AND session_runs.environment_id = terminalized.environment_id
       AND session_runs.session_id = terminalized.session_id
       AND session_runs.run_id = terminalized.id
    RETURNING session_runs.id
),
cleanup AS (
    SELECT count(*) AS ended_session_run_count FROM ended_session_run
)
SELECT terminalized.id AS run_id,
       terminalized.org_id,
       terminalized.worker_group_id,
       terminalized.project_id,
       terminalized.environment_id,
       terminalized.state_version
  FROM terminalized
 WHERE (SELECT ended_session_run_count FROM cleanup) >= 0;
