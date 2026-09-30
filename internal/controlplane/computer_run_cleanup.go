package controlplane

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func (s *Server) workerGetComputerRunCleanup(w http.ResponseWriter, r *http.Request) {
	var request workerapi.ComputerRunCleanupRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, fmt.Errorf("invalid Run cleanup request: %w", err))
		return
	}
	writer, ok := runCleanupWriter(request)
	if !ok {
		writeError(w, badRequest(errors.New("invalid Run cleanup request")))
		return
	}
	process, err := computer.RunCleanup(r.Context(), s.tx, workerFromContext(r.Context()), writer)
	if err != nil {
		writeError(w, computerError(err, computerRunCleanupOperation))
		return
	}
	var response workerapi.ComputerRunCleanupResponse
	if process != nil {
		response.Run = &workerapi.ComputerRunCleanup{RunID: process.RunID.String(), RunLeaseID: process.RunLeaseID.String(), AttemptNumber: process.AttemptNumber}
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) workerReconcileComputerRun(w http.ResponseWriter, r *http.Request) {
	var request workerapi.ComputerRunReconcileRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, fmt.Errorf("invalid Run reconciliation request: %w", err))
		return
	}
	writer, ok := runCleanupWriter(request.ComputerRunCleanupRequest)
	runID, runErr := ids.Parse(request.RunID)
	leaseID, leaseErr := ids.Parse(request.RunLeaseID)
	if !ok || runErr != nil || leaseErr != nil || request.AttemptNumber == 0 {
		writeError(w, badRequest(errors.New("invalid Run reconciliation request")))
		return
	}
	process := computer.RunProcess{RunID: runID, RunLeaseID: leaseID, AttemptNumber: request.AttemptNumber}
	if err := computer.ReconcileRun(r.Context(), s.tx, workerFromContext(r.Context()), writer, process); err != nil {
		writeError(w, computerError(err, computerRunCleanupOperation))
		return
	}
	writeJSON(w, http.StatusOK, struct{}{})
}

func runCleanupWriter(request workerapi.ComputerRunCleanupRequest) (computer.WriterRef, bool) {
	environmentID, environmentErr := ids.Parse(request.EnvironmentID)
	instanceID, instanceErr := ids.Parse(request.ComputerInstanceID)
	if environmentErr != nil || instanceErr != nil || request.WriterGeneration <= 0 {
		return computer.WriterRef{}, false
	}
	return computer.WriterRef{EnvironmentID: environmentID, InstanceID: instanceID, WriterGeneration: request.WriterGeneration}, true
}
