package controlplane

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"encoding/json"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/jackc/pgx/v5"
)

func (s *Server) workerRegisterRunFinalization(w http.ResponseWriter, r *http.Request) {
	var request workerapi.RegisterRunFinalizationRequest
	if err := decodeClosedWorkerRequest(r, &request); err != nil {
		writeError(w, badRequest(err))
		return
	}
	if err := s.registerRunFinalization(r.Context(), workerFromContext(r.Context()), request); err != nil {
		if writeStaleWorkerClaims(w, err) {
			return
		}
		if errors.Is(err, errStaleRunFinalization) || errors.Is(err, errStaleRunLeaseClaim) || errors.Is(err, pgx.ErrNoRows) || isDeterministicWorkerAdmission(err) {
			writeError(w, conflict(errors.New("finalization candidate conflicts with current authority")))
			return
		}
		if errorStatus(err) < 500 {
			writeError(w, err)
			return
		}
		s.log.Error("register run finalization failed", "error", err)
		writeError(w, errors.New("register run finalization"))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) registerRunFinalization(ctx context.Context, worker workerActor, request workerapi.RegisterRunFinalizationRequest) error {
	lease, err := parseRunLeaseFence(request.Lease)
	if err != nil {
		return badRequest(err)
	}
	operationID, err := parseCanonicalUUID("operation_id", request.OperationID)
	if err != nil {
		return badRequest(err)
	}
	computerID, err := parseCanonicalUUID("disk.computer_id", request.Disk.ComputerID)
	if err != nil {
		return badRequest(err)
	}
	disk := request.Disk.Root
	if err := disk.Validate(request.Disk.LogicalBytes); err != nil {
		return badRequest(err)
	}
	return s.inTx(ctx, func(work *txWork) error {
		locators, err := work.q.GetLiveRunLeaseLocators(ctx, db.GetLiveRunLeaseLocatorsParams{ID: pgvalue.UUID(lease.leaseID), LeaseSequence: request.Lease.LeaseSequence, WorkerGroupID: pgvalue.UUID(worker.WorkerGroupID), WorkerInstanceID: pgvalue.UUID(worker.WorkerInstanceID), WorkerEpoch: worker.WorkerEpoch})
		if err != nil {
			return staleRunFinalization(err)
		}
		if _, err := secret.LockAttemptDelivery(ctx, work.q, locators.RunID, locators.AttemptNumber, locators.WorkspaceID); err != nil {
			return err
		}
		authority, err := lockLiveRunFinalizationAuthority(ctx, work.q, worker, pgvalue.UUID(lease.leaseID), request.Lease.LeaseSequence, locators)
		if err != nil {
			return staleRunFinalization(err)
		}
		if err := validateRunFinalizationOwner(authority, locators); err != nil {
			return err
		}
		if err := lockSameWorkspaceChildFinalization(ctx, work.q, &authority); err != nil {
			return err
		}
		if authority.runLease.Status != db.RunLeaseStatusFinalizing || authority.runLease.FinalizationOperationID != pgvalue.UUID(operationID) || authority.workspace.ID != pgvalue.UUID(computerID) || authority.run.ActiveStartedAt.Valid || !authority.attempt.EntrypointEnteredAt.Valid {
			return errStaleRunFinalization
		}
		if err := disk.Validate(authority.runtime.ReservedGuestEphemeralDiskBytes); err != nil {
			return errStaleRunFinalization
		}
		rawRoot, err := json.Marshal(disk)
		if err != nil {
			return err
		}
		if _, err := work.q.RegisterRunFinalizationObject(ctx, db.RegisterRunFinalizationObjectParams{RunLeaseID: authority.runLease.ID, OperationID: pgvalue.UUID(operationID), Root: rawRoot}); err != nil {
			return fmt.Errorf("register immutable finalization disk: %w", err)
		}
		now, err := work.q.GetRunLeaseRenewalTime(ctx)
		if err != nil {
			return err
		}
		if !now.Valid || !now.Time.Before(authority.runLease.ExpiresAt.Time) || !now.Time.Before(authority.workspaceLease.ExpiresAt.Time) {
			return errStaleRunFinalization
		}
		return nil
	})
}
