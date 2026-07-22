package control

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/helmrdotdev/helmr/internal/api"
)

func (s *Server) workerCommitActorTurn(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, unavailable(errors.New("run storage is not configured")))
		return
	}
	var request api.WorkerCommitActorTurnRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		if errors.Is(err, io.EOF) {
			err = errors.New("request body is required")
		}
		writeError(w, badRequest(fmt.Errorf("invalid Actor turn commit JSON: %w", err)))
		return
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		writeError(w, badRequest(errors.New("invalid Actor turn commit JSON: trailing value")))
		return
	}
	commit, err := parseActorTurnCommitRequest(request)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	worker := workerFromContext(r.Context())
	if request.Lease.WorkerGroupID != worker.WorkerGroupID || commit.lease.workerInstanceID != worker.WorkerInstanceID {
		writeError(w, forbidden(errors.New("Actor turn commit belongs to another worker")))
		return
	}
	response, err := s.commitActorTurn(r.Context(), worker, request, commit)
	if errors.Is(err, errStaleActorTurnCommit) {
		writeError(w, conflict(errStaleActorTurnCommit))
		return
	}
	if err != nil {
		s.log.Error("commit Actor turn failed", "run_id", request.Lease.RunID, "error", err)
		writeError(w, errors.New("commit Actor turn"))
		return
	}
	writeJSON(w, http.StatusOK, response)
}
