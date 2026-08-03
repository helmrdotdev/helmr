package controlplane

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func (s *Server) workerRenewRunLease(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, unavailable(errors.New("run storage is not configured")))
		return
	}
	var request workerapi.RunLeaseRenewRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		if errors.Is(err, io.EOF) {
			err = errors.New("request body is required")
		}
		writeError(w, badRequest(fmt.Errorf("invalid worker run lease renewal JSON: %w", err)))
		return
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		writeError(w, badRequest(errors.New("invalid worker run lease renewal JSON: trailing value")))
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
