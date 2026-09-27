package run

import (
	"context"
	"time"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// ExecutionFinalization identifies a quiesced member and its idempotent terminal
// operation. It conveys no authority to capture, unmount or stop the Computer.
type ExecutionFinalization struct {
	Fence              ExecutionFence
	RunID, OperationID pgtype.UUID
	AttemptNumber      int32
	Fingerprint        string
}

// BeginExecutionFinalization closes only the member's active interval and grants
// time to record its outcome. The caller owns commit/rollback. Process cleanup is
// separately observed; a logical finalization is not evidence of physical cleanup.
func BeginExecutionFinalization(ctx context.Context, tx pgx.Tx, request ExecutionFinalization) (ExecutionAuthority, error) {
	if !request.RunID.Valid || !request.OperationID.Valid || request.AttemptNumber <= 0 || request.Fingerprint == "" {
		return ExecutionAuthority{}, pgx.ErrNoRows
	}
	a, err := lockExecution(ctx, tx, request.Fence, executionLive, executionTarget{})
	if err != nil {
		return ExecutionAuthority{}, err
	}
	r, l := a.Run, a.Lease
	if r.ID != request.RunID || a.Attempt.Number != request.AttemptNumber || r.Status != db.RunStatusRunning || !a.Attempt.EntrypointEnteredAt.Valid {
		return ExecutionAuthority{}, pgx.ErrNoRows
	}
	if l.Status == db.RunLeaseStatusFinalizing {
		if r.ActiveStartedAt.Valid || l.FinalizationOperationID != request.OperationID || l.FinalizationRequestFingerprint.String != request.Fingerprint || !l.FinalizationStartedAt.Valid {
			return ExecutionAuthority{}, pgx.ErrNoRows
		}
		return a, nil
	}
	if l.Status != db.RunLeaseStatusRunning || !r.ActiveStartedAt.Valid || l.FinalizationOperationID.Valid || l.FinalizationStartedAt.Valid || l.FinalizationRequestFingerprint.Valid {
		return ExecutionAuthority{}, pgx.ErrNoRows
	}
	q := db.New(tx)
	clear, err := q.RunFinalizationScopeIsClear(ctx, db.RunFinalizationScopeIsClearParams{RunID: r.ID, AttemptNumber: a.Attempt.Number, ComputerID: a.Computer.ID})
	if err != nil {
		return ExecutionAuthority{}, err
	}
	if !clear {
		return ExecutionAuthority{}, pgx.ErrNoRows
	}
	now, err := q.GetRunFinalizationTime(ctx)
	if err != nil {
		return ExecutionAuthority{}, err
	}
	if !now.Valid || !now.Time.Before(l.ExpiresAt.Time) || !now.Time.Before(a.Instance.WriterExpiresAt.Time) || r.ActiveElapsedMs < 0 || r.ActiveElapsedMs >= r.MaxActiveDurationMs || !now.Time.Before(r.ActiveStartedAt.Time.Add(time.Duration(r.MaxActiveDurationMs-r.ActiveElapsedMs)*time.Millisecond)) {
		return ExecutionAuthority{}, pgx.ErrNoRows
	}
	expiry := now.Time.Add(FinalizationTTL)
	if minimum := l.ExpiresAt.Time.Add(time.Microsecond); expiry.Before(minimum) {
		expiry = minimum
	}
	a.Run, err = q.CloseRunActiveIntervalForFinalization(ctx, db.CloseRunActiveIntervalForFinalizationParams{FinalizationStartedAt: now, ID: r.ID, OrgID: r.OrgID, ProjectID: r.ProjectID, EnvironmentID: r.EnvironmentID, ComputerID: r.ComputerID, AttemptNumber: a.Attempt.Number, RunLeaseID: l.ID, ExpectedRevision: r.Revision})
	if err != nil {
		return ExecutionAuthority{}, err
	}
	a.Lease, err = q.BeginRunLeaseFinalization(ctx, db.BeginRunLeaseFinalizationParams{ExpiresAt: pgvalue.Timestamptz(expiry), FinalizationOperationID: request.OperationID, FinalizationStartedAt: now, FinalizationRequestFingerprint: pgvalue.Text(request.Fingerprint), ID: l.ID, RunID: r.ID, ComputerID: r.ComputerID, AttemptNumber: a.Attempt.Number, LeaseSequence: l.LeaseSequence, PreviousExpiresAt: l.ExpiresAt})
	if err != nil {
		return ExecutionAuthority{}, err
	}
	// Triggered writes and row contention can consume authority after the first
	// check. Revalidate the physical and newly recorded logical grants before commit.
	var live bool
	err = tx.QueryRow(ctx, `SELECT l.expires_at>clock_timestamp() AND i.writer_expires_at>clock_timestamp() FROM run_leases l JOIN computer_instances i ON i.id=l.computer_instance_id WHERE l.id=$1`, l.ID).Scan(&live)
	if err != nil {
		return ExecutionAuthority{}, err
	}
	if !live {
		return ExecutionAuthority{}, pgx.ErrNoRows
	}
	return a, nil
}

// LockFinalizingExecution acquires the complete owned graph before validating
// the exact finalizing member. It owns Secret and Worker lock ordering; the caller
// owns outcome writes and transaction commit/rollback.
func LockFinalizingExecution(ctx context.Context, tx pgx.Tx, fence ExecutionFence) (ExecutionAuthority, OwnedFinalization, error) {
	q := db.New(tx)
	loc, err := q.GetLiveRunLeaseLocators(ctx, db.GetLiveRunLeaseLocatorsParams{ID: fence.LeaseID, LeaseSequence: fence.LeaseSequence, WorkerGroupID: fence.WorkerGroupID, WorkerHostID: fence.WorkerHostID, WorkerEpoch: fence.WorkerEpoch})
	if err != nil {
		return ExecutionAuthority{}, OwnedFinalization{}, err
	}
	secrets, err := secret.LockAttemptDelivery(ctx, q, loc.RunID, loc.AttemptNumber, loc.ComputerID)
	if err != nil {
		return ExecutionAuthority{}, OwnedFinalization{}, err
	}
	graph, err := LockOwnedFinalizationWithInstanceFence(ctx, tx, OwnedFinalizationRequest{OrgID: pgvalue.MustUUIDValue(loc.OrgID), ProjectID: pgvalue.MustUUIDValue(loc.ProjectID), EnvironmentID: pgvalue.MustUUIDValue(loc.EnvironmentID), RunID: pgvalue.MustUUIDValue(loc.RunID)}, func() error { return lockExecutionWorker(ctx, q, fence, loc.RegionID) })
	if err != nil {
		return ExecutionAuthority{}, OwnedFinalization{}, err
	}
	a, err := lockExecution(ctx, tx, fence, executionLive, executionTarget{})
	if err != nil {
		return ExecutionAuthority{}, OwnedFinalization{}, err
	}
	if a.Run.Status != db.RunStatusRunning || a.Lease.Status != db.RunLeaseStatusFinalizing || a.Run.ActiveStartedAt.Valid || !a.Attempt.EntrypointEnteredAt.Valid || !a.Lease.FinalizationOperationID.Valid || !a.Lease.FinalizationStartedAt.Valid || !a.Lease.FinalizationRequestFingerprint.Valid {
		return ExecutionAuthority{}, OwnedFinalization{}, pgx.ErrNoRows
	}
	a.Secrets = secrets
	return a, graph, nil
}
