-- name: GetComputerDiskVersionAuthority :one
SELECT computer_disk_versions.id AS version_id,
       computer_disk_versions.parent_version_id,
       computer_disk_versions.source_computer_instance_id,
       computer_disk_versions.writer_generation
  FROM computer_disk_versions
  JOIN computers
    ON computers.environment_id = computer_disk_versions.environment_id
   AND computers.id = computer_disk_versions.computer_id
  JOIN environments ON environments.id = computers.environment_id
 WHERE environments.org_id = sqlc.arg(org_id)
   AND environments.project_id = sqlc.arg(project_id)
   AND computer_disk_versions.environment_id = sqlc.arg(environment_id)
   AND computer_disk_versions.computer_id = sqlc.arg(computer_id)
   AND computer_disk_versions.id = sqlc.arg(version_id)
   AND computer_disk_versions.status IN ('committed', 'private');
