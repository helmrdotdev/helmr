-- name: UpsertScopedSecret :one
INSERT INTO secrets (
    id,
    org_id,
    cell_id,
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
    sqlc.arg(cell_id),
    sqlc.arg(project_id),
    sqlc.arg(environment_id),
    sqlc.arg(name),
    sqlc.arg(version),
    sqlc.arg(key_id),
    sqlc.arg(nonce),
    sqlc.arg(ciphertext)
 WHERE EXISTS (
       SELECT 1
         FROM environment_cells
         JOIN org_cells ON org_cells.org_id = environment_cells.org_id
                       AND org_cells.cell_id = environment_cells.cell_id
                       AND org_cells.state = 'active'
         JOIN cells ON cells.id = environment_cells.cell_id
                   AND cells.region_id = environment_cells.region_id
                   AND cells.state IN ('active', 'draining')
        WHERE environment_cells.org_id = sqlc.arg(org_id)
          AND environment_cells.project_id = sqlc.arg(project_id)
          AND environment_cells.environment_id = sqlc.arg(environment_id)
          AND environment_cells.cell_id = sqlc.arg(cell_id)
          AND environment_cells.route_state IN ('active', 'draining')
   )
ON CONFLICT (org_id, cell_id, project_id, environment_id, name) DO UPDATE
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
       AND EXISTS (
           SELECT 1
             FROM environment_cells
             JOIN org_cells ON org_cells.org_id = environment_cells.org_id
                           AND org_cells.cell_id = environment_cells.cell_id
                           AND org_cells.state = 'active'
             JOIN cells ON cells.id = environment_cells.cell_id
                       AND cells.region_id = environment_cells.region_id
                       AND cells.state IN ('active', 'draining')
            WHERE environment_cells.org_id = projects.org_id
              AND environment_cells.project_id = projects.id
              AND environment_cells.environment_id = environments.id
              AND environment_cells.cell_id = sqlc.arg(cell_id)
              AND environment_cells.route_state IN ('active', 'draining')
       )
     LIMIT 1
)
SELECT secrets.*
  FROM secrets
 JOIN default_scope ON default_scope.project_id = secrets.project_id
                    AND default_scope.environment_id = secrets.environment_id
 WHERE secrets.org_id = sqlc.arg(org_id)
   AND secrets.cell_id = sqlc.arg(cell_id)
   AND secrets.name = sqlc.arg(name);

-- name: GetScopedSecretByName :one
SELECT secrets.*
  FROM secrets
 WHERE secrets.org_id = sqlc.arg(org_id)
   AND secrets.cell_id = sqlc.arg(cell_id)
   AND secrets.project_id = sqlc.arg(project_id)
   AND secrets.environment_id = sqlc.arg(environment_id)
   AND secrets.name = sqlc.arg(name)
   AND EXISTS (
       SELECT 1
         FROM environment_cells
         JOIN org_cells ON org_cells.org_id = environment_cells.org_id
                       AND org_cells.cell_id = environment_cells.cell_id
                       AND org_cells.state = 'active'
         JOIN cells ON cells.id = environment_cells.cell_id
                   AND cells.region_id = environment_cells.region_id
                   AND cells.state IN ('active', 'draining')
        WHERE environment_cells.org_id = secrets.org_id
          AND environment_cells.project_id = secrets.project_id
          AND environment_cells.environment_id = secrets.environment_id
          AND environment_cells.cell_id = secrets.cell_id
          AND environment_cells.route_state IN ('active', 'draining')
   );

-- name: GetScopedSecretMetadataByName :one
SELECT secrets.id, secrets.org_id, secrets.project_id, secrets.environment_id, secrets.name, secrets.created_at, secrets.updated_at
  FROM secrets
 WHERE secrets.org_id = sqlc.arg(org_id)
   AND secrets.cell_id = sqlc.arg(cell_id)
   AND secrets.project_id = sqlc.arg(project_id)
   AND secrets.environment_id = sqlc.arg(environment_id)
   AND secrets.name = sqlc.arg(name)
   AND EXISTS (
       SELECT 1
         FROM environment_cells
         JOIN org_cells ON org_cells.org_id = environment_cells.org_id
                       AND org_cells.cell_id = environment_cells.cell_id
                       AND org_cells.state = 'active'
         JOIN cells ON cells.id = environment_cells.cell_id
                   AND cells.region_id = environment_cells.region_id
                   AND cells.state IN ('active', 'draining')
        WHERE environment_cells.org_id = secrets.org_id
          AND environment_cells.project_id = secrets.project_id
          AND environment_cells.environment_id = secrets.environment_id
          AND environment_cells.cell_id = secrets.cell_id
          AND environment_cells.route_state IN ('active', 'draining')
   );

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
       AND EXISTS (
           SELECT 1
             FROM environment_cells
             JOIN org_cells ON org_cells.org_id = environment_cells.org_id
                           AND org_cells.cell_id = environment_cells.cell_id
                           AND org_cells.state = 'active'
             JOIN cells ON cells.id = environment_cells.cell_id
                       AND cells.region_id = environment_cells.region_id
                       AND cells.state IN ('active', 'draining')
            WHERE environment_cells.org_id = projects.org_id
              AND environment_cells.project_id = projects.id
              AND environment_cells.environment_id = environments.id
              AND environment_cells.cell_id = sqlc.arg(cell_id)
              AND environment_cells.route_state IN ('active', 'draining')
       )
     LIMIT 1
)
SELECT secrets.id, secrets.org_id, secrets.project_id, secrets.environment_id, secrets.name, secrets.created_at, secrets.updated_at
  FROM secrets
 JOIN default_scope ON default_scope.project_id = secrets.project_id
                    AND default_scope.environment_id = secrets.environment_id
 WHERE secrets.org_id = sqlc.arg(org_id)
   AND secrets.cell_id = sqlc.arg(cell_id)
 ORDER BY name ASC
 LIMIT sqlc.arg(row_limit);

-- name: ListScopedSecrets :many
SELECT secrets.id, secrets.org_id, secrets.project_id, secrets.environment_id, secrets.name, secrets.created_at, secrets.updated_at
  FROM secrets
 WHERE secrets.org_id = sqlc.arg(org_id)
   AND secrets.cell_id = sqlc.arg(cell_id)
   AND secrets.project_id = sqlc.arg(project_id)
   AND secrets.environment_id = sqlc.arg(environment_id)
   AND EXISTS (
       SELECT 1
         FROM environment_cells
         JOIN org_cells ON org_cells.org_id = environment_cells.org_id
                       AND org_cells.cell_id = environment_cells.cell_id
                       AND org_cells.state = 'active'
         JOIN cells ON cells.id = environment_cells.cell_id
                   AND cells.region_id = environment_cells.region_id
                   AND cells.state IN ('active', 'draining')
        WHERE environment_cells.org_id = secrets.org_id
          AND environment_cells.project_id = secrets.project_id
          AND environment_cells.environment_id = secrets.environment_id
          AND environment_cells.cell_id = secrets.cell_id
          AND environment_cells.route_state IN ('active', 'draining')
   )
 ORDER BY name ASC
 LIMIT sqlc.arg(row_limit);

-- name: DeleteScopedSecret :execrows
DELETE FROM secrets
 WHERE secrets.org_id = sqlc.arg(org_id)
   AND secrets.cell_id = sqlc.arg(cell_id)
   AND secrets.project_id = sqlc.arg(project_id)
   AND secrets.environment_id = sqlc.arg(environment_id)
   AND secrets.name = sqlc.arg(name)
   AND EXISTS (
       SELECT 1
         FROM environment_cells
         JOIN org_cells ON org_cells.org_id = environment_cells.org_id
                       AND org_cells.cell_id = environment_cells.cell_id
                       AND org_cells.state = 'active'
         JOIN cells ON cells.id = environment_cells.cell_id
                   AND cells.region_id = environment_cells.region_id
                   AND cells.state IN ('active', 'draining')
        WHERE environment_cells.org_id = secrets.org_id
          AND environment_cells.project_id = secrets.project_id
          AND environment_cells.environment_id = secrets.environment_id
          AND environment_cells.cell_id = secrets.cell_id
          AND environment_cells.route_state IN ('active', 'draining')
   );

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
