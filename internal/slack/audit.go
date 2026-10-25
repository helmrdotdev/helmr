package slack

import (
	"context"
	"github.com/helmrdotdev/helmr/internal/access"
	"github.com/helmrdotdev/helmr/internal/audit"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/jackc/pgx/v5"
	"uuid"
)

func connectionAuditActor(ctx context.Context, tx pgx.Tx, actor audit.Actor, org, user, connection uuid.UUID, permission auth.Permission) (audit.Actor, error) {
	if actor.Kind != "human" || actor.ID != user {
		return audit.Actor{}, errInstallationAuthority
	}
	var environment, agent uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT c.environment_id,c.agent_id FROM slack_connections c JOIN environments e ON e.id=c.environment_id WHERE c.id=$1 AND e.org_id=$2`, connection, org).Scan(&environment, &agent); err != nil {
		return audit.Actor{}, err
	}
	p, err := access.Principal(ctx, tx, "user", user, environment)
	if err != nil {
		return audit.Actor{}, err
	}
	p.SessionID = actor.CredentialID
	return audit.Authorized(p, permission, auth.Scope{OrgID: org, ProjectID: p.ProjectID, EnvironmentID: environment.String(), AgentID: agent.String()})
}

func registrationAuditActor(ctx context.Context, tx pgx.Tx, actor audit.Actor, org, user, registration uuid.UUID) (audit.Actor, error) {
	var connection uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT id FROM slack_connections WHERE slack_app_registration_id=$1`, registration).Scan(&connection); err != nil {
		return audit.Actor{}, err
	}
	return connectionAuditActor(ctx, tx, actor, org, user, connection, auth.PermissionAgentConnectionsUpdateCredentials)
}
