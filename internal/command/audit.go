package command

import (
	"context"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/access"
	"github.com/helmrdotdev/helmr/internal/audit"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
)

func auditCompletion(ctx context.Context, tx pgx.Tx, policy auditpolicy.Policy, worker workergroup.HostPrincipal, lease Lease, c db.Command, p completion, generation int64) error {
	actor := audit.System("worker-host")
	actor.CredentialKind, actor.CredentialID, actor.CredentialVersion = "worker_host", worker.HostID, worker.HostClaimVersion
	actor.RuntimeGeneration = generation
	action, outcome := "command.completed", "succeeded"
	var details audit.Details = audit.LifecycleDetails{Generation: generation}
	switch p.status {
	case "cancelled":
		action, outcome = "command.cancelled", "cancelled"
	case "lost":
		action, outcome = "command.failed", "unknown"
		details = audit.FailureDetails{Reason: "operation_failed"}
	case "failed", "timed_out":
		action, outcome = "command.failed", "failed"
		details = audit.FailureDetails{Reason: "operation_failed"}
	}
	return audit.Append(ctx, tx, policy, audit.Event{Actor: actor, EnvironmentID: lease.EnvironmentID, ComputerID: lease.ComputerID, Action: action, Outcome: outcome, TargetKind: "command", TargetID: uuid.UUID(p.command.Bytes), Causality: audit.Causality{InitiatorKind: c.CallerKind, InitiatorID: pgvalue.MustUUIDValue(c.CallerID)}, Details: details})
}

// AdmitOutputRead rechecks the current reader and persists admission after a
// bounded output page has been selected, before the transport releases its bytes.
func AdmitOutputRead(ctx context.Context, policy auditpolicy.Policy, pool db.TxBeginner, principal auth.Principal, ref Ref) error {
	return db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		scope := auth.Scope{OrgID: ref.OrgID, ProjectID: ref.ProjectID.String(), EnvironmentID: ref.EnvironmentID.String()}
		current, err := access.Current(ctx, tx, principal, scope)
		if err != nil {
			return err
		}
		if !current.HasPermission(auth.PermissionComputersReadOutput, scope) {
			return access.ErrDenied
		}
		c, err := db.New(tx).GetCommand(ctx, ref.params())
		if err != nil {
			return err
		}
		actor, err := audit.Authorized(current, auth.PermissionComputersReadOutput, scope)
		if err != nil {
			return err
		}
		return audit.Append(ctx, tx, policy, audit.Event{Actor: actor, EnvironmentID: ref.EnvironmentID, ComputerID: pgvalue.MustUUIDValue(c.ComputerID), Action: "command.output_read", TargetKind: "command", TargetID: ref.CommandID, Details: audit.LifecycleDetails{}})
	})
}

// auditServiceFailure follows a fresh terminal transition under the Command lock.
func auditServiceFailure(ctx context.Context, tx pgx.Tx, policy auditpolicy.Policy, c db.Command, source, outcome, reason string) error {
	return audit.Append(ctx, tx, policy, audit.Event{Actor: audit.System(source), EnvironmentID: pgvalue.MustUUIDValue(c.EnvironmentID), ComputerID: pgvalue.MustUUIDValue(c.ComputerID), Action: "command.failed", Outcome: outcome, TargetKind: "command", TargetID: pgvalue.MustUUIDValue(c.ID), Causality: audit.Causality{InitiatorKind: c.CallerKind, InitiatorID: pgvalue.MustUUIDValue(c.CallerID)}, Details: audit.FailureDetails{Reason: reason}})
}
