-- name: UpsertWorkerBootstrapToken :one
INSERT INTO worker_bootstrap_tokens (id, token_hash)
VALUES (
    sqlc.arg(id),
    sqlc.arg(token_hash)::bytea
)
ON CONFLICT (token_hash) DO UPDATE
   SET revoked_at = worker_bootstrap_tokens.revoked_at
RETURNING *;

-- name: CreateWorkerInstanceCredentialFromBootstrap :one
WITH bootstrap_token AS (
    SELECT worker_bootstrap_tokens.id
      FROM worker_bootstrap_tokens
     WHERE worker_bootstrap_tokens.token_hash = sqlc.arg(bootstrap_token_hash)
       AND worker_bootstrap_tokens.revoked_at IS NULL
     FOR UPDATE
),
reserved_worker_instance AS (
    INSERT INTO worker_instances (
        id,
        resource_id,
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
    SELECT sqlc.arg(worker_instance_id),
           sqlc.arg(resource_id),
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
      FROM bootstrap_token
    ON CONFLICT (resource_id) DO UPDATE
       SET resource_id = EXCLUDED.resource_id
    RETURNING id AS worker_instance_id
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
       AND worker_bootstrap_tokens.revoked_at IS NULL
     RETURNING 1
)
INSERT INTO worker_instance_credentials (id, worker_instance_id, key_prefix, secret_hash)
SELECT sqlc.arg(credential_id),
       reserved_worker_instance.worker_instance_id,
       sqlc.arg(key_prefix),
       sqlc.arg(secret_hash)
  FROM bootstrap_token
 CROSS JOIN reserved_worker_instance
 CROSS JOIN bootstrap_token_update
 CROSS JOIN credential_rotation
RETURNING id, worker_instance_id, key_prefix, created_at;

-- name: AuthenticateWorkerInstanceCredential :one
UPDATE worker_instance_credentials
   SET last_used_at = now()
 WHERE worker_instance_credentials.worker_instance_id = sqlc.arg(worker_instance_id)
   AND worker_instance_credentials.secret_hash = sqlc.arg(secret_hash)
   AND worker_instance_credentials.revoked_at IS NULL
RETURNING worker_instance_credentials.id, worker_instance_credentials.worker_instance_id;

-- name: AuthorizeWorkerInstanceCredential :one
SELECT worker_instance_credentials.id,
       worker_instance_credentials.worker_instance_id,
       worker_instances.resource_id
  FROM worker_instance_credentials
  JOIN worker_instances ON worker_instances.id = worker_instance_credentials.worker_instance_id
 WHERE worker_instance_credentials.id = sqlc.arg(credential_id)
   AND worker_instance_credentials.worker_instance_id = sqlc.arg(worker_instance_id)
   AND worker_instance_credentials.revoked_at IS NULL;
