package deployment

import (
	"context"
	"encoding/json"
	"errors"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/agent"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/definition"
	"github.com/jackc/pgx/v5"
)

type scheduleRouteKey struct {
	agent   uuid.UUID
	channel string
}

// Promotion verifies authored channel IDs before locking the Environment. The
// resolved connection generations, not current defaults at fire time, are pinned.
func prepareScheduleRoutes(ctx context.Context, pool db.TxBeginner, principal auth.Principal, env, project, deployment uuid.UUID, preflight agent.SlackStartPreflight) (map[scheduleRouteKey]agent.SlackConnection, error) {
	routes := map[scheduleRouteKey]agent.SlackConnection{}
	err := db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		var allowed bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM deployments d JOIN environments e ON e.id=d.environment_id WHERE e.id=$1 AND e.project_id=$2 AND e.org_id=$3 AND e.retired_at IS NULL AND d.id=$4 AND d.execution_revoked_at IS NULL)`, env, project, principal.OrgID, deployment).Scan(&allowed); err != nil {
			return err
		}
		if !allowed {
			return ErrNotDeployable
		}
		type authored struct {
			agent uuid.UUID
			raw   []byte
		}
		rows, err := tx.Query(ctx, `SELECT agent_id,triggers FROM agent_definitions WHERE environment_id=$1 AND deployment_id=$2`, env, deployment)
		if err != nil {
			return err
		}
		values, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (authored, error) {
			var a authored
			err := row.Scan(&a.agent, &a.raw)
			return a, err
		})
		if err != nil {
			return err
		}
		for _, value := range values {
			var triggers map[string]definition.CronTrigger
			if err = json.Unmarshal(value.raw, &triggers); err != nil {
				return err
			}
			for _, trigger := range triggers {
				channel, err := definition.SlackChannelReference(trigger.Slack)
				if err != nil {
					return err
				}
				if channel == nil {
					continue
				}
				key := scheduleRouteKey{value.agent, *channel}
				if _, ok := routes[key]; ok {
					continue
				}
				connection, err := agent.ReadSlackConnection(ctx, tx, env, value.agent, *channel)
				if err != nil {
					return err
				}
				routes[key] = connection
			}
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
		if _, err = preflight.PrepareStart(ctx, agent.SlackStartContext{Route: agent.SlackStartRoute{Target: connection, DeploymentID: deployment}}); err != nil {
			return nil, err
		}
	}
	return routes, nil
}

func pinScheduleRoutes(ctx context.Context, tx pgx.Tx, env uuid.UUID, routes map[scheduleRouteKey]agent.SlackConnection) (map[scheduleRouteKey]uuid.UUID, error) {
	pubs := []uuid.UUID{}
	for _, connection := range routes {
		pubs = append(pubs, connection.PublicationID)
	}
	active, err := agent.LockSlackPublications(ctx, tx, env, pubs)
	if err != nil {
		return nil, err
	}
	if !active {
		return nil, agent.ErrConversationChanged
	}
	pins := map[scheduleRouteKey]uuid.UUID{}
	for key, expected := range routes {
		current, err := agent.ReadSlackConnection(ctx, tx, env, key.agent, key.channel)
		if errors.Is(err, agent.ErrTargetNotPublished) {
			return nil, agent.ErrConversationChanged
		}
		if err != nil {
			return nil, err
		}
		if current.PublicationID != expected.PublicationID || current.InstallationID != expected.InstallationID || !current.AuthorizedAt.Equal(expected.AuthorizedAt) {
			return nil, agent.ErrConversationChanged
		}
		id, err := agent.ResolveSlackRoute(ctx, tx, current)
		if err != nil {
			return nil, err
		}
		pins[key] = id
	}
	return pins, nil
}
