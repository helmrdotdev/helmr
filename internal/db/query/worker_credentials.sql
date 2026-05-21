-- name: UpsertWorkerRegistrationToken :one
INSERT INTO worker_registration_tokens (id, worker_pool_id, token_hash)
VALUES (
    sqlc.arg(id),
    sqlc.arg(worker_pool_id),
    sqlc.arg(token_hash)::bytea
)
ON CONFLICT (token_hash) DO UPDATE
   SET revoked_at = worker_registration_tokens.revoked_at
 WHERE worker_registration_tokens.worker_pool_id = excluded.worker_pool_id
RETURNING *;

-- name: CreateWorkerCredentialFromRegistration :one
WITH registration_token AS (
    SELECT worker_registration_tokens.worker_pool_id
      FROM worker_registration_tokens
      JOIN worker_pools ON worker_pools.id = worker_registration_tokens.worker_pool_id
     WHERE worker_registration_tokens.token_hash = sqlc.arg(registration_token_hash)
       AND worker_registration_tokens.revoked_at IS NULL
       AND worker_pools.archived_at IS NULL
     FOR UPDATE
),
reserved_worker_host AS (
    INSERT INTO worker_hosts (
        id,
        worker_pool_id,
        external_id,
        status,
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
    SELECT sqlc.arg(worker_host_id),
           registration_token.worker_pool_id,
           sqlc.arg(external_id),
           'offline',
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
      FROM registration_token
    ON CONFLICT (worker_pool_id, external_id) DO UPDATE
       SET external_id = EXCLUDED.external_id
    RETURNING id::text AS worker_host_id
),
revoked_existing_credentials AS (
    UPDATE worker_credentials
       SET revoked_at = now()
     FROM registration_token, reserved_worker_host
     WHERE worker_credentials.worker_pool_id = registration_token.worker_pool_id
       AND worker_credentials.worker_host_id = reserved_worker_host.worker_host_id
       AND worker_credentials.revoked_at IS NULL
    RETURNING worker_credentials.id
),
credential_rotation AS (
    SELECT count(*) FROM revoked_existing_credentials
),
registration_token_update AS (
    UPDATE worker_registration_tokens
       SET last_used_at = now(),
           last_used_by_worker_host_id = (SELECT worker_host_id FROM reserved_worker_host)
     WHERE worker_registration_tokens.token_hash = sqlc.arg(registration_token_hash)
       AND worker_registration_tokens.revoked_at IS NULL
     RETURNING 1
)
INSERT INTO worker_credentials (id, worker_pool_id, worker_host_id, external_id, key_prefix, secret_hash)
SELECT sqlc.arg(credential_id),
       registration_token.worker_pool_id,
       reserved_worker_host.worker_host_id,
       sqlc.arg(external_id),
       sqlc.arg(key_prefix),
       sqlc.arg(secret_hash)
  FROM registration_token
 CROSS JOIN reserved_worker_host
 CROSS JOIN registration_token_update
 CROSS JOIN credential_rotation
RETURNING id, worker_pool_id, worker_host_id, external_id, key_prefix, created_at;

-- name: AuthenticateWorkerCredential :one
UPDATE worker_credentials
   SET last_used_at = now()
  FROM worker_pools
 WHERE worker_credentials.worker_pool_id = worker_pools.id
   AND worker_pools.archived_at IS NULL
   AND worker_credentials.worker_host_id = sqlc.arg(worker_host_id)
   AND worker_credentials.secret_hash = sqlc.arg(secret_hash)
   AND worker_credentials.revoked_at IS NULL
RETURNING worker_credentials.id, worker_credentials.worker_pool_id, worker_credentials.worker_host_id, worker_credentials.external_id;

-- name: AuthorizeWorkerCredential :one
SELECT worker_credentials.id,
       worker_credentials.worker_pool_id,
       worker_credentials.worker_host_id,
       worker_credentials.external_id
  FROM worker_credentials
  JOIN worker_pools ON worker_pools.id = worker_credentials.worker_pool_id
 WHERE worker_credentials.id = sqlc.arg(credential_id)
   AND worker_credentials.worker_pool_id = sqlc.arg(worker_pool_id)
   AND worker_credentials.worker_host_id = sqlc.arg(worker_host_id)
   AND worker_credentials.revoked_at IS NULL
   AND worker_pools.archived_at IS NULL;
