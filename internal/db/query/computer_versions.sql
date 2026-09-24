-- name: GetComputerVersionAuthority :one
SELECT computer_versions.id AS version_id,
       computer_versions.parent_version_id,
       computer_versions.source_workspace_lease_id,
       computer_versions.ownership_generation,
       computer_versions.writer_generation
  FROM computer_versions
  JOIN computers
    ON computers.environment_id = computer_versions.environment_id
   AND computers.id = computer_versions.workspace_id
  JOIN environments ON environments.id = computers.environment_id
 WHERE environments.org_id = sqlc.arg(org_id)
   AND environments.project_id = sqlc.arg(project_id)
   AND computer_versions.environment_id = sqlc.arg(environment_id)
   AND computer_versions.workspace_id = sqlc.arg(workspace_id)
   AND computer_versions.id = sqlc.arg(version_id)
   AND computer_versions.status IN ('committed', 'private');
