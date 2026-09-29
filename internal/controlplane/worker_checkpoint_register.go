package controlplane

import (
	"context"
	"errors"
	"net/http"

	"github.com/helmrdotdev/helmr/internal/dispatch"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/jackc/pgx/v5"
)

func (s *Server) workerRegisterCheckpoint(w http.ResponseWriter, r *http.Request) {
	var request workerapi.RegisterCheckpointRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, err)
		return
	}
	response, err := s.registerCheckpoint(r.Context(), workerFromContext(r.Context()), request)
	if err != nil {
		if writeStaleWorkerClaims(w, err) {
			return
		}
		if errors.Is(err, errStaleRunLeaseClaim) || errors.Is(err, pgx.ErrNoRows) {
			writeError(w, conflict(errors.New("checkpoint registration is stale or differs from its candidate")))
			return
		}
		if isDeterministicWorkerAdmission(err) {
			writeError(w, conflict(errors.New("checkpoint candidate conflicts with retained storage authority")))
			return
		}
		if errorStatus(err) < 500 {
			writeError(w, err)
			return
		}
		s.log.Error("register checkpoint failed", "error", err)
		writeError(w, errors.New("register checkpoint"))
		return
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) registerCheckpoint(ctx context.Context, worker workerActor, request workerapi.RegisterCheckpointRequest) (workerapi.ComputerCheckpointResponse, error) {
	for name, value := range map[string]string{"computer_instance_id": request.ComputerInstanceID, "checkpoint_id": request.CheckpointID} {
		if _, err := parseCanonicalUUID(name, value); err != nil {
			return workerapi.ComputerCheckpointResponse{}, badRequest(err)
		}
	}
	if request.WorkerEpoch <= 0 || request.DesiredVersion <= 0 {
		return workerapi.ComputerCheckpointResponse{}, badRequest(errors.New("checkpoint source versions must be positive"))
	}
	err := s.inTx(ctx, func(work *txWork) error {
		_, err := dispatch.RegisterComputerCheckpoint(ctx, work.tx, dispatch.ComputerCaptureWorker{GroupID: pgvalue.UUID(worker.WorkerGroupID), HostID: pgvalue.UUID(worker.WorkerHostID), Epoch: worker.WorkerEpoch}, request)
		return err
	})
	if errors.Is(err, dispatch.ErrCheckpointCandidate) {
		return workerapi.ComputerCheckpointResponse{}, badRequest(err)
	}
	if err != nil {
		return workerapi.ComputerCheckpointResponse{}, err
	}
	return workerapi.ComputerCheckpointResponse{ComputerInstanceID: request.ComputerInstanceID, WorkerEpoch: request.WorkerEpoch, DesiredVersion: request.DesiredVersion, CheckpointID: request.CheckpointID}, nil
}
