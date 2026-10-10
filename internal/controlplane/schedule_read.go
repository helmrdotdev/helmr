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
	scheduleListDefaultLimit = int32(50)
	scheduleListMaxLimit     = int32(100)
)

type scheduleListCursor struct {
	FilterAgentID string `json:"filter_agent_id,omitempty"`
	ProjectID     string `json:"project_id"`
	EnvironmentID string `json:"environment_id"`
	AgentID       string `json:"agent_id"`
	ScheduleID    string `json:"schedule_id"`
}

func (s *Server) listSchedules(w http.ResponseWriter, r *http.Request) {
	principal := principalFromContext(r.Context())
	scope, projectID, environmentID, err := s.requestEnvironmentScopeFromRequest(r, principal)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	if !principal.HasPermission(auth.PermissionSessionsRead, scope) {
		writeError(w, forbidden(errors.New("permission is required")))
		return
	}
	limit, cursor, exactAgentID, err := parseScheduleListQuery(
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
		id, err := ids.Parse(cursor.ScheduleID)
		if err != nil {
			writeError(w, badRequest(errors.New("schedule cursor is invalid")))
			return
		}
		afterID = pgvalue.UUID(id)
	}
	agentID := pgtype.UUID{}
	if exactAgentID != nil {
		id, _ := ids.Parse(*exactAgentID)
		agentID = pgvalue.UUID(id)
	}
	rows, err := s.db.ListSchedules(r.Context(), db.ListSchedulesParams{
		OrgID:         pgvalue.UUID(principal.OrgID),
		ProjectID:     projectID,
		EnvironmentID: environmentID,
		AgentID:       agentID,
		AfterAgentID:  afterAgent,
		AfterID:       afterID,
		LimitCount:    limit + 1,
	})
	if err != nil {
		s.log.Error("list schedules failed", "error", err)
		writeError(w, errors.New("list schedules"))
		return
	}
	hasMore := len(rows) > int(limit)
	if hasMore {
		rows = rows[:limit]
	}
	response := api.ListSchedulesResponse{
		Schedules: make([]api.ScheduleResponse, 0, len(rows)),
	}
	for _, row := range rows {
		item, err := scheduleResponse(row)
		if err != nil {
			s.log.Error("project schedule failed", "schedule_id", pgvalue.UUIDString(row.ID), "error", err)
			writeError(w, errors.New("list schedules"))
			return
		}
		response.Schedules = append(response.Schedules, item)
	}
	if hasMore {
		last := rows[len(rows)-1]
		response.NextCursor, err = encodeScheduleListCursor(scheduleListCursor{
			ProjectID: scope.ProjectID, EnvironmentID: scope.EnvironmentID, FilterAgentID: r.URL.Query().Get("agent_id"),
			AgentID: pgvalue.UUIDString(last.AgentID), ScheduleID: pgvalue.UUIDString(last.ID),
		})
		if err != nil {
			s.log.Error("encode schedule cursor failed", "error", err)
			writeError(w, errors.New("list schedules"))
			return
		}
	}
	writeJSON(w, http.StatusOK, response)
}

func parseScheduleListQuery(
	r *http.Request,
	projectID string,
	environmentID string,
) (int32, *scheduleListCursor, *string, error) {
	values := r.URL.Query()
	for name, entries := range values {
		if name != "agent_id" && name != "cursor" && name != "limit" {
			return 0, nil, nil, fmt.Errorf("query parameter %q is not supported", name)
		}
		if len(entries) != 1 || strings.TrimSpace(entries[0]) == "" {
			return 0, nil, nil, fmt.Errorf("query parameter %q must appear once", name)
		}
	}
	var filter *string
	if raw := values.Get("agent_id"); raw != "" {
		if ids.Validate(raw) != nil {
			return 0, nil, nil, errors.New("invalid agent ID")
		}
		filter = &raw
	}
	limit := scheduleListDefaultLimit
	if raw := values.Get("limit"); raw != "" {
		parsed, err := strconv.ParseInt(raw, 10, 32)
		if err != nil || parsed < 1 || parsed > int64(scheduleListMaxLimit) {
			return 0, nil, nil, fmt.Errorf(
				"limit must be an integer in [1,%d]",
				scheduleListMaxLimit,
			)
		}
		limit = int32(parsed)
	}
	rawCursor := values.Get("cursor")
	if rawCursor == "" {
		return limit, nil, filter, nil
	}
	cursor, err := decodeScheduleListCursor(rawCursor)
	if err != nil {
		return 0, nil, nil, err
	}
	if cursor.ProjectID != projectID || cursor.EnvironmentID != environmentID {
		return 0, nil, nil, errors.New("schedule cursor belongs to another scope")
	}
	if err := ids.Validate(cursor.AgentID); err != nil {
		return 0, nil, nil, errors.New("schedule cursor is invalid")
	}
	if ids.Validate(cursor.ScheduleID) != nil {
		return 0, nil, nil, errors.New("schedule cursor is invalid")
	}
	if cursor.FilterAgentID != values.Get("agent_id") {
		return 0, nil, nil, errors.New("schedule cursor belongs to another filter")
	}
	return limit, &cursor, filter, nil
}

func encodeScheduleListCursor(cursor scheduleListCursor) (string, error) {
	raw, err := json.Marshal(cursor)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func decodeScheduleListCursor(raw string) (scheduleListCursor, error) {
	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return scheduleListCursor{}, errors.New("schedule cursor is invalid")
	}
	var cursor scheduleListCursor
	if json.Unmarshal(decoded, &cursor) != nil ||
		cursor.ProjectID == "" ||
		cursor.EnvironmentID == "" ||
		cursor.AgentID == "" ||
		cursor.ScheduleID == "" {
		return scheduleListCursor{}, errors.New("schedule cursor is invalid")
	}
	return cursor, nil
}

func (s *Server) getSchedule(w http.ResponseWriter, r *http.Request) {
	principal := principalFromContext(r.Context())
	scope, projectID, environmentID, err := s.requestEnvironmentScopeFromRequest(r, principal)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	if !principal.HasPermission(auth.PermissionSessionsRead, scope) {
		writeError(w, forbidden(errors.New("permission is required")))
		return
	}
	scheduleID, err := ids.Parse(chi.URLParam(r, "scheduleID"))
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	row, err := s.db.GetScheduleByID(r.Context(), db.GetScheduleByIDParams{
		OrgID:         pgvalue.UUID(principal.OrgID),
		ProjectID:     projectID,
		EnvironmentID: environmentID,
		ID:            pgvalue.UUID(scheduleID),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, notFound(errors.New("schedule not found")))
		return
	}
	if err != nil {
		s.log.Error("get schedule failed", "schedule_id", scheduleID.String(), "error", err)
		writeError(w, errors.New("get schedule"))
		return
	}
	response, err := scheduleResponse(row)
	if err != nil {
		s.log.Error("project schedule failed", "schedule_id", scheduleID.String(), "error", err)
		writeError(w, errors.New("get schedule"))
		return
	}
	writeJSON(w, http.StatusOK, response)
}

func scheduleResponse(row db.AgentSchedule) (api.ScheduleResponse, error) {
	if !row.ID.Valid || !row.AgentID.Valid || !row.DeploymentID.Valid || !row.ActiveFrom.Valid || !row.NextFireAt.Valid {
		return api.ScheduleResponse{}, errors.New("schedule identity or interval is invalid")
	}
	result := api.ScheduleResponse{
		ID: pgvalue.UUIDString(row.ID), AgentID: pgvalue.UUIDString(row.AgentID),
		DeploymentID: pgvalue.UUIDString(row.DeploymentID), TriggerKey: row.TriggerKey,
		Cron: api.ScheduleCron{Pattern: row.Cron, Timezone: row.Timezone}, Input: row.Input,
		ActiveFrom: row.ActiveFrom.Time.UTC(), ActiveUntil: pgvalue.TimePtr(row.ActiveUntil),
	}
	if !row.ActiveUntil.Valid || row.NextFireAt.Time.Before(row.ActiveUntil.Time) {
		result.NextFireAt = pgvalue.TimePtr(row.NextFireAt)
	}
	return result, nil
}
