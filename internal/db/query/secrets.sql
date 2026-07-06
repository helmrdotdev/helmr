-- name: UpsertScopedSecret :one
INSERT INTO secrets (
    id,
    org_id,
    project_id,
    environment_id,
    name,
    version,
    key_id,
    nonce,
    ciphertext
)
SELECT
    sqlc.arg(id),
    sqlc.arg(org_id),
    sqlc.arg(project_id),
    sqlc.arg(environment_id),
    sqlc.arg(name),
    sqlc.arg(version),
    sqlc.arg(key_id),
    sqlc.arg(nonce),
    sqlc.arg(ciphertext)
ON CONFLICT (org_id, project_id, environment_id, name) DO UPDATE
   SET version = EXCLUDED.version,
       key_id = EXCLUDED.key_id,
       nonce = EXCLUDED.nonce,
       ciphertext = EXCLUDED.ciphertext,
       updated_at = now()
 WHERE secrets.version = sqlc.arg(previous_version)
RETURNING *;

-- name: GetSecretByName :one
WITH default_scope AS (
    SELECT projects.id AS project_id,
           environments.id AS environment_id
      FROM projects
      JOIN environments ON environments.org_id = projects.org_id
                       AND environments.project_id = projects.id
                       AND environments.is_default
     WHERE projects.org_id = sqlc.arg(org_id)
       AND projects.is_default
     LIMIT 1
)
SELECT secrets.*
  FROM secrets
 JOIN default_scope ON default_scope.project_id = secrets.project_id
                    AND default_scope.environment_id = secrets.environment_id
 WHERE secrets.org_id = sqlc.arg(org_id)
   AND secrets.name = sqlc.arg(name);

-- name: GetScopedSecretByName :one
SELECT secrets.*
  FROM secrets
 WHERE secrets.org_id = sqlc.arg(org_id)
   AND secrets.project_id = sqlc.arg(project_id)
   AND secrets.environment_id = sqlc.arg(environment_id)
   AND secrets.name = sqlc.arg(name);

-- name: GetScopedSecretMetadataByName :one
SELECT secrets.id, secrets.org_id, secrets.project_id, secrets.environment_id, secrets.name, secrets.created_at, secrets.updated_at
  FROM secrets
 WHERE secrets.org_id = sqlc.arg(org_id)
   AND secrets.project_id = sqlc.arg(project_id)
   AND secrets.environment_id = sqlc.arg(environment_id)
   AND secrets.name = sqlc.arg(name);

-- name: ListSecrets :many
WITH default_scope AS (
    SELECT projects.id AS project_id,
           environments.id AS environment_id
      FROM projects
      JOIN environments ON environments.org_id = projects.org_id
                       AND environments.project_id = projects.id
                       AND environments.is_default
     WHERE projects.org_id = sqlc.arg(org_id)
       AND projects.is_default
     LIMIT 1
)
SELECT secrets.id, secrets.org_id, secrets.project_id, secrets.environment_id, secrets.name, secrets.created_at, secrets.updated_at
  FROM secrets
 JOIN default_scope ON default_scope.project_id = secrets.project_id
                    AND default_scope.environment_id = secrets.environment_id
 WHERE secrets.org_id = sqlc.arg(org_id)
 ORDER BY name ASC
 LIMIT sqlc.arg(row_limit);

-- name: ListScopedSecrets :many
SELECT secrets.id, secrets.org_id, secrets.project_id, secrets.environment_id, secrets.name, secrets.created_at, secrets.updated_at
  FROM secrets
 WHERE secrets.org_id = sqlc.arg(org_id)
   AND secrets.project_id = sqlc.arg(project_id)
   AND secrets.environment_id = sqlc.arg(environment_id)
 ORDER BY name ASC
 LIMIT sqlc.arg(row_limit);

-- name: DeleteScopedSecret :execrows
DELETE FROM secrets
 WHERE secrets.org_id = sqlc.arg(org_id)
   AND secrets.project_id = sqlc.arg(project_id)
   AND secrets.environment_id = sqlc.arg(environment_id)
   AND secrets.name = sqlc.arg(name);

-- name: ListSecretKeyUsage :many
SELECT key_id, count(*)::bigint AS secret_count
  FROM secrets
 GROUP BY key_id
 ORDER BY key_id ASC;

-- name: CountSecretsByKeyID :one
SELECT count(*)::bigint
  FROM secrets
 WHERE key_id = sqlc.arg(key_id);

-- name: ListSecretsByKeyIDForRotation :many
SELECT *
  FROM secrets
 WHERE key_id = sqlc.arg(key_id)
 ORDER BY updated_at ASC, id ASC
 LIMIT sqlc.arg(row_limit);

-- name: UpdateSecretCiphertextForRotation :execrows
UPDATE secrets
   SET version = sqlc.arg(new_version),
       key_id = sqlc.arg(new_key_id),
       nonce = sqlc.arg(nonce),
       ciphertext = sqlc.arg(ciphertext),
       updated_at = now(),
       rotated_at = now()
 WHERE id = sqlc.arg(id)
   AND key_id = sqlc.arg(previous_key_id)
   AND version = sqlc.arg(previous_version);
