package controlplane

import (
	"context"
	"fmt"
	"net/http"

	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/disk/blockformat"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/helmrdotdev/helmr/internal/workergroup"
)

const computerObjectRequestLimit = (16 << 20) + 4096

func (s *Server) workerRegisterInitialComputerObject(w http.ResponseWriter, r *http.Request) {
	s.workerInitialComputerObject(w, r, s.publisher.RegisterInitialObject)
}

func (s *Server) workerCertifyInitialComputerObject(w http.ResponseWriter, r *http.Request) {
	s.workerInitialComputerObject(w, r, s.publisher.CertifyInitialObject)
}

func (s *Server) workerInitialComputerObject(w http.ResponseWriter, r *http.Request, record func(ctx context.Context, principal workergroup.HostPrincipal, ref computer.PreparationRef, inspection blockformat.ObjectInspection) error) {
	w.Header().Set("Cache-Control", "no-store")
	var request workerapi.InitialComputerObjectRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, fmt.Errorf("invalid computer object request: %w", err))
		return
	}
	ref, err := computerPreparationRef(request.ComputerInstanceID, request.DesiredVersion)
	if err != nil {
		writeError(w, err)
		return
	}
	if err = computer.ValidateObjectInspection(request.Inspection); err != nil {
		writeError(w, badRequest(err))
		return
	}
	if err = record(r.Context(), workerFromContext(r.Context()), ref, request.Inspection); err != nil {
		writeError(w, computerError(err, computerInitialObjectOperation))
		return
	}
	writeJSON(w, http.StatusOK, struct{}{})
}
