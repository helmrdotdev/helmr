package controlplane

import (
	"errors"
	"net/http"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/session"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/helmrdotdev/helmr/internal/workergroup"
)

func agentSavePublication(in workerapi.AgentSave, host workergroup.HostPrincipal) (computer.SavePublication, error) {
	env, err := uuid.Parse(in.EnvironmentID)
	if err != nil || env == uuid.Nil() {
		return computer.SavePublication{}, session.ErrInvalidInput
	}
	id, err := uuid.Parse(in.SaveID)
	if err != nil || id == uuid.Nil() || in.LeaseEpoch <= 0 {
		return computer.SavePublication{}, session.ErrInvalidInput
	}
	return computer.SavePublication{EnvironmentID: env, SaveID: id, LeaseEpoch: in.LeaseEpoch, Host: host}, nil
}
func (server *Server) workerRegisterAgentSaveObject(w http.ResponseWriter, r *http.Request) {
	var request workerapi.AgentSaveObject
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, badRequest(err))
		return
	}
	ref, err := agentSavePublication(request.Save, workerFromContext(r.Context()))
	if err == nil {
		err = server.savePublisher.RegisterBatch(r.Context(), ref, request.Inspections)
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
		err = server.savePublisher.CertifyBatch(r.Context(), ref, request.Inspections)
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
		server.writeAgentWorkerError(w, session.ErrInvalidInput)
		return
	}
	var retirement *computer.SaveRetirement
	if request.Retirement != nil {
		id, parseErr := uuid.Parse(request.Retirement.SaveID)
		if parseErr != nil {
			server.writeAgentWorkerError(w, session.ErrInvalidInput)
			return
		}
		retirement = &computer.SaveRetirement{SaveID: id, Root: request.Retirement.Root}
	}
	save, err := computer.NextSave(r.Context(), server.tx, workerFromContext(r.Context()), computer.LeaseIdentity{EnvironmentID: identity.EnvironmentID, ComputerID: identity.OwnerID, InstanceID: identity.InstanceID, Epoch: identity.Epoch}, request.Admit, request.BackgroundDue, retirement)
	if errors.Is(err, session.ErrNotReady) || errors.Is(err, computer.ErrNotReady) {
		writeError(w, unavailable(errors.New("save discovery is temporarily unavailable")))
		return
	}
	if err != nil {
		server.writeAgentSaveError(w, err)
		return
	}
	result := workerapi.AgentSavePending{}
	if save != nil && save.Claimed {
		result.Save = &workerapi.AgentSave{EnvironmentID: request.EnvironmentID, SaveID: save.ID.String(), LeaseEpoch: save.LeaseEpoch}
		result.Sequence = save.Sequence
	} else if save != nil {
		result.PendingForMillis = &save.AgeMillis
	}
	writeJSON(w, http.StatusOK, result)
}

// A settled save cannot become publishable by retrying the same writer.
func (server *Server) writeAgentSaveError(w http.ResponseWriter, err error) {
	if errors.Is(err, session.ErrTerminal) || errors.Is(err, computer.ErrTerminal) || errors.Is(err, computer.ErrDenied) {
		writeError(w, conflict(codedError{code: workerapi.SaveAuthorityLost, message: "save writer authority is unavailable"}))
		return
	}
	server.writeAgentWorkerError(w, err)
}
