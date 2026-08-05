package run

import (
	"context"
	"errors"
	"fmt"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

type timerWaitReconcileDB interface {
	db.DBTX
	Begin(context.Context) (pgx.Tx, error)
}

type TimerWaitReconciler struct {
	db timerWaitReconcileDB
}

func NewTimerWaitReconciler(database timerWaitReconcileDB) (*TimerWaitReconciler, error) {
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
	workspaceLocator, err := db.New(r.db).GetWorkspace(ctx, db.GetWorkspaceParams{
		EnvironmentID: candidate.EnvironmentID,
		ID:            candidate.WorkspaceID,
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
	if _, err := q.LockWorkspaceSecretsForAdmission(ctx, candidate.WorkspaceID); err != nil {
		return false, err
	}
	if locator.SessionID.Valid {
		actor, err := q.LockActorForInputReconcile(ctx, db.LockActorForInputReconcileParams{
			EnvironmentID: locator.EnvironmentID,
			SessionID:     locator.SessionID,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return false, tx.Commit(ctx)
		}
		if err != nil {
			return false, err
		}
		if !actor.CurrentRunID.Valid || actor.CurrentRunID != locator.ID ||
			(actor.State != "open" && actor.State != "closing") {
			return false, tx.Commit(ctx)
		}
	} else if locator.ParentRunID.Valid && locator.ParentOwnsLifecycle.Valid &&
		locator.ParentOwnsLifecycle.Bool {
		if _, err := q.LockRunFinalizationParentRun(ctx, db.LockRunFinalizationParentRunParams{
			ID: locator.ParentRunID, OrgID: locator.OrgID,
			ProjectID: locator.ProjectID, EnvironmentID: locator.EnvironmentID,
		}); errors.Is(err, pgx.ErrNoRows) {
			return false, tx.Commit(ctx)
		} else if err != nil {
			return false, err
		}
	}
	run, err := q.LockRunLeaseClaimRun(ctx, db.LockRunLeaseClaimRunParams{
		ID: locator.ID, OrgID: locator.OrgID, ProjectID: locator.ProjectID,
		EnvironmentID: locator.EnvironmentID, WorkspaceID: locator.WorkspaceID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, tx.Commit(ctx)
	}
	if err != nil {
		return false, err
	}
	workspace, err := q.LockRunLeaseClaimWorkspace(ctx, db.LockRunLeaseClaimWorkspaceParams{
		ID: locator.WorkspaceID, OrgID: locator.OrgID, ProjectID: locator.ProjectID,
		EnvironmentID: locator.EnvironmentID, RegionID: workspaceLocator.RegionID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, tx.Commit(ctx)
	}
	if err != nil {
		return false, err
	}
	attempt, err := q.LockRunLeaseClaimAttempt(ctx, db.LockRunLeaseClaimAttemptParams{
		RunID: run.ID, Number: candidate.AttemptNumber, WorkspaceID: workspace.ID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, tx.Commit(ctx)
	}
	if err != nil {
		return false, err
	}
	wait, err := q.LockRunStartWait(ctx, db.LockRunStartWaitParams{
		ID: candidate.ID, EnvironmentID: candidate.EnvironmentID,
		RunID: candidate.RunID, WorkspaceID: candidate.WorkspaceID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, tx.Commit(ctx)
	}
	if err != nil {
		return false, err
	}
	if !timerWaitAuthorityCurrent(run, workspace, attempt, wait) {
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
	workspace db.Workspace,
	attempt db.RunAttempt,
	wait db.RunWait,
) bool {
	if workspace.State != db.WorkspaceStateActive ||
		workspace.DesiredState != db.WorkspaceDesiredStateActive ||
		attempt.TerminalAt.Valid || run.Status != db.RunStatusWaiting ||
		run.CurrentAttemptNumber != wait.AttemptNumber ||
		run.StateVersion != wait.ExpectedRunStateVersion ||
		wait.Kind != db.WaitKindTimer || wait.ConditionState != db.WaitStatePending {
		return false
	}
	switch wait.SuspensionState {
	case db.RunWaitStateHot, db.RunWaitStateCheckpointing:
		return run.CurrentRunLeaseID.Valid &&
			run.CurrentRunLeaseID == wait.CurrentRunLeaseID &&
			!wait.PriorRunLeaseID.Valid
	case db.RunWaitStateParked:
		return !run.CurrentRunLeaseID.Valid &&
			!wait.CurrentRunLeaseID.Valid &&
			wait.PriorRunLeaseID.Valid &&
			wait.SuspendCheckpointID.Valid
	default:
		return false
	}
}
