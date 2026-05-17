-- name: UpsertWorkerHeartbeat :one
WITH default_pool AS (
    SELECT worker_pools.id
      FROM worker_pools
      JOIN projects ON projects.org_id = worker_pools.org_id
                   AND projects.id = worker_pools.project_id
                   AND projects.is_default
                   AND projects.archived_at IS NULL
      JOIN environments ON environments.org_id = worker_pools.org_id
                       AND environments.project_id = worker_pools.project_id
                       AND environments.id = worker_pools.environment_id
                       AND environments.is_default
                       AND environments.archived_at IS NULL
     WHERE worker_pools.org_id = sqlc.arg(org_id)
       AND worker_pools.is_default
       AND worker_pools.archived_at IS NULL
     LIMIT 1
)
INSERT INTO workers (
    org_id,
    worker_pool_id,
    id,
    status,
    runtime_arch,
    runtime_abi,
    kernel_digest,
    rootfs_digest,
    cni_profile,
    max_vcpus,
    max_memory_mib,
    slots_available,
    last_seen_at
) VALUES (
    sqlc.arg(org_id),
    (SELECT id FROM default_pool),
    sqlc.arg(id),
    'active',
    sqlc.arg(runtime_arch),
    sqlc.arg(runtime_abi),
    sqlc.arg(kernel_digest),
    sqlc.arg(rootfs_digest),
    sqlc.arg(cni_profile),
    sqlc.arg(max_vcpus),
    sqlc.arg(max_memory_mib),
    sqlc.arg(slots_available),
    now()
)
ON CONFLICT (org_id, id) DO UPDATE
   SET worker_pool_id = excluded.worker_pool_id,
       runtime_arch = excluded.runtime_arch,
       runtime_abi = excluded.runtime_abi,
       kernel_digest = excluded.kernel_digest,
       rootfs_digest = excluded.rootfs_digest,
       cni_profile = excluded.cni_profile,
       max_vcpus = excluded.max_vcpus,
       max_memory_mib = excluded.max_memory_mib,
       slots_available = excluded.slots_available,
       last_seen_at = now()
RETURNING *;

-- name: UpsertScopedWorkerHeartbeat :one
INSERT INTO workers (
    org_id,
    worker_pool_id,
    id,
    status,
    runtime_arch,
    runtime_abi,
    kernel_digest,
    rootfs_digest,
    cni_profile,
    max_vcpus,
    max_memory_mib,
    slots_available,
    last_seen_at
) VALUES (
    sqlc.arg(org_id),
    sqlc.arg(worker_pool_id),
    sqlc.arg(id),
    'active',
    sqlc.arg(runtime_arch),
    sqlc.arg(runtime_abi),
    sqlc.arg(kernel_digest),
    sqlc.arg(rootfs_digest),
    sqlc.arg(cni_profile),
    sqlc.arg(max_vcpus),
    sqlc.arg(max_memory_mib),
    sqlc.arg(slots_available),
    now()
)
ON CONFLICT (org_id, id) DO UPDATE
   SET worker_pool_id = excluded.worker_pool_id,
       runtime_arch = excluded.runtime_arch,
       runtime_abi = excluded.runtime_abi,
       kernel_digest = excluded.kernel_digest,
       rootfs_digest = excluded.rootfs_digest,
       cni_profile = excluded.cni_profile,
       max_vcpus = excluded.max_vcpus,
       max_memory_mib = excluded.max_memory_mib,
       slots_available = excluded.slots_available,
       last_seen_at = now()
RETURNING *;

-- name: RefreshWorkerHeartbeat :one
UPDATE workers
   SET runtime_arch = sqlc.arg(runtime_arch),
       runtime_abi = sqlc.arg(runtime_abi),
       kernel_digest = sqlc.arg(kernel_digest),
       rootfs_digest = sqlc.arg(rootfs_digest),
       cni_profile = sqlc.arg(cni_profile),
       max_vcpus = sqlc.arg(max_vcpus),
       max_memory_mib = sqlc.arg(max_memory_mib),
       slots_available = sqlc.arg(slots_available),
       last_seen_at = now()
 WHERE org_id = sqlc.arg(org_id)
   AND id = sqlc.arg(id)
RETURNING *;

-- name: RefreshScopedWorkerHeartbeat :one
UPDATE workers
   SET runtime_arch = sqlc.arg(runtime_arch),
       runtime_abi = sqlc.arg(runtime_abi),
       kernel_digest = sqlc.arg(kernel_digest),
       rootfs_digest = sqlc.arg(rootfs_digest),
       cni_profile = sqlc.arg(cni_profile),
       max_vcpus = sqlc.arg(max_vcpus),
       max_memory_mib = sqlc.arg(max_memory_mib),
       slots_available = sqlc.arg(slots_available),
       last_seen_at = now()
 WHERE org_id = sqlc.arg(org_id)
   AND worker_pool_id = sqlc.arg(worker_pool_id)
   AND id = sqlc.arg(id)
RETURNING *;

-- name: ActivateWorkerHeartbeat :one
WITH default_pool AS (
    SELECT worker_pools.id
      FROM worker_pools
      JOIN projects ON projects.org_id = worker_pools.org_id
                   AND projects.id = worker_pools.project_id
                   AND projects.is_default
                   AND projects.archived_at IS NULL
      JOIN environments ON environments.org_id = worker_pools.org_id
                       AND environments.project_id = worker_pools.project_id
                       AND environments.id = worker_pools.environment_id
                       AND environments.is_default
                       AND environments.archived_at IS NULL
     WHERE worker_pools.org_id = sqlc.arg(org_id)
       AND worker_pools.is_default
       AND worker_pools.archived_at IS NULL
     LIMIT 1
)
INSERT INTO workers (
    org_id,
    worker_pool_id,
    id,
    status,
    runtime_arch,
    runtime_abi,
    kernel_digest,
    rootfs_digest,
    cni_profile,
    max_vcpus,
    max_memory_mib,
    slots_available,
    last_seen_at
) VALUES (
    sqlc.arg(org_id),
    (SELECT id FROM default_pool),
    sqlc.arg(id),
    'active',
    sqlc.arg(runtime_arch),
    sqlc.arg(runtime_abi),
    sqlc.arg(kernel_digest),
    sqlc.arg(rootfs_digest),
    sqlc.arg(cni_profile),
    sqlc.arg(max_vcpus),
    sqlc.arg(max_memory_mib),
    sqlc.arg(slots_available),
    now()
)
ON CONFLICT (org_id, id) DO UPDATE
   SET status = 'active',
       worker_pool_id = excluded.worker_pool_id,
       runtime_arch = excluded.runtime_arch,
       runtime_abi = excluded.runtime_abi,
       kernel_digest = excluded.kernel_digest,
       rootfs_digest = excluded.rootfs_digest,
       cni_profile = excluded.cni_profile,
       max_vcpus = excluded.max_vcpus,
       max_memory_mib = excluded.max_memory_mib,
       slots_available = excluded.slots_available,
       last_seen_at = now()
RETURNING *;

-- name: ActivateScopedWorkerHeartbeat :one
INSERT INTO workers (
    org_id,
    worker_pool_id,
    id,
    status,
    runtime_arch,
    runtime_abi,
    kernel_digest,
    rootfs_digest,
    cni_profile,
    max_vcpus,
    max_memory_mib,
    slots_available,
    last_seen_at
) VALUES (
    sqlc.arg(org_id),
    sqlc.arg(worker_pool_id),
    sqlc.arg(id),
    'active',
    sqlc.arg(runtime_arch),
    sqlc.arg(runtime_abi),
    sqlc.arg(kernel_digest),
    sqlc.arg(rootfs_digest),
    sqlc.arg(cni_profile),
    sqlc.arg(max_vcpus),
    sqlc.arg(max_memory_mib),
    sqlc.arg(slots_available),
    now()
)
ON CONFLICT (org_id, id) DO UPDATE
   SET status = 'active',
       worker_pool_id = excluded.worker_pool_id,
       runtime_arch = excluded.runtime_arch,
       runtime_abi = excluded.runtime_abi,
       kernel_digest = excluded.kernel_digest,
       rootfs_digest = excluded.rootfs_digest,
       cni_profile = excluded.cni_profile,
       max_vcpus = excluded.max_vcpus,
       max_memory_mib = excluded.max_memory_mib,
       slots_available = excluded.slots_available,
       last_seen_at = now()
RETURNING *;

-- name: DrainWorker :one
UPDATE workers
   SET status = 'draining',
       last_seen_at = now()
 WHERE org_id = sqlc.arg(org_id)
   AND id = sqlc.arg(id)
RETURNING *;

-- name: GetWorkerState :one
SELECT workers.*,
       (
           SELECT count(*)::int
             FROM run_executions
            WHERE run_executions.org_id = workers.org_id
              AND run_executions.worker_id = workers.id
              AND run_executions.status IN ('claimed', 'running')
       ) AS active_executions
  FROM workers
 WHERE workers.org_id = sqlc.arg(org_id)
   AND workers.id = sqlc.arg(id);

-- name: ListScopedWorkers :many
SELECT workers.*
  FROM workers
  JOIN worker_pools ON worker_pools.org_id = workers.org_id
                   AND worker_pools.id = workers.worker_pool_id
 WHERE workers.org_id = sqlc.arg(org_id)
   AND worker_pools.project_id = sqlc.arg(project_id)
   AND worker_pools.environment_id = sqlc.arg(environment_id)
   AND (
       sqlc.arg(status_filter)::text = 'all'
       OR workers.status::text = sqlc.arg(status_filter)::text
   )
 ORDER BY workers.last_seen_at DESC
 LIMIT sqlc.arg(row_limit);

-- name: CreateWorkerPool :one
INSERT INTO worker_pools (id, org_id, project_id, environment_id, slug, name, is_default)
VALUES (
    sqlc.arg(id),
    sqlc.arg(org_id),
    sqlc.arg(project_id),
    sqlc.arg(environment_id),
    sqlc.arg(slug),
    sqlc.arg(name),
    sqlc.arg(is_default)
)
RETURNING *;

-- name: ListWorkerPools :many
SELECT *
  FROM worker_pools
 WHERE org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND archived_at IS NULL
 ORDER BY is_default DESC, lower(slug), created_at ASC;
