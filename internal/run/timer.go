package run

import (
	"context"
	"errors"
	"fmt"

	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

type TimerWaitReconciler struct {
	db db.TxDB
}

func NewTimerWaitReconciler(database db.TxDB) (*TimerWaitReconciler, error) {
	if database == nil {
		return nil, errors.New("timer wait reconciliation database is required")
	}
	return &TimerWaitReconciler{db: database}, nil
}

func (r *TimerWaitReconciler) ReconcileDue(
	ctx context.Context,
	limit int32,
) (int, error) {
	if limit <= 0 {
		return 0, nil
	}
	candidates, err := db.New(r.db).ListDueTimerRunWaits(ctx, limit)
	if err != nil {
		return 0, err
	}
	resolved := 0
	for _, candidate := range candidates {
		didResolve, err := r.reconcileOne(ctx, candidate)
		if err != nil {
			return resolved, err
		}
		if didResolve {
			resolved++
		}
	}
	return resolved, nil
}

func (r *TimerWaitReconciler) reconcileOne(
	ctx context.Context,
	candidate db.RunWait,
) (resolved bool, returnErr error) {
	locator, err := db.New(r.db).GetRun(ctx, db.GetRunParams{
		EnvironmentID: candidate.EnvironmentID,
		ID:            candidate.RunID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	computerLocator, err := db.New(r.db).GetComputer(ctx, db.GetComputerParams{
		OrgID:         locator.OrgID,
		ProjectID:     locator.ProjectID,
		EnvironmentID: candidate.EnvironmentID,
		ID:            candidate.ComputerID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("begin timer wait reconciliation: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	q := db.New(tx)
	if _, err := q.LockComputerSecretsForAdmission(ctx, candidate.ComputerID); err != nil {
		return false, err
	}
	residence, err := computer.LockRunResidence(ctx, tx, computer.RunResidenceRef{
		OrgID: pgvalue.MustUUIDValue(locator.OrgID), ProjectID: pgvalue.MustUUIDValue(locator.ProjectID),
		EnvironmentID: pgvalue.MustUUIDValue(locator.EnvironmentID), RegionID: computerLocator.RegionID,
		ComputerID: pgvalue.MustUUIDValue(locator.ComputerID),
	})
	if errors.Is(err, computer.ErrNotFound) {
		return false, tx.Commit(ctx)
	}
	if err != nil {
		return false, err
	}
	residentComputer := residence.Computer()
	if locator.SessionID.Valid {
		session, err := q.LockSessionForInputReconcile(ctx, db.LockSessionForInputReconcileParams{
			EnvironmentID: locator.EnvironmentID,
			SessionID:     locator.SessionID,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return false, tx.Commit(ctx)
		}
		if err != nil {
			return false, err
		}
		if session.DispatchHoldID.Valid || !session.CurrentRunID.Valid || session.CurrentRunID != locator.ID ||
			(session.Status != "open" && session.Status != "closing") {
			return false, tx.Commit(ctx)
		}
	}

	run, err := q.LockRunLeaseClaimRun(ctx, db.LockRunLeaseClaimRunParams{
		ID: locator.ID, OrgID: locator.OrgID, ProjectID: locator.ProjectID,
		EnvironmentID: locator.EnvironmentID, ComputerID: locator.ComputerID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, tx.Commit(ctx)
	}
	if err != nil {
		return false, err
	}
	attempt, err := q.LockRunLeaseClaimAttempt(ctx, db.LockRunLeaseClaimAttemptParams{
		RunID: run.ID, Number: candidate.AttemptNumber, ComputerID: residentComputer.ID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, tx.Commit(ctx)
	}
	if err != nil {
		return false, err
	}
	wait, err := q.LockRunStartWait(ctx, db.LockRunStartWaitParams{
		ID: candidate.ID, EnvironmentID: candidate.EnvironmentID,
		RunID: candidate.RunID, ComputerID: candidate.ComputerID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, tx.Commit(ctx)
	}
	if err != nil {
		return false, err
	}
	current, err := q.RunWaitTurnCurrent(ctx, wait.ID)
	if err != nil {
		return false, err
	}
	if !current {
		return false, tx.Commit(ctx)
	}
	if !timerWaitAuthorityCurrent(run, residentComputer, attempt, wait) {
		return false, tx.Commit(ctx)
	}
	now, err := q.GetRunLeaseRenewalTime(ctx)
	if err != nil || !now.Valid {
		return false, fmt.Errorf("load timer wait reconciliation time: %w", err)
	}
	if !wait.DueAt.Valid || now.Time.Before(wait.DueAt.Time) {
		return false, tx.Commit(ctx)
	}
	if _, err := Complete(ctx, q, wait, nil, pgtype.UUID{}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, tx.Commit(ctx)
		}
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return true, nil
}

func timerWaitAuthorityCurrent(
	run db.Run,
	computer db.LockRunLeaseClaimComputerRow,
	attempt db.RunAttempt,
	wait db.RunWait,
) bool {
	if computer.Status != db.ComputerStatusActive ||
		computer.DesiredState != db.ComputerDesiredStateActive ||
		attempt.TerminalAt.Valid || run.Status != db.RunStatusWaiting ||
		run.CurrentAttemptNumber != wait.AttemptNumber ||
		run.Revision != wait.ExpectedRunRevision ||
		wait.Kind != db.WaitKindTimer || wait.ConditionStatus != db.WaitStatusPending {
		return false
	}
	switch wait.SuspensionStatus {
	case db.RunWaitStatusHot, db.RunWaitStatusCheckpointing:
		return run.CurrentRunLeaseID.Valid &&
			run.CurrentRunLeaseID == wait.CurrentRunLeaseID &&
			!wait.PriorRunLeaseID.Valid
	case db.RunWaitStatusResuming:
		return run.CurrentRunLeaseID.Valid && run.CurrentRunLeaseID == wait.CurrentRunLeaseID &&
			wait.PriorRunLeaseID.Valid && wait.SuspendCheckpointID.Valid
	case db.RunWaitStatusParked:
		return !run.CurrentRunLeaseID.Valid &&
			!wait.CurrentRunLeaseID.Valid &&
			wait.PriorRunLeaseID.Valid &&
			wait.SuspendCheckpointID.Valid
	default:
		return false
	}
}
