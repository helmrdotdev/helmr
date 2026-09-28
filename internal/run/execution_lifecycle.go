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
func StartExecution(ctx context.Context, tx pgx.Tx, fence ExecutionFence) (ExecutionAuthority, error) {
	a, err := lockExecution(ctx, tx, fence, executionStart, executionTarget{})
	if err != nil {
		return ExecutionAuthority{}, err
	}
	r, l := a.Run, a.Lease
	if l.Status == db.RunLeaseStatusRunning {
		if r.Status != db.RunStatusRunning || !r.ActiveStartedAt.Valid {
			return ExecutionAuthority{}, pgx.ErrNoRows
		}
		return a, nil
	}
	if r.Status != db.RunStatusQueued || r.ActiveStartedAt.Valid || r.ActiveElapsedMs >= r.MaxActiveDurationMs {
		return ExecutionAuthority{}, pgx.ErrNoRows
	}
	q := db.New(tx)
	a.Lease, err = q.MarkRunLeaseRunning(ctx, db.MarkRunLeaseRunningParams{ID: l.ID, RunID: r.ID, ComputerID: r.ComputerID, AttemptNumber: l.AttemptNumber, LeaseSequence: l.LeaseSequence, WorkerGroupID: l.WorkerGroupID, WorkerHostID: l.WorkerHostID, WorkerEpoch: l.WorkerEpoch, ComputerInstanceID: l.ComputerInstanceID})
	if err != nil {
		return ExecutionAuthority{}, err
	}
	a.Run, err = q.MarkRunRunning(ctx, db.MarkRunRunningParams{ID: r.ID, OrgID: r.OrgID, ProjectID: r.ProjectID, EnvironmentID: r.EnvironmentID, ComputerID: r.ComputerID, ExpectedRevision: r.Revision, AttemptNumber: r.CurrentAttemptNumber, RunLeaseID: l.ID})
	if err != nil {
		return ExecutionAuthority{}, err
	}
	_, err = tx.Exec(ctx, `UPDATE computers SET last_activity_at=greatest(last_activity_at,clock_timestamp()),updated_at=clock_timestamp() WHERE id=$1`, r.ComputerID)
	return a, err
}

// RenewExecution only extends this Run's grant, bounded by its active budget.
// A replay with the immediately preceding expiry returns the current receipt.
func RenewExecution(ctx context.Context, tx pgx.Tx, fence ExecutionFence, expectedExpiry time.Time) (ExecutionAuthority, error) {
	a, err := lockExecution(ctx, tx, fence, executionLive, executionTarget{})
	if err != nil {
		return ExecutionAuthority{}, err
	}
	r, l := a.Run, a.Lease
	if (l.Status != db.RunLeaseStatusRunning && l.Status != db.RunLeaseStatusCheckpointing) || !r.ActiveStartedAt.Valid || r.ActiveElapsedMs < 0 || r.ActiveElapsedMs >= r.MaxActiveDurationMs {
		return ExecutionAuthority{}, pgx.ErrNoRows
	}
	current := expectedExpiry.Equal(l.ExpiresAt.Time)
	replay := l.PreviousExpiresAt.Valid && expectedExpiry.Equal(l.PreviousExpiresAt.Time)
	if !current && !replay {
		return ExecutionAuthority{}, pgx.ErrNoRows
	}
	now, err := db.New(tx).GetRunLeaseRenewalTime(ctx)
	if err != nil {
		return ExecutionAuthority{}, err
	}
	deadline := r.ActiveStartedAt.Time.Add(time.Duration(r.MaxActiveDurationMs-r.ActiveElapsedMs) * time.Millisecond)
	if !now.Time.Before(l.ExpiresAt.Time) || !now.Time.Before(deadline) {
		return ExecutionAuthority{}, pgx.ErrNoRows
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
	a.Lease, err = db.New(tx).RenewRunLeaseExpiry(ctx, db.RenewRunLeaseExpiryParams{RenewedAt: now, ExpiresAt: pgvalue.Timestamptz(expiry), ID: l.ID, RunID: r.ID, ComputerID: r.ComputerID, AttemptNumber: l.AttemptNumber, LeaseSequence: l.LeaseSequence, PreviousExpiresAt: l.ExpiresAt})
	return a, err
}

// EnterExecution records the point after which automatic replay of user code is
// unsafe. It shares the live grant fence but never changes physical ownership.
func EnterExecution(ctx context.Context, tx pgx.Tx, fence ExecutionFence, kind, declaredID string) error {
	a, err := lockExecution(ctx, tx, fence, executionLive, executionTarget{})
	if err != nil {
		return err
	}
	if a.Run.Status != db.RunStatusRunning || a.Lease.Status != db.RunLeaseStatusRunning || a.Run.EntrypointKind != kind || a.Run.EntrypointDeclaredID != declaredID {
		return pgx.ErrNoRows
	}
	if a.Attempt.EntrypointEnteredAt.Valid {
		return nil
	}
	if a.Instance.AdmissionState != "open" || a.Session.DispatchHoldID.Valid {
		return pgx.ErrNoRows
	}
	_, err = db.New(tx).MarkRunEntrypointEntered(ctx, db.MarkRunEntrypointEnteredParams{RunID: a.Run.ID, Number: a.Attempt.Number, ComputerID: a.Computer.ID})
	return err
}

// LockLiveExecution checks an already-running member without renewing it.
// Operations requiring Secret serialization acquire those locks before this call.
func LockLiveExecution(ctx context.Context, tx pgx.Tx, fence ExecutionFence) (ExecutionAuthority, error) {
	a, err := lockExecution(ctx, tx, fence, executionLive, executionTarget{})
	if err != nil {
		return ExecutionAuthority{}, err
	}
	if a.Lease.Status != db.RunLeaseStatusRunning && a.Lease.Status != db.RunLeaseStatusCheckpointing {
		return ExecutionAuthority{}, pgx.ErrNoRows
	}
	return a, nil
}
