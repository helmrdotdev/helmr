package controlplane

import (
	"encoding/base64"
	"net/http"
	"strconv"
	"strings"
	"uuid"

	"github.com/go-chi/chi/v5"
	"github.com/helmrdotdev/helmr/internal/agent"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
)

func (s *Server) askHTTPAuthority(r *http.Request, respond bool) (agent.Caller, uuid.UUID, error) {
	principal := principalFromContext(r.Context())
	scope, _, environment, err := s.requestEnvironmentScopeFromRequest(r, principal)
	if err != nil {
		return agent.Caller{}, uuid.Nil(), err
	}
	permission := auth.PermissionSessionsRead
	if respond {
		permission = auth.PermissionAsksRespond
	}
	// The ask owner verifies current human membership, including Viewer. A service
	// key must carry its own explicit scope and never borrows that human rule.
	if principal.Kind == auth.PrincipalKindAPIKey && !principal.HasPermission(permission, scope) {
		return agent.Caller{}, uuid.Nil(), agent.ErrDenied
	}
	caller, err := externalAgentCaller(principal)
	return caller, pgvalue.MustUUIDValue(environment), err
}
func askHTTPIDs(r *http.Request, exact bool) (session, turn, id uuid.UUID, err error) {
	session, err = ids.Parse(chi.URLParam(r, "sessionID"))
	if err != nil {
		return
	}
	turn, err = ids.Parse(chi.URLParam(r, "turnID"))
	if err != nil {
		return
	}
	if exact {
		id, err = ids.Parse(chi.URLParam(r, "askID"))
	}
	return
}
func askHTTPView(v agent.AskView) map[string]any {
	value := map[string]any{"id": v.ID.String(), "session_id": v.SessionID.String(), "turn_id": v.TurnID.String(), "status": v.Status, "created_at": v.CreatedAt.UTC()}
	if v.PayloadExpiredAt != nil {
		value["payload_expired"] = true
	} else {
		value["prompt"] = v.Prompt
		value["answer_control"] = v.AnswerControl
		if v.Status == "responded" {
			value["answer"] = v.Answer
		}
	}
	if v.RespondedAt != nil {
		value["responded_at"] = v.RespondedAt.UTC()
	}
	if v.CancelledAt != nil {
		value["cancelled_at"] = v.CancelledAt.UTC()
	}
	if v.RespondedByUserID != nil {
		value["responded_by_user_id"] = v.RespondedByUserID.String()
	}
	if v.RespondedByAPIKeyID != nil {
		value["responded_by_api_key_id"] = v.RespondedByAPIKeyID.String()
	}
	return value
}
func askCursor(env, session, turn uuid.UUID, sequence int64) string {
	return base64.RawURLEncoding.EncodeToString([]byte(env.String() + "/" + session.String() + "/" + turn.String() + "/" + strconv.FormatInt(sequence, 10)))
}
func parseAskCursor(raw string, env, session, turn uuid.UUID) (int64, error) {
	if raw == "" {
		return 0, nil
	}
	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return 0, agent.ErrInvalidInput
	}
	prefix := env.String() + "/" + session.String() + "/" + turn.String() + "/"
	if !strings.HasPrefix(string(decoded), prefix) {
		return 0, agent.ErrInvalidInput
	}
	sequence, err := strconv.ParseInt(strings.TrimPrefix(string(decoded), prefix), 10, 64)
	if err != nil || sequence < 1 {
		return 0, agent.ErrInvalidInput
	}
	return sequence, nil
}
func (s *Server) listAgentAsksHTTP(w http.ResponseWriter, r *http.Request) {
	caller, env, err := s.askHTTPAuthority(r, false)
	if err != nil {
		s.writeAgentHTTPError(w, err)
		return
	}
	session, turn, _, err := askHTTPIDs(r, false)
	if err != nil {
		s.writeAgentHTTPError(w, agent.ErrInvalidInput)
		return
	}
	limit, err := agentPageLimit(r)
	if err != nil {
		s.writeAgentHTTPError(w, err)
		return
	}
	after, err := parseAskCursor(r.URL.Query().Get("cursor"), env, session, turn)
	if err != nil {
		s.writeAgentHTTPError(w, err)
		return
	}
	page, err := agent.ListAsks(r.Context(), s.tx, caller, agent.TurnListRequest{EnvironmentID: env, SessionID: session, After: after, Limit: limit}, turn)
	if err != nil {
		s.writeAgentHTTPError(w, err)
		return
	}
	asks := make([]map[string]any, 0, len(page.Asks))
	for _, v := range page.Asks {
		asks = append(asks, askHTTPView(v))
	}
	response := map[string]any{"asks": asks}
	if page.NextSequence != 0 {
		response["next_cursor"] = askCursor(env, session, turn, page.NextSequence)
	}
	writeJSON(w, http.StatusOK, response)
}
func (s *Server) getAgentAskHTTP(w http.ResponseWriter, r *http.Request) {
	caller, env, err := s.askHTTPAuthority(r, false)
	if err != nil {
		s.writeAgentHTTPError(w, err)
		return
	}
	session, turn, id, err := askHTTPIDs(r, true)
	if err != nil {
		s.writeAgentHTTPError(w, agent.ErrInvalidInput)
		return
	}
	view, err := agent.GetAsk(r.Context(), s.tx, caller, env, session, turn, id)
	if err != nil {
		s.writeAgentHTTPError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, askHTTPView(view))
}
func (s *Server) respondAgentAskHTTP(w http.ResponseWriter, r *http.Request) {
	caller, env, err := s.askHTTPAuthority(r, true)
	if err != nil {
		s.writeAgentHTTPError(w, err)
		return
	}
	session, turn, id, err := askHTTPIDs(r, true)
	if err != nil {
		s.writeAgentHTTPError(w, agent.ErrInvalidInput)
		return
	}
	request, err := decodeAskAnswerRequest(r)
	if err != nil {
		writeError(w, err)
		return
	}
	view, err := agent.RespondAsk(r.Context(), s.tx, caller, agent.AskAnswerRequest{EnvironmentID: env, SessionID: session, TurnID: turn, AskID: id, ResponseID: request.ResponseID, Answer: request.Answer})
	if err != nil {
		s.writeAgentHTTPError(w, err)
		return
	}
	receipt := askHTTPView(view)
	delete(receipt, "prompt")
	delete(receipt, "answer_control")
	writeJSON(w, http.StatusOK, receipt)
}
