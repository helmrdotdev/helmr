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
		r.Put("/schedules/{id}", s.updateSchedule)
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
		if errors.Is(err, errPermissionRequired) {
			writeError(w, http.StatusForbidden, err)
			return
		}
		s.writeCreateScheduleError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, scheduleResponse(createScheduleView(row)))
}

func (s *Server) updateSchedule(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("schedule storage is not configured"))
		return
	}
	actor := actorFromContext(r.Context())
	row, ok := s.loadScheduleForRequest(w, r, auth.PermissionRunsCreate)
	if !ok {
		return
	}
	if row.ScheduleType == db.TaskScheduleTypeDeclarative {
		writeError(w, http.StatusBadRequest, errors.New("declarative schedules are managed by task definitions"))
		return
	}
	var request api.CreateScheduleRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid schedule request JSON: %w", err))
		return
	}
	updated, err := s.updateScheduleForActor(r.Context(), actor, row, request)
	if err != nil {
		if isUniqueViolation(err) {
			writeError(w, http.StatusConflict, errors.New("schedule already exists"))
			return
		}
		if errors.Is(err, errPermissionRequired) {
			writeError(w, http.StatusForbidden, err)
			return
		}
		s.writeCreateScheduleError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, scheduleResponse(updatedScheduleView(updated)))
}

func (s *Server) writeCreateScheduleError(w http.ResponseWriter, err error) {
	var upstreamErr createRunUpstreamError
	if errors.As(err, &upstreamErr) {
		writeError(w, http.StatusBadGateway, upstreamErr)
		return
	}
	var runDeploymentErr runDeploymentSelectionError
	if errors.As(err, &runDeploymentErr) {
		writeError(w, http.StatusBadRequest, runDeploymentErr)
		return
	}
	if isCreateRunConfigError(err) {
		writeError(w, http.StatusServiceUnavailable, err)
		return
	}
	if isCreateRunClientError(err) {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	s.log.Error("create schedule failed", "error", err)
	writeError(w, http.StatusInternalServerError, errors.New("create schedule"))
}

func (s *Server) updateScheduleForActor(ctx context.Context, actor auth.Actor, current db.GetScheduleSummaryRow, request api.CreateScheduleRequest) (db.UpdateScheduleRow, error) {
	request.Task = strings.TrimSpace(request.Task)
	if err := api.ValidateTaskID(request.Task); err != nil {
		return db.UpdateScheduleRow{}, err
	}
	cronExpression := strings.TrimSpace(request.Cron)
	if strings.TrimSpace(request.DeduplicationKey) != "" {
		return db.UpdateScheduleRow{}, errors.New("deduplication_key cannot be updated")
	}
	runOptions := request.Options.CreateRunOptions()
	dedupKey := current.DedupKey
	projectUUID := ids.MustFromPG(current.ProjectID)
	environmentUUID := ids.MustFromPG(current.EnvironmentID)
	scope := auth.Scope{
		OrgID:         actor.OrgID,
		ProjectID:     apiKeyScopeID(current.ProjectID, auth.DefaultProjectID),
		EnvironmentID: apiKeyScopeID(current.EnvironmentID, auth.DefaultEnvironmentID),
	}
	if !actor.HasPermission(auth.PermissionRunsCreate, scope) {
		return db.UpdateScheduleRow{}, errPermissionRequired
	}
	if request.SecretBindings == nil {
		request.SecretBindings = api.SecretBindings{}
	}
	if err := secret.ValidateBindings(request.SecretBindings); err != nil {
		return db.UpdateScheduleRow{}, err
	}
	if len(request.SecretBindings) > 0 && !actor.HasPermission(auth.PermissionSecretsUse, scope) {
		return db.UpdateScheduleRow{}, fmt.Errorf("%w to bind secrets", errPermissionRequired)
	}
	if len(request.SecretBindings) > 0 && s.secrets == nil {
		return db.UpdateScheduleRow{}, errors.New("secret store is not configured")
	}
	if len(request.SecretBindings) > 0 {
		if err := s.secrets.CheckScoped(ctx, actor.OrgID, projectUUID, environmentUUID, request.SecretBindings); err != nil {
			return db.UpdateScheduleRow{}, err
		}
	}
	workspace, err := ghapp.NormalizeSource(api.GitHubSource{
		Repository: request.Workspace.Repository,
		Ref:        request.Workspace.Ref,
		SHA:        request.Workspace.SHA,
		Subpath:    request.Workspace.Subpath,
	})
	if err != nil {
		return db.UpdateScheduleRow{}, relabelGitHubSourceError(err, "workspace")
	}
	if _, err := s.db.GetActiveProjectGitHubRepositoryByFullName(ctx, db.GetActiveProjectGitHubRepositoryByFullNameParams{
		OrgID:     ids.ToPG(actor.OrgID),
		ProjectID: current.ProjectID,
		FullName:  workspace.Repository,
	}); errors.Is(err, pgx.ErrNoRows) {
		return db.UpdateScheduleRow{}, relabelGitHubSourceError(ghapp.InvalidSourceError{Err: errors.New("github repository is not enabled for this project workspace")}, "workspace")
	} else if err != nil {
		return db.UpdateScheduleRow{}, fmt.Errorf("authorize github workspace repository: %w", err)
	}
	deploymentSelection, err := normalizeRunDeploymentSelection(runOptions.DeploymentID, runOptions.Version)
	if err != nil {
		return db.UpdateScheduleRow{}, err
	}
	deploymentTask, err := s.deploymentTaskForRunRequest(ctx, actor.OrgID, current.ProjectID, current.EnvironmentID, request.Task, deploymentSelection)
	if errors.Is(err, pgx.ErrNoRows) {
		return db.UpdateScheduleRow{}, fmt.Errorf("task %q is not deployed in the selected deployment", request.Task)
	}
	if err != nil {
		return db.UpdateScheduleRow{}, err
	}
	if _, err := runMaxDurationSeconds(runOptions.MaxDurationSeconds, deploymentTask.MaxDurationSeconds); err != nil {
		return db.UpdateScheduleRow{}, err
	}
	scheduling, err := s.resolveRunScheduling(runOptions, deploymentTask)
	if err != nil {
		return db.UpdateScheduleRow{}, err
	}
	if _, err := s.validateRunQueueOverride(ctx, actor.OrgID, current.ProjectID, current.EnvironmentID, runOptions, deploymentTask, scheduling); err != nil {
		return db.UpdateScheduleRow{}, err
	}
	timezone := api.NormalizeTimezone(request.Timezone)
	next, err := schedule.NextCronTime(cronExpression, timezone, time.Now())
	if err != nil {
		return db.UpdateScheduleRow{}, err
	}
	active := current.ScheduleActive && current.InstanceActive
	if request.Active != nil {
		active = *request.Active
	}
	var nextScheduledAt pgtype.Timestamptz
	if active {
		nextScheduledAt = pgTimeToPG(next)
	}
	workspaceJSON, err := json.Marshal(api.ScheduleWorkspace{
		Repository: workspace.Repository,
		Ref:        workspace.Ref,
		SHA:        workspace.SHA,
		Subpath:    workspace.Subpath,
	})
	if err != nil {
		return db.UpdateScheduleRow{}, err
	}
	secretBindingsJSON, err := json.Marshal(request.SecretBindings)
	if err != nil {
		return db.UpdateScheduleRow{}, err
	}
	runOptionsJSON, err := json.Marshal(runOptions)
	if err != nil {
		return db.UpdateScheduleRow{}, err
	}
	return s.db.UpdateSchedule(ctx, db.UpdateScheduleParams{
		TaskID:          request.Task,
		DedupKey:        dedupKey,
		ExternalID:      nullableText(strings.TrimSpace(request.ExternalID)),
		Cron:            cronExpression,
		Timezone:        timezone,
		SecretBindings:  secretBindingsJSON,
		Workspace:       workspaceJSON,
		RunOptions:      runOptionsJSON,
		Active:          active,
		OrgID:           ids.ToPG(actor.OrgID),
		ProjectID:       current.ProjectID,
		EnvironmentID:   current.EnvironmentID,
		ScheduleID:      current.ScheduleID,
		NextScheduledAt: nextScheduledAt,
	})
}

func (s *Server) createScheduleForActor(ctx context.Context, actor auth.Actor, request api.CreateScheduleRequest) (db.CreateScheduleRow, error) {
	request.Task = strings.TrimSpace(request.Task)
	if err := api.ValidateTaskID(request.Task); err != nil {
		return db.CreateScheduleRow{}, err
	}
	cronExpression := strings.TrimSpace(request.Cron)
	dedupKey := strings.TrimSpace(request.DeduplicationKey)
	if dedupKey == "" {
		return db.CreateScheduleRow{}, errors.New("deduplication_key is required")
	}
	runOptions := request.Options.CreateRunOptions()
	if err := api.ValidateScheduleID(dedupKey); err != nil {
		return db.CreateScheduleRow{}, err
	}
	scope, projectID, environmentID, err := s.createRunRequestScope(ctx, actor, request.ProjectID, request.EnvironmentID)
	if err != nil {
		return db.CreateScheduleRow{}, err
	}
	if !actor.HasPermission(auth.PermissionRunsCreate, scope) {
		return db.CreateScheduleRow{}, errPermissionRequired
	}
	if request.SecretBindings == nil {
		request.SecretBindings = api.SecretBindings{}
	}
	if err := secret.ValidateBindings(request.SecretBindings); err != nil {
		return db.CreateScheduleRow{}, err
	}
	if len(request.SecretBindings) > 0 && !actor.HasPermission(auth.PermissionSecretsUse, scope) {
		return db.CreateScheduleRow{}, fmt.Errorf("%w to bind secrets", errPermissionRequired)
	}
	if len(request.SecretBindings) > 0 && s.secrets == nil {
		return db.CreateScheduleRow{}, errors.New("secret store is not configured")
	}
	if len(request.SecretBindings) > 0 {
		if err := s.secrets.CheckScoped(ctx, actor.OrgID, ids.MustFromPG(projectID), ids.MustFromPG(environmentID), request.SecretBindings); err != nil {
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
	deploymentSelection, err := normalizeRunDeploymentSelection(runOptions.DeploymentID, runOptions.Version)
	if err != nil {
		return db.CreateScheduleRow{}, err
	}
	deploymentTask, err := s.deploymentTaskForRunRequest(ctx, actor.OrgID, projectID, environmentID, request.Task, deploymentSelection)
	if errors.Is(err, pgx.ErrNoRows) {
		return db.CreateScheduleRow{}, fmt.Errorf("task %q is not deployed in the selected deployment", request.Task)
	}
	if err != nil {
		return db.CreateScheduleRow{}, err
	}
	if _, err := runMaxDurationSeconds(runOptions.MaxDurationSeconds, deploymentTask.MaxDurationSeconds); err != nil {
		return db.CreateScheduleRow{}, err
	}
	scheduling, err := s.resolveRunScheduling(runOptions, deploymentTask)
	if err != nil {
		return db.CreateScheduleRow{}, err
	}
	if _, err := s.validateRunQueueOverride(ctx, actor.OrgID, projectID, environmentID, runOptions, deploymentTask, scheduling); err != nil {
		return db.CreateScheduleRow{}, err
	}
	timezone := api.NormalizeTimezone(request.Timezone)
	next, err := schedule.NextCronTime(cronExpression, timezone, time.Now())
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
	if active {
		nextScheduledAt = pgTimeToPG(next)
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
	secretBindingsJSON, err := json.Marshal(request.SecretBindings)
	if err != nil {
		return db.CreateScheduleRow{}, err
	}
	runOptionsJSON, err := json.Marshal(runOptions)
	if err != nil {
		return db.CreateScheduleRow{}, err
	}
	return s.db.CreateSchedule(ctx, db.CreateScheduleParams{
		ScheduleID:      ids.ToPG(scheduleID),
		OrgID:           ids.ToPG(actor.OrgID),
		ProjectID:       projectID,
		ScheduleType:    db.TaskScheduleTypeImperative,
		TaskID:          request.Task,
		DedupKey:        dedupKey,
		ExternalID:      nullableText(strings.TrimSpace(request.ExternalID)),
		Cron:            cronExpression,
		Timezone:        timezone,
		SecretBindings:  secretBindingsJSON,
		Workspace:       workspaceJSON,
		RunOptions:      runOptionsJSON,
		Active:          active,
		InstanceID:      ids.ToPG(instanceID),
		EnvironmentID:   environmentID,
		NextScheduledAt: nextScheduledAt,
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
	if row.ScheduleType == db.TaskScheduleTypeDeclarative {
		writeError(w, http.StatusBadRequest, errors.New("declarative schedules are managed by task definitions"))
		return
	}
	affected, err := s.db.DeleteSchedule(r.Context(), db.DeleteScheduleParams{
		OrgID:         row.OrgID,
		ProjectID:     row.ProjectID,
		EnvironmentID: row.EnvironmentID,
		ScheduleID:    row.ScheduleID,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, errors.New("delete schedule"))
		return
	}
	if affected == 0 {
		writeError(w, http.StatusNotFound, errors.New("schedule not found"))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) setScheduleState(w http.ResponseWriter, r *http.Request, active bool) {
	row, ok := s.loadScheduleForRequest(w, r, auth.PermissionRunsCreate)
	if !ok {
		return
	}
	if row.ScheduleType == db.TaskScheduleTypeDeclarative {
		writeError(w, http.StatusBadRequest, errors.New("declarative schedules are managed by task definitions"))
		return
	}
	var nextScheduledAt pgtype.Timestamptz
	if active {
		next, err := schedule.NextCronTime(row.Cron, row.Timezone, time.Now())
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		nextScheduledAt = pgTimeToPG(next)
	}
	updated, err := s.db.UpdateScheduleState(r.Context(), db.UpdateScheduleStateParams{
		Active:          active,
		OrgID:           row.OrgID,
		ProjectID:       row.ProjectID,
		ScheduleID:      row.ScheduleID,
		NextScheduledAt: nextScheduledAt,
		EnvironmentID:   row.EnvironmentID,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, errors.New("update schedule"))
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
	ScheduleType    db.TaskScheduleType
	ProjectID       pgtype.UUID
	EnvironmentID   pgtype.UUID
	TaskID          string
	DedupKey        string
	ExternalID      pgtype.Text
	Cron            string
	Timezone        string
	Workspace       []byte
	ScheduleActive  bool
	InstanceActive  bool
	NextScheduledAt pgtype.Timestamptz
	LastScheduledAt pgtype.Timestamptz
	TriggerAttempts int32
	TriggerError    string
	CreatedAt       pgtype.Timestamptz
	UpdatedAt       pgtype.Timestamptz
}

func createScheduleView(row db.CreateScheduleRow) scheduleView {
	return scheduleView{
		ScheduleID:      row.ScheduleID,
		ScheduleType:    row.ScheduleType,
		ProjectID:       row.ProjectID,
		EnvironmentID:   row.EnvironmentID,
		TaskID:          row.TaskID,
		DedupKey:        row.DedupKey,
		ExternalID:      row.ExternalID,
		Cron:            row.Cron,
		Timezone:        row.Timezone,
		Workspace:       row.Workspace,
		ScheduleActive:  row.ScheduleActive,
		InstanceActive:  row.InstanceActive,
		NextScheduledAt: row.NextScheduledAt,
		LastScheduledAt: row.LastScheduledAt,
		TriggerAttempts: row.TriggerAttemptCount,
		TriggerError:    row.TriggerErrorMessage,
		CreatedAt:       row.CreatedAt,
		UpdatedAt:       row.UpdatedAt,
	}
}

func listScheduleView(row db.ListScheduleSummariesRow) scheduleView {
	return scheduleView{
		ScheduleID:      row.ScheduleID,
		ScheduleType:    row.ScheduleType,
		ProjectID:       row.ProjectID,
		EnvironmentID:   row.EnvironmentID,
		TaskID:          row.TaskID,
		DedupKey:        row.DedupKey,
		ExternalID:      row.ExternalID,
		Cron:            row.Cron,
		Timezone:        row.Timezone,
		Workspace:       row.Workspace,
		ScheduleActive:  row.ScheduleActive,
		InstanceActive:  row.InstanceActive,
		NextScheduledAt: row.NextScheduledAt,
		LastScheduledAt: row.LastScheduledAt,
		TriggerAttempts: row.TriggerAttemptCount,
		TriggerError:    row.TriggerErrorMessage,
		CreatedAt:       row.CreatedAt,
		UpdatedAt:       row.UpdatedAt,
	}
}

func getScheduleView(row db.GetScheduleSummaryRow) scheduleView {
	return scheduleView{
		ScheduleID:      row.ScheduleID,
		ScheduleType:    row.ScheduleType,
		ProjectID:       row.ProjectID,
		EnvironmentID:   row.EnvironmentID,
		TaskID:          row.TaskID,
		DedupKey:        row.DedupKey,
		ExternalID:      row.ExternalID,
		Cron:            row.Cron,
		Timezone:        row.Timezone,
		Workspace:       row.Workspace,
		ScheduleActive:  row.ScheduleActive,
		InstanceActive:  row.InstanceActive,
		NextScheduledAt: row.NextScheduledAt,
		LastScheduledAt: row.LastScheduledAt,
		TriggerAttempts: row.TriggerAttemptCount,
		TriggerError:    row.TriggerErrorMessage,
		CreatedAt:       row.CreatedAt,
		UpdatedAt:       row.UpdatedAt,
	}
}

func updateScheduleView(row db.UpdateScheduleStateRow) scheduleView {
	return scheduleView{
		ScheduleID:      row.ScheduleID,
		ScheduleType:    row.ScheduleType,
		ProjectID:       row.ProjectID,
		EnvironmentID:   row.EnvironmentID,
		TaskID:          row.TaskID,
		DedupKey:        row.DedupKey,
		ExternalID:      row.ExternalID,
		Cron:            row.Cron,
		Timezone:        row.Timezone,
		Workspace:       row.Workspace,
		ScheduleActive:  row.ScheduleActive,
		InstanceActive:  row.InstanceActive,
		NextScheduledAt: row.NextScheduledAt,
		LastScheduledAt: row.LastScheduledAt,
		TriggerAttempts: row.TriggerAttemptCount,
		TriggerError:    row.TriggerErrorMessage,
		CreatedAt:       row.CreatedAt,
		UpdatedAt:       row.UpdatedAt,
	}
}

func updatedScheduleView(row db.UpdateScheduleRow) scheduleView {
	return scheduleView{
		ScheduleID:      row.ScheduleID,
		ScheduleType:    row.ScheduleType,
		ProjectID:       row.ProjectID,
		EnvironmentID:   row.EnvironmentID,
		TaskID:          row.TaskID,
		DedupKey:        row.DedupKey,
		ExternalID:      row.ExternalID,
		Cron:            row.Cron,
		Timezone:        row.Timezone,
		Workspace:       row.Workspace,
		ScheduleActive:  row.ScheduleActive,
		InstanceActive:  row.InstanceActive,
		NextScheduledAt: row.NextScheduledAt,
		LastScheduledAt: row.LastScheduledAt,
		TriggerAttempts: row.TriggerAttemptCount,
		TriggerError:    row.TriggerErrorMessage,
		CreatedAt:       row.CreatedAt,
		UpdatedAt:       row.UpdatedAt,
	}
}

func scheduleResponse(row scheduleView) api.ScheduleResponse {
	response := api.ScheduleResponse{
		ID:               ids.MustFromPG(row.ScheduleID).String(),
		Type:             string(row.ScheduleType),
		ProjectID:        apiKeyScopeID(row.ProjectID, auth.DefaultProjectID),
		EnvironmentID:    apiKeyScopeID(row.EnvironmentID, auth.DefaultEnvironmentID),
		Task:             row.TaskID,
		DeduplicationKey: row.DedupKey,
		ExternalID:       pgTextValue(row.ExternalID),
		Cron:             row.Cron,
		Timezone:         row.Timezone,
		Active:           row.ScheduleActive && row.InstanceActive,
		Status:           scheduleStatus(row),
		LastError:        row.TriggerError,
		Workspace:        append(json.RawMessage(nil), row.Workspace...),
		CreatedAt:        pgTime(row.CreatedAt),
		UpdatedAt:        pgTime(row.UpdatedAt),
	}
	response.NextScheduledAt = pgTimePtr(row.NextScheduledAt)
	response.LastScheduledAt = pgTimePtr(row.LastScheduledAt)
	return response
}

func pgTextValue(value pgtype.Text) string {
	if !value.Valid {
		return ""
	}
	return value.String
}

func scheduleStatus(row scheduleView) string {
	if row.TriggerError != "" {
		return "errored"
	}
	if row.ScheduleActive && row.InstanceActive {
		return "active"
	}
	return "inactive"
}
