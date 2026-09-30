package run

import (
	"context"
	"time"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
)

// StartExecution commits logical start without changing Instance ownership.
// Replays return the original start. The caller commits or rolls back.
func StartExecution(ctx context.Context, tx pgx.Tx, fence ExecutionFence) (Execution, error) {
	a, err := lockExecution(ctx, tx, fence, executionStart, executionTarget{})
	if err != nil {
		return Execution{}, err
	}
	r, l := a.run, a.lease
	if l.Status == db.RunLeaseStatusRunning {
		if r.Status != db.RunStatusRunning || !r.ActiveStartedAt.Valid {
			return Execution{}, pgx.ErrNoRows
		}
		return a, nil
	}
	if r.Status != db.RunStatusQueued || r.ActiveStartedAt.Valid || r.ActiveElapsedMs >= r.MaxActiveDurationMs {
		return Execution{}, pgx.ErrNoRows
	}
	q := db.New(tx)
	a.lease, err = q.MarkRunLeaseRunning(ctx, db.MarkRunLeaseRunningParams{ID: l.ID, RunID: r.ID, ComputerID: r.ComputerID, AttemptNumber: l.AttemptNumber, LeaseSequence: l.LeaseSequence, WorkerGroupID: l.WorkerGroupID, WorkerHostID: l.WorkerHostID, WorkerEpoch: l.WorkerEpoch, ComputerInstanceID: l.ComputerInstanceID})
	if err != nil {
		return Execution{}, err
	}
	a.run, err = q.MarkRunRunning(ctx, db.MarkRunRunningParams{ID: r.ID, OrgID: r.OrgID, ProjectID: r.ProjectID, EnvironmentID: r.EnvironmentID, ComputerID: r.ComputerID, ExpectedRevision: r.Revision, AttemptNumber: r.CurrentAttemptNumber, RunLeaseID: l.ID})
	if err != nil {
		return Execution{}, err
	}
	return a, a.instance.RecordStart(ctx)
}

// RenewExecution only extends this Run's grant, bounded by its active budget.
// A replay with the immediately preceding expiry returns the current receipt.
func RenewExecution(ctx context.Context, tx pgx.Tx, fence ExecutionFence, expectedExpiry time.Time) (Execution, error) {
	a, err := lockExecution(ctx, tx, fence, executionLive, executionTarget{})
	if err != nil {
		return Execution{}, err
	}
	r, l := a.run, a.lease
	if (l.Status != db.RunLeaseStatusRunning && l.Status != db.RunLeaseStatusCheckpointing) || !r.ActiveStartedAt.Valid || r.ActiveElapsedMs < 0 || r.ActiveElapsedMs >= r.MaxActiveDurationMs {
		return Execution{}, pgx.ErrNoRows
	}
	current := expectedExpiry.Equal(l.ExpiresAt.Time)
	replay := l.PreviousExpiresAt.Valid && expectedExpiry.Equal(l.PreviousExpiresAt.Time)
	if !current && !replay {
		return Execution{}, pgx.ErrNoRows
	}
	now, err := db.New(tx).GetRunLeaseRenewalTime(ctx)
	if err != nil {
		return Execution{}, err
	}
	deadline := r.ActiveStartedAt.Time.Add(time.Duration(r.MaxActiveDurationMs-r.ActiveElapsedMs) * time.Millisecond)
	if !now.Time.Before(l.ExpiresAt.Time) || !now.Time.Before(deadline) {
		return Execution{}, pgx.ErrNoRows
	}
	if replay {
		return a, nil
	}
	expiry := now.Time.Add(LeaseTTL)
	if expiry.After(deadline) {
		expiry = deadline
	}
	if !expiry.After(l.ExpiresAt.Time) {
		return a, nil
	}
	a.lease, err = db.New(tx).RenewRunLeaseExpiry(ctx, db.RenewRunLeaseExpiryParams{RenewedAt: now, ExpiresAt: pgvalue.Timestamptz(expiry), ID: l.ID, RunID: r.ID, ComputerID: r.ComputerID, AttemptNumber: l.AttemptNumber, LeaseSequence: l.LeaseSequence, PreviousExpiresAt: l.ExpiresAt})
	return a, err
}

// EnterExecution records the point after which automatic replay of user code is
// unsafe. It shares the live grant fence but never changes physical ownership.
func EnterExecution(ctx context.Context, tx pgx.Tx, fence ExecutionFence, kind, declaredID string) error {
	a, err := lockExecution(ctx, tx, fence, executionLive, executionTarget{})
	if err != nil {
		return err
	}
	if a.run.Status != db.RunStatusRunning || a.lease.Status != db.RunLeaseStatusRunning || a.run.EntrypointKind != kind || a.run.EntrypointDeclaredID != declaredID {
		return pgx.ErrNoRows
	}
	if a.attempt.EntrypointEnteredAt.Valid {
		return nil
	}
	if a.Instance().AdmissionState != "open" || a.session.DispatchHoldID.Valid {
		return pgx.ErrNoRows
	}
	_, err = db.New(tx).MarkRunEntrypointEntered(ctx, db.MarkRunEntrypointEnteredParams{RunID: a.run.ID, Number: a.attempt.Number, ComputerID: a.Computer().ID})
	return err
}

// LockLiveExecution checks an already-running member without renewing it.
// Operations requiring Secret serialization acquire those locks before this call.
func LockLiveExecution(ctx context.Context, tx pgx.Tx, fence ExecutionFence) (Execution, error) {
	a, err := lockExecution(ctx, tx, fence, executionLive, executionTarget{})
	if err != nil {
		return Execution{}, err
	}
	if a.lease.Status != db.RunLeaseStatusRunning && a.lease.Status != db.RunLeaseStatusCheckpointing {
		return Execution{}, pgx.ErrNoRows
	}
	return a, nil
}
