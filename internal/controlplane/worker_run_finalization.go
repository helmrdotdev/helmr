package controlplane

import (
	"fmt"
	"net/http"

	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run"
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
	finalization, err := run.BeginFinalization(r.Context(), s.tx, run.ExecutionFinalization{
		Fence: workerExecutionFence(worker, parsed.lease, request.Lease),
		RunID: pgvalue.UUID(parsed.runID), AttemptNumber: parsed.attempt,
		OperationID: pgvalue.UUID(parsed.operationID), Fingerprint: parsed.fingerprint,
	})
	if err != nil {
		s.writeRunError(w, err, runFinalizationOperation, worker, request.Lease)
		return
	}
	writeJSON(w, http.StatusOK, workerapi.BeginRunFinalizationResponse{
		Lease:       request.Lease,
		ExpiresAt:   finalization.ExpiresAt.UTC(),
		OperationID: parsed.operationID.String(),
		StartedAt:   finalization.StartedAt.UTC(),
	})
}
