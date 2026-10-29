package slack

import (
	"context"
	"github.com/helmrdotdev/helmr/internal/access"
	"github.com/helmrdotdev/helmr/internal/audit"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/jackc/pgx/v5"
	"uuid"
)

func connectionAuditActor(ctx context.Context, tx pgx.Tx, prior auth.Principal, connection uuid.UUID, permission auth.Permission) (audit.Evidence, error) {
	org := prior.OrgID
	var environment, agent uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT c.environment_id,c.agent_id FROM slack_connections c JOIN environments e ON e.id=c.environment_id WHERE c.id=$1 AND e.org_id=$2`, connection, org).Scan(&environment, &agent); err != nil {
		return audit.Evidence{}, err
	}
	p, err := access.HumanInEnvironment(ctx, tx, prior, environment)
	if err != nil {
		return audit.Evidence{}, err
	}
	evidence, err := audit.Authorized(p, permission, auth.Scope{OrgID: org, ProjectID: p.ProjectID, EnvironmentID: environment.String(), AgentID: agent.String()})
	return evidence, err
}

func registrationAuditActor(ctx context.Context, tx pgx.Tx, prior auth.Principal, registration uuid.UUID) (audit.Evidence, error) {
	var connection uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT id FROM slack_connections WHERE slack_app_registration_id=$1`, registration).Scan(&connection); err != nil {
		return audit.Evidence{}, err
	}
	return connectionAuditActor(ctx, tx, prior, connection, auth.PermissionAgentConnectionsUpdateCredentials)
}
