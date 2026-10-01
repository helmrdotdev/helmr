package controlplane

import (
	"fmt"
	"net/http"

	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func (s *Server) workerPublishInitialComputerVersion(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	var request workerapi.InitialComputerVersionRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, fmt.Errorf("invalid computer version request: %w", err))
		return
	}
	ref, err := computerPreparationRef(request.ComputerInstanceID, request.DesiredVersion)
	if err != nil {
		writeError(w, err)
		return
	}
	published, err := s.publisher.PublishInitialVersion(r.Context(), workerFromContext(r.Context()), ref, computer.InitialVersion{Root: request.Root, Config: request.Config})
	if err != nil {
		s.writeWorkerComputerError(w, err, computerInitialVersionOperation, "initial computer version publication failed")
		return
	}
	writeJSON(w, http.StatusOK, workerapi.InitialComputerVersionResponse{ComputerID: published.ComputerID.String(), VersionID: published.VersionID.String()})
}
