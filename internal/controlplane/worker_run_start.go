package controlplane

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/run"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func (s *Server) workerStart(w http.ResponseWriter, r *http.Request) {
	var request workerapi.RunStartRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, fmt.Errorf("invalid worker run start request JSON: %w", err))
		return
	}
	leaseID, err := ids.Parse(request.Lease.ID)
	if err != nil || request.Lease.LeaseSequence <= 0 {
		writeError(w, badRequest(errors.New("lease.id must be a canonical UUIDv7 and lease.lease_sequence must be positive")))
		return
	}
	worker := workerFromContext(r.Context())
	if err := run.StartLease(r.Context(), s.tx, workerExecutionFence(worker, parsedRunLeaseFence{leaseID: leaseID}, request.Lease)); err != nil {
		s.writeRunError(w, err, runStartOperation, worker, request.Lease)
		return
	}
	writeJSON(w, http.StatusOK, workerapi.RunStartResponse(request))
}
