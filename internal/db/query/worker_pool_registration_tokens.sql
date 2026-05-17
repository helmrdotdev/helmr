-- name: EnsureDefaultWorkerPoolRegistrationToken :one
WITH default_pool AS (
    SELECT worker_pools.id,
           worker_pools.project_id,
           worker_pools.environment_id
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
),
upserted AS (
    INSERT INTO worker_pool_registration_tokens (id, org_id, project_id, environment_id, worker_pool_id, token_hash)
    SELECT
        sqlc.arg(id),
        sqlc.arg(org_id),
        default_pool.project_id,
        default_pool.environment_id,
        default_pool.id,
        sqlc.arg(token_hash)
      FROM default_pool
    ON CONFLICT (token_hash) DO UPDATE
       SET org_id = excluded.org_id,
           project_id = excluded.project_id,
           environment_id = excluded.environment_id,
           worker_pool_id = excluded.worker_pool_id,
           revoked_at = NULL
    RETURNING *
),
revoked AS (
    UPDATE worker_pool_registration_tokens
       SET revoked_at = now()
      FROM upserted
     WHERE worker_pool_registration_tokens.org_id = upserted.org_id
       AND worker_pool_registration_tokens.project_id = upserted.project_id
       AND worker_pool_registration_tokens.environment_id = upserted.environment_id
       AND worker_pool_registration_tokens.worker_pool_id = upserted.worker_pool_id
       AND worker_pool_registration_tokens.token_hash <> upserted.token_hash
       AND worker_pool_registration_tokens.revoked_at IS NULL
    RETURNING worker_pool_registration_tokens.id
)
SELECT * FROM upserted;
