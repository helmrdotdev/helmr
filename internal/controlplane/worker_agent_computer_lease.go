package controlplane

import (
	"github.com/helmrdotdev/helmr/internal/agent"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"net/http"
	"uuid"
)

func decodeAgentComputerLease(r *http.Request) (agent.ComputerLeaseIdentity, error) {
	var request workerapi.AgentComputerLeaseRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		return agent.ComputerLeaseIdentity{}, agent.ErrInvalidInput
	}
	return agentComputerLeaseIdentity(request)
}

func agentComputerLeaseIdentity(request workerapi.AgentComputerLeaseRequest) (agent.ComputerLeaseIdentity, error) {
	env, e1 := agentComputerID(request.EnvironmentID)
	computer, e2 := agentComputerID(request.ComputerID)
	instance, e3 := agentComputerID(request.InstanceID)
	if e1 != nil || e2 != nil || e3 != nil || request.LeaseEpoch <= 0 {
		return agent.ComputerLeaseIdentity{}, agent.ErrInvalidInput
	}
	return agent.ComputerLeaseIdentity{EnvironmentID: env, ComputerID: computer, InstanceID: instance, Epoch: request.LeaseEpoch}, nil
}
func (server *Server) workerRenewAgentComputerLease(w http.ResponseWriter, r *http.Request) {
	identity, err := decodeAgentComputerLease(r)
	if err != nil {
		server.writeAgentComputerError(w, err)
		return
	}
	expires, err := agent.RenewComputerLease(r.Context(), server.tx, workerFromContext(r.Context()), identity)
	if err != nil {
		server.writeAgentComputerError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, workerapi.AgentComputerLeaseResponse{ExpiresAt: expires})
}
func (server *Server) workerAgentComputerStopped(w http.ResponseWriter, r *http.Request) {
	var request workerapi.AgentComputerStoppedRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		server.writeAgentComputerError(w, agent.ErrInvalidInput)
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
	if err = agent.ObserveComputerStopped(r.Context(), server.tx, workerFromContext(r.Context()), identity, invalidCheckpoint); err != nil {
		server.writeAgentComputerError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
