package controlplane

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func (s *Server) workerCompleteTask(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, unavailable(errors.New("run storage is not configured")))
		return
	}
	var request workerapi.CompleteTaskRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		if errors.Is(err, io.EOF) {
			err = errors.New("request body is required")
		}
		writeError(w, badRequest(fmt.Errorf("invalid Task completion JSON: %w", err)))
		return
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		writeError(w, badRequest(errors.New("invalid Task completion JSON: trailing value")))
		return
	}
	completion, err := parseTaskCompletionRequest(request)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	worker := workerFromContext(r.Context())
	if err := s.completeTask(r.Context(), worker, request, completion); err != nil {
		if errors.Is(err, errStaleTaskCompletion) {
			writeError(w, conflict(errStaleTaskCompletion))
			return
		}
		s.log.Error("complete Task failed", "run_lease_id", request.Lease.ID, "error", err)
		writeError(w, errors.New("complete Task"))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
