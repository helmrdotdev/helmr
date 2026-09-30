package controlplane

import (
	"context"
	"time"

	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5/pgtype"
)

func (s *Server) renewRunLease(ctx context.Context, worker workergroup.HostPrincipal, leaseID pgtype.UUID, fence workerapi.RunLeaseFence, expectedExpiresAt time.Time) (workerapi.RunLeaseRenewResponse, error) {
	var response workerapi.RunLeaseRenewResponse
	err := s.inTx(ctx, func(work *txWork) error {
		a, err := run.RenewExecution(ctx, work.tx, run.ExecutionFence{LeaseID: leaseID, LeaseSequence: fence.LeaseSequence, WorkerGroupID: pgvalue.UUID(worker.GroupID), WorkerHostID: pgvalue.UUID(worker.HostID), WorkerEpoch: worker.Epoch, GroupClaimVersion: worker.GroupClaimVersion, HostClaimVersion: worker.HostClaimVersion}, expectedExpiresAt)
		if err != nil {
			return staleRunLeaseClaim(err)
		}
		response = workerapi.RunLeaseRenewResponse{Lease: fence, ExpiresAt: a.Lease().ExpiresAt.Time.UTC(), BaseComputerDiskVersionID: pgvalue.UUIDString(a.Attempt().BaseComputerDiskVersionID)}
		return nil
	})
	if err != nil {
		return workerapi.RunLeaseRenewResponse{}, err
	}
	return response, nil
}
