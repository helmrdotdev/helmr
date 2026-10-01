package controlplane

import (
	"fmt"
	"net/http"

	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/session"
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
	eventID, err := session.CommitTurnFromRun(r.Context(), s.tx, workerExecutionFence(worker, commit.lease, request.Lease), commit.turnCommit())
	if err != nil {
		mapped := sessionError(err, sessionWorkerCommitOperation)
		if errorStatus(mapped) == http.StatusInternalServerError {
			s.log.Error("commit Actor turn failed", "run_lease_id", request.Lease.ID, "error", err)
		}
		writeError(w, mapped)
		return
	}
	response := projectActorTurnResponse(request, commit)
	response.EventID = pgvalue.UUIDString(eventID)
	writeJSON(w, http.StatusOK, response)
}
