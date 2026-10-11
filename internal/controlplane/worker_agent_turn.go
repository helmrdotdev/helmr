package controlplane

import (
	"errors"
	"net/http"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/agent"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func (server *Server) workerAgentTurn(w http.ResponseWriter, r *http.Request) {
	var request workerapi.AgentTurnRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, badRequest(err))
		return
	}
	host := workerFromContext(r.Context())
	e, err := runtimeExecution(request.Session, host)
	if err != nil || request.AttachmentSequence <= 0 || request.AuthorityGeneration <= 0 {
		writeError(w, badRequest(errors.New("invalid Turn dispatch identity")))
		return
	}
	e.AuthorityGeneration = request.AuthorityGeneration
	dispatch, err := agent.DispatchRuntimeTurn(r.Context(), server.tx, host, e, request.AttachmentSequence)
	if err != nil {
		server.writeAgentWorkerError(w, err)
		return
	}
	var response *workerapi.AgentTurnDispatch
	if dispatch != nil {
		response = &workerapi.AgentTurnDispatch{TurnID: dispatch.TurnID.String(), Sequence: dispatch.Sequence, CreatedAt: dispatch.CreatedAt, Input: dispatch.Input, Source: agentInputSource(dispatch.Source)}
	}
	writeJSON(w, http.StatusOK, response)
}

func (server *Server) workerAgentTurnReceipt(w http.ResponseWriter, r *http.Request) {
	var request workerapi.AgentTurnReceipt
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, badRequest(err))
		return
	}
	host := workerFromContext(r.Context())
	e, err := runtimeExecution(request.Session, host)
	turn, turnErr := uuid.Parse(request.TurnID)
	if err != nil || turnErr != nil || turn == uuid.Nil() || request.AttachmentSequence <= 0 {
		writeError(w, badRequest(errors.New("invalid Turn receipt identity")))
		return
	}
	sequence, err := agent.ObserveRuntimeTurn(r.Context(), server.tx, host, e, request.AttachmentSequence, turn, request.Outcome)
	if err != nil {
		server.writeAgentWorkerError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, workerapi.AgentTurnAcknowledgment{Sequence: sequence})
}

func agentInputSource(source agent.TurnSource) workerapi.AgentTurnSource {
	result := workerapi.AgentTurnSource{Kind: source.Kind}
	if source.RequesterSessionID != nil {
		result.RequesterSessionID = source.RequesterSessionID.String()
	}
	if source.OriginTurnID != nil {
		result.OriginTurnID = source.OriginTurnID.String()
	}
	if source.Slack != nil {
		result.Slack = &workerapi.SlackInputSource{TeamID: source.Slack.TeamID, ChannelID: source.Slack.ChannelID, ThreadTS: source.Slack.ThreadTS, MessageTS: source.Slack.MessageTS, UserID: source.Slack.UserID}
	}
	return result
}
