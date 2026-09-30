package controlplane

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
)

// Cleanup follows physical ownership, independently of an expired or terminal
// member lease. It cannot grant execution authority or close another member.
func lockRunCleanupInstance(ctx context.Context, work *txWork, worker workergroup.HostPrincipal, request workerapi.ComputerRunCleanupRequest) (db.ComputerInstance, error) {
	instance, e1 := ids.Parse(request.ComputerInstanceID)
	environment, e2 := ids.Parse(request.EnvironmentID)
	if e1 != nil || e2 != nil || request.WriterGeneration <= 0 {
		return db.ComputerInstance{}, pgx.ErrNoRows
	}
	target, err := work.q.GetComputerInstance(ctx, db.GetComputerInstanceParams{ID: pgvalue.UUID(instance), EnvironmentID: pgvalue.UUID(environment)})
	if err != nil {
		return db.ComputerInstance{}, err
	}
	locked, err := workergroup.LockHost(ctx, work.q, worker)
	if err != nil {
		return db.ComputerInstance{}, err
	}
	if !locked.Continues() {
		return db.ComputerInstance{}, pgx.ErrNoRows
	}
	computer, err := work.q.LockComputer(ctx, db.LockComputerParams{EnvironmentID: target.EnvironmentID, ID: target.ComputerID})
	if err != nil {
		return db.ComputerInstance{}, err
	}
	i, err := work.q.LockWorkerComputerInstance(ctx, db.LockWorkerComputerInstanceParams{ID: target.ID, OrgID: target.OrgID, WorkerHostID: locked.Host.ID, WorkerGroupID: locked.Group.ID, WorkerEpoch: worker.Epoch})
	if err != nil {
		return db.ComputerInstance{}, err
	}
	now, err := work.q.GetRunLeaseRenewalTime(ctx)
	if err != nil {
		return db.ComputerInstance{}, err
	}
	if i.WriterGeneration != request.WriterGeneration || computer.WriterGeneration != i.WriterGeneration || i.ReclaimedAt.Valid || i.DesiredState != "ready" || i.MountState != "mounted" || (i.AdmissionState != "open" && i.AdmissionState != "draining") || !i.WriterExpiresAt.Time.After(now.Time) {
		return db.ComputerInstance{}, pgx.ErrNoRows
	}
	return i, nil
}

func (s *Server) getComputerRunCleanup(ctx context.Context, worker workergroup.HostPrincipal, request workerapi.ComputerRunCleanupRequest) (workerapi.ComputerRunCleanupResponse, error) {
	var response workerapi.ComputerRunCleanupResponse
	err := s.inTx(ctx, func(work *txWork) error {
		i, err := lockRunCleanupInstance(ctx, work, worker, request)
		if err != nil {
			return err
		}
		var member workerapi.ComputerRunCleanup
		err = work.tx.QueryRow(ctx, `SELECT run_id::text,id::text,attempt_number FROM run_leases
   WHERE computer_instance_id=$1 AND writer_generation=$2 AND process_reconciled_at IS NULL
   AND status IN ('completed','failed','cancelled','lost','rejected','expired')
   ORDER BY created_at,id LIMIT 1`, i.ID, i.WriterGeneration).Scan(&member.RunID, &member.RunLeaseID, &member.AttemptNumber)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err == nil {
			response.Run = &member
		}
		return err
	})
	return response, err
}

func (s *Server) reconcileComputerRun(ctx context.Context, worker workergroup.HostPrincipal, request workerapi.ComputerRunReconcileRequest) error {
	return s.inTx(ctx, func(work *txWork) error {
		i, err := lockRunCleanupInstance(ctx, work, worker, request.ComputerRunCleanupRequest)
		if err != nil {
			return err
		}
		tag, err := work.tx.Exec(ctx, `UPDATE run_leases SET process_reconciled_at=COALESCE(process_reconciled_at,clock_timestamp())
   WHERE id=$1 AND run_id=$2 AND attempt_number=$3 AND computer_instance_id=$4 AND writer_generation=$5
   AND status IN ('completed','failed','cancelled','lost','rejected','expired')`, request.RunLeaseID, request.RunID, request.AttemptNumber, i.ID, i.WriterGeneration)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return pgx.ErrNoRows
		}
		return nil
	})
}

func validRunCleanupRequest(r workerapi.ComputerRunCleanupRequest) bool {
	return ids.Validate(r.EnvironmentID) == nil && ids.Validate(r.ComputerInstanceID) == nil && r.WriterGeneration > 0
}
func (s *Server) workerGetComputerRunCleanup(w http.ResponseWriter, r *http.Request) {
	var request workerapi.ComputerRunCleanupRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, fmt.Errorf("invalid Run cleanup request: %w", err))
		return
	}
	if !validRunCleanupRequest(request) {
		writeError(w, badRequest(errors.New("invalid Run cleanup request")))
		return
	}
	response, err := s.getComputerRunCleanup(r.Context(), workerFromContext(r.Context()), request)
	if writeRunCleanupError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, response)
}
func (s *Server) workerReconcileComputerRun(w http.ResponseWriter, r *http.Request) {
	var request workerapi.ComputerRunReconcileRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, fmt.Errorf("invalid Run reconciliation request: %w", err))
		return
	}
	if !validRunCleanupRequest(request.ComputerRunCleanupRequest) || ids.Validate(request.RunID) != nil || ids.Validate(request.RunLeaseID) != nil || request.AttemptNumber == 0 {
		writeError(w, badRequest(errors.New("invalid Run reconciliation request")))
		return
	}
	if writeRunCleanupError(w, s.reconcileComputerRun(r.Context(), workerFromContext(r.Context()), request)) {
		return
	}
	writeJSON(w, http.StatusOK, struct{}{})
}
func writeRunCleanupError(w http.ResponseWriter, err error) bool {
	if writeStaleWorkerClaims(w, err) {
		return true
	}
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, conflict(errors.New("Run cleanup authority is stale")))
		return true
	}
	if err != nil {
		writeError(w, errors.New("reconcile Computer Run processes"))
		return true
	}
	return false
}
