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
	ProjectID      string `json:"project_id"`
	EnvironmentID  string `json:"environment_id"`
	TaskDeclaredID string `json:"task_declared_id"`
	ScheduleID     string `json:"schedule_id"`
}

func (s *Server) listSchedules(w http.ResponseWriter, r *http.Request) {
	actor := actorFromContext(r.Context())
	scope, projectID, environmentID, err := s.requestEnvironmentScopeFromRequest(r, actor)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	if !actor.HasPermission(auth.PermissionRunsRead, scope) {
		writeError(w, forbidden(errors.New("permission is required")))
		return
	}
	limit, cursor, err := parseScheduleListQuery(
		r,
		scope.ProjectID,
		scope.EnvironmentID,
	)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	var afterTask pgtype.Text
	var afterID pgtype.UUID
	if cursor != nil {
		afterTask = pgvalue.Text(cursor.TaskDeclaredID)
		id, err := ids.Parse(cursor.ScheduleID)
		if err != nil {
			writeError(w, badRequest(errors.New("schedule cursor is invalid")))
			return
		}
		afterID = pgvalue.UUID(id)
	}
	rows, err := s.db.ListSchedules(r.Context(), db.ListSchedulesParams{
		OrgID:               pgvalue.UUID(actor.OrgID),
		ProjectID:           projectID,
		EnvironmentID:       environmentID,
		AfterTaskDeclaredID: afterTask,
		AfterID:             afterID,
		LimitCount:          limit + 1,
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
			ProjectID: scope.ProjectID, EnvironmentID: scope.EnvironmentID,
			TaskDeclaredID: last.TaskDeclaredID, ScheduleID: pgvalue.UUIDString(last.ID),
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
) (int32, *scheduleListCursor, error) {
	values := r.URL.Query()
	for name, entries := range values {
		if name != "cursor" && name != "limit" {
			return 0, nil, fmt.Errorf("query parameter %q is not supported", name)
		}
		if len(entries) != 1 || strings.TrimSpace(entries[0]) == "" {
			return 0, nil, fmt.Errorf("query parameter %q must appear once", name)
		}
	}
	limit := scheduleListDefaultLimit
	if raw := values.Get("limit"); raw != "" {
		parsed, err := strconv.ParseInt(raw, 10, 32)
		if err != nil || parsed < 1 || parsed > int64(scheduleListMaxLimit) {
			return 0, nil, fmt.Errorf(
				"limit must be an integer in [1,%d]",
				scheduleListMaxLimit,
			)
		}
		limit = int32(parsed)
	}
	rawCursor := values.Get("cursor")
	if rawCursor == "" {
		return limit, nil, nil
	}
	cursor, err := decodeScheduleListCursor(rawCursor)
	if err != nil {
		return 0, nil, err
	}
	if cursor.ProjectID != projectID || cursor.EnvironmentID != environmentID {
		return 0, nil, errors.New("schedule cursor belongs to another scope")
	}
	if err := api.ValidateDefinitionID(cursor.TaskDeclaredID); err != nil {
		return 0, nil, errors.New("schedule cursor is invalid")
	}
	if ids.Validate(cursor.ScheduleID) != nil {
		return 0, nil, errors.New("schedule cursor is invalid")
	}
	return limit, &cursor, nil
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
		cursor.TaskDeclaredID == "" ||
		cursor.ScheduleID == "" {
		return scheduleListCursor{}, errors.New("schedule cursor is invalid")
	}
	return cursor, nil
}

func (s *Server) getSchedule(w http.ResponseWriter, r *http.Request) {
	actor := actorFromContext(r.Context())
	scope, projectID, environmentID, err := s.requestEnvironmentScopeFromRequest(r, actor)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	if !actor.HasPermission(auth.PermissionRunsRead, scope) {
		writeError(w, forbidden(errors.New("permission is required")))
		return
	}
	scheduleID, err := ids.Parse(chi.URLParam(r, "scheduleID"))
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	row, err := s.db.GetScheduleByID(r.Context(), db.GetScheduleByIDParams{
		OrgID:         pgvalue.UUID(actor.OrgID),
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

func scheduleResponse(row db.Schedule) (api.ScheduleResponse, error) {
	scheduleID := pgvalue.UUIDString(row.ID)
	if ids.Validate(scheduleID) != nil {
		return api.ScheduleResponse{}, errors.New("schedule identity is invalid")
	}
	status, err := schedulePublicStatus(row.State)
	if err != nil {
		return api.ScheduleResponse{}, err
	}
	response := api.ScheduleResponse{
		ID:         scheduleID,
		TaskID:     row.TaskDeclaredID,
		Cron:       api.ScheduleCron{Pattern: row.CronPattern, Timezone: row.Timezone},
		Status:     status,
		Generation: row.Generation,
		NextFireAt: pgvalue.TimePtr(row.NextFireAt),
		LastFireAt: pgvalue.TimePtr(row.LastFireAt),
	}
	if !row.EffectiveFrom.Valid || !row.CreatedAt.Valid || !row.UpdatedAt.Valid {
		return api.ScheduleResponse{}, errors.New("schedule timestamps are invalid")
	}
	response.EffectiveFrom = row.EffectiveFrom.Time.UTC()
	response.CreatedAt = row.CreatedAt.Time.UTC()
	response.UpdatedAt = row.UpdatedAt.Time.UTC()
	if status == api.ScheduleStatusErrored && len(row.LastFailure) == 0 {
		return api.ScheduleResponse{}, errors.New("errored schedule failure is unavailable")
	}
	if len(row.LastFailure) > 0 {
		var failure api.ScheduleFailure
		if err := json.Unmarshal(row.LastFailure, &failure); err != nil ||
			failure.Code == "" || failure.Message == "" || len(failure.Details) == 0 {
			return api.ScheduleResponse{}, errors.New("schedule failure is invalid")
		}
		var details map[string]json.RawMessage
		if err := json.Unmarshal(failure.Details, &details); err != nil || details == nil {
			return api.ScheduleResponse{}, errors.New("schedule failure details are invalid")
		}
		response.LastFailure = &failure
	}
	return response, nil
}

func schedulePublicStatus(state string) (api.ScheduleStatus, error) {
	switch state {
	case "active":
		return api.ScheduleStatusActive, nil
	case "errored":
		return api.ScheduleStatusErrored, nil
	case "archived":
		return api.ScheduleStatusArchived, nil
	default:
		return "", fmt.Errorf("schedule state %q has no public projection", state)
	}
}
