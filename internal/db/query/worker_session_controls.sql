-- name: LockWorkerControlSecrets :many
SELECT
    computer_secrets.*,
    secrets.status AS secret_status,
    secrets.revision AS secret_revision,
    secrets.current_version_id,
    secrets.revocation_generation
FROM computer_secrets
JOIN secrets ON secrets.id = computer_secrets.secret_id
WHERE computer_secrets.computer_id = ANY(sqlc.arg(computer_ids)::uuid[])
ORDER BY computer_secrets.secret_id, computer_secrets.computer_id, computer_secrets.placement_kind, computer_secrets.placement_target
FOR UPDATE OF secrets;

-- name: ReadWorkerControlSecrets :many
SELECT
    computer_secrets.*,
    secrets.status AS secret_status,
    secrets.revision AS secret_revision,
    secrets.current_version_id,
    secrets.revocation_generation
FROM computer_secrets
JOIN secrets ON secrets.id = computer_secrets.secret_id
WHERE computer_secrets.computer_id = ANY(sqlc.arg(computer_ids)::uuid[])
ORDER BY computer_secrets.secret_id, computer_secrets.computer_id, computer_secrets.placement_kind, computer_secrets.placement_target;

-- name: LockWorkerControlActors :many
WITH RECURSIVE source_owners AS (
    SELECT runs.id, runs.parent_run_id, runs.parent_owns_lifecycle, runs.session_id
      FROM runs
     WHERE runs.id = sqlc.arg(source_run_id)
       AND runs.environment_id = sqlc.arg(environment_id)
    UNION
    SELECT parent.id, parent.parent_run_id, parent.parent_owns_lifecycle, parent.session_id
      FROM runs parent
      JOIN source_owners child ON child.parent_run_id = parent.id
     WHERE child.parent_owns_lifecycle IS TRUE
       AND parent.environment_id = sqlc.arg(environment_id)
)
SELECT sqlc.embed(s), source_owners.id AS source_owner_run_id
  FROM sessions s
  LEFT JOIN source_owners ON source_owners.session_id = s.id
 WHERE s.environment_id = sqlc.arg(environment_id)
   AND (s.id = sqlc.arg(target_session_id) OR source_owners.id IS NOT NULL)
 ORDER BY s.id
 FOR UPDATE OF s;
