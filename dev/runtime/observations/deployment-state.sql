SELECT COALESCE(jsonb_agg(jsonb_build_object(
 'organization_id',p.org_id,'project_id',p.id,'environment',e.slug,'environment_id',e.id,'deployment_id',d.id,
 'bundle_digest',d.bundle_digest,'execution_revoked_at',d.execution_revoked_at,
 'definitions',(SELECT COALESCE(jsonb_agg(jsonb_build_object('agent_id',a.agent_id,'definition_key',a.definition_key,'computer_definition_key',a.computer_definition_key,'setup',a.setup) ORDER BY a.definition_key),'[]'::jsonb) FROM agent_definitions a WHERE a.environment_id=d.environment_id AND a.deployment_id=d.id)
) ORDER BY e.id),'[]'::jsonb)
FROM input JOIN projects p ON p.slug=args->>'project'
JOIN environments e ON e.project_id=p.id AND e.org_id=p.org_id AND e.slug=args->>'environment'
LEFT JOIN deployments d ON d.id=e.current_deployment_id AND d.environment_id=e.id;
