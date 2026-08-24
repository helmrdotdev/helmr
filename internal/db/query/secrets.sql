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

-- name: LockActiveSecretsByNameForWorkspaceCreate :many
SELECT secrets.*
FROM secrets
WHERE environment_id = sqlc.arg(environment_id)
  AND name = ANY(sqlc.arg(names)::text[])
  AND state = 'active'
  AND current_version_id IS NOT NULL
ORDER BY secrets.id
FOR UPDATE;

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
           runs.workspace_id,
           runs.id,
           runs.parent_run_id,
           runs.created_at
      FROM secret_resolutions
      JOIN runs
        ON runs.id = secret_resolutions.run_id
       AND runs.workspace_id = secret_resolutions.workspace_id
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
       affected_runs.workspace_id,
       affected_runs.id
  FROM affected_runs
  JOIN candidate_depths ON candidate_depths.candidate_id = affected_runs.id
 ORDER BY candidate_depths.depth, affected_runs.created_at, affected_runs.id
 LIMIT sqlc.arg(row_limit);

-- name: ListSecretRevocationProcesses :many
SELECT DISTINCT workspace_processes.org_id,
       workspace_processes.workspace_id,
       workspace_processes.id,
       workspace_processes.state_version,
       workspace_processes.created_at
  FROM secret_resolutions
  JOIN workspace_processes
    ON workspace_processes.id = secret_resolutions.process_id
   AND workspace_processes.workspace_id = secret_resolutions.workspace_id
 WHERE secret_resolutions.secret_id = sqlc.arg(secret_id)
   AND secret_resolutions.revocation_generation < sqlc.arg(revocation_generation)
   AND workspace_processes.environment_id = sqlc.arg(environment_id)
   AND workspace_processes.state IN ('starting', 'running', 'exit_requested')
 ORDER BY workspace_processes.created_at, workspace_processes.id
 LIMIT sqlc.arg(row_limit);

-- name: ListWorkspaceSecrets :many
SELECT
    workspace_secrets.*,
    secrets.name AS secret_name,
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

-- name: LockAttemptSecretResolutionMetadata :many
SELECT
    workspace_secrets.placement_kind,
    workspace_secrets.placement_target,
    workspace_secrets.secret_id,
    secrets.state AS secret_state,
    secrets.current_version_id,
    secrets.revocation_generation,
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

-- name: LockProcessSecretDelivery :many
SELECT
    sqlc.embed(workspace_secrets),
    sqlc.embed(secrets),
    secret_resolutions.id AS resolution_id,
    secret_resolutions.process_id AS resolution_process_id,
    secret_resolutions.secret_version_id AS resolution_secret_version_id,
    secret_resolutions.revocation_generation AS resolution_revocation_generation
FROM workspace_secrets
JOIN secrets
  ON secrets.environment_id = workspace_secrets.environment_id
 AND secrets.id = workspace_secrets.secret_id
LEFT JOIN secret_resolutions
  ON secret_resolutions.workspace_id = workspace_secrets.workspace_id
 AND secret_resolutions.process_id = sqlc.arg(process_id)
 AND secret_resolutions.placement_kind = workspace_secrets.placement_kind
 AND secret_resolutions.placement_target = workspace_secrets.placement_target
 AND secret_resolutions.secret_id = workspace_secrets.secret_id
WHERE workspace_secrets.workspace_id = sqlc.arg(workspace_id)
ORDER BY secrets.id, workspace_secrets.placement_kind, workspace_secrets.placement_target
LIMIT 65
FOR UPDATE OF secrets;

-- name: CreateAttemptSecretResolutions :execrows
INSERT INTO secret_resolutions (
    id,
    workspace_id,
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
    sqlc.arg(workspace_id),
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
    workspace_id,
    process_id,
    placement_kind,
    placement_target,
    secret_id,
    secret_version_id,
    revocation_generation
)
SELECT
    input_ids.id,
    sqlc.arg(workspace_id),
    sqlc.arg(process_id),
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
