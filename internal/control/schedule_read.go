package control

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

func (s *Server) listSchedules(w http.ResponseWriter, r *http.Request) {
	actor := actorFromContext(r.Context())
	scope, projectID, environmentID, err := s.requestEnvironmentScopeFromRequest(
		r,
		actor,
		"",
		"",
	)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	if !actor.HasPermission(auth.PermissionRunsRead, scope) {
		writeError(w, forbidden(errors.New("permission is required")))
		return
	}
	task := strings.TrimSpace(r.URL.Query().Get("task"))
	if raw, present := r.URL.Query()["task"]; present &&
		(len(raw) != 1 || task == "" || task != raw[0]) {
		writeError(w, badRequest(errors.New("task must be one non-empty exact declared ID")))
		return
	}
	rows, err := s.db.ListSchedules(r.Context(), db.ListSchedulesParams{
		OrgID:          pgvalue.UUID(actor.OrgID),
		ProjectID:      projectID,
		EnvironmentID:  environmentID,
		TaskDeclaredID: pgvalue.Text(task),
	})
	if err != nil {
		s.log.Error("list schedules failed", "error", err)
		writeError(w, errors.New("list schedules"))
		return
	}
	response := api.ListSchedulesResponse{
		Schedules: make([]api.ScheduleResponse, 0, len(rows)),
	}
	for _, row := range rows {
		item, err := scheduleResponse(row.Schedule, row.WorkspaceRefPublicID)
		if err != nil {
			s.log.Error("project schedule failed", "schedule_id", row.Schedule.PublicID, "error", err)
			writeError(w, errors.New("list schedules"))
			return
		}
		response.Schedules = append(response.Schedules, item)
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) getSchedule(w http.ResponseWriter, r *http.Request) {
	actor := actorFromContext(r.Context())
	scope, projectID, environmentID, err := s.requestEnvironmentScopeFromRequest(
		r,
		actor,
		"",
		"",
	)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	if !actor.HasPermission(auth.PermissionRunsRead, scope) {
		writeError(w, forbidden(errors.New("permission is required")))
		return
	}
	scheduleID := strings.TrimSpace(chi.URLParam(r, "scheduleID"))
	if scheduleID == "" {
		writeError(w, badRequest(errors.New("schedule ID is required")))
		return
	}
	row, err := s.db.GetScheduleByPublicID(r.Context(), db.GetScheduleByPublicIDParams{
		OrgID:         pgvalue.UUID(actor.OrgID),
		ProjectID:     projectID,
		EnvironmentID: environmentID,
		PublicID:      scheduleID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, notFound(errors.New("schedule not found")))
		return
	}
	if err != nil {
		s.log.Error("get schedule failed", "schedule_id", scheduleID, "error", err)
		writeError(w, errors.New("get schedule"))
		return
	}
	response, err := scheduleResponse(row.Schedule, row.WorkspaceRefPublicID)
	if err != nil {
		s.log.Error("project schedule failed", "schedule_id", row.Schedule.PublicID, "error", err)
		writeError(w, errors.New("get schedule"))
		return
	}
	writeJSON(w, http.StatusOK, response)
}

func scheduleResponse(row db.Schedule, workspaceRefPublicID pgtype.Text) (api.ScheduleResponse, error) {
	response := api.ScheduleResponse{
		ID:         row.PublicID,
		Task:       row.TaskDeclaredID,
		Cron:       api.ScheduleCron{Pattern: row.CronPattern, Timezone: row.Timezone},
		Status:     strings.ReplaceAll(row.State, "_", "-"),
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
	switch {
	case row.WorkspaceRefID.Valid:
		if !workspaceRefPublicID.Valid {
			return api.ScheduleResponse{}, errors.New("schedule ID-addressed Workspace is absent")
		}
		response.Workspace.ID = workspaceRefPublicID.String
	case row.WorkspaceRefKey.Valid:
		response.Workspace.Key = row.WorkspaceRefKey.String
	default:
		return api.ScheduleResponse{}, errors.New("schedule Workspace address is absent")
	}
	if len(row.LastError) != 0 {
		var lastError api.ScheduleError
		if err := json.Unmarshal(row.LastError, &lastError); err != nil {
			return api.ScheduleResponse{}, fmt.Errorf("decode Schedule error: %w", err)
		}
		if lastError.Code == "" || lastError.Message == "" {
			return api.ScheduleResponse{}, errors.New("schedule error is incomplete")
		}
		response.LastError = &lastError
	}
	return response, nil
}
