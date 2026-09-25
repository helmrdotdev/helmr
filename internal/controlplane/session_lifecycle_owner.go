package controlplane

import (
	"context"
	"errors"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/jackc/pgx/v5"

	"github.com/helmrdotdev/helmr/internal/session"
)

// Public callers authenticate and authorize their principal before entering
// these owners. Worker callers additionally establish explicit lease authority.
// Business rejections commit before becoming transport errors.
func (s *Server) applySessionAdmission(ctx context.Context, request session.AdmissionRequest) (session.AdmissionReceipt, error) {
	var receipt session.AdmissionReceipt
	err := s.inTx(ctx, func(work *txWork) error {
		var err error
		receipt, err = session.Admit(ctx, work.q, request)
		return err
	})
	if err == nil && receipt.Code != "" {
		err = &session.OperationError{Code: receipt.Code}
	}
	return receipt, err
}

func (s *Server) applySessionClose(ctx context.Context, request session.ControlRequest) (session.ControlReceipt, error) {
	var receipt session.ControlReceipt
	err := s.inTx(ctx, func(work *txWork) error {
		var err error
		receipt, err = session.Close(ctx, work.q, request)
		return err
	})
	if err == nil && receipt.Code != "" {
		err = &session.OperationError{Code: receipt.Code}
	}
	return receipt, err
}

func (s *Server) applySessionResume(ctx context.Context, request session.ResumeRequest) (session.ControlReceipt, error) {
	var receipt session.ControlReceipt
	err := s.inTx(ctx, func(work *txWork) error {
		var err error
		receipt, err = session.Resume(ctx, work.q, request)
		return err
	})
	if err == nil && receipt.Code != "" {
		err = &session.OperationError{Code: receipt.Code}
	}
	return receipt, err
}

func (s *Server) applySessionCancel(ctx context.Context, request session.ControlRequest) (session.ControlReceipt, error) {
	var receipt session.ControlReceipt
	err := s.inTx(ctx, func(work *txWork) error {
		graph, err := lockSessionControlGraph(ctx, work, request.Target)
		if err != nil {
			return err
		}
		receipt, err = session.Cancel(ctx, work.q, request, graph)
		return err
	})
	if err == nil && receipt.Code != "" {
		err = &session.OperationError{Code: receipt.Code}
	}
	return receipt, err
}

func (s *Server) applySessionInterrupt(ctx context.Context, request session.InterruptRequest) (session.ControlReceipt, error) {
	var receipt session.InterruptReceipt
	err := s.inTx(ctx, func(work *txWork) error {
		graph, err := lockSessionControlGraph(ctx, work, request.Target)
		if err != nil {
			return err
		}
		receipt, err = session.InterruptTurn(ctx, work.q, request.EnvironmentID, request.SessionID, request.TurnID, request.IdempotencyKey, graph)
		return err
	})
	result := session.ControlReceipt{ID: receipt.ID, SessionID: request.SessionID, TurnID: &request.TurnID, Status: receipt.Status, Code: receipt.Code}
	if receipt.Status == "accepted" {
		result.HoldID = &receipt.HoldID
	}
	if err == nil && result.Code != "" {
		err = &session.OperationError{Code: result.Code}
	}
	return result, err
}

// Controls that may retire a parked execution acquire the existing owned graph
// before Session/operation claims. No path may acquire descendant Run locks
// after it has entered the Session owner. The locator is revalidated under that
// graph, so a concurrent replacement cannot inherit its authority.
func lockSessionControlGraph(ctx context.Context, work *txWork, target session.Target) (run.OwnedFinalization, error) {
	actor, err := work.q.GetActor(ctx, db.GetActorParams{EnvironmentID: pgvalue.UUID(target.EnvironmentID), ID: pgvalue.UUID(target.SessionID)})
	if err != nil {
		return run.OwnedFinalization{}, err
	}
	if !actor.CurrentRunID.Valid {
		locked, err := work.q.LockSessionTurnAuthority(ctx, db.LockSessionTurnAuthorityParams{EnvironmentID: actor.EnvironmentID, ID: actor.ID})
		if err != nil {
			return run.OwnedFinalization{}, err
		}
		if locked.CurrentRunID.Valid || locked.RunGeneration != actor.RunGeneration {
			return run.OwnedFinalization{}, session.ErrAuthority
		}
		return run.OwnedFinalization{}, nil
	}
	current, err := work.q.GetRun(ctx, db.GetRunParams{EnvironmentID: actor.EnvironmentID, ID: actor.CurrentRunID})
	if err != nil {
		return run.OwnedFinalization{}, err
	}
	if _, err = secret.LockAttemptDelivery(ctx, work.q, current.ID, current.CurrentAttemptNumber, actor.WorkspaceID); err != nil {
		return run.OwnedFinalization{}, err
	}
	tx, ok := work.tx.(pgx.Tx)
	if !ok {
		return run.OwnedFinalization{}, errors.New("session control transaction does not expose PostgreSQL authority")
	}
	graph, err := run.LockOwnedFinalization(ctx, tx, run.OwnedFinalizationRequest{OrgID: pgvalue.MustUUIDValue(current.OrgID), ProjectID: pgvalue.MustUUIDValue(current.ProjectID), EnvironmentID: target.EnvironmentID, RunID: pgvalue.MustUUIDValue(current.ID)})
	if err != nil {
		return graph, err
	}
	locked, err := work.q.GetActor(ctx, db.GetActorParams{EnvironmentID: actor.EnvironmentID, ID: actor.ID})
	if err == nil && !locked.CurrentRunID.Valid {
		// A concurrent control retired the Run while this graph was locking.
		// Lock the Session before returning an empty graph so Resume cannot
		// admit a new Run between this check and the control operation.
		locked, err = work.q.LockSessionTurnAuthority(ctx, db.LockSessionTurnAuthorityParams{EnvironmentID: actor.EnvironmentID, ID: actor.ID})
		if err != nil {
			return graph, err
		}
		if locked.CurrentRunID.Valid {
			return graph, session.ErrAuthority
		}
		return run.OwnedFinalization{}, nil
	}
	if err == nil && (locked.CurrentRunID != actor.CurrentRunID || locked.RunGeneration != actor.RunGeneration) {
		err = session.ErrAuthority
	}
	return graph, err
}
