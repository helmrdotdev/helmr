package controlplane

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/helmrdotdev/helmr/internal/agent"
	"github.com/helmrdotdev/helmr/internal/session"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func (server *Server) workerAgentMCP(w http.ResponseWriter, r *http.Request, request workerapi.AgentOperationRequest, caller session.Caller) {
	method := request.Method
	payload := request.Payload

	if request.TurnID != "" {
		writeJSON(w, http.StatusOK, agentOperationFailure(request.RequestID, "invalid_arguments", "Session MCP cannot infer an originating Turn"))
		return
	}
	var call struct {
		Tool      string          `json:"tool"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := decodeAgentPayload(payload, &call); err != nil {
		writeJSON(w, http.StatusOK, agentOperationFailure(request.RequestID, "invalid_arguments", err.Error()))
		return
	}
	switch call.Tool {
	case "list_agents":
		server.workerAgentListAgents(w, r, request.RequestID, caller, call.Arguments)
		return
	case "list_computers":
		server.workerAgentListComputers(w, r, request.RequestID, caller, call.Arguments)
		return
	case "spawn":
		method = workerapi.AgentMethodSpawn
		payload = call.Arguments
	case "start":
		method = workerapi.AgentMethodStart
		payload = call.Arguments
	case "inspect_turn":
		method = workerapi.AgentMethodWaitTurn
		payload = call.Arguments
	case "inspect_session":
		method = workerapi.AgentMethodInspectSession
		payload = call.Arguments
	case "list_sessions":
		method = workerapi.AgentMethodListSessions
		payload = call.Arguments
	case "send_turn":
		method = workerapi.AgentMethodSendMessage
		payload = call.Arguments
	case "interrupt_turn", "interrupt_session", "resume_session", "close_session", "cancel_session":
		var input struct {
			SessionID      string `json:"sessionId"`
			TurnID         string `json:"turnId,omitempty"`
			HoldID         string `json:"holdId,omitempty"`
			IdempotencyKey string `json:"idempotencyKey"`
		}
		if err := decodeAgentPayload(call.Arguments, &input); err != nil || input.IdempotencyKey == "" || ((call.Tool == "interrupt_turn") != (input.TurnID != "")) || (call.Tool != "resume_session" && input.HoldID != "") || (call.Tool == "resume_session" && input.HoldID == "") {
			writeJSON(w, http.StatusOK, agentOperationFailure(request.RequestID, "invalid_arguments", "invalid Session control arguments"))
			return
		}
		kind := strings.TrimSuffix(call.Tool, "_session")
		if call.Tool == "interrupt_turn" {
			kind = "turn_interrupt"
		}
		payload, _ = json.Marshal(map[string]string{"sessionId": input.SessionID, "kind": kind, "turnId": input.TurnID, "holdId": input.HoldID, "idempotencyKey": input.IdempotencyKey})
		method = workerapi.AgentMethodControlSession
	case "enqueue":
		method = workerapi.AgentMethodEnqueue
		payload = call.Arguments
	default:
		writeJSON(w, http.StatusOK, agentOperationFailure(request.RequestID, "unsupported_operation", "Runtime operation is not available"))
		return
	}
	request.Method, request.Payload = method, payload
	server.dispatchAgentOperation(w, r, request, caller)
}

func (server *Server) workerAgentListAgents(w http.ResponseWriter, r *http.Request, requestID string, caller session.Caller, payload json.RawMessage) {
	var input struct {
		Operation string `json:"operation"`
		Cursor    string `json:"cursor,omitempty"`
	}
	if err := decodeAgentPayload(payload, &input); err != nil {
		writeJSON(w, http.StatusOK, agentOperationFailure(requestID, "invalid_arguments", "invalid Agent list arguments"))
		return
	}
	page, err := agent.RuntimeListAgents(r.Context(), server.tx, caller, input.Operation, input.Cursor)
	if err != nil {
		server.writeAgentAdmissionError(w, requestID, err)
		return
	}
	body, _ := json.Marshal(page)
	writeJSON(w, http.StatusOK, workerapi.AgentOperationResponse{RequestID: requestID, Value: body})
	return
}
