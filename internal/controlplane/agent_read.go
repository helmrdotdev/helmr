package controlplane

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"
	"uuid"

	"github.com/go-chi/chi/v5"
	"github.com/helmrdotdev/helmr/internal/agent"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/ids"
)

func agentPageLimit(r *http.Request) (int, error) {
	if r.URL.Query().Get("limit") == "" {
		return 50, nil
	}
	n, err := strconv.Atoi(r.URL.Query().Get("limit"))
	if err != nil || n < 1 || n > 100 {
		return 0, agent.ErrInvalidInput
	}
	return n, nil
}

func (s *Server) listAgentSessionsHTTP(w http.ResponseWriter, r *http.Request) {
	caller, env, err := s.agentRequestAuthority(r, auth.PermissionSessionsRead)
	if err != nil {
		s.writeAgentHTTPError(w, err)
		return
	}
	limit, err := agentPageLimit(r)
	if err != nil {
		s.writeAgentHTTPError(w, err)
		return
	}
	var before uuid.UUID
	if cursor := r.URL.Query().Get("cursor"); cursor != "" {
		before, err = ids.Parse(cursor)
		if err != nil {
			s.writeAgentHTTPError(w, agent.ErrInvalidInput)
			return
		}
	}
	var agentID uuid.UUID
	if value := r.URL.Query().Get("agent_id"); value != "" {
		agentID, err = ids.Parse(value)
		if err != nil {
			s.writeAgentHTTPError(w, agent.ErrInvalidInput)
			return
		}
	}
	var key *string
	if r.URL.Query().Has("key") {
		value := r.URL.Query().Get("key")
		key = &value
	}
	var parent, requester uuid.UUID
	for name, target := range map[string]*uuid.UUID{"parent_session_id": &parent, "requester_session_id": &requester} {
		if value := r.URL.Query().Get(name); value != "" {
			*target, err = ids.Parse(value)
			if err != nil {
				s.writeAgentHTTPError(w, agent.ErrInvalidInput)
				return
			}
		}
	}
	result, err := agent.ListSessions(r.Context(), s.tx, caller, agent.SessionListRequest{EnvironmentID: env, Before: before, Limit: limit, AgentID: agentID, Key: key, Statuses: r.URL.Query()["status"], ParentSessionID: parent, RequesterSessionID: requester})
	if err != nil {
		s.writeAgentHTTPError(w, err)
		return
	}
	response := api.AgentSessionsPage{Sessions: []api.AgentSession{}}
	for _, v := range result.Sessions {
		response.Sessions = append(response.Sessions, agentSessionResponse(v))
	}
	if result.NextCursor != uuid.Nil() {
		response.NextCursor = result.NextCursor.String()
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) getAgentSessionHTTP(w http.ResponseWriter, r *http.Request) {
	caller, env, err := s.agentRequestAuthority(r, auth.PermissionSessionsRead)
	if err != nil {
		s.writeAgentHTTPError(w, err)
		return
	}
	id, err := ids.Parse(chi.URLParam(r, "sessionID"))
	if err != nil {
		s.writeAgentHTTPError(w, agent.ErrInvalidInput)
		return
	}
	result, err := agent.GetSession(r.Context(), s.tx, caller, env, id)
	if err != nil {
		s.writeAgentHTTPError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, agentSessionResponse(result))
}

func (s *Server) listAgentTurnsHTTP(w http.ResponseWriter, r *http.Request) {
	caller, env, err := s.agentRequestAuthority(r, auth.PermissionSessionsRead)
	if err != nil {
		s.writeAgentHTTPError(w, err)
		return
	}
	session, err := ids.Parse(chi.URLParam(r, "sessionID"))
	if err != nil {
		s.writeAgentHTTPError(w, agent.ErrInvalidInput)
		return
	}
	limit, err := agentPageLimit(r)
	if err != nil {
		s.writeAgentHTTPError(w, err)
		return
	}
	var after int64
	if cursor := r.URL.Query().Get("cursor"); cursor != "" {
		after, err = strconv.ParseInt(cursor, 10, 64)
		if err != nil || after < 0 {
			s.writeAgentHTTPError(w, agent.ErrInvalidInput)
			return
		}
	}
	result, err := agent.ListTurns(r.Context(), s.tx, caller, agent.TurnListRequest{EnvironmentID: env, SessionID: session, After: after, Limit: limit})
	if err != nil {
		s.writeAgentHTTPError(w, err)
		return
	}
	response := api.AgentTurnsPage{Turns: []api.AgentTurn{}}
	for _, v := range result.Turns {
		response.Turns = append(response.Turns, agentTurnResponse(v))
	}
	if result.NextSequence != 0 {
		response.NextCursor = strconv.FormatInt(result.NextSequence, 10)
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) getAgentTurnHTTP(w http.ResponseWriter, r *http.Request) {
	caller, env, err := s.agentRequestAuthority(r, auth.PermissionSessionsRead)
	if err != nil {
		s.writeAgentHTTPError(w, err)
		return
	}
	session, err := ids.Parse(chi.URLParam(r, "sessionID"))
	if err != nil {
		s.writeAgentHTTPError(w, agent.ErrInvalidInput)
		return
	}
	turn, err := ids.Parse(chi.URLParam(r, "turnID"))
	if err != nil {
		s.writeAgentHTTPError(w, agent.ErrInvalidInput)
		return
	}
	result, err := agent.GetTurn(r.Context(), s.tx, caller, env, session, turn)
	if err != nil {
		s.writeAgentHTTPError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, agentTurnResponse(result))
}

func agentOptionalID(id *uuid.UUID) *string {
	if id == nil {
		return nil
	}
	value := id.String()
	return &value
}
func agentSessionResponse(v agent.SessionView) api.AgentSession {
	result := api.AgentSession{ID: v.ID.String(), AgentID: v.AgentID.String(), DeploymentID: v.DeploymentID.String(), ComputerID: v.ComputerID.String(), RootSessionID: v.RootSessionID.String(), ParentSessionID: agentOptionalID(v.ParentSessionID), SlackChannelID: v.SlackChannelID, Key: v.Key, Status: v.Status, CreatedAt: v.CreatedAt.UTC(), Holds: []api.AgentSessionHold{}}
	result.RequesterSessionID = agentOptionalID(v.RequesterSessionID)
	if v.InitialTurnID != nil && v.InitialTurnStatus != nil {
		result.InitialTurn = &api.AgentSessionInitialTurn{ID: v.InitialTurnID.String(), Status: *v.InitialTurnStatus}
	}
	for _, h := range v.Holds {
		result.Holds = append(result.Holds, api.AgentSessionHold{ID: h.ID.String(), SessionID: h.SessionID.String(), Scope: h.Scope, Reason: h.Reason, CreatedAt: h.CreatedAt.UTC()})
	}
	return result
}
func agentTurnResponse(v agent.TurnView) api.AgentTurn {
	var failure *api.AgentTurnError
	if v.Error != nil {
		failure = &api.AgentTurnError{Code: v.Error.Code, Message: &v.Error.Message}
	}
	if failure != nil && v.PayloadExpiredAt != nil {
		failure.Message = nil
	}
	return api.AgentTurn{PayloadExpiredAt: agentUTCTime(v.PayloadExpiredAt), Error: failure, ID: v.ID.String(), SessionID: v.SessionID.String(), Sequence: v.Sequence, Status: v.Status, Input: v.Input, Result: v.Result, Response: v.Response, StartedAt: agentUTCTime(v.StartedAt), TerminalAt: agentUTCTime(v.TerminalAt), CompletionSaveID: agentOptionalID(v.CompletionSaveID)}
}

func agentUTCTime(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	utc := value.UTC()
	return &utc
}

func (s *Server) listAgentEventsHTTP(w http.ResponseWriter, r *http.Request) {
	caller, env, err := s.agentRequestAuthority(r, auth.PermissionSessionsRead)
	if err != nil {
		s.writeAgentHTTPError(w, err)
		return
	}
	session, err := ids.Parse(chi.URLParam(r, "sessionID"))
	if err != nil {
		s.writeAgentHTTPError(w, agent.ErrInvalidInput)
		return
	}
	after := int64(0)
	if value := r.URL.Query().Get("after"); value != "" {
		after, err = strconv.ParseInt(value, 10, 64)
		if err != nil {
			s.writeAgentHTTPError(w, agent.ErrInvalidInput)
			return
		}
	}
	limit := 100
	if value := r.URL.Query().Get("limit"); value != "" {
		limit, err = strconv.Atoi(value)
		if err != nil {
			s.writeAgentHTTPError(w, agent.ErrInvalidInput)
			return
		}
	}
	result, err := agent.ListEvents(r.Context(), s.tx, caller, agent.TurnListRequest{EnvironmentID: env, SessionID: session, After: after, Limit: limit})
	var expired *agent.CursorExpired
	if errors.As(err, &expired) {
		writeJSON(w, http.StatusGone, api.HTTPErrorResponse{Error: api.HTTPError{Code: "cursor_expired", Message: expired.Error(), Details: map[string]json.RawMessage{"retained_after": json.RawMessage(strconv.FormatInt(expired.RetainedAfter, 10))}}})
		return
	}
	if err != nil {
		s.writeAgentHTTPError(w, err)
		return
	}
	response := api.AgentSessionEventPage{Records: []api.AgentSessionEvent{}, NextAfter: result.NextAfter, HasMore: result.HasMore, RetainedAfter: result.RetainedAfter}
	for _, e := range result.Records {
		response.Records = append(response.Records, api.AgentSessionEvent{SessionID: e.SessionID.String(), TurnID: agentOptionalID(e.TurnID), Sequence: e.Sequence, Kind: e.Kind, Data: e.Data, CreatedAt: e.CreatedAt.UTC()})
	}
	writeJSON(w, http.StatusOK, response)
}
