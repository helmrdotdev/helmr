-- name: ListSchedules :many
SELECT s.* FROM agent_schedules s
JOIN environments e ON e.id=s.environment_id
WHERE e.org_id=sqlc.arg(org_id) AND e.project_id=sqlc.arg(project_id) AND e.id=sqlc.arg(environment_id)
AND (sqlc.narg(agent_id)::uuid IS NULL OR s.agent_id=sqlc.narg(agent_id))
AND (sqlc.narg(after_id)::uuid IS NULL OR (s.agent_id,s.id)>(sqlc.narg(after_agent_id)::uuid,sqlc.narg(after_id)::uuid))
ORDER BY s.agent_id,s.id LIMIT sqlc.arg(limit_count);

-- name: GetScheduleByID :one
SELECT s.* FROM agent_schedules s
JOIN environments e ON e.id=s.environment_id
WHERE e.org_id=sqlc.arg(org_id) AND e.project_id=sqlc.arg(project_id) AND e.id=sqlc.arg(environment_id) AND s.id=sqlc.arg(id);
