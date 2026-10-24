SELECT COALESCE(jsonb_agg(jsonb_build_object(
  'valid', ARRAY(SELECT DISTINCT g.permission FROM role_grant_permissions g
    WHERE (g.org_id,g.environment_id,g.api_key_id)=(k.org_id,k.environment_id,k.id)
      AND g.agent_id IS NULL) @> ARRAY['agents.use','sessions.manage','computers.operate','deployments.publish','secrets.manage']::text[]
) ORDER BY k.id), '[]'::jsonb)
FROM input JOIN api_keys k ON k.key_prefix=args->>'key_prefix'
JOIN projects p ON p.id=k.project_id AND p.org_id=k.org_id AND p.slug=args->>'project'
JOIN environments e ON e.id=k.environment_id AND e.project_id=p.id AND e.org_id=p.org_id AND e.slug=args->>'environment'
WHERE k.revoked_at IS NULL AND (k.expires_at IS NULL OR k.expires_at>now());
