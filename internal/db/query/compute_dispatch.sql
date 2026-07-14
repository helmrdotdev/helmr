-- name: ListQueueScopes :many
SELECT runs.org_id,
       runs.project_id,
       runs.environment_id,
       workspaces.region_id,
       runs.queue_class,
       runs.queue_name
  FROM runs
  JOIN workspaces
    ON workspaces.org_id = runs.org_id
   AND workspaces.project_id = runs.project_id
   AND workspaces.environment_id = runs.environment_id
   AND workspaces.id = runs.workspace_id
  JOIN regions ON regions.id = workspaces.region_id AND regions.state = 'available'
 WHERE runs.status = 'queued'
   AND runs.current_run_lease_id IS NULL
   AND runs.queue_timestamp <= now()
   AND (runs.queued_expires_at IS NULL OR runs.queued_expires_at > now())
 GROUP BY runs.org_id, runs.project_id, runs.environment_id, workspaces.region_id,
          runs.queue_class, runs.queue_name
 ORDER BY md5(runs.org_id::text || ':' || runs.project_id::text || ':' ||
              runs.environment_id::text || ':' || workspaces.region_id || ':' ||
              runs.queue_class || ':' || runs.queue_name || ':' || sqlc.arg(scan_seed)::text),
          runs.org_id, runs.project_id, runs.environment_id, workspaces.region_id,
          runs.queue_class, runs.queue_name
 LIMIT sqlc.arg(row_limit)
OFFSET sqlc.arg(row_offset);

-- name: SetWorkerInstanceState :one
UPDATE worker_instances
   SET state = sqlc.arg(state)::worker_instance_state,
       draining_at = CASE WHEN sqlc.arg(state)::worker_instance_state = 'draining'
                          THEN COALESCE(draining_at, now()) ELSE draining_at END,
       disabled_at = CASE WHEN sqlc.arg(state)::worker_instance_state = 'disabled'
                          THEN COALESCE(disabled_at, now()) ELSE disabled_at END,
       lost_at = CASE WHEN sqlc.arg(state)::worker_instance_state = 'lost'
                      THEN COALESCE(lost_at, now()) ELSE lost_at END,
       updated_at = now()
 WHERE id = sqlc.arg(id)
   AND worker_group_id = sqlc.arg(worker_group_id)
   AND (sqlc.narg(expected_epoch)::bigint IS NULL OR current_epoch = sqlc.narg(expected_epoch)::bigint)
RETURNING *;

-- name: DrainWorkerInstance :one
WITH target AS (
    UPDATE worker_instances
       SET state = 'draining', draining_at = COALESCE(draining_at, now()), updated_at = now()
     WHERE worker_instances.id = sqlc.arg(id)
       AND worker_instances.worker_group_id = sqlc.arg(worker_group_id)
       AND worker_instances.current_epoch = sqlc.arg(expected_epoch)
       AND worker_instances.state IN ('active', 'draining')
    RETURNING *
), idle_mounts AS (
    UPDATE workspace_mounts
       SET state = 'unmounting', stopped_at = COALESCE(stopped_at, now()), updated_at = now()
      FROM target
     WHERE workspace_mounts.worker_instance_id = target.id
       AND workspace_mounts.worker_epoch = target.current_epoch
       AND workspace_mounts.state IN ('mounting', 'mounted')
       AND NOT EXISTS (
           SELECT 1 FROM workspace_leases
            WHERE workspace_leases.workspace_mount_id = workspace_mounts.id
              AND workspace_leases.state IN ('active', 'releasing')
       )
    RETURNING workspace_mounts.id
), idle_runtimes AS (
    UPDATE runtime_instances
       SET desired_state = 'closed', desired_version = desired_version + 1,
           desired_at = now(), desired_reason = 'worker_draining', updated_at = now()
      FROM target
     WHERE runtime_instances.worker_instance_id = target.id
       AND runtime_instances.worker_epoch = target.current_epoch
       AND runtime_instances.reclaimed_at IS NULL
       AND runtime_instances.desired_state <> 'closed'
       AND runtime_instances.observed_state IN ('allocated', 'preparing', 'ready')
       AND NOT EXISTS (
           SELECT 1 FROM run_leases
            WHERE run_leases.runtime_instance_id = runtime_instances.id
              AND run_leases.state IN ('assigned', 'starting', 'running', 'checkpointing')
       )
       AND NOT EXISTS (
           SELECT 1 FROM workspace_mounts
            WHERE workspace_mounts.runtime_instance_id = runtime_instances.id
              AND workspace_mounts.state IN ('mounting', 'mounted', 'unmounting')
       )
    RETURNING runtime_instances.id
)
SELECT target.*
  FROM target
 WHERE (SELECT count(*) FROM idle_mounts) >= 0
   AND (SELECT count(*) FROM idle_runtimes) >= 0;

-- name: FenceWorkerInstance :one
WITH target AS (
    UPDATE worker_instances
       SET state = 'lost', claim_version = claim_version + 1,
           lost_at = COALESCE(lost_at, now()), updated_at = now()
     WHERE worker_instances.id = sqlc.arg(id)
       AND worker_instances.worker_group_id = sqlc.arg(worker_group_id)
       AND worker_instances.current_epoch = sqlc.arg(expected_epoch)
       AND worker_instances.state IN ('active', 'draining')
    RETURNING *
), revoked_credentials AS (
    UPDATE worker_instance_credentials
       SET revoked_at = COALESCE(revoked_at, now())
      FROM target
     WHERE worker_instance_credentials.worker_instance_id = target.id
       AND worker_instance_credentials.revoked_at IS NULL
    RETURNING worker_instance_credentials.id
), lost_mounts AS (
    UPDATE workspace_mounts
       SET state = 'lost', lost_at = now(), terminal_at = now(),
           terminal_reason_code = sqlc.arg(reason_code), updated_at = now()
      FROM target
     WHERE workspace_mounts.worker_instance_id = target.id
       AND workspace_mounts.worker_epoch = target.current_epoch
       AND workspace_mounts.state IN ('mounting', 'mounted', 'unmounting')
    RETURNING workspace_mounts.id
), lost_runtimes AS (
    UPDATE runtime_instances
       SET observed_state = 'lost', observed_version = observed_version + 1,
           observed_at = now(), lost_at = now(), terminal_at = now(),
           terminal_reason_code = sqlc.arg(reason_code), updated_at = now()
      FROM target
     WHERE runtime_instances.worker_instance_id = target.id
       AND runtime_instances.worker_epoch = target.current_epoch
       AND runtime_instances.reclaimed_at IS NULL
       AND runtime_instances.observed_state IN ('allocated', 'preparing', 'ready', 'closing')
    RETURNING runtime_instances.id
), lost_slots AS (
    UPDATE worker_network_slots
       SET state = 'lost', generation = generation + 1,
           lost_at = now(), state_reason_code = sqlc.arg(reason_code), updated_at = now()
      FROM target
     WHERE worker_network_slots.worker_instance_id = target.id
       AND worker_network_slots.worker_epoch = target.current_epoch
       AND worker_network_slots.state IN ('assigned', 'bound', 'reclaiming', 'quarantined')
    RETURNING worker_network_slots.id
)
SELECT target.*
  FROM target
 WHERE (SELECT count(*) FROM revoked_credentials) >= 0
   AND (SELECT count(*) FROM lost_mounts) >= 0
   AND (SELECT count(*) FROM lost_runtimes) >= 0
   AND (SELECT count(*) FROM lost_slots) >= 0;

-- name: ListWorkerInstances :many
SELECT * FROM worker_instances
 WHERE sqlc.arg(state_filter)::text = 'all' OR state::text = sqlc.arg(state_filter)::text
 ORDER BY updated_at DESC, created_at ASC
 LIMIT sqlc.arg(row_limit);

-- name: GetWorkerInstanceState :one
SELECT worker_instances.*,
       runtime_identities.rootfs_digest,
       runtime_identities.runtime_abi,
       ((SELECT count(*) FROM run_leases
         WHERE run_leases.worker_instance_id = worker_instances.id
           AND run_leases.worker_epoch = worker_instances.current_epoch
           AND run_leases.state IN ('assigned', 'starting', 'running', 'checkpointing')) +
        (SELECT count(*) FROM deployment_build_leases
         WHERE deployment_build_leases.worker_instance_id = worker_instances.id
           AND deployment_build_leases.worker_epoch = worker_instances.current_epoch
           AND deployment_build_leases.state IN ('assigned', 'starting', 'running')) +
        (SELECT count(*) FROM workspace_mounts
         WHERE workspace_mounts.worker_instance_id = worker_instances.id
           AND workspace_mounts.worker_epoch = worker_instances.current_epoch
           AND workspace_mounts.state IN ('mounting', 'mounted', 'unmounting')) +
        (SELECT count(*) FROM workspace_process_operations
         WHERE workspace_process_operations.claimed_by_worker_instance_id = worker_instances.id
           AND workspace_process_operations.claimed_worker_epoch = worker_instances.current_epoch
           AND workspace_process_operations.state IN ('claimed', 'running')) +
        (SELECT count(*) FROM runtime_instances
         WHERE runtime_instances.worker_instance_id = worker_instances.id
           AND runtime_instances.worker_epoch = worker_instances.current_epoch
           AND runtime_instances.observed_state IN ('allocated', 'preparing', 'ready', 'closing')))::int AS active_executions
  FROM worker_instances
  LEFT JOIN runtime_identities ON runtime_identities.id = worker_instances.runtime_identity_id
 WHERE worker_instances.id = sqlc.arg(id)
   AND worker_instances.worker_group_id = sqlc.arg(worker_group_id);

-- name: GetWorkerInstanceRunDispatchCapacity :one
SELECT GREATEST(worker_instances.certified_cpu_millis - usage.cpu_millis, 0)::bigint AS available_cpu_millis,
       GREATEST(worker_instances.certified_memory_bytes - usage.memory_bytes, 0)::bigint AS available_memory_bytes,
       GREATEST(worker_instances.certified_workload_disk_bytes - usage.workload_disk_bytes, 0)::bigint AS available_workload_disk_bytes,
       GREATEST(worker_instances.certified_scratch_bytes - usage.scratch_bytes, 0)::bigint AS available_scratch_bytes,
       GREATEST(worker_instances.max_vm_slots - usage.vm_slots, 0)::int AS available_vm_slots,
       GREATEST(worker_instances.max_run_consumers - usage.run_consumers, 0)::int AS available_run_consumers,
       GREATEST(worker_instances.max_build_executors - usage.build_executors, 0)::int AS available_build_executors
  FROM worker_instances
  CROSS JOIN LATERAL (
      SELECT
        COALESCE((SELECT sum(reserved_cpu_millis) FROM runtime_instances
                    WHERE worker_instance_id = worker_instances.id
                      AND worker_epoch = worker_instances.current_epoch
                      AND (observed_state IN ('allocated','preparing','ready','closing')
                           OR (observed_state IN ('failed','lost') AND reclaimed_at IS NULL))), 0)
        + COALESCE((SELECT sum(requested_cpu_millis) FROM deployment_build_leases
                    WHERE worker_instance_id = worker_instances.id
                      AND worker_epoch = worker_instances.current_epoch
                      AND state IN ('assigned','starting','running')), 0) AS cpu_millis,
        COALESCE((SELECT sum(reserved_memory_bytes) FROM runtime_instances
                    WHERE worker_instance_id = worker_instances.id
                      AND worker_epoch = worker_instances.current_epoch
                      AND (observed_state IN ('allocated','preparing','ready','closing')
                           OR (observed_state IN ('failed','lost') AND reclaimed_at IS NULL))), 0)
        + COALESCE((SELECT sum(requested_memory_bytes) FROM deployment_build_leases
                    WHERE worker_instance_id = worker_instances.id
                      AND worker_epoch = worker_instances.current_epoch
                      AND state IN ('assigned','starting','running')), 0) AS memory_bytes,
        COALESCE((SELECT sum(reserved_workload_disk_bytes) FROM runtime_instances
                    WHERE worker_instance_id = worker_instances.id
                      AND worker_epoch = worker_instances.current_epoch
                      AND (observed_state IN ('allocated','preparing','ready','closing')
                           OR (observed_state IN ('failed','lost') AND reclaimed_at IS NULL))), 0)
        + COALESCE((SELECT sum(requested_workload_disk_bytes) FROM deployment_build_leases
                    WHERE worker_instance_id = worker_instances.id
                      AND worker_epoch = worker_instances.current_epoch
                      AND state IN ('assigned','starting','running')), 0) AS workload_disk_bytes,
        COALESCE((SELECT sum(reserved_scratch_bytes) FROM runtime_instances
                    WHERE worker_instance_id = worker_instances.id
                      AND worker_epoch = worker_instances.current_epoch
                      AND (observed_state IN ('allocated','preparing','ready','closing')
                           OR (observed_state IN ('failed','lost') AND reclaimed_at IS NULL))), 0)
        + COALESCE((SELECT sum(requested_scratch_bytes) FROM deployment_build_leases
                    WHERE worker_instance_id = worker_instances.id
                      AND worker_epoch = worker_instances.current_epoch
                      AND state IN ('assigned','starting','running')), 0) AS scratch_bytes,
        COALESCE((SELECT count(*) FROM runtime_instances
                   WHERE worker_instance_id = worker_instances.id
                     AND worker_epoch = worker_instances.current_epoch
                     AND (observed_state IN ('allocated','preparing','ready','closing')
                          OR (observed_state IN ('failed','lost') AND reclaimed_at IS NULL))), 0)::int AS vm_slots,
        COALESCE((SELECT sum(requested_execution_slots) FROM run_leases
                   WHERE worker_instance_id = worker_instances.id
                     AND worker_epoch = worker_instances.current_epoch
                     AND state IN ('assigned','starting','running','checkpointing')), 0)::int AS run_consumers,
        COALESCE((SELECT sum(requested_build_executors) FROM deployment_build_leases
                   WHERE worker_instance_id = worker_instances.id
                     AND worker_epoch = worker_instances.current_epoch
                     AND state IN ('assigned','starting','running')), 0)::int AS build_executors
  ) usage
 WHERE worker_instances.id = sqlc.arg(id)
   AND worker_instances.worker_group_id = sqlc.arg(worker_group_id)
   AND worker_instances.state = 'active'
   AND worker_instances.current_epoch = sqlc.arg(worker_epoch);

-- name: GetWorkerInstanceQueueCapacity :one
SELECT GREATEST(worker_instances.certified_cpu_millis - usage.cpu_millis, 0)::bigint AS available_cpu_millis,
       GREATEST(worker_instances.certified_memory_bytes - usage.memory_bytes, 0)::bigint AS available_memory_bytes,
       GREATEST(worker_instances.certified_workload_disk_bytes - usage.workload_disk_bytes, 0)::bigint AS available_workload_disk_bytes,
       GREATEST(worker_instances.certified_scratch_bytes - usage.scratch_bytes, 0)::bigint AS available_scratch_bytes,
       GREATEST(worker_instances.max_run_consumers - usage.run_consumers, 0)::int AS available_run_consumers,
       GREATEST(worker_instances.max_build_executors - usage.build_executors, 0)::int AS available_build_executors
  FROM worker_instances
  CROSS JOIN LATERAL (
      SELECT
        COALESCE((SELECT sum(reserved_cpu_millis) FROM runtime_instances
                   WHERE worker_instance_id = worker_instances.id
                     AND worker_epoch = worker_instances.current_epoch
                     AND (observed_state IN ('allocated','preparing','ready','closing')
                          OR (observed_state IN ('failed','lost') AND reclaimed_at IS NULL))), 0)
        + COALESCE((SELECT sum(requested_cpu_millis) FROM deployment_build_leases
                    WHERE worker_instance_id = worker_instances.id
                      AND worker_epoch = worker_instances.current_epoch
                      AND state IN ('assigned','starting','running')), 0) AS cpu_millis,
        COALESCE((SELECT sum(reserved_memory_bytes) FROM runtime_instances
                   WHERE worker_instance_id = worker_instances.id
                     AND worker_epoch = worker_instances.current_epoch
                     AND (observed_state IN ('allocated','preparing','ready','closing')
                          OR (observed_state IN ('failed','lost') AND reclaimed_at IS NULL))), 0)
        + COALESCE((SELECT sum(requested_memory_bytes) FROM deployment_build_leases
                    WHERE worker_instance_id = worker_instances.id
                      AND worker_epoch = worker_instances.current_epoch
                      AND state IN ('assigned','starting','running')), 0) AS memory_bytes,
        COALESCE((SELECT sum(reserved_workload_disk_bytes) FROM runtime_instances
                   WHERE worker_instance_id = worker_instances.id
                     AND worker_epoch = worker_instances.current_epoch
                     AND (observed_state IN ('allocated','preparing','ready','closing')
                          OR (observed_state IN ('failed','lost') AND reclaimed_at IS NULL))), 0)
        + COALESCE((SELECT sum(requested_workload_disk_bytes) FROM deployment_build_leases
                    WHERE worker_instance_id = worker_instances.id
                      AND worker_epoch = worker_instances.current_epoch
                      AND state IN ('assigned','starting','running')), 0) AS workload_disk_bytes,
        COALESCE((SELECT sum(reserved_scratch_bytes) FROM runtime_instances
                   WHERE worker_instance_id = worker_instances.id
                     AND worker_epoch = worker_instances.current_epoch
                     AND (observed_state IN ('allocated','preparing','ready','closing')
                          OR (observed_state IN ('failed','lost') AND reclaimed_at IS NULL))), 0)
        + COALESCE((SELECT sum(requested_scratch_bytes) FROM deployment_build_leases
                    WHERE worker_instance_id = worker_instances.id
                      AND worker_epoch = worker_instances.current_epoch
                      AND state IN ('assigned','starting','running')), 0) AS scratch_bytes,
        COALESCE((SELECT sum(requested_execution_slots) FROM run_leases
                   WHERE worker_instance_id = worker_instances.id
                     AND worker_epoch = worker_instances.current_epoch
                     AND state IN ('assigned','starting','running','checkpointing')), 0)::int AS run_consumers,
        COALESCE((SELECT sum(requested_build_executors) FROM deployment_build_leases
                   WHERE worker_instance_id = worker_instances.id
                     AND worker_epoch = worker_instances.current_epoch
                     AND state IN ('assigned','starting','running')), 0)::int AS build_executors
  ) usage
 WHERE worker_instances.id = sqlc.arg(id)
   AND worker_instances.worker_group_id = sqlc.arg(worker_group_id)
   AND worker_instances.current_epoch = sqlc.arg(worker_epoch)
   AND worker_instances.state = 'active';

-- name: PrepareQueuedRunDispatch :one
SELECT runs.*, workspaces.region_id
  FROM runs
  JOIN workspaces
    ON workspaces.org_id = runs.org_id
   AND workspaces.project_id = runs.project_id
   AND workspaces.environment_id = runs.environment_id
   AND workspaces.id = runs.workspace_id
 WHERE runs.org_id = sqlc.arg(org_id)
   AND runs.id = sqlc.arg(run_id)
   AND runs.status = 'queued'
   AND runs.current_run_lease_id IS NULL
   AND runs.queue_timestamp <= now()
   AND (runs.queued_expires_at IS NULL OR runs.queued_expires_at > now())
 FOR UPDATE OF runs;

-- name: ListQueuedRunCandidateScopes :many
WITH candidate_scopes AS (
    SELECT runs.org_id, runs.project_id, runs.environment_id, workspaces.region_id,
           runs.queue_class, runs.queue_name,
           md5(runs.org_id::text || ':' || runs.project_id::text || ':' ||
               runs.environment_id::text || ':' || workspaces.region_id || ':' ||
               runs.queue_class || ':' || runs.queue_name || ':' || sqlc.arg(scan_seed)::text) AS sort_key
      FROM runs
      JOIN workspaces ON workspaces.org_id = runs.org_id
                     AND workspaces.project_id = runs.project_id
                     AND workspaces.environment_id = runs.environment_id
                     AND workspaces.id = runs.workspace_id
     WHERE runs.status = 'queued' AND runs.current_run_lease_id IS NULL
       AND runs.queue_timestamp <= now()
       AND (runs.queued_expires_at IS NULL OR runs.queued_expires_at > now())
     GROUP BY runs.org_id, runs.project_id, runs.environment_id, workspaces.region_id,
              runs.queue_class, runs.queue_name
)
SELECT * FROM candidate_scopes
 WHERE sqlc.arg(after_sort_key)::text = ''
    OR (sort_key, org_id, project_id, environment_id, region_id, queue_class, queue_name)
       > (sqlc.arg(after_sort_key)::text, sqlc.arg(after_org_id)::uuid,
          sqlc.arg(after_project_id)::uuid, sqlc.arg(after_environment_id)::uuid,
          sqlc.arg(after_region_id)::text, sqlc.arg(after_queue_class)::text,
          sqlc.arg(after_queue_name)::text)
 ORDER BY sort_key, org_id, project_id, environment_id, region_id, queue_class, queue_name
 LIMIT sqlc.arg(row_limit);

-- name: ListQueuedRunDispatchCandidatesForScope :many
SELECT runs.org_id, runs.id AS run_id, runs.state_version
  FROM runs
  JOIN workspaces ON workspaces.org_id = runs.org_id
                 AND workspaces.project_id = runs.project_id
                 AND workspaces.environment_id = runs.environment_id
                 AND workspaces.id = runs.workspace_id
 WHERE runs.org_id = sqlc.arg(org_id)
   AND runs.project_id = sqlc.arg(project_id)
   AND runs.environment_id = sqlc.arg(environment_id)
   AND workspaces.region_id = sqlc.arg(region_id)
   AND runs.queue_class = sqlc.arg(queue_class)
   AND runs.queue_name = sqlc.arg(queue_name)
   AND runs.status = 'queued' AND runs.current_run_lease_id IS NULL
   AND runs.queue_timestamp <= now()
   AND (runs.queued_expires_at IS NULL OR runs.queued_expires_at > now())
 ORDER BY runs.priority DESC, runs.queue_timestamp, runs.id
 LIMIT sqlc.arg(row_limit);

-- name: CompleteRunDispatch :one
SELECT * FROM runs WHERE org_id = sqlc.arg(org_id) AND id = sqlc.arg(run_id);

-- name: DeadLetterRunDispatch :one
WITH terminalized AS (
    UPDATE runs
       SET status = 'failed', execution_status = 'finished', terminal_outcome = 'dead_lettered',
           current_run_lease_id = NULL, state_version = state_version + 1,
           error_message = sqlc.arg(last_error), finished_at = COALESCE(finished_at, now()),
           updated_at = now()
     WHERE runs.org_id = sqlc.arg(org_id) AND runs.id = sqlc.arg(run_id)
       AND runs.status = 'queued' AND runs.state_version = sqlc.arg(expected_run_state_version)
    RETURNING *
), snapshot AS (
    INSERT INTO run_state_snapshots
        (org_id, run_id, version, status, execution_status, terminal_outcome,
         attempt_number, previous_version, transition, reason, error)
    SELECT terminalized.org_id, terminalized.id, terminalized.state_version,
           terminalized.status, terminalized.execution_status, terminalized.terminal_outcome,
           terminalized.current_attempt_number, terminalized.state_version - 1, 'run.dead_lettered',
           jsonb_build_object('message', sqlc.arg(last_error)::text),
           jsonb_build_object('message', sqlc.arg(last_error)::text)
      FROM terminalized
    RETURNING run_id
)
SELECT terminalized.id AS run_id, terminalized.org_id, terminalized.project_id,
       terminalized.environment_id, terminalized.state_version
  FROM terminalized JOIN snapshot ON snapshot.run_id = terminalized.id;
