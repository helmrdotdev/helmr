SELECT COALESCE(jsonb_agg(jsonb_build_object(
 'org_id',p.org_id,'project_id',p.id,'environment',e.slug,'environment_id',e.id,'deployment_id',d.id,
 'bundle_digest',d.bundle_digest,'execution_revoked_at',d.execution_revoked_at,
 'definitions',(SELECT COALESCE(jsonb_agg(jsonb_build_object('agent_id',a.agent_id,'agent',identity.key,'template',template.key,'has_init',a.has_init) ORDER BY identity.key),'[]'::jsonb) FROM agent_definitions a JOIN agents identity ON (identity.environment_id,identity.id)=(a.environment_id,a.agent_id) JOIN templates template ON (template.environment_id,template.id)=(a.environment_id,a.template_id) WHERE a.environment_id=d.environment_id AND a.deployment_id=d.id)
) ORDER BY e.id),'[]'::jsonb)
FROM input JOIN projects p ON p.slug=args->>'project'
JOIN environments e ON e.project_id=p.id AND e.org_id=p.org_id AND e.slug=args->>'environment'
LEFT JOIN deployments d ON d.id=e.current_deployment_id AND d.environment_id=e.id;
