package controlplane

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func (s *Server) workerPublishInitialComputerGeneration(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	var request workerapi.InitialComputerGenerationRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, fmt.Errorf("invalid computer generation request: %w", err))
		return
	}
	ref, err := computerPreparationRef(request.ComputerInstanceID, request.DesiredVersion)
	if err != nil {
		writeError(w, err)
		return
	}
	if err := request.Root.Validate(request.Root.LogicalBytes); err != nil {
		writeError(w, badRequest(errors.New("invalid computer generation root")))
		return
	}
	published, err := s.publisher.PublishInitialVersion(r.Context(), workerFromContext(r.Context()), ref, computer.InitialVersion{Root: request.Root, Config: request.Config})
	if err != nil {
		writeError(w, computerError(err, computerInitialVersionOperation))
		return
	}
	writeJSON(w, http.StatusOK, workerapi.InitialComputerGenerationResponse{ComputerID: published.ComputerID.String(), VersionID: published.VersionID.String()})
}
