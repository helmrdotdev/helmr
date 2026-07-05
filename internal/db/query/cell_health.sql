-- name: UpsertCellComponentHealth :one
INSERT INTO cell_component_health (cell_id, component, state, checked_at, routing_fresh_until, details)
VALUES (
    sqlc.arg(cell_id),
    sqlc.arg(component),
    sqlc.arg(state)::cell_health_state,
    now(),
    now() + sqlc.arg(fresh_for)::interval,
    COALESCE(sqlc.arg(details)::jsonb, '{}'::jsonb)
)
ON CONFLICT (cell_id, component) DO UPDATE
   SET state = EXCLUDED.state,
       checked_at = EXCLUDED.checked_at,
       routing_fresh_until = EXCLUDED.routing_fresh_until,
       details = EXCLUDED.details
RETURNING *;

-- name: RefreshCellHealthFromComponents :one
WITH required_components AS (
    SELECT unnest(sqlc.arg(required_components)::text[]) AS component
),
component_status AS (
    SELECT required_components.component,
           cell_component_health.state,
           cell_component_health.checked_at,
           cell_component_health.routing_fresh_until,
           cell_component_health.details,
           cell_component_health.component IS NULL AS missing,
           cell_component_health.routing_fresh_until <= now() AS stale
      FROM required_components
      LEFT JOIN cell_component_health
        ON cell_component_health.cell_id = sqlc.arg(cell_id)
       AND cell_component_health.component = required_components.component
),
summary AS (
    SELECT CASE
               WHEN count(*) = 0 THEN 'unavailable'::cell_health_state
               WHEN count(*) FILTER (WHERE missing OR stale OR state = 'unavailable') > 0 THEN 'unavailable'::cell_health_state
               WHEN count(*) FILTER (WHERE state = 'degraded') > 0 THEN 'degraded'::cell_health_state
               ELSE 'healthy'::cell_health_state
           END AS state,
           CASE
               WHEN count(*) = 0 OR count(*) FILTER (WHERE missing OR stale OR state = 'unavailable') > 0 THEN now() - interval '1 second'
               ELSE min(routing_fresh_until)
           END AS routing_fresh_until,
           jsonb_object_agg(
               component,
               jsonb_build_object(
                   'state', COALESCE(state::text, 'missing'),
                   'checked_at', checked_at,
                   'routing_fresh_until', routing_fresh_until,
                   'details', COALESCE(details, '{}'::jsonb)
               )
           ) AS details
      FROM component_status
)
INSERT INTO cell_health (cell_id, state, checked_at, routing_fresh_until, details)
SELECT sqlc.arg(cell_id),
       summary.state,
       now(),
       summary.routing_fresh_until,
       jsonb_build_object('required_components', COALESCE(summary.details, '{}'::jsonb))
  FROM summary
ON CONFLICT (cell_id) DO UPDATE
   SET state = EXCLUDED.state,
       checked_at = EXCLUDED.checked_at,
       routing_fresh_until = EXCLUDED.routing_fresh_until,
       details = EXCLUDED.details
RETURNING *;

-- name: GetCellHealth :one
SELECT *
  FROM cell_health
 WHERE cell_id = sqlc.arg(cell_id);

-- name: GetCellComponentReadiness :one
SELECT cells.id AS cell_id,
       cells.state AS cell_state,
       cell_component_health.component,
       cell_component_health.state AS health_state,
       cell_component_health.checked_at,
       cell_component_health.routing_fresh_until,
       cell_component_health.details,
       (cells.state = 'active'
        AND cell_component_health.state IN ('healthy', 'degraded')
        AND cell_component_health.routing_fresh_until > now())::boolean AS ready
  FROM cells
  JOIN cell_component_health
    ON cell_component_health.cell_id = cells.id
   AND cell_component_health.component = sqlc.arg(component)
 WHERE cells.id = sqlc.arg(cell_id);

-- name: GetControlCellReadiness :one
SELECT cells.id AS cell_id,
       cells.state AS cell_state,
       cell_health.state AS health_state,
       cell_health.checked_at,
       cell_health.routing_fresh_until,
       cell_health.details,
       (cells.state = 'active'
        AND cell_health.state IN ('healthy', 'degraded')
        AND cell_health.routing_fresh_until > now())::boolean AS routable
  FROM cells
  JOIN cell_health ON cell_health.cell_id = cells.id
 WHERE cells.id = sqlc.arg(cell_id);
