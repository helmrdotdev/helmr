-- name: SelectProjectPlacementWorkerGroup :one
SELECT projects.org_id,
       projects.id AS project_id,
       projects.default_region_id AS region_id,
       worker_groups.id AS worker_group_id,
       worker_groups.health_state,
       worker_groups.routing_fresh_until
  FROM projects
  JOIN regions ON regions.id = projects.default_region_id
              AND regions.state = 'available'
  JOIN worker_groups ON worker_groups.region_id = projects.default_region_id
                    AND worker_groups.state = 'active'
                    AND worker_groups.health_state IN ('healthy', 'degraded')
                    AND worker_groups.routing_fresh_until > now()
 WHERE projects.org_id = sqlc.arg(org_id)
   AND projects.id = sqlc.arg(project_id)
 ORDER BY worker_groups.id ASC
 LIMIT 1;

-- name: GetWorkerGroupPlacementForRecord :one
SELECT worker_groups.id AS worker_group_id,
       worker_groups.region_id,
       worker_groups.state,
       worker_groups.health_state,
       worker_groups.routing_fresh_until
  FROM worker_groups
 WHERE worker_groups.id = sqlc.arg(worker_group_id)
   AND worker_groups.state IN ('active', 'draining')
 LIMIT 1;

-- name: ListProjectPlacementWorkerGroups :many
SELECT projects.org_id,
       projects.id AS project_id,
       projects.default_region_id AS region_id,
       worker_groups.id AS worker_group_id,
       worker_groups.state,
       worker_groups.health_state,
       worker_groups.routing_fresh_until
  FROM projects
  JOIN worker_groups ON worker_groups.region_id = projects.default_region_id
                    AND worker_groups.state IN ('active', 'draining')
 WHERE projects.org_id = sqlc.arg(org_id)
   AND projects.id = sqlc.arg(project_id)
 ORDER BY worker_groups.id;
