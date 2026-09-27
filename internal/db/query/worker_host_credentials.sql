-- name: AuthenticateWorkerHostCredential :one
WITH credential AS (
    SELECT worker_host_credentials.*,
           worker_groups.claim_version AS group_claim_version
      FROM worker_host_credentials
      JOIN worker_hosts ON worker_hosts.id = worker_host_credentials.worker_host_id
                           AND worker_hosts.worker_group_id = worker_host_credentials.worker_group_id
      JOIN worker_groups ON worker_groups.id = worker_host_credentials.worker_group_id
      JOIN worker_pools ON worker_pools.id = worker_hosts.worker_pool_id
                       AND worker_pools.worker_group_id = worker_hosts.worker_group_id
     WHERE worker_host_credentials.worker_host_id = sqlc.arg(worker_host_id)
       AND worker_host_credentials.secret_hash = sqlc.arg(secret_hash)
       AND worker_host_credentials.revoked_at IS NULL
       AND (worker_host_credentials.expires_at IS NULL OR worker_host_credentials.expires_at > now())
       AND worker_host_credentials.claim_version = worker_hosts.claim_version
       AND worker_hosts.status IN ('registering','active','draining')
       AND worker_groups.status IN ('active','paused','draining')
       AND worker_pools.status IN ('pending','active','draining')
     FOR UPDATE OF worker_host_credentials, worker_hosts, worker_groups, worker_pools
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
           observed_at = CASE WHEN worker_hosts.current_service_id = sqlc.arg(service_id)
                              THEN worker_hosts.observed_at ELSE NULL END,
	       run_paused_reason = CASE WHEN worker_hosts.current_service_id = sqlc.arg(service_id)
	                                THEN worker_hosts.run_paused_reason ELSE NULL END,
           vm_paused_reason = CASE WHEN worker_hosts.current_service_id = sqlc.arg(service_id)
                                        THEN worker_hosts.vm_paused_reason ELSE NULL END,
           updated_at = now()
      FROM credential
     WHERE worker_hosts.id = credential.worker_host_id
    RETURNING worker_hosts.*
)
SELECT credential.id, credential.worker_group_id,
       credential.worker_host_id, credential.key_prefix, credential.claim_version,
       credential.group_claim_version,
       advanced.current_epoch, advanced.current_service_id, advanced.status,
       advanced.resource_id
  FROM credential JOIN advanced ON advanced.id = credential.worker_host_id;

-- name: AuthorizeWorkerHostCredential :one
UPDATE worker_host_credentials
   SET last_used_at = now()
  FROM worker_hosts, worker_groups, worker_pools
 WHERE worker_host_credentials.id = sqlc.arg(credential_id)
   AND worker_hosts.id = worker_host_credentials.worker_host_id
   AND worker_hosts.worker_group_id = worker_host_credentials.worker_group_id
   AND worker_groups.id = worker_host_credentials.worker_group_id
   AND worker_pools.id = worker_hosts.worker_pool_id
   AND worker_pools.worker_group_id = worker_hosts.worker_group_id
   AND worker_host_credentials.revoked_at IS NULL
   AND worker_host_credentials.claim_version = sqlc.arg(claim_version)
   AND worker_host_credentials.claim_version = worker_hosts.claim_version
	AND worker_groups.claim_version = sqlc.arg(group_claim_version)
	AND worker_hosts.current_epoch = sqlc.arg(worker_epoch)
	AND worker_hosts.status IN ('active','draining')
   AND worker_groups.status IN ('active','paused','draining')
   AND worker_pools.status IN ('active','draining')
RETURNING worker_host_credentials.*, worker_hosts.resource_id,
          worker_hosts.current_epoch, worker_hosts.status AS worker_status,
          worker_hosts.epoch_started_at;

-- name: AuthorizeWorkerActivationCredential :one
UPDATE worker_host_credentials
   SET last_used_at = now()
  FROM worker_hosts, worker_groups, worker_pools
 WHERE worker_host_credentials.id = sqlc.arg(credential_id)
   AND worker_hosts.id = worker_host_credentials.worker_host_id
   AND worker_hosts.worker_group_id = worker_host_credentials.worker_group_id
   AND worker_groups.id = worker_host_credentials.worker_group_id
   AND worker_pools.id = worker_hosts.worker_pool_id
   AND worker_pools.worker_group_id = worker_hosts.worker_group_id
   AND worker_host_credentials.revoked_at IS NULL
   AND worker_host_credentials.claim_version = sqlc.arg(claim_version)
   AND worker_host_credentials.claim_version = worker_hosts.claim_version
   AND worker_groups.claim_version = sqlc.arg(group_claim_version)
   AND worker_hosts.current_epoch = sqlc.arg(worker_epoch)
   AND worker_hosts.status IN ('registering', 'active', 'draining')
   AND worker_groups.status IN ('active','paused','draining')
   AND worker_pools.status IN ('pending','active','draining')
RETURNING worker_host_credentials.*, worker_hosts.resource_id,
          worker_hosts.current_epoch, worker_hosts.status AS worker_status,
          worker_hosts.epoch_started_at;

-- name: AuthorizeRecoveringWorkerHostCredential :one
UPDATE worker_host_credentials
   SET last_used_at = now()
  FROM worker_hosts, worker_groups, worker_pools
 WHERE worker_host_credentials.id = sqlc.arg(credential_id)
   AND worker_hosts.id = worker_host_credentials.worker_host_id
   AND worker_hosts.worker_group_id = worker_host_credentials.worker_group_id
   AND worker_groups.id = worker_host_credentials.worker_group_id
   AND worker_pools.id = worker_hosts.worker_pool_id
   AND worker_pools.worker_group_id = worker_hosts.worker_group_id
   AND worker_host_credentials.revoked_at IS NULL
   AND worker_host_credentials.claim_version = sqlc.arg(claim_version)
   AND worker_host_credentials.claim_version = worker_hosts.claim_version
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
RETURNING worker_host_credentials.*, worker_hosts.resource_id,
          worker_hosts.current_epoch, worker_hosts.status AS worker_status,
          worker_hosts.epoch_started_at;

-- name: AuthorizeWorkerDrainReplay :one
SELECT worker_host_credentials.*, worker_hosts.resource_id,
       worker_hosts.current_epoch, worker_hosts.status AS worker_status,
       worker_hosts.epoch_started_at
  FROM worker_host_credentials
  JOIN worker_hosts
    ON worker_hosts.id = worker_host_credentials.worker_host_id
   AND worker_hosts.worker_group_id = worker_host_credentials.worker_group_id
  JOIN worker_groups ON worker_groups.id = worker_host_credentials.worker_group_id
 WHERE worker_host_credentials.id = sqlc.arg(credential_id)
   AND worker_host_credentials.claim_version = sqlc.arg(claim_version)
   AND worker_host_credentials.revoked_at IS NOT NULL
   AND worker_hosts.current_epoch = sqlc.arg(worker_epoch)
   AND worker_hosts.status = 'termination_ready'
   AND worker_hosts.claim_version = worker_host_credentials.claim_version + 1;

-- name: AuthorizeWorkerFenceReplay :one
SELECT worker_host_credentials.*, worker_hosts.resource_id,
       worker_hosts.current_epoch, worker_hosts.status AS worker_status,
       worker_hosts.epoch_started_at
  FROM worker_host_credentials
  JOIN worker_hosts
    ON worker_hosts.id = worker_host_credentials.worker_host_id
   AND worker_hosts.worker_group_id = worker_host_credentials.worker_group_id
 WHERE worker_host_credentials.id = sqlc.arg(credential_id)
   AND worker_host_credentials.claim_version = sqlc.arg(claim_version)
   AND worker_host_credentials.revoked_at IS NOT NULL
   AND worker_hosts.current_epoch = sqlc.arg(worker_epoch)
   AND worker_hosts.status = 'lost'
   AND worker_hosts.claim_version = worker_host_credentials.claim_version + 1;

-- name: EnrollWorkerHost :one
WITH enrollment_token AS (
    SELECT worker_group_tokens.id AS token_id,
	       worker_groups.id AS worker_group_id
      FROM worker_group_tokens
      JOIN worker_groups ON worker_groups.token_id = worker_group_tokens.id
     WHERE worker_group_tokens.token_hash = sqlc.arg(token_hash)
	AND worker_groups.status IN ('active', 'paused')
     FOR UPDATE OF worker_group_tokens, worker_groups
), pool AS (
    INSERT INTO worker_pools (id, worker_group_id, name, status, claim_version)
    SELECT sqlc.arg(worker_pool_id), enrollment_token.worker_group_id,
	       sqlc.arg(pool_name), 'pending', 1
      FROM enrollment_token
    ON CONFLICT (worker_group_id, name)
    DO UPDATE SET updated_at = worker_pools.updated_at
	 WHERE worker_pools.status IN ('pending', 'active')
    RETURNING worker_pools.*
), worker AS (
    INSERT INTO worker_hosts (
	    id, worker_group_id, worker_pool_id, resource_id, status, claim_version
	)
    SELECT sqlc.arg(worker_host_id), enrollment_token.worker_group_id, pool.id,
	       sqlc.arg(resource_id), 'registering', 1
      FROM enrollment_token JOIN pool ON pool.worker_group_id = enrollment_token.worker_group_id
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
           activated_at = NULL, draining_at = NULL,
	       observed_at = NULL,
	       run_paused_reason = NULL,
	       vm_paused_reason = NULL,
           updated_at = now()
     WHERE worker_hosts.status = 'registering'
       AND worker_hosts.worker_pool_id = (SELECT id FROM pool)
    RETURNING *
), revoked AS (
    UPDATE worker_host_credentials SET revoked_at = now()
      FROM worker WHERE worker_host_credentials.worker_host_id = worker.id
                    AND worker_host_credentials.revoked_at IS NULL
    RETURNING worker_host_credentials.id
), credential AS (
    INSERT INTO worker_host_credentials (
	    id, worker_group_id, worker_host_id, key_prefix, secret_hash,
	    claim_version, expires_at
	)
    SELECT sqlc.arg(credential_id), worker.worker_group_id, worker.id,
	       sqlc.arg(key_prefix), sqlc.arg(secret_hash), worker.claim_version,
	       sqlc.narg(credential_expires_at)
      FROM worker WHERE (SELECT count(*) FROM revoked) >= 0
    RETURNING *
), touched AS (
    UPDATE worker_group_tokens
       SET last_used_at = now()
      FROM credential
     WHERE worker_group_tokens.id = (SELECT token_id FROM enrollment_token)
    RETURNING worker_group_tokens.id
)
SELECT credential.*, pool.id AS worker_pool_id
  FROM credential JOIN touched ON true JOIN pool ON pool.worker_group_id = credential.worker_group_id;
