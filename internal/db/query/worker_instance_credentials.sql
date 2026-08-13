-- name: AuthenticateWorkerInstanceCredential :one
WITH credential AS (
    SELECT worker_instance_credentials.*,
           worker_groups.claim_version AS group_claim_version
      FROM worker_instance_credentials
      JOIN worker_instances ON worker_instances.id = worker_instance_credentials.worker_instance_id
                           AND worker_instances.worker_group_id = worker_instance_credentials.worker_group_id
      JOIN worker_groups ON worker_groups.id = worker_instance_credentials.worker_group_id
      JOIN worker_pools ON worker_pools.id = worker_instances.worker_pool_id
                       AND worker_pools.worker_group_id = worker_instances.worker_group_id
     WHERE worker_instance_credentials.worker_instance_id = sqlc.arg(worker_instance_id)
       AND worker_instance_credentials.secret_hash = sqlc.arg(secret_hash)
       AND worker_instance_credentials.revoked_at IS NULL
       AND (worker_instance_credentials.expires_at IS NULL OR worker_instance_credentials.expires_at > now())
       AND worker_instance_credentials.claim_version = worker_instances.claim_version
       AND worker_instances.state IN ('registering','active','draining')
       AND worker_groups.state IN ('active','paused','draining')
       AND worker_pools.state IN ('pending','active','draining')
     FOR UPDATE OF worker_instance_credentials, worker_instances, worker_groups, worker_pools
), advanced AS (
    UPDATE worker_instances
       SET current_epoch = CASE WHEN worker_instances.current_service_id = sqlc.arg(service_id)
                                THEN worker_instances.current_epoch
                                ELSE COALESCE(worker_instances.current_epoch, 0) + 1 END,
           current_service_id = sqlc.arg(service_id),
           epoch_started_at = CASE WHEN worker_instances.current_service_id = sqlc.arg(service_id)
                                   THEN worker_instances.epoch_started_at ELSE now() END,
           state = CASE
               WHEN worker_instances.current_service_id = sqlc.arg(service_id) THEN worker_instances.state
               WHEN worker_instances.state = 'active' THEN 'registering'
               ELSE worker_instances.state
           END,
	       runtime_identity_id = CASE
               WHEN worker_instances.current_service_id = sqlc.arg(service_id)
               THEN worker_instances.runtime_identity_id
               ELSE NULL
           END,
           substrate_format = CASE
               WHEN worker_instances.current_service_id = sqlc.arg(service_id)
               THEN worker_instances.substrate_format
               ELSE ''
           END,
           substrate_contract = CASE
               WHEN worker_instances.current_service_id = sqlc.arg(service_id)
               THEN worker_instances.substrate_contract
               ELSE ''
           END,
           epoch_cpu_millis = CASE
               WHEN worker_instances.current_service_id = sqlc.arg(service_id)
               THEN worker_instances.epoch_cpu_millis ELSE 0
           END,
           epoch_memory_bytes = CASE
               WHEN worker_instances.current_service_id = sqlc.arg(service_id)
               THEN worker_instances.epoch_memory_bytes ELSE 0
           END,
           epoch_guest_ephemeral_disk_bytes = CASE
               WHEN worker_instances.current_service_id = sqlc.arg(service_id)
               THEN worker_instances.epoch_guest_ephemeral_disk_bytes ELSE 0
           END,
           per_vm_cpu_millis = CASE
               WHEN worker_instances.current_service_id = sqlc.arg(service_id)
               THEN worker_instances.per_vm_cpu_millis ELSE 0
           END,
           per_vm_memory_bytes = CASE
               WHEN worker_instances.current_service_id = sqlc.arg(service_id)
               THEN worker_instances.per_vm_memory_bytes ELSE 0
           END,
           per_vm_guest_ephemeral_disk_bytes = CASE
               WHEN worker_instances.current_service_id = sqlc.arg(service_id)
               THEN worker_instances.per_vm_guest_ephemeral_disk_bytes ELSE 0
           END,
	       max_vm_slots = CASE
	           WHEN worker_instances.current_service_id = sqlc.arg(service_id)
	           THEN worker_instances.max_vm_slots ELSE 0
	       END,
           max_runtime_starts = CASE
               WHEN worker_instances.current_service_id = sqlc.arg(service_id)
               THEN worker_instances.max_runtime_starts ELSE 0
           END,
           cpu_environment = CASE
               WHEN worker_instances.current_service_id = sqlc.arg(service_id)
               THEN worker_instances.cpu_environment ELSE NULL
           END,
           cpu_environment_digest = CASE
               WHEN worker_instances.current_service_id = sqlc.arg(service_id)
               THEN worker_instances.cpu_environment_digest ELSE NULL
           END,
           activated_at = CASE WHEN worker_instances.current_service_id = sqlc.arg(service_id)
                               THEN worker_instances.activated_at ELSE NULL END,
           observed_at = CASE WHEN worker_instances.current_service_id = sqlc.arg(service_id)
                              THEN worker_instances.observed_at ELSE NULL END,
	       run_paused_reason = CASE WHEN worker_instances.current_service_id = sqlc.arg(service_id)
	                                THEN worker_instances.run_paused_reason ELSE NULL END,
           runtime_paused_reason = CASE WHEN worker_instances.current_service_id = sqlc.arg(service_id)
                                        THEN worker_instances.runtime_paused_reason ELSE NULL END,
           updated_at = now()
      FROM credential
     WHERE worker_instances.id = credential.worker_instance_id
    RETURNING worker_instances.*
)
SELECT credential.id, credential.worker_group_id,
       credential.worker_instance_id, credential.key_prefix, credential.claim_version,
       credential.group_claim_version,
       advanced.current_epoch, advanced.current_service_id, advanced.state,
       advanced.resource_id
  FROM credential JOIN advanced ON advanced.id = credential.worker_instance_id;

-- name: AuthorizeWorkerInstanceCredential :one
UPDATE worker_instance_credentials
   SET last_used_at = now()
  FROM worker_instances, worker_groups, worker_pools
 WHERE worker_instance_credentials.id = sqlc.arg(credential_id)
   AND worker_instances.id = worker_instance_credentials.worker_instance_id
   AND worker_instances.worker_group_id = worker_instance_credentials.worker_group_id
   AND worker_groups.id = worker_instance_credentials.worker_group_id
   AND worker_pools.id = worker_instances.worker_pool_id
   AND worker_pools.worker_group_id = worker_instances.worker_group_id
   AND worker_instance_credentials.revoked_at IS NULL
   AND worker_instance_credentials.claim_version = sqlc.arg(claim_version)
   AND worker_instance_credentials.claim_version = worker_instances.claim_version
	AND worker_groups.claim_version = sqlc.arg(group_claim_version)
	AND worker_instances.current_epoch = sqlc.arg(worker_epoch)
	AND worker_instances.state IN ('active','draining')
   AND worker_groups.state IN ('active','paused','draining')
   AND worker_pools.state IN ('active','draining')
RETURNING worker_instance_credentials.*, worker_instances.resource_id,
          worker_instances.current_epoch, worker_instances.state AS worker_state,
          worker_instances.epoch_started_at;

-- name: AuthorizeWorkerActivationCredential :one
UPDATE worker_instance_credentials
   SET last_used_at = now()
  FROM worker_instances, worker_groups, worker_pools
 WHERE worker_instance_credentials.id = sqlc.arg(credential_id)
   AND worker_instances.id = worker_instance_credentials.worker_instance_id
   AND worker_instances.worker_group_id = worker_instance_credentials.worker_group_id
   AND worker_groups.id = worker_instance_credentials.worker_group_id
   AND worker_pools.id = worker_instances.worker_pool_id
   AND worker_pools.worker_group_id = worker_instances.worker_group_id
   AND worker_instance_credentials.revoked_at IS NULL
   AND worker_instance_credentials.claim_version = sqlc.arg(claim_version)
   AND worker_instance_credentials.claim_version = worker_instances.claim_version
   AND worker_groups.claim_version = sqlc.arg(group_claim_version)
   AND worker_instances.current_epoch = sqlc.arg(worker_epoch)
   AND worker_instances.state IN ('registering', 'active', 'draining')
   AND worker_groups.state IN ('active','paused','draining')
   AND worker_pools.state IN ('pending','active','draining')
RETURNING worker_instance_credentials.*, worker_instances.resource_id,
          worker_instances.current_epoch, worker_instances.state AS worker_state,
          worker_instances.epoch_started_at;

-- name: AuthorizeRecoveringWorkerInstanceCredential :one
UPDATE worker_instance_credentials
   SET last_used_at = now()
  FROM worker_instances, worker_groups, worker_pools
 WHERE worker_instance_credentials.id = sqlc.arg(credential_id)
   AND worker_instances.id = worker_instance_credentials.worker_instance_id
   AND worker_instances.worker_group_id = worker_instance_credentials.worker_group_id
   AND worker_groups.id = worker_instance_credentials.worker_group_id
   AND worker_pools.id = worker_instances.worker_pool_id
   AND worker_pools.worker_group_id = worker_instances.worker_group_id
   AND worker_instance_credentials.revoked_at IS NULL
   AND worker_instance_credentials.claim_version = sqlc.arg(claim_version)
   AND worker_instance_credentials.claim_version = worker_instances.claim_version
   AND worker_groups.claim_version = sqlc.arg(group_claim_version)
   AND worker_instances.current_epoch = sqlc.arg(worker_epoch)
	AND (
	    worker_instances.state = 'registering'
	    OR (
	        worker_instances.state = 'draining'
	        AND worker_instances.runtime_identity_id IS NULL
	    )
   )
   AND worker_groups.state IN ('active','paused','draining')
   AND worker_pools.state IN ('pending','active','draining')
RETURNING worker_instance_credentials.*, worker_instances.resource_id,
          worker_instances.current_epoch, worker_instances.state AS worker_state,
          worker_instances.epoch_started_at;

-- name: AuthorizeWorkerDrainReplay :one
SELECT worker_instance_credentials.*, worker_instances.resource_id,
       worker_instances.current_epoch, worker_instances.state AS worker_state,
       worker_instances.epoch_started_at
  FROM worker_instance_credentials
  JOIN worker_instances
    ON worker_instances.id = worker_instance_credentials.worker_instance_id
   AND worker_instances.worker_group_id = worker_instance_credentials.worker_group_id
  JOIN worker_groups ON worker_groups.id = worker_instance_credentials.worker_group_id
 WHERE worker_instance_credentials.id = sqlc.arg(credential_id)
   AND worker_instance_credentials.claim_version = sqlc.arg(claim_version)
   AND worker_instance_credentials.revoked_at IS NOT NULL
   AND worker_instances.current_epoch = sqlc.arg(worker_epoch)
   AND worker_instances.state = 'termination_ready'
   AND worker_instances.claim_version = worker_instance_credentials.claim_version + 1;

-- name: AuthorizeWorkerFenceReplay :one
SELECT worker_instance_credentials.*, worker_instances.resource_id,
       worker_instances.current_epoch, worker_instances.state AS worker_state,
       worker_instances.epoch_started_at
  FROM worker_instance_credentials
  JOIN worker_instances
    ON worker_instances.id = worker_instance_credentials.worker_instance_id
   AND worker_instances.worker_group_id = worker_instance_credentials.worker_group_id
 WHERE worker_instance_credentials.id = sqlc.arg(credential_id)
   AND worker_instance_credentials.claim_version = sqlc.arg(claim_version)
   AND worker_instance_credentials.revoked_at IS NOT NULL
   AND worker_instances.current_epoch = sqlc.arg(worker_epoch)
   AND worker_instances.state = 'lost'
   AND worker_instances.claim_version = worker_instance_credentials.claim_version + 1;

-- name: EnrollWorkerInstance :one
WITH enrollment_token AS (
    SELECT worker_group_tokens.id AS token_id,
	       worker_groups.id AS worker_group_id
      FROM worker_group_tokens
      JOIN worker_groups ON worker_groups.token_id = worker_group_tokens.id
     WHERE worker_group_tokens.token_hash = sqlc.arg(token_hash)
	AND worker_groups.state IN ('active', 'paused')
     FOR UPDATE OF worker_group_tokens, worker_groups
), pool AS (
    INSERT INTO worker_pools (id, worker_group_id, name, state, claim_version)
    SELECT sqlc.arg(worker_pool_id), enrollment_token.worker_group_id,
	       sqlc.arg(pool_name), 'pending', 1
      FROM enrollment_token
    ON CONFLICT (worker_group_id, name)
    DO UPDATE SET updated_at = worker_pools.updated_at
	 WHERE worker_pools.state IN ('pending', 'active')
    RETURNING worker_pools.*
), worker AS (
    INSERT INTO worker_instances (
	    id, worker_group_id, worker_pool_id, resource_id, state, claim_version
	)
    SELECT sqlc.arg(worker_instance_id), enrollment_token.worker_group_id, pool.id,
	       sqlc.arg(resource_id), 'registering', 1
      FROM enrollment_token JOIN pool ON pool.worker_group_id = enrollment_token.worker_group_id
    ON CONFLICT (worker_group_id, resource_id)
        WHERE state IN ('registering', 'active', 'draining')
    DO UPDATE
	   SET claim_version = worker_instances.claim_version + 1,
	       state = 'registering',
	       runtime_identity_id = NULL,
           substrate_format = '', substrate_contract = '',
           epoch_cpu_millis = 0, epoch_memory_bytes = 0,
           epoch_guest_ephemeral_disk_bytes = 0,
           per_vm_cpu_millis = 0, per_vm_memory_bytes = 0,
           per_vm_guest_ephemeral_disk_bytes = 0,
	       max_vm_slots = 0, max_runtime_starts = 0,
           cpu_environment = NULL, cpu_environment_digest = NULL,
           current_service_id = CASE
               WHEN worker_instances.current_epoch IS NULL THEN NULL
               ELSE sqlc.arg(current_service_id)::uuid
           END,
           epoch_started_at = CASE WHEN worker_instances.current_epoch IS NULL THEN NULL ELSE now() END,
           activated_at = NULL, draining_at = NULL,
	       observed_at = NULL,
	       run_paused_reason = NULL,
	       runtime_paused_reason = NULL,
           updated_at = now()
     WHERE worker_instances.state = 'registering'
       AND worker_instances.worker_pool_id = (SELECT id FROM pool)
    RETURNING *
), revoked AS (
    UPDATE worker_instance_credentials SET revoked_at = now()
      FROM worker WHERE worker_instance_credentials.worker_instance_id = worker.id
                    AND worker_instance_credentials.revoked_at IS NULL
    RETURNING worker_instance_credentials.id
), credential AS (
    INSERT INTO worker_instance_credentials (
	    id, worker_group_id, worker_instance_id, key_prefix, secret_hash,
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
