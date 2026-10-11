package controlplane

import (
	"errors"
	"github.com/helmrdotdev/helmr/internal/agent"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"net/http"
	"uuid"
)

func agentSavePublication(in workerapi.AgentSave, host workergroup.HostPrincipal) (agent.SavePublication, error) {
	env, err := uuid.Parse(in.EnvironmentID)
	if err != nil || env == uuid.Nil() {
		return agent.SavePublication{}, agent.ErrInvalidInput
	}
	id, err := uuid.Parse(in.SaveID)
	if err != nil || id == uuid.Nil() || in.LeaseEpoch <= 0 {
		return agent.SavePublication{}, agent.ErrInvalidInput
	}
	return agent.SavePublication{EnvironmentID: env, SaveID: id, LeaseEpoch: in.LeaseEpoch, Host: host}, nil
}
func (server *Server) workerRegisterAgentSaveObject(w http.ResponseWriter, r *http.Request) {
	var request workerapi.AgentSaveObject
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, badRequest(err))
		return
	}
	ref, err := agentSavePublication(request.Save, workerFromContext(r.Context()))
	if err == nil {
		err = server.savePublisher.Register(r.Context(), ref, request.Inspection)
	}
	if err != nil {
		server.writeAgentSaveError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
func (server *Server) workerCertifyAgentSaveObject(w http.ResponseWriter, r *http.Request) {
	var request workerapi.AgentSaveObject
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, badRequest(err))
		return
	}
	ref, err := agentSavePublication(request.Save, workerFromContext(r.Context()))
	if err == nil {
		err = server.savePublisher.Certify(r.Context(), ref, request.Inspection)
	}
	if err != nil {
		server.writeAgentSaveError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
func (server *Server) workerCaptureAgentSave(w http.ResponseWriter, r *http.Request) {
	var request workerapi.AgentSavePublication
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, badRequest(err))
		return
	}
	ref, err := agentSavePublication(request.Save, workerFromContext(r.Context()))
	if err == nil {
		err = server.savePublisher.Capture(r.Context(), ref, request.Root, request.Evidence)
	}
	if err != nil {
		server.writeAgentSaveError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
func (server *Server) workerPublishAgentSave(w http.ResponseWriter, r *http.Request) {
	var request workerapi.AgentSavePublication
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, badRequest(err))
		return
	}
	ref, err := agentSavePublication(request.Save, workerFromContext(r.Context()))
	if err == nil {
		err = server.savePublisher.Publish(r.Context(), ref, request.Root, request.Evidence)
	}
	if err != nil {
		server.writeAgentSaveError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (server *Server) workerNextAgentSave(w http.ResponseWriter, r *http.Request) {
	var request workerapi.AgentSaveDiscovery
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, badRequest(err))
		return
	}
	identity, err := allocationIdentity(request.AllocationIdentity)
	if err != nil || identity.Kind != "computer" {
		server.writeAgentWorkerError(w, agent.ErrInvalidInput)
		return
	}
	save, err := agent.NextComputerSave(r.Context(), server.tx, workerFromContext(r.Context()), agent.ComputerLeaseIdentity{EnvironmentID: identity.EnvironmentID, ComputerID: identity.OwnerID, InstanceID: identity.InstanceID, Epoch: identity.Epoch}, request.BackgroundDue)
	if errors.Is(err, agent.ErrNotReady) {
		writeError(w, unavailable(errors.New("save discovery is temporarily unavailable")))
		return
	}
	if err != nil {
		server.writeAgentWorkerError(w, err)
		return
	}
	result := workerapi.AgentSavePending{}
	if save != nil {
		result.Save = &workerapi.AgentSave{EnvironmentID: request.EnvironmentID, SaveID: save.ID.String(), LeaseEpoch: save.LeaseEpoch}
		result.Sequence = save.Sequence
	}
	writeJSON(w, http.StatusOK, result)
}

// A settled save cannot become publishable by retrying the same writer.
func (server *Server) writeAgentSaveError(w http.ResponseWriter, err error) {
	if errors.Is(err, agent.ErrTerminal) {
		writeError(w, conflict(errors.New("save is terminal")))
		return
	}
	server.writeAgentWorkerError(w, err)
}
