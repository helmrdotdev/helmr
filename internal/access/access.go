// Package access reads current workload authority for operation owners. It does
// not write policy; organization operations own Roles, groups and their Grants.
package access

import (
	"context"
	"errors"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5"
)

var ErrDenied = errors.New("permission is required")

// Principal resolves a human or machine against an exact environment using the
// caller's transaction snapshot. A schedule/runtime Session is not a human.
func Principal(ctx context.Context, database db.DBTX, kind string, id, env uuid.UUID) (auth.Principal, error) {
	var p auth.Principal
	var project, org uuid.UUID
	if err := database.QueryRow(ctx, `SELECT org_id,project_id FROM environments WHERE id=$1`, env).Scan(&org, &project); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return p, ErrDenied
		}
		return p, err
	}
	p.OrgID, p.ProjectID, p.EnvironmentID = org, project.String(), env.String()
	switch kind {
	case "user":
		human, err := OrganizationPrincipal(ctx, database, org, id)
		if err != nil {
			return p, err
		}
		human.ProjectID, human.EnvironmentID = p.ProjectID, p.EnvironmentID
		return human, nil
	case "api_key":
		p.Kind, p.APIKeyID = auth.PrincipalKindAPIKey, id
		var permissions []string
		if err := database.QueryRow(ctx, `SELECT permissions FROM api_keys WHERE id=$1 AND org_id=$2 AND environment_id=$3 AND revoked_at IS NULL AND (expires_at IS NULL OR expires_at>statement_timestamp())`, id, org, env).Scan(&permissions); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return p, ErrDenied
			}
			return p, err
		}
		for _, permission := range permissions {
			if cap, ok := auth.ParseAPIKeyGrant(permission); ok {
				p.Permissions = append(p.Permissions, cap)
			}
		}
		return p, nil
	default:
		return p, ErrDenied
	}
}

// OrganizationPrincipal loads the active membership and its current additive Grants.
// Ownership is identity metadata; it never supplies ordinary permissions.
func OrganizationPrincipal(ctx context.Context, database db.DBTX, org, user uuid.UUID) (auth.Principal, error) {
	p := auth.Principal{OrgID: org, UserID: user, Kind: auth.PrincipalKindSession}
	if err := database.QueryRow(ctx, `SELECT id,is_owner FROM org_members WHERE org_id=$1 AND user_id=$2 AND removed_at IS NULL`, org, user).Scan(&p.MemberID, &p.IsOwner); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return p, ErrDenied
		}
		return p, err
	}
	rows, err := database.Query(ctx, `SELECT DISTINCT g.permission,e.project_id::text,g.environment_id::text,g.agent_id::text
      FROM role_grant_permissions g
      LEFT JOIN environments e ON e.id=g.environment_id
      WHERE g.org_id=$1 AND EXISTS (SELECT 1 FROM org_members m WHERE m.org_id=$1 AND m.id=$2 AND m.removed_at IS NULL) AND (g.all_members OR g.member_id=$2 OR EXISTS (
        SELECT 1 FROM permission_group_members gm WHERE (gm.org_id,gm.group_id,gm.member_id)=(g.org_id,g.group_id,$2)))`, org, p.MemberID)
	if err != nil {
		return p, err
	}
	defer rows.Close()
	for rows.Next() {
		var g auth.PermissionGrant
		var project, env, agent *string
		if err := rows.Scan(&g.Permission, &project, &env, &agent); err != nil {
			return p, err
		}
		if project != nil {
			g.ProjectID = *project
		}
		if env != nil {
			g.EnvironmentID = *env
		}
		if agent != nil {
			g.AgentID = *agent
		}
		p.Grants = append(p.Grants, g)
	}
	return p, rows.Err()
}

func Check(ctx context.Context, database db.DBTX, kind string, id, env, agent uuid.UUID, permission auth.Permission) error {
	p, err := Principal(ctx, database, kind, id, env)
	if err != nil {
		return err
	}
	scope := auth.Scope{OrgID: p.OrgID, ProjectID: p.ProjectID, EnvironmentID: p.EnvironmentID}
	if agent != uuid.Nil() {
		scope.AgentID = agent.String()
	}
	if !p.HasPermission(permission, scope) {
		return ErrDenied
	}
	return nil
}

// Current binds a transport identity to its still-active exact environment.
func Current(ctx context.Context, q db.DBTX, prior auth.Principal, scope auth.Scope) (auth.Principal, error) {
	if scope.EnvironmentID == "" && scope.ProjectID == "" && scope.AgentID == "" {
		if prior.Kind != auth.PrincipalKindSession || (scope.OrgID != uuid.Nil() && scope.OrgID != prior.OrgID) {
			return auth.Principal{}, ErrDenied
		}
		return OrganizationPrincipal(ctx, q, prior.OrgID, prior.UserID)
	}
	env, err := uuid.Parse(scope.EnvironmentID)
	if err != nil {
		return auth.Principal{}, ErrDenied
	}
	kind, id := "user", prior.UserID
	if prior.Kind == auth.PrincipalKindAPIKey {
		kind, id = "api_key", prior.APIKeyID
	} else if prior.Kind != auth.PrincipalKindSession {
		return auth.Principal{}, ErrDenied
	}
	p, err := Principal(ctx, q, kind, id, env)
	if err != nil {
		return p, err
	}
	if p.OrgID != prior.OrgID || p.ProjectID != scope.ProjectID || scope.OrgID != uuid.Nil() && scope.OrgID != p.OrgID {
		return auth.Principal{}, ErrDenied
	}
	return p, nil
}
