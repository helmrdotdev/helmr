package controlplane

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func (s *Server) workerRenewRunLease(w http.ResponseWriter, r *http.Request) {
	var request workerapi.RunLeaseRenewRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, fmt.Errorf("invalid worker run lease renewal JSON: %w", err))
		return
	}
	parsed, err := parseRunLeaseFence(request.Lease)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	if request.ExpectedExpiresAt.IsZero() {
		writeError(w, badRequest(errors.New("expected_expires_at is required")))
		return
	}
	worker := workerFromContext(r.Context())
	renewal, err := run.RenewLease(r.Context(), s.tx, workerExecutionFence(worker, parsed, request.Lease), request.ExpectedExpiresAt)
	if err != nil {
		s.writeRunError(w, err, runLeaseRenewalOperation, worker, request.Lease)
		return
	}
	writeJSON(w, http.StatusOK, workerapi.RunLeaseRenewResponse{
		Lease:                     request.Lease,
		ExpiresAt:                 renewal.ExpiresAt.UTC(),
		BaseComputerDiskVersionID: pgvalue.UUIDString(renewal.BaseComputerDiskVersionID),
	})
}
