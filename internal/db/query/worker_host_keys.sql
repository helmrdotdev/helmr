-- name: AuthenticateWorkerHostKey :one
-- Each dependent stage locks one supply row before the next, matching runtime
-- admission and lifecycle operations. A joined FOR UPDATE does not guarantee order.
WITH locked_group AS MATERIALIZED (
    SELECT worker_groups.id, worker_groups.claim_version
      FROM worker_host_keys
      JOIN worker_hosts ON worker_hosts.id = worker_host_keys.worker_host_id
                           AND worker_hosts.worker_group_id = worker_host_keys.worker_group_id
      JOIN worker_groups ON worker_groups.id = worker_host_keys.worker_group_id
      JOIN worker_pools ON worker_pools.id = worker_hosts.worker_pool_id
                       AND worker_pools.worker_group_id = worker_hosts.worker_group_id
     WHERE worker_host_keys.worker_host_id = sqlc.arg(worker_host_id)
       AND worker_host_keys.id = sqlc.arg(host_key_id)
       AND worker_host_keys.revoked_at IS NULL
       AND worker_host_keys.claim_version = worker_hosts.claim_version
       AND worker_hosts.status IN ('registering','active','draining')
       AND worker_groups.status IN ('active','paused','draining')
       AND worker_pools.status IN ('pending','active','draining')
     FOR UPDATE OF worker_groups
), locked_pool AS MATERIALIZED (
    SELECT worker_pools.id, worker_pools.worker_group_id
      FROM worker_pools
      JOIN locked_group ON locked_group.id = worker_pools.worker_group_id
      JOIN worker_hosts ON worker_hosts.worker_pool_id = worker_pools.id
                           AND worker_hosts.worker_group_id = locked_group.id
     WHERE worker_hosts.id = sqlc.arg(worker_host_id)
       AND worker_pools.status IN ('pending','active','draining')
     FOR UPDATE OF worker_pools
), locked_host AS MATERIALIZED (
    SELECT worker_hosts.*
      FROM worker_hosts
      JOIN locked_pool ON locked_pool.id = worker_hosts.worker_pool_id
                          AND locked_pool.worker_group_id = worker_hosts.worker_group_id
     WHERE worker_hosts.id = sqlc.arg(worker_host_id)
       AND worker_hosts.status IN ('registering','active','draining')
     FOR UPDATE OF worker_hosts
), host_key AS (
    SELECT worker_host_keys.*,
           locked_group.claim_version AS group_claim_version
      FROM worker_host_keys
      JOIN locked_host ON locked_host.id = worker_host_keys.worker_host_id
                          AND locked_host.worker_group_id = worker_host_keys.worker_group_id
      JOIN locked_group ON locked_group.id = worker_host_keys.worker_group_id
     WHERE worker_host_keys.id = sqlc.arg(host_key_id)
       AND worker_host_keys.revoked_at IS NULL
       AND worker_host_keys.claim_version = locked_host.claim_version
     FOR UPDATE OF worker_host_keys
), advanced AS (
    UPDATE worker_hosts
       SET current_epoch = CASE WHEN worker_hosts.current_service_id = sqlc.arg(service_id)
                                THEN worker_hosts.current_epoch
                                ELSE COALESCE(worker_hosts.current_epoch, 0) + 1 END,
           current_service_id = sqlc.arg(service_id),
           epoch_started_at = CASE WHEN worker_hosts.current_service_id = sqlc.arg(service_id)
                                   THEN worker_hosts.epoch_started_at ELSE now() END,
           status = CASE
               WHEN worker_hosts.current_service_id = sqlc.arg(service_id) THEN worker_hosts.status
               WHEN worker_hosts.status = 'active' THEN 'registering'
               ELSE worker_hosts.status
           END,
	       vm_platform_id = CASE
               WHEN worker_hosts.current_service_id = sqlc.arg(service_id)
               THEN worker_hosts.vm_platform_id
               ELSE NULL
           END,
           epoch_cpu_millis = CASE
               WHEN worker_hosts.current_service_id = sqlc.arg(service_id)
               THEN worker_hosts.epoch_cpu_millis ELSE 0
           END,
           epoch_memory_bytes = CASE
               WHEN worker_hosts.current_service_id = sqlc.arg(service_id)
               THEN worker_hosts.epoch_memory_bytes ELSE 0
           END,
           epoch_guest_ephemeral_disk_bytes = CASE
               WHEN worker_hosts.current_service_id = sqlc.arg(service_id)
               THEN worker_hosts.epoch_guest_ephemeral_disk_bytes ELSE 0
           END,
           per_vm_cpu_millis = CASE
               WHEN worker_hosts.current_service_id = sqlc.arg(service_id)
               THEN worker_hosts.per_vm_cpu_millis ELSE 0
           END,
           per_vm_memory_bytes = CASE
               WHEN worker_hosts.current_service_id = sqlc.arg(service_id)
               THEN worker_hosts.per_vm_memory_bytes ELSE 0
           END,
           per_vm_guest_ephemeral_disk_bytes = CASE
               WHEN worker_hosts.current_service_id = sqlc.arg(service_id)
               THEN worker_hosts.per_vm_guest_ephemeral_disk_bytes ELSE 0
           END,
	       max_vm_slots = CASE
	           WHEN worker_hosts.current_service_id = sqlc.arg(service_id)
	           THEN worker_hosts.max_vm_slots ELSE 0
	       END,
           max_vm_starts = CASE
               WHEN worker_hosts.current_service_id = sqlc.arg(service_id)
               THEN worker_hosts.max_vm_starts ELSE 0
           END,
           cpu_environment = CASE
               WHEN worker_hosts.current_service_id = sqlc.arg(service_id)
               THEN worker_hosts.cpu_environment ELSE NULL
           END,
           cpu_environment_digest = CASE
               WHEN worker_hosts.current_service_id = sqlc.arg(service_id)
               THEN worker_hosts.cpu_environment_digest ELSE NULL
           END,
           activated_at = CASE WHEN worker_hosts.current_service_id = sqlc.arg(service_id)
                               THEN worker_hosts.activated_at ELSE NULL END,
           resource_observation_sequence = CASE WHEN worker_hosts.current_service_id = sqlc.arg(service_id) THEN worker_hosts.resource_observation_sequence ELSE NULL END,
           resource_policy_revision = CASE WHEN worker_hosts.current_service_id = sqlc.arg(service_id) THEN worker_hosts.resource_policy_revision ELSE NULL END,
           resource_observed_at = CASE WHEN worker_hosts.current_service_id = sqlc.arg(service_id) THEN worker_hosts.resource_observed_at ELSE NULL END,
           local_optional_cpu_millis = CASE WHEN worker_hosts.current_service_id = sqlc.arg(service_id) THEN worker_hosts.local_optional_cpu_millis ELSE NULL END,
           local_optional_memory_bytes = CASE WHEN worker_hosts.current_service_id = sqlc.arg(service_id) THEN worker_hosts.local_optional_memory_bytes ELSE NULL END,
           optional_disk_bytes = CASE WHEN worker_hosts.current_service_id = sqlc.arg(service_id) THEN worker_hosts.optional_disk_bytes ELSE NULL END,
           local_optional_vm_slots = CASE WHEN worker_hosts.current_service_id = sqlc.arg(service_id) THEN worker_hosts.local_optional_vm_slots ELSE NULL END,
           local_optional_tasks = CASE WHEN worker_hosts.current_service_id = sqlc.arg(service_id) THEN worker_hosts.local_optional_tasks ELSE NULL END,
           optional_reclaim_pending = CASE WHEN worker_hosts.current_service_id = sqlc.arg(service_id) THEN worker_hosts.optional_reclaim_pending ELSE NULL END,
           disk_slots_for_real_work = CASE WHEN worker_hosts.current_service_id = sqlc.arg(service_id) THEN worker_hosts.disk_slots_for_real_work ELSE NULL END,
           ready_leaf_keys = CASE WHEN worker_hosts.current_service_id = sqlc.arg(service_id) THEN worker_hosts.ready_leaf_keys ELSE NULL END,
           observed_at = CASE WHEN worker_hosts.current_service_id = sqlc.arg(service_id)
                              THEN worker_hosts.observed_at ELSE NULL END,
	       admission_paused_reason = CASE WHEN worker_hosts.current_service_id = sqlc.arg(service_id)
	                                THEN worker_hosts.admission_paused_reason ELSE NULL END,
           updated_at = now()
      FROM host_key
     WHERE worker_hosts.id = host_key.worker_host_id
    RETURNING worker_hosts.*
)
SELECT host_key.id, host_key.worker_group_id,
       host_key.worker_host_id, host_key.claim_version,
       host_key.group_claim_version,
       advanced.current_epoch, advanced.current_service_id, advanced.status,
       advanced.resource_id
  FROM host_key JOIN advanced ON advanced.id = host_key.worker_host_id;

-- name: AuthorizeWorkerHostKey :one
UPDATE worker_host_keys
   SET last_used_at = now()
  FROM worker_hosts, worker_groups, worker_pools
 WHERE worker_host_keys.id = sqlc.arg(host_key_id)
   AND worker_hosts.id = worker_host_keys.worker_host_id
   AND worker_hosts.worker_group_id = worker_host_keys.worker_group_id
   AND worker_groups.id = worker_host_keys.worker_group_id
   AND worker_pools.id = worker_hosts.worker_pool_id
   AND worker_pools.worker_group_id = worker_hosts.worker_group_id
   AND worker_host_keys.revoked_at IS NULL
   AND worker_host_keys.claim_version = sqlc.arg(claim_version)
   AND worker_host_keys.claim_version = worker_hosts.claim_version
	AND worker_groups.claim_version = sqlc.arg(group_claim_version)
	AND worker_hosts.current_epoch = sqlc.arg(worker_epoch)
	AND worker_hosts.status IN ('active','draining')
   AND worker_groups.status IN ('active','paused','draining')
   AND worker_pools.status IN ('active','draining')
RETURNING worker_host_keys.*, worker_hosts.resource_id,
          worker_hosts.current_epoch, worker_hosts.status AS worker_status,
          worker_hosts.epoch_started_at,
          worker_groups.owner_org_id AS worker_group_owner_org_id;

-- name: AuthorizeActivatingWorkerHostKey :one
UPDATE worker_host_keys
   SET last_used_at = now()
  FROM worker_hosts, worker_groups, worker_pools
 WHERE worker_host_keys.id = sqlc.arg(host_key_id)
   AND worker_hosts.id = worker_host_keys.worker_host_id
   AND worker_hosts.worker_group_id = worker_host_keys.worker_group_id
   AND worker_groups.id = worker_host_keys.worker_group_id
   AND worker_pools.id = worker_hosts.worker_pool_id
   AND worker_pools.worker_group_id = worker_hosts.worker_group_id
   AND worker_host_keys.revoked_at IS NULL
   AND worker_host_keys.claim_version = sqlc.arg(claim_version)
   AND worker_host_keys.claim_version = worker_hosts.claim_version
   AND worker_groups.claim_version = sqlc.arg(group_claim_version)
   AND worker_hosts.current_epoch = sqlc.arg(worker_epoch)
   AND worker_hosts.status IN ('registering', 'active', 'draining')
   AND worker_groups.status IN ('active','paused','draining')
   AND worker_pools.status IN ('pending','active','draining')
RETURNING worker_host_keys.*, worker_hosts.resource_id,
          worker_hosts.current_epoch, worker_hosts.status AS worker_status,
          worker_hosts.epoch_started_at,
          worker_groups.owner_org_id AS worker_group_owner_org_id;

-- name: AuthorizeRecoveringWorkerHostKey :one
UPDATE worker_host_keys
   SET last_used_at = now()
  FROM worker_hosts, worker_groups, worker_pools
 WHERE worker_host_keys.id = sqlc.arg(host_key_id)
   AND worker_hosts.id = worker_host_keys.worker_host_id
   AND worker_hosts.worker_group_id = worker_host_keys.worker_group_id
   AND worker_groups.id = worker_host_keys.worker_group_id
   AND worker_pools.id = worker_hosts.worker_pool_id
   AND worker_pools.worker_group_id = worker_hosts.worker_group_id
   AND worker_host_keys.revoked_at IS NULL
   AND worker_host_keys.claim_version = sqlc.arg(claim_version)
   AND worker_host_keys.claim_version = worker_hosts.claim_version
   AND worker_groups.claim_version = sqlc.arg(group_claim_version)
   AND worker_hosts.current_epoch = sqlc.arg(worker_epoch)
	AND (
	    worker_hosts.status = 'registering'
	    OR (
	        worker_hosts.status = 'draining'
	        AND worker_hosts.vm_platform_id IS NULL
	    )
   )
   AND worker_groups.status IN ('active','paused','draining')
   AND worker_pools.status IN ('pending','active','draining')
RETURNING worker_host_keys.*, worker_hosts.resource_id,
          worker_hosts.current_epoch, worker_hosts.status AS worker_status,
          worker_hosts.epoch_started_at,
          worker_groups.owner_org_id AS worker_group_owner_org_id;

-- name: AuthorizeWorkerDrainReplay :one
SELECT worker_host_keys.*, worker_hosts.resource_id,
       worker_hosts.current_epoch, worker_hosts.status AS worker_status,
       worker_hosts.epoch_started_at,
       worker_groups.owner_org_id AS worker_group_owner_org_id
  FROM worker_host_keys
  JOIN worker_hosts
    ON worker_hosts.id = worker_host_keys.worker_host_id
   AND worker_hosts.worker_group_id = worker_host_keys.worker_group_id
  JOIN worker_groups ON worker_groups.id = worker_host_keys.worker_group_id
 WHERE worker_host_keys.id = sqlc.arg(host_key_id)
   AND worker_host_keys.claim_version = sqlc.arg(claim_version)
   AND worker_host_keys.revoked_at IS NOT NULL
   AND worker_hosts.current_epoch = sqlc.arg(worker_epoch)
   AND worker_hosts.status = 'termination_ready'
   AND worker_hosts.claim_version = worker_host_keys.claim_version + 1;

-- name: AuthorizeWorkerFenceReplay :one
SELECT worker_host_keys.*, worker_hosts.resource_id,
       worker_hosts.current_epoch, worker_hosts.status AS worker_status,
       worker_hosts.epoch_started_at,
       worker_groups.owner_org_id AS worker_group_owner_org_id
  FROM worker_host_keys
  JOIN worker_hosts
    ON worker_hosts.id = worker_host_keys.worker_host_id
   AND worker_hosts.worker_group_id = worker_host_keys.worker_group_id
  JOIN worker_groups ON worker_groups.id = worker_host_keys.worker_group_id
 WHERE worker_host_keys.id = sqlc.arg(host_key_id)
   AND worker_host_keys.claim_version = sqlc.arg(claim_version)
   AND worker_host_keys.revoked_at IS NOT NULL
   AND worker_hosts.current_epoch = sqlc.arg(worker_epoch)
   AND worker_hosts.status = 'lost'
   AND worker_hosts.claim_version = worker_host_keys.claim_version + 1;

-- EnrollHost resolves and locks the enrollment token and its group, and owns
-- the physical-custody exclusion, before this statement.
-- name: EnrollWorkerHost :one
WITH pool AS (
    INSERT INTO worker_pools (id, worker_group_id, name, status, claim_version)
    VALUES (sqlc.arg(worker_pool_id), sqlc.arg(worker_group_id), sqlc.arg(pool_name), 'pending', 1)
    ON CONFLICT (worker_group_id, name)
    DO UPDATE SET updated_at = worker_pools.updated_at
	 WHERE worker_pools.status IN ('pending', 'active')
    RETURNING worker_pools.*
), worker AS (
    INSERT INTO worker_hosts (
	    id, worker_group_id, worker_pool_id, resource_id, status, claim_version
	)
    SELECT sqlc.arg(worker_host_id), pool.worker_group_id, pool.id,
	       sqlc.arg(resource_id), 'registering', 1
      FROM pool
    ON CONFLICT (worker_group_id, resource_id)
        WHERE status IN ('registering', 'active', 'draining')
    DO UPDATE
	   SET claim_version = worker_hosts.claim_version + 1,
	       status = 'registering',
	       vm_platform_id = NULL,
           epoch_cpu_millis = 0, epoch_memory_bytes = 0,
           epoch_guest_ephemeral_disk_bytes = 0,
           per_vm_cpu_millis = 0, per_vm_memory_bytes = 0,
           per_vm_guest_ephemeral_disk_bytes = 0,
	       max_vm_slots = 0, max_vm_starts = 0,
           cpu_environment = NULL, cpu_environment_digest = NULL,
           current_service_id = CASE
               WHEN worker_hosts.current_epoch IS NULL THEN NULL
               ELSE sqlc.arg(current_service_id)::uuid
           END,
           epoch_started_at = CASE WHEN worker_hosts.current_epoch IS NULL THEN NULL ELSE now() END,
           activated_at = NULL, draining_at = NULL, drain_reason = NULL,
	       observed_at = NULL,
	       admission_paused_reason = NULL,
           updated_at = now()
     WHERE worker_hosts.status = 'registering'
       AND worker_hosts.worker_pool_id = (SELECT id FROM pool)
    RETURNING *
), revoked AS (
    UPDATE worker_host_keys SET revoked_at = now()
      FROM worker WHERE worker_host_keys.worker_host_id = worker.id
                    AND worker_host_keys.revoked_at IS NULL
    RETURNING worker_host_keys.id
), host_key AS (
    INSERT INTO worker_host_keys (
	    id, worker_group_id, worker_host_id, public_key,
	    claim_version
	)
    SELECT sqlc.arg(host_key_id), worker.worker_group_id, worker.id,
	       sqlc.arg(public_key), worker.claim_version
      FROM worker WHERE (SELECT count(*) FROM revoked) >= 0
    RETURNING *
)
SELECT host_key.*, pool.id AS worker_pool_id
  FROM host_key JOIN pool ON pool.worker_group_id = host_key.worker_group_id;

-- name: GetActiveWorkerHostKey :one
-- The key a host answers credential challenges with. AuthenticateWorkerHostKey
-- then locks it with the host and rechecks it is current.
SELECT id, public_key
  FROM worker_host_keys
 WHERE worker_host_id = sqlc.arg(worker_host_id)
   AND revoked_at IS NULL;
