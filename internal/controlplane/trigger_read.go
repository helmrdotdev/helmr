package controlplane

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

const (
	triggerListDefaultLimit = int32(50)
	triggerListMaxLimit     = int32(100)
)

type triggerListCursor struct {
	FilterAgent   string `json:"filter_agent,omitempty"`
	ProjectID     string `json:"project_id"`
	EnvironmentID string `json:"environment_id"`
	AgentID       string `json:"agent_id"`
	TriggerID     string `json:"trigger_id"`
}

func (s *Server) listTriggers(w http.ResponseWriter, r *http.Request) {
	principal := principalFromContext(r.Context())
	scope, projectID, environmentID, err := s.requestEnvironmentScopeFromRequest(r, principal)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	allAgents, agentIDs := principal.AgentPermissionScope(auth.PermissionSessionsRead, scope)
	authorizedAgents := make([]pgtype.UUID, 0, len(agentIDs))
	for _, id := range agentIDs {
		authorizedAgents = append(authorizedAgents, pgvalue.UUID(id))
	}
	if !allAgents && len(agentIDs) == 0 {
		writeError(w, forbidden(errors.New("permission is required")))
		return
	}
	limit, cursor, err := parseTriggerListQuery(
		r,
		scope.ProjectID,
		scope.EnvironmentID,
	)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	var afterAgent pgtype.UUID
	var afterID pgtype.UUID
	if cursor != nil {
		agent, _ := ids.Parse(cursor.AgentID)
		afterAgent = pgvalue.UUID(agent)
		id, err := ids.Parse(cursor.TriggerID)
		if err != nil {
			writeError(w, badRequest(errors.New("trigger cursor is invalid")))
			return
		}
		afterID = pgvalue.UUID(id)
	}
	rows, err := s.db.ListTriggers(r.Context(), db.ListTriggersParams{AllAgents: allAgents, AuthorizedAgents: authorizedAgents,
		OrgID:         pgvalue.UUID(principal.OrgID),
		ProjectID:     projectID,
		EnvironmentID: environmentID,
		Agent:         pgtype.Text{String: r.URL.Query().Get("agent"), Valid: r.URL.Query().Has("agent")},
		AfterAgentID:  afterAgent,
		AfterID:       afterID,
		LimitCount:    limit + 1,
	})
	if err != nil {
		s.log.Error("list triggers failed", "error", err)
		writeError(w, errors.New("list triggers"))
		return
	}
	hasMore := len(rows) > int(limit)
	if hasMore {
		rows = rows[:limit]
	}
	response := api.ListTriggersResponse{
		Triggers: make([]api.TriggerResponse, 0, len(rows)),
	}
	for _, row := range rows {
		item, err := triggerResponse(row)
		if err != nil {
			s.log.Error("project trigger failed", "trigger_id", pgvalue.UUIDString(row.ID), "error", err)
			writeError(w, errors.New("list triggers"))
			return
		}
		response.Triggers = append(response.Triggers, item)
	}
	if hasMore {
		last := rows[len(rows)-1]
		response.NextCursor, err = encodeTriggerListCursor(triggerListCursor{
			ProjectID: scope.ProjectID, EnvironmentID: scope.EnvironmentID, FilterAgent: r.URL.Query().Get("agent"),
			AgentID: pgvalue.UUIDString(last.AgentID), TriggerID: pgvalue.UUIDString(last.ID),
		})
		if err != nil {
			s.log.Error("encode trigger cursor failed", "error", err)
			writeError(w, errors.New("list triggers"))
			return
		}
	}
	writeJSON(w, http.StatusOK, response)
}

func parseTriggerListQuery(
	r *http.Request,
	projectID string,
	environmentID string,
) (int32, *triggerListCursor, error) {
	values := r.URL.Query()
	for name, entries := range values {
		if name != "agent" && name != "cursor" && name != "limit" {
			return 0, nil, fmt.Errorf("query parameter %q is not supported", name)
		}
		if len(entries) != 1 || strings.TrimSpace(entries[0]) == "" {
			return 0, nil, fmt.Errorf("query parameter %q must appear once", name)
		}
	}
	if values.Has("agent") {
		if err := api.ValidateDefinitionID(values.Get("agent")); err != nil {
			return 0, nil, err
		}
	}
	limit := triggerListDefaultLimit
	if raw := values.Get("limit"); raw != "" {
		parsed, err := strconv.ParseInt(raw, 10, 32)
		if err != nil || parsed < 1 || parsed > int64(triggerListMaxLimit) {
			return 0, nil, fmt.Errorf(
				"limit must be an integer in [1,%d]",
				triggerListMaxLimit,
			)
		}
		limit = int32(parsed)
	}
	rawCursor := values.Get("cursor")
	if rawCursor == "" {
		return limit, nil, nil
	}
	cursor, err := decodeTriggerListCursor(rawCursor)
	if err != nil {
		return 0, nil, err
	}
	if cursor.ProjectID != projectID || cursor.EnvironmentID != environmentID {
		return 0, nil, errors.New("trigger cursor belongs to another scope")
	}
	if err := ids.Validate(cursor.AgentID); err != nil {
		return 0, nil, errors.New("trigger cursor is invalid")
	}
	if ids.Validate(cursor.TriggerID) != nil {
		return 0, nil, errors.New("trigger cursor is invalid")
	}
	if cursor.FilterAgent != values.Get("agent") {
		return 0, nil, errors.New("trigger cursor belongs to another filter")
	}
	return limit, &cursor, nil
}

func encodeTriggerListCursor(cursor triggerListCursor) (string, error) {
	raw, err := json.Marshal(cursor)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func decodeTriggerListCursor(raw string) (triggerListCursor, error) {
	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return triggerListCursor{}, errors.New("trigger cursor is invalid")
	}
	var cursor triggerListCursor
	if json.Unmarshal(decoded, &cursor) != nil ||
		cursor.ProjectID == "" ||
		cursor.EnvironmentID == "" ||
		cursor.AgentID == "" ||
		cursor.TriggerID == "" {
		return triggerListCursor{}, errors.New("trigger cursor is invalid")
	}
	return cursor, nil
}

func (s *Server) getTrigger(w http.ResponseWriter, r *http.Request) {
	principal := principalFromContext(r.Context())
	scope, projectID, environmentID, err := s.requestEnvironmentScopeFromRequest(r, principal)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	allAgents, agentIDs := principal.AgentPermissionScope(auth.PermissionSessionsRead, scope)
	authorizedAgents := make([]pgtype.UUID, 0, len(agentIDs))
	for _, id := range agentIDs {
		authorizedAgents = append(authorizedAgents, pgvalue.UUID(id))
	}
	if !allAgents && len(agentIDs) == 0 {
		writeError(w, forbidden(errors.New("permission is required")))
		return
	}
	triggerID, err := ids.Parse(chi.URLParam(r, "triggerID"))
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	row, err := s.db.GetTriggerByID(r.Context(), db.GetTriggerByIDParams{AllAgents: allAgents, AuthorizedAgents: authorizedAgents,
		OrgID:         pgvalue.UUID(principal.OrgID),
		ProjectID:     projectID,
		EnvironmentID: environmentID,
		ID:            pgvalue.UUID(triggerID),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, notFound(errors.New("trigger not found")))
		return
	}
	if err != nil {
		s.log.Error("get trigger failed", "trigger_id", triggerID.String(), "error", err)
		writeError(w, errors.New("get trigger"))
		return
	}
	response, err := triggerResponse(db.ListTriggersRow(row))
	if err != nil {
		s.log.Error("project trigger failed", "trigger_id", triggerID.String(), "error", err)
		writeError(w, errors.New("get trigger"))
		return
	}
	writeJSON(w, http.StatusOK, response)
}

func triggerResponse(row db.ListTriggersRow) (api.TriggerResponse, error) {
	if !row.ID.Valid || !row.AgentID.Valid || !row.DeploymentID.Valid || !row.ActiveFrom.Valid || !row.NextFireAt.Valid {
		return api.TriggerResponse{}, errors.New("trigger identity or interval is invalid")
	}
	result := api.TriggerResponse{
		Agent:        row.Agent,
		ID:           pgvalue.UUIDString(row.ID),
		DeploymentID: pgvalue.UUIDString(row.DeploymentID), TriggerKey: row.TriggerKey,
		Cron: api.TriggerCron{Pattern: row.Cron, Timezone: row.Timezone}, Input: row.Input,
		ActiveFrom: row.ActiveFrom.Time.UTC(), ActiveUntil: pgvalue.TimePtr(row.ActiveUntil),
	}
	if !row.ActiveUntil.Valid || row.NextFireAt.Time.Before(row.ActiveUntil.Time) {
		result.NextFireAt = pgvalue.TimePtr(row.NextFireAt)
	}
	return result, nil
}
