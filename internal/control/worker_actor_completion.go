package control

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/helmrdotdev/helmr/internal/api"
)

func (s *Server) workerCompleteActor(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, unavailable(errors.New("run storage is not configured")))
		return
	}
	var request api.WorkerCompleteActorRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		if errors.Is(err, io.EOF) {
			err = errors.New("request body is required")
		}
		writeError(w, badRequest(fmt.Errorf("invalid Actor completion JSON: %w", err)))
		return
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		writeError(w, badRequest(errors.New("invalid Actor completion JSON: trailing value")))
		return
	}
	completion, err := parseActorCompletionRequest(request)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	worker := workerFromContext(r.Context())
	if request.Lease.WorkerGroupID != worker.WorkerGroupID || completion.lease.workerInstanceID != worker.WorkerInstanceID {
		writeError(w, forbidden(errors.New("Actor completion belongs to another worker")))
		return
	}
	if err := s.completeActor(r.Context(), worker, request, completion); err != nil {
		if errors.Is(err, errStaleActorCompletion) {
			writeError(w, conflict(errStaleActorCompletion))
			return
		}
		s.log.Error("complete Actor failed", "run_id", request.Lease.RunID, "error", err)
		writeError(w, errors.New("complete Actor"))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
