package controlplane

import (
	"context"
	"errors"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/jackc/pgx/v5"
)

func lockWorkerWaitExecution(ctx context.Context, tx pgx.Tx, worker workerActor, parsed parsedRunLeaseFence, receipt workerapi.RunLeaseFence) (run.ExecutionAuthority, error) {
	q := db.New(tx)
	loc, err := q.GetLiveRunLeaseLocators(ctx, db.GetLiveRunLeaseLocatorsParams{ID: pgvalue.UUID(parsed.leaseID), LeaseSequence: receipt.LeaseSequence, WorkerGroupID: pgvalue.UUID(worker.WorkerGroupID), WorkerHostID: pgvalue.UUID(worker.WorkerHostID), WorkerEpoch: worker.WorkerEpoch})
	if err != nil {
		return run.ExecutionAuthority{}, staleRunLeaseClaim(err)
	}
	if _, err = secret.LockAttemptDelivery(ctx, q, loc.RunID, loc.AttemptNumber, loc.ComputerID); err != nil {
		return run.ExecutionAuthority{}, err
	}
	a, err := run.LockLiveExecution(ctx, tx, workerExecutionFence(worker, parsed, receipt))
	if errors.Is(err, run.ErrExecutionWorkerClaims) {
		return run.ExecutionAuthority{}, errStaleWorkerClaims
	}
	if err != nil {
		return run.ExecutionAuthority{}, staleRunLeaseClaim(err)
	}
	if a.Lease.Status != db.RunLeaseStatusRunning || !a.Attempt.EntrypointEnteredAt.Valid || a.Lease.FinalizationOperationID.Valid {
		return run.ExecutionAuthority{}, errStaleRunLeaseClaim
	}
	return a, nil
}
