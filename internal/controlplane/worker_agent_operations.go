package controlplane

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/lifetime"
	"github.com/helmrdotdev/helmr/internal/session"
	"github.com/helmrdotdev/helmr/internal/slack"
	"github.com/helmrdotdev/helmr/internal/slackroute"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func (server *Server) dispatchAgentOperation(w http.ResponseWriter, r *http.Request, request workerapi.AgentOperationRequest, caller session.Caller) {
	switch request.Method {
	case workerapi.AgentMethodInspectSession:
		server.workerAgentInspectSession(w, r, request, caller)
	case workerapi.AgentMethodListSessions:
		server.workerAgentListSessions(w, r, request, caller)

	case workerapi.AgentMethodWaitTurn:
		server.workerAgentObserveTurn(w, r, request, caller)

	case workerapi.AgentMethodRegisterMessages:
		server.workerAgentRegisterMessages(w, r, request, caller)

	case workerapi.AgentMethodAsk:
		server.workerAgentAsk(w, r, request, *caller.Host, caller.Execution, caller.TurnID)
	case workerapi.AgentMethodWaitAsk:
		server.workerAgentWaitAsk(w, r, request, *caller.Host, caller.Execution, caller.TurnID)
	case workerapi.AgentMethodWithdrawAsk:
		server.workerAgentWithdrawAsk(w, r, request, *caller.Host, caller.Execution, caller.TurnID)

	case workerapi.AgentMethodOutput:
		server.workerAgentOutput(w, r, request, caller)
	case workerapi.AgentMethodRespond:
		server.workerAgentRespond(w, r, request, caller)

	case workerapi.AgentMethodHold:
		server.workerAgentHold(w, r, request, caller)

	case workerapi.AgentMethodCloseProcessing:
		server.workerAgentCloseProcessing(w, r, request, caller)

	case workerapi.AgentMethodFail:
		server.workerAgentFail(w, r, request, caller)

	case workerapi.AgentMethodFinalize:
		server.workerAgentFinalize(w, r, request, *caller.Host, caller.Execution, caller.TurnID)

	case workerapi.AgentMethodStart:
		server.workerAgentStartSession(w, r, request, caller)
	case workerapi.AgentMethodSpawn:
		server.workerAgentSpawn(w, r, request, caller)

	case workerapi.AgentMethodSendMessage:
		server.workerAgentSendTurn(w, r, request, caller)

	case workerapi.AgentMethodControlSession:
		server.workerAgentControlSession(w, r, request, caller)

	case workerapi.AgentMethodEnqueue:
		server.workerAgentEnqueue(w, r, request, caller)

	default:
		writeJSON(w, http.StatusOK, agentOperationFailure(request.RequestID, "unsupported_operation", "Runtime operation is not available"))
	}
}

func (server *Server) workerAgentObserveTurn(w http.ResponseWriter, r *http.Request, request workerapi.AgentOperationRequest, caller session.Caller) {
	// Each snapshot releases the shared host operation slot. SDK and MCP
	// observers own their local deadlines and poll without holding that slot.
	var input struct {
		SessionID string `json:"sessionId"`
		TurnID    string `json:"turnId"`
	}
	if err := decodeAgentPayload(request.Payload, &input); err != nil {
		writeJSON(w, http.StatusOK, agentOperationFailure(request.RequestID, "invalid_arguments", "invalid Turn observation"))
		return
	}
	target, targetErr := uuid.Parse(input.SessionID)
	turn, turnErr := uuid.Parse(input.TurnID)
	if targetErr != nil || turnErr != nil || target == uuid.Nil() || turn == uuid.Nil() {
		writeJSON(w, http.StatusOK, agentOperationFailure(request.RequestID, "invalid_arguments", "invalid Turn target"))
		return
	}
	view, err := session.RuntimeObserveTurn(r.Context(), server.auditPolicy, server.tx, caller, target, turn)
	if err != nil {
		server.writeAgentAdmissionError(w, r, request.RequestID, err)
		return
	}
	value := map[string]any{"id": view.ID.String(), "sessionId": view.SessionID.String(), "status": view.Status}
	if view.WaitBlocked != "" {
		value["waitBlocked"] = view.WaitBlocked
	}
	if view.Result != nil {
		value["result"] = view.Result
	}
	if view.Response != nil {
		value["response"] = view.Response
	}
	if view.Error != nil {
		failure := map[string]string{"code": view.Error.Code}
		if view.PayloadExpiredAt == nil {
			failure["message"] = view.Error.Message
		}
		value["error"] = failure
	}
	if view.PayloadExpiredAt != nil {
		value["payloadExpiredAt"] = view.PayloadExpiredAt.UTC()
	}
	body, _ := json.Marshal(value)
	writeJSON(w, http.StatusOK, workerapi.AgentOperationResponse{RequestID: request.RequestID, Value: body})
}

func (server *Server) workerAgentRegisterMessages(w http.ResponseWriter, r *http.Request, request workerapi.AgentOperationRequest, caller session.Caller) {
	if !bytes.Equal(bytes.TrimSpace(request.Payload), []byte("null")) {
		writeJSON(w, http.StatusOK, agentOperationFailure(request.RequestID, "invalid_arguments", "message registration takes no arguments"))
		return
	}
	if err := session.RuntimeRegisterMessages(r.Context(), server.tx, *caller.Host, caller.Execution, caller.TurnID); err != nil {
		server.writeAgentAdmissionError(w, r, request.RequestID, err)
		return
	}
	writeJSON(w, http.StatusOK, workerapi.AgentOperationResponse{RequestID: request.RequestID, Value: json.RawMessage("null")})
}

func (server *Server) workerAgentOutput(w http.ResponseWriter, r *http.Request, request workerapi.AgentOperationRequest, caller session.Caller) {
	if _, err := canonicalJSON(request.Payload); err != nil {
		writeJSON(w, http.StatusOK, agentOperationFailure(request.RequestID, "invalid_arguments", "ambiguous content arguments"))
		return
	}
	var input struct {
		OutputID string          `json:"outputId"`
		Value    json.RawMessage `json:"value"`
	}
	if err := decodeAgentPayload(request.Payload, &input); err != nil {
		writeJSON(w, http.StatusOK, agentOperationFailure(request.RequestID, "invalid_arguments", "invalid content arguments"))
		return
	}
	operation, err := uuid.Parse(input.OutputID)
	if err != nil || operation == uuid.Nil() {
		writeJSON(w, http.StatusOK, agentOperationFailure(request.RequestID, "invalid_arguments", "invalid content identity"))
		return
	}
	sequence, err := session.RuntimeOutput(r.Context(), server.tx, *caller.Host, caller.Execution, caller.TurnID, operation, input.Value)
	if err != nil {
		server.writeAgentAdmissionError(w, r, request.RequestID, err)
		return
	}
	value, _ := json.Marshal(map[string]int64{"sequence": sequence})
	writeJSON(w, http.StatusOK, workerapi.AgentOperationResponse{RequestID: request.RequestID, Value: value})
}

func (server *Server) workerAgentRespond(w http.ResponseWriter, r *http.Request, request workerapi.AgentOperationRequest, caller session.Caller) {
	if _, err := canonicalJSON(request.Payload); err != nil {
		writeJSON(w, http.StatusOK, agentOperationFailure(request.RequestID, "invalid_arguments", "ambiguous content arguments"))
		return
	}
	var input struct {
		ResponseID string          `json:"responseId"`
		Value      json.RawMessage `json:"value"`
	}
	if err := decodeAgentPayload(request.Payload, &input); err != nil {
		writeJSON(w, http.StatusOK, agentOperationFailure(request.RequestID, "invalid_arguments", "invalid content arguments"))
		return
	}
	operation, err := uuid.Parse(input.ResponseID)
	if err != nil || operation == uuid.Nil() {
		writeJSON(w, http.StatusOK, agentOperationFailure(request.RequestID, "invalid_arguments", "invalid content identity"))
		return
	}
	if err := session.RuntimeRespond(r.Context(), server.tx, *caller.Host, caller.Execution, caller.TurnID, operation, input.Value); err != nil {
		server.writeAgentAdmissionError(w, r, request.RequestID, err)
		return
	}
	value := json.RawMessage("null")
	writeJSON(w, http.StatusOK, workerapi.AgentOperationResponse{RequestID: request.RequestID, Value: value})
}

func (server *Server) workerAgentHold(w http.ResponseWriter, r *http.Request, request workerapi.AgentOperationRequest, caller session.Caller) {
	var input struct {
		Reason string `json:"reason"`
	}
	if err := decodeAgentPayload(request.Payload, &input); err != nil {
		writeJSON(w, http.StatusOK, agentOperationFailure(request.RequestID, "invalid_arguments", "invalid runtime hold"))
		return
	}
	if err := session.RuntimeHold(r.Context(), server.auditPolicy, server.tx, *caller.Host, caller.Execution, input.Reason); err != nil {
		server.writeAgentAdmissionError(w, r, request.RequestID, err)
		return
	}
	writeJSON(w, http.StatusOK, workerapi.AgentOperationResponse{RequestID: request.RequestID, Value: json.RawMessage("null")})
}

func (server *Server) workerAgentCloseProcessing(w http.ResponseWriter, r *http.Request, request workerapi.AgentOperationRequest, caller session.Caller) {
	if !bytes.Equal(bytes.TrimSpace(request.Payload), []byte("null")) {
		writeJSON(w, http.StatusOK, agentOperationFailure(request.RequestID, "invalid_arguments", "processing close takes no arguments"))
		return
	}
	if err := session.RuntimeCloseProcessing(r.Context(), server.auditPolicy, server.tx, *caller.Host, caller.Execution, caller.TurnID); err != nil {
		server.writeAgentAdmissionError(w, r, request.RequestID, err)
		return
	}
	writeJSON(w, http.StatusOK, workerapi.AgentOperationResponse{RequestID: request.RequestID, Value: json.RawMessage("null")})
}

func (server *Server) workerAgentFail(w http.ResponseWriter, r *http.Request, request workerapi.AgentOperationRequest, caller session.Caller) {
	var input struct {
		Error session.TurnError `json:"error"`
	}
	if _, err := canonicalJSON(request.Payload); err != nil {
		writeJSON(w, http.StatusOK, agentOperationFailure(request.RequestID, "invalid_arguments", "ambiguous failure arguments"))
		return
	}
	if err := decodeAgentPayload(request.Payload, &input); err != nil || request.DrainEvidence == "" || len(request.DrainEvidence) > 512 {
		writeJSON(w, http.StatusOK, agentOperationFailure(request.RequestID, "invalid_arguments", "failure requires native drainage evidence"))
		return
	}
	outcome, err := session.RuntimeFail(r.Context(), server.auditPolicy, server.tx, *caller.Host, caller.Execution, caller.TurnID, input.Error, request.DrainEvidence)
	if err != nil {
		server.writeAgentAdmissionError(w, r, request.RequestID, err)
		return
	}
	writeJSON(w, http.StatusOK, workerapi.AgentOperationResponse{RequestID: request.RequestID, Value: outcome})
}

func decodeAgentStart(payload json.RawMessage, environment uuid.UUID) (session.StartRequest, error) {
	var input struct {
		AgentID        string          `json:"agent"`
		CloseAfterIdle lifetime.Policy `json:"closeAfterIdleMs,omitzero"`
		Input          json.RawMessage `json:"input"`
		ComputerID     json.RawMessage `json:"computerId,omitempty"`
		IdempotencyKey string          `json:"idempotencyKey,omitempty"`
	}
	if err := decodeAgentPayload(payload, &input); err != nil || !json.Valid(input.Input) {
		return session.StartRequest{}, errors.New("invalid Session admission arguments")
	}
	var computer uuid.UUID
	if len(input.ComputerID) != 0 {
		var id string
		if err := decodeAgentPayload(input.ComputerID, &id); err == nil {
			computer, _ = uuid.Parse(id)
		}
		if computer == uuid.Nil() {
			return session.StartRequest{}, errors.New("invalid Computer identity")
		}
	}
	if input.IdempotencyKey == "" {
		return session.StartRequest{}, errors.New("MCP mutation requires an idempotency key")
	}
	return session.StartRequest{CloseAfterIdle: input.CloseAfterIdle, EnvironmentID: environment, Agent: input.AgentID, ComputerID: computer, IdempotencyKey: input.IdempotencyKey, Input: input.Input}, nil
}

func (server *Server) workerAgentStartSession(w http.ResponseWriter, r *http.Request, request workerapi.AgentOperationRequest, caller session.Caller) {
	admission, err := decodeAgentStart(request.Payload, caller.Execution.EnvironmentID)
	if err != nil {
		writeJSON(w, http.StatusOK, agentOperationFailure(request.RequestID, "invalid_arguments", err.Error()))
		return
	}
	var preflight slackroute.StartPreflight
	if server.slackConfig != nil {
		preflight = server.slackConfig.Client
	}
	receipt, err := slack.Start(r.Context(), server.auditPolicy, server.tx, server.secretProxy, preflight, caller, admission)
	if err != nil {
		server.writeAgentAdmissionError(w, r, request.RequestID, err)
		return
	}
	body, _ := json.Marshal(map[string]any{"sessionId": receipt.SessionID.String(), "turnId": receipt.TurnID.String(), "sequence": receipt.Sequence, "closeAfterIdleMs": receipt.CloseAfterIdleMs})
	writeJSON(w, http.StatusOK, workerapi.AgentOperationResponse{RequestID: request.RequestID, Value: body})
}

func (server *Server) workerAgentSpawn(w http.ResponseWriter, r *http.Request, request workerapi.AgentOperationRequest, caller session.Caller) {
	admission, err := decodeAgentStart(request.Payload, caller.Execution.EnvironmentID)
	if err != nil {
		writeJSON(w, http.StatusOK, agentOperationFailure(request.RequestID, "invalid_arguments", err.Error()))
		return
	}
	receipt, err := session.Spawn(r.Context(), server.auditPolicy, server.tx, server.secretProxy, caller, admission)
	if err != nil {
		server.writeAgentAdmissionError(w, r, request.RequestID, err)
		return
	}
	body, _ := json.Marshal(map[string]any{"sessionId": receipt.SessionID.String(), "turnId": receipt.TurnID.String(), "sequence": receipt.Sequence, "closeAfterIdleMs": receipt.CloseAfterIdleMs})
	writeJSON(w, http.StatusOK, workerapi.AgentOperationResponse{RequestID: request.RequestID, Value: body})
}

func (server *Server) workerAgentSendTurn(w http.ResponseWriter, r *http.Request, request workerapi.AgentOperationRequest, caller session.Caller) {
	var input struct {
		SessionID      string          `json:"sessionId"`
		TurnID         string          `json:"turnId"`
		Message        json.RawMessage `json:"message"`
		IdempotencyKey string          `json:"idempotencyKey,omitempty"`
	}
	if err := decodeAgentPayload(request.Payload, &input); err != nil || !json.Valid(input.Message) {
		writeJSON(w, http.StatusOK, agentOperationFailure(request.RequestID, "invalid_arguments", "invalid message arguments"))
		return
	}
	target, targetErr := uuid.Parse(input.SessionID)
	turn, turnErr := uuid.Parse(input.TurnID)
	if targetErr != nil || turnErr != nil || target == uuid.Nil() || turn == uuid.Nil() {
		writeJSON(w, http.StatusOK, agentOperationFailure(request.RequestID, "invalid_arguments", "invalid message target"))
		return
	}
	if input.IdempotencyKey == "" {
		writeJSON(w, http.StatusOK, agentOperationFailure(request.RequestID, "invalid_arguments", "MCP mutation requires an idempotency key"))
		return
	}
	receipt, err := session.SendTurn(r.Context(), server.auditPolicy, server.tx, caller, session.EnqueueRequest{EnvironmentID: caller.Execution.EnvironmentID, SessionID: target, IdempotencyKey: input.IdempotencyKey, Input: input.Message}, turn)
	if err != nil {
		server.writeAgentAdmissionError(w, r, request.RequestID, err)
		return
	}
	body, _ := json.Marshal(map[string]any{"id": receipt.MessageID.String(), "turnId": receipt.TurnID.String()})
	writeJSON(w, http.StatusOK, workerapi.AgentOperationResponse{RequestID: request.RequestID, Value: body})
}

func (server *Server) workerAgentControlSession(w http.ResponseWriter, r *http.Request, request workerapi.AgentOperationRequest, caller session.Caller) {
	var input struct {
		SessionID      string `json:"sessionId"`
		Kind           string `json:"kind"`
		TurnID         string `json:"turnId,omitempty"`
		HoldID         string `json:"holdId,omitempty"`
		IdempotencyKey string `json:"idempotencyKey,omitempty"`
	}
	if err := decodeAgentPayload(request.Payload, &input); err != nil {
		writeJSON(w, http.StatusOK, agentOperationFailure(request.RequestID, "invalid_arguments", "invalid Session control arguments"))
		return
	}
	target, err := uuid.Parse(input.SessionID)
	if err != nil || target == uuid.Nil() {
		writeJSON(w, http.StatusOK, agentOperationFailure(request.RequestID, "invalid_arguments", "invalid control target Session"))
		return
	}
	var targetTurn uuid.UUID
	if input.TurnID != "" {
		targetTurn, err = uuid.Parse(input.TurnID)
		if err != nil || targetTurn == uuid.Nil() {
			writeJSON(w, http.StatusOK, agentOperationFailure(request.RequestID, "invalid_arguments", "invalid control target Turn"))
			return
		}
	}
	var hold uuid.UUID
	if input.HoldID != "" {
		hold, err = uuid.Parse(input.HoldID)
		if err != nil || hold == uuid.Nil() {
			writeJSON(w, http.StatusOK, agentOperationFailure(request.RequestID, "invalid_arguments", "invalid control hold"))
			return
		}
	}
	if input.IdempotencyKey == "" {
		writeJSON(w, http.StatusOK, agentOperationFailure(request.RequestID, "invalid_arguments", "MCP mutation requires an idempotency key"))
		return
	}
	receipt, err := session.ControlSession(r.Context(), server.auditPolicy, server.tx, caller, session.SessionControlRequest{
		EnvironmentID: caller.Execution.EnvironmentID, SessionID: target, TurnID: targetTurn, Kind: input.Kind, HoldID: hold, IdempotencyKey: input.IdempotencyKey,
	})
	if err != nil {
		server.writeAgentAdmissionError(w, r, request.RequestID, err)
		return
	}
	value := map[string]any{"id": receipt.ID.String(), "sessionId": receipt.SessionID.String(), "status": "accepted"}
	if receipt.HoldID != uuid.Nil() {
		value["holdId"] = receipt.HoldID.String()
	}
	if targetTurn != uuid.Nil() {
		value["turnId"] = targetTurn.String()
	}
	body, _ := json.Marshal(value)
	writeJSON(w, http.StatusOK, workerapi.AgentOperationResponse{RequestID: request.RequestID, Value: body})
}

func (server *Server) workerAgentEnqueue(w http.ResponseWriter, r *http.Request, request workerapi.AgentOperationRequest, caller session.Caller) {
	var input struct {
		SessionID      string          `json:"sessionId"`
		Input          json.RawMessage `json:"input"`
		IdempotencyKey string          `json:"idempotencyKey,omitempty"`
	}
	if err := decodeAgentPayload(request.Payload, &input); err != nil || !json.Valid(input.Input) {
		writeJSON(w, http.StatusOK, agentOperationFailure(request.RequestID, "invalid_arguments", "invalid enqueue arguments"))
		return
	}
	target, err := uuid.Parse(input.SessionID)
	if err != nil || target == uuid.Nil() {
		writeJSON(w, http.StatusOK, agentOperationFailure(request.RequestID, "invalid_arguments", "invalid target Session"))
		return
	}
	if input.IdempotencyKey == "" {
		writeJSON(w, http.StatusOK, agentOperationFailure(request.RequestID, "invalid_arguments", "MCP mutation requires an idempotency key"))
		return
	}
	receipt, err := session.Enqueue(r.Context(), server.auditPolicy, server.tx, server.secretProxy, caller, session.EnqueueRequest{EnvironmentID: caller.Execution.EnvironmentID, SessionID: target, IdempotencyKey: input.IdempotencyKey, Input: input.Input})
	if err != nil {
		server.writeAgentAdmissionError(w, r, request.RequestID, err)
		return
	}
	body, _ := json.Marshal(map[string]any{"sessionId": receipt.SessionID.String(), "turnId": receipt.TurnID.String(), "sequence": receipt.Sequence})
	writeJSON(w, http.StatusOK, workerapi.AgentOperationResponse{RequestID: request.RequestID, Value: body})
}
