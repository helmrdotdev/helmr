package controlplane

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func (s *Server) workerCommitActorTurn(w http.ResponseWriter, r *http.Request) {
	var request workerapi.CommitActorTurnRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, fmt.Errorf("invalid actor turn commit JSON: %w", err))
		return
	}
	commit, err := parseActorTurnCommitRequest(request)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	worker := workerFromContext(r.Context())
	response, err := s.commitActorTurn(r.Context(), worker, request, commit)
	if writeStaleWorkerClaims(w, err) {
		return
	}
	if errors.Is(err, errStaleActorTurnCommit) {
		writeError(w, conflict(errStaleActorTurnCommit))
		return
	}
	if err != nil {
		s.log.Error("commit Actor turn failed", "run_lease_id", request.Lease.ID, "error", err)
		writeError(w, errors.New("commit actor turn"))
		return
	}
	writeJSON(w, http.StatusOK, response)
}
