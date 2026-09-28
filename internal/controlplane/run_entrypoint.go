package controlplane

import (
	"context"
	"errors"

	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/jackc/pgx/v5/pgtype"
)

func enterRunEntrypoint(ctx context.Context, txb TxBeginner, worker workerActor, leaseID pgtype.UUID, request workerapi.RunEntrypointRequest) error {
	tx, err := txb.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	err = run.EnterExecution(ctx, tx, run.ExecutionFence{LeaseID: leaseID, LeaseSequence: request.Lease.LeaseSequence, WorkerGroupID: pgvalue.UUID(worker.WorkerGroupID), WorkerHostID: pgvalue.UUID(worker.WorkerHostID), WorkerEpoch: worker.WorkerEpoch, GroupClaimVersion: worker.GroupClaimVersion, HostClaimVersion: worker.ClaimVersion}, request.EntrypointKind, request.EntrypointDeclaredID)
	if errors.Is(err, run.ErrExecutionWorkerClaims) {
		return errStaleWorkerClaims
	}
	if err != nil {
		return staleRunLeaseClaim(err)
	}
	return tx.Commit(ctx)
}
