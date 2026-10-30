package slack

import (
	"context"
	"errors"
	"github.com/helmrdotdev/helmr/internal/access"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/jackc/pgx/v5"
	"uuid"
)

var errInstallationAuthority = errors.New("management of Slack installations is unavailable")

func lockAgentManager(ctx context.Context, tx pgx.Tx, prior auth.Principal, environment, agent uuid.UUID, permission auth.Permission) error {
	org := prior.OrgID
	switch permission {
	case auth.PermissionAgentConnectionsRead, auth.PermissionAgentConnectionsConfigure, auth.PermissionAgentConnectionsDelete, auth.PermissionAgentsDelete:
	default:
		return errInstallationAuthority
	}
	p, err := access.HumanInEnvironment(ctx, tx, prior, environment)
	if errors.Is(err, access.ErrDenied) {
		return errInstallationAuthority
	}
	if err != nil {
		return err
	}
	if p.OrgID != org || !p.HasPermission(permission, auth.Scope{OrgID: org, ProjectID: p.ProjectID, EnvironmentID: environment.String(), AgentID: agent.String()}) {
		return errInstallationAuthority
	}
	return auth.RequireRecentFor(p, permission)
}

func lockConnectionManager(ctx context.Context, tx pgx.Tx, prior auth.Principal, connection uuid.UUID, permission auth.Permission) error {
	org := prior.OrgID
	var environment, agent uuid.UUID
	err := tx.QueryRow(ctx, `SELECT c.environment_id,c.agent_id FROM slack_connections c JOIN environments e ON e.id=c.environment_id WHERE c.id=$1 AND e.org_id=$2`, connection, org).Scan(&environment, &agent)
	if errors.Is(err, pgx.ErrNoRows) {
		return errInstallationAuthority
	}
	if err != nil {
		return err
	}
	return lockAgentManager(ctx, tx, prior, environment, agent, permission)
}

func lockRegistrationManager(ctx context.Context, tx pgx.Tx, prior auth.Principal, registration uuid.UUID, permission auth.Permission) error {
	org := prior.OrgID
	var connection uuid.UUID
	err := tx.QueryRow(ctx, `SELECT c.id FROM slack_connections c JOIN environments e ON e.id=c.environment_id WHERE c.slack_app_registration_id=$1 AND e.org_id=$2`, registration, org).Scan(&connection)
	if errors.Is(err, pgx.ErrNoRows) {
		return errInstallationAuthority
	}
	if err != nil {
		return err
	}
	return lockConnectionManager(ctx, tx, prior, connection, permission)
}
