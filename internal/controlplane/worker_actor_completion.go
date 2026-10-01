package controlplane

import (
	"fmt"
	"net/http"

	"github.com/helmrdotdev/helmr/internal/session"
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
	if err := session.CompleteActorFromRun(r.Context(), s.tx, s.db, workerExecutionFence(worker, completion.lease, request.Lease), completion.completion); err != nil {
		mapped := sessionError(err, sessionWorkerCompleteOperation)
		if errorStatus(mapped) == http.StatusInternalServerError {
			s.log.Error("complete Actor failed", "run_lease_id", request.Lease.ID, "error", err)
		}
		writeError(w, mapped)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
