-- name: SetEnvironmentPlacement :one
INSERT INTO environment_placements (org_id, project_id, environment_id, region_id, name, worker_group_id)
SELECT environments.org_id, environments.project_id, environments.id,
       projects.default_region_id, sqlc.arg(name), sqlc.narg(worker_group_id)
  FROM environments
  JOIN projects ON projects.org_id = environments.org_id AND projects.id = environments.project_id
 WHERE environments.id = sqlc.arg(environment_id)
ON CONFLICT (environment_id, name) DO UPDATE
   SET worker_group_id = EXCLUDED.worker_group_id, updated_at = clock_timestamp()
RETURNING *;
-- name: DeleteEnvironmentPlacement :execrows
DELETE FROM environment_placements
 WHERE environment_id = sqlc.arg(environment_id)
   AND name = sqlc.arg(name);
-- name: ListEnvironmentPlacements :many
SELECT *
  FROM environment_placements
 WHERE environment_id = sqlc.arg(environment_id)
 ORDER BY name COLLATE "C";
