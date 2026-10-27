package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/helmrdotdev/helmr/internal/mcppolicy"
	"net/http"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/agent"
	"github.com/helmrdotdev/helmr/internal/audit"
	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/content"
	"github.com/helmrdotdev/helmr/internal/dispatch"
	"github.com/helmrdotdev/helmr/internal/session"
	"github.com/helmrdotdev/helmr/internal/slack"
	"github.com/helmrdotdev/helmr/internal/slackroute"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/helmrdotdev/helmr/internal/workergroup"
)

func runtimeExecution(sessionID workerapi.RuntimeSession, host workergroup.HostPrincipal) (session.Execution, error) {
	env, err := uuid.Parse(sessionID.EnvironmentID)
	if err != nil || env == uuid.Nil() {
		return session.Execution{}, errors.New("invalid Environment identity")
	}
	id, err := uuid.Parse(sessionID.SessionID)
	if err != nil || id == uuid.Nil() || sessionID.ProcessEpoch <= 0 || sessionID.ComputerLeaseEpoch <= 0 {
		return session.Execution{}, errors.New("invalid Session execution identity")
	}
	return session.Execution{EnvironmentID: env, SessionID: id, ProcessEpoch: sessionID.ProcessEpoch, LeaseEpoch: sessionID.ComputerLeaseEpoch, WorkerHostID: host.HostID, AuthenticatedHostVersion: host.HostClaimVersion, WorkerEpoch: host.Epoch}, nil
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
	authority, err := session.RenewRuntimeAuthority(r.Context(), server.tx, host, execution)
	if err != nil {
		server.writeAgentWorkerError(w, r, err)
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
	attachment, err := session.AcquireRuntimeAttachment(r.Context(), server.tx, host, execution)
	if err != nil {
		server.writeAgentWorkerError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, workerapi.AgentAttachmentResponse{Stopped: attachment.Stopped, Starting: attachment.Starting, RetireAbsent: attachment.RetireAbsent, AuthorityGeneration: attachment.Authority.Generation, AttachmentSequence: attachment.Sequence, ExpiresAt: attachment.Authority.ExpiresAt})
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
	caller := session.Caller{Kind: "session", ID: execution.SessionID, Execution: execution, Host: &host}
	method := request.Method
	if request.TurnID != "" {
		caller.TurnID, err = uuid.Parse(request.TurnID)
		if err != nil || caller.TurnID == uuid.Nil() {
			writeJSON(w, http.StatusOK, agentOperationFailure(request.RequestID, "invalid_arguments", "invalid originating Turn"))
			return
		}
	}
	if method == workerapi.AgentMethodGetMCPConnection {
		if caller.TurnID != uuid.Nil() || request.MCPScopeID != "" {
			writeJSON(w, http.StatusOK, agentOperationFailure(request.RequestID, "invalid_arguments", "invalid MCP descriptor request"))
			return
		}
		if body := bytes.TrimSpace(request.Payload); len(body) == 0 || body[0] != '{' {
			server.writeAgentAdmissionError(w, r, request.RequestID, mcppolicy.ErrInvalid)
			return
		}
		if _, err := canonicalJSON(request.Payload); err != nil {
			server.writeAgentAdmissionError(w, r, request.RequestID, mcppolicy.ErrInvalid)
			return
		}
		var input struct {
			Policy json.RawMessage `json:"policy,omitempty"`
		}
		if err := decodeAgentPayload(request.Payload, &input); err != nil {
			server.writeAgentAdmissionError(w, r, request.RequestID, mcppolicy.ErrInvalid)
			return
		}
		raw := input.Policy
		scope, err := session.IssueMCPScope(r.Context(), server.tx, caller, raw)
		if err != nil {
			server.writeAgentAdmissionError(w, r, request.RequestID, err)
			return
		}
		body, _ := json.Marshal(scope)
		writeJSON(w, http.StatusOK, workerapi.AgentOperationResponse{RequestID: request.RequestID, Value: body})
		return
	}
	if request.MCPScopeID != "" {
		caller.MCPScopeID, err = uuid.Parse(request.MCPScopeID)
		if err != nil || caller.MCPScopeID == uuid.Nil() {
			server.writeAgentAdmissionError(w, r, request.RequestID, session.ErrRuntimeMCPDenied)
			return
		}
	}
	if method == workerapi.AgentMethodRuntimeMCP {
		server.workerAgentMCP(w, r, request, caller)
		return
	} else if method == workerapi.AgentMethodStart || method == workerapi.AgentMethodSpawn || method == workerapi.AgentMethodEnqueue || method == workerapi.AgentMethodSendMessage || method == workerapi.AgentMethodControlSession || method == workerapi.AgentMethodWaitTurn || method == workerapi.AgentMethodInspectSession || method == workerapi.AgentMethodListSessions {
		writeJSON(w, http.StatusOK, agentOperationFailure(request.RequestID, "unsupported_operation", "Use Session MCP for Agent operations"))
		return
	} else if caller.TurnID == uuid.Nil() && method != workerapi.AgentMethodHold {
		writeJSON(w, http.StatusOK, agentOperationFailure(request.RequestID, "invalid_arguments", "SDK operation requires its exact originating Turn"))
		return
	}
	server.dispatchAgentOperation(w, r, request, caller)
}

func (server *Server) writeAgentAdmissionError(w http.ResponseWriter, r *http.Request, id string, err error) {
	if errors.Is(err, audit.ErrCapture) {
		server.writeOperationError(w, r, err)
		return
	}
	if notInterruptible, ok := errors.AsType[session.TurnNotInterruptibleError](err); ok {
		writeJSON(w, http.StatusOK, agentOperationFailure(id, notInterruptible.ErrorCode(), notInterruptible.Error()))
		return
	}

	code := ""
	switch {
	case errors.Is(err, mcppolicy.ErrInvalid):
		code = "invalid_runtime_mcp_policy"
	case errors.Is(err, session.ErrRuntimeMCPScopeLimit):
		code = "runtime_mcp_scope_limit"
	case errors.Is(err, session.ErrRuntimeMCPDenied):
		code = "runtime_mcp_denied"
	case errors.Is(err, slack.ErrSourceConversationPending):
		code = "source_conversation_pending"
	case errors.Is(err, slack.ErrSourceConversationUnavailable):
		code = "source_conversation_unavailable"
	case errors.Is(err, slackroute.ErrTargetNotPublished):
		code = "target_not_published"
	case errors.Is(err, slack.ErrTargetWorkspaceMismatch):
		code = "target_workspace_mismatch"
	case errors.Is(err, slackroute.ErrChannelUnavailable):
		code = "channel_unavailable"
	case errors.Is(err, slack.ErrStartMessageTooLarge):
		code = "start_message_too_large"
	case errors.Is(err, slackroute.ErrConversationChanged):
		code = "conversation_changed"
	case errors.Is(err, session.ErrAgentDeleted):
		code = "agent_deleted"
	case errors.Is(err, session.ErrRootSessionRequired):
		code = "root_session_required"
	case errors.Is(err, session.ErrMessageClosed):
		code = "message_closed"
	case errors.Is(err, content.ErrUnsupported):
		code = "content_kind_unsupported"
	case errors.Is(err, content.ErrLimit):
		code = "content_limit_exceeded"
	case errors.Is(err, session.ErrResponseAlreadyStaged):
		code = "response_already_staged"
	case errors.Is(err, content.ErrInvalid):
		code = "invalid_arguments"
	case errors.Is(err, session.ErrInvalidInput), errors.Is(err, computer.ErrInvalidInput), errors.Is(err, agent.ErrInvalidInput):
		code = "invalid_arguments"
	case errors.Is(err, session.ErrDenied), errors.Is(err, computer.ErrDenied), errors.Is(err, agent.ErrDenied):
		code = "authority_changed"
	case errors.Is(err, session.ErrConflict), errors.Is(err, computer.ErrConflict):
		code = "idempotency_conflict"
	case errors.Is(err, session.ErrNotReady), errors.Is(err, computer.ErrNotReady), errors.Is(err, agent.ErrNotReady):
		code = "admission_unavailable"
	}
	if code == "" {
		server.writeAgentWorkerError(w, r, err)
		return
	}
	if code == "authority_changed" {
		if !server.refuseOperation(w, r, "stale_authority", err) {
			return
		}
	}
	message := "Operation was not newly admitted; preserve the original arguments and idempotency key when reconciling an uncertain outcome"
	if errors.Is(err, mcppolicy.ErrInvalid) || errors.Is(err, session.ErrRuntimeMCPDenied) || errors.Is(err, session.ErrRuntimeMCPScopeLimit) || errors.Is(err, session.ErrAgentDeleted) || errors.Is(err, slack.ErrSourceConversationPending) || errors.Is(err, slack.ErrSourceConversationUnavailable) || errors.Is(err, slackroute.ErrTargetNotPublished) || errors.Is(err, slack.ErrTargetWorkspaceMismatch) || errors.Is(err, slackroute.ErrChannelUnavailable) || errors.Is(err, slack.ErrStartMessageTooLarge) || errors.Is(err, slackroute.ErrConversationChanged) || errors.Is(err, session.ErrRootSessionRequired) || errors.Is(err, content.ErrLimit) || errors.Is(err, content.ErrUnsupported) || errors.Is(err, session.ErrResponseAlreadyStaged) {
		message = err.Error()
	}
	writeJSON(w, http.StatusOK, agentOperationFailure(id, code, message))
}

func agentOperationFailure(id, code, message string) workerapi.AgentOperationResponse {
	return workerapi.AgentOperationResponse{RequestID: id, Error: &workerapi.AgentOperationError{Code: code, Message: message}}
}
func (server *Server) writeAgentWorkerError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, audit.ErrCapture) {
		server.writeOperationError(w, r, err)
		return
	}
	switch {
	case errors.Is(err, session.ErrConversationVerificationUnavailable):
		server.log.Warn("Slack input is waiting because conversation verification is not configured")
		server.writeOperationError(w, r, conflict(errors.New("session execution authority is unavailable")))
	case errors.Is(err, session.ErrInvalidInput), errors.Is(err, computer.ErrInvalidInput), errors.Is(err, dispatch.ErrInvalidInput):
		writeError(w, badRequest(errors.New("invalid Session worker operation")))
	case errors.Is(err, session.ErrConflict), errors.Is(err, computer.ErrConflict), errors.Is(err, dispatch.ErrConflict):
		server.writeOperationError(w, r, conflict(errors.New("session control changed")))
	case errors.Is(err, workergroup.ErrStaleClaims):
		server.writeOperationError(w, r, unauthorized(errors.New("worker authentication is stale")))
	case errors.Is(err, session.ErrDenied), errors.Is(err, computer.ErrDenied), errors.Is(err, dispatch.ErrDenied):
		if !server.refuseOperation(w, r, "stale_authority", err) {
			return
		}
		writeError(w, conflict(errors.New("session execution authority is unavailable")))
	case errors.Is(err, session.ErrNotReady), errors.Is(err, computer.ErrNotReady), errors.Is(err, dispatch.ErrNotReady):
		server.writeOperationError(w, r, conflict(errors.New("session execution authority is unavailable")))
	case errors.Is(err, context.Canceled):
		server.log.Info("Session worker operation cancelled", "error", err)
		writeError(w, unavailable(errors.New("session operation is temporarily unavailable")))
	default:
		server.log.Error("Session worker operation failed", "error", err)
		writeError(w, unavailable(errors.New("session operation is temporarily unavailable")))
	}
}
