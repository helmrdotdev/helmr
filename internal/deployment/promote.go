package deployment

import (
	"context"
	"errors"
	"fmt"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"slices"
	"strings"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/access"
	"github.com/helmrdotdev/helmr/internal/agent"
	"github.com/helmrdotdev/helmr/internal/audit"
	"github.com/helmrdotdev/helmr/internal/org"

	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/slackroute"

	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/definition"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
)

// Promotion is the receipt of a promotion. RestoredAgents names the deleted
// Agents the Deployment declares and the promotion restored. WaitingTriggers
// names the triggers whose Slack destination waits for a connection.
type Promotion struct {
	Record
	RestoredAgents  []string
	WaitingTriggers []WaitingTrigger
}

type WaitingTrigger struct {
	Agent, Trigger string
}

// Promote atomically switches the current Deployment, restores the deleted
// Agents it declares, closes prior trigger intervals and recomputes background
// preparation cadence. Existing Sessions keep their deployment pins.
// Re-promotion starts new intervals, never reopens old ones.
func Promote(ctx context.Context, txb db.TxBeginner, policy auditpolicy.Policy, principal auth.Principal, scope auth.Scope, id uuid.UUID, preflight slackroute.StartPreflight) (Promotion, error) {
	if err := authorize(principal, scope, auth.PermissionDeploymentsPromote); err != nil {
		return Promotion{}, err
	}
	project, env, err := scopeIDs(scope)
	if err != nil {
		return Promotion{}, err
	}
	routes, err := prepareTriggerRoutes(ctx, txb, principal, pgvalue.MustUUIDValue(env), pgvalue.MustUUIDValue(project), id, preflight)
	if err != nil {
		return Promotion{}, err
	}
	var result Promotion
	err = db.RunTx(ctx, txb, func(tx pgx.Tx) error {
		// Retained human authority precedes environment and route locks, matching
		// membership changes and Agent cleanup. Re-read permissions below.
		if _, err := access.Current(ctx, tx, principal, scope); err != nil {
			return err
		}
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
		current, err := access.Current(ctx, tx, principal, scope)
		if err != nil {
			return err
		}
		actor, err := audit.Authorized(current, auth.PermissionDeploymentsPromote, scope)
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
		// Placement writers take the same lock, so the binding holds from
		// before preparation is queued through cutover.
		if err := workergroup.RequireDeploymentPlacements(ctx, tx, pgvalue.MustUUIDValue(env), id); err != nil {
			return err
		}
		ready, err := preparePromotion(ctx, tx, pgvalue.MustUUIDValue(env), id)
		if err != nil {
			return err
		}
		if !ready {
			result.Record, err = readRecord(ctx, tx, pgvalue.MustUUIDValue(env), id)
			result.PreparationState = "preparing"
			return err
		}
		if err := confirmWaitingRoutes(ctx, tx, pgvalue.MustUUIDValue(env), routes); err != nil {
			return err
		}
		var cutover time.Time
		if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&cutover); err != nil {
			return err
		}
		if result.RestoredAgents, err = restoreDeclaredAgents(ctx, tx, policy, actor, pgvalue.MustUUIDValue(env), id); err != nil {
			return err
		}
		if result.WaitingTriggers, err = reconcileTriggers(ctx, tx, pgvalue.MustUUIDValue(env), id, cutover, pins); err != nil {
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
		if err := audit.Append(ctx, tx, policy, audit.Event{Evidence: actor, EnvironmentID: pgvalue.MustUUIDValue(env), Action: "deployment.promoted", TargetKind: "deployment", TargetID: id}); err != nil {
			return err
		}
		result.Record, err = readRecord(ctx, tx, pgvalue.MustUUIDValue(env), id)
		return err
	})
	if err != nil {
		return Promotion{}, err
	}
	return result, nil
}

// restoreDeclaredAgents clears deletion of the Agents the Deployment declares.
// Grants, Slack connections, sealed activations and cancelled Sessions removed
// by deletion stay removed. The caller holds the Environment lock, which
// precedes the Agent rows as in deletion.
func restoreDeclaredAgents(ctx context.Context, tx pgx.Tx, policy auditpolicy.Policy, actor audit.Evidence, env, deployment uuid.UUID) ([]string, error) {
	rows, err := tx.Query(ctx, `UPDATE agents a SET deleted_at=NULL,revision=a.revision+1 FROM agent_definitions d
 WHERE d.environment_id=$1 AND d.deployment_id=$2 AND (a.environment_id,a.id)=(d.environment_id,d.agent_id) AND a.deleted_at IS NOT NULL
 RETURNING a.id,a.key,a.revision`, env, deployment)
	if err != nil {
		return nil, err
	}
	type restored struct {
		id       uuid.UUID
		key      string
		revision int64
	}
	values, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (restored, error) {
		var r restored
		err := row.Scan(&r.id, &r.key, &r.revision)
		return r, err
	})
	if err != nil {
		return nil, err
	}
	slices.SortFunc(values, func(a, b restored) int { return strings.Compare(a.key, b.key) })
	keys := []string{}
	for _, r := range values {
		if err := audit.Append(ctx, tx, policy, audit.Event{Evidence: actor, EnvironmentID: env, DeploymentID: deployment, Action: "agent.restored", TargetKind: "agent", TargetID: r.id, Details: audit.AgentChangeDetails{Revision: r.revision}}); err != nil {
			return nil, err
		}
		keys = append(keys, r.key)
	}
	return keys, nil
}

func reconcileTriggers(ctx context.Context, tx pgx.Tx, env, deployment uuid.UUID, cutover time.Time, pins map[triggerRouteKey]uuid.UUID) ([]WaitingTrigger, error) {
	rows, err := tx.Query(ctx, `SELECT t.agent_id,a.key,t.key,t.cron,t.timezone,t.slack_channel_id FROM agent_triggers t JOIN agents a ON (a.environment_id,a.id)=(t.environment_id,t.agent_id) WHERE t.environment_id=$1 AND t.deployment_id=$2 ORDER BY a.key,t.key`, env, deployment)
	if err != nil {
		return nil, err
	}
	type declared struct {
		agent                         uuid.UUID
		agentKey, key, cron, timezone string
		destination                   *string
	}
	declarations, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (declared, error) {
		var d declared
		err := row.Scan(&d.agent, &d.agentKey, &d.key, &d.cron, &d.timezone, &d.destination)
		return d, err
	})
	if err != nil {
		return nil, err
	}
	entries := make([]agent.Activation, 0, len(declarations))
	waiting := []WaitingTrigger{}
	for _, d := range declarations {
		next, err := definition.NextCronTime(d.cron, d.timezone, cutover.Add(-time.Nanosecond))
		if err != nil {
			return nil, invalidInput(fmt.Errorf("agent trigger %q cannot be scheduled: %w", d.key, err))
		}
		var pin *uuid.UUID
		if d.destination != nil {
			if value, ok := pins[triggerRouteKey{d.agent, *d.destination}]; ok {
				pin = &value
			} else {
				// The activation waits for a Slack connection of its Agent.
				waiting = append(waiting, WaitingTrigger{Agent: d.agentKey, Trigger: d.key})
			}
		}
		entries = append(entries, agent.Activation{SlackChannelBindingID: pin, Agent: d.agent, Key: d.key, Next: next, ID: uuid.NewV7()})
	}
	return waiting, agent.ReplaceActivations(ctx, tx, env, deployment, cutover, entries)
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
