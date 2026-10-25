package org

import (
	"context"
	"errors"
	"slices"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/access"
	"github.com/helmrdotdev/helmr/internal/audit"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/jackc/pgx/v5"
)

var ErrAgentDependenciesChanged = errors.New("agent access changed during deletion; refresh and retry")

// AgentGrantCleanup owns selected-edge cleanup. Lock before Slack/Environment;
// recheck after the caller acquires the Agent FOR UPDATE fence.
type AgentGrantCleanup struct {
	manager            ManagingMember
	environment, agent uuid.UUID
	grants             []RoleGrant
	permission         auth.Permission
}

func LockAgentGrantCleanup(ctx context.Context, tx pgx.Tx, m ManagingMember, env, agent uuid.UUID, restore bool) (*AgentGrantCleanup, error) {
	permission := auth.PermissionAgentsDelete
	if restore {
		permission = auth.PermissionAgentsRestore
	}
	if err := lockActiveMembers(ctx, tx, m); err != nil {
		return nil, err
	}
	if err := authorizeAgentCleanup(ctx, tx, &m, env, agent, permission); err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, `SELECT r.id FROM roles r WHERE r.org_id=$1 AND EXISTS(SELECT 1 FROM role_grants g JOIN role_grant_agents a ON a.grant_id=g.id WHERE g.role_id=r.id AND a.environment_id=$2 AND a.agent_id=$3) ORDER BY r.id FOR UPDATE`, m.OrgID, env, agent)
	if err != nil {
		return nil, err
	}
	roles, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return nil, err
	}
	rows, err = tx.Query(ctx, `SELECT g.id FROM role_grants g JOIN role_grant_agents a ON a.grant_id=g.id WHERE g.org_id=$1 AND a.environment_id=$2 AND a.agent_id=$3 AND g.role_id=ANY($4::uuid[]) ORDER BY g.id FOR UPDATE OF g`, m.OrgID, env, agent, roles)
	if err != nil {
		return nil, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return nil, err
	}
	result := &AgentGrantCleanup{manager: m, environment: env, agent: agent, permission: permission}
	for _, id := range ids {
		grants, err := readRoleGrants(ctx, tx, m.OrgID, GrantFilter{ID: id})
		if err != nil {
			return nil, err
		}
		result.grants = append(result.grants, grants...)
	}
	return result, nil
}

func (c *AgentGrantCleanup) Recheck(ctx context.Context, tx pgx.Tx) error {
	if err := authorizeAgentCleanup(ctx, tx, &c.manager, c.environment, c.agent, c.permission); err != nil {
		return err
	}
	rows, err := tx.Query(ctx, `SELECT grant_id FROM role_grant_agents WHERE environment_id=$1 AND agent_id=$2 ORDER BY grant_id`, c.environment, c.agent)
	if err != nil {
		return err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return err
	}
	for _, id := range ids {
		if !slices.ContainsFunc(c.grants, func(g RoleGrant) bool { return g.ID == id }) {
			return ErrAgentDependenciesChanged
		}
	}
	return nil
}

func grantWithoutAgent(g RoleGrant, agent uuid.UUID) (RoleGrant, bool) {
	g.AgentIDs = slices.Clone(g.AgentIDs)
	g.AgentKeys = slices.Clone(g.AgentKeys)
	for i, id := range g.AgentIDs {
		if id == agent {
			g.AgentIDs = append(g.AgentIDs[:i], g.AgentIDs[i+1:]...)
			if i < len(g.AgentKeys) {
				g.AgentKeys = append(g.AgentKeys[:i], g.AgentKeys[i+1:]...)
			}
			break
		}
	}
	effective := len(g.AgentIDs) > 0
	for _, p := range g.Permissions {
		if !auth.IsAgentPermission(auth.Permission(p)) {
			effective = true
		}
	}
	return g, effective
}

func (c *AgentGrantCleanup) Counts() (edges, removed, mixed int) {
	for _, g := range c.grants {
		edges++
		next, effective := grantWithoutAgent(g, c.agent)
		if !effective {
			removed++
		} else if len(next.AgentIDs) == 0 {
			mixed++
		}
	}
	return
}

func (c *AgentGrantCleanup) Apply(ctx context.Context, tx pgx.Tx) error {
	if c.permission != auth.PermissionAgentsDelete {
		return access.ErrDenied
	}
	if err := c.Recheck(ctx, tx); err != nil {
		return err
	}
	for _, g := range c.grants {
		next, effective := grantWithoutAgent(g, c.agent)
		action := "role_grant.updated"
		if !effective {
			if _, err := tx.Exec(ctx, `DELETE FROM role_grants WHERE org_id=$1 AND id=$2`, c.manager.OrgID, g.ID); err != nil {
				return err
			}
			action = "role_grant.deleted"
		} else {
			if _, err := tx.Exec(ctx, `DELETE FROM role_grant_agents WHERE grant_id=$1 AND environment_id=$2 AND agent_id=$3`, g.ID, c.environment, c.agent); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE role_grants SET revision=revision+1 WHERE org_id=$1 AND id=$2`, c.manager.OrgID, g.ID); err != nil {
				return err
			}
		}
		next.Revision++
		var after = grantSnapshot(next)
		if !effective {
			after = nil
		}
		if err := audit.Append(ctx, tx, c.manager.AuditPolicy, audit.Event{Actor: c.manager.Actor, OrgID: c.manager.OrgID, EnvironmentID: c.environment, Action: action, TargetKind: "role_grant", TargetID: g.ID, Details: audit.AccessChangeDetails{Before: grantSnapshot(g), After: after, CauseAgentID: c.agent.String(), Reason: "agent_deleted"}}); err != nil {
			return err
		}
	}
	return nil
}

func authorizeAgentCleanup(ctx context.Context, tx pgx.Tx, m *ManagingMember, env, agent uuid.UUID, permission auth.Permission) error {
	current, err := access.Principal(ctx, tx, "user", m.UserID, env)
	if err != nil {
		return err
	}
	if current.OrgID != m.OrgID || current.MemberID != m.MemberID {
		return access.ErrDenied
	}
	scope := auth.Scope{OrgID: m.OrgID, ProjectID: current.ProjectID, EnvironmentID: env.String(), AgentID: agent.String()}
	current.SessionID = m.Actor.CredentialID
	actor, err := audit.Authorized(current, permission, scope)
	if err != nil {
		return access.ErrDenied
	}
	m.Actor = actor
	return nil
}
