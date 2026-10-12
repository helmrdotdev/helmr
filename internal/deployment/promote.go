package deployment

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/agent"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/definition"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
)

// Promote atomically switches the current Deployment, closes prior trigger
// intervals and recomputes background preparation cadence. Existing Sessions keep
// their deployment pins. Re-promotion starts new intervals, never reopens old ones.
func Promote(ctx context.Context, txb db.TxBeginner, principal auth.Principal, scope auth.Scope, id uuid.UUID, preflight agent.SlackStartPreflight) (Record, error) {
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
		err = tx.QueryRow(ctx, `SELECT id FROM environments WHERE id=$1 AND org_id=$2 AND project_id=$3 AND retired_at IS NULL FOR NO KEY UPDATE`, env, principal.OrgID, project).Scan(&locked)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotDeployable
		}
		if err != nil {
			return err
		}
		// Hold the deployment against concurrent revocation through the cutover.
		var allowed bool
		if err := tx.QueryRow(ctx, `SELECT execution_revoked_at IS NULL FROM deployments WHERE environment_id=$1 AND id=$2 FOR SHARE`, env, id).Scan(&allowed); errors.Is(err, pgx.ErrNoRows) {
			return ErrNotDeployable
		} else if err != nil {
			return err
		}
		if !allowed {
			return ErrNotDeployable
		}
		var cutover time.Time
		if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&cutover); err != nil {
			return err
		}
		if err := reconcileTriggers(ctx, tx, pgvalue.MustUUIDValue(env), id, cutover, pins); err != nil {
			return err
		}
		if err := reconcilePreparationRefresh(ctx, tx, pgvalue.MustUUIDValue(env), id, cutover); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE environments SET current_deployment_id=$2,updated_at=$3 WHERE id=$1`, env, id, cutover); err != nil {
			return err
		}
		if err = appendEvent(ctx, tx, pgvalue.MustUUIDValue(env), id, "deployment.promoted", "Deployment promoted"); err != nil {
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
	type entry struct {
		SlackChannelID *uuid.UUID `json:"slack_channel_id"`
		Agent          uuid.UUID  `json:"agent"`
		Key            string     `json:"key"`
		Next           time.Time  `json:"next"`
		ID             uuid.UUID  `json:"id"`
	}
	rows, err := tx.Query(ctx, `SELECT agent_id,key,cron,timezone,slack_destination FROM agent_triggers WHERE environment_id=$1 AND deployment_id=$2 ORDER BY agent_id,key`, env, deployment)
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
	entries := make([]entry, 0, len(declarations))
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
		entries = append(entries, entry{SlackChannelID: pin, Agent: d.agent, Key: d.key, Next: next, ID: uuid.NewV7()})
	}
	if _, err := tx.Exec(ctx, `UPDATE trigger_activations SET active_until=$2 WHERE environment_id=$1 AND active_until IS NULL`, env, cutover); err != nil {
		return err
	}
	if len(entries) > 0 {
		raw, err := json.Marshal(entries)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO trigger_activations(environment_id,id,agent_id,deployment_id,trigger_key,active_from,next_fire_at,slack_channel_id)
 SELECT $1,v.id,v.agent,$2,v.key,$3,v.next,v.slack_channel_id
 FROM jsonb_to_recordset($4::jsonb) AS v(id uuid,agent uuid,key text,next timestamptz,slack_channel_id uuid)`, env, deployment, cutover, raw)
		if err != nil {
			return err
		}
	}
	return nil
}

func reconcilePreparationRefresh(ctx context.Context, tx pgx.Tx, env, deployment uuid.UUID, cutover time.Time) error {
	// Lock in stable order. Refresh workers use the same Environment -> spec order.
	rows, err := tx.Query(ctx, `WITH affected AS (SELECT id FROM computer_preparation_specs WHERE environment_id=$1 AND refresh_every_ms IS NOT NULL UNION SELECT preparation_spec_id FROM template_definitions WHERE environment_id=$1 AND deployment_id=$2 AND refresh_every_ms IS NOT NULL) SELECT s.id FROM computer_preparation_specs s JOIN affected a ON a.id=s.id WHERE s.environment_id=$1 ORDER BY s.id FOR NO KEY UPDATE OF s`, env, deployment)
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
	_, err = tx.Exec(ctx, `WITH requested AS (
 SELECT preparation_spec_id,min(refresh_every_ms) AS every_ms FROM template_definitions
 WHERE environment_id=$1 AND deployment_id=$2 AND refresh_every_ms IS NOT NULL GROUP BY preparation_spec_id
 ) , affected AS (
 SELECT id FROM computer_preparation_specs WHERE environment_id=$1 AND refresh_every_ms IS NOT NULL
 UNION SELECT preparation_spec_id FROM requested
 ), policy AS (
 SELECT a.id,r.every_ms FROM affected a LEFT JOIN requested r ON r.preparation_spec_id=a.id
 ) UPDATE computer_preparation_specs s SET refresh_every_ms=p.every_ms,
 next_refresh_at=CASE WHEN p.every_ms IS NULL THEN NULL
 WHEN s.refresh_every_ms=p.every_ms THEN s.next_refresh_at
 ELSE LEAST(s.next_refresh_at,$3::timestamptz+p.every_ms*interval '1 millisecond') END
 FROM policy p WHERE s.environment_id=$1 AND s.id=p.id AND s.refresh_every_ms IS DISTINCT FROM p.every_ms`, env, deployment, cutover)
	return err
}
