-- name: DrainActiveEnvironmentCellRoutes :exec
UPDATE environment_cells
   SET route_state = 'draining',
       updated_at = now()
 WHERE org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND region_id = sqlc.arg(region_id)
   AND cell_id <> sqlc.arg(cell_id)
   AND route_state = 'active';

-- name: EnsureEnvironmentCellRoute :one
WITH active_same_cell AS MATERIALIZED (
    SELECT environment_cells.*
      FROM environment_cells
     WHERE environment_cells.org_id = sqlc.arg(org_id)
       AND environment_cells.project_id = sqlc.arg(project_id)
       AND environment_cells.environment_id = sqlc.arg(environment_id)
       AND environment_cells.region_id = sqlc.arg(region_id)
       AND environment_cells.cell_id = sqlc.arg(cell_id)
       AND environment_cells.route_state = 'active'
     ORDER BY environment_cells.route_generation DESC
     LIMIT 1
),
next_route_generation AS (
    SELECT COALESCE(max(route_generation), 0) + 1 AS route_generation
      FROM environment_cells
     WHERE environment_cells.org_id = sqlc.arg(org_id)
       AND environment_cells.project_id = sqlc.arg(project_id)
       AND environment_cells.environment_id = sqlc.arg(environment_id)
       AND environment_cells.region_id = sqlc.arg(region_id)
),
inserted AS (
INSERT INTO environment_cells (
    org_id,
    project_id,
    environment_id,
    region_id,
    cell_id,
    route_state,
    route_generation
) SELECT
    sqlc.arg(org_id),
    sqlc.arg(project_id),
    sqlc.arg(environment_id),
    sqlc.arg(region_id),
    sqlc.arg(cell_id),
    sqlc.arg(route_state)::environment_cell_route_state,
    (SELECT route_generation FROM next_route_generation)
 WHERE NOT EXISTS (SELECT 1 FROM active_same_cell)
RETURNING *
)
SELECT *
  FROM active_same_cell
UNION ALL
SELECT *
  FROM inserted
LIMIT 1;

-- name: GetActiveEnvironmentCellRoute :one
SELECT environment_cells.*
  FROM environment_cells
  JOIN cells ON cells.id = environment_cells.cell_id
            AND cells.region_id = environment_cells.region_id
  JOIN regions ON regions.id = environment_cells.region_id
 WHERE environment_cells.org_id = sqlc.arg(org_id)
   AND environment_cells.project_id = sqlc.arg(project_id)
   AND environment_cells.environment_id = sqlc.arg(environment_id)
   AND environment_cells.region_id = sqlc.arg(region_id)
   AND environment_cells.route_state = 'active'
   AND cells.state = 'active'
   AND regions.state = 'available'
   AND EXISTS (
       SELECT 1
         FROM org_cells
        WHERE org_cells.org_id = environment_cells.org_id
          AND org_cells.cell_id = environment_cells.cell_id
          AND org_cells.state = 'active'
   )
 ORDER BY environment_cells.route_generation DESC
 LIMIT 1;

-- name: GetRoutableEnvironmentCellRoute :one
SELECT environment_cells.org_id,
       environment_cells.project_id,
       environment_cells.environment_id,
       environment_cells.region_id,
       environment_cells.cell_id,
       environment_cells.route_generation,
       cell_health.state AS health_state,
       cell_health.routing_fresh_until
  FROM environment_cells
  JOIN cells ON cells.id = environment_cells.cell_id
            AND cells.region_id = environment_cells.region_id
  JOIN regions ON regions.id = environment_cells.region_id
  JOIN cell_health ON cell_health.cell_id = environment_cells.cell_id
 WHERE environment_cells.org_id = sqlc.arg(org_id)
   AND environment_cells.project_id = sqlc.arg(project_id)
   AND environment_cells.environment_id = sqlc.arg(environment_id)
   AND environment_cells.region_id = sqlc.arg(region_id)
   AND environment_cells.route_state = 'active'
   AND cells.state = 'active'
   AND regions.state = 'available'
   AND cell_health.state IN ('healthy', 'degraded')
   AND cell_health.routing_fresh_until > now()
   AND EXISTS (
       SELECT 1
         FROM org_cells
        WHERE org_cells.org_id = environment_cells.org_id
          AND org_cells.cell_id = environment_cells.cell_id
          AND org_cells.state = 'active'
   )
 ORDER BY environment_cells.route_generation DESC
 LIMIT 1;

-- name: GetEnvironmentCellRouteForRecord :one
SELECT environment_cells.org_id,
       environment_cells.project_id,
       environment_cells.environment_id,
       environment_cells.region_id,
       environment_cells.cell_id,
       environment_cells.route_state,
       environment_cells.route_generation
  FROM environment_cells
  JOIN cells ON cells.id = environment_cells.cell_id
            AND cells.region_id = environment_cells.region_id
            AND cells.state IN ('active', 'draining')
 WHERE environment_cells.org_id = sqlc.arg(org_id)
   AND environment_cells.project_id = sqlc.arg(project_id)
   AND environment_cells.environment_id = sqlc.arg(environment_id)
   AND environment_cells.cell_id = sqlc.arg(cell_id)
   AND environment_cells.route_state IN ('active', 'draining')
   AND EXISTS (
       SELECT 1
         FROM org_cells
        WHERE org_cells.org_id = environment_cells.org_id
          AND org_cells.cell_id = environment_cells.cell_id
          AND org_cells.state = 'active'
   )
 ORDER BY environment_cells.route_generation DESC
 LIMIT 1;

-- name: GetEnvironmentCellRouteForRecordGeneration :one
SELECT environment_cells.org_id,
       environment_cells.project_id,
       environment_cells.environment_id,
       environment_cells.region_id,
       environment_cells.cell_id,
       environment_cells.route_state,
       environment_cells.route_generation
  FROM environment_cells
  JOIN cells ON cells.id = environment_cells.cell_id
            AND cells.region_id = environment_cells.region_id
            AND cells.state IN ('active', 'draining')
 WHERE environment_cells.org_id = sqlc.arg(org_id)
   AND environment_cells.project_id = sqlc.arg(project_id)
   AND environment_cells.environment_id = sqlc.arg(environment_id)
   AND environment_cells.cell_id = sqlc.arg(cell_id)
   AND environment_cells.route_generation = sqlc.arg(route_generation)
   AND environment_cells.route_state IN ('active', 'draining')
   AND EXISTS (
       SELECT 1
         FROM org_cells
        WHERE org_cells.org_id = environment_cells.org_id
          AND org_cells.cell_id = environment_cells.cell_id
          AND org_cells.state = 'active'
   )
 LIMIT 1;

-- name: GetOrgCellRouteTarget :one
SELECT cells.id AS cell_id,
       cells.region_id
  FROM cells
  JOIN regions ON regions.id = cells.region_id
 WHERE cells.region_id = sqlc.arg(region_id)
   AND cells.id = sqlc.arg(cell_id)
   AND cells.state = 'active'
   AND regions.state = 'available'
   AND EXISTS (
       SELECT 1
         FROM org_cells
        WHERE org_cells.org_id = sqlc.arg(org_id)
          AND org_cells.cell_id = cells.id
          AND org_cells.state = 'active'
   )
 LIMIT 1;

-- name: ListEnvironmentCellRoutes :many
SELECT *
  FROM environment_cells
 WHERE org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
 ORDER BY region_id, route_generation DESC, cell_id;
