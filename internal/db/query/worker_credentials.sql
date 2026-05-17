-- name: CreateWorkerCredentialFromRegistration :one
WITH registration_token AS (
    UPDATE worker_pool_registration_tokens
       SET last_used_at = now(),
           last_used_by_worker_id = sqlc.arg(worker_id)
     WHERE token_hash = sqlc.arg(registration_token_hash)
       AND revoked_at IS NULL
     RETURNING org_id, project_id, environment_id, worker_pool_id
)
INSERT INTO worker_credentials (id, org_id, project_id, environment_id, worker_pool_id, worker_id, key_prefix, secret_hash)
SELECT sqlc.arg(credential_id),
       registration_token.org_id,
       registration_token.project_id,
       registration_token.environment_id,
       registration_token.worker_pool_id,
       sqlc.arg(worker_id),
       sqlc.arg(key_prefix),
       sqlc.arg(secret_hash)
  FROM registration_token
RETURNING id, org_id, project_id, environment_id, worker_pool_id, worker_id, key_prefix, created_at;

-- name: AuthenticateWorkerCredential :one
UPDATE worker_credentials
   SET last_used_at = now()
 WHERE worker_id = sqlc.arg(worker_id)
   AND secret_hash = sqlc.arg(secret_hash)
   AND revoked_at IS NULL
RETURNING id, org_id, project_id, environment_id, worker_pool_id, worker_id;

-- name: AuthenticateWorkerCredentialWithPool :one
UPDATE worker_credentials
   SET last_used_at = now()
 WHERE worker_id = sqlc.arg(worker_id)
   AND secret_hash = sqlc.arg(secret_hash)
   AND revoked_at IS NULL
RETURNING id, org_id, project_id, environment_id, worker_pool_id, worker_id;

-- name: AuthorizeWorkerCredential :one
SELECT id, org_id, project_id, environment_id, worker_pool_id, worker_id
  FROM worker_credentials
 WHERE id = sqlc.arg(credential_id)
   AND org_id = sqlc.arg(org_id)
   AND worker_id = sqlc.arg(worker_id)
   AND revoked_at IS NULL;

-- name: AuthorizeWorkerCredentialInPool :one
SELECT id, org_id, project_id, environment_id, worker_pool_id, worker_id
  FROM worker_credentials
 WHERE id = sqlc.arg(credential_id)
   AND org_id = sqlc.arg(org_id)
   AND worker_pool_id = sqlc.arg(worker_pool_id)
   AND worker_id = sqlc.arg(worker_id)
   AND revoked_at IS NULL;

-- name: RevokeWorkerCredential :execrows
UPDATE worker_credentials
   SET revoked_at = now()
 WHERE org_id = sqlc.arg(org_id)
   AND worker_id = sqlc.arg(worker_id)
   AND id = sqlc.arg(credential_id)
   AND revoked_at IS NULL;

-- name: RevokeWorkerCredentialsByWorkerID :execrows
UPDATE worker_credentials
   SET revoked_at = now()
 WHERE org_id = sqlc.arg(org_id)
   AND worker_id = sqlc.arg(worker_id)
   AND revoked_at IS NULL;
