-- name: CreateSecret :one
WITH authority AS (
    SELECT lookup_hmac_versions.version
    FROM lookup_hmac_versions
    WHERE lookup_hmac_versions.version = sqlc.arg(authenticator_key_version)
      AND is_current
      AND retired_at IS NULL
),
secret AS (
    INSERT INTO secrets (
        id,
        environment_id,
        name,
        current_version_id
    )
    SELECT
        sqlc.arg(id),
        sqlc.arg(environment_id),
        sqlc.arg(name),
        sqlc.arg(version_id)
    FROM authority
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
        value_authenticator,
        authenticator_key_version
    )
    SELECT
        sqlc.arg(version_id),
        secret.id,
        1,
        sqlc.arg(key_id),
        sqlc.arg(nonce),
        sqlc.arg(ciphertext),
        sqlc.arg(value_authenticator),
        sqlc.arg(authenticator_key_version)
    FROM secret
    RETURNING secret_id
)
SELECT secret.*
FROM secret
JOIN version ON version.secret_id = secret.id;

-- name: RotateSecret :one
WITH authority AS (
    SELECT lookup_hmac_versions.version
    FROM lookup_hmac_versions
    WHERE lookup_hmac_versions.version = sqlc.arg(authenticator_key_version)
      AND is_current
      AND retired_at IS NULL
),
locked AS (
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
        value_authenticator,
        authenticator_key_version
    )
    SELECT
        sqlc.arg(version_id),
        locked.id,
        sqlc.arg(version),
        sqlc.arg(key_id),
        sqlc.arg(nonce),
        sqlc.arg(ciphertext),
        sqlc.arg(value_authenticator),
        sqlc.arg(authenticator_key_version)
    FROM locked
    JOIN authority
      ON authority.version = sqlc.arg(authenticator_key_version)
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

-- name: GetSecretSnapshotByName :one
SELECT
    secrets.id,
    secrets.environment_id,
    secrets.name,
    secrets.state,
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
  AND secrets.name = sqlc.arg(name)
  AND secrets.state <> 'deleted';

-- name: GetSecretSnapshot :one
SELECT
    secrets.id,
    secrets.environment_id,
    secrets.name,
    secrets.state,
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
  AND secrets.id = sqlc.arg(id)
  AND secrets.state <> 'deleted';

-- name: GetSecret :one
SELECT secrets.*
FROM secrets
WHERE environment_id = sqlc.arg(environment_id)
  AND id = sqlc.arg(id)
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
    secrets.name,
    secrets.state,
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
  AND secrets.state <> 'deleted'
ORDER BY secrets.name, secrets.id
LIMIT sqlc.arg(row_limit);

-- name: ListSecretEncryptionKeyUsage :many
SELECT secret_versions.key_id, count(*)::bigint AS secret_count
FROM secret_versions
GROUP BY secret_versions.key_id
ORDER BY secret_versions.key_id;

-- name: ListSecretAuthenticatorKeyUsage :many
SELECT secret_versions.authenticator_key_version, count(*)::bigint AS secret_count
FROM secret_versions
GROUP BY secret_versions.authenticator_key_version
ORDER BY secret_versions.authenticator_key_version;

-- name: ListSecretVersionsByKeyID :many
SELECT
    secrets.id AS secret_id,
    secrets.environment_id,
    secrets.name,
    secret_versions.id AS version_id,
    secret_versions.version,
    secret_versions.key_id,
    secret_versions.nonce,
    secret_versions.ciphertext,
    secret_versions.value_authenticator,
    secret_versions.authenticator_key_version
FROM secrets
JOIN secret_versions ON secret_versions.secret_id = secrets.id
WHERE secret_versions.key_id = sqlc.arg(key_id)
ORDER BY secret_versions.created_at, secret_versions.id
LIMIT sqlc.arg(row_limit);

-- name: UpdateSecretVersionEnvelope :execrows
UPDATE secret_versions
SET key_id = sqlc.arg(new_key_id),
    nonce = sqlc.arg(new_nonce),
    ciphertext = sqlc.arg(new_ciphertext)
WHERE id = sqlc.arg(version_id)
  AND key_id = sqlc.arg(previous_key_id)
  AND nonce = sqlc.arg(previous_nonce)
  AND ciphertext = sqlc.arg(previous_ciphertext);

-- name: ListSecretVersionsByAuthenticatorKeyVersion :many
SELECT
    secrets.id AS secret_id,
    secrets.environment_id,
    secrets.name,
    secret_versions.id AS version_id,
    secret_versions.version,
    secret_versions.key_id,
    secret_versions.nonce,
    secret_versions.ciphertext,
    secret_versions.value_authenticator,
    secret_versions.authenticator_key_version
FROM secrets
JOIN secret_versions ON secret_versions.secret_id = secrets.id
WHERE secret_versions.authenticator_key_version = sqlc.arg(authenticator_key_version)
ORDER BY secret_versions.created_at, secret_versions.id
LIMIT sqlc.arg(row_limit);

-- name: UpdateSecretVersionAuthenticator :execrows
UPDATE secret_versions
SET value_authenticator = sqlc.arg(new_value_authenticator),
    authenticator_key_version = sqlc.arg(new_authenticator_key_version)
WHERE id = sqlc.arg(version_id)
  AND authenticator_key_version = sqlc.arg(previous_authenticator_key_version)
  AND value_authenticator = sqlc.arg(previous_value_authenticator)
  AND EXISTS (
      SELECT 1
      FROM lookup_hmac_versions
      WHERE lookup_hmac_versions.version = sqlc.arg(new_authenticator_key_version)
        AND is_current
        AND retired_at IS NULL
  );

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

-- name: LockWorkspaceSecretsForAdmission :many
SELECT
    workspace_secrets.*,
    secrets.state AS secret_state,
    secrets.state_version AS secret_state_version,
    secrets.current_version_id,
    secrets.revocation_generation
FROM workspace_secrets
JOIN secrets ON secrets.id = workspace_secrets.secret_id
WHERE workspace_secrets.workspace_id = sqlc.arg(workspace_id)
ORDER BY workspace_secrets.secret_id
FOR UPDATE OF secrets;

-- name: LockAttemptSecretDelivery :many
SELECT
    sqlc.embed(workspace_secrets),
    sqlc.embed(secrets),
    secret_resolutions.id AS resolution_id,
    secret_resolutions.run_id AS resolution_run_id,
    secret_resolutions.attempt_number AS resolution_attempt_number,
    secret_resolutions.secret_version_id AS resolution_secret_version_id,
    secret_resolutions.revocation_generation AS resolution_revocation_generation
FROM workspace_secrets
JOIN secrets
  ON secrets.environment_id = workspace_secrets.environment_id
 AND secrets.id = workspace_secrets.secret_id
LEFT JOIN secret_resolutions
  ON secret_resolutions.workspace_id = workspace_secrets.workspace_id
 AND secret_resolutions.run_id = sqlc.arg(run_id)
 AND secret_resolutions.attempt_number = sqlc.arg(attempt_number)
 AND secret_resolutions.placement_kind = workspace_secrets.placement_kind
 AND secret_resolutions.placement_target = workspace_secrets.placement_target
 AND secret_resolutions.secret_id = workspace_secrets.secret_id
WHERE workspace_secrets.workspace_id = sqlc.arg(workspace_id)
ORDER BY secrets.id, workspace_secrets.placement_kind, workspace_secrets.placement_target
LIMIT 65
FOR UPDATE OF secrets;

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
