-- name: ListTriggers :many
SELECT a.id,a.agent_id,a.deployment_id,a.trigger_key,t.cron,t.timezone,t.input,a.active_from,a.active_until,a.next_fire_at,ag.key AS agent
FROM trigger_activations a
JOIN agent_triggers t ON (t.environment_id,t.agent_id,t.deployment_id,t.key)=(a.environment_id,a.agent_id,a.deployment_id,a.trigger_key)
JOIN agents ag ON (ag.environment_id,ag.id)=(a.environment_id,a.agent_id)
JOIN environments e ON e.id=a.environment_id
WHERE (sqlc.arg(all_agents)::boolean OR a.agent_id=ANY(sqlc.arg(authorized_agents)::uuid[])) AND e.org_id=sqlc.arg(org_id) AND e.project_id=sqlc.arg(project_id) AND e.id=sqlc.arg(environment_id)
AND (sqlc.narg(agent)::text IS NULL OR ag.key=sqlc.narg(agent))
AND (sqlc.arg(include_history)::boolean OR a.active_until IS NULL)
AND (sqlc.narg(trigger_key)::text IS NULL OR a.trigger_key=sqlc.narg(trigger_key))
AND (sqlc.narg(after_id)::uuid IS NULL OR (a.agent_id,a.id)>(sqlc.narg(after_agent_id)::uuid,sqlc.narg(after_id)::uuid))
ORDER BY a.agent_id,a.id LIMIT sqlc.arg(limit_count);

-- name: GetTriggerByID :one
SELECT a.id,a.agent_id,a.deployment_id,a.trigger_key,t.cron,t.timezone,t.input,a.active_from,a.active_until,a.next_fire_at,ag.key AS agent
FROM trigger_activations a
JOIN agent_triggers t ON (t.environment_id,t.agent_id,t.deployment_id,t.key)=(a.environment_id,a.agent_id,a.deployment_id,a.trigger_key)
JOIN agents ag ON (ag.environment_id,ag.id)=(a.environment_id,a.agent_id)
JOIN environments e ON e.id=a.environment_id
WHERE (sqlc.arg(all_agents)::boolean OR a.agent_id=ANY(sqlc.arg(authorized_agents)::uuid[])) AND e.org_id=sqlc.arg(org_id) AND e.project_id=sqlc.arg(project_id) AND e.id=sqlc.arg(environment_id) AND a.id=sqlc.arg(id);

-- name: ListTriggerOccurrences :many
SELECT o.scheduled_at,o.disposition,o.reason,o.session_id,o.turn_id,o.evaluated_at
FROM trigger_occurrences o JOIN trigger_activations a ON (a.environment_id,a.id)=(o.environment_id,o.activation_id)
JOIN environments e ON e.id=o.environment_id
WHERE e.org_id=sqlc.arg(org_id) AND e.project_id=sqlc.arg(project_id) AND e.id=sqlc.arg(environment_id) AND a.id=sqlc.arg(activation_id)
AND (sqlc.arg(all_agents)::boolean OR a.agent_id=ANY(sqlc.arg(authorized_agents)::uuid[]))
AND (sqlc.narg(after_at)::timestamptz IS NULL OR o.scheduled_at>sqlc.narg(after_at)::timestamptz)
ORDER BY o.scheduled_at LIMIT sqlc.arg(limit_count);
