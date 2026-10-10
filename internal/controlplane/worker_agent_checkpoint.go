package controlplane

import (
	"github.com/helmrdotdev/helmr/internal/agent"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"net/http"
)

func (server *Server) workerRegisterAgentCheckpoint(w http.ResponseWriter, r *http.Request) {
	var request workerapi.AgentCheckpointPublication
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, badRequest(err))
		return
	}
	env, err := agentComputerID(request.EnvironmentID)
	if err == nil {
		err = server.savePublisher.RegisterCheckpoint(r.Context(), agent.CheckpointPublication{EnvironmentID: env, Host: workerFromContext(r.Context())}, request.Manifest)
	}
	if err != nil {
		server.writeAgentSaveError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
func (server *Server) workerCompleteAgentCheckpoint(w http.ResponseWriter, r *http.Request) {
	var request workerapi.AgentCheckpointPublication
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, badRequest(err))
		return
	}
	env, err := agentComputerID(request.EnvironmentID)
	if err == nil {
		err = server.savePublisher.CompleteCheckpoint(r.Context(), agent.CheckpointPublication{EnvironmentID: env, Host: workerFromContext(r.Context())}, request.Manifest)
	}
	if err != nil {
		server.writeAgentSaveError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
func (server *Server) workerReadAgentCheckpoint(w http.ResponseWriter, r *http.Request) {
	identity, err := decodeAllocationIdentity(r, "computer")
	if err != nil {
		server.writeAllocationError(w, err)
		return
	}
	result, err := agent.ReadComputerCheckpoint(r.Context(), server.tx, workerFromContext(r.Context()), agent.ComputerLeaseIdentity{EnvironmentID: identity.EnvironmentID, ComputerID: identity.OwnerID, InstanceID: identity.InstanceID, Epoch: identity.Epoch})
	if err != nil {
		server.writeAllocationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}
