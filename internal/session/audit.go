package session

import (
	"context"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/access"
	"github.com/helmrdotdev/helmr/internal/audit"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
)

// admissionActor runs after the owner's authority checks, in their transaction.
// Runtime execution is attributed to the Session; its origin is separate causality.
func admissionActor(ctx context.Context, tx pgx.Tx, caller Caller, req StartRequest, agent uuid.UUID) (audit.Evidence, audit.Causality, error) {
	env := req.EnvironmentID
	if caller.Kind == "schedule" {
		return audit.Internal(audit.Dispatcher), audit.Causality{InitiatorKind: "schedule", InitiatorID: caller.ID}, nil
	}
	if caller.Kind == "session" {
		origin, err := loadSessionOrigin(ctx, tx, env, caller.ID)
		if err != nil {
			return audit.Evidence{}, audit.Causality{}, err
		}
		return callerRuntimeAuditEvidence(caller), audit.Causality{InitiatorKind: origin.kind(), InitiatorID: origin.id(), SourceID: caller.TurnID}, nil
	}
	p, err := access.Principal(ctx, tx, caller.Kind, caller.ID, env)
	if err != nil {
		return audit.Evidence{}, audit.Causality{}, err
	}
	if caller.Kind == "api_key" && caller.CredentialVersion > 0 && caller.CredentialVersion != p.CredentialVersion {
		return audit.Evidence{}, audit.Causality{}, ErrDenied
	}
	p.SessionID = caller.CredentialID
	actor, err := audit.Authorized(p, auth.PermissionAgentsUse, auth.Scope{OrgID: p.OrgID, ProjectID: p.ProjectID, EnvironmentID: p.EnvironmentID, AgentID: agent.String()})
	if err != nil {
		return audit.Evidence{}, audit.Causality{}, err
	}
	if caller.SlackConnectionID == uuid.Nil() {
		permissions := []auth.Permission{}
		if req.SessionKey != nil || req.SlackChannelID != nil {
			permissions = append(permissions, auth.PermissionSessionsCreateKeyed)
		}
		if req.ComputerID != uuid.Nil() {
			permissions = append(permissions, auth.PermissionComputersExecute)
		}
		for _, permission := range permissions {
			scope := auth.Scope{OrgID: p.OrgID, ProjectID: p.ProjectID, EnvironmentID: p.EnvironmentID}
			if permission == auth.PermissionSessionsCreateKeyed {
				scope.AgentID = agent.String()
			}
			decisions, allowed := p.Decide(permission, scope)
			if !allowed {
				return audit.Evidence{}, audit.Causality{}, ErrDenied
			}
			actor.Authorization.Checks = append(actor.Authorization.Checks, audit.PermissionEvidence(p, decisions)...)
		}
	}
	callerAuthentication(&actor, caller)
	return actor, audit.Causality{}, nil
}

func auditStart(ctx context.Context, tx pgx.Tx, plan StartPlan, result Admission, agent, deployment, computer uuid.UUID) error {
	actor, cause, err := admissionActor(ctx, tx, plan.Caller, plan.Request, agent)
	if err != nil {
		return err
	}
	operation := audit.RequestFrom(ctx).OperationID
	if operation == uuid.Nil() {
		operation = uuid.NewV7()
	}
	e := audit.Event{OperationID: operation, Evidence: actor, Causality: cause, EnvironmentID: plan.Request.EnvironmentID, SessionID: result.SessionID, TurnID: result.TurnID, ComputerID: computer, DeploymentID: deployment, TargetKind: "session", TargetID: result.SessionID, Action: "session.created", Details: audit.LifecycleDetails{Sequence: result.Sequence}}
	if plan.Request.ComputerID == uuid.Nil() && computer != uuid.Nil() {
		created := e
		created.Action, created.TargetKind, created.TargetID = "computer.created", "computer", computer
		if err := audit.Append(ctx, tx, plan.AuditPolicy, created); err != nil {
			return err
		}
	}
	if err := audit.Append(ctx, tx, plan.AuditPolicy, e); err != nil {
		return err
	}
	e.Action, e.Outcome = "session.admitted", "accepted"
	if err := audit.Append(ctx, tx, plan.AuditPolicy, e); err != nil {
		return err
	}
	if result.TurnID != uuid.Nil() {
		e.Action, e.TargetKind, e.TargetID = "turn.admitted", "turn", result.TurnID
		return audit.Append(ctx, tx, plan.AuditPolicy, e)
	}
	return nil
}

// auditOperation is called only after an operation has accepted a fresh receipt.
// It captures the same target authorization path while the owner still holds its
// transaction. Receipt replays return before this boundary.
func auditOperation(ctx context.Context, tx pgx.Tx, policy auditpolicy.Policy, caller Caller, permission auth.Permission, e audit.Event) error {
	if err := authorizeTargetAuthority(ctx, tx, caller, e.EnvironmentID, e.SessionID, permission); err != nil {
		return err
	}
	if caller.Kind == "session" {
		origin, err := loadSessionOrigin(ctx, tx, e.EnvironmentID, caller.ID)
		if err != nil {
			return err
		}
		e.Evidence = callerRuntimeAuditEvidence(caller)
		e.Causality = audit.Causality{InitiatorKind: origin.kind(), InitiatorID: origin.id(), SourceID: caller.TurnID}
	} else {
		origin, err := loadSessionOrigin(ctx, tx, e.EnvironmentID, e.SessionID)
		if err != nil {
			return err
		}
		p, err := access.Principal(ctx, tx, caller.Kind, caller.ID, e.EnvironmentID)
		if err != nil {
			return err
		}
		if caller.Kind == "api_key" && caller.CredentialVersion > 0 && caller.CredentialVersion != p.CredentialVersion {
			return ErrDenied
		}
		p.SessionID = caller.CredentialID
		scope := auth.Scope{OrgID: p.OrgID, ProjectID: p.ProjectID, EnvironmentID: p.EnvironmentID, AgentID: origin.Agent.String()}
		if p.HasPermission(permission, scope) {
			e.Evidence, err = audit.Authorized(p, permission, scope)
		} else {
			e.Evidence, err = audit.Authorized(p, auth.PermissionAgentsUse, scope)
			if origin.mode() == "shared" {
				e.Authorization.Checks = append(e.Authorization.Checks, audit.SlackThreadCheck{Type: "slack_thread", SessionID: e.SessionID, ThreadID: caller.SlackThreadID, UserID: caller.ID, Result: "passed"})
			} else {
				e.Authorization.Checks = append(e.Authorization.Checks, audit.OriginCheck{Type: "session_origin", SessionID: e.SessionID, InitiatorKind: caller.Kind, InitiatorID: caller.ID, Result: "passed"})
			}
		}
		if err != nil {
			return err
		}
		callerAuthentication(&e.Evidence, caller)
	}
	var computer *uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT deployment_id,computer_id FROM sessions WHERE environment_id=$1 AND id=$2`, e.EnvironmentID, e.SessionID).Scan(&e.DeploymentID, &computer); err != nil {
		return err
	}
	if computer != nil {
		e.ComputerID = *computer
	}
	return audit.Append(ctx, tx, policy, e)
}

// discloseTransaction commits admission before content can leave the owner.
// Metadata-only reads retain their separate read-only transaction.
func discloseTransaction(ctx context.Context, policy auditpolicy.Policy, pool db.TxBeginner, caller Caller, env, session, turn uuid.UUID, read func(pgx.Tx) error) error {
	return hideMissing(db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SET TRANSACTION ISOLATION LEVEL REPEATABLE READ`); err != nil {
			return err
		}
		if err := authorizeRead(ctx, tx, caller, env); err != nil {
			return err
		}
		if err := read(tx); err != nil {
			return err
		}
		return auditOperation(ctx, tx, policy, caller, auth.PermissionSessionsRead, audit.Event{EnvironmentID: env, SessionID: session, TurnID: turn, Action: "session.content_read", TargetKind: "session", TargetID: session, Details: audit.LifecycleDetails{}})
	}))
}

// auditRuntimeEvent follows the runtime owner's physical/generation check. It
// records an admitted outcome without reauthorizing it as new origin work.
func auditRuntimeEvent(ctx context.Context, tx pgx.Tx, policy auditpolicy.Policy, execution Execution, turn uuid.UUID, action, outcome string, details audit.Details) error {
	origin, err := loadSessionOrigin(ctx, tx, execution.EnvironmentID, execution.SessionID)
	if err != nil {
		return err
	}
	event := audit.Event{Evidence: runtimeAuditEvidence(execution), EnvironmentID: execution.EnvironmentID, SessionID: execution.SessionID, TurnID: turn, Action: action, Outcome: outcome, TargetKind: "turn", TargetID: turn, Causality: audit.Causality{InitiatorKind: origin.kind(), InitiatorID: origin.id()}, Details: details}
	if action == "session.content_read" {
		event.TargetKind, event.TargetID = "session", execution.SessionID
	}
	var computer *uuid.UUID
	if err = tx.QueryRow(ctx, `SELECT deployment_id,computer_id FROM sessions WHERE environment_id=$1 AND id=$2`, execution.EnvironmentID, execution.SessionID).Scan(&event.DeploymentID, &computer); err != nil {
		return err
	}
	if computer != nil {
		event.ComputerID = *computer
	}
	return audit.Append(ctx, tx, policy, event)
}

func auditServiceFact(ctx context.Context, tx pgx.Tx, policy auditpolicy.Policy, source audit.Component, e audit.Event) error {
	return auditActorFact(ctx, tx, policy, audit.Internal(source), e)
}

func auditActorFact(ctx context.Context, tx pgx.Tx, policy auditpolicy.Policy, actor audit.Evidence, e audit.Event) error {
	e.Evidence = actor
	if actor.Actor.SessionID != uuid.Nil() {
		origin, err := loadSessionOrigin(ctx, tx, e.EnvironmentID, actor.Actor.SessionID)
		if err != nil {
			return err
		}
		e.Causality = audit.Causality{InitiatorKind: origin.kind(), InitiatorID: origin.id()}
	}
	var computer *uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT deployment_id,computer_id FROM sessions WHERE environment_id=$1 AND id=$2`, e.EnvironmentID, e.SessionID).Scan(&e.DeploymentID, &computer); err != nil {
		return err
	}
	if computer != nil {
		e.ComputerID = *computer
	}
	return audit.Append(ctx, tx, policy, e)
}

func auditWorkerActor(host workergroup.HostPrincipal, e Execution) audit.Evidence {
	return audit.AssignedWorker(host.HostID, host.HostClaimVersion, "session", e.SessionID, e.ProcessEpoch)
}

// RecordContentRead joins a caller-owned bounded content selection transaction.
// The caller must withhold the selected content unless that transaction commits.
func RecordContentRead(ctx context.Context, policy auditpolicy.Policy, tx pgx.Tx, caller Caller, env, session uuid.UUID) error {
	return auditOperation(ctx, tx, policy, caller, auth.PermissionSessionsRead, audit.Event{EnvironmentID: env, SessionID: session, Action: "session.content_read", TargetKind: "session", TargetID: session, Details: audit.LifecycleDetails{}})
}

// runtimeAuditEvidence records only transport versions provided by the authenticated
// boundary; worker boot epochs are not credential versions.
func runtimeAuditEvidence(e Execution) audit.Evidence {
	return audit.RuntimeActor(e.SessionID, e.AuthorityGeneration, e.ProcessEpoch, e.WorkerHostID, e.AuthenticatedHostVersion)
}

func callerRuntimeAuditEvidence(caller Caller) audit.Evidence {
	e := caller.Execution
	if caller.Host != nil {
		e.WorkerHostID = caller.Host.HostID
		e.AuthenticatedHostVersion = caller.Host.HostClaimVersion
	}
	return runtimeAuditEvidence(e)
}
func callerAuthentication(e *audit.Evidence, caller Caller) {
	if caller.CredentialKind == "slack" {
		e.Authentication = audit.Authentication{Origin: "request", Result: "verified", Provider: "slack", CredentialKind: "provider_assertion"}
	} else if caller.CredentialKind != "" {
		e.Authentication.CredentialKind = caller.CredentialKind
	}
}

func runtimeGenerationRefusal(e Execution, observed int64) error {
	return &audit.Refusal{Cause: ErrDenied, Authorization: audit.Authorization{Decision: "denied", Checks: []audit.Check{audit.RuntimeBindingCheck{Type: "runtime_binding", SessionID: e.SessionID, Generation: e.AuthorityGeneration, ProcessEpoch: e.ProcessEpoch, Result: "failed", Reason: "generation_changed", ObservedGeneration: observed}}}}
}
