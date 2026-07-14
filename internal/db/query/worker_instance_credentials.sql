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
       AND worker_instance_credentials.protocol_version = sqlc.arg(protocol_version)
       AND worker_instance_credentials.protocol_version = worker_instances.protocol_version
       AND worker_instance_credentials.protocol_version = worker_groups.protocol_version
       AND worker_instances.state IN ('registering','active','draining')
       AND worker_groups.state IN ('active','draining')
     FOR UPDATE OF worker_instance_credentials, worker_instances, worker_groups
), advanced AS (
    UPDATE worker_instances
       SET current_epoch = CASE WHEN worker_instances.current_service_id = sqlc.arg(service_id)
                                THEN worker_instances.current_epoch
                                ELSE COALESCE(worker_instances.current_epoch, 0) + 1 END,
           current_service_id = sqlc.arg(service_id),
           epoch_started_at = CASE WHEN worker_instances.current_service_id = sqlc.arg(service_id)
                                   THEN worker_instances.epoch_started_at ELSE now() END,
           startup_inventory_epoch = CASE WHEN worker_instances.current_service_id = sqlc.arg(service_id)
                                          THEN worker_instances.startup_inventory_epoch ELSE NULL END,
           startup_inventory_evidence = CASE WHEN worker_instances.current_service_id = sqlc.arg(service_id)
                                             THEN worker_instances.startup_inventory_evidence ELSE NULL END,
           state = CASE
               WHEN worker_instances.current_service_id = sqlc.arg(service_id) THEN worker_instances.state
               WHEN worker_instances.state = 'active' THEN 'registering'
               ELSE worker_instances.state
           END,
           supports_run = credential.effective_allows_run,
           supports_build = credential.effective_allows_build,
           certified_at = CASE WHEN worker_instances.current_service_id = sqlc.arg(service_id)
                                      OR worker_instances.state = 'draining'
                               THEN worker_instances.certified_at ELSE NULL END,
           activated_at = CASE WHEN worker_instances.current_service_id = sqlc.arg(service_id)
                                      OR worker_instances.state = 'draining'
                               THEN worker_instances.activated_at ELSE NULL END,
           certification_profile = CASE WHEN worker_instances.current_service_id = sqlc.arg(service_id)
                                               OR worker_instances.state = 'draining'
                                        THEN worker_instances.certification_profile ELSE '' END,
           certification_fingerprint = CASE WHEN worker_instances.current_service_id = sqlc.arg(service_id)
                                                   OR worker_instances.state = 'draining'
                                            THEN worker_instances.certification_fingerprint ELSE '' END,
           updated_at = now()
      FROM credential
     WHERE worker_instances.id = credential.worker_instance_id
    RETURNING worker_instances.*
)
SELECT credential.id, credential.worker_group_id,
       credential.worker_instance_id, credential.key_prefix, credential.claim_version,
       credential.protocol_version, credential.group_claim_version,
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
   AND worker_instance_credentials.protocol_version = sqlc.arg(protocol_version)
   AND worker_instance_credentials.protocol_version = worker_instances.protocol_version
   AND worker_instance_credentials.protocol_version = worker_groups.protocol_version
   AND worker_instances.current_epoch = sqlc.arg(worker_epoch)
   AND worker_instances.state IN ('active','draining')
   AND worker_groups.state IN ('active','draining')
RETURNING worker_instance_credentials.*, worker_instances.resource_id,
          worker_instances.current_epoch, worker_instances.state AS worker_state,
          worker_instances.supports_run, worker_instances.supports_build,
          worker_instances.epoch_started_at;

-- name: AuthorizeTerminalWorkerInstanceCredential :one
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
   AND worker_instance_credentials.protocol_version = sqlc.arg(protocol_version)
   AND worker_instance_credentials.protocol_version = worker_instances.protocol_version
   AND worker_instance_credentials.protocol_version = worker_groups.protocol_version
   AND worker_instances.current_epoch = sqlc.arg(worker_epoch)
   AND worker_instances.state IN ('active','draining','disabled')
   AND worker_groups.state IN ('active','draining')
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
   AND worker_instance_credentials.protocol_version = sqlc.arg(protocol_version)
   AND worker_instance_credentials.protocol_version = worker_instances.protocol_version
   AND worker_instance_credentials.protocol_version = worker_groups.protocol_version
   AND worker_instances.current_epoch = sqlc.arg(worker_epoch)
   AND worker_instances.state = 'registering'
   AND worker_groups.state IN ('active','draining')
RETURNING worker_instance_credentials.*, worker_instances.resource_id,
          worker_instances.current_epoch, worker_instances.state AS worker_state,
          worker_instances.supports_run, worker_instances.supports_build,
          worker_instances.epoch_started_at;

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
   AND worker_groups.state = 'active'
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
   AND worker_groups.state = 'active';

-- name: EnrollWorkerInstance :one
WITH nonce AS (
    SELECT worker_enrollment_nonces.*, worker_groups.allows_run,
           worker_groups.allows_build, worker_groups.protocol_version
      FROM worker_enrollment_nonces
      JOIN worker_groups ON worker_groups.id = worker_enrollment_nonces.worker_group_id
     WHERE worker_enrollment_nonces.nonce_hash = sqlc.arg(nonce_hash)
       AND worker_enrollment_nonces.worker_group_id = sqlc.arg(worker_group_id)
       AND worker_enrollment_nonces.consumed_at IS NULL
       AND worker_enrollment_nonces.expires_at > now()
       AND worker_groups.state = 'active'
       AND worker_groups.allows_run = sqlc.arg(allows_run)
       AND worker_groups.allows_build = sqlc.arg(allows_build)
       AND worker_groups.protocol_version = sqlc.arg(protocol_version)
       AND worker_groups.enrollment_policy_fingerprint = sqlc.arg(enrollment_policy_fingerprint)
       AND sqlc.arg(attestation_fingerprint)::text = ANY(worker_groups.allowed_attestation_fingerprints)
     FOR UPDATE OF worker_enrollment_nonces, worker_groups
), worker AS (
    INSERT INTO worker_instances (
        id, worker_group_id, resource_id, state, claim_version,
        protocol_version, supports_run, supports_build, attestation_fingerprint
    )
    SELECT sqlc.arg(worker_instance_id), nonce.worker_group_id,
           sqlc.arg(resource_id), 'registering', 1, nonce.protocol_version,
           nonce.allows_run, nonce.allows_build, sqlc.arg(attestation_fingerprint)
      FROM nonce
    ON CONFLICT (worker_group_id, resource_id) DO UPDATE
       SET claim_version = worker_instances.claim_version + 1,
           state = 'registering', protocol_version = EXCLUDED.protocol_version,
           supports_run = EXCLUDED.supports_run, supports_build = EXCLUDED.supports_build,
           attestation_fingerprint = EXCLUDED.attestation_fingerprint,
           current_service_id = CASE WHEN worker_instances.current_epoch IS NULL THEN NULL ELSE uuidv7() END,
           epoch_started_at = CASE WHEN worker_instances.current_epoch IS NULL THEN NULL ELSE now() END,
           startup_inventory_epoch = NULL, startup_inventory_evidence = NULL,
           drain_cleanup_fingerprint = NULL, drain_cleanup_evidence = NULL,
           certified_at = NULL, activated_at = NULL,
           certification_profile = '', certification_fingerprint = '',
           draining_at = NULL, disabled_at = NULL, lost_at = NULL, updated_at = now()
     WHERE worker_instances.termination_claimed_at IS NULL
    RETURNING *
), revoked AS (
    UPDATE worker_instance_credentials SET revoked_at = now()
      FROM worker WHERE worker_instance_credentials.worker_instance_id = worker.id
                    AND worker_instance_credentials.revoked_at IS NULL
    RETURNING worker_instance_credentials.id
), credential AS (
    INSERT INTO worker_instance_credentials (
        id, worker_group_id, worker_instance_id, key_prefix, secret_hash,
        claim_version, allows_run, allows_build, protocol_version, expires_at
    )
    SELECT sqlc.arg(credential_id), worker.worker_group_id, worker.id,
           sqlc.arg(key_prefix), sqlc.arg(secret_hash), worker.claim_version,
           worker.supports_run, worker.supports_build, worker.protocol_version,
           sqlc.narg(credential_expires_at)
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
