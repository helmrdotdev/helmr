package controlplane

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5/pgtype"
)

func (s *Server) workerStart(w http.ResponseWriter, r *http.Request) {
	var request workerapi.RunStartRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, fmt.Errorf("invalid worker run start request JSON: %w", err))
		return
	}
	leaseID, err := ids.Parse(request.Lease.ID)
	if err != nil || request.Lease.LeaseSequence <= 0 {
		writeError(w, badRequest(errors.New("lease.id must be a canonical UUIDv7 and lease.lease_sequence must be positive")))
		return
	}
	receipt, err := s.startRun(
		r.Context(), workerFromContext(r.Context()), pgvalue.UUID(leaseID), request.Lease,
	)
	if err != nil {
		if writeStaleWorkerClaims(w, err) {
			return
		}
		if errors.Is(err, errStaleRunLeaseClaim) {
			if point, ok := staleAuthorityPointOf(err); ok {
				s.log.Warn(
					"run start acknowledgement is stale",
					"failure_point", point,
					"run_lease_id", request.Lease.ID,
					"lease_sequence", request.Lease.LeaseSequence,
					"worker_group_id", workerFromContext(r.Context()).GroupID,
					"worker_host_id", workerFromContext(r.Context()).HostID,
					"worker_epoch", workerFromContext(r.Context()).Epoch,
				)
			}
			writeError(w, conflict(err))
			return
		}
		s.log.Error("start Run failed", "run_lease_id", request.Lease.ID, "error", err)
		writeError(w, errors.New("start run"))
		return
	}
	writeJSON(w, http.StatusOK, workerapi.RunStartResponse{Lease: receipt})
}

func (s *Server) startRun(ctx context.Context, worker workergroup.HostPrincipal, leaseID pgtype.UUID, expected workerapi.RunLeaseFence) (workerapi.RunLeaseFence, error) {
	err := s.inTx(ctx, func(work *txWork) error {
		_, err := run.StartExecution(ctx, work.tx, run.ExecutionFence{LeaseID: leaseID, LeaseSequence: expected.LeaseSequence, WorkerGroupID: pgvalue.UUID(worker.GroupID), WorkerHostID: pgvalue.UUID(worker.HostID), WorkerEpoch: worker.Epoch, GroupClaimVersion: worker.GroupClaimVersion, HostClaimVersion: worker.HostClaimVersion})
		if err != nil {
			return staleAuthority(staleAuthorityRunStart, "execution", staleRunLeaseClaim(err))
		}
		return nil
	})
	if err != nil {
		return workerapi.RunLeaseFence{}, err
	}
	return expected, nil
}
