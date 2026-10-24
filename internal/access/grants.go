package access

import (
	"context"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
)

// grantPermissionsSQL expands current Role permissions without copying the
// catalog's resource classification into the database schema.
const grantPermissionsSQL = `SELECT g.id,g.org_id,g.role_id,g.member_id,g.group_id,g.api_key_id,g.all_members,g.environment_id,
 a.agent_id,p.permission
 FROM role_grants g JOIN role_permissions p ON (p.org_id,p.role_id)=(g.org_id,g.role_id)
 LEFT JOIN role_grant_agents a ON a.grant_id=g.id
  AND p.permission=ANY($1::text[]) AND g.agents_mode='selected'
 WHERE NOT (p.permission=ANY($1::text[]))
  OR g.agents_mode='all' OR (g.agents_mode='selected' AND a.agent_id IS NOT NULL)`

// MemberPermissionGrantsSQL reads one active membership's direct, Group and
// all-members assignments. Hosts bind $1 to auth.CapabilityIDsForResource("agents"),
// $2 to organization ID and $3 to exact membership ID; host parameters start at $4.
// Embed this relation when authorization must precede SQL pagination or aggregation.
const MemberPermissionGrantsSQL = `SELECT g.* FROM (` + grantPermissionsSQL + `) g
 JOIN org_members m ON m.org_id=g.org_id AND m.id=$3 AND m.removed_at IS NULL
 WHERE g.org_id=$2 AND (g.all_members OR g.member_id=m.id OR EXISTS (
 SELECT 1 FROM permission_group_members gm
 WHERE (gm.org_id,gm.group_id,gm.member_id)=(m.org_id,g.group_id,m.id)))`

type MemberGrant struct {
	auth.PermissionGrant
	ProjectSlug     string
	EnvironmentSlug string
	AgentKey        string
}

// MemberGrants reads current authority and display names on the caller's snapshot.
func MemberGrants(ctx context.Context, database db.DBTX, org, member uuid.UUID) ([]MemberGrant, error) {
	rows, err := database.Query(ctx, `SELECT DISTINCT g.permission,COALESCE(e.project_id::text,''),
 COALESCE(g.environment_id::text,''),COALESCE(g.agent_id::text,''),
 COALESCE(p.slug,''),COALESCE(e.slug,''),COALESCE(a.key,'')
 FROM (`+MemberPermissionGrantsSQL+`) g
 LEFT JOIN environments e ON e.id=g.environment_id LEFT JOIN projects p ON p.id=e.project_id
 LEFT JOIN agents a ON (a.environment_id,a.id)=(g.environment_id,g.agent_id)
 ORDER BY 2,3,4,1`, auth.CapabilityIDsForResource("agents"), org, member)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	grants := []MemberGrant{}
	for rows.Next() {
		var g MemberGrant
		if err := rows.Scan(&g.Permission, &g.ProjectID, &g.EnvironmentID, &g.AgentID, &g.ProjectSlug, &g.EnvironmentSlug, &g.AgentKey); err != nil {
			return nil, err
		}
		grants = append(grants, g)
	}
	return grants, rows.Err()
}

// APIKeyGrants reads only this valid key's assignments. Human audiences never
// apply to machines; eligibility is enforced when policy is written.
func APIKeyGrants(ctx context.Context, database db.DBTX, org, key uuid.UUID) ([]auth.PermissionGrant, error) {
	rows, err := database.Query(ctx, `SELECT DISTINCT g.permission,e.project_id::text,g.environment_id::text,COALESCE(g.agent_id::text,'')
 FROM (`+grantPermissionsSQL+`) g
 JOIN api_keys k ON (k.org_id,k.environment_id,k.id)=(g.org_id,g.environment_id,g.api_key_id)
 JOIN environments e ON e.id=g.environment_id
 WHERE k.org_id=$2 AND k.id=$3 AND k.revoked_at IS NULL
 AND (k.expires_at IS NULL OR k.expires_at>statement_timestamp()) ORDER BY 2,3,4,1`, auth.CapabilityIDsForResource("agents"), org, key)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	grants := []auth.PermissionGrant{}
	for rows.Next() {
		var g auth.PermissionGrant
		if err := rows.Scan(&g.Permission, &g.ProjectID, &g.EnvironmentID, &g.AgentID); err != nil {
			return nil, err
		}
		grants = append(grants, g)
	}
	return grants, rows.Err()
}
