package controlplane

import (
	"encoding/base64"
	"net/http"
	"strconv"
	"strings"
	"uuid"

	"github.com/go-chi/chi/v5"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/session"
)

func (s *Server) askHTTPAuthority(r *http.Request, respond bool) (session.Caller, uuid.UUID, error) {
	principal := principalFromContext(r.Context())
	scope, _, environment, err := s.requestEnvironmentScopeFromRequest(r, principal)
	if err != nil {
		return session.Caller{}, uuid.Nil(), err
	}
	permission := auth.PermissionSessionsRead
	if respond {
		permission = auth.PermissionAsksAnswer
	}
	// The ask owner verifies current human membership, including Viewer. A service
	// key must carry its own explicit scope and never borrows that human rule.
	if principal.Kind == auth.PrincipalKindAPIKey && !principal.HasPermission(permission, scope) {
		return session.Caller{}, uuid.Nil(), session.ErrDenied
	}
	caller, err := externalAgentCaller(principal)
	return caller, pgvalue.MustUUIDValue(environment), err
}
func askHTTPIDs(r *http.Request, exact bool) (sessionID, turn, id uuid.UUID, err error) {
	sessionID, err = ids.Parse(chi.URLParam(r, "sessionID"))
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
func askHTTPView(v session.AskView) map[string]any {
	value := map[string]any{"id": v.ID.String(), "session_id": v.SessionID.String(), "turn_id": v.TurnID.String(), "status": v.Status, "created_at": v.CreatedAt.UTC()}
	if v.PayloadExpiredAt != nil {
		value["payload_expired"] = true
	} else {
		value["question"] = v.Question
		if v.Status == "answered" {
			value["answer"] = v.Answer
		}
	}
	if v.AnsweredAt != nil {
		value["answered_at"] = v.AnsweredAt.UTC()
	}
	if v.CancelledAt != nil {
		value["cancelled_at"] = v.CancelledAt.UTC()
	}
	if v.AnsweredByUserID != nil {
		value["answered_by_user_id"] = v.AnsweredByUserID.String()
	}
	if v.AnsweredByAPIKeyID != nil {
		value["answered_by_api_key_id"] = v.AnsweredByAPIKeyID.String()
	}
	return value
}
func askCursor(env, sessionID, turn uuid.UUID, sequence int64) string {
	return base64.RawURLEncoding.EncodeToString([]byte(env.String() + "/" + sessionID.String() + "/" + turn.String() + "/" + strconv.FormatInt(sequence, 10)))
}
func parseAskCursor(raw string, env, sessionID, turn uuid.UUID) (int64, error) {
	if raw == "" {
		return 0, nil
	}
	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return 0, session.ErrInvalidInput
	}
	prefix := env.String() + "/" + sessionID.String() + "/" + turn.String() + "/"
	if !strings.HasPrefix(string(decoded), prefix) {
		return 0, session.ErrInvalidInput
	}
	sequence, err := strconv.ParseInt(strings.TrimPrefix(string(decoded), prefix), 10, 64)
	if err != nil || sequence < 1 {
		return 0, session.ErrInvalidInput
	}
	return sequence, nil
}
func (s *Server) listAgentAsksHTTP(w http.ResponseWriter, r *http.Request) {
	caller, env, err := s.askHTTPAuthority(r, false)
	if err != nil {
		s.writeAgentHTTPError(w, err)
		return
	}
	sessionID, turn, _, err := askHTTPIDs(r, false)
	if err != nil {
		s.writeAgentHTTPError(w, session.ErrInvalidInput)
		return
	}
	limit, err := agentPageLimit(r)
	if err != nil {
		s.writeAgentHTTPError(w, err)
		return
	}
	after, err := parseAskCursor(r.URL.Query().Get("cursor"), env, sessionID, turn)
	if err != nil {
		s.writeAgentHTTPError(w, err)
		return
	}
	page, err := session.ListAsks(r.Context(), s.tx, caller, session.TurnListRequest{EnvironmentID: env, SessionID: sessionID, After: after, Limit: limit}, turn)
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
		response["next_cursor"] = askCursor(env, sessionID, turn, page.NextSequence)
	}
	writeJSON(w, http.StatusOK, response)
}
func (s *Server) getAgentAskHTTP(w http.ResponseWriter, r *http.Request) {
	caller, env, err := s.askHTTPAuthority(r, false)
	if err != nil {
		s.writeAgentHTTPError(w, err)
		return
	}
	sessionID, turn, id, err := askHTTPIDs(r, true)
	if err != nil {
		s.writeAgentHTTPError(w, session.ErrInvalidInput)
		return
	}
	view, err := session.GetAsk(r.Context(), s.tx, caller, env, sessionID, turn, id)
	if err != nil {
		s.writeAgentHTTPError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, askHTTPView(view))
}
func (s *Server) answerAgentAskHTTP(w http.ResponseWriter, r *http.Request) {
	caller, env, err := s.askHTTPAuthority(r, true)
	if err != nil {
		s.writeAgentHTTPError(w, err)
		return
	}
	sessionID, turn, id, err := askHTTPIDs(r, true)
	if err != nil {
		s.writeAgentHTTPError(w, session.ErrInvalidInput)
		return
	}
	request, err := decodeAskAnswerRequest(r)
	if err != nil {
		writeError(w, err)
		return
	}
	view, err := session.AnswerAsk(r.Context(), s.tx, caller, session.AskAnswerRequest{EnvironmentID: env, SessionID: sessionID, TurnID: turn, AskID: id, AnswerID: request.AnswerID, Answer: request.Answer})
	if err != nil {
		s.writeAgentHTTPError(w, err)
		return
	}
	receipt := askHTTPView(view)
	delete(receipt, "question")
	writeJSON(w, http.StatusOK, receipt)
}
