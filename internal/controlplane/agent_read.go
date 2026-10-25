package controlplane

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"time"
	"uuid"

	"github.com/go-chi/chi/v5"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/session"
)

func agentPageLimit(r *http.Request) (int, error) {
	if r.URL.Query().Get("limit") == "" {
		return 50, nil
	}
	n, err := strconv.Atoi(r.URL.Query().Get("limit"))
	if err != nil || n < 1 || n > 100 {
		return 0, session.ErrInvalidInput
	}
	return n, nil
}

func (s *Server) listAgentSessionsHTTP(w http.ResponseWriter, r *http.Request) {
	caller, env, err := s.agentRequestAuthority(r, auth.PermissionSessionsRead)
	if err != nil {
		s.writeAgentHTTPError(w, r, err)
		return
	}
	limit, err := agentPageLimit(r)
	if err != nil {
		s.writeAgentHTTPError(w, r, err)
		return
	}
	before, filter, err := parseSessionListCursor(r, env)
	if err != nil {
		s.writeAgentHTTPError(w, r, session.ErrInvalidInput)
		return
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
				s.writeAgentHTTPError(w, r, session.ErrInvalidInput)
				return
			}
		}
	}
	result, err := session.ListSessions(r.Context(), s.auditPolicy, s.tx, caller, session.SessionListRequest{EnvironmentID: env, Before: before, Limit: limit, Agent: r.URL.Query().Get("agent"), Key: key, Statuses: r.URL.Query()["status"], ParentSessionID: parent, RequesterSessionID: requester})
	if err != nil {
		s.writeAgentHTTPError(w, r, err)
		return
	}
	response := api.AgentSessionsPage{Sessions: []api.AgentSession{}}
	for _, v := range result.Sessions {
		response.Sessions = append(response.Sessions, agentSessionResponse(v))
	}
	if result.NextCursor != uuid.Nil() {
		raw, err := json.Marshal(sessionListCursor{EnvironmentID: env.String(), Filter: filter, Before: result.NextCursor.String()})
		if err != nil {
			s.writeAgentHTTPError(w, r, err)
			return
		}
		response.NextCursor = base64.RawURLEncoding.EncodeToString(raw)
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) getAgentSessionHTTP(w http.ResponseWriter, r *http.Request) {
	caller, env, err := s.agentRequestAuthority(r, auth.PermissionSessionsRead)
	if err != nil {
		s.writeAgentHTTPError(w, r, err)
		return
	}
	id, err := ids.Parse(chi.URLParam(r, "sessionID"))
	if err != nil {
		s.writeAgentHTTPError(w, r, session.ErrInvalidInput)
		return
	}
	result, err := session.GetSession(r.Context(), s.auditPolicy, s.tx, caller, env, id)
	if err != nil {
		s.writeAgentHTTPError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, agentSessionResponse(result))
}

func (s *Server) listAgentTurnsHTTP(w http.ResponseWriter, r *http.Request) {
	caller, env, err := s.agentRequestAuthority(r, auth.PermissionSessionsRead)
	if err != nil {
		s.writeAgentHTTPError(w, r, err)
		return
	}
	sessionID, err := ids.Parse(chi.URLParam(r, "sessionID"))
	if err != nil {
		s.writeAgentHTTPError(w, r, session.ErrInvalidInput)
		return
	}
	limit, err := agentPageLimit(r)
	if err != nil {
		s.writeAgentHTTPError(w, r, err)
		return
	}
	var after int64
	if cursor := r.URL.Query().Get("cursor"); cursor != "" {
		after, err = strconv.ParseInt(cursor, 10, 64)
		if err != nil || after < 0 {
			s.writeAgentHTTPError(w, r, session.ErrInvalidInput)
			return
		}
	}
	result, err := session.ListTurns(r.Context(), s.auditPolicy, s.tx, caller, session.TurnListRequest{EnvironmentID: env, SessionID: sessionID, After: after, Limit: limit})
	if err != nil {
		s.writeAgentHTTPError(w, r, err)
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
		s.writeAgentHTTPError(w, r, err)
		return
	}
	sessionID, err := ids.Parse(chi.URLParam(r, "sessionID"))
	if err != nil {
		s.writeAgentHTTPError(w, r, session.ErrInvalidInput)
		return
	}
	turn, err := ids.Parse(chi.URLParam(r, "turnID"))
	if err != nil {
		s.writeAgentHTTPError(w, r, session.ErrInvalidInput)
		return
	}
	result, err := session.GetTurn(r.Context(), s.auditPolicy, s.tx, caller, env, sessionID, turn)
	if err != nil {
		s.writeAgentHTTPError(w, r, err)
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
func agentSessionResponse(v session.SessionView) api.AgentSession {
	result := api.AgentSession{Permissions: v.Permissions, Suspension: v.Suspension, AgentDefinitionID: v.AgentDefinitionID, TemplateDefinitionID: v.TemplateDefinitionID, CloseAfterIdleMs: v.CloseAfterIdleMs, IdleSince: agentUTCTime(v.IdleSince), ID: v.ID.String(), DeploymentID: v.DeploymentID.String(), ComputerID: agentOptionalID(v.ComputerID), RootSessionID: v.RootSessionID.String(), ParentSessionID: agentOptionalID(v.ParentSessionID), SlackChannelID: v.SlackChannelID, Key: v.Key, Status: v.Status, CreatedAt: v.CreatedAt.UTC(), Holds: []api.AgentSessionHold{}}
	result.Activity = agentActivityResponse(v.Activity)
	result.RequesterSessionID = agentOptionalID(v.RequesterSessionID)
	if v.InitialTurnID != nil && v.InitialTurnStatus != nil {
		result.InitialTurn = &api.AgentSessionInitialTurn{ID: v.InitialTurnID.String(), Status: *v.InitialTurnStatus}
	}
	for _, h := range v.Holds {
		result.Holds = append(result.Holds, api.AgentSessionHold{ID: h.ID.String(), SessionID: h.SessionID.String(), Scope: h.Scope, Kind: h.Kind, RecoveryGap: agentRecoveryGapResponse(h.RecoveryGap), Reason: h.Reason, CreatedAt: h.CreatedAt.UTC()})
	}
	return result
}
func agentTurnResponse(v session.TurnView) api.AgentTurn {
	var failure *api.AgentTurnError
	if v.Error != nil {
		failure = &api.AgentTurnError{Code: v.Error.Code, Message: &v.Error.Message}
	}
	if failure != nil && v.PayloadExpiredAt != nil {
		failure.Message = nil
	}
	return api.AgentTurn{Stop: agentStopResponse(v.Stop), PayloadExpiredAt: agentUTCTime(v.PayloadExpiredAt), Error: failure, ID: v.ID.String(), SessionID: v.SessionID.String(), Sequence: v.Sequence, Status: v.Status, Input: v.Input, Result: v.Result, Response: v.Response, StartedAt: agentUTCTime(v.StartedAt), TerminalAt: agentUTCTime(v.TerminalAt)}
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
		s.writeAgentHTTPError(w, r, err)
		return
	}
	sessionID, err := ids.Parse(chi.URLParam(r, "sessionID"))
	if err != nil {
		s.writeAgentHTTPError(w, r, session.ErrInvalidInput)
		return
	}
	after := int64(0)
	if value := r.URL.Query().Get("after"); value != "" {
		after, err = strconv.ParseInt(value, 10, 64)
		if err != nil {
			s.writeAgentHTTPError(w, r, session.ErrInvalidInput)
			return
		}
	}
	limit := 100
	if value := r.URL.Query().Get("limit"); value != "" {
		limit, err = strconv.Atoi(value)
		if err != nil {
			s.writeAgentHTTPError(w, r, session.ErrInvalidInput)
			return
		}
	}
	result, err := session.ListEvents(r.Context(), s.auditPolicy, s.tx, caller, session.TurnListRequest{EnvironmentID: env, SessionID: sessionID, After: after, Limit: limit})
	var expired *session.CursorExpired
	if errors.As(err, &expired) {
		writeJSON(w, http.StatusGone, api.HTTPErrorResponse{Error: api.HTTPError{Code: "cursor_expired", Message: expired.Error(), Details: map[string]json.RawMessage{"retained_after": json.RawMessage(strconv.FormatInt(expired.RetainedAfter, 10))}}})
		return
	}
	if err != nil {
		s.writeAgentHTTPError(w, r, err)
		return
	}
	response := api.AgentSessionEventPage{Records: []api.AgentSessionEvent{}, NextAfter: result.NextAfter, HasMore: result.HasMore, RetainedAfter: result.RetainedAfter}
	for _, e := range result.Records {
		response.Records = append(response.Records, api.AgentSessionEvent{SessionID: e.SessionID.String(), TurnID: agentOptionalID(e.TurnID), Sequence: e.Sequence, Kind: e.Kind, Data: e.Data, CreatedAt: e.CreatedAt.UTC()})
	}
	writeJSON(w, http.StatusOK, response)
}

func agentStopResponse(v *session.ProcessStopView) *api.AgentProcessStop {
	if v == nil {
		return nil
	}
	return &api.AgentProcessStop{ProcessEpoch: v.ProcessEpoch, TurnID: v.TurnID, State: v.State, Deadline: v.Deadline.UTC()}
}
func agentActivityResponse(v session.SessionActivityView) api.AgentSessionActivity {
	result := api.AgentSessionActivity{}
	if v.Process != nil {
		p := v.Process
		result.Process = &api.AgentProcessActivity{Epoch: p.Epoch, Status: p.Status, Applied: p.Applied, ReconstructionRequired: p.ReconstructionRequired, Stop: agentStopResponse(p.Stop)}
	}
	return result
}

// Cursor scope includes selectors so a continuation cannot silently change its dataset.
type sessionListCursor struct {
	EnvironmentID string `json:"environment_id"`
	Filter        string `json:"filter"`
	Before        string `json:"before"`
}

func parseSessionListCursor(r *http.Request, env uuid.UUID) (uuid.UUID, string, error) {
	values := r.URL.Query()
	filters := url.Values{}
	for name, entries := range values {
		switch name {
		case "cursor", "limit", "agent", "key", "status", "parent_session_id", "requester_session_id":
		default:
			return uuid.Nil(), "", session.ErrInvalidInput
		}
		if len(entries) != 1 && name != "status" {
			return uuid.Nil(), "", session.ErrInvalidInput
		}
		for _, value := range entries {
			if value == "" {
				return uuid.Nil(), "", session.ErrInvalidInput
			}
		}
		if name != "cursor" && name != "limit" {
			filters[name] = append([]string(nil), entries...)
			sort.Strings(filters[name])
		}
	}
	filter := filters.Encode()
	if !values.Has("cursor") {
		return uuid.Nil(), filter, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(values.Get("cursor"))
	var cursor sessionListCursor
	if err != nil || json.Unmarshal(raw, &cursor) != nil || cursor.EnvironmentID != env.String() || cursor.Filter != filter {
		return uuid.Nil(), "", session.ErrInvalidInput
	}
	before, err := ids.Parse(cursor.Before)
	return before, filter, err
}

func agentRecoveryGapResponse(v *session.RecoveryGap) *api.AgentRecoveryGap {
	if v == nil {
		return nil
	}
	ids := make([]string, len(v.AffectedTurnIDs))
	for i, id := range v.AffectedTurnIDs {
		ids[i] = id.String()
	}
	return &api.AgentRecoveryGap{HoldID: v.HoldID.String(), ComputerID: v.ComputerID.String(), LeaseEpoch: v.LeaseEpoch, RecoverySaveID: agentOptionalID(v.RecoverySaveID), RestoredAt: agentUTCTime(v.RestoredAt), AffectedTurnIDs: ids}
}
