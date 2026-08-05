-- name: AuthenticateWorkerInstanceCredential :one
WITH credential AS (
    SELECT worker_instance_credentials.*,
           worker_groups.claim_version AS group_claim_version,
           worker_groups.allows_run AS group_allows_run,
           worker_groups.allows_build AS group_allows_build,
           (worker_instance_credentials.allows_run AND worker_groups.allows_run AND sqlc.arg(supports_run)::boolean) AS effective_allows_run,
           (worker_instance_credentials.allows_build AND worker_groups.allows_build AND sqlc.arg(supports_build)::boolean) AS effective_allows_build
      FROM worker_instance_credentials
      JOIN worker_instances ON worker_instances.id = worker_instance_credentials.worker_instance_id
                           AND worker_instances.worker_group_id = worker_instance_credentials.worker_group_id
      JOIN worker_groups ON worker_groups.id = worker_instance_credentials.worker_group_id
     WHERE worker_instance_credentials.worker_instance_id = sqlc.arg(worker_instance_id)
       AND worker_instance_credentials.secret_hash = sqlc.arg(secret_hash)
       AND worker_instance_credentials.revoked_at IS NULL
       AND (worker_instance_credentials.expires_at IS NULL OR worker_instance_credentials.expires_at > now())
       AND worker_instance_credentials.claim_version = worker_instances.claim_version
       AND worker_instances.state IN ('registering','active','draining')
       AND worker_groups.state IN ('active','paused','draining')
     FOR UPDATE OF worker_instance_credentials, worker_instances, worker_groups
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
           supervisor_version = CASE
               WHEN worker_instances.current_service_id = sqlc.arg(service_id)
               THEN worker_instances.supervisor_version
               ELSE ''
           END,
           supports_run = CASE
               WHEN worker_instances.current_service_id = sqlc.arg(service_id)
               THEN worker_instances.supports_run
               ELSE false
           END,
           supports_build = CASE
               WHEN worker_instances.current_service_id = sqlc.arg(service_id)
               THEN worker_instances.supports_build
               ELSE false
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
           epoch_build_cache_bytes = CASE
               WHEN worker_instances.current_service_id = sqlc.arg(service_id)
               THEN worker_instances.epoch_build_cache_bytes ELSE 0
           END,
           epoch_artifact_cache_bytes = CASE
               WHEN worker_instances.current_service_id = sqlc.arg(service_id)
               THEN worker_instances.epoch_artifact_cache_bytes ELSE 0
           END,
           epoch_hugepages_bytes = CASE
               WHEN worker_instances.current_service_id = sqlc.arg(service_id)
               THEN worker_instances.epoch_hugepages_bytes ELSE 0
           END,
           epoch_checkpoint_bytes = CASE
               WHEN worker_instances.current_service_id = sqlc.arg(service_id)
               THEN worker_instances.epoch_checkpoint_bytes ELSE 0
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
           max_run_consumers = CASE
               WHEN worker_instances.current_service_id = sqlc.arg(service_id)
               THEN worker_instances.max_run_consumers ELSE 0
           END,
           max_build_executors = CASE
               WHEN worker_instances.current_service_id = sqlc.arg(service_id)
               THEN worker_instances.max_build_executors ELSE 0
           END,
           max_runtime_starts = CASE
               WHEN worker_instances.current_service_id = sqlc.arg(service_id)
               THEN worker_instances.max_runtime_starts ELSE 0
           END,
           activated_at = CASE WHEN worker_instances.current_service_id = sqlc.arg(service_id)
                               THEN worker_instances.activated_at ELSE NULL END,
           updated_at = now()
      FROM credential
     WHERE worker_instances.id = credential.worker_instance_id
    RETURNING worker_instances.*
)
SELECT credential.id, credential.worker_group_id,
       credential.worker_instance_id, credential.key_prefix, credential.claim_version,
       credential.group_claim_version,
       credential.allows_run AS credential_allows_run,
       credential.allows_build AS credential_allows_build,
       credential.group_allows_run, credential.group_allows_build,
       credential.effective_allows_run, credential.effective_allows_build,
       advanced.current_epoch, advanced.current_service_id, advanced.state,
       advanced.resource_id
  FROM credential JOIN advanced ON advanced.id = credential.worker_instance_id;

-- name: AuthorizeWorkerInstanceCredential :one
UPDATE worker_instance_credentials
   SET last_used_at = now()
  FROM worker_instances, worker_groups
 WHERE worker_instance_credentials.id = sqlc.arg(credential_id)
   AND worker_instances.id = worker_instance_credentials.worker_instance_id
   AND worker_instances.worker_group_id = worker_instance_credentials.worker_group_id
   AND worker_groups.id = worker_instance_credentials.worker_group_id
   AND worker_instance_credentials.revoked_at IS NULL
   AND worker_instance_credentials.claim_version = sqlc.arg(claim_version)
   AND worker_instance_credentials.claim_version = worker_instances.claim_version
   AND worker_groups.claim_version = sqlc.arg(group_claim_version)
   AND worker_instances.current_epoch = sqlc.arg(worker_epoch)
   AND worker_instances.state IN ('active','draining')
   AND (worker_instances.supports_run OR worker_instances.supports_build)
   AND worker_groups.state IN ('active','paused','draining')
RETURNING worker_instance_credentials.*, worker_instances.resource_id,
          worker_instances.current_epoch, worker_instances.state AS worker_state,
          worker_instances.supports_run, worker_instances.supports_build,
          worker_instances.epoch_started_at;

-- name: AuthorizeRegisteringWorkerInstanceCredential :one
UPDATE worker_instance_credentials
   SET last_used_at = now()
  FROM worker_instances, worker_groups
 WHERE worker_instance_credentials.id = sqlc.arg(credential_id)
   AND worker_instances.id = worker_instance_credentials.worker_instance_id
   AND worker_instances.worker_group_id = worker_instance_credentials.worker_group_id
   AND worker_groups.id = worker_instance_credentials.worker_group_id
   AND worker_instance_credentials.revoked_at IS NULL
   AND worker_instance_credentials.claim_version = sqlc.arg(claim_version)
   AND worker_instance_credentials.claim_version = worker_instances.claim_version
   AND worker_groups.claim_version = sqlc.arg(group_claim_version)
   AND worker_instances.current_epoch = sqlc.arg(worker_epoch)
   AND (
       worker_instances.state = 'registering'
       OR (
           worker_instances.state = 'active'
           AND EXISTS (
               SELECT 1
                 FROM worker_observations
                WHERE worker_observations.worker_instance_id = worker_instances.id
                  AND worker_observations.worker_epoch = worker_instances.current_epoch
                  AND (
                      worker_observations.run_paused_reason = 'datapath_unverified'
                      OR worker_observations.build_paused_reason = 'datapath_unverified'
                      OR worker_observations.runtime_paused_reason = 'datapath_unverified'
                  )
           )
       )
   )
   AND worker_groups.state IN ('active','paused','draining')
RETURNING worker_instance_credentials.*, worker_instances.resource_id,
          worker_instances.current_epoch, worker_instances.state AS worker_state,
          worker_instances.supports_run, worker_instances.supports_build,
          worker_instances.epoch_started_at;

-- name: AuthorizeRecoveringWorkerInstanceCredential :one
UPDATE worker_instance_credentials
   SET last_used_at = now()
  FROM worker_instances, worker_groups
 WHERE worker_instance_credentials.id = sqlc.arg(credential_id)
   AND worker_instances.id = worker_instance_credentials.worker_instance_id
   AND worker_instances.worker_group_id = worker_instance_credentials.worker_group_id
   AND worker_groups.id = worker_instance_credentials.worker_group_id
   AND worker_instance_credentials.revoked_at IS NULL
   AND worker_instance_credentials.claim_version = sqlc.arg(claim_version)
   AND worker_instance_credentials.claim_version = worker_instances.claim_version
   AND worker_groups.claim_version = sqlc.arg(group_claim_version)
   AND worker_instances.current_epoch = sqlc.arg(worker_epoch)
   AND (
       worker_instances.state = 'registering'
       OR (
           worker_instances.state = 'draining'
           AND NOT worker_instances.supports_run
           AND NOT worker_instances.supports_build
       )
   )
   AND worker_groups.state IN ('active','paused','draining')
RETURNING worker_instance_credentials.*, worker_instances.resource_id,
          worker_instances.current_epoch, worker_instances.state AS worker_state,
          worker_instances.supports_run, worker_instances.supports_build,
          worker_instances.epoch_started_at;

-- name: AuthorizeWorkerDrainReplay :one
SELECT worker_instance_credentials.*, worker_instances.resource_id,
       worker_instances.current_epoch, worker_instances.state AS worker_state,
       worker_instances.supports_run, worker_instances.supports_build,
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
       worker_instances.supports_run, worker_instances.supports_build,
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

-- name: CreateWorkerEnrollmentNonce :one
WITH pruned AS (
    DELETE FROM worker_enrollment_nonces
     WHERE expires_at <= now() AND created_at < now() - interval '10 minutes'
    RETURNING id
)
INSERT INTO worker_enrollment_nonces (id, nonce_hash, worker_group_id, expires_at)
SELECT sqlc.arg(id), sqlc.arg(nonce_hash), worker_groups.id, sqlc.arg(expires_at)
  FROM worker_groups
 WHERE worker_groups.id = sqlc.arg(worker_group_id)
   AND worker_groups.state IN ('active','paused')
   AND (SELECT count(*) FROM pruned) >= 0
RETURNING *;

-- name: GetActiveWorkerEnrollmentNonce :one
SELECT worker_enrollment_nonces.*
  FROM worker_enrollment_nonces
  JOIN worker_groups ON worker_groups.id = worker_enrollment_nonces.worker_group_id
 WHERE worker_enrollment_nonces.nonce_hash = sqlc.arg(nonce_hash)
   AND worker_enrollment_nonces.worker_group_id = sqlc.arg(worker_group_id)
   AND worker_enrollment_nonces.consumed_at IS NULL
   AND worker_enrollment_nonces.expires_at > now()
   AND worker_groups.state IN ('active','paused');

-- name: EnrollWorkerInstance :one
WITH nonce AS (
    SELECT worker_enrollment_nonces.*, worker_groups.allows_run,
           worker_groups.allows_build
      FROM worker_enrollment_nonces
      JOIN worker_groups ON worker_groups.id = worker_enrollment_nonces.worker_group_id
     WHERE worker_enrollment_nonces.nonce_hash = sqlc.arg(nonce_hash)
       AND worker_enrollment_nonces.worker_group_id = sqlc.arg(worker_group_id)
       AND worker_enrollment_nonces.consumed_at IS NULL
       AND worker_enrollment_nonces.expires_at > now()
       AND worker_groups.state IN ('active','paused')
       AND (NOT sqlc.arg(allows_run)::boolean OR worker_groups.allows_run)
       AND (NOT sqlc.arg(allows_build)::boolean OR worker_groups.allows_build)
     FOR UPDATE OF worker_enrollment_nonces, worker_groups
), worker AS (
    INSERT INTO worker_instances (
        id, worker_group_id, resource_id, state, claim_version,
        supports_run, supports_build
    )
    SELECT sqlc.arg(worker_instance_id), nonce.worker_group_id,
           sqlc.arg(resource_id), 'registering', 1, false, false
      FROM nonce
    ON CONFLICT (worker_group_id, resource_id)
        WHERE state IN ('registering', 'active', 'draining')
    DO UPDATE
       SET claim_version = worker_instances.claim_version + 1,
           state = 'registering',
           supervisor_version = '',
           supports_run = false, supports_build = false,
           runtime_identity_id = NULL,
           substrate_format = '', substrate_contract = '',
           epoch_cpu_millis = 0, epoch_memory_bytes = 0,
           epoch_guest_ephemeral_disk_bytes = 0,
           epoch_build_cache_bytes = 0, epoch_artifact_cache_bytes = 0,
           epoch_hugepages_bytes = 0, epoch_checkpoint_bytes = 0,
           per_vm_cpu_millis = 0, per_vm_memory_bytes = 0,
           per_vm_guest_ephemeral_disk_bytes = 0,
           max_vm_slots = 0, max_run_consumers = 0,
           max_build_executors = 0, max_runtime_starts = 0,
           current_service_id = CASE
               WHEN worker_instances.current_epoch IS NULL THEN NULL
               ELSE sqlc.arg(current_service_id)::uuid
           END,
           epoch_started_at = CASE WHEN worker_instances.current_epoch IS NULL THEN NULL ELSE now() END,
           activated_at = NULL, draining_at = NULL, updated_at = now()
     WHERE worker_instances.state = 'registering'
    RETURNING *
), revoked AS (
    UPDATE worker_instance_credentials SET revoked_at = now()
      FROM worker WHERE worker_instance_credentials.worker_instance_id = worker.id
                    AND worker_instance_credentials.revoked_at IS NULL
    RETURNING worker_instance_credentials.id
), credential AS (
    INSERT INTO worker_instance_credentials (
        id, worker_group_id, worker_instance_id, key_prefix, secret_hash,
        claim_version, allows_run, allows_build, expires_at
    )
    SELECT sqlc.arg(credential_id), worker.worker_group_id, worker.id,
           sqlc.arg(key_prefix), sqlc.arg(secret_hash), worker.claim_version,
           sqlc.arg(allows_run), sqlc.arg(allows_build), sqlc.narg(credential_expires_at)
      FROM worker WHERE (SELECT count(*) FROM revoked) >= 0
    RETURNING *
), consumed AS (
    UPDATE worker_enrollment_nonces
       SET consumed_at = now(), consumed_by_worker_instance_id = credential.worker_instance_id
      FROM credential
     WHERE worker_enrollment_nonces.id = (SELECT id FROM nonce)
    RETURNING worker_enrollment_nonces.id
)
SELECT credential.* FROM credential JOIN consumed ON true;
