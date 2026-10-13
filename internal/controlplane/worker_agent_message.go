package controlplane

import (
	"errors"
	"net/http"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/session"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func (server *Server) workerAgentMessage(w http.ResponseWriter, r *http.Request) {
	var request workerapi.AgentMessageRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, badRequest(err))
		return
	}
	host := workerFromContext(r.Context())
	e, err := runtimeExecution(request.Session, host)
	turn, parseErr := uuid.Parse(request.TurnID)
	if err != nil || parseErr != nil || turn == uuid.Nil() || request.AttachmentSequence <= 0 || request.AuthorityGeneration <= 0 {
		writeError(w, badRequest(errors.New("invalid message dispatch identity")))
		return
	}
	e.AuthorityGeneration = request.AuthorityGeneration
	next, err := session.ClaimRuntimeMessage(r.Context(), server.tx, host, e, request.AttachmentSequence, turn)
	if err != nil {
		server.writeAgentWorkerError(w, err)
		return
	}
	var response *workerapi.AgentMessageDispatch
	if next != nil {
		response = &workerapi.AgentMessageDispatch{TurnID: next.TurnID.String(), MessageID: next.ID.String(), Input: next.Input, Source: agentInputSource(next.Source)}
	}
	writeJSON(w, http.StatusOK, response)
}
func (server *Server) workerAgentMessageReceipt(w http.ResponseWriter, r *http.Request) {
	var request workerapi.AgentMessageReceipt
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, badRequest(err))
		return
	}
	host := workerFromContext(r.Context())
	e, err := runtimeExecution(request.Session, host)
	turn, turnErr := uuid.Parse(request.TurnID)
	id, idErr := uuid.Parse(request.MessageID)
	if err != nil || turnErr != nil || idErr != nil || turn == uuid.Nil() || id == uuid.Nil() || request.AttachmentSequence <= 0 {
		writeError(w, badRequest(errors.New("invalid message receipt identity")))
		return
	}
	if err := session.CompleteRuntimeMessage(r.Context(), server.tx, host, e, request.AttachmentSequence, turn, id, request.RejectionReason); err != nil {
		server.writeAgentWorkerError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, nil)
}
