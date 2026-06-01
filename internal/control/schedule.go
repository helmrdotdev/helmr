package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/ghapp"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/schedule"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

const (
	scheduleListPageSize = int32(200)
)

func (s *Server) mountScheduleRoutes(r chi.Router) {
	r.Group(func(r chi.Router) {
		r.Use(s.requireActor)
		r.Get("/schedules", s.listSchedules)
		r.Post("/schedules", s.createSchedule)
		r.Get("/schedules/{id}", s.getSchedule)
		r.Post("/schedules/{id}/activate", s.activateSchedule)
		r.Post("/schedules/{id}/deactivate", s.deactivateSchedule)
		r.Delete("/schedules/{id}", s.deleteSchedule)
	})
}

func (s *Server) createSchedule(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("schedule storage is not configured"))
		return
	}
	actor := actorFromContext(r.Context())
	var request api.CreateScheduleRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid schedule request JSON: %w", err))
		return
	}
	row, err := s.createScheduleForActor(r.Context(), actor, request)
	if err != nil {
		if isUniqueViolation(err) {
			writeError(w, http.StatusConflict, errors.New("schedule already exists"))
			return
		}
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusCreated, scheduleResponse(createScheduleView(row)))
}

func (s *Server) createScheduleForActor(ctx context.Context, actor auth.Actor, request api.CreateScheduleRequest) (db.CreateScheduleRow, error) {
	request.TaskID = strings.TrimSpace(request.TaskID)
	if err := api.ValidateTaskID(request.TaskID); err != nil {
		return db.CreateScheduleRow{}, err
	}
	dedupKey := strings.TrimSpace(request.DedupKey)
	if dedupKey == "" {
		dedupKey = schedule.DefaultDedupKey(request.TaskID, request.Cron)
	}
	if err := api.ValidateScheduleID(dedupKey); err != nil {
		return db.CreateScheduleRow{}, err
	}
	scope, projectID, environmentID, err := s.createRunRequestScope(ctx, actor, request.ProjectID, request.EnvironmentID)
	if err != nil {
		return db.CreateScheduleRow{}, err
	}
	if !actor.HasPermission(auth.PermissionRunsCreate, scope) {
		return db.CreateScheduleRow{}, errors.New("permission is required")
	}
	payload := request.Payload
	if len(payload) == 0 {
		payload = json.RawMessage(`{}`)
	}
	if !json.Valid(payload) {
		return db.CreateScheduleRow{}, errors.New("payload must be valid JSON")
	}
	if request.Secrets == nil {
		request.Secrets = api.SecretBindings{}
	}
	if err := secret.ValidateBindings(request.Secrets); err != nil {
		return db.CreateScheduleRow{}, err
	}
	if len(request.Secrets) > 0 && !actor.HasPermission(auth.PermissionSecretsUse, scope) {
		return db.CreateScheduleRow{}, errors.New("permission is required to bind secrets")
	}
	if len(request.Secrets) > 0 && s.secrets == nil {
		return db.CreateScheduleRow{}, errors.New("secret store is not configured")
	}
	if len(request.Secrets) > 0 {
		if err := s.secrets.CheckScoped(ctx, actor.OrgID, ids.MustFromPG(projectID), ids.MustFromPG(environmentID), request.Secrets); err != nil {
			return db.CreateScheduleRow{}, err
		}
	}
	workspace, err := ghapp.NormalizeSource(api.GitHubSource{
		Repository: request.Workspace.Repository,
		Ref:        request.Workspace.Ref,
		SHA:        request.Workspace.SHA,
		Subpath:    request.Workspace.Subpath,
	})
	if err != nil {
		return db.CreateScheduleRow{}, relabelGitHubSourceError(err, "workspace")
	}
	if _, err := s.db.GetActiveProjectGitHubRepositoryByFullName(ctx, db.GetActiveProjectGitHubRepositoryByFullNameParams{
		OrgID:     ids.ToPG(actor.OrgID),
		ProjectID: projectID,
		FullName:  workspace.Repository,
	}); errors.Is(err, pgx.ErrNoRows) {
		return db.CreateScheduleRow{}, relabelGitHubSourceError(ghapp.InvalidSourceError{Err: errors.New("github repository is not enabled for this project workspace")}, "workspace")
	} else if err != nil {
		return db.CreateScheduleRow{}, fmt.Errorf("authorize github workspace repository: %w", err)
	}
	deploymentSelection, err := normalizeRunDeploymentSelection(request.Options.DeploymentID, request.Options.Version)
	if err != nil {
		return db.CreateScheduleRow{}, err
	}
	deploymentTask, err := s.deploymentTaskForRunRequest(ctx, actor.OrgID, projectID, environmentID, request.TaskID, deploymentSelection)
	if errors.Is(err, pgx.ErrNoRows) {
		return db.CreateScheduleRow{}, fmt.Errorf("task %q is not deployed in the selected deployment", request.TaskID)
	}
	if err != nil {
		return db.CreateScheduleRow{}, err
	}
	if _, err := runMaxDurationSeconds(request.Options.MaxDurationSeconds, deploymentTask.MaxDurationSeconds); err != nil {
		return db.CreateScheduleRow{}, err
	}
	scheduling, err := s.resolveRunScheduling(request.Options, deploymentTask)
	if err != nil {
		return db.CreateScheduleRow{}, err
	}
	if _, err := s.validateRunQueueOverride(ctx, actor.OrgID, projectID, environmentID, request.Options, deploymentTask, scheduling); err != nil {
		return db.CreateScheduleRow{}, err
	}
	timezone := api.NormalizeTimezone(request.Timezone)
	next, err := schedule.NextCronTime(request.Cron, timezone, time.Now())
	if err != nil {
		return db.CreateScheduleRow{}, err
	}
	active := true
	if request.Active != nil {
		active = *request.Active
	}
	scheduleID := ids.New()
	instanceID := ids.New()
	var nextScheduledAt pgtype.Timestamptz
	var nextDueAt pgtype.Timestamptz
	if active {
		nextScheduledAt = pgTimeToPG(next)
		nextDueAt = pgTimeToPG(next.Add(schedule.Jitter(instanceID, schedule.DefaultJitter)))
	}
	workspaceJSON, err := json.Marshal(api.ScheduleWorkspace{
		Repository: workspace.Repository,
		Ref:        workspace.Ref,
		SHA:        workspace.SHA,
		Subpath:    workspace.Subpath,
	})
	if err != nil {
		return db.CreateScheduleRow{}, err
	}
	secretBindingsJSON, err := json.Marshal(request.Secrets)
	if err != nil {
		return db.CreateScheduleRow{}, err
	}
	runOptionsJSON, err := json.Marshal(request.Options)
	if err != nil {
		return db.CreateScheduleRow{}, err
	}
	return s.db.CreateSchedule(ctx, db.CreateScheduleParams{
		ScheduleID:      ids.ToPG(scheduleID),
		OrgID:           ids.ToPG(actor.OrgID),
		ProjectID:       projectID,
		TaskID:          request.TaskID,
		DedupKey:        dedupKey,
		CronExpression:  strings.TrimSpace(request.Cron),
		Timezone:        timezone,
		Payload:         payload,
		SecretBindings:  secretBindingsJSON,
		Workspace:       workspaceJSON,
		RunOptions:      runOptionsJSON,
		Active:          active,
		InstanceID:      ids.ToPG(instanceID),
		EnvironmentID:   environmentID,
		NextScheduledAt: nextScheduledAt,
		NextDueAt:       nextDueAt,
	})
}

func (s *Server) listSchedules(w http.ResponseWriter, r *http.Request) {
	actor := actorFromContext(r.Context())
	scope, projectID, environmentID, err := s.requestScopeForPermission(r.Context(), actor, r.URL.Query().Get("project_id"), r.URL.Query().Get("environment_id"), auth.PermissionRunsRead, "schedule read")
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if !actor.HasPermission(auth.PermissionRunsRead, scope) {
		writeError(w, http.StatusForbidden, errors.New("permission is required"))
		return
	}
	rows, err := s.db.ListScheduleSummaries(r.Context(), db.ListScheduleSummariesParams{
		OrgID:         ids.ToPG(actor.OrgID),
		ProjectID:     projectID,
		EnvironmentID: environmentID,
		RowLimit:      scheduleListPageSize,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, errors.New("list schedules"))
		return
	}
	out := make([]api.ScheduleResponse, 0, len(rows))
	for _, row := range rows {
		out = append(out, scheduleResponse(listScheduleView(row)))
	}
	writeJSON(w, http.StatusOK, api.ListSchedulesResponse{Schedules: out})
}

func (s *Server) getSchedule(w http.ResponseWriter, r *http.Request) {
	row, ok := s.loadScheduleForRequest(w, r, auth.PermissionRunsRead)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, scheduleResponse(getScheduleView(row)))
}

func (s *Server) activateSchedule(w http.ResponseWriter, r *http.Request) {
	s.setScheduleState(w, r, true)
}

func (s *Server) deactivateSchedule(w http.ResponseWriter, r *http.Request) {
	s.setScheduleState(w, r, false)
}

func (s *Server) deleteSchedule(w http.ResponseWriter, r *http.Request) {
	row, ok := s.loadScheduleForRequest(w, r, auth.PermissionRunsCreate)
	if !ok {
		return
	}
	affected, err := s.db.DeleteSchedule(r.Context(), db.DeleteScheduleParams{
		OrgID:      row.OrgID,
		ProjectID:  row.ProjectID,
		ScheduleID: row.ScheduleID,
	})
	if err != nil || affected == 0 {
		writeError(w, http.StatusInternalServerError, errors.New("delete schedule"))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) setScheduleState(w http.ResponseWriter, r *http.Request, active bool) {
	row, ok := s.loadScheduleForRequest(w, r, auth.PermissionRunsCreate)
	if !ok {
		return
	}
	var nextScheduledAt pgtype.Timestamptz
	var nextDueAt pgtype.Timestamptz
	if active {
		next, err := schedule.NextCronTime(row.CronExpression, row.Timezone, time.Now())
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		nextScheduledAt = pgTimeToPG(next)
		nextDueAt = pgTimeToPG(next.Add(schedule.Jitter(ids.MustFromPG(row.InstanceID), schedule.DefaultJitter)))
	}
	updated, err := s.db.UpdateScheduleState(r.Context(), db.UpdateScheduleStateParams{
		Active:          active,
		OrgID:           row.OrgID,
		ProjectID:       row.ProjectID,
		ScheduleID:      row.ScheduleID,
		NextScheduledAt: nextScheduledAt,
		NextDueAt:       nextDueAt,
		EnvironmentID:   row.EnvironmentID,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, errors.New("update schedule"))
		return
	}
	if err := s.db.SupersedeScheduleInstanceFires(r.Context(), db.SupersedeScheduleInstanceFiresParams{
		ScheduleInstanceID: updated.InstanceID,
		Generation:         updated.Generation,
	}); err != nil {
		writeError(w, http.StatusInternalServerError, errors.New("update schedule fires"))
		return
	}
	writeJSON(w, http.StatusOK, scheduleResponse(updateScheduleView(updated)))
}

func (s *Server) loadScheduleForRequest(w http.ResponseWriter, r *http.Request, permission auth.Permission) (db.GetScheduleSummaryRow, bool) {
	actor := actorFromContext(r.Context())
	scheduleID, err := ids.Parse(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, errors.New("schedule id must be a UUID"))
		return db.GetScheduleSummaryRow{}, false
	}
	scope, projectID, environmentID, err := s.requestScopeForPermission(r.Context(), actor, r.URL.Query().Get("project_id"), r.URL.Query().Get("environment_id"), permission, "schedule access")
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return db.GetScheduleSummaryRow{}, false
	}
	if !actor.HasPermission(permission, scope) {
		writeError(w, http.StatusForbidden, errors.New("permission is required"))
		return db.GetScheduleSummaryRow{}, false
	}
	row, err := s.db.GetScheduleSummary(r.Context(), db.GetScheduleSummaryParams{
		OrgID:         ids.ToPG(actor.OrgID),
		ProjectID:     projectID,
		EnvironmentID: environmentID,
		ScheduleID:    ids.ToPG(scheduleID),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, errors.New("schedule not found"))
		return db.GetScheduleSummaryRow{}, false
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, errors.New("load schedule"))
		return db.GetScheduleSummaryRow{}, false
	}
	return row, true
}

type scheduleView struct {
	ScheduleID      pgtype.UUID
	ProjectID       pgtype.UUID
	EnvironmentID   pgtype.UUID
	TaskID          string
	DedupKey        string
	CronExpression  string
	Timezone        string
	Payload         []byte
	Workspace       []byte
	ScheduleActive  bool
	InstanceActive  bool
	NextScheduledAt pgtype.Timestamptz
	NextDueAt       pgtype.Timestamptz
	LastScheduledAt pgtype.Timestamptz
	CreatedAt       pgtype.Timestamptz
	UpdatedAt       pgtype.Timestamptz
}

func createScheduleView(row db.CreateScheduleRow) scheduleView {
	return scheduleView{ScheduleID: row.ScheduleID, ProjectID: row.ProjectID, EnvironmentID: row.EnvironmentID, TaskID: row.TaskID, DedupKey: row.DedupKey, CronExpression: row.CronExpression, Timezone: row.Timezone, Payload: row.Payload, Workspace: row.Workspace, ScheduleActive: row.ScheduleActive, InstanceActive: row.InstanceActive, NextScheduledAt: row.NextScheduledAt, NextDueAt: row.NextDueAt, LastScheduledAt: row.LastScheduledAt, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt}
}

func listScheduleView(row db.ListScheduleSummariesRow) scheduleView {
	return scheduleView{ScheduleID: row.ScheduleID, ProjectID: row.ProjectID, EnvironmentID: row.EnvironmentID, TaskID: row.TaskID, DedupKey: row.DedupKey, CronExpression: row.CronExpression, Timezone: row.Timezone, Payload: row.Payload, Workspace: row.Workspace, ScheduleActive: row.ScheduleActive, InstanceActive: row.InstanceActive, NextScheduledAt: row.NextScheduledAt, NextDueAt: row.NextDueAt, LastScheduledAt: row.LastScheduledAt, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt}
}

func getScheduleView(row db.GetScheduleSummaryRow) scheduleView {
	return scheduleView{ScheduleID: row.ScheduleID, ProjectID: row.ProjectID, EnvironmentID: row.EnvironmentID, TaskID: row.TaskID, DedupKey: row.DedupKey, CronExpression: row.CronExpression, Timezone: row.Timezone, Payload: row.Payload, Workspace: row.Workspace, ScheduleActive: row.ScheduleActive, InstanceActive: row.InstanceActive, NextScheduledAt: row.NextScheduledAt, NextDueAt: row.NextDueAt, LastScheduledAt: row.LastScheduledAt, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt}
}

func updateScheduleView(row db.UpdateScheduleStateRow) scheduleView {
	return scheduleView{ScheduleID: row.ScheduleID, ProjectID: row.ProjectID, EnvironmentID: row.EnvironmentID, TaskID: row.TaskID, DedupKey: row.DedupKey, CronExpression: row.CronExpression, Timezone: row.Timezone, Payload: row.Payload, Workspace: row.Workspace, ScheduleActive: row.ScheduleActive, InstanceActive: row.InstanceActive, NextScheduledAt: row.NextScheduledAt, NextDueAt: row.NextDueAt, LastScheduledAt: row.LastScheduledAt, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt}
}

func scheduleResponse(row scheduleView) api.ScheduleResponse {
	response := api.ScheduleResponse{
		ID:            ids.MustFromPG(row.ScheduleID).String(),
		ProjectID:     apiKeyScopeID(row.ProjectID, auth.DefaultProjectID),
		EnvironmentID: apiKeyScopeID(row.EnvironmentID, auth.DefaultEnvironmentID),
		TaskID:        row.TaskID,
		DedupKey:      row.DedupKey,
		Cron:          row.CronExpression,
		Timezone:      row.Timezone,
		Active:        row.ScheduleActive && row.InstanceActive,
		Payload:       append(json.RawMessage(nil), row.Payload...),
		Workspace:     append(json.RawMessage(nil), row.Workspace...),
		CreatedAt:     pgTime(row.CreatedAt),
		UpdatedAt:     pgTime(row.UpdatedAt),
	}
	response.NextScheduledAt = pgTimePtr(row.NextScheduledAt)
	response.NextDueAt = pgTimePtr(row.NextDueAt)
	response.LastScheduledAt = pgTimePtr(row.LastScheduledAt)
	return response
}
