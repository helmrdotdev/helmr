package controlplane

import (
	"errors"
	"net/http"

	"github.com/helmrdotdev/helmr/internal/dispatch"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func (s *Server) workerPublishInitialComputerGeneration(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	var request workerapi.InitialComputerGenerationRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(errors.New("invalid computer generation request")))
		return
	}
	id, err := ids.Parse(request.RuntimeInstanceID)
	if err != nil || request.DesiredVersion <= 0 {
		writeError(w, badRequest(errors.New("runtime identity and desired version are required")))
		return
	}
	if err := request.Root.Validate(request.Root.LogicalBytes); err != nil {
		writeError(w, badRequest(errors.New("invalid computer generation root")))
		return
	}
	worker := workerFromContext(r.Context())
	fence := computerKeyFence{ComputerPreparationFence: dispatch.ComputerPreparationFence{
		RuntimeID: pgvalue.UUID(id), WorkerID: pgvalue.UUID(worker.WorkerInstanceID), WorkerGroupID: pgvalue.UUID(worker.WorkerGroupID), WorkerEpoch: worker.WorkerEpoch, DesiredVersion: request.DesiredVersion,
	}, ClaimVersion: worker.ClaimVersion, GroupClaimVersion: worker.GroupClaimVersion}
	result, err := s.publishInitialComputerGeneration(r.Context(), fence, initialComputerPublication{Root: request.Root, Config: request.Config})
	if err != nil {
		writeError(w, conflict(errors.New("computer generation publication is unavailable")))
		return
	}
	writeJSON(w, http.StatusOK, workerapi.InitialComputerGenerationResponse{ComputerID: pgvalue.UUIDString(result.ComputerID), VersionID: pgvalue.UUIDString(result.VersionID)})
}
