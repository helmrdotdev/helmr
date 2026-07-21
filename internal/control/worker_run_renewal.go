package control

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
)

func (s *Server) workerRenewRunLease(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, unavailable(errors.New("run storage is not configured")))
		return
	}
	var request api.WorkerRunLeaseRenewRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		if errors.Is(err, io.EOF) {
			err = errors.New("request body is required")
		}
		writeError(w, badRequest(fmt.Errorf("invalid worker Run Lease renewal JSON: %w", err)))
		return
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		writeError(w, badRequest(errors.New("invalid worker Run Lease renewal JSON: trailing value")))
		return
	}
	parsed, err := parseRunLeaseReceipt(request.Lease)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	worker := workerFromContext(r.Context())
	if request.Lease.WorkerGroupID != worker.WorkerGroupID ||
		parsed.workerInstanceID != worker.WorkerInstanceID ||
		request.Lease.WorkerEpoch != worker.WorkerEpoch ||
		request.Lease.WorkerProtocolVersion != worker.ProtocolVersion {
		writeError(w, forbidden(errors.New("worker Run Lease receipt belongs to another worker epoch")))
		return
	}
	renewed, err := s.renewRunLease(r.Context(), worker, pgvalue.UUID(parsed.leaseID), request.Lease)
	if errors.Is(err, errStaleRunLeaseClaim) {
		writeError(w, conflict(errors.New("worker Run Lease receipt is stale")))
		return
	}
	if err != nil {
		s.log.Error("renew worker Run Lease failed", "run_id", request.Lease.RunID, "error", err)
		writeError(w, errors.New("renew worker Run Lease"))
		return
	}
	writeJSON(w, http.StatusOK, api.WorkerRunLeaseRenewResponse{Lease: renewed})
}
