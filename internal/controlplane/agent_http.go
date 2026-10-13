package controlplane

import (
	"errors"
	"github.com/helmrdotdev/helmr/internal/content"
	"net/http"
	"uuid"

	"github.com/go-chi/chi/v5"
	"github.com/helmrdotdev/helmr/internal/agent"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
)

// externalAgentCaller derives identity only from authenticated transport. The
// operation owner rechecks current membership/key authority in its transaction.
func externalAgentCaller(principal auth.Principal) (agent.Caller, error) {
	switch principal.Kind {
	case auth.PrincipalKindSession:
		return agent.Caller{Kind: "user", ID: principal.UserID}, nil
	case auth.PrincipalKindAPIKey:
		return agent.Caller{Kind: "api_key", ID: principal.APIKeyID}, nil
	default:
		return agent.Caller{}, agent.ErrDenied
	}
}

func (s *Server) agentRequestAuthority(r *http.Request, permission auth.Permission) (agent.Caller, uuid.UUID, error) {
	principal := principalFromContext(r.Context())
	scope, _, environment, err := s.requestEnvironmentScopeFromRequest(r, principal)
	if err != nil {
		return agent.Caller{}, uuid.Nil(), err
	}
	if !principal.HasPermission(permission, scope) {
		return agent.Caller{}, uuid.Nil(), agent.ErrDenied
	}
	caller, err := externalAgentCaller(principal)
	return caller, pgvalue.MustUUIDValue(environment), err
}

func (s *Server) createSessionHTTP(w http.ResponseWriter, r *http.Request) {
	var request api.CreateSessionRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, err)
		return
	}
	var slackChannelID *string
	if request.Slack != nil {
		var route struct {
			ChannelID string `json:"channel_id"`
		}
		if err := decodeAgentPayload(request.Slack, &route); err != nil {
			s.writeAgentHTTPError(w, agent.ErrInvalidInput)
			return
		}
		if !agent.ValidSlackChannelID(route.ChannelID) {
			s.writeAgentHTTPError(w, agent.ErrInvalidInput)
			return
		}
		slackChannelID = &route.ChannelID
	}
	caller, environment, err := s.agentRequestAuthority(r, auth.PermissionSessionsCreate)
	if err != nil {
		if slackChannelID != nil && errors.Is(err, agent.ErrDenied) {
			err = agent.ErrSlackChannelUnavailable
		}
		s.writeAgentHTTPError(w, err)
		return
	}
	var computerID uuid.UUID
	if request.ComputerID != "" {
		computerID, err = ids.Parse(request.ComputerID)
		if err != nil {
			s.writeAgentHTTPError(w, agent.ErrInvalidInput)
			return
		}
	}
	var preflight agent.SlackStartPreflight
	if s.slackConfig != nil {
		preflight = s.slackConfig.Client
	}
	result, err := agent.Start(r.Context(), s.tx, s.secretProxy, caller, agent.StartRequest{
		SlackPreflight: preflight, EnvironmentID: environment, Agent: request.Agent, ComputerID: computerID,
		CloseAfterIdle: request.CloseAfterIdle, SessionKey: request.Key, SlackChannelID: slackChannelID, IdempotencyKey: request.IdempotencyKey, Input: request.Input,
	})
	if err != nil {
		s.writeAgentHTTPError(w, err)
		return
	}
	var turn *api.TurnAdmission
	if result.TurnID != uuid.Nil() {
		admission := agentAdmission(result)
		turn = &admission
	}
	writeJSON(w, http.StatusOK, api.CreateSessionResponse{SessionID: result.SessionID.String(), Turn: turn, CloseAfterIdleMs: result.CloseAfterIdleMs})
}

func (s *Server) enqueueSessionHTTP(w http.ResponseWriter, r *http.Request) {
	caller, environment, err := s.agentRequestAuthority(r, auth.PermissionSessionsSend)
	if err != nil {
		s.writeAgentHTTPError(w, err)
		return
	}
	sessionID, err := ids.Parse(chi.URLParam(r, "sessionID"))
	if err != nil {
		s.writeAgentHTTPError(w, agent.ErrInvalidInput)
		return
	}
	var request api.EnqueueSessionRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, err)
		return
	}
	result, err := agent.Enqueue(r.Context(), s.tx, s.secretProxy, caller, agent.EnqueueRequest{
		EnvironmentID: environment, SessionID: sessionID, IdempotencyKey: request.IdempotencyKey, Input: request.Input,
	})
	if err != nil {
		s.writeAgentHTTPError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, agentAdmission(result))
}

func (s *Server) interruptSessionHTTP(w http.ResponseWriter, r *http.Request) {
	caller, environment, err := s.agentRequestAuthority(r, auth.PermissionSessionsInterrupt)
	if err != nil {
		s.writeAgentHTTPError(w, err)
		return
	}
	sessionID, err := ids.Parse(chi.URLParam(r, "sessionID"))
	if err != nil {
		s.writeAgentHTTPError(w, agent.ErrInvalidInput)
		return
	}
	var request api.InterruptSessionRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, err)
		return
	}
	result, err := agent.ControlSession(r.Context(), s.tx, caller, agent.SessionControlRequest{
		EnvironmentID: environment, SessionID: sessionID, Kind: "interrupt", IdempotencyKey: request.IdempotencyKey,
	})
	if err != nil {
		s.writeAgentHTTPError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, api.SessionInterruptReceipt{ID: result.ID.String(), SessionID: result.SessionID.String(), HoldID: result.HoldID.String(), Status: "accepted"})
}

func (s *Server) interruptTurnHTTP(w http.ResponseWriter, r *http.Request) {
	caller, environment, err := s.agentRequestAuthority(r, auth.PermissionSessionsInterrupt)
	if err != nil {
		s.writeAgentHTTPError(w, err)
		return
	}
	sessionID, err := ids.Parse(chi.URLParam(r, "sessionID"))
	if err != nil {
		s.writeAgentHTTPError(w, agent.ErrInvalidInput)
		return
	}
	turnID, err := ids.Parse(chi.URLParam(r, "turnID"))
	if err != nil {
		s.writeAgentHTTPError(w, agent.ErrInvalidInput)
		return
	}
	var request api.InterruptTurnRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, err)
		return
	}
	result, err := agent.ControlSession(r.Context(), s.tx, caller, agent.SessionControlRequest{
		EnvironmentID: environment, SessionID: sessionID, TurnID: turnID, Kind: "turn_interrupt", IdempotencyKey: request.IdempotencyKey,
	})
	if err != nil {
		s.writeAgentHTTPError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, api.TurnInterruptReceipt{ID: result.ID.String(), SessionID: result.SessionID.String(), TurnID: turnID.String(), Status: "accepted"})
}

func (s *Server) closeSessionHTTP(w http.ResponseWriter, r *http.Request) {
	caller, environment, err := s.agentRequestAuthority(r, auth.PermissionSessionsClose)
	if err != nil {
		s.writeAgentHTTPError(w, err)
		return
	}
	sessionID, err := ids.Parse(chi.URLParam(r, "sessionID"))
	if err != nil {
		s.writeAgentHTTPError(w, agent.ErrInvalidInput)
		return
	}
	var request api.CloseSessionRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, err)
		return
	}
	result, err := agent.ControlSession(r.Context(), s.tx, caller, agent.SessionControlRequest{
		EnvironmentID: environment, SessionID: sessionID, Kind: "close", IdempotencyKey: request.IdempotencyKey,
	})
	if err != nil {
		s.writeAgentHTTPError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, api.SessionCloseReceipt{ID: result.ID.String(), SessionID: result.SessionID.String(), Status: "accepted"})
}

func (s *Server) cancelSessionHTTP(w http.ResponseWriter, r *http.Request) {
	caller, environment, err := s.agentRequestAuthority(r, auth.PermissionSessionsCancel)
	if err != nil {
		s.writeAgentHTTPError(w, err)
		return
	}
	sessionID, err := ids.Parse(chi.URLParam(r, "sessionID"))
	if err != nil {
		s.writeAgentHTTPError(w, agent.ErrInvalidInput)
		return
	}
	var request api.CancelSessionRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, err)
		return
	}
	result, err := agent.ControlSession(r.Context(), s.tx, caller, agent.SessionControlRequest{
		EnvironmentID: environment, SessionID: sessionID, Kind: "cancel", IdempotencyKey: request.IdempotencyKey,
	})
	if err != nil {
		s.writeAgentHTTPError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, api.SessionCancelReceipt{ID: result.ID.String(), SessionID: result.SessionID.String(), Status: "accepted"})
}

func (s *Server) resumeSessionHTTP(w http.ResponseWriter, r *http.Request) {
	caller, environment, err := s.agentRequestAuthority(r, auth.PermissionSessionsResume)
	if err != nil {
		s.writeAgentHTTPError(w, err)
		return
	}
	sessionID, err := ids.Parse(chi.URLParam(r, "sessionID"))
	if err != nil {
		s.writeAgentHTTPError(w, agent.ErrInvalidInput)
		return
	}
	var request api.ResumeSessionRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, err)
		return
	}
	holdID, err := ids.Parse(request.HoldID)
	if err != nil || holdID == uuid.Nil() {
		s.writeAgentHTTPError(w, agent.ErrInvalidInput)
		return
	}
	result, err := agent.ControlSession(r.Context(), s.tx, caller, agent.SessionControlRequest{
		EnvironmentID: environment, SessionID: sessionID, Kind: "resume", IdempotencyKey: request.IdempotencyKey, HoldID: holdID,
	})
	if err != nil {
		s.writeAgentHTTPError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, api.SessionResumeReceipt{ID: result.ID.String(), SessionID: result.SessionID.String(), HoldID: result.HoldID.String(), Status: "accepted"})
}

func agentAdmission(receipt agent.Admission) api.TurnAdmission {
	return api.TurnAdmission{SessionID: receipt.SessionID.String(), TurnID: receipt.TurnID.String(), Sequence: receipt.Sequence}
}

func (s *Server) writeAgentHTTPError(w http.ResponseWriter, err error) {
	var invalidScope environmentScopeReferenceError
	var notInterruptible agent.TurnNotInterruptibleError
	switch {
	case errors.As(err, &notInterruptible):
		writeError(w, conflict(notInterruptible))
	case errors.Is(err, agent.ErrMessageClosed):
		writeError(w, conflict(codedError{code: "message_closed", message: "turn is not accepting messages"}))
	case errors.Is(err, content.ErrUnsupported):
		writeErrorStatus(w, http.StatusUnprocessableEntity, codedError{code: "content_kind_unsupported", message: "Content kind is unsupported by the pinned Agent"})
	case errors.Is(err, content.ErrLimit):
		writeErrorStatus(w, http.StatusRequestEntityTooLarge, codedError{code: "content_limit_exceeded", message: err.Error()})
	case errors.Is(err, content.ErrAnswerInvalid):
		writeErrorStatus(w, http.StatusUnprocessableEntity, codedError{code: "answer_invalid", message: "Answer does not match its declared control"})
	case errors.Is(err, agent.ErrAskAlreadyResponded):
		writeError(w, conflict(codedError{code: "ask_already_responded", message: "Ask already has a response"}))
	case errors.Is(err, agent.ErrAskCancelled):
		writeError(w, conflict(codedError{code: "ask_cancelled", message: "Ask is cancelled"}))
	case errors.Is(err, content.ErrInvalid):
		writeError(w, badRequest(codedError{code: "bad_request", message: "Input must be an array of text parts"}))
	case errors.Is(err, agent.ErrInvalidCursor):
		writeError(w, badRequest(codedError{code: "invalid_cursor", message: "Session event cursor exceeds the allocated end"}))
	case errors.Is(err, agent.ErrInvalidInput), errors.As(err, &invalidScope):
		writeError(w, badRequest(errors.New("invalid Agent or Session request")))
	case errors.Is(err, agent.ErrSlackChannelUnavailable):
		writeError(w, notFound(codedError{code: "channel_unavailable", message: err.Error()}))
	case errors.Is(err, agent.ErrSourceConversationPending):
		writeError(w, conflict(codedError{code: "source_conversation_pending", message: err.Error()}))
	case errors.Is(err, agent.ErrSourceConversationUnavailable):
		writeError(w, conflict(codedError{code: "source_conversation_unavailable", message: err.Error()}))
	case errors.Is(err, agent.ErrTargetNotPublished):
		writeError(w, conflict(codedError{code: "target_not_published", message: err.Error()}))
	case errors.Is(err, agent.ErrTargetWorkspaceMismatch):
		writeError(w, conflict(codedError{code: "target_workspace_mismatch", message: err.Error()}))
	case errors.Is(err, agent.ErrStartMessageTooLarge):
		writeError(w, conflict(codedError{code: "start_message_too_large", message: err.Error()}))
	case errors.Is(err, agent.ErrConversationChanged):
		writeError(w, conflict(codedError{code: "conversation_changed", message: err.Error()}))
	case errors.Is(err, agent.ErrSlackDestinationConflict):
		writeError(w, conflict(codedError{code: "slack_destination_conflict", message: "session Slack destination conflicts"}))
	case errors.Is(err, agent.ErrDenied):
		writeError(w, forbidden(errors.New("agent or Session operation is not authorized")))
	case errors.Is(err, agent.ErrConflict):
		writeError(w, conflict(codedError{code: "idempotency_conflict", message: "request conflicts with its recorded identity"}))
	case errors.Is(err, agent.ErrNotReady), errors.Is(err, agent.ErrTerminal):
		// Unavailability also includes a closed Session. The code distinguishes
		// admission state from conflicting identity without promising retryability.
		writeError(w, conflict(codedError{code: "admission_unavailable", message: "Agent or Session is not available for admission"}))
	default:
		s.log.Error("Agent admission failed", "error", err)
		writeError(w, unavailable(errors.New("agent admission is temporarily unavailable")))
	}
}
