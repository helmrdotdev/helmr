-- name: UpsertSecret :one
WITH default_scope AS (
    SELECT projects.id AS project_id,
           environments.id AS environment_id
      FROM projects
      JOIN environments ON environments.org_id = projects.org_id
                       AND environments.project_id = projects.id
                       AND environments.is_default
                       AND environments.archived_at IS NULL
     WHERE projects.org_id = sqlc.arg(org_id)
       AND projects.is_default
       AND projects.archived_at IS NULL
     LIMIT 1
)
INSERT INTO secrets (
    id,
    org_id,
    project_id,
    environment_id,
    name,
    key_id,
    nonce,
    ciphertext
)
SELECT
    sqlc.arg(id),
    sqlc.arg(org_id),
    default_scope.project_id,
    default_scope.environment_id,
    sqlc.arg(name),
    sqlc.arg(key_id),
    sqlc.arg(nonce),
    sqlc.arg(ciphertext)
  FROM default_scope
ON CONFLICT (org_id, project_id, environment_id, name) DO UPDATE
   SET key_id = EXCLUDED.key_id,
       nonce = EXCLUDED.nonce,
       ciphertext = EXCLUDED.ciphertext,
       updated_at = now(),
       deleted_at = NULL
RETURNING *;

-- name: UpsertScopedSecret :one
INSERT INTO secrets (
    id,
    org_id,
    project_id,
    environment_id,
    name,
    key_id,
    nonce,
    ciphertext
) VALUES (
    sqlc.arg(id),
    sqlc.arg(org_id),
    sqlc.arg(project_id),
    sqlc.arg(environment_id),
    sqlc.arg(name),
    sqlc.arg(key_id),
    sqlc.arg(nonce),
    sqlc.arg(ciphertext)
)
ON CONFLICT (org_id, project_id, environment_id, name) DO UPDATE
   SET key_id = EXCLUDED.key_id,
       nonce = EXCLUDED.nonce,
       ciphertext = EXCLUDED.ciphertext,
       updated_at = now(),
       deleted_at = NULL
RETURNING *;

-- name: GetSecretByName :one
WITH default_scope AS (
    SELECT projects.id AS project_id,
           environments.id AS environment_id
      FROM projects
      JOIN environments ON environments.org_id = projects.org_id
                       AND environments.project_id = projects.id
                       AND environments.is_default
                       AND environments.archived_at IS NULL
     WHERE projects.org_id = sqlc.arg(org_id)
       AND projects.is_default
       AND projects.archived_at IS NULL
     LIMIT 1
)
SELECT secrets.*
  FROM secrets
  JOIN default_scope ON default_scope.project_id = secrets.project_id
                    AND default_scope.environment_id = secrets.environment_id
 WHERE secrets.org_id = sqlc.arg(org_id)
   AND secrets.name = sqlc.arg(name)
   AND secrets.deleted_at IS NULL;

-- name: GetScopedSecretByName :one
SELECT *
  FROM secrets
 WHERE org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND name = sqlc.arg(name)
   AND deleted_at IS NULL;

-- name: ListSecrets :many
WITH default_scope AS (
    SELECT projects.id AS project_id,
           environments.id AS environment_id
      FROM projects
      JOIN environments ON environments.org_id = projects.org_id
                       AND environments.project_id = projects.id
                       AND environments.is_default
                       AND environments.archived_at IS NULL
     WHERE projects.org_id = sqlc.arg(org_id)
       AND projects.is_default
       AND projects.archived_at IS NULL
     LIMIT 1
)
SELECT secrets.id, secrets.org_id, secrets.project_id, secrets.environment_id, secrets.name, secrets.created_at, secrets.updated_at
  FROM secrets
  JOIN default_scope ON default_scope.project_id = secrets.project_id
                    AND default_scope.environment_id = secrets.environment_id
 WHERE secrets.org_id = sqlc.arg(org_id)
   AND secrets.deleted_at IS NULL
 ORDER BY name ASC
 LIMIT sqlc.arg(row_limit);

-- name: ListScopedSecrets :many
SELECT id, org_id, project_id, environment_id, name, created_at, updated_at
  FROM secrets
 WHERE org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND deleted_at IS NULL
 ORDER BY name ASC
 LIMIT sqlc.arg(row_limit);
