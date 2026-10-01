package controlplane

import (
	"fmt"
	"net/http"

	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func (s *Server) workerCompleteTask(w http.ResponseWriter, r *http.Request) {
	var request workerapi.CompleteTaskRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, fmt.Errorf("invalid task completion JSON: %w", err))
		return
	}
	completion, err := parseTaskCompletionRequest(request)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	worker := workerFromContext(r.Context())
	if err := run.CompleteTask(r.Context(), s.tx, s.db, run.TaskCompletion{
		Fence:       workerExecutionFence(worker, completion.lease, request.Lease),
		OperationID: pgvalue.UUID(completion.operationID), Fingerprint: completion.fingerprint,
		Kind: string(completion.kind), Output: completion.output, Error: completion.errorObject,
	}); err != nil {
		s.writeRunError(w, err, runTaskCompletionOperation, worker, request.Lease)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
