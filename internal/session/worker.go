package session

import (
	"context"
	"errors"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run"
	"github.com/jackc/pgx/v5"
)

// The FromRun operations in this file and in worker_control.go act on a
// Session for a worker from the live source Run its execution fence
// addresses, each in its own transaction. The addressed Session is always in
// the source Run's environment: a Session outside it is reported as
// session_not_found or ErrNotFound. A missing or mismatched source is
// run.ErrStaleSource; stale worker claims are workergroup.ErrStaleClaims. A
// business rejection commits with its receipt and is then returned with that
// receipt as an *OperationError carrying the rejection code. The operations
// on the worker's own Actor execution report a lost or changed execution as
// ErrStaleOutput or ErrStaleExecution, or with the stale error they
// document.

// authorizeFromRun locks the source Computer's Secrets, then the execution
// fence together with the addressed Session, then the source attempt's
// delivery Secrets, and checks the source Session as CheckSourceSession
// does.
func authorizeFromRun(ctx context.Context, tx pgx.Tx, fence run.ExecutionFence, sessionID uuid.UUID) (run.LiveSource, error) {
	secrets, err := run.LockSourceSecretsForSession(ctx, tx, fence, pgvalue.UUID(sessionID))
	if err != nil {
		return run.LiveSource{}, err
	}
	authority, source, err := secrets.LockLiveSource(ctx)
	if errors.Is(err, run.ErrExecutionTargetNotFound) {
		return run.LiveSource{}, &OperationError{Code: "session_not_found"}
	}
	if err != nil {
		return run.LiveSource{}, err
	}
	if err = secrets.ValidateSourceDelivery(ctx); err != nil {
		return run.LiveSource{}, err
	}
	if err = CheckSourceSession(ctx, db.New(tx), authority.Session()); err != nil {
		return run.LiveSource{}, err
	}
	return source, nil
}

// sourceTarget addresses the Session in the source Run's environment.
func sourceTarget(source run.LiveSource, sessionID uuid.UUID) Target {
	return Target{EnvironmentID: pgvalue.MustUUIDValue(source.EnvironmentID()), SessionID: sessionID}
}

// AdmitFromRun sends, enqueues or messages a Turn of the Session that
// request.SessionID addresses, as Admit does, after authorizing the source
// Run as a worker Session operation. The source's environment and Run
// replace request.EnvironmentID and request.SourceRunID.
func AdmitFromRun(ctx context.Context, txb db.TxBeginner, fence run.ExecutionFence, request AdmissionRequest) (AdmissionReceipt, error) {
	var receipt AdmissionReceipt
	err := db.RunTx(ctx, txb, func(tx pgx.Tx) error {
		source, err := authorizeFromRun(ctx, tx, fence, request.SessionID)
		if err != nil {
			return err
		}
		request.Target = sourceTarget(source, request.SessionID)
		request.SourceRunID = pgvalue.MustUUIDValue(source.RunID())
		receipt, err = Admit(ctx, tx, request)
		return err
	})
	if err == nil && receipt.Code != "" {
		err = &OperationError{Code: receipt.Code}
	}
	return receipt, err
}

// CloseFromRun closes the Session that request.SessionID addresses, as Close
// does, after authorizing the source Run as a worker Session operation.
func CloseFromRun(ctx context.Context, txb db.TxBeginner, fence run.ExecutionFence, request ControlRequest) (ControlReceipt, error) {
	var receipt ControlReceipt
	err := db.RunTx(ctx, txb, func(tx pgx.Tx) error {
		source, err := authorizeFromRun(ctx, tx, fence, request.SessionID)
		if err != nil {
			return err
		}
		receipt, err = Close(ctx, tx, ControlRequest{Target: sourceTarget(source, request.SessionID), IdempotencyKey: request.IdempotencyKey})
		return err
	})
	if err == nil && receipt.Code != "" {
		err = &OperationError{Code: receipt.Code}
	}
	return receipt, err
}

// ReadEventsFromRun reads a page of the Session's events, as ReadEvents
// does, after authorizing the source Run as a worker Session operation.
func ReadEventsFromRun(ctx context.Context, txb db.TxBeginner, fence run.ExecutionFence, sessionID uuid.UUID, after int64, limit int32) (EventPage, error) {
	var page EventPage
	err := db.RunTx(ctx, txb, func(tx pgx.Tx) error {
		source, err := authorizeFromRun(ctx, tx, fence, sessionID)
		if err != nil {
			return err
		}
		page, err = ReadEvents(ctx, db.New(tx), sourceTarget(source, sessionID), after, limit)
		return err
	})
	return page, err
}

// GetFromRun locks the live source Run and then reads the snapshot of the
// Session in the source's organization, project and environment. A Session
// outside that scope is ErrNotFound.
func GetFromRun(ctx context.Context, txb db.TxBeginner, fence run.ExecutionFence, sessionID uuid.UUID) (db.GetSessionSnapshotRow, error) {
	var row db.GetSessionSnapshotRow
	err := db.RunTx(ctx, txb, func(tx pgx.Tx) error {
		source, err := run.LockLiveSource(ctx, tx, fence)
		if err != nil {
			return err
		}
		row, err = Get(ctx, db.New(tx), Scope{OrgID: source.OrgID(), ProjectID: source.ProjectID(), EnvironmentID: source.EnvironmentID()}, pgvalue.UUID(sessionID))
		return err
	})
	return row, err
}

// GetTurnFromRun locks the live source Run and then reads the Turn of the
// Session in the source's environment, as GetTurn does.
func GetTurnFromRun(ctx context.Context, txb db.TxBeginner, fence run.ExecutionFence, sessionID, turnID uuid.UUID) (TurnView, error) {
	var view TurnView
	err := db.RunTx(ctx, txb, func(tx pgx.Tx) error {
		source, err := run.LockLiveSource(ctx, tx, fence)
		if err != nil {
			return err
		}
		view, err = GetTurn(ctx, db.New(tx), sourceTarget(source, sessionID), turnID)
		return err
	})
	return view, err
}
