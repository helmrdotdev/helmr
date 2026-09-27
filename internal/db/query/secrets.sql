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

-- name: LockActiveSecretsByNameForComputerCreate :many
SELECT secrets.*
FROM secrets
WHERE environment_id = sqlc.arg(environment_id)
  AND name = ANY(sqlc.arg(names)::text[])
  AND status = 'active'
  AND current_version_id IS NOT NULL
ORDER BY secrets.id
FOR NO KEY UPDATE;

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

-- name: LockSecretVersion :one
SELECT secret_versions.*
FROM secret_versions
JOIN secrets ON secrets.id = secret_versions.secret_id
WHERE secrets.environment_id = sqlc.arg(environment_id)
  AND secret_versions.secret_id = sqlc.arg(secret_id)
  AND secret_versions.id = sqlc.arg(version_id)
FOR SHARE OF secret_versions;

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

-- name: ListSecretRevocationRuns :many
WITH RECURSIVE affected_runs AS MATERIALIZED (
    SELECT DISTINCT runs.org_id,
           runs.project_id,
           runs.environment_id,
           runs.computer_id,
           runs.id,
           runs.parent_run_id,
           runs.created_at
      FROM secret_resolutions
      JOIN runs
        ON runs.id = secret_resolutions.run_id
       AND runs.computer_id = secret_resolutions.computer_id
       AND runs.current_attempt_number = secret_resolutions.attempt_number
     WHERE secret_resolutions.secret_id = sqlc.arg(secret_id)
       AND secret_resolutions.revocation_generation < sqlc.arg(revocation_generation)
       AND runs.environment_id = sqlc.arg(environment_id)
       AND runs.status IN (
           'queued', 'running', 'waiting', 'retry_delayed', 'cancel_requested'
       )
), ancestor_walk AS (
    SELECT affected_runs.id AS candidate_id,
           affected_runs.parent_run_id,
           0 AS depth
      FROM affected_runs
    UNION ALL
    SELECT ancestor_walk.candidate_id,
           parent.parent_run_id,
           ancestor_walk.depth + 1
      FROM ancestor_walk
      JOIN runs AS parent ON parent.id = ancestor_walk.parent_run_id
), candidate_depths AS (
    SELECT candidate_id, max(depth) AS depth
      FROM ancestor_walk
     GROUP BY candidate_id
)
SELECT affected_runs.org_id,
       affected_runs.project_id,
       affected_runs.environment_id,
       affected_runs.computer_id,
       affected_runs.id
  FROM affected_runs
  JOIN candidate_depths ON candidate_depths.candidate_id = affected_runs.id
 ORDER BY candidate_depths.depth, affected_runs.created_at, affected_runs.id
 LIMIT sqlc.arg(row_limit);

-- name: ListSecretRevocationProcesses :many
SELECT DISTINCT command_environment.org_id,
       computer_commands.computer_id,
       computer_commands.id,
       computer_commands.revision,
       computer_commands.created_at
  FROM secret_resolutions
  JOIN computer_commands
    ON computer_commands.id = secret_resolutions.command_id
   AND computer_commands.computer_id = secret_resolutions.computer_id
  JOIN environments command_environment ON command_environment.id=computer_commands.environment_id
 WHERE secret_resolutions.secret_id = sqlc.arg(secret_id)
   AND secret_resolutions.revocation_generation < sqlc.arg(revocation_generation)
   AND computer_commands.environment_id = sqlc.arg(environment_id)
   AND computer_commands.terminal_at IS NULL
   AND computer_commands.cancel_requested_at IS NULL
 ORDER BY computer_commands.created_at, computer_commands.id
 LIMIT sqlc.arg(row_limit);

-- name: ListComputerSecrets :many
SELECT
    computer_secrets.*,
    secrets.name AS secret_name,
    secrets.status AS secret_status,
    secrets.revision AS secret_revision,
    secrets.current_version_id,
    secrets.revocation_generation
FROM computer_secrets
JOIN secrets ON secrets.id = computer_secrets.secret_id
WHERE computer_secrets.computer_id = sqlc.arg(computer_id)
ORDER BY computer_secrets.placement_kind, computer_secrets.placement_target;

-- name: LockComputerSecretsForAdmission :many
SELECT
    computer_secrets.*,
    secrets.status AS secret_status,
    secrets.revision AS secret_revision,
    secrets.current_version_id,
    secrets.revocation_generation
FROM computer_secrets
JOIN secrets ON secrets.id = computer_secrets.secret_id
WHERE computer_secrets.computer_id = sqlc.arg(computer_id)
ORDER BY computer_secrets.secret_id
FOR UPDATE OF secrets;

-- name: LockAttemptSecretDelivery :many
SELECT
    sqlc.embed(computer_secrets),
    sqlc.embed(secrets),
    secret_resolutions.id AS resolution_id,
    secret_resolutions.run_id AS resolution_run_id,
    secret_resolutions.attempt_number AS resolution_attempt_number,
    secret_resolutions.secret_version_id AS resolution_secret_version_id,
    secret_resolutions.revocation_generation AS resolution_revocation_generation
FROM computer_secrets
JOIN secrets
  ON secrets.environment_id = computer_secrets.environment_id
 AND secrets.id = computer_secrets.secret_id
LEFT JOIN secret_resolutions
  ON secret_resolutions.computer_id = computer_secrets.computer_id
 AND secret_resolutions.run_id = sqlc.arg(run_id)
 AND secret_resolutions.attempt_number = sqlc.arg(attempt_number)
 AND secret_resolutions.placement_kind = computer_secrets.placement_kind
 AND secret_resolutions.placement_target = computer_secrets.placement_target
 AND secret_resolutions.secret_id = computer_secrets.secret_id
WHERE computer_secrets.computer_id = sqlc.arg(computer_id)
ORDER BY secrets.id, computer_secrets.placement_kind, computer_secrets.placement_target
LIMIT 65
FOR UPDATE OF secrets;

-- name: LockProcessSecretDelivery :many
SELECT
    sqlc.embed(computer_secrets),
    sqlc.embed(secrets),
    secret_resolutions.id AS resolution_id,
    secret_resolutions.command_id AS resolution_command_id,
    secret_resolutions.secret_version_id AS resolution_secret_version_id,
    secret_resolutions.revocation_generation AS resolution_revocation_generation
FROM computer_secrets
JOIN secrets
  ON secrets.environment_id = computer_secrets.environment_id
 AND secrets.id = computer_secrets.secret_id
LEFT JOIN secret_resolutions
  ON secret_resolutions.computer_id = computer_secrets.computer_id
 AND secret_resolutions.command_id = sqlc.arg(command_id)
 AND secret_resolutions.placement_kind = computer_secrets.placement_kind
 AND secret_resolutions.placement_target = computer_secrets.placement_target
 AND secret_resolutions.secret_id = computer_secrets.secret_id
WHERE computer_secrets.computer_id = sqlc.arg(computer_id)
ORDER BY secrets.id, computer_secrets.placement_kind, computer_secrets.placement_target
LIMIT 65
FOR UPDATE OF secrets;

-- name: CreateAttemptSecretResolutions :execrows
INSERT INTO secret_resolutions (
    id,
    computer_id,
    run_id,
    attempt_number,
    placement_kind,
    placement_target,
    secret_id,
    secret_version_id,
    revocation_generation
)
SELECT
    input_ids.id,
    sqlc.arg(computer_id),
    sqlc.arg(run_id),
    sqlc.arg(attempt_number),
    input_kinds.placement_kind,
    input_targets.placement_target,
    input_secrets.secret_id,
    input_versions.secret_version_id,
    input_generations.revocation_generation
FROM unnest(sqlc.arg(ids)::uuid[])
     WITH ORDINALITY AS input_ids(id, position)
JOIN unnest(sqlc.arg(placement_kinds)::text[])
     WITH ORDINALITY AS input_kinds(placement_kind, position)
  ON input_kinds.position = input_ids.position
JOIN unnest(sqlc.arg(placement_targets)::text[])
     WITH ORDINALITY AS input_targets(placement_target, position)
  ON input_targets.position = input_ids.position
JOIN unnest(sqlc.arg(secret_ids)::uuid[])
     WITH ORDINALITY AS input_secrets(secret_id, position)
  ON input_secrets.position = input_ids.position
JOIN unnest(sqlc.arg(secret_version_ids)::uuid[])
     WITH ORDINALITY AS input_versions(secret_version_id, position)
  ON input_versions.position = input_ids.position
JOIN unnest(sqlc.arg(revocation_generations)::bigint[])
     WITH ORDINALITY AS input_generations(revocation_generation, position)
  ON input_generations.position = input_ids.position
WHERE cardinality(sqlc.arg(ids)::uuid[]) BETWEEN 1 AND 64
  AND cardinality(sqlc.arg(placement_kinds)::text[]) = cardinality(sqlc.arg(ids)::uuid[])
  AND cardinality(sqlc.arg(placement_targets)::text[]) = cardinality(sqlc.arg(ids)::uuid[])
  AND cardinality(sqlc.arg(secret_ids)::uuid[]) = cardinality(sqlc.arg(ids)::uuid[])
  AND cardinality(sqlc.arg(secret_version_ids)::uuid[]) = cardinality(sqlc.arg(ids)::uuid[])
  AND cardinality(sqlc.arg(revocation_generations)::bigint[]) = cardinality(sqlc.arg(ids)::uuid[]);

-- name: CreateProcessSecretResolutions :execrows
INSERT INTO secret_resolutions (
    id,
    computer_id,
    command_id,
    placement_kind,
    placement_target,
    secret_id,
    secret_version_id,
    revocation_generation
)
SELECT
    input_ids.id,
    sqlc.arg(computer_id),
    sqlc.arg(command_id),
    input_kinds.placement_kind,
    input_targets.placement_target,
    input_secrets.secret_id,
    input_versions.secret_version_id,
    input_generations.revocation_generation
FROM unnest(sqlc.arg(ids)::uuid[])
     WITH ORDINALITY AS input_ids(id, position)
JOIN unnest(sqlc.arg(placement_kinds)::text[])
     WITH ORDINALITY AS input_kinds(placement_kind, position)
  ON input_kinds.position = input_ids.position
JOIN unnest(sqlc.arg(placement_targets)::text[])
     WITH ORDINALITY AS input_targets(placement_target, position)
  ON input_targets.position = input_ids.position
JOIN unnest(sqlc.arg(secret_ids)::uuid[])
     WITH ORDINALITY AS input_secrets(secret_id, position)
  ON input_secrets.position = input_ids.position
JOIN unnest(sqlc.arg(secret_version_ids)::uuid[])
     WITH ORDINALITY AS input_versions(secret_version_id, position)
  ON input_versions.position = input_ids.position
JOIN unnest(sqlc.arg(revocation_generations)::bigint[])
     WITH ORDINALITY AS input_generations(revocation_generation, position)
  ON input_generations.position = input_ids.position
WHERE cardinality(sqlc.arg(ids)::uuid[]) BETWEEN 1 AND 64
  AND cardinality(sqlc.arg(placement_kinds)::text[]) = cardinality(sqlc.arg(ids)::uuid[])
  AND cardinality(sqlc.arg(placement_targets)::text[]) = cardinality(sqlc.arg(ids)::uuid[])
  AND cardinality(sqlc.arg(secret_ids)::uuid[]) = cardinality(sqlc.arg(ids)::uuid[])
  AND cardinality(sqlc.arg(secret_version_ids)::uuid[]) = cardinality(sqlc.arg(ids)::uuid[])
  AND cardinality(sqlc.arg(revocation_generations)::bigint[]) = cardinality(sqlc.arg(ids)::uuid[]);
