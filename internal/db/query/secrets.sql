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
      AND secrets.status = 'active'
      AND secrets.revision = sqlc.arg(expected_revision)
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
    revision = revision + 1,
    updated_at = now()
FROM version
WHERE secrets.id = version.secret_id
RETURNING secrets.*;
-- name: RevokeSecret :one
UPDATE secrets
SET status = 'revoked',
    revision = revision + 1,
    current_version_id = NULL,
    revocation_generation = revocation_generation + 1,
    revoked_at = now(),
    updated_at = now()
WHERE environment_id = sqlc.arg(environment_id)
  AND id = sqlc.arg(id)
  AND status = 'active'
  AND revision = sqlc.arg(expected_revision)
RETURNING *;
-- name: GetSecretSnapshotByName :one
SELECT
    secrets.id,
    secrets.environment_id,
    secrets.name,
    secrets.status,
    secrets.created_at,
    CASE
        WHEN latest.version > 1 THEN latest.created_at
        ELSE NULL::timestamptz
    END AS rotated_at,
    secrets.revoked_at
FROM secrets
LEFT JOIN LATERAL (
    SELECT secret_versions.version, secret_versions.created_at
    FROM secret_versions
    WHERE secret_versions.secret_id = secrets.id
    ORDER BY secret_versions.version DESC
    LIMIT 1
) AS latest ON true
WHERE secrets.environment_id = sqlc.arg(environment_id)
  AND secrets.name = sqlc.arg(name);
-- name: GetSecretSnapshot :one
SELECT
    secrets.id,
    secrets.environment_id,
    secrets.name,
    secrets.status,
    secrets.created_at,
    CASE
        WHEN latest.version > 1 THEN latest.created_at
        ELSE NULL::timestamptz
    END AS rotated_at,
    secrets.revoked_at
FROM secrets
LEFT JOIN LATERAL (
    SELECT secret_versions.version, secret_versions.created_at
    FROM secret_versions
    WHERE secret_versions.secret_id = secrets.id
    ORDER BY secret_versions.version DESC
    LIMIT 1
) AS latest ON true
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
-- name: GetCurrentSecretValue :one
SELECT secret_versions.*
FROM secrets
JOIN secret_versions
  ON secret_versions.secret_id = secrets.id
 AND secret_versions.id = secrets.current_version_id
WHERE secrets.environment_id = sqlc.arg(environment_id)
  AND secrets.id = sqlc.arg(secret_id)
  AND secrets.status = 'active';
-- name: ListSecrets :many
SELECT
    secrets.id,
    secrets.environment_id,
    secrets.name,
    secrets.status,
    secrets.created_at,
    CASE
        WHEN latest.version > 1 THEN latest.created_at
        ELSE NULL::timestamptz
    END AS rotated_at,
    secrets.revoked_at
FROM secrets
LEFT JOIN LATERAL (
    SELECT secret_versions.version, secret_versions.created_at
    FROM secret_versions
    WHERE secret_versions.secret_id = secrets.id
    ORDER BY secret_versions.version DESC
    LIMIT 1
) AS latest ON true
WHERE secrets.environment_id = sqlc.arg(environment_id)
  AND (
      sqlc.narg(after_name)::text IS NULL
      OR (secrets.name, secrets.id) >
         (sqlc.narg(after_name)::text, sqlc.narg(after_id)::uuid)
  )
ORDER BY secrets.name, secrets.id
LIMIT sqlc.arg(row_limit);
