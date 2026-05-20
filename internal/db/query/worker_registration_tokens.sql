-- name: EnsureDefaultWorkerRegistrationToken :one
WITH default_group AS (
    SELECT worker_groups.id,
           worker_groups.project_id,
           worker_groups.environment_id
      FROM worker_groups
      JOIN projects ON projects.org_id = worker_groups.org_id
                   AND projects.id = worker_groups.project_id
                   AND projects.is_default
                   AND projects.archived_at IS NULL
      JOIN environments ON environments.org_id = worker_groups.org_id
                       AND environments.project_id = worker_groups.project_id
                       AND environments.id = worker_groups.environment_id
                       AND environments.is_default
                       AND environments.archived_at IS NULL
     WHERE worker_groups.org_id = sqlc.arg(org_id)
       AND worker_groups.slug = 'default'
       AND worker_groups.archived_at IS NULL
     LIMIT 1
),
upserted AS (
    INSERT INTO worker_registration_tokens (id, org_id, project_id, environment_id, worker_group_id, token_hash)
    SELECT
        sqlc.arg(id),
        sqlc.arg(org_id),
        default_group.project_id,
        default_group.environment_id,
        default_group.id,
        sqlc.arg(token_hash)
      FROM default_group
    ON CONFLICT (token_hash) DO UPDATE
       SET org_id = excluded.org_id,
           project_id = excluded.project_id,
           environment_id = excluded.environment_id,
           worker_group_id = excluded.worker_group_id,
           revoked_at = NULL
    RETURNING *
),
revoked AS (
    UPDATE worker_registration_tokens
       SET revoked_at = now()
      FROM upserted
     WHERE worker_registration_tokens.org_id = upserted.org_id
       AND worker_registration_tokens.project_id = upserted.project_id
       AND worker_registration_tokens.environment_id = upserted.environment_id
       AND worker_registration_tokens.worker_group_id = upserted.worker_group_id
       AND worker_registration_tokens.token_hash <> upserted.token_hash
       AND worker_registration_tokens.revoked_at IS NULL
    RETURNING worker_registration_tokens.id
)
SELECT * FROM upserted;
