package controlplane

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func (s *Server) workerCompleteActor(w http.ResponseWriter, r *http.Request) {
	var request workerapi.CompleteActorRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, fmt.Errorf("invalid actor completion JSON: %w", err))
		return
	}
	completion, err := parseActorCompletionRequest(request)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	worker := workerFromContext(r.Context())
	if err := s.completeActor(r.Context(), worker, request, completion); err != nil {
		if writeStaleWorkerClaims(w, err) {
			return
		}
		if errors.Is(err, errActorStopCleanupPending) {
			writeError(w, unavailable(err))
			return
		}
		if errors.Is(err, errStaleActorCompletion) {
			writeError(w, conflict(errStaleActorCompletion))
			return
		}
		if isDeterministicWorkerAdmission(err) {
			writeError(w, apiError{kind: errUnprocessable, err: errors.New("actor completion admission is invalid")})
			return
		}

		s.log.Error("complete Actor failed", "run_lease_id", request.Lease.ID, "error", err)
		writeError(w, errors.New("complete actor"))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
