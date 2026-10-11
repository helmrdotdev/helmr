package deployment

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/agent"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/definition"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
)

// ScheduleLatenessTolerance bounds catch-up to the latest occurrence within
// five minutes. Each activation records this platform policy for evaluation.
const ScheduleLatenessTolerance = 5 * time.Minute

// Promote atomically switches the current Deployment, closes prior schedule
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
	routes, err := prepareScheduleRoutes(ctx, txb, principal, pgvalue.MustUUIDValue(env), pgvalue.MustUUIDValue(project), id, preflight)
	if err != nil {
		return Record{}, err
	}
	var result Record
	err = db.RunTx(ctx, txb, func(tx pgx.Tx) error {
		pins, err := pinScheduleRoutes(ctx, tx, pgvalue.MustUUIDValue(env), routes)
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
		if err := reconcileSchedules(ctx, tx, pgvalue.MustUUIDValue(env), id, cutover, pins); err != nil {
			return err
		}
		if err := reconcilePreparationRefresh(ctx, tx, pgvalue.MustUUIDValue(env), id, cutover); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE environments SET current_deployment_id=$2,updated_at=$3 WHERE id=$1`, env, id, cutover); err != nil {
			return err
		}
		if _, err = db.New(tx).AppendDeploymentEvent(ctx, db.AppendDeploymentEventParams{EnvironmentID: env, DeploymentID: pgvalue.UUID(id), OrgID: pgvalue.UUID(principal.OrgID), ProjectID: project, Kind: "deployment.promoted", Message: "Deployment promoted", Payload: []byte(`{}`)}); err != nil {
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

func reconcileSchedules(ctx context.Context, tx pgx.Tx, env, deployment uuid.UUID, cutover time.Time, pins map[scheduleRouteKey]uuid.UUID) error {
	type entry struct {
		SlackChannelID *uuid.UUID `json:"slack_channel_id"`
		Agent          uuid.UUID  `json:"agent"`
		Key            string     `json:"key"`
		Cron           string     `json:"cron"`
		Timezone       string     `json:"timezone"`
		Input          []byte     `json:"input"`
		Next           time.Time  `json:"next"`
		ID             uuid.UUID  `json:"id"`
	}
	var entries []entry
	rows, err := tx.Query(ctx, `SELECT agent_id,triggers FROM agent_definitions WHERE environment_id=$1 AND deployment_id=$2 ORDER BY agent_id`, env, deployment)
	if err != nil {
		return err
	}
	for rows.Next() {
		var agent uuid.UUID
		var raw []byte
		if err := rows.Scan(&agent, &raw); err != nil {
			rows.Close()
			return err
		}
		var triggers map[string]definition.CronTrigger
		if err := json.Unmarshal(raw, &triggers); err != nil {
			rows.Close()
			return err
		}
		keys := make([]string, 0, len(triggers))
		for key := range triggers {
			keys = append(keys, key)
		}
		slices.Sort(keys)
		for _, key := range keys {
			trigger := triggers[key]
			next, err := definition.NextCronTime(trigger.Cron, trigger.Timezone, cutover.Add(-time.Nanosecond))
			if err != nil {
				rows.Close()
				return invalidInput(fmt.Errorf("agent trigger %q cannot be scheduled: %w", key, err))
			}
			route, err := definition.SlackChannelReference(trigger.Slack)
			if err != nil {
				rows.Close()
				return err
			}
			var pin *uuid.UUID
			if route != nil {
				value, ok := pins[scheduleRouteKey{agent, *route}]
				if !ok {
					rows.Close()
					return ErrNotDeployable
				}
				pin = &value
			}
			entries = append(entries, entry{SlackChannelID: pin, Agent: agent, Key: key, Cron: trigger.Cron, Timezone: trigger.Timezone, Input: trigger.Input, Next: next, ID: uuid.NewV7()})
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE agent_schedules SET active_until=$2 WHERE environment_id=$1 AND active_until IS NULL`, env, cutover); err != nil {
		return err
	}
	if len(entries) > 0 {
		raw, err := json.Marshal(entries)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO agent_schedules(environment_id,id,agent_id,deployment_id,trigger_key,cron,timezone,input,active_from,next_fire_at,lateness_tolerance_ms,slack_channel_id)
 SELECT $1,v.id,v.agent,$2,v.key,v.cron,v.timezone,decode(v.input,'base64'),$3,v.next,$4,v.slack_channel_id
 FROM jsonb_to_recordset($5::jsonb) AS v(id uuid,agent uuid,key text,cron text,timezone text,input text,next timestamptz,slack_channel_id uuid)`, env, deployment, cutover, ScheduleLatenessTolerance.Milliseconds(), raw)
		if err != nil {
			return err
		}
	}

	return nil
}

func reconcilePreparationRefresh(ctx context.Context, tx pgx.Tx, env, deployment uuid.UUID, cutover time.Time) error {
	// Lock in stable order. Refresh workers use the same Environment -> spec order.
	rows, err := tx.Query(ctx, `WITH affected AS (SELECT id FROM computer_preparation_specs WHERE environment_id=$1 AND refresh_every_ms IS NOT NULL UNION SELECT preparation_spec_id FROM computer_definitions WHERE environment_id=$1 AND deployment_id=$2 AND refresh_every_ms IS NOT NULL) SELECT s.id FROM computer_preparation_specs s JOIN affected a ON a.id=s.id WHERE s.environment_id=$1 ORDER BY s.id FOR NO KEY UPDATE OF s`, env, deployment)
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
 SELECT preparation_spec_id,min(refresh_every_ms) AS every_ms FROM computer_definitions
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
