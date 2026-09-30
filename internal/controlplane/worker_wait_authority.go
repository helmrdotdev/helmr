package controlplane

import (
	"context"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/run"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
)

func lockWorkerWaitExecution(ctx context.Context, tx pgx.Tx, worker workergroup.HostPrincipal, parsed parsedRunLeaseFence, receipt workerapi.RunLeaseFence) (run.Execution, error) {
	locator, err := run.LocateLiveExecution(ctx, tx, workerExecutionFence(worker, parsed, receipt))
	if err != nil {
		return run.Execution{}, staleRunLeaseClaim(err)
	}
	secrets, err := locator.LockSecrets(ctx)
	if err != nil {
		return run.Execution{}, err
	}
	a, err := secrets.LockExecution(ctx)
	if err != nil {
		return run.Execution{}, staleRunLeaseClaim(err)
	}
	if a.Lease().Status != db.RunLeaseStatusRunning || !a.Attempt().EntrypointEnteredAt.Valid || a.Lease().FinalizationOperationID.Valid {
		return run.Execution{}, errStaleRunLeaseClaim
	}
	return a, nil
}
