package controlplane

import (
	"errors"
	"net/http"

	"github.com/helmrdotdev/helmr/internal/dispatch"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/jackc/pgx/v5"
)

func (s *Server) workerMarkCheckpointFailed(w http.ResponseWriter, r *http.Request) {
	var request workerapi.CheckpointFailedRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, err)
		return
	}
	for name, value := range map[string]string{"computer_instance_id": request.ComputerInstanceID, "checkpoint_id": request.CheckpointID} {
		if _, err := parseCanonicalUUID(name, value); err != nil {
			writeError(w, badRequest(err))
			return
		}
	}
	if request.WorkerEpoch <= 0 || request.DesiredVersion <= 0 {
		writeError(w, badRequest(errors.New("checkpoint source versions must be positive")))
		return
	}
	worker := workerFromContext(r.Context())
	err := s.inTx(r.Context(), func(work *txWork) error {
		_, err := dispatch.FailComputerCheckpoint(r.Context(), work.tx, dispatch.ComputerCaptureWorker{GroupID: pgvalue.UUID(worker.WorkerGroupID), HostID: pgvalue.UUID(worker.WorkerHostID), Epoch: worker.WorkerEpoch}, request)
		return err
	})
	if errors.Is(err, dispatch.ErrCheckpointCandidate) {
		writeError(w, badRequest(err))
		return
	}
	if writeStaleWorkerClaims(w, err) {
		return
	}
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, conflict(errors.New("checkpoint failure is stale or differs from its committed receipt")))
		return
	}
	if err != nil {
		s.log.Error("fail Computer checkpoint", "error", err)
		writeError(w, errors.New("fail Computer checkpoint"))
		return
	}
	writeJSON(w, http.StatusOK, workerapi.ComputerCheckpointResponse{ComputerInstanceID: request.ComputerInstanceID, WorkerEpoch: request.WorkerEpoch, DesiredVersion: request.DesiredVersion, CheckpointID: request.CheckpointID})
}
