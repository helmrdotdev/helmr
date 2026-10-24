package access

import (
	"context"
	"errors"
	"sort"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5"
)

var ErrNotFound = errors.New("member or target not found")

type MemberPermissionFilter struct{ Project, Environment, Agent string }

// MemberPermissions projects every source from a single snapshot. It is not
// a promise that a later operation will pass origin or runtime admission checks.
func MemberPermissions(ctx context.Context, database db.TxBeginner, caller auth.Principal, member uuid.UUID, filter MemberPermissionFilter) (api.MemberPermissionsResponse, error) {
	result := api.MemberPermissionsResponse{MembershipID: member.String(), Scopes: []api.MemberPermissionScope{}, Notice: "Current additive access. Session origin, credential validity and runtime admission are checked separately at execution."}
	if caller.Kind != auth.PrincipalKindSession {
		return result, ErrDenied
	}
	err := db.RunTx(ctx, database, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SET TRANSACTION ISOLATION LEVEL REPEATABLE READ, READ ONLY`); err != nil {
			return err
		}
		actor, err := OrganizationPrincipal(ctx, tx, caller.OrgID, caller.UserID)
		if err != nil {
			return err
		}
		admin := actor.HasPermission(auth.CapabilityAccessManage, auth.Scope{OrgID: actor.OrgID})
		if actor.MemberID != member && !admin {
			return ErrDenied
		}
		var user uuid.UUID
		var active bool
		if err := tx.QueryRow(ctx, `SELECT user_id,is_owner,removed_at IS NULL FROM org_members WHERE org_id=$1 AND id=$2`, caller.OrgID, member).Scan(&user, &result.IsOwner, &active); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		result.UserID = user.String()
		result.Status = "active"
		if !active {
			result.Status = "removed"
			return nil
		}
		type source struct {
			project, environment, agent string
			data                        api.MemberPermissionSource
		}
		sources := []source{}
		rows, err := tx.Query(ctx, `SELECT COALESCE(p.slug,''),COALESCE(e.slug,''),COALESCE(a.key,''),g.id::text,
  CASE WHEN g.all_members THEN 'all_members' WHEN g.member_id IS NOT NULL THEN 'member' ELSE 'group' END,
  COALESCE(g.member_id::text,''),COALESCE(g.group_id::text,''),COALESCE(pg.name,''),r.id::text,r.name,
  COALESCE(g.agent_id::text,''),CASE WHEN g.environment_id IS NULL THEN 'organization' WHEN g.agent_id IS NULL THEN 'environment' ELSE 'agent' END,
  ARRAY[g.permission]
  FROM role_grant_permissions g JOIN roles r ON (r.org_id,r.id)=(g.org_id,g.role_id)
  LEFT JOIN environments e ON e.id=g.environment_id LEFT JOIN projects p ON p.id=e.project_id
  LEFT JOIN agents a ON (a.environment_id,a.id)=(g.environment_id,g.agent_id)
  LEFT JOIN permission_groups pg ON (pg.org_id,pg.id)=(g.org_id,g.group_id)
  WHERE g.org_id=$1 AND (g.all_members OR g.member_id=$2 OR EXISTS (
   SELECT 1 FROM permission_group_members gm WHERE (gm.org_id,gm.group_id,gm.member_id)=(g.org_id,g.group_id,$2)))
  AND ($3='' OR p.slug=$3) AND ($4='' OR e.slug=$4)
  AND ($5='' OR a.key=$5 OR (g.environment_id IS NOT NULL AND g.agent_id IS NULL)) ORDER BY g.id`, caller.OrgID, member, filter.Project, filter.Environment, filter.Agent)
		if err != nil {
			return err
		}
		for rows.Next() {
			var s source
			s.data.Kind = "role_grant"
			if err := rows.Scan(&s.project, &s.environment, &s.agent, &s.data.GrantID, &s.data.SubjectKind, &s.data.MemberID, &s.data.GroupID, &s.data.GroupName, &s.data.RoleID, &s.data.RoleName, &s.data.AgentID, &s.data.TargetKind, &s.data.Permissions); err != nil {
				rows.Close()
				return err
			}
			s.data.Agent = s.agent
			caps := map[string]bool{}
			for _, p := range s.data.Permissions {
				for _, c := range auth.IncludedCapabilities(auth.Permission(p)) {
					caps[string(c)] = true
				}
			}
			s.data.IncludedPermissions = []string{}
			for c := range caps {
				s.data.IncludedPermissions = append(s.data.IncludedPermissions, c)
			}
			sort.Strings(s.data.IncludedPermissions)
			sources = append(sources, s)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		// Requested Agent identity is disclosed only through an applicable Grant or
		// the access-administration directory, never through ownership alone.
		if filter.Agent != "" {
			visible := admin
			for _, s := range sources {
				if s.agent == filter.Agent {
					visible = true
				}
				if s.environment != "" && s.agent == "" {
					for _, permission := range s.data.IncludedPermissions {
						if auth.CapabilityResourceType(auth.Permission(permission)) == "agents" {
							visible = true
						}
					}
				}
			}
			if !visible {
				return ErrNotFound
			}
			var exists bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM agents a JOIN environments e ON e.id=a.environment_id JOIN projects p ON p.id=e.project_id WHERE e.org_id=$1 AND p.slug=$2 AND e.slug=$3 AND a.key=$4)`, caller.OrgID, filter.Project, filter.Environment, filter.Agent).Scan(&exists); err != nil {
				return err
			}
			if !exists {
				return ErrNotFound
			}
		}
		type target struct{ project, environment, agent string }
		targets := map[target]bool{}
		for _, s := range sources {
			agent := s.agent
			if filter.Agent != "" {
				agent = filter.Agent
			}
			targets[target{s.project, s.environment, agent}] = true
		}
		for t := range targets {
			item := api.MemberPermissionScope{Type: "environment", Project: t.project, Environment: t.environment, Agent: t.agent, Permissions: []string{}, Sources: []api.MemberPermissionSource{}}
			if t.environment == "" {
				item.Type = "organization"
			} else if t.agent != "" {
				item.Type = "agent"
			}
			caps := map[string]bool{}
			for _, s := range sources {
				if s.project != t.project || s.environment != t.environment || s.agent != "" && s.agent != t.agent {
					continue
				}
				item.Sources = append(item.Sources, s.data)
				for _, c := range s.data.IncludedPermissions {
					caps[c] = true
				}
			}
			for c := range caps {
				item.Permissions = append(item.Permissions, c)
			}
			sort.Strings(item.Permissions)
			result.Scopes = append(result.Scopes, item)
		}
		sort.Slice(result.Scopes, func(i, j int) bool {
			a, b := result.Scopes[i], result.Scopes[j]
			if a.Project != b.Project {
				return a.Project < b.Project
			}
			if a.Environment != b.Environment {
				return a.Environment < b.Environment
			}
			return a.Agent < b.Agent
		})
		return nil
	})
	return result, err
}
