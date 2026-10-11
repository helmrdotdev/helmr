-- name: AppendDeploymentEvent :one
INSERT INTO telemetry_outbox(environment_id,deployment_id,stream_kind,category,severity,source,kind,message,payload,redaction_class)
SELECT d.environment_id,d.id,'event',
 COALESCE(NULLIF(sqlc.arg(category)::text,''),'system'),
 COALESCE(NULLIF(sqlc.arg(severity)::text,''),'info'),
 COALESCE(NULLIF(sqlc.arg(source)::text,''),'control'),sqlc.arg(kind)::text,
 COALESCE(sqlc.arg(message)::text,''),COALESCE(sqlc.arg(payload)::jsonb,'{}'::jsonb),
 COALESCE(NULLIF(sqlc.arg(redaction_class)::text,''),'internal')
FROM deployments d JOIN environments e ON e.id=d.environment_id
WHERE d.environment_id=sqlc.arg(environment_id) AND d.id=sqlc.arg(deployment_id)
 AND e.org_id=sqlc.arg(org_id) AND e.project_id=sqlc.arg(project_id)
RETURNING id;
