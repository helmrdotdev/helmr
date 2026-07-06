-- name: EnsureDefaultWorkerGroup :one
INSERT INTO worker_groups (id, region_id, name, description)
VALUES (sqlc.arg(id), sqlc.arg(region_id), 'default', 'Default worker group')
ON CONFLICT (region_id, name) DO UPDATE
   SET description = worker_groups.description
RETURNING *;

-- name: ListWorkerGroups :many
SELECT *
  FROM worker_groups
 WHERE region_id = sqlc.arg(region_id)
 ORDER BY name ASC
 LIMIT sqlc.arg(row_limit);

-- name: ReportWorkerGroupHealth :one
UPDATE worker_groups
   SET health_state = sqlc.arg(health_state)::worker_group_health_state,
       health_checked_at = now(),
       routing_fresh_until = now() + sqlc.arg(fresh_for)::interval,
       health_details = sqlc.arg(health_details)::jsonb
 WHERE id = sqlc.arg(worker_group_id)::text
RETURNING *;

-- name: GetControlWorkerGroupReadiness :one
SELECT id AS worker_group_id,
       state,
       health_state,
       routing_fresh_until,
       (
           state = 'active'
           AND health_state IN ('healthy', 'degraded')
           AND routing_fresh_until > now()
       ) AS routable
  FROM worker_groups
 WHERE id = sqlc.arg(worker_group_id);
