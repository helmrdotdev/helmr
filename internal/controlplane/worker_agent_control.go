package controlplane

import (
	"errors"
	"net/http"

	"github.com/helmrdotdev/helmr/internal/agent"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func (server *Server) workerAgentReady(w http.ResponseWriter, r *http.Request) {
	var request workerapi.AgentControlRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, badRequest(err))
		return
	}
	host := workerFromContext(r.Context())
	e, err := runtimeExecution(request.Session, host)
	if err != nil || request.AttachmentSequence <= 0 {
		writeError(w, badRequest(errors.New("invalid Session readiness identity")))
		return
	}
	if err := agent.ObserveSessionReady(r.Context(), server.tx, host, e, request.AttachmentSequence); err != nil {
		if errors.Is(err, agent.ErrConflict) {
			writeJSON(w, http.StatusOK, struct{}{})
			return
		}
		server.writeAgentWorkerError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, struct{}{})
}

func (server *Server) workerAgentControl(w http.ResponseWriter, r *http.Request) {
	var request workerapi.AgentControlRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, badRequest(err))
		return
	}
	host := workerFromContext(r.Context())
	e, err := runtimeExecution(request.Session, host)
	if err != nil || request.AttachmentSequence <= 0 {
		writeError(w, badRequest(errors.New("invalid Session control identity")))
		return
	}
	control, err := agent.PrepareSessionDelivery(r.Context(), server.tx, host, e, request.AttachmentSequence)
	if err != nil {
		server.writeAgentWorkerError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, workerapi.AgentControlResponse{Sequence: control.Sequence, AuthorityGeneration: control.Generation, Kind: control.Kind, Acknowledged: control.Acknowledged, Error: control.Error, ExpiresAt: control.ExpiresAt})
}

func (server *Server) workerAgentControlReceipt(w http.ResponseWriter, r *http.Request) {
	var request workerapi.AgentControlReceipt
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, badRequest(err))
		return
	}
	host := workerFromContext(r.Context())
	e, err := runtimeExecution(request.Session, host)
	if err != nil || request.AttachmentSequence <= 0 {
		writeError(w, badRequest(errors.New("invalid Session control receipt")))
		return
	}
	if err := agent.AcknowledgeSessionDelivery(r.Context(), server.tx, host, e, request.AttachmentSequence, request.Sequence, request.AuthorityGeneration, request.Kind, request.Error); err != nil {
		if errors.Is(err, agent.ErrConflict) {
			// Current authority accepted the receipt, but a newer desired control
			// has superseded it. Retire the old guest event and keep converging.
			writeJSON(w, http.StatusOK, struct {
				Superseded bool `json:"superseded"`
			}{true})
			return
		}
		server.writeAgentWorkerError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, struct{}{})
}

func (server *Server) workerAgentStopped(w http.ResponseWriter, r *http.Request) {
	var request workerapi.AgentControlRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, badRequest(err))
		return
	}
	host := workerFromContext(r.Context())
	e, err := runtimeExecution(request.Session, host)
	if err != nil || request.AttachmentSequence <= 0 {
		writeError(w, badRequest(errors.New("invalid Session stop identity")))
		return
	}
	if err := agent.ObserveSessionStopped(r.Context(), server.tx, host, e, request.AttachmentSequence); err != nil {
		server.writeAgentWorkerError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, struct{}{})
}

func (server *Server) workerAgentFailure(w http.ResponseWriter, r *http.Request) {
	var request workerapi.AgentControlRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, badRequest(err))
		return
	}
	host := workerFromContext(r.Context())
	e, err := runtimeExecution(request.Session, host)
	if err != nil || request.AttachmentSequence <= 0 {
		writeError(w, badRequest(errors.New("invalid Session failure identity")))
		return
	}
	if err := agent.ObserveSessionFailure(r.Context(), server.tx, host, e, request.AttachmentSequence); err != nil {
		server.writeAgentWorkerError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, struct{}{})
}
