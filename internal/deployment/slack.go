package deployment

import (
	"context"
	"errors"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/slackroute"

	"github.com/helmrdotdev/helmr/internal/agent"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5"
)

type triggerRouteKey struct {
	agent   uuid.UUID
	channel string
}

// Promotion verifies authored channel IDs before locking the Environment. The
// resolved connection generations, not current defaults at fire time, are pinned.
func prepareTriggerRoutes(ctx context.Context, pool db.TxBeginner, principal auth.Principal, env, project, deployment uuid.UUID, preflight slackroute.StartPreflight) (map[triggerRouteKey]slackroute.Connection, error) {
	routes := map[triggerRouteKey]slackroute.Connection{}
	err := db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		var allowed bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM deployments d JOIN environments e ON e.id=d.environment_id WHERE e.id=$1 AND e.project_id=$2 AND e.org_id=$3 AND e.retired_at IS NULL AND d.id=$4 AND d.execution_revoked_at IS NULL)`, env, project, principal.OrgID, deployment).Scan(&allowed); err != nil {
			return err
		}
		if !allowed {
			return ErrNotDeployable
		}
		rows, err := tx.Query(ctx, `SELECT DISTINCT agent_id,slack_destination FROM agent_triggers WHERE environment_id=$1 AND deployment_id=$2 AND slack_destination IS NOT NULL ORDER BY agent_id,slack_destination`, env, deployment)
		if err != nil {
			return err
		}
		values, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (triggerRouteKey, error) {
			var a triggerRouteKey
			err := row.Scan(&a.agent, &a.channel)
			return a, err
		})
		if err != nil {
			return err
		}
		for _, value := range values {
			connection, err := slackroute.ReadConnection(ctx, tx, env, value.agent, value.channel)
			if err != nil {
				return err
			}
			routes[value] = connection
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	for _, connection := range routes {
		if preflight == nil {
			return nil, agent.ErrSlackChannelUnavailable
		}
		if _, err = preflight.PrepareStart(ctx, slackroute.StartContext{Route: slackroute.StartRoute{Target: connection, DeploymentID: deployment}}); err != nil {
			return nil, err
		}
	}
	return routes, nil
}

func pinTriggerRoutes(ctx context.Context, tx pgx.Tx, env uuid.UUID, routes map[triggerRouteKey]slackroute.Connection) (map[triggerRouteKey]uuid.UUID, error) {
	pubs := []uuid.UUID{}
	for _, connection := range routes {
		pubs = append(pubs, connection.ConnectionID)
	}
	active, err := slackroute.LockConnections(ctx, tx, env, pubs)
	if err != nil {
		return nil, err
	}
	if !active {
		return nil, slackroute.ErrConversationChanged
	}
	pins := map[triggerRouteKey]uuid.UUID{}
	for key, expected := range routes {
		current, err := slackroute.ReadConnection(ctx, tx, env, key.agent, key.channel)
		if errors.Is(err, slackroute.ErrTargetNotPublished) {
			return nil, slackroute.ErrConversationChanged
		}
		if err != nil {
			return nil, err
		}
		if current.ConnectionID != expected.ConnectionID || current.InstallationID != expected.InstallationID || !current.AuthorizedAt.Equal(expected.AuthorizedAt) {
			return nil, slackroute.ErrConversationChanged
		}
		id, err := slackroute.ResolveChannelBinding(ctx, tx, current)
		if err != nil {
			return nil, err
		}
		pins[key] = id
	}
	return pins, nil
}
