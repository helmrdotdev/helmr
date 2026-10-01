SELECT COALESCE(jsonb_agg(jsonb_build_object(
 'organization_id',p.org_id,'project_id', p.id, 'environment',e.slug, 'environment_id', e.id, 'deployment_id', d.id,
 'bundle_digest', d.bundle_digest,'runtime_artifact_digest',d.runtime_artifact_digest,'program_index_digest',encode(d.program_index_digest,'hex'),
 'finalized', d.runtime_artifact_digest ~ '^sha256:[0-9a-f]{64}$' AND octet_length(d.program_index_digest)=32,
 'definitions', (SELECT COALESCE(jsonb_agg(jsonb_build_object('id',df.id,'kind',df.kind,'declared_id',df.declared_id,'manifest_version',df.manifest_version,'manifest_digest',encode(df.manifest_digest,'hex'),'created_at',df.created_at,'computer_image_digest',(SELECT seed_digest FROM computer_specs WHERE id=df.computer_spec_id AND environment_id=df.environment_id)) ORDER BY df.kind,df.declared_id),'[]'::jsonb) FROM deployment_definitions df WHERE df.deployment_id=d.id)
) ORDER BY e.id), '[]'::jsonb)
FROM input JOIN projects p ON p.slug=args->>'project'
JOIN environments e ON e.project_id=p.id AND e.org_id=p.org_id AND e.slug=args->>'environment'
LEFT JOIN deployments d ON d.id=e.current_deployment_id AND d.environment_id=e.id;
