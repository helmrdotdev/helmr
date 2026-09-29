package controlplane

import (
	"context"
	"errors"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/jackc/pgx/v5/pgtype"
)

func enterRunEntrypoint(ctx context.Context, txb db.TxBeginner, worker workerActor, leaseID pgtype.UUID, request workerapi.RunEntrypointRequest) error {
	return inTxWith(ctx, txb, func(work *txWork) error {
		err := run.EnterExecution(ctx, work.tx, run.ExecutionFence{LeaseID: leaseID, LeaseSequence: request.Lease.LeaseSequence, WorkerGroupID: pgvalue.UUID(worker.WorkerGroupID), WorkerHostID: pgvalue.UUID(worker.WorkerHostID), WorkerEpoch: worker.WorkerEpoch, GroupClaimVersion: worker.GroupClaimVersion, HostClaimVersion: worker.ClaimVersion}, request.EntrypointKind, request.EntrypointDeclaredID)
		if errors.Is(err, run.ErrExecutionWorkerClaims) {
			return errStaleWorkerClaims
		}
		if err != nil {
			return staleRunLeaseClaim(err)
		}
		return nil
	})
}
