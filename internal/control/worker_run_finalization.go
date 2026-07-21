package control

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/helmrdotdev/helmr/internal/api"
)

func (s *Server) workerBeginRunFinalization(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, unavailable(errors.New("run storage is not configured")))
		return
	}
	var request api.WorkerBeginRunFinalizationRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		if errors.Is(err, io.EOF) {
			err = errors.New("request body is required")
		}
		writeError(w, badRequest(fmt.Errorf("invalid Run finalization JSON: %w", err)))
		return
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		writeError(w, badRequest(errors.New("invalid Run finalization JSON: trailing value")))
		return
	}
	parsed, err := parseRunFinalization(request)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	worker := workerFromContext(r.Context())
	if request.Lease.WorkerGroupID != worker.WorkerGroupID ||
		parsed.lease.workerInstanceID != worker.WorkerInstanceID ||
		request.Lease.WorkerEpoch != worker.WorkerEpoch ||
		request.Lease.WorkerProtocolVersion != worker.ProtocolVersion {
		writeError(w, forbidden(errors.New("Run finalization belongs to another worker")))
		return
	}
	response, err := s.beginRunFinalization(r.Context(), worker, request, parsed)
	if err != nil {
		if errors.Is(err, errStaleRunFinalization) {
			writeError(w, conflict(errStaleRunFinalization))
			return
		}
		s.log.Error("begin Run finalization failed", "run_id", request.Lease.RunID, "error", err)
		writeError(w, errors.New("begin Run finalization"))
		return
	}
	writeJSON(w, http.StatusOK, response)
}
