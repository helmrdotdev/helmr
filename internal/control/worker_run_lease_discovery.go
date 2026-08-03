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

func (s *Server) workerDiscoverRunLeases(w http.ResponseWriter, r *http.Request) {
	var request api.WorkerRunLeaseDiscoveryRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, badRequest(fmt.Errorf("invalid worker run lease discovery request JSON: %w", err)))
		return
	}

	worker := workerFromContext(r.Context())
	response, err := discoverWorkerRunLeases(
		r.Context(),
		s.db,
		worker.WorkerGroupID,
		pgvalue.UUID(worker.WorkerInstanceID),
		worker.WorkerEpoch,
		worker.ProtocolVersion,
	)
	if err != nil {
		s.log.Error("discover worker run leases failed",
			"worker_instance_id", worker.WorkerInstanceID.String(),
			"worker_epoch", worker.WorkerEpoch,
			"error", err,
		)
		writeError(w, errors.New("discover worker run leases"))
		return
	}
	writeJSON(w, http.StatusOK, response)
}
