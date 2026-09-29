package controlplane

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func (s *Server) workerEnterRunEntrypoint(w http.ResponseWriter, r *http.Request) {
	var request workerapi.RunEntrypointRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, fmt.Errorf("invalid worker run entrypoint request JSON: %w", err))
		return
	}
	leaseID, err := ids.Parse(request.Lease.ID)
	if err != nil || request.Lease.LeaseSequence <= 0 {
		writeError(w, badRequest(errors.New("lease.id must be a canonical UUIDv7 and lease.lease_sequence must be positive")))
		return
	}
	if (request.EntrypointKind != "task" && request.EntrypointKind != "actor") ||
		strings.TrimSpace(request.EntrypointDeclaredID) == "" {
		writeError(w, badRequest(errors.New("entrypoint_kind must be task or actor and entrypoint_declared_id is required")))
		return
	}
	if err := enterRunEntrypoint(
		r.Context(),
		s.tx,
		workerFromContext(r.Context()),
		pgvalue.UUID(leaseID),
		request,
	); err != nil {
		if writeStaleWorkerClaims(w, err) {
			return
		}
		if errors.Is(err, errStaleRunLeaseClaim) {
			writeError(w, conflict(errors.New("run entrypoint acknowledgement is stale")))
			return
		}
		s.log.Error("enter Run entrypoint failed", "run_lease_id", request.Lease.ID, "error", err)
		writeError(w, errors.New("enter run entrypoint"))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
