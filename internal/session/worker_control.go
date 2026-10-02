package session

import (
	"context"
	"errors"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// lockControlFromRun authorizes a worker Session control (cancel, interrupt,
// resume) from the source Run. It locks the complete Secret union of the
// source Computer and the target Session's Computer before the execution
// fence: with the target's owned graph for an interruption, with the target
// Session otherwise. It then checks the source's owning ancestor Sessions,
// reads the target again and requires its Computer, current Run and
// generation unchanged (run.ErrExecutionTargetChanged); an immutable Computer
// mismatch remains ErrAuthority. It locks the target's Computer for a
// resume or cancel of a Session with no current Run, and re-reads the
// Secret union with ControlSecrets.Recheck: a new binding invalidates this
// attempt rather than acquiring a Secret out of order. It returns the source,
// the interruption's owned graph and the target Computer's locked Secret
// bindings. A target outside the source's environment is session_not_found.
func lockControlFromRun(ctx context.Context, tx pgx.Tx, fence run.ExecutionFence, sessionID uuid.UUID, interrupt bool) (run.LiveSource, run.OwnedFinalization, []db.LockComputerSecretsForAdmissionRow, error) {
	fail := func(err error) (run.LiveSource, run.OwnedFinalization, []db.LockComputerSecretsForAdmissionRow, error) {
		return run.LiveSource{}, run.OwnedFinalization{}, nil, err
	}
	q := db.New(tx)
	targetID := pgvalue.UUID(sessionID)
	controls, err := run.LockControlSecrets(ctx, tx, fence, targetID)
	if err != nil {
		return fail(controlTargetError(err))
	}
	var source run.LiveSource
	var graph run.OwnedFinalization
	if interrupt {
		_, source, graph, err = controls.LockInterruptionLiveSource(ctx)
	} else {
		_, source, err = controls.LockLiveSource(ctx)
	}
	if err != nil {
		return fail(controlTargetError(err))
	}
	// The union operation has already locked all source-ancestor and target
	// Sessions. This validation cannot add a differently ordered Session lock.
	if err = lockSourceControlSessions(ctx, q, source, targetID); err != nil {
		return fail(err)
	}
	target := controls.Target()
	lockedTarget, err := q.GetSession(ctx, db.GetSessionParams{EnvironmentID: source.EnvironmentID(), ID: targetID})
	if err != nil {
		return fail(err)
	}
	if target.ComputerID != lockedTarget.ComputerID {
		return fail(ErrAuthority)
	}
	if target.CurrentRunID != lockedTarget.CurrentRunID || target.RunGeneration != lockedTarget.RunGeneration {
		return fail(run.ErrExecutionTargetChanged)
	}

	if !interrupt && !target.CurrentRunID.Valid {
		if _, err = computer.LockSessionComputer(ctx, tx, computer.SessionComputerRef{EnvironmentID: pgvalue.MustUUIDValue(target.EnvironmentID), ComputerID: pgvalue.MustUUIDValue(target.ComputerID), SessionID: pgvalue.MustUUIDValue(target.ID)}); err != nil {
			return fail(err)
		}
	}
	if err = controls.Recheck(ctx, interrupt); err != nil {
		return fail(err)
	}
	return source, graph, controls.TargetBindings(), nil
}

// controlTargetError reports a control target outside the source's
// environment as a missing Session.
func controlTargetError(err error) error {
	if errors.Is(err, run.ErrExecutionTargetNotFound) {
		return &OperationError{Code: "session_not_found"}
	}
	return err
}

// lockSourceControlSessions checks the Sessions that own the source Run's
// lineage. Owned Tasks retain their ancestor Actor's lifecycle fence even in
// a different Computer. Locking only the source Computer's Session would let
// reciprocal child controls each hold the other's graph root while waiting
// on its child. An owning Session whose current Run changed is run.ErrStaleSource;
// a held one is session_held, and one whose active Turn began settlement is
// turn_unsettled.
func lockSourceControlSessions(ctx context.Context, q db.Querier, source run.LiveSource, targetID pgtype.UUID) error {
	sessions, err := q.LockWorkerControlSessions(ctx, db.LockWorkerControlSessionsParams{EnvironmentID: source.EnvironmentID(), SourceRunID: source.RunID(), TargetSessionID: targetID})
	if err != nil {
		return err
	}
	for _, row := range sessions {
		if !row.SourceOwnerRunID.Valid {
			continue
		}
		session := row.Session
		if session.CurrentRunID != row.SourceOwnerRunID {
			return run.ErrStaleSource
		}
		if session.DispatchHoldID.Valid {
			return &OperationError{Code: "session_held"}
		}
		if session.ActiveTurnID.Valid {
			turn, err := q.GetSessionTurn(ctx, db.GetSessionTurnParams{EnvironmentID: session.EnvironmentID, SessionID: session.ID, ID: session.ActiveTurnID})
			if err != nil {
				return err
			}
			if turn.SettlementStartedAt.Valid {
				return &OperationError{Code: "turn_unsettled"}
			}
		}
	}
	return nil
}

// CancelFromRun cancels the Session that request.SessionID addresses, as
// Cancel does, with the owned graph locked as a worker Session control
// interruption locks it.
func CancelFromRun(ctx context.Context, txb db.TxBeginner, fence run.ExecutionFence, request ControlRequest) (ControlReceipt, error) {
	var receipt ControlReceipt
	err := db.RunTx(ctx, txb, func(tx pgx.Tx) error {
		source, graph, _, err := lockControlFromRun(ctx, tx, fence, request.SessionID, true)
		if err != nil {
			return err
		}
		receipt, err = Cancel(ctx, tx, ControlRequest{Target: sourceTarget(source, request.SessionID), IdempotencyKey: request.IdempotencyKey}, graph)
		return err
	})
	if err == nil && receipt.Code != "" {
		err = &OperationError{Code: receipt.Code}
	}
	return receipt, err
}

// InterruptFromRun interrupts the Turn of the Session that request.SessionID
// addresses, as InterruptTurn does, with the owned graph locked as a worker
// Session control interruption locks it.
func InterruptFromRun(ctx context.Context, txb db.TxBeginner, fence run.ExecutionFence, request InterruptRequest) (InterruptReceipt, error) {
	var receipt InterruptReceipt
	err := db.RunTx(ctx, txb, func(tx pgx.Tx) error {
		source, graph, _, err := lockControlFromRun(ctx, tx, fence, request.SessionID, true)
		if err != nil {
			return err
		}
		receipt, err = InterruptTurn(ctx, tx, pgvalue.MustUUIDValue(source.EnvironmentID()), request.SessionID, request.TurnID, request.IdempotencyKey, graph)
		return err
	})
	if err == nil && receipt.Code != "" {
		err = &OperationError{Code: receipt.Code}
	}
	return receipt, err
}

// ResumeFromRun resumes the held Session that request.SessionID addresses
// after authorizing the source Run as a worker Session control: it reads the
// target again and resumes it with the target Computer's Secret bindings the
// control locked.
func ResumeFromRun(ctx context.Context, txb db.TxBeginner, fence run.ExecutionFence, request ResumeRequest) (ControlReceipt, error) {
	var receipt ControlReceipt
	err := db.RunTx(ctx, txb, func(tx pgx.Tx) error {
		source, _, bindings, err := lockControlFromRun(ctx, tx, fence, request.SessionID, false)
		if err != nil {
			return err
		}
		target, err := db.New(tx).GetSession(ctx, db.GetSessionParams{EnvironmentID: source.EnvironmentID(), ID: pgvalue.UUID(request.SessionID)})
		if err != nil {
			return err
		}
		request.Target = sourceTarget(source, request.SessionID)
		receipt, err = resumeWithLockedSecrets(ctx, tx, request, target.ComputerID, bindings)
		return err
	})
	if err == nil && receipt.Code != "" {
		err = &OperationError{Code: receipt.Code}
	}
	return receipt, err
}
