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
// guarded by the Actor current-Run CAS and the pending Wait CAS.
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
	if err != nil || session.ComputerID != locator.ComputerID {
		return false, ErrAuthority
	}
	// Cancellation terminalizes queued input atomically under this same owner.
	// Its prior delivery records no longer need execution or Computer authority.
	if session.CancelRequestedAt.Valid {
		return false, tx.Commit(ctx)
	}
	var currentRun db.Run
	if session.CurrentRunID.Valid {
		currentRun, err = q.LockSessionInputCurrentRun(ctx, db.LockSessionInputCurrentRunParams{
			EnvironmentID: session.EnvironmentID, RunID: session.CurrentRunID, SessionID: session.ID,
		})
		if err != nil {
			return false, ErrAuthority
		}
	}
	locked, err := computer.LockOpenSessionComputer(ctx, tx, sessionComputerRef(session))
	if err != nil {
		return false, ErrAuthority
	}
	sessionComputer := locked.Computer()
	if session.CurrentRunID.Valid {
		attempt, err := q.LockRunLeaseClaimAttempt(ctx, db.LockRunLeaseClaimAttemptParams{
			RunID: currentRun.ID, Number: currentRun.CurrentAttemptNumber, ComputerID: sessionComputer.ID,
		})
		if err != nil || attempt.TerminalAt.Valid {
			return false, ErrAuthority
		}
	}
	turn, err := q.GetSessionTurnByIDForUpdate(ctx, db.GetSessionTurnByIDForUpdateParams{
		EnvironmentID: session.EnvironmentID, SessionID: session.ID, ID: pgvalue.UUID(turnID),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, tx.Commit(ctx)
	}
	if err != nil || !turn.ID.Valid || uuid.UUID(turn.ID.Bytes) != turnID {
		return false, ErrAuthority
	}
	wait, err := q.GetPendingSessionInputRunWait(ctx, db.GetPendingSessionInputRunWaitParams{
		EnvironmentID: session.EnvironmentID, SessionID: session.ID,
		RunID: currentRun.ID, AttemptNumber: currentRun.CurrentAttemptNumber,
		AfterInputSequence: pgtype.Int8{Int64: turn.Sequence - 1, Valid: true},
	})
	if err == nil {
		if _, err := CompleteWait(ctx, tx, wait, turn); err != nil {
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
		if !candidate.SessionID.Valid || !candidate.AfterInputSequence.Valid {
			return resolved, ErrAuthority
		}
		tx, err := r.db.Begin(ctx)
		if err != nil {
			return resolved, err
		}
		q := db.New(tx)
		_, err = q.LockComputerSecretsForAdmission(ctx, candidate.ComputerID)
		if err != nil {
			_ = tx.Rollback(context.Background())
			return resolved, err
		}
		if err := lockSessionComputer(ctx, tx, candidate.EnvironmentID, candidate.ComputerID); err != nil {
			_ = tx.Rollback(context.Background())
			return resolved, err
		}
		session, err := q.LockSessionForInputReconcile(ctx, db.LockSessionForInputReconcileParams{
			EnvironmentID: candidate.EnvironmentID, SessionID: candidate.SessionID,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			_ = tx.Rollback(context.Background())
			continue
		}
		if err != nil {
			_ = tx.Rollback(context.Background())
			return resolved, err
		}
		if session.DispatchHoldID.Valid || !session.CurrentRunID.Valid || session.CurrentRunID != candidate.RunID {
			_ = tx.Rollback(context.Background())
			continue
		}
		run, err := q.LockSessionInputCurrentRun(ctx, db.LockSessionInputCurrentRunParams{
			EnvironmentID: candidate.EnvironmentID, RunID: candidate.RunID, SessionID: candidate.SessionID,
		})
		if err != nil {
			_ = tx.Rollback(context.Background())
			return resolved, ErrAuthority
		}
		locked, err := computer.LockOpenSessionComputer(ctx, tx, computer.SessionComputerRef{
			EnvironmentID: pgvalue.MustUUIDValue(candidate.EnvironmentID),
			ComputerID:    pgvalue.MustUUIDValue(candidate.ComputerID),
			SessionID:     pgvalue.MustUUIDValue(candidate.SessionID),
		})
		if err != nil {
			_ = tx.Rollback(context.Background())
			return resolved, ErrAuthority
		}
		sessionComputer := locked.Computer()
		attempt, err := q.LockRunLeaseClaimAttempt(ctx, db.LockRunLeaseClaimAttemptParams{
			RunID: run.ID, Number: run.CurrentAttemptNumber, ComputerID: sessionComputer.ID,
		})
		if err != nil || attempt.TerminalAt.Valid {
			_ = tx.Rollback(context.Background())
			return resolved, ErrAuthority
		}
		wait, err := q.GetPendingSessionInputRunWait(ctx, db.GetPendingSessionInputRunWaitParams{
			EnvironmentID: candidate.EnvironmentID, SessionID: candidate.SessionID,
			RunID: candidate.RunID, AttemptNumber: candidate.AttemptNumber,
			AfterInputSequence: candidate.AfterInputSequence,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			_ = tx.Rollback(context.Background())
			continue
		}
		if err != nil {
			_ = tx.Rollback(context.Background())
			return resolved, err
		}
		if wait.ID != candidate.ID {
			_ = tx.Rollback(context.Background())
			continue
		}
		if !wait.TimeoutAt.Valid {
			_ = tx.Rollback(context.Background())
			return resolved, ErrAuthority
		}
		now, err := q.GetRunLeaseRenewalTime(ctx)
		if err != nil {
			_ = tx.Rollback(context.Background())
			return resolved, err
		}
		if !now.Valid {
			_ = tx.Rollback(context.Background())
			return resolved, ErrAuthority
		}
		if now.Time.Before(wait.TimeoutAt.Time) {
			_ = tx.Rollback(context.Background())
			continue
		}
		if _, err := FailWait(ctx, tx, wait, "wait_timeout"); err != nil {
			_ = tx.Rollback(context.Background())
			return resolved, err
		}
		if err := tx.Commit(ctx); err != nil {
			return resolved, err
		}
		resolved++
	}
	return resolved, nil
}
