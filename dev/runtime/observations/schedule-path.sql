, target AS (SELECT s.* FROM agent_schedules s,input WHERE s.id=(args->>'schedule_id')::uuid),
occurrences AS (
 SELECT o.*, s.computer_id, s.deployment_id, t.status AS turn_status
 FROM agent_schedule_occurrences o JOIN target a ON a.environment_id=o.environment_id AND a.id=o.schedule_id
 LEFT JOIN sessions s ON s.environment_id=o.environment_id AND s.id=o.session_id
 LEFT JOIN turns t ON t.environment_id=o.environment_id AND t.session_id=o.session_id AND t.id=o.turn_id
 ORDER BY o.scheduled_at LIMIT 257
)
SELECT jsonb_build_object(
 'schedule', (SELECT jsonb_build_object('id',id,'agent_id',agent_id,'deployment_id',deployment_id,'trigger_key',trigger_key,'active_from',active_from,'active_until',active_until,'next_fire_at',next_fire_at,'interval_settled',active_until IS NOT NULL AND next_fire_at>=active_until) FROM target),
 'has_more', (SELECT count(*)>256 FROM occurrences),
 'occurrences', COALESCE((SELECT jsonb_agg(jsonb_build_object('scheduled_at',o.scheduled_at,'disposition',o.disposition,'reason',o.reason,'session_id',o.session_id,'turn_id',o.turn_id,'computer_id',o.computer_id,'deployment_id',o.deployment_id,'turn_status',o.turn_status) ORDER BY o.scheduled_at)
 FROM (SELECT * FROM occurrences ORDER BY scheduled_at LIMIT 256) o),'[]'::jsonb)
);
