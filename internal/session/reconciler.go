package session

import (
	"context"
	"errors"
	"fmt"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

type Reconciler struct {
	db db.TxDB
}

func NewReconciler(database db.TxDB) (*Reconciler, error) {
	if database == nil {
		return nil, errors.New("session reconciliation database is required")
	}
	return &Reconciler{db: database}, nil
}

func (r *Reconciler) ReconcileLifecycle(
	ctx context.Context,
	environmentID uuid.UUID,
	sessionID uuid.UUID,
) (deferred bool, returnErr error) {
	locator, err := db.New(r.db).GetSession(ctx, db.GetSessionParams{
		EnvironmentID: pgvalue.UUID(environmentID),
		ID:            pgvalue.UUID(sessionID),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if locator.Status != "open" && locator.Status != "closing" && locator.DispatchHoldReason.String != "recovery_required" && locator.DispatchHoldReason.String != "interrupt_requested" {
		return false, nil
	}
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("begin session lifecycle reconciliation: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	q := db.New(tx)
	bindings, err := q.LockComputerSecretsForAdmission(ctx, locator.ComputerID)
	if err != nil {
		return false, err
	}
	if err := lockSessionComputer(ctx, tx, locator.EnvironmentID, locator.ComputerID); err != nil {
		return false, err
	}
	session, err := q.LockSessionClose(ctx, db.LockSessionCloseParams{
		EnvironmentID: pgvalue.UUID(environmentID),
		SessionID:     pgvalue.UUID(sessionID),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, tx.Commit(ctx)
	}
	if err != nil {
		return false, err
	}
	if session.ComputerID != locator.ComputerID {
		return false, ErrAuthority
	}
	session, deferred, err = reconcileStoppedExecution(ctx, tx, session)
	if err != nil {
		return false, err
	}
	if deferred {
		return true, tx.Commit(ctx)
	}
	session, deferred, err = reconcileLostExecution(ctx, tx, session, bindings)
	if err != nil {
		return false, err
	}
	if deferred {
		return true, tx.Commit(ctx)
	}
	// Completion commits before process cleanup. The same durable lifecycle
	// intent admits a continuation only after the previous scopes are excluded.
	if session.Status == "open" && !session.CancelRequestedAt.Valid && CanStartContinuation(session) {
		locked, err := computer.LockSessionComputer(ctx, tx, sessionComputerRef(session))
		if err != nil {
			return false, err
		}
		sessionComputer := locked.Computer()
		if !bindingsCanAdmit(session, bindings) {
			return true, tx.Commit(ctx)
		}
		if _, err = CreateContinuation(ctx, tx, session, sessionComputer, bindings); errors.Is(err, pgx.ErrNoRows) {
			return true, tx.Commit(ctx)
		} else if err != nil {
			return false, err
		}
		return false, tx.Commit(ctx)
	}
	_, deferred, err = ReconcileClose(ctx, tx, session, bindings)
	if err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return deferred, nil
}

// ReconcileInput repairs append delivery after the append transaction. It is safe to
// run after an in-transaction match or continuation because every transition is
// guarded by the Session current-Run CAS and the pending Wait CAS.
func (r *Reconciler) ReconcileInput(
	ctx context.Context,
	environmentID uuid.UUID,
	sessionID uuid.UUID,
	turnID uuid.UUID,
) (deferred bool, returnErr error) {
	locator, err := db.New(r.db).GetSession(ctx, db.GetSessionParams{
		EnvironmentID: pgvalue.UUID(environmentID), ID: pgvalue.UUID(sessionID),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("begin session input reconciliation: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	q := db.New(tx)
	bindings, err := q.LockComputerSecretsForAdmission(ctx, locator.ComputerID)
	if err != nil {
		return false, err
	}
	if err := lockSessionComputer(ctx, tx, locator.EnvironmentID, locator.ComputerID); err != nil {
		return false, err
	}
	session, err := q.LockSessionForInputReconcile(ctx, db.LockSessionForInputReconcileParams{
		EnvironmentID: pgvalue.UUID(environmentID), SessionID: pgvalue.UUID(sessionID),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, tx.Commit(ctx)
	}
	if err != nil {
		return false, err
	}
	if session.ComputerID != locator.ComputerID {
		return false, ErrAuthority
	}
	// Cancellation, holds and terminal Sessions leave settlement to their owners.
	// Their lifecycle or resume path arranges any subsequent input delivery.
	if session.CancelRequestedAt.Valid || session.DispatchHoldID.Valid || (session.Status != "open" && session.Status != "closing") {
		return false, tx.Commit(ctx)
	}
	var currentRun db.Run
	if session.CurrentRunID.Valid {
		currentRun, err = q.LockSessionInputCurrentRun(ctx, db.LockSessionInputCurrentRunParams{
			EnvironmentID: session.EnvironmentID, RunID: session.CurrentRunID, SessionID: session.ID,
		})
		if err != nil {
			return false, err
		}
	}
	locked, err := computer.LockSessionComputer(ctx, tx, sessionComputerRef(session))
	if err != nil {
		return false, err
	}
	sessionComputer := locked.Computer()
	if session.CurrentRunID.Valid {
		attempt, err := q.LockRunLeaseClaimAttempt(ctx, db.LockRunLeaseClaimAttemptParams{
			RunID: currentRun.ID, Number: currentRun.CurrentAttemptNumber, ComputerID: sessionComputer.ID,
		})
		if err != nil {
			return false, err
		}
		if attempt.TerminalAt.Valid {
			return false, tx.Commit(ctx)
		}
	}
	_, err = q.GetSessionTurn(ctx, db.GetSessionTurnParams{
		EnvironmentID: session.EnvironmentID, SessionID: session.ID, ID: pgvalue.UUID(turnID),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, tx.Commit(ctx)
	}
	if err != nil {
		return false, err
	}
	wait, err := q.GetPendingSessionInputRunWait(ctx, db.GetPendingSessionInputRunWaitParams{
		EnvironmentID: session.EnvironmentID, SessionID: session.ID,
		RunID: currentRun.ID, AttemptNumber: currentRun.CurrentAttemptNumber,
		AfterInputSequence: pgtype.Int8{Int64: session.CommittedInputSequence, Valid: true},
	})
	if err == nil {
		if _, err := resolveInputWait(ctx, tx, session, wait); err != nil {
			return false, err
		}
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return false, err
	}
	if CanStartContinuation(session) {
		if _, err := CreateContinuation(ctx, tx, session, sessionComputer, bindings); errors.Is(err, pgx.ErrNoRows) {
			if err := tx.Commit(ctx); err != nil {
				return false, err
			}
			return true, nil
		} else if err != nil {
			return false, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return false, nil
}

// ReconcileTimeouts also delivers ready input and drained close before considering
// an elapsed timeout. Its count includes only newly committed timeout failures.
func (r *Reconciler) ReconcileTimeouts(ctx context.Context, limit int32) (int, error) {
	if limit <= 0 {
		return 0, nil
	}
	candidates, err := db.New(r.db).ListPendingSessionInputWaitTimeouts(ctx, limit)
	if err != nil {
		return 0, err
	}
	resolved := 0
	for _, candidate := range candidates {
		timedOut := false
		err := db.RunTx(ctx, r.db, func(tx pgx.Tx) error {
			if !candidate.SessionID.Valid || !candidate.AfterInputSequence.Valid {
				return ErrAuthority
			}
			q := db.New(tx)
			if _, err := q.LockComputerSecretsForAdmission(ctx, candidate.ComputerID); err != nil {
				return err
			}
			if err := lockSessionComputer(ctx, tx, candidate.EnvironmentID, candidate.ComputerID); err != nil {
				return err
			}
			session, err := q.LockSessionForInputReconcile(ctx, db.LockSessionForInputReconcileParams{
				EnvironmentID: candidate.EnvironmentID, SessionID: candidate.SessionID,
			})
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			if err != nil {
				return err
			}
			if session.ComputerID != candidate.ComputerID {
				return ErrAuthority
			}
			if session.DispatchHoldID.Valid || session.CancelRequestedAt.Valid || session.CurrentRunID != candidate.RunID {
				return nil
			}
			current, err := q.LockSessionInputCurrentRun(ctx, db.LockSessionInputCurrentRunParams{
				EnvironmentID: candidate.EnvironmentID, RunID: candidate.RunID, SessionID: candidate.SessionID,
			})
			if err != nil {
				return err
			}
			attempt, err := q.LockRunLeaseClaimAttempt(ctx, db.LockRunLeaseClaimAttemptParams{
				RunID: current.ID, Number: current.CurrentAttemptNumber, ComputerID: session.ComputerID,
			})
			if err != nil {
				return err
			}
			if attempt.TerminalAt.Valid || current.CurrentAttemptNumber != candidate.AttemptNumber {
				return nil
			}
			wait, err := q.GetPendingSessionInputRunWait(ctx, db.GetPendingSessionInputRunWaitParams{
				EnvironmentID: candidate.EnvironmentID, SessionID: candidate.SessionID,
				RunID: candidate.RunID, AttemptNumber: candidate.AttemptNumber,
				AfterInputSequence: candidate.AfterInputSequence,
			})
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			if err != nil {
				return err
			}
			if wait.ID != candidate.ID {
				return nil
			}
			settled, err := resolveInputWait(ctx, tx, session, wait)
			if err != nil {
				return err
			}
			timedOut = settled.ConditionStatus == "failed" && settled.ConditionReasonCode.String == "wait_timeout"
			return nil
		})
		if err != nil {
			return resolved, err
		}
		if timedOut {
			resolved++
		}
	}
	return resolved, nil
}
