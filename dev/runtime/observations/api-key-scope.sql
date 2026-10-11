SELECT COALESCE(jsonb_agg(jsonb_build_object('role', k.role, 'valid', k.role IN ('owner','admin') AND k.permissions @> ARRAY['agents.start','asks.respond','computer.exec.create','computers.create','computers.delete','computers.read','deployments.write','secrets.write','sessions.cancel','sessions.close','sessions.interrupt','sessions.read','sessions.resume','sessions.send']::text[]) ORDER BY k.id), '[]'::jsonb)
FROM input JOIN api_keys k ON k.key_prefix=args->>'key_prefix'
JOIN projects p ON p.id=k.project_id AND p.org_id=k.org_id AND p.slug=args->>'project'
JOIN environments e ON e.id=k.environment_id AND e.project_id=p.id AND e.org_id=p.org_id AND e.slug=args->>'environment'
WHERE k.revoked_at IS NULL AND (k.expires_at IS NULL OR k.expires_at>now());
