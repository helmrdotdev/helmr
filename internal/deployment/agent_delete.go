package deployment

import (
	"context"
	"errors"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"strconv"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/access"
	"github.com/helmrdotdev/helmr/internal/agent"
	"github.com/helmrdotdev/helmr/internal/audit"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/org"
	"github.com/helmrdotdev/helmr/internal/session"
	"github.com/helmrdotdev/helmr/internal/slack"
	"github.com/jackc/pgx/v5"
)

type AgentChange struct {
	RetainedAgent
	CanApply, Changed bool
	Blockers          []session.AgentDeletionBlocker
	Effects           AgentDeletionEffects
	Warnings          []string
}
type AgentDeletionEffects struct {
	SelectedAgentEdgesRemoved     int
	IneffectiveAssignmentsRemoved int
	MixedAssignmentsPreserved     int
	SlackConnectionDisconnected   *string
	TriggerActivationsSealed      int64
}

var ErrAgentDeletionBlocked = errors.New("agent deletion has unresolved prerequisites")
var ErrAgentNotFound = errors.New("agent not found")
var ErrAgentDeleted = errors.New("restore deleted Agents before promotion")

type DeletedAgentsError struct {
	Keys  []string `json:"agents"`
	Count int64    `json:"count"`
}

func (e DeletedAgentsError) Error() string { return ErrAgentDeleted.Error() }
func (e DeletedAgentsError) Unwrap() error { return ErrAgentDeleted }

// ChangeAgent joins access, Slack and scheduling owners in one durable operation.
// Discovery takes earlier-order locks first, then the Environment and Agent fences.
// A new earlier-order dependency rolls back rather than inverting lock order.
func ChangeAgent(ctx context.Context, database db.TxBeginner, policy auditpolicy.Policy, p auth.Principal, scope auth.Scope, key, revision string, restore, dryRun bool) (AgentChange, error) {
	permission := auth.PermissionAgentsDelete
	if restore {
		permission = auth.PermissionAgentsRestore
	}
	result := AgentChange{Blockers: []session.AgentDeletionBlocker{}, Warnings: []string{}}
	if p.Kind != auth.PrincipalKindSession {
		return result, ErrPermissionRequired
	}
	project, envValue, err := scopeIDs(scope)
	if err != nil {
		return result, err
	}
	env := uuid.MustParse(scope.EnvironmentID)
	m := org.ManagingMember{AuditPolicy: policy, Actor: audit.FromPrincipal(p), OrgID: p.OrgID, UserID: p.UserID, MemberID: p.MemberID}
	err = db.RunTx(ctx, database, func(tx pgx.Tx) error {
		current, err := currentPrincipal(ctx, tx, p, scope)
		if err != nil {
			return err
		}
		all, visible := agentIdentityScope(current, scope)
		var id uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT a.id FROM agents a JOIN environments e ON e.id=a.environment_id WHERE e.org_id=$1 AND e.project_id=$2 AND e.id=$3 AND a.key=$4 AND ($5::boolean OR a.id=ANY($6::uuid[]))`, p.OrgID, project, envValue, key, all, visible).Scan(&id); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrAgentNotFound
			}
			return err
		}
		scope.AgentID = id.String()
		if err := authorize(current, scope, permission); err != nil {
			return err
		}
		grants, err := org.LockAgentGrantCleanup(ctx, tx, m, env, id, restore)
		if err != nil {
			return err
		}
		var connection *uuid.UUID
		if err = tx.QueryRow(ctx, `SELECT (SELECT id FROM slack_connections WHERE environment_id=$1 AND agent_id=$2 AND revoked_at IS NULL)`, env, id).Scan(&connection); err != nil {
			return err
		}
		var disconnection *slack.ConnectionDisconnection
		if connection != nil && !restore {
			disconnection, err = slack.LockAgentDeletionDisconnection(ctx, tx, p.OrgID, p.UserID, env, id, *connection)
			if err != nil {
				return err
			}
		}
		if _, err = org.LockAdmission(ctx, tx, env); err != nil {
			return err
		}
		var currentConnection *uuid.UUID
		if err = tx.QueryRow(ctx, `SELECT (SELECT id FROM slack_connections WHERE environment_id=$1 AND agent_id=$2 AND revoked_at IS NULL)`, env, id).Scan(&currentConnection); err != nil {
			return err
		}
		if !restore && ((connection == nil) != (currentConnection == nil) || connection != nil && currentConnection != nil && *connection != *currentConnection) {
			return org.ErrAgentDependenciesChanged
		}
		if err = tx.QueryRow(ctx, `SELECT a.key,a.deleted_at,a.revision,e.current_deployment_id::text,EXISTS(SELECT 1 FROM agent_definitions d WHERE d.environment_id=a.environment_id AND d.agent_id=a.id AND d.deployment_id=e.current_deployment_id)
 FROM agents a JOIN environments e ON e.id=a.environment_id WHERE a.environment_id=$1 AND a.id=$2 FOR UPDATE OF a`, env, id).Scan(&result.Agent, &result.DeletedAt, &result.Revision, &result.CurrentDeploymentID, &result.DefinedInCurrent); err != nil {
			return err
		}
		if err = grants.Recheck(ctx, tx); err != nil {
			return err
		}
		current, err = currentPrincipal(ctx, tx, p, scope)
		if err != nil {
			return err
		}
		m.Actor, err = audit.Authorized(current, permission, scope)
		if err != nil {
			return errors.Join(ErrPermissionRequired, err)
		}
		if !dryRun && revision != strconv.FormatInt(result.Revision, 10) {
			return org.ErrStaleRevision
		}
		result.CanApply = true
		if (result.DeletedAt == nil) == restore {
			return nil
		}
		if restore {
			result.Warnings = append(result.Warnings, "Removed access and Slack connections will not be restored. Broad access assignments apply again, and existing parent Sessions can create children immediately. New independent Sessions and schedules require a deployment containing this Agent.")
		} else {
			if result.DefinedInCurrent {
				result.Blockers = append(result.Blockers, session.AgentDeletionBlocker{Code: "defined_in_current", Count: 1})
			}
			visible := current.HasPermission(auth.PermissionSessionsRead, scope)
			blockers, err := session.AgentDeletionBlockers(ctx, tx, env, id, visible)
			if err != nil {
				return err
			}
			result.Blockers = append(result.Blockers, blockers...)
			result.CanApply = len(result.Blockers) == 0
			result.Effects.SelectedAgentEdgesRemoved, result.Effects.IneffectiveAssignmentsRemoved, result.Effects.MixedAssignmentsPreserved = grants.Counts()
			if connection != nil {
				v := connection.String()
				result.Effects.SlackConnectionDisconnected = &v
			}
			if err = tx.QueryRow(ctx, `SELECT count(*) FROM trigger_activations WHERE environment_id=$1 AND agent_id=$2 AND active_until IS NOT NULL AND next_fire_at<active_until`, env, id).Scan(&result.Effects.TriggerActivationsSealed); err != nil {
				return err
			}
			broad := scope
			broad.AgentID = ""
			if !current.HasPermission(permission, broad) {
				result.Warnings = append(result.Warnings, "Deletion removes your selected Agent management access. A manager with surviving broad access, or an owner using access recovery, must inspect status or restore it afterward.")
			}
			if !visible {
				result.Warnings = append(result.Warnings, "Session details require Session viewing permission; only blocker counts are shown.")
			}
		}
		if dryRun {
			return nil
		}
		if !result.CanApply {
			return ErrAgentDeletionBlocked
		}
		action := "agent.restored"
		if !restore {
			if disconnection != nil {
				if err = disconnection.Apply(ctx, tx, policy, m.Actor); err != nil {
					return err
				}
			}
			if err = grants.Apply(ctx, tx); err != nil {
				return err
			}
			result.Effects.TriggerActivationsSealed, err = agent.SealDeletedAgent(ctx, tx, env, id)
			if err != nil {
				return err
			}
			action = "agent.deleted"
		}
		if err = tx.QueryRow(ctx, `UPDATE agents SET deleted_at=CASE WHEN $3::boolean THEN NULL ELSE clock_timestamp() END,revision=revision+1 WHERE environment_id=$1 AND id=$2 RETURNING deleted_at,revision`, env, id, restore).Scan(&result.DeletedAt, &result.Revision); err != nil {
			return err
		}
		result.Changed = true
		e := result.Effects
		return audit.Append(ctx, tx, policy, audit.Event{Evidence: m.Actor, OrgID: p.OrgID, EnvironmentID: env, Action: action, TargetKind: "agent", TargetID: id, Details: audit.AgentChangeDetails{Revision: result.Revision, EdgesRemoved: e.SelectedAgentEdgesRemoved, AssignmentsRemoved: e.IneffectiveAssignmentsRemoved, MixedPreserved: e.MixedAssignmentsPreserved, ConnectionID: e.SlackConnectionDisconnected, ActivationsSealed: e.TriggerActivationsSealed}})
	})
	if errors.Is(err, access.ErrDenied) || slack.IsInstallationDenied(err) {
		err = ErrPermissionRequired
	}
	if err != nil {
		result.Changed = false
	}
	return result, err
}
