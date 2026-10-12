-- name: ListSchedules :many
SELECT sqlc.embed(s), a.key AS agent FROM agent_schedules s
JOIN agents a ON (a.environment_id,a.id)=(s.environment_id,s.agent_id)
JOIN environments e ON e.id=s.environment_id
WHERE e.org_id=sqlc.arg(org_id) AND e.project_id=sqlc.arg(project_id) AND e.id=sqlc.arg(environment_id)
AND (sqlc.narg(agent_id)::uuid IS NULL OR s.agent_id=sqlc.narg(agent_id))
AND (sqlc.narg(agent)::text IS NULL OR a.key=sqlc.narg(agent))
AND (sqlc.narg(after_id)::uuid IS NULL OR (s.agent_id,s.id)>(sqlc.narg(after_agent_id)::uuid,sqlc.narg(after_id)::uuid))
ORDER BY s.agent_id,s.id LIMIT sqlc.arg(limit_count);

-- name: GetScheduleByID :one
SELECT sqlc.embed(s), a.key AS agent FROM agent_schedules s
JOIN agents a ON (a.environment_id,a.id)=(s.environment_id,s.agent_id)
JOIN environments e ON e.id=s.environment_id
WHERE e.org_id=sqlc.arg(org_id) AND e.project_id=sqlc.arg(project_id) AND e.id=sqlc.arg(environment_id) AND s.id=sqlc.arg(id);
