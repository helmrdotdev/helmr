-- name: ListUserPermissionGrants :many
SELECT DISTINCT e.project_id,g.environment_id,g.agent_id,g.permission,
 COALESCE(project.slug,'')::text AS project_slug,
 COALESCE(e.slug,'')::text AS environment_slug,COALESCE(agent.key,'')::text AS agent_key
FROM org_members m
JOIN role_grant_permissions g ON g.org_id=m.org_id
LEFT JOIN environments e ON e.id=g.environment_id
LEFT JOIN projects project ON project.id=e.project_id
LEFT JOIN agents agent ON (agent.environment_id,agent.id)=(g.environment_id,g.agent_id)
WHERE m.org_id=$1 AND m.user_id=$2 AND m.removed_at IS NULL
 AND (g.all_members OR g.member_id=m.id OR EXISTS (
 SELECT 1 FROM permission_group_members gm
 WHERE (gm.org_id,gm.group_id,gm.member_id)=(m.org_id,g.group_id,m.id)))
ORDER BY e.project_id,g.environment_id,g.agent_id,g.permission;

-- name: ListAPIKeyPermissionGrants :many
SELECT DISTINCT e.project_id,g.environment_id,g.agent_id,g.permission
FROM api_keys k
JOIN role_grant_permissions g ON (g.org_id,g.environment_id,g.api_key_id)=(k.org_id,k.environment_id,k.id)
JOIN environments e ON e.id=g.environment_id
WHERE k.org_id=$1 AND k.id=$2 AND k.revoked_at IS NULL
 AND (k.expires_at IS NULL OR k.expires_at>statement_timestamp())
ORDER BY e.project_id,g.environment_id,g.agent_id,g.permission;
