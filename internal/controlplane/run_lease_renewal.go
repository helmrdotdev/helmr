package controlplane

import (
	"context"
	"errors"
	"time"

	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/jackc/pgx/v5/pgtype"
)

func (s *Server) renewRunLease(ctx context.Context, worker workerActor, leaseID pgtype.UUID, fence workerapi.RunLeaseFence, expectedExpiresAt time.Time) (workerapi.RunLeaseRenewResponse, error) {
	tx, err := s.tx.Begin(ctx)
	if err != nil {
		return workerapi.RunLeaseRenewResponse{}, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	a, err := run.RenewExecution(ctx, tx, run.ExecutionFence{LeaseID: leaseID, LeaseSequence: fence.LeaseSequence, WorkerGroupID: pgvalue.UUID(worker.WorkerGroupID), WorkerHostID: pgvalue.UUID(worker.WorkerHostID), WorkerEpoch: worker.WorkerEpoch, GroupClaimVersion: worker.GroupClaimVersion, HostClaimVersion: worker.ClaimVersion}, expectedExpiresAt)
	if errors.Is(err, run.ErrExecutionWorkerClaims) {
		return workerapi.RunLeaseRenewResponse{}, errStaleWorkerClaims
	}
	if err != nil {
		return workerapi.RunLeaseRenewResponse{}, staleRunLeaseClaim(err)
	}
	if err = tx.Commit(ctx); err != nil {
		return workerapi.RunLeaseRenewResponse{}, err
	}
	return workerapi.RunLeaseRenewResponse{Lease: fence, ExpiresAt: a.Lease.ExpiresAt.Time.UTC(), BaseComputerDiskVersionID: pgvalue.UUIDString(a.Attempt.BaseComputerDiskVersionID)}, nil
}
