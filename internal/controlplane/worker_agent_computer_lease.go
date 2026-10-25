package controlplane

import (
	"net/http"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/dispatch"

	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/session"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func decodeAgentComputerLease(r *http.Request) (computer.LeaseIdentity, error) {
	var request workerapi.AgentComputerLeaseRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		return computer.LeaseIdentity{}, session.ErrInvalidInput
	}
	return agentComputerLeaseIdentity(request)
}

func agentComputerLeaseIdentity(request workerapi.AgentComputerLeaseRequest) (computer.LeaseIdentity, error) {
	env, e1 := agentComputerID(request.EnvironmentID)
	computerID, e2 := agentComputerID(request.ComputerID)
	instance, e3 := agentComputerID(request.AllocationID)
	if e1 != nil || e2 != nil || e3 != nil || request.LeaseEpoch <= 0 {
		return computer.LeaseIdentity{}, session.ErrInvalidInput
	}
	return computer.LeaseIdentity{EnvironmentID: env, ComputerID: computerID, InstanceID: instance, Epoch: request.LeaseEpoch}, nil
}
func (server *Server) workerRenewAgentComputerLease(w http.ResponseWriter, r *http.Request) {
	identity, err := decodeAgentComputerLease(r)
	if err != nil {
		server.writeAgentComputerError(w, err)
		return
	}
	expires, err := computer.RenewLease(r.Context(), server.tx, workerFromContext(r.Context()), identity)
	if err != nil {
		server.writeAgentComputerError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, workerapi.AgentComputerLeaseResponse{ExpiresAt: expires})
}
func (server *Server) workerAgentComputerStopped(w http.ResponseWriter, r *http.Request) {
	var request workerapi.AgentComputerStoppedRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		server.writeAgentComputerError(w, session.ErrInvalidInput)
		return
	}
	identity, err := agentComputerLeaseIdentity(request.AgentComputerLeaseRequest)
	if err != nil {
		server.writeAgentComputerError(w, err)
		return
	}
	invalidCheckpoint := uuid.Nil()
	if request.InvalidCheckpointID != "" {
		invalidCheckpoint, err = agentComputerID(request.InvalidCheckpointID)
		if err != nil {
			server.writeAgentComputerError(w, err)
			return
		}
	}
	failedBootSeed := uuid.Nil()
	if request.FailedBootSeedID != "" {
		failedBootSeed, err = agentComputerID(request.FailedBootSeedID)
		if err != nil {
			server.writeAgentComputerError(w, err)
			return
		}
	}
	if err = dispatch.ObserveLeaseStopped(r.Context(), server.auditPolicy, server.tx, workerFromContext(r.Context()), identity, invalidCheckpoint, failedBootSeed); err != nil {
		server.writeAgentComputerError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
