package controlplane

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/run"
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
	worker := workerFromContext(r.Context())
	fence := workerExecutionFence(worker, parsedRunLeaseFence{leaseID: leaseID}, request.Lease)
	if err := run.EnterEntrypoint(r.Context(), s.tx, fence, request.EntrypointKind, request.EntrypointDeclaredID); err != nil {
		s.writeRunError(w, err, runEntrypointOperation, worker, request.Lease)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
