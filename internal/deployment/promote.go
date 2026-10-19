package deployment

import (
	"context"
	"errors"
	"fmt"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/agent"
	"github.com/helmrdotdev/helmr/internal/audit"
	"github.com/helmrdotdev/helmr/internal/org"

	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/slackroute"

	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/definition"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
)

// Promote atomically switches the current Deployment, closes prior trigger
// intervals and recomputes background preparation cadence. Existing Sessions keep
// their deployment pins. Re-promotion starts new intervals, never reopens old ones.
func Promote(ctx context.Context, txb db.TxBeginner, principal auth.Principal, scope auth.Scope, id uuid.UUID, preflight slackroute.StartPreflight) (Record, error) {
	if err := authorizeDeploy(principal, scope); err != nil {
		return Record{}, err
	}
	project, env, err := scopeIDs(scope)
	if err != nil {
		return Record{}, err
	}
	routes, err := prepareTriggerRoutes(ctx, txb, principal, pgvalue.MustUUIDValue(env), pgvalue.MustUUIDValue(project), id, preflight)
	if err != nil {
		return Record{}, err
	}
	var result Record
	err = db.RunTx(ctx, txb, func(tx pgx.Tx) error {
		pins, err := pinTriggerRoutes(ctx, tx, pgvalue.MustUUIDValue(env), routes)
		if err != nil {
			return err
		}
		var locked uuid.UUID
		err = tx.QueryRow(ctx, `SELECT id FROM environments WHERE id=$1 AND org_id=$2 AND project_id=$3 FOR NO KEY UPDATE`, env, principal.OrgID, project).Scan(&locked)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotDeployable
		}
		if err != nil {
			return err
		}
		// Verify that the selected Deployment belongs to this Environment.
		if err := tx.QueryRow(ctx, `SELECT id FROM deployments WHERE environment_id=$1 AND id=$2`, env, id).Scan(&locked); errors.Is(err, pgx.ErrNoRows) {
			return ErrNotDeployable
		} else if err != nil {
			return err
		}
		// The Environment lock also serializes connection configuration. An
		// active conversational front cannot acquire a structured input contract.
		var incompatible bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM slack_connections c JOIN agent_definitions d ON (d.environment_id,d.agent_id)=(c.environment_id,c.agent_id) WHERE c.environment_id=$1 AND c.revoked_at IS NULL AND d.deployment_id=$2 AND d.input_kind<>'text')`, env, id).Scan(&incompatible); err != nil {
			return err
		}
		if incompatible {
			return invalidInput(errors.New("a connected Slack agent must accept conversation input"))
		}
		ready, err := preparePromotion(ctx, tx, pgvalue.MustUUIDValue(env), id)
		if err != nil {
			return err
		}
		if !ready {
			result, err = readRecord(ctx, tx, pgvalue.MustUUIDValue(env), id)
			result.PreparationState = "preparing"
			return err
		}
		var cutover time.Time
		if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&cutover); err != nil {
			return err
		}
		if err := reconcileTriggers(ctx, tx, pgvalue.MustUUIDValue(env), id, cutover, pins); err != nil {
			return err
		}
		if err := reconcileImageRefresh(ctx, tx, pgvalue.MustUUIDValue(env), id, cutover); err != nil {
			return err
		}
		if err := org.SetCurrentDeployment(ctx, tx, pgvalue.MustUUIDValue(env), id, cutover); err != nil {
			return err
		}
		if err = appendEvent(ctx, tx, pgvalue.MustUUIDValue(env), id, "deployment.promoted", "Deployment promoted"); err != nil {
			return err
		}
		if err := audit.Append(ctx, tx, audit.Event{Actor: audit.FromPrincipal(principal), EnvironmentID: pgvalue.MustUUIDValue(env), Action: "deployment.promoted", TargetKind: "deployment", TargetID: id}); err != nil {
			return err
		}
		result, err = readRecord(ctx, tx, pgvalue.MustUUIDValue(env), id)
		return err
	})
	if err != nil {
		return Record{}, err
	}
	return result, nil
}

func reconcileTriggers(ctx context.Context, tx pgx.Tx, env, deployment uuid.UUID, cutover time.Time, pins map[triggerRouteKey]uuid.UUID) error {
	rows, err := tx.Query(ctx, `SELECT agent_id,key,cron,timezone,slack_channel_id FROM agent_triggers WHERE environment_id=$1 AND deployment_id=$2 ORDER BY agent_id,key`, env, deployment)
	if err != nil {
		return err
	}
	type declared struct {
		agent               uuid.UUID
		key, cron, timezone string
		destination         *string
	}
	declarations, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (declared, error) {
		var d declared
		err := row.Scan(&d.agent, &d.key, &d.cron, &d.timezone, &d.destination)
		return d, err
	})
	if err != nil {
		return err
	}
	entries := make([]agent.Activation, 0, len(declarations))
	for _, d := range declarations {
		next, err := definition.NextCronTime(d.cron, d.timezone, cutover.Add(-time.Nanosecond))
		if err != nil {
			return invalidInput(fmt.Errorf("agent trigger %q cannot be scheduled: %w", d.key, err))
		}
		var pin *uuid.UUID
		if d.destination != nil {
			value, ok := pins[triggerRouteKey{d.agent, *d.destination}]
			if !ok {
				return ErrNotDeployable
			}
			pin = &value
		}
		entries = append(entries, agent.Activation{SlackChannelBindingID: pin, Agent: d.agent, Key: d.key, Next: next, ID: uuid.NewV7()})
	}
	return agent.ReplaceActivations(ctx, tx, env, deployment, cutover, entries)
}

func reconcileImageRefresh(ctx context.Context, tx pgx.Tx, env, deployment uuid.UUID, cutover time.Time) error {
	// Lock in stable order. Refresh workers use the same Environment -> spec order.
	rows, err := tx.Query(ctx, `WITH affected AS (SELECT id FROM image_specs WHERE environment_id=$1 AND refresh_every_ms IS NOT NULL UNION SELECT image_spec_id FROM template_definitions WHERE environment_id=$1 AND deployment_id=$2 AND refresh_every_ms IS NOT NULL) SELECT s.id FROM image_specs s JOIN affected a ON a.id=s.id WHERE s.environment_id=$1 ORDER BY s.id FOR NO KEY UPDATE OF s`, env, deployment)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	return computer.ApplyImageRefreshPolicy(ctx, tx, env, deployment, cutover)
}
