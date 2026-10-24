-- name: ListTriggers :many
SELECT a.id,a.agent_id,a.deployment_id,a.trigger_key,t.cron,t.timezone,t.input,a.active_from,a.active_until,a.next_fire_at,ag.key AS agent
FROM trigger_activations a
JOIN agent_triggers t ON (t.environment_id,t.agent_id,t.deployment_id,t.key)=(a.environment_id,a.agent_id,a.deployment_id,a.trigger_key)
JOIN agents ag ON (ag.environment_id,ag.id)=(a.environment_id,a.agent_id)
JOIN environments e ON e.id=a.environment_id
WHERE (sqlc.arg(all_agents)::boolean OR a.agent_id=ANY(sqlc.arg(authorized_agents)::uuid[])) AND e.org_id=sqlc.arg(org_id) AND e.project_id=sqlc.arg(project_id) AND e.id=sqlc.arg(environment_id)
AND (sqlc.narg(agent)::text IS NULL OR ag.key=sqlc.narg(agent))
AND (sqlc.narg(after_id)::uuid IS NULL OR (a.agent_id,a.id)>(sqlc.narg(after_agent_id)::uuid,sqlc.narg(after_id)::uuid))
ORDER BY a.agent_id,a.id LIMIT sqlc.arg(limit_count);

-- name: GetTriggerByID :one
SELECT a.id,a.agent_id,a.deployment_id,a.trigger_key,t.cron,t.timezone,t.input,a.active_from,a.active_until,a.next_fire_at,ag.key AS agent
FROM trigger_activations a
JOIN agent_triggers t ON (t.environment_id,t.agent_id,t.deployment_id,t.key)=(a.environment_id,a.agent_id,a.deployment_id,a.trigger_key)
JOIN agents ag ON (ag.environment_id,ag.id)=(a.environment_id,a.agent_id)
JOIN environments e ON e.id=a.environment_id
WHERE (sqlc.arg(all_agents)::boolean OR a.agent_id=ANY(sqlc.arg(authorized_agents)::uuid[])) AND e.org_id=sqlc.arg(org_id) AND e.project_id=sqlc.arg(project_id) AND e.id=sqlc.arg(environment_id) AND a.id=sqlc.arg(id);
