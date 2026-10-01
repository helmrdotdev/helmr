package controlplane

import (
	"fmt"
	"net/http"

	"github.com/helmrdotdev/helmr/internal/run"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func (s *Server) workerDiscoverRunLeases(w http.ResponseWriter, r *http.Request) {
	var request workerapi.RunLeaseDiscoveryRequest
	if err := decodeOptionalRequestJSON(r, &request); err != nil {
		writeError(w, fmt.Errorf("invalid worker run lease discovery request JSON: %w", err))
		return
	}

	worker := workerFromContext(r.Context())
	work, err := run.DiscoverLeases(r.Context(), s.db, worker.GroupID, worker.HostID, worker.Epoch)
	if err != nil {
		s.writeRunError(w, err, runLeaseDiscoveryOperation, worker, workerapi.RunLeaseFence{})
		return
	}
	items := make([]workerapi.RunLeaseWork, 0, len(work))
	for _, lease := range work {
		items = append(items, workerapi.RunLeaseWork{LeaseID: lease.LeaseID.String(), LeaseSequence: lease.LeaseSequence})
	}
	writeJSON(w, http.StatusOK, workerapi.RunLeaseDiscoveryResponse{Items: items})
}
