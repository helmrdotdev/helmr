package agent

import (
	"context"
	"errors"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/slackroute"
	"github.com/jackc/pgx/v5"
)

// BindSlackTriggers binds the Agent's open trigger activations whose
// declaration has a Slack channel and that have no binding, or a binding to a
// revoked connection, to the Agent's authorized connection. It runs in the
// connection authorization transaction, which holds the Environment lock
// before these activation rows as promotion and evaluation do. The channel is
// not checked live; a channel problem surfaces in the delivery result of the
// first scheduled Session. The activation interval does not change.
func BindSlackTriggers(ctx context.Context, tx pgx.Tx, env, agent uuid.UUID) error {
	rows, err := tx.Query(ctx, `SELECT a.id,t.slack_channel_id FROM trigger_activations a
 JOIN agent_triggers t ON (t.environment_id,t.agent_id,t.deployment_id,t.key)=(a.environment_id,a.agent_id,a.deployment_id,a.trigger_key)
 LEFT JOIN slack_channel_bindings b ON b.id=a.slack_channel_binding_id LEFT JOIN slack_connections c ON c.id=b.connection_id
 WHERE a.environment_id=$1 AND a.agent_id=$2 AND a.active_until IS NULL AND t.slack_channel_id IS NOT NULL AND (a.slack_channel_binding_id IS NULL OR c.revoked_at IS NOT NULL)
 ORDER BY a.id FOR NO KEY UPDATE OF a`, env, agent)
	if err != nil {
		return err
	}
	type waiting struct {
		id      uuid.UUID
		channel string
	}
	activations, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (waiting, error) {
		var w waiting
		err := row.Scan(&w.id, &w.channel)
		return w, err
	})
	if err != nil {
		return err
	}
	for _, w := range activations {
		target, err := slackroute.ReadConnection(ctx, tx, env, agent, w.channel)
		if errors.Is(err, slackroute.ErrTargetNotPublished) {
			// The granted scopes cannot reach this channel; it keeps waiting.
			continue
		}
		if err != nil {
			return err
		}
		binding, err := slackroute.ResolveChannelBinding(ctx, tx, target)
		if err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `UPDATE trigger_activations SET slack_channel_binding_id=$3 WHERE environment_id=$1 AND id=$2`, env, w.id, binding); err != nil {
			return err
		}
	}
	return nil
}
