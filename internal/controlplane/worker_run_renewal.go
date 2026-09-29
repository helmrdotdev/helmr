package controlplane

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/helmrdotdev/helmr/internal/pgvalue"
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
	renewed, err := s.renewRunLease(
		r.Context(), worker, pgvalue.UUID(parsed.leaseID), request.Lease, request.ExpectedExpiresAt,
	)
	if writeStaleWorkerClaims(w, err) {
		return
	}
	if errors.Is(err, errStaleRunLeaseClaim) {
		writeError(w, conflict(errors.New("worker run lease fence is stale")))
		return
	}
	if err != nil {
		s.log.Error("renew worker Run Lease failed", "run_lease_id", request.Lease.ID, "error", err)
		writeError(w, errors.New("renew worker run lease"))
		return
	}
	writeJSON(w, http.StatusOK, renewed)
}
