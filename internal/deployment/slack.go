package deployment

import (
	"context"
	"errors"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/slackroute"

	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5"
)

type triggerRouteKey struct {
	agent   uuid.UUID
	channel string
}

// triggerRoutes holds the Slack destinations of the promoted triggers. A
// destination whose Agent has no eligible connection is waiting: its
// activation opens without a channel binding and is bound when a connection
// for the Agent is authorized.
type triggerRoutes struct {
	connected map[triggerRouteKey]slackroute.Connection
	waiting   map[triggerRouteKey]bool
	// configured is false when Slack is not configured; every destination waits.
	configured bool
}

// Promotion verifies authored channel IDs of connected Agents before locking
// the Environment. The resolved connection generations, not current defaults
// at fire time, are bound.
func prepareTriggerRoutes(ctx context.Context, pool db.TxBeginner, principal auth.Principal, env, project, deployment uuid.UUID, preflight slackroute.StartPreflight) (triggerRoutes, error) {
	routes := triggerRoutes{connected: map[triggerRouteKey]slackroute.Connection{}, waiting: map[triggerRouteKey]bool{}, configured: preflight != nil}
	err := db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		var allowed bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM deployments d JOIN environments e ON e.id=d.environment_id WHERE e.id=$1 AND e.project_id=$2 AND e.org_id=$3 AND d.id=$4)`, env, project, principal.OrgID, deployment).Scan(&allowed); err != nil {
			return err
		}
		if !allowed {
			return ErrNotDeployable
		}
		rows, err := tx.Query(ctx, `SELECT DISTINCT agent_id,slack_channel_id FROM agent_triggers WHERE environment_id=$1 AND deployment_id=$2 AND slack_channel_id IS NOT NULL ORDER BY agent_id,slack_channel_id`, env, deployment)
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
			if !routes.configured {
				routes.waiting[value] = true
				continue
			}
			connection, err := slackroute.ReadConnection(ctx, tx, env, value.agent, value.channel)
			if errors.Is(err, slackroute.ErrTargetNotPublished) {
				routes.waiting[value] = true
				continue
			}
			if err != nil {
				return err
			}
			routes.connected[value] = connection
		}
		return nil
	})
	if err != nil {
		return triggerRoutes{}, err
	}
	// A live connection that cannot use the channel fails promotion: no later
	// event reports that the channel became usable.
	for _, connection := range routes.connected {
		if _, err = preflight.PrepareStart(ctx, slackroute.StartContext{Route: slackroute.StartRoute{Target: connection, DeploymentID: deployment}}); err != nil {
			return triggerRoutes{}, err
		}
	}
	return routes, nil
}

// pinTriggerRoutes resolves the channel bindings of connected destinations and
// confirms that waiting destinations still have no eligible connection.
func pinTriggerRoutes(ctx context.Context, tx pgx.Tx, env uuid.UUID, routes triggerRoutes) (map[triggerRouteKey]uuid.UUID, error) {
	pubs := []uuid.UUID{}
	for _, connection := range routes.connected {
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
	for key, expected := range routes.connected {
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

// confirmWaitingRoutes runs under the Environment lock, which also serializes
// connection authorization. A connection authorized after preflight would
// otherwise miss the activations this promotion opens.
func confirmWaitingRoutes(ctx context.Context, tx pgx.Tx, env uuid.UUID, routes triggerRoutes) error {
	if !routes.configured {
		return nil
	}
	for key := range routes.waiting {
		_, err := slackroute.ReadConnection(ctx, tx, env, key.agent, key.channel)
		if errors.Is(err, slackroute.ErrTargetNotPublished) {
			continue
		}
		if err != nil {
			return err
		}
		return slackroute.ErrConversationChanged
	}
	return nil
}
