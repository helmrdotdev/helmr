package slack

import (
	"context"
	"github.com/helmrdotdev/helmr/internal/access"
	"github.com/helmrdotdev/helmr/internal/auth"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5"
)

// ReauthorizationView is an Agent's Slack connection whose workspace
// authorization was lost and must be granted again by an Agent manager.
type ReauthorizationView struct {
	ConnectionID  uuid.UUID
	Agent         string
	WorkspaceName *string
	LostAt        time.Time
}

// ReauthorizationRequired lists, oldest loss first and at most limit, the
// Environment's connections whose status reads reauthorization_required, with
// the complete count, filtered to connections the caller can manage.
func ReauthorizationRequired(ctx context.Context, pool db.TxBeginner, org, user, environment uuid.UUID, limit int) ([]ReauthorizationView, int64, error) {
	views := []ReauthorizationView{}
	var count int64
	err := db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		all, agents, err := reauthorizationScope(ctx, tx, org, user, environment)
		if err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT p.id,a.key,i.workspace_name,i.authorization_lost_at,count(*) OVER ()`+reauthorizationFromSQL+`
 ORDER BY i.authorization_lost_at,p.id LIMIT $5`, environment, org, all, agents, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var view ReauthorizationView
			if err := rows.Scan(&view.ConnectionID, &view.Agent, &view.WorkspaceName, &view.LostAt, &count); err != nil {
				return err
			}
			views = append(views, view)
		}
		return rows.Err()
	})
	return views, count, err
}

const reauthorizationFromSQL = `
 FROM slack_connections p
 JOIN environments e ON e.id=p.environment_id
 JOIN agents a ON (a.environment_id,a.id)=(p.environment_id,p.agent_id)
 JOIN slack_app_registrations r ON r.id=p.slack_app_registration_id
 JOIN slack_installations i ON i.id=p.slack_installation_id
 WHERE p.environment_id=$1 AND e.org_id=$2 AND r.org_id=$2 AND p.revoked_at IS NULL
 AND ($3 OR p.agent_id::text=ANY($4::text[]))
 AND i.disconnected_at IS NULL AND r.retired_at IS NULL AND i.authorization_lost_at IS NOT NULL`

func reauthorizationScope(ctx context.Context, tx pgx.Tx, org, user, environment uuid.UUID) (bool, []string, error) {
	if err := lockSlackMember(ctx, tx, org, user); err != nil {
		return false, nil, err
	}
	principal, err := access.Principal(ctx, tx, "user", user, environment)
	if err != nil {
		return false, nil, err
	}
	if principal.OrgID != org {
		return false, nil, errInstallationAuthority
	}
	all := false
	agents := []string{}
	for _, grant := range principal.Grants {
		if grant.EnvironmentID == environment.String() && grant.Permission == auth.PermissionAgentConnectionsRead {
			if grant.AgentID == "" {
				all = true
			} else {
				agents = append(agents, grant.AgentID)
			}
		}
	}
	if !all && len(agents) == 0 {
		return false, nil, errInstallationAuthority
	}
	return all, agents, nil
}

// CountReauthorizationRequired selects metadata only under the same current authority.
func CountReauthorizationRequired(ctx context.Context, pool db.TxBeginner, org, user, environment uuid.UUID) (int64, error) {
	var count int64
	err := db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		all, agents, err := reauthorizationScope(ctx, tx, org, user, environment)
		if err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT count(*)`+reauthorizationFromSQL, environment, org, all, agents).Scan(&count)
	})
	return count, err
}
