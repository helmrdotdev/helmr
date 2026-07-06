-- name: UpsertWorkerBootstrapToken :one
INSERT INTO worker_bootstrap_tokens (id, token_hash, worker_group_id)
VALUES (
    sqlc.arg(id),
    sqlc.arg(token_hash)::bytea,
    sqlc.arg(worker_group_id)
)
ON CONFLICT (token_hash) DO UPDATE
   SET revoked_at = worker_bootstrap_tokens.revoked_at
RETURNING *;

-- name: CreateWorkerInstanceCredentialFromBootstrap :one
WITH bootstrap_token AS (
    SELECT worker_bootstrap_tokens.id,
           worker_bootstrap_tokens.org_id,
           worker_bootstrap_tokens.worker_group_id,
           worker_groups.claim_version
     FROM worker_bootstrap_tokens
      JOIN worker_groups ON worker_groups.id = worker_bootstrap_tokens.worker_group_id
     WHERE worker_bootstrap_tokens.token_hash = sqlc.arg(bootstrap_token_hash)
       AND worker_bootstrap_tokens.worker_group_id = sqlc.arg(worker_group_id)
       AND worker_groups.state = 'active'
       AND worker_bootstrap_tokens.revoked_at IS NULL
       AND (worker_bootstrap_tokens.expires_at IS NULL OR worker_bootstrap_tokens.expires_at > now())
     FOR UPDATE
),
reserved_worker_instance AS (
    INSERT INTO worker_instances (
        id,
        org_id,
        worker_group_id,
        resource_id,
        status,
        claim_version,
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
    )
    SELECT sqlc.arg(worker_instance_id),
           bootstrap_token.org_id,
           bootstrap_token.worker_group_id,
           sqlc.arg(resource_id),
           'offline',
           bootstrap_token.claim_version,
           1,
           1,
           0,
           1,
           0,
           0,
           0,
           0,
           '{}'::jsonb,
           '{}'::jsonb,
           now()
      FROM bootstrap_token
    ON CONFLICT (worker_group_id, resource_id) DO UPDATE
       SET resource_id = EXCLUDED.resource_id,
           worker_group_id = EXCLUDED.worker_group_id,
           claim_version = EXCLUDED.claim_version
    RETURNING id AS worker_instance_id, org_id, worker_group_id, claim_version
),
revoked_existing_credentials AS (
    UPDATE worker_instance_credentials
       SET revoked_at = now()
      FROM reserved_worker_instance
     WHERE worker_instance_credentials.worker_instance_id = reserved_worker_instance.worker_instance_id
       AND worker_instance_credentials.revoked_at IS NULL
    RETURNING worker_instance_credentials.id
),
credential_rotation AS (
    SELECT count(*) FROM revoked_existing_credentials
),
bootstrap_token_update AS (
    UPDATE worker_bootstrap_tokens
       SET last_used_at = now(),
           last_used_by_worker_instance_id = (SELECT worker_instance_id FROM reserved_worker_instance)
     WHERE worker_bootstrap_tokens.token_hash = sqlc.arg(bootstrap_token_hash)
       AND worker_bootstrap_tokens.worker_group_id = sqlc.arg(worker_group_id)
       AND worker_bootstrap_tokens.revoked_at IS NULL
     RETURNING 1
)
INSERT INTO worker_instance_credentials (id, org_id, worker_group_id, worker_instance_id, key_prefix, claim_version, secret_hash)
SELECT sqlc.arg(credential_id),
       reserved_worker_instance.org_id,
       reserved_worker_instance.worker_group_id,
       reserved_worker_instance.worker_instance_id,
       sqlc.arg(key_prefix),
       reserved_worker_instance.claim_version,
       sqlc.arg(secret_hash)
  FROM bootstrap_token
 CROSS JOIN reserved_worker_instance
 CROSS JOIN bootstrap_token_update
 CROSS JOIN credential_rotation
RETURNING id, worker_group_id, worker_instance_id, key_prefix, claim_version, created_at;

-- name: AuthenticateWorkerInstanceCredential :one
WITH credential AS (
    SELECT worker_instance_credentials.id
      FROM worker_instance_credentials
      JOIN worker_instances ON worker_instances.id = worker_instance_credentials.worker_instance_id
      JOIN worker_groups ON worker_groups.id = worker_instances.worker_group_id
     WHERE worker_instance_credentials.worker_instance_id = sqlc.arg(worker_instance_id)
       AND worker_instance_credentials.secret_hash = sqlc.arg(secret_hash)
       AND worker_instance_credentials.worker_group_id = sqlc.arg(worker_group_id)
       AND worker_instance_credentials.worker_group_id = worker_instances.worker_group_id
       AND worker_instance_credentials.claim_version = worker_instances.claim_version
       AND worker_instance_credentials.claim_version = worker_groups.claim_version
       AND worker_groups.state = 'active'
       AND worker_instance_credentials.revoked_at IS NULL
)
UPDATE worker_instance_credentials
   SET last_used_at = now()
  FROM credential
 WHERE worker_instance_credentials.id = credential.id
RETURNING worker_instance_credentials.id,
          worker_instance_credentials.worker_group_id,
          worker_instance_credentials.worker_instance_id,
          worker_instance_credentials.claim_version;

-- name: AuthorizeWorkerInstanceCredential :one
SELECT worker_instance_credentials.id,
       worker_instance_credentials.worker_group_id,
       worker_instance_credentials.worker_instance_id,
       worker_instance_credentials.claim_version,
       worker_instances.resource_id
  FROM worker_instance_credentials
  JOIN worker_instances ON worker_instances.id = worker_instance_credentials.worker_instance_id
  JOIN worker_groups ON worker_groups.id = worker_instances.worker_group_id
WHERE worker_instance_credentials.id = sqlc.arg(credential_id)
   AND worker_instance_credentials.worker_instance_id = sqlc.arg(worker_instance_id)
   AND worker_instance_credentials.worker_group_id = sqlc.arg(worker_group_id)
   AND worker_instance_credentials.worker_group_id = worker_instances.worker_group_id
   AND worker_instance_credentials.claim_version = worker_instances.claim_version
   AND worker_instance_credentials.claim_version = worker_groups.claim_version
   AND worker_groups.state = 'active'
   AND worker_instance_credentials.revoked_at IS NULL;
