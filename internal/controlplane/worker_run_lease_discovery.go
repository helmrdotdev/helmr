package controlplane

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func (s *Server) workerDiscoverRunLeases(w http.ResponseWriter, r *http.Request) {
	var request workerapi.RunLeaseDiscoveryRequest
	if err := decodeOptionalRequestJSON(r, &request); err != nil {
		writeError(w, fmt.Errorf("invalid worker run lease discovery request JSON: %w", err))
		return
	}

	worker := workerFromContext(r.Context())
	response, err := discoverWorkerRunLeases(
		r.Context(),
		s.db,
		worker.GroupID,
		pgvalue.UUID(worker.HostID),
		worker.Epoch,
	)
	if err != nil {
		s.log.Error("discover worker run leases failed",
			"worker_host_id", worker.HostID.String(),
			"worker_epoch", worker.Epoch,
			"error", err,
		)
		writeError(w, errors.New("discover worker run leases"))
		return
	}
	writeJSON(w, http.StatusOK, response)
}
