package session

import (
	"context"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run"
	"github.com/jackc/pgx/v5"
)

// The Apply operations run one public Session operation in its own
// transaction. The caller authenticates and authorizes its principal first.
// A business rejection commits with its receipt and is then returned with
// that receipt as an *OperationError carrying the rejection code.

// ApplyAdmission sends, enqueues or messages a Turn as Admit does.
func ApplyAdmission(ctx context.Context, txb db.TxBeginner, request AdmissionRequest) (AdmissionReceipt, error) {
	var receipt AdmissionReceipt
	err := db.RunTx(ctx, txb, func(tx pgx.Tx) error {
		var err error
		receipt, err = Admit(ctx, tx, request)
		return err
	})
	if err == nil && receipt.Code != "" {
		err = &OperationError{Code: receipt.Code}
	}
	return receipt, err
}

// ApplyClose closes the Session as Close does.
func ApplyClose(ctx context.Context, txb db.TxBeginner, request ControlRequest) (ControlReceipt, error) {
	var receipt ControlReceipt
	err := db.RunTx(ctx, txb, func(tx pgx.Tx) error {
		var err error
		receipt, err = Close(ctx, tx, request)
		return err
	})
	if err == nil && receipt.Code != "" {
		err = &OperationError{Code: receipt.Code}
	}
	return receipt, err
}

// ApplyResume reads the Session without a lock to find its Computer, locks
// that Computer's complete admission Secret set and then resumes the held
// Session with those Secrets as resumeWithLockedSecrets does.
func ApplyResume(ctx context.Context, txb db.TxBeginner, request ResumeRequest) (ControlReceipt, error) {
	var receipt ControlReceipt
	err := db.RunTx(ctx, txb, func(tx pgx.Tx) error {
		var err error
		receipt, err = resume(ctx, tx, request)
		return err
	})
	if err == nil && receipt.Code != "" {
		err = &OperationError{Code: receipt.Code}
	}
	return receipt, err
}

// ApplyCancel locks the Session's control graph and then cancels the
// Session as Cancel does.
func ApplyCancel(ctx context.Context, txb db.TxBeginner, request ControlRequest) (ControlReceipt, error) {
	var receipt ControlReceipt
	err := db.RunTx(ctx, txb, func(tx pgx.Tx) error {
		graph, err := lockControlGraph(ctx, tx, request.Target)
		if err != nil {
			return err
		}
		receipt, err = Cancel(ctx, tx, request, graph)
		return err
	})
	if err == nil && receipt.Code != "" {
		err = &OperationError{Code: receipt.Code}
	}
	return receipt, err
}

// ApplyInterrupt locks the Session's control graph and then interrupts the
// Turn as InterruptTurn does. The receipt carries the hold only when the
// interruption was accepted.
func ApplyInterrupt(ctx context.Context, txb db.TxBeginner, request InterruptRequest) (ControlReceipt, error) {
	var receipt InterruptReceipt
	err := db.RunTx(ctx, txb, func(tx pgx.Tx) error {
		graph, err := lockControlGraph(ctx, tx, request.Target)
		if err != nil {
			return err
		}
		receipt, err = InterruptTurn(ctx, tx, request.EnvironmentID, request.SessionID, request.TurnID, request.IdempotencyKey, graph)
		return err
	})
	result := ControlReceipt{ID: receipt.ID, SessionID: request.SessionID, TurnID: &request.TurnID, Status: receipt.Status, Code: receipt.Code}
	if receipt.Status == "accepted" {
		result.HoldID = &receipt.HoldID
	}
	if err == nil && result.Code != "" {
		err = &OperationError{Code: result.Code}
	}
	return result, err
}

// noControlGraph is the control graph of a Session with no current Run. Cancel
// and InterruptTurn request a held execution's stop only for a current Run,
// so they never use it.
var noControlGraph run.OwnedFinalization

// lockControlGraph acquires, before any Session or operation claim, the
// owned graph of the Session's current Run, so a control that may retire a
// parked execution never acquires descendant Run locks after it entered the
// Session owner. It reads the Session without a lock, locks the current
// Run's attempt Secrets and owned finalization graph, and reads the Session
// again so a concurrent replacement cannot inherit the graph's authority.
// With no current Run, or when a concurrent control retired the Run while
// the graph was locking, it locks the Session before its Computer, so a
// resume cannot admit a new Run before the control, and returns
// noControlGraph. A Session whose current Run or generation changed is
// ErrAuthority.
func lockControlGraph(ctx context.Context, tx pgx.Tx, target Target) (run.OwnedFinalization, error) {
	q := db.New(tx)
	actor, err := q.GetActor(ctx, db.GetActorParams{EnvironmentID: pgvalue.UUID(target.EnvironmentID), ID: pgvalue.UUID(target.SessionID)})
	if err != nil {
		return noControlGraph, err
	}
	if !actor.CurrentRunID.Valid {
		current, err := q.LockSessionTurnAuthority(ctx, db.LockSessionTurnAuthorityParams{EnvironmentID: actor.EnvironmentID, ID: actor.ID})
		if err != nil {
			return noControlGraph, err
		}
		if current.CurrentRunID.Valid || current.RunGeneration != actor.RunGeneration {
			return noControlGraph, ErrAuthority
		}
		return noControlGraph, nil
	}
	graph, err := run.LockOwnedFinalizationWithSecrets(ctx, tx, run.SessionRunSecrets{EnvironmentID: target.EnvironmentID, RunID: pgvalue.MustUUIDValue(actor.CurrentRunID), ComputerID: pgvalue.MustUUIDValue(actor.ComputerID)})
	if err != nil {
		return graph, err
	}
	current, err := q.GetActor(ctx, db.GetActorParams{EnvironmentID: actor.EnvironmentID, ID: actor.ID})
	if err == nil && !current.CurrentRunID.Valid {
		current, err = q.LockSessionTurnAuthority(ctx, db.LockSessionTurnAuthorityParams{EnvironmentID: actor.EnvironmentID, ID: actor.ID})
		if err != nil {
			return graph, err
		}
		if current.CurrentRunID.Valid {
			return graph, ErrAuthority
		}
		return noControlGraph, nil
	}
	if err == nil && (current.CurrentRunID != actor.CurrentRunID || current.RunGeneration != actor.RunGeneration) {
		err = ErrAuthority
	}
	return graph, err
}
