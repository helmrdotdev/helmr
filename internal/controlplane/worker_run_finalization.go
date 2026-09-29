package controlplane

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func (s *Server) workerBeginRunFinalization(w http.ResponseWriter, r *http.Request) {
	var request workerapi.BeginRunFinalizationRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, fmt.Errorf("invalid run finalization JSON: %w", err))
		return
	}
	parsed, err := parseRunFinalization(request)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	worker := workerFromContext(r.Context())
	response, err := s.beginRunFinalization(r.Context(), worker, request, parsed)
	if err != nil {
		if writeStaleWorkerClaims(w, err) {
			return
		}
		if errors.Is(err, errStaleRunFinalization) {
			writeError(w, conflict(errStaleRunFinalization))
			return
		}
		s.log.Error("begin Run finalization failed", "run_lease_id", request.Lease.ID, "error", err)
		writeError(w, errors.New("begin run finalization"))
		return
	}
	writeJSON(w, http.StatusOK, response)
}
