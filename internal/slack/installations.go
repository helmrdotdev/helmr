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

func lockSlackMember(ctx context.Context, tx pgx.Tx, org, user uuid.UUID) error {
	var member uuid.UUID
	err := tx.QueryRow(ctx, `SELECT id FROM org_members WHERE org_id=$1 AND user_id=$2 AND removed_at IS NULL FOR SHARE`, org, user).Scan(&member)
	if errors.Is(err, pgx.ErrNoRows) {
		return errInstallationAuthority
	}
	return err
}

func lockAgentManager(ctx context.Context, tx pgx.Tx, org, user, environment, agent uuid.UUID) error {
	if err := lockSlackMember(ctx, tx, org, user); err != nil {
		return err
	}
	p, err := access.Principal(ctx, tx, "user", user, environment)
	if errors.Is(err, access.ErrDenied) {
		return errInstallationAuthority
	}
	if err != nil {
		return err
	}
	if p.OrgID != org || !p.HasPermission(auth.CapabilityAgentsManage, auth.Scope{OrgID: org, ProjectID: p.ProjectID, EnvironmentID: environment.String(), AgentID: agent.String()}) {
		return errInstallationAuthority
	}
	return nil
}

func lockConnectionManager(ctx context.Context, tx pgx.Tx, org, user, connection uuid.UUID) error {
	var environment, agent uuid.UUID
	err := tx.QueryRow(ctx, `SELECT c.environment_id,c.agent_id FROM slack_connections c JOIN environments e ON e.id=c.environment_id WHERE c.id=$1 AND e.org_id=$2`, connection, org).Scan(&environment, &agent)
	if errors.Is(err, pgx.ErrNoRows) {
		return errInstallationAuthority
	}
	if err != nil {
		return err
	}
	return lockAgentManager(ctx, tx, org, user, environment, agent)
}

func lockRegistrationManager(ctx context.Context, tx pgx.Tx, org, user, registration uuid.UUID) error {
	var connection uuid.UUID
	err := tx.QueryRow(ctx, `SELECT c.id FROM slack_connections c JOIN environments e ON e.id=c.environment_id WHERE c.slack_app_registration_id=$1 AND e.org_id=$2`, registration, org).Scan(&connection)
	if errors.Is(err, pgx.ErrNoRows) {
		return errInstallationAuthority
	}
	if err != nil {
		return err
	}
	return lockConnectionManager(ctx, tx, org, user, connection)
}
