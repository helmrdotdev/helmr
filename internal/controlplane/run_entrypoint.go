package controlplane

import (
	"context"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5/pgtype"
)

func enterRunEntrypoint(ctx context.Context, txb db.TxBeginner, worker workergroup.HostPrincipal, leaseID pgtype.UUID, request workerapi.RunEntrypointRequest) error {
	return inTxWith(ctx, txb, func(work *txWork) error {
		err := run.EnterExecution(ctx, work.tx, run.ExecutionFence{LeaseID: leaseID, LeaseSequence: request.Lease.LeaseSequence, WorkerGroupID: pgvalue.UUID(worker.GroupID), WorkerHostID: pgvalue.UUID(worker.HostID), WorkerEpoch: worker.Epoch, GroupClaimVersion: worker.GroupClaimVersion, HostClaimVersion: worker.HostClaimVersion}, request.EntrypointKind, request.EntrypointDeclaredID)
		if err != nil {
			return staleRunLeaseClaim(err)
		}
		return nil
	})
}
