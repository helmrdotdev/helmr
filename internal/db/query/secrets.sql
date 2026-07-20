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
        key_id,
        nonce,
        ciphertext,
        value_authenticator
    )
    SELECT
        sqlc.arg(version_id),
        secret.id,
        1,
        sqlc.arg(key_id),
        sqlc.arg(nonce),
        sqlc.arg(ciphertext),
        sqlc.arg(value_authenticator)
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
      AND secrets.state = 'active'
      AND secrets.state_version = sqlc.arg(expected_state_version)
      AND secrets.current_version_id = sqlc.arg(expected_current_version_id)
    FOR UPDATE
),
version AS (
    INSERT INTO secret_versions (
        id,
        secret_id,
        version,
        key_id,
        nonce,
        ciphertext,
        value_authenticator
    )
    SELECT
        sqlc.arg(version_id),
        locked.id,
        sqlc.arg(version),
        sqlc.arg(key_id),
        sqlc.arg(nonce),
        sqlc.arg(ciphertext),
        sqlc.arg(value_authenticator)
    FROM locked
    RETURNING secret_id, id
)
UPDATE secrets
SET current_version_id = version.id,
    state_version = state_version + 1,
    updated_at = now()
FROM version
WHERE secrets.id = version.secret_id
RETURNING secrets.*;

-- name: RevokeSecret :one
UPDATE secrets
SET state = 'revoked',
    state_version = state_version + 1,
    current_version_id = NULL,
    revocation_generation = revocation_generation + 1,
    revoked_at = now(),
    updated_at = now()
WHERE environment_id = sqlc.arg(environment_id)
  AND id = sqlc.arg(id)
  AND state = 'active'
  AND state_version = sqlc.arg(expected_state_version)
RETURNING *;

-- name: GetSecretByName :one
SELECT secrets.*
FROM secrets
WHERE environment_id = sqlc.arg(environment_id)
  AND name = sqlc.arg(name)
  AND state <> 'deleted';

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
  AND secrets.state = 'active';

-- name: ListSecrets :many
SELECT
    secrets.id,
    secrets.environment_id,
    environments.project_id,
    environments.org_id,
    secrets.name,
    secrets.state,
    secrets.state_version,
    secrets.created_at,
    secrets.updated_at,
    secrets.revoked_at
FROM secrets
JOIN environments ON environments.id = secrets.environment_id
WHERE secrets.environment_id = sqlc.arg(environment_id)
  AND secrets.state <> 'deleted'
ORDER BY secrets.name, secrets.id
LIMIT sqlc.arg(row_limit);

-- name: ListCurrentSecretKeyUsage :many
SELECT secret_versions.key_id, count(*)::bigint AS secret_count
FROM secrets
JOIN secret_versions
  ON secret_versions.secret_id = secrets.id
 AND secret_versions.id = secrets.current_version_id
WHERE secrets.state = 'active'
GROUP BY secret_versions.key_id
ORDER BY secret_versions.key_id;

-- name: ListCurrentSecretsByKeyID :many
SELECT
    secrets.id AS secret_id,
    secrets.environment_id,
    secrets.state_version,
    secret_versions.id AS version_id,
    secret_versions.version,
    secret_versions.key_id,
    secret_versions.nonce,
    secret_versions.ciphertext,
    secret_versions.value_authenticator
FROM secrets
JOIN secret_versions
  ON secret_versions.secret_id = secrets.id
 AND secret_versions.id = secrets.current_version_id
WHERE secrets.state = 'active'
  AND secret_versions.key_id = sqlc.arg(key_id)
ORDER BY secrets.updated_at, secrets.id
LIMIT sqlc.arg(row_limit);

-- name: ListWorkspaceSecrets :many
SELECT
    workspace_secrets.*,
    secrets.state AS secret_state,
    secrets.state_version AS secret_state_version,
    secrets.current_version_id,
    secrets.revocation_generation
FROM workspace_secrets
JOIN secrets ON secrets.id = workspace_secrets.secret_id
WHERE workspace_secrets.workspace_id = sqlc.arg(workspace_id)
ORDER BY workspace_secrets.placement_kind, workspace_secrets.placement_target;

-- name: CreateSecretResolution :one
INSERT INTO secret_resolutions (
    id,
    workspace_id,
    run_id,
    attempt_number,
    process_id,
    placement_kind,
    placement_target,
    secret_id,
    secret_version_id,
    revocation_generation
)
VALUES (
    sqlc.arg(id),
    sqlc.arg(workspace_id),
    sqlc.narg(run_id),
    sqlc.narg(attempt_number),
    sqlc.narg(process_id),
    sqlc.arg(placement_kind),
    sqlc.arg(placement_target),
    sqlc.arg(secret_id),
    sqlc.arg(secret_version_id),
    sqlc.arg(revocation_generation)
)
RETURNING *;
