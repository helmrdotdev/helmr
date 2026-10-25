package command

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/helmrdotdev/helmr/internal/access"
	"github.com/helmrdotdev/helmr/internal/audit"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"github.com/helmrdotdev/helmr/internal/auth"

	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/idempotency"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
)

// CancelReceipt is the idempotency receipt of a Command cancellation: the
// claim that accepted it, the Command it targets and the accepted status.
type CancelReceipt struct {
	ID       string `json:"id"`
	TargetID string `json:"target_id"`
	Status   string `json:"status"`
}

// Cancel requests cancellation of the Command once: a pending Command is
// cancelled, an assigned one stops until its worker host reports the result,
// and a terminal one is unchanged. A repeated request replays the first
// receipt without changing the Command, also after its result was pruned.
// In one transaction Cancel reads the Command in its scope, acquires the
// Computer lock before the idempotency claim and Command update, so cancelling
// the last unallocated demand releases its execution reservation atomically.
func Cancel(ctx context.Context, auditPolicy auditpolicy.Policy, txb db.TxBeginner, principal auth.Principal, ref Ref) (CancelReceipt, error) {
	var receipt CancelReceipt
	err := db.RunTx(ctx, txb, func(tx pgx.Tx) error {
		scope := auth.Scope{OrgID: ref.OrgID, ProjectID: ref.ProjectID.String(), EnvironmentID: ref.EnvironmentID.String()}
		current, err := access.Current(ctx, tx, principal, scope)
		if err != nil {
			return err
		}
		if !current.HasPermission(auth.PermissionComputersCancelCommand, scope) {
			return access.ErrDenied
		}
		receipt, err = cancel(ctx, auditPolicy, tx, principal, ref)
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return CancelReceipt{}, ErrNotFound
	}
	if err != nil {
		return CancelReceipt{}, err
	}
	return receipt, nil
}

func cancel(ctx context.Context, auditPolicy auditpolicy.Policy, tx pgx.Tx, principal auth.Principal, ref Ref) (CancelReceipt, error) {
	q := db.New(tx)
	c, err := q.GetCommand(ctx, ref.params())
	if err != nil {
		return CancelReceipt{}, err
	}
	if _, err = tx.Exec(ctx, `SELECT id FROM computers WHERE environment_id=$1 AND id=$2 FOR NO KEY UPDATE`, ref.EnvironmentID, c.ComputerID); err != nil {
		return CancelReceipt{}, err
	}
	scope := auth.Scope{OrgID: ref.OrgID, ProjectID: ref.ProjectID.String(), EnvironmentID: ref.EnvironmentID.String()}
	current, err := access.Current(ctx, tx, principal, scope)
	if err != nil {
		return CancelReceipt{}, err
	}
	if !current.HasPermission(auth.PermissionComputersCancelCommand, scope) {
		return CancelReceipt{}, access.ErrDenied
	}
	request, err := idempotency.NewCommandCancelRequest(ref.EnvironmentID, ref.CommandID)
	if err != nil {
		return CancelReceipt{}, err
	}
	var result CancelReceipt
	err = idempotency.Run(ctx, tx, request, func(tx pgx.Tx, acquired idempotency.Result) (idempotency.Completion, error) {
		q := db.New(tx)
		want := CancelReceipt{ID: pgvalue.UUIDString(acquired.Claim.ID), TargetID: ref.CommandID.String(), Status: "accepted"}
		if !acquired.New {
			var replayed CancelReceipt
			if err := json.Unmarshal(acquired.Claim.Receipt, &replayed); err != nil {
				return idempotency.Completion{}, fmt.Errorf("%w: %v", ErrReceiptInvalid, err)
			}
			if replayed != want {
				return idempotency.Completion{}, fmt.Errorf("%w: command cancellation receipt differs from its claim", ErrReceiptInvalid)
			}
			result = replayed
			return idempotency.Completion{}, nil
		}
		changed, changeErr := q.RequestCommandCancellation(ctx, db.RequestCommandCancellationParams{
			EnvironmentID: pgvalue.UUID(ref.EnvironmentID), CommandID: pgvalue.UUID(ref.CommandID),
		})
		if changeErr != nil && !errors.Is(changeErr, pgx.ErrNoRows) {
			return idempotency.Completion{}, changeErr
		}
		body, err := json.Marshal(want)
		if err != nil {
			return idempotency.Completion{}, err
		}

		actor, err := audit.Authorized(current, auth.PermissionComputersCancelCommand, scope)
		if err != nil {
			return idempotency.Completion{}, err
		}
		if err = audit.Append(ctx, tx, auditPolicy, audit.Event{Actor: actor, EnvironmentID: ref.EnvironmentID, ComputerID: pgvalue.MustUUIDValue(c.ComputerID), Action: "command.cancel_requested", Outcome: "accepted", TargetKind: "command", TargetID: ref.CommandID, Details: audit.LifecycleDetails{RequestID: pgvalue.MustUUIDValue(acquired.Claim.ID)}}); err != nil {
			return idempotency.Completion{}, err
		}
		if changeErr == nil && changed.Status == "cancelled" {
			if err = audit.Append(ctx, tx, auditPolicy, audit.Event{Actor: actor, EnvironmentID: ref.EnvironmentID, ComputerID: pgvalue.MustUUIDValue(c.ComputerID), Action: "command.cancelled", Outcome: "cancelled", TargetKind: "command", TargetID: ref.CommandID, Details: audit.LifecycleDetails{RequestID: pgvalue.MustUUIDValue(acquired.Claim.ID)}}); err != nil {
				return idempotency.Completion{}, err
			}
		}
		result = want
		return idempotency.Completion{Target: idempotency.Target{CommandID: ref.CommandID}, Receipt: body}, nil
	})
	if err != nil {
		return CancelReceipt{}, err
	}
	if err := computer.RetireUnusedExecutionAdmission(ctx, tx, ref.EnvironmentID, pgvalue.MustUUIDValue(c.ComputerID)); err != nil {
		return CancelReceipt{}, err
	}
	return result, nil
}
