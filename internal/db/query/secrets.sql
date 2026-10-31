-- name: CreateSecret :one
WITH secret AS (
    INSERT INTO secrets (
        id,
        environment_id,
        name,
        current_version_id
    )
    VALUES (
        sqlc.arg(id),
        sqlc.arg(environment_id),
        sqlc.arg(name),
        sqlc.arg(version_id)
    )
    RETURNING *
),
version AS (
    INSERT INTO secret_versions (
        id,
        secret_id,
        version,
        nonce,
        ciphertext
    )
    SELECT
        sqlc.arg(version_id),
        secret.id,
        1,
        sqlc.arg(nonce),
        sqlc.arg(ciphertext)
    FROM secret
    RETURNING secret_id
)
SELECT secret.*
FROM secret
JOIN version ON version.secret_id = secret.id;
-- name: RotateSecret :one
WITH locked AS (
    SELECT *
    FROM secrets
    WHERE secrets.environment_id = sqlc.arg(environment_id)
      AND secrets.id = sqlc.arg(secret_id)
      AND secrets.current_version_id = sqlc.arg(expected_current_version_id)
    FOR UPDATE
),
version AS (
    INSERT INTO secret_versions (
        id,
        secret_id,
        version,
        nonce,
        ciphertext
    )
    SELECT
        sqlc.arg(version_id),
        locked.id,
        sqlc.arg(version),
        sqlc.arg(nonce),
        sqlc.arg(ciphertext)
    FROM locked
    RETURNING secret_id, id
)
UPDATE secrets
SET current_version_id = version.id,
    revoked_at = NULL
FROM version
WHERE secrets.id = version.secret_id
RETURNING secrets.*;
-- name: RevokeSecret :one
UPDATE secrets
SET revoked_at = now()
WHERE environment_id = sqlc.arg(environment_id)
  AND id = sqlc.arg(id)
  AND revoked_at IS NULL
  AND current_version_id = sqlc.arg(expected_current_version_id)
RETURNING *;
-- name: GetSecretSnapshotByName :one
SELECT
    secrets.id,
    secrets.environment_id,
    secrets.name,
    secrets.created_at,
    CASE
        WHEN current.version > 1 THEN current.created_at
        ELSE NULL::timestamptz
    END AS rotated_at,
    secrets.revoked_at
FROM secrets
JOIN secret_versions AS current
  ON current.secret_id = secrets.id
 AND current.id = secrets.current_version_id
WHERE secrets.environment_id = sqlc.arg(environment_id)
  AND secrets.name = sqlc.arg(name);
-- name: GetSecretSnapshot :one
SELECT
    secrets.id,
    secrets.environment_id,
    secrets.name,
    secrets.created_at,
    CASE
        WHEN current.version > 1 THEN current.created_at
        ELSE NULL::timestamptz
    END AS rotated_at,
    secrets.revoked_at
FROM secrets
JOIN secret_versions AS current
  ON current.secret_id = secrets.id
 AND current.id = secrets.current_version_id
WHERE secrets.environment_id = sqlc.arg(environment_id)
  AND secrets.id = sqlc.arg(id);
-- name: GetSecret :one
SELECT secrets.*
FROM secrets
WHERE environment_id = sqlc.arg(environment_id)
  AND id = sqlc.arg(id);
-- name: GetSecretVersion :one
SELECT secret_versions.*
FROM secret_versions
JOIN secrets ON secrets.id = secret_versions.secret_id
WHERE secrets.environment_id = sqlc.arg(environment_id)
  AND secret_versions.secret_id = sqlc.arg(secret_id)
  AND secret_versions.id = sqlc.arg(version_id);
-- name: ListSecrets :many
SELECT
    secrets.id,
    secrets.environment_id,
    secrets.name,
    secrets.created_at,
    CASE
        WHEN current.version > 1 THEN current.created_at
        ELSE NULL::timestamptz
    END AS rotated_at,
    secrets.revoked_at
FROM secrets
JOIN secret_versions AS current
  ON current.secret_id = secrets.id
 AND current.id = secrets.current_version_id
WHERE secrets.environment_id = sqlc.arg(environment_id)
  AND (
      sqlc.narg(after_name)::text IS NULL
      OR (secrets.name, secrets.id) >
         (sqlc.narg(after_name)::text, sqlc.narg(after_id)::uuid)
  )
ORDER BY secrets.name, secrets.id
LIMIT sqlc.arg(row_limit);
