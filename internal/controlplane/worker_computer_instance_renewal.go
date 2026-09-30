package controlplane

import (
	"errors"
	"net/http"

	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func (s *Server) workerRenewComputerInstance(w http.ResponseWriter, r *http.Request) {
	var request workerapi.ComputerInstanceRenewRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, err)
		return
	}
	environmentID, environmentErr := ids.Parse(request.EnvironmentID)
	instanceID, instanceErr := ids.Parse(request.ComputerInstanceID)
	if environmentErr != nil || instanceErr != nil || request.WriterGeneration <= 0 {
		writeError(w, badRequest(errors.New("environment, Instance and positive writer generation are required")))
		return
	}
	instance, err := computer.RenewInstance(r.Context(), s.tx, workerFromContext(r.Context()), computer.WriterRef{EnvironmentID: environmentID, InstanceID: instanceID, WriterGeneration: request.WriterGeneration})
	if err != nil {
		s.writeWorkerComputerError(w, err, computerInstanceRenewalOperation, "Computer Instance renewal failed")
		return
	}
	writeJSON(w, http.StatusOK, workerapi.ComputerInstanceRenewResponse{ComputerInstanceID: pgvalue.UUIDString(instance.ID), WriterGeneration: instance.WriterGeneration, DesiredState: instance.DesiredState, DesiredVersion: instance.DesiredVersion, ObservedVersion: instance.ObservedVersion, WriterExpiresAt: instance.WriterExpiresAt.Time})
}
