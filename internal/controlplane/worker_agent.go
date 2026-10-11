package controlplane

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/agent"
	"github.com/helmrdotdev/helmr/internal/conversation"
	agentv1 "github.com/helmrdotdev/helmr/internal/proto/agent/v1"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/helmrdotdev/helmr/internal/workergroup"
)

func runtimeExecution(session workerapi.RuntimeSession, host workergroup.HostPrincipal) (agent.Execution, error) {
	env, err := uuid.Parse(session.EnvironmentID)
	if err != nil || env == uuid.Nil() {
		return agent.Execution{}, errors.New("invalid Environment identity")
	}
	id, err := uuid.Parse(session.SessionID)
	if err != nil || id == uuid.Nil() || session.ProcessEpoch <= 0 || session.ComputerLeaseEpoch <= 0 {
		return agent.Execution{}, errors.New("invalid Session execution identity")
	}
	return agent.Execution{EnvironmentID: env, SessionID: id, ProcessEpoch: session.ProcessEpoch, LeaseEpoch: session.ComputerLeaseEpoch, WorkerHostID: host.HostID, WorkerEpoch: host.Epoch}, nil
}
func (server *Server) workerAgentAuthority(w http.ResponseWriter, r *http.Request) {
	var request workerapi.RuntimeSession
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, badRequest(err))
		return
	}
	host := workerFromContext(r.Context())
	execution, err := runtimeExecution(request, host)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	authority, err := agent.RenewRuntimeAuthority(r.Context(), server.tx, host, execution)
	if err != nil {
		server.writeAgentWorkerError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, workerapi.AgentAuthorityResponse{AuthorityGeneration: authority.Generation, ExpiresAt: authority.ExpiresAt})
}
func (server *Server) workerAgentAttachment(w http.ResponseWriter, r *http.Request) {
	var request workerapi.RuntimeSession
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, badRequest(err))
		return
	}
	host := workerFromContext(r.Context())
	execution, err := runtimeExecution(request, host)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	attachment, err := agent.AcquireRuntimeAttachment(r.Context(), server.tx, host, execution)
	if err != nil {
		server.writeAgentWorkerError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, workerapi.AgentAttachmentResponse{Stopped: attachment.Stopped, Starting: attachment.Starting, AuthorityGeneration: attachment.Authority.Generation, AttachmentSequence: attachment.Sequence, ExpiresAt: attachment.Authority.ExpiresAt})
}

func (server *Server) workerAgentOperation(w http.ResponseWriter, r *http.Request) {
	var request workerapi.AgentOperationRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, badRequest(err))
		return
	}
	host := workerFromContext(r.Context())
	execution, err := runtimeExecution(request.Session, host)
	if err != nil || request.RequestID == "" || len(request.RequestID) > 512 || request.AuthorityGeneration <= 0 || !json.Valid(request.Payload) {
		writeError(w, badRequest(errors.New("invalid Session operation")))
		return
	}
	execution.AuthorityGeneration = request.AuthorityGeneration
	caller := agent.Caller{Kind: "session", ID: execution.SessionID, Execution: execution, Host: &host}
	method := agentv1.Operation_Method(request.Method)
	if request.TurnID != "" {
		caller.TurnID, err = uuid.Parse(request.TurnID)
		if err != nil || caller.TurnID == uuid.Nil() {
			writeJSON(w, http.StatusOK, agentOperationFailure(request.RequestID, "invalid_arguments", "invalid originating Turn"))
			return
		}
	}
	payload := request.Payload
	if method == agentv1.Operation_METHOD_RUNTIME_MCP {
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
			var input struct {
				Operation string `json:"operation"`
				Cursor    string `json:"cursor,omitempty"`
			}
			if err := decodeAgentPayload(call.Arguments, &input); err != nil {
				writeJSON(w, http.StatusOK, agentOperationFailure(request.RequestID, "invalid_arguments", "invalid Agent list arguments"))
				return
			}
			page, err := agent.RuntimeListAgents(r.Context(), server.tx, caller, input.Operation, input.Cursor)
			if err != nil {
				server.writeAgentAdmissionError(w, request.RequestID, err)
				return
			}
			body, _ := json.Marshal(page)
			writeJSON(w, http.StatusOK, workerapi.AgentOperationResponse{RequestID: request.RequestID, Value: body})
			return
		case "list_computers":
			server.workerAgentListComputers(w, r, request.RequestID, caller, call.Arguments)
			return
		case "spawn":
			method = agentv1.Operation_METHOD_SPAWN
			payload = call.Arguments
		case "start":
			method = agentv1.Operation_METHOD_START
			payload = call.Arguments
		case "inspect_turn":
			method = agentv1.Operation_METHOD_WAIT_TURN
			payload = call.Arguments
		case "inspect_session":
			method = agentv1.Operation_METHOD_INSPECT_SESSION
			payload = call.Arguments
		case "list_sessions":
			method = agentv1.Operation_METHOD_LIST_SESSIONS
			payload = call.Arguments
		case "send_turn":
			method = agentv1.Operation_METHOD_SEND_MESSAGE
			payload = call.Arguments
		case "interrupt_session", "resume_session", "close_session", "cancel_session":
			var input struct {
				SessionID      string `json:"sessionId"`
				HoldID         string `json:"holdId,omitempty"`
				IdempotencyKey string `json:"idempotencyKey"`
			}
			if err := decodeAgentPayload(call.Arguments, &input); err != nil || input.IdempotencyKey == "" || (call.Tool != "resume_session" && input.HoldID != "") || (call.Tool == "resume_session" && input.HoldID == "") {
				writeJSON(w, http.StatusOK, agentOperationFailure(request.RequestID, "invalid_arguments", "invalid Session control arguments"))
				return
			}
			kind := strings.TrimSuffix(call.Tool, "_session")
			payload, _ = json.Marshal(map[string]string{"sessionId": input.SessionID, "kind": kind, "holdId": input.HoldID, "idempotencyKey": input.IdempotencyKey})
			method = agentv1.Operation_METHOD_CONTROL_SESSION
		case "enqueue":
			method = agentv1.Operation_METHOD_ENQUEUE
			payload = call.Arguments
		default:
			writeJSON(w, http.StatusOK, agentOperationFailure(request.RequestID, "unsupported_operation", "Runtime operation is not available"))
			return
		}
	} else if method == agentv1.Operation_METHOD_START || method == agentv1.Operation_METHOD_SPAWN || method == agentv1.Operation_METHOD_ENQUEUE || method == agentv1.Operation_METHOD_SEND_MESSAGE || method == agentv1.Operation_METHOD_CONTROL_SESSION || method == agentv1.Operation_METHOD_WAIT_TURN || method == agentv1.Operation_METHOD_INSPECT_SESSION || method == agentv1.Operation_METHOD_LIST_SESSIONS {
		writeJSON(w, http.StatusOK, agentOperationFailure(request.RequestID, "unsupported_operation", "Use Session MCP for Agent operations"))
		return
	} else if caller.TurnID == uuid.Nil() && method != agentv1.Operation_METHOD_HOLD {
		writeJSON(w, http.StatusOK, agentOperationFailure(request.RequestID, "invalid_arguments", "SDK operation requires its exact originating Turn"))
		return
	}
	switch method {
	case agentv1.Operation_METHOD_INSPECT_SESSION, agentv1.Operation_METHOD_LIST_SESSIONS:
		server.workerAgentReadSessions(w, r, request.RequestID, caller, method, payload)
		return
	case agentv1.Operation_METHOD_WAIT_TURN:
		// Each snapshot releases the shared host operation slot. SDK and MCP
		// observers own their local deadlines and poll without holding that slot.
		var input struct {
			SessionID string `json:"sessionId"`
			TurnID    string `json:"turnId"`
		}
		if err := decodeAgentPayload(payload, &input); err != nil {
			writeJSON(w, http.StatusOK, agentOperationFailure(request.RequestID, "invalid_arguments", "invalid Turn observation"))
			return
		}
		target, targetErr := uuid.Parse(input.SessionID)
		turn, turnErr := uuid.Parse(input.TurnID)
		if targetErr != nil || turnErr != nil || target == uuid.Nil() || turn == uuid.Nil() {
			writeJSON(w, http.StatusOK, agentOperationFailure(request.RequestID, "invalid_arguments", "invalid Turn target"))
			return
		}
		view, err := agent.RuntimeObserveTurn(r.Context(), server.tx, caller, target, turn)
		if err != nil {
			server.writeAgentAdmissionError(w, request.RequestID, err)
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
	case agentv1.Operation_METHOD_REGISTER_MESSAGES:
		if !bytes.Equal(bytes.TrimSpace(payload), []byte("null")) {
			writeJSON(w, http.StatusOK, agentOperationFailure(request.RequestID, "invalid_arguments", "message registration takes no arguments"))
			return
		}
		if err := agent.RuntimeRegisterMessages(r.Context(), server.tx, host, execution, caller.TurnID); err != nil {
			server.writeAgentAdmissionError(w, request.RequestID, err)
			return
		}
		writeJSON(w, http.StatusOK, workerapi.AgentOperationResponse{RequestID: request.RequestID, Value: json.RawMessage("null")})
	case agentv1.Operation_METHOD_ASK, agentv1.Operation_METHOD_WAIT_ASK, agentv1.Operation_METHOD_WITHDRAW_ASK:
		server.workerAgentAsk(w, r, request, host, execution, caller.TurnID)
	case agentv1.Operation_METHOD_OUTPUT, agentv1.Operation_METHOD_RESPOND:
		if _, err := canonicalJSON(payload); err != nil {
			writeJSON(w, http.StatusOK, agentOperationFailure(request.RequestID, "invalid_arguments", "ambiguous content arguments"))
			return
		}
		var id string
		var content json.RawMessage
		var decodeErr error
		if method == agentv1.Operation_METHOD_OUTPUT {
			var input struct {
				OutputID string          `json:"outputId"`
				Value    json.RawMessage `json:"value"`
			}
			decodeErr = decodeAgentPayload(payload, &input)
			id, content = input.OutputID, input.Value
		} else {
			var input struct {
				ResponseID string          `json:"responseId"`
				Value      json.RawMessage `json:"value"`
			}
			decodeErr = decodeAgentPayload(payload, &input)
			id, content = input.ResponseID, input.Value
		}
		if decodeErr != nil {
			writeJSON(w, http.StatusOK, agentOperationFailure(request.RequestID, "invalid_arguments", "invalid content arguments"))
			return
		}
		operation, err := uuid.Parse(id)
		if err != nil || operation == uuid.Nil() {
			writeJSON(w, http.StatusOK, agentOperationFailure(request.RequestID, "invalid_arguments", "invalid content identity"))
			return
		}
		value := json.RawMessage("null")
		if method == agentv1.Operation_METHOD_OUTPUT {
			var sequence int64
			sequence, err = agent.RuntimeOutput(r.Context(), server.tx, host, execution, caller.TurnID, operation, content)
			if err == nil {
				value, _ = json.Marshal(map[string]int64{"sequence": sequence})
			}
		} else {
			err = agent.RuntimeRespond(r.Context(), server.tx, host, execution, caller.TurnID, operation, content)
		}
		if err != nil {
			server.writeAgentAdmissionError(w, request.RequestID, err)
			return
		}
		writeJSON(w, http.StatusOK, workerapi.AgentOperationResponse{RequestID: request.RequestID, Value: value})
	case agentv1.Operation_METHOD_HOLD:
		var input struct {
			Reason string `json:"reason"`
		}
		if err := decodeAgentPayload(payload, &input); err != nil {
			writeJSON(w, http.StatusOK, agentOperationFailure(request.RequestID, "invalid_arguments", "invalid runtime hold"))
			return
		}
		if err := agent.RuntimeHold(r.Context(), server.tx, host, execution, input.Reason); err != nil {
			server.writeAgentAdmissionError(w, request.RequestID, err)
			return
		}
		writeJSON(w, http.StatusOK, workerapi.AgentOperationResponse{RequestID: request.RequestID, Value: json.RawMessage("null")})
	case agentv1.Operation_METHOD_CLOSE_PROCESSING:
		if !bytes.Equal(bytes.TrimSpace(payload), []byte("null")) {
			writeJSON(w, http.StatusOK, agentOperationFailure(request.RequestID, "invalid_arguments", "processing close takes no arguments"))
			return
		}
		if err := agent.RuntimeCloseProcessing(r.Context(), server.tx, host, execution, caller.TurnID); err != nil {
			server.writeAgentAdmissionError(w, request.RequestID, err)
			return
		}
		writeJSON(w, http.StatusOK, workerapi.AgentOperationResponse{RequestID: request.RequestID, Value: json.RawMessage("null")})
	case agentv1.Operation_METHOD_FAIL:
		var input struct {
			Error agent.TurnError `json:"error"`
		}
		if _, err := canonicalJSON(payload); err != nil {
			writeJSON(w, http.StatusOK, agentOperationFailure(request.RequestID, "invalid_arguments", "ambiguous failure arguments"))
			return
		}
		if err := decodeAgentPayload(payload, &input); err != nil || request.DrainEvidence == "" || len(request.DrainEvidence) > 512 {
			writeJSON(w, http.StatusOK, agentOperationFailure(request.RequestID, "invalid_arguments", "failure requires native drainage evidence"))
			return
		}
		outcome, err := agent.RuntimeFail(r.Context(), server.tx, host, execution, caller.TurnID, input.Error, request.DrainEvidence)
		if err != nil {
			server.writeAgentAdmissionError(w, request.RequestID, err)
			return
		}
		writeJSON(w, http.StatusOK, workerapi.AgentOperationResponse{RequestID: request.RequestID, Value: outcome})
	case agentv1.Operation_METHOD_FINALIZE:
		server.workerAgentFinalize(w, r, request, host, execution, caller.TurnID)
	case agentv1.Operation_METHOD_START, agentv1.Operation_METHOD_SPAWN:
		var input struct {
			AgentID        string          `json:"agentId"`
			Input          json.RawMessage `json:"input"`
			ComputerID     json.RawMessage `json:"computerId,omitempty"`
			IdempotencyKey string          `json:"idempotencyKey,omitempty"`
		}
		if err := decodeAgentPayload(payload, &input); err != nil || !json.Valid(input.Input) {
			writeJSON(w, http.StatusOK, agentOperationFailure(request.RequestID, "invalid_arguments", "invalid Session admission arguments"))
			return
		}
		var computer uuid.UUID
		if len(input.ComputerID) != 0 {
			var id string
			if err := decodeAgentPayload(input.ComputerID, &id); err == nil {
				computer, _ = uuid.Parse(id)
			}
			if computer == uuid.Nil() {
				writeJSON(w, http.StatusOK, agentOperationFailure(request.RequestID, "invalid_arguments", "invalid Computer identity"))
				return
			}
		}
		if input.IdempotencyKey == "" {
			writeJSON(w, http.StatusOK, agentOperationFailure(request.RequestID, "invalid_arguments", "MCP mutation requires an idempotency key"))
			return
		}
		var preflight agent.SlackStartPreflight
		if server.slackConfig != nil {
			preflight = server.slackConfig.Client
		}
		admission := agent.StartRequest{SlackPreflight: preflight, EnvironmentID: execution.EnvironmentID, Agent: input.AgentID, ComputerID: computer, RetryKey: input.IdempotencyKey, Input: input.Input}
		var receipt agent.Admission
		if method == agentv1.Operation_METHOD_SPAWN {
			receipt, err = agent.Spawn(r.Context(), server.tx, server.secretProxy, caller, admission)
		} else {
			receipt, err = agent.Start(r.Context(), server.tx, server.secretProxy, caller, admission)
		}
		if err != nil {
			server.writeAgentAdmissionError(w, request.RequestID, err)
			return
		}
		body, _ := json.Marshal(map[string]any{"sessionId": receipt.SessionID.String(), "turnId": receipt.TurnID.String(), "sequence": receipt.Sequence, "created": receipt.Created})
		writeJSON(w, http.StatusOK, workerapi.AgentOperationResponse{RequestID: request.RequestID, Value: body})

	case agentv1.Operation_METHOD_SEND_MESSAGE:
		var input struct {
			SessionID      string          `json:"sessionId"`
			TurnID         string          `json:"turnId"`
			Message        json.RawMessage `json:"message"`
			IdempotencyKey string          `json:"idempotencyKey,omitempty"`
		}
		if err := decodeAgentPayload(payload, &input); err != nil || !json.Valid(input.Message) {
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
		receipt, err := agent.SendTurn(r.Context(), server.tx, caller, agent.EnqueueRequest{EnvironmentID: execution.EnvironmentID, SessionID: target, RetryKey: input.IdempotencyKey, Input: input.Message}, turn)
		if err != nil {
			server.writeAgentAdmissionError(w, request.RequestID, err)
			return
		}
		body, _ := json.Marshal(map[string]any{"id": receipt.MessageID.String(), "turnId": receipt.TurnID.String()})
		writeJSON(w, http.StatusOK, workerapi.AgentOperationResponse{RequestID: request.RequestID, Value: body})
	case agentv1.Operation_METHOD_CONTROL_SESSION:
		var input struct {
			SessionID      string `json:"sessionId"`
			Kind           string `json:"kind"`
			HoldID         string `json:"holdId,omitempty"`
			IdempotencyKey string `json:"idempotencyKey,omitempty"`
		}
		if err := decodeAgentPayload(payload, &input); err != nil {
			writeJSON(w, http.StatusOK, agentOperationFailure(request.RequestID, "invalid_arguments", "invalid Session control arguments"))
			return
		}
		target, err := uuid.Parse(input.SessionID)
		if err != nil || target == uuid.Nil() {
			writeJSON(w, http.StatusOK, agentOperationFailure(request.RequestID, "invalid_arguments", "invalid control target Session"))
			return
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
		receipt, err := agent.ControlSession(r.Context(), server.tx, caller, agent.SessionControlRequest{
			EnvironmentID: execution.EnvironmentID, SessionID: target, Kind: input.Kind, HoldID: hold, RetryKey: input.IdempotencyKey,
		})
		if err != nil {
			server.writeAgentAdmissionError(w, request.RequestID, err)
			return
		}
		value := map[string]any{"id": receipt.ID.String(), "sessionId": receipt.SessionID.String(), "status": "accepted"}
		if receipt.HoldID != uuid.Nil() {
			value["holdId"] = receipt.HoldID.String()
		}
		body, _ := json.Marshal(value)
		writeJSON(w, http.StatusOK, workerapi.AgentOperationResponse{RequestID: request.RequestID, Value: body})
	case agentv1.Operation_METHOD_ENQUEUE:
		var input struct {
			SessionID      string          `json:"sessionId"`
			Input          json.RawMessage `json:"input"`
			IdempotencyKey string          `json:"idempotencyKey,omitempty"`
		}
		if err := decodeAgentPayload(payload, &input); err != nil || !json.Valid(input.Input) {
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
		receipt, err := agent.Enqueue(r.Context(), server.tx, caller, agent.EnqueueRequest{EnvironmentID: execution.EnvironmentID, SessionID: target, RetryKey: input.IdempotencyKey, Input: input.Input})
		if err != nil {
			server.writeAgentAdmissionError(w, request.RequestID, err)
			return
		}
		body, _ := json.Marshal(map[string]any{"sessionId": receipt.SessionID.String(), "turnId": receipt.TurnID.String(), "sequence": receipt.Sequence})
		writeJSON(w, http.StatusOK, workerapi.AgentOperationResponse{RequestID: request.RequestID, Value: body})
	default:
		writeJSON(w, http.StatusOK, agentOperationFailure(request.RequestID, "unsupported_operation", "Runtime operation is not available"))
	}
}

func (server *Server) writeAgentAdmissionError(w http.ResponseWriter, id string, err error) {
	code := ""
	switch {
	case errors.Is(err, agent.ErrSourceConversationPending):
		code = "source_conversation_pending"
	case errors.Is(err, agent.ErrSourceConversationUnavailable):
		code = "source_conversation_unavailable"
	case errors.Is(err, agent.ErrTargetNotPublished):
		code = "target_not_published"
	case errors.Is(err, agent.ErrTargetWorkspaceMismatch):
		code = "target_workspace_mismatch"
	case errors.Is(err, agent.ErrSlackChannelUnavailable):
		code = "channel_unavailable"
	case errors.Is(err, agent.ErrStartMessageTooLarge):
		code = "start_message_too_large"
	case errors.Is(err, agent.ErrConversationChanged):
		code = "conversation_changed"
	case errors.Is(err, agent.ErrRootSessionRequired):
		code = "root_session_required"
	case errors.Is(err, agent.ErrMessageClosed):
		code = "message_closed"
	case errors.Is(err, conversation.ErrUnsupported):
		code = "content_kind_unsupported"
	case errors.Is(err, conversation.ErrLimit):
		code = "content_limit_exceeded"
	case errors.Is(err, agent.ErrResponseAlreadyStaged):
		code = "response_already_staged"
	case errors.Is(err, conversation.ErrInvalid):
		code = "invalid_arguments"
	case errors.Is(err, agent.ErrInvalidInput):
		code = "invalid_arguments"
	case errors.Is(err, agent.ErrDenied):
		code = "authority_changed"
	case errors.Is(err, agent.ErrConflict):
		code = "idempotency_conflict"
	case errors.Is(err, agent.ErrNotReady):
		code = "admission_unavailable"
	}
	if code == "" {
		server.writeAgentWorkerError(w, err)
		return
	}
	message := "Operation was not newly admitted; preserve the original arguments and idempotency key when reconciling an uncertain outcome"
	if errors.Is(err, agent.ErrSourceConversationPending) || errors.Is(err, agent.ErrSourceConversationUnavailable) || errors.Is(err, agent.ErrTargetNotPublished) || errors.Is(err, agent.ErrTargetWorkspaceMismatch) || errors.Is(err, agent.ErrSlackChannelUnavailable) || errors.Is(err, agent.ErrStartMessageTooLarge) || errors.Is(err, agent.ErrConversationChanged) || errors.Is(err, agent.ErrRootSessionRequired) || errors.Is(err, conversation.ErrLimit) || errors.Is(err, conversation.ErrUnsupported) || errors.Is(err, agent.ErrResponseAlreadyStaged) {
		message = err.Error()
	}
	writeJSON(w, http.StatusOK, agentOperationFailure(id, code, message))
}

func agentOperationFailure(id, code, message string) workerapi.AgentOperationResponse {
	return workerapi.AgentOperationResponse{RequestID: id, Error: &workerapi.AgentOperationError{Code: code, Message: message}}
}
func (server *Server) writeAgentWorkerError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, agent.ErrInvalidInput):
		writeError(w, badRequest(errors.New("invalid Session worker operation")))
	case errors.Is(err, agent.ErrConflict):
		writeError(w, conflict(errors.New("session control changed")))
	case errors.Is(err, workergroup.ErrStaleClaims):
		writeError(w, unauthorized(errors.New("worker authentication is stale")))
	case errors.Is(err, agent.ErrDenied), errors.Is(err, agent.ErrNotReady):
		writeError(w, conflict(errors.New("session execution authority is unavailable")))
	default:
		server.log.Error("Session worker operation failed", "error", err)
		writeError(w, unavailable(errors.New("session operation is temporarily unavailable")))
	}
}
