package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/jackc/pgx/v5/pgtype"
)

func (s *Server) workerStart(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, unavailable(errors.New("run storage is not configured")))
		return
	}
	var request workerapi.RunStartRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		if errors.Is(err, io.EOF) {
			err = errors.New("request body is required")
		}
		writeError(w, badRequest(fmt.Errorf("invalid worker run start request JSON: %w", err)))
		return
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		writeError(w, badRequest(errors.New("invalid worker run start request JSON: trailing value")))
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
					"worker_group_id", workerFromContext(r.Context()).WorkerGroupID,
					"worker_host_id", workerFromContext(r.Context()).WorkerHostID,
					"worker_epoch", workerFromContext(r.Context()).WorkerEpoch,
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

func (s *Server) startRun(ctx context.Context, worker workerActor, leaseID pgtype.UUID, expected workerapi.RunLeaseFence) (workerapi.RunLeaseFence, error) {
	tx, err := s.tx.Begin(ctx)
	if err != nil {
		return workerapi.RunLeaseFence{}, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	_, err = run.StartExecution(ctx, tx, run.ExecutionFence{LeaseID: leaseID, LeaseSequence: expected.LeaseSequence, WorkerGroupID: pgvalue.UUID(worker.WorkerGroupID), WorkerHostID: pgvalue.UUID(worker.WorkerHostID), WorkerEpoch: worker.WorkerEpoch, GroupClaimVersion: worker.GroupClaimVersion, HostClaimVersion: worker.ClaimVersion})
	if errors.Is(err, run.ErrExecutionWorkerClaims) {
		return workerapi.RunLeaseFence{}, errStaleWorkerClaims
	}
	if err != nil {
		return workerapi.RunLeaseFence{}, staleAuthority(staleAuthorityRunStart, "execution", staleRunLeaseClaim(err))
	}
	if err = tx.Commit(ctx); err != nil {
		return workerapi.RunLeaseFence{}, err
	}
	return expected, nil
}
