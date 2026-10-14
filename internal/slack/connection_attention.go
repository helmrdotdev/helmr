package slack

import (
	"context"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5"
)

// ReauthorizationView is an Agent's Slack connection whose workspace
// authorization was lost and must be granted again by an organization manager.
type ReauthorizationView struct {
	ConnectionID  uuid.UUID
	Agent         string
	WorkspaceName *string
	LostAt        time.Time
}

// ReauthorizationRequired lists, oldest loss first and at most limit, the
// Environment's connections whose status reads reauthorization_required, with
// the complete count. Like reading a connection, it requires an organization
// owner or admin.
func ReauthorizationRequired(ctx context.Context, pool db.TxBeginner, org, user, environment uuid.UUID, limit int) ([]ReauthorizationView, int64, error) {
	views := []ReauthorizationView{}
	var count int64
	err := db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		if err := lockInstallationManager(ctx, tx, org, user); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT p.id,a.key,i.workspace_name,i.authorization_lost_at,count(*) OVER ()
 FROM slack_connections p
 JOIN environments e ON e.id=p.environment_id
 JOIN agents a ON (a.environment_id,a.id)=(p.environment_id,p.agent_id)
 JOIN slack_app_registrations r ON r.id=p.slack_app_registration_id
 JOIN slack_installations i ON i.id=p.slack_installation_id
 WHERE p.environment_id=$1 AND e.org_id=$2 AND r.org_id=$2 AND p.revoked_at IS NULL
 AND i.disconnected_at IS NULL AND r.retired_at IS NULL AND i.authorization_lost_at IS NOT NULL
 ORDER BY i.authorization_lost_at,p.id LIMIT $3`, environment, org, limit)
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
