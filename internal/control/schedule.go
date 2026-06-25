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
	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/schedule"
	"github.com/jackc/pgx/v5/pgtype"
)

const (
	scheduleListPageSize = int32(200)
)

func (s *Server) createSchedule(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, unavailable(errors.New("schedule storage is not configured")))
		return
	}
	actor := actorFromContext(r.Context())
	var request api.CreateScheduleRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid schedule request JSON: %w", err)))
		return
	}
	projectID, environmentID, err := environmentScopeRefsFromRequest(r, actor, request.ProjectID, request.EnvironmentID)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	request.ProjectID = projectID
	request.EnvironmentID = environmentID
	row, err := s.createScheduleForActor(r.Context(), actor, request)
	if err != nil {
		if isUniqueViolation(err) {
			writeError(w, conflict(errors.New("schedule already exists")))
			return
		}
		if errors.Is(err, errPermissionRequired) {
			writeError(w, forbidden(err))
			return
		}
		s.writeCreateScheduleError(w, err)
		return
	}
	view := createScheduleView(row)
	s.registerScheduleInstances(r.Context(), pgvalue.UUID(actor.OrgID), view.ProjectID, view.ScheduleID)
	writeJSON(w, http.StatusCreated, scheduleResponse(view))
}

func (s *Server) updateSchedule(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, unavailable(errors.New("schedule storage is not configured")))
		return
	}
	actor := actorFromContext(r.Context())
	row, ok := s.loadScheduleForRequest(w, r, auth.PermissionRunsCreate)
	if !ok {
		return
	}
	if row.ScheduleType == db.TaskScheduleTypeDeclarative {
		writeError(w, badRequest(errors.New("declarative schedules are managed by task definitions")))
		return
	}
	var request api.CreateScheduleRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid schedule request JSON: %w", err)))
		return
	}
	if _, _, err := environmentScopeRefsFromRequest(r, actor, request.ProjectID, request.EnvironmentID); err != nil {
		writeError(w, badRequest(err))
		return
	}
	updated, err := s.updateScheduleForActor(r.Context(), actor, row, request)
	if err != nil {
		if isUniqueViolation(err) {
			writeError(w, conflict(errors.New("schedule already exists")))
			return
		}
		if errors.Is(err, errPermissionRequired) {
			writeError(w, forbidden(err))
			return
		}
		s.writeCreateScheduleError(w, err)
		return
	}
	view := updatedScheduleView(updated)
	s.registerScheduleInstances(r.Context(), pgvalue.UUID(actor.OrgID), view.ProjectID, view.ScheduleID)
	writeJSON(w, http.StatusOK, scheduleResponse(view))
}

func (s *Server) writeCreateScheduleError(w http.ResponseWriter, err error) {
	var upstreamErr createRunUpstreamError
	if errors.As(err, &upstreamErr) {
		writeError(w, badGateway(upstreamErr))
		return
	}
	var runDeploymentErr runDeploymentSelectionError
	if errors.As(err, &runDeploymentErr) {
		writeError(w, badRequest(runDeploymentErr))
		return
	}
	if isCreateRunConfigError(err) {
		writeError(w, unavailable(err))
		return
	}
	if err.Error() == "deduplication_key is required" {
		writeError(w, badRequest(err))
		return
	}
	if isCreateRunClientError(err) {
		writeError(w, badRequest(err))
		return
	}
	s.log.Error("create schedule failed", "error", err)
	writeError(w, errors.New("create schedule"))
}

func (s *Server) updateScheduleForActor(ctx context.Context, actor auth.Actor, current db.GetScheduleSummaryRow, request api.CreateScheduleRequest) (db.UpdateScheduleRow, error) {
	request.Task = strings.TrimSpace(request.Task)
	if err := api.ValidateTaskID(request.Task); err != nil {
		return db.UpdateScheduleRow{}, err
	}
	cronExpression := strings.TrimSpace(request.Cron)
	requestDedupKey := strings.TrimSpace(request.DeduplicationKey)
	if requestDedupKey != "" && (!current.UserDedupKey.Valid || requestDedupKey != current.UserDedupKey.String) {
		return db.UpdateScheduleRow{}, errors.New("deduplication_key cannot be updated")
	}
	runOptions := request.Options.CreateRunOptions()
	scope := auth.Scope{
		OrgID:         actor.OrgID,
		ProjectID:     pgvalue.MustUUIDValue(current.ProjectID).String(),
		EnvironmentID: pgvalue.MustUUIDValue(current.EnvironmentID).String(),
	}
	if !actor.HasPermission(auth.PermissionRunsCreate, scope) {
		return db.UpdateScheduleRow{}, errPermissionRequired
	}
	deploymentTask, err := s.deploymentTaskForRunRequest(ctx, actor.OrgID, current.ProjectID, current.EnvironmentID, request.Task, runDeploymentSelection{})
	if isNoRows(err) {
		return db.UpdateScheduleRow{}, fmt.Errorf("task %q is not deployed in the selected deployment", request.Task)
	}
	if err != nil {
		return db.UpdateScheduleRow{}, err
	}
	if _, err := runMaxDurationSeconds(runOptions.MaxDurationSeconds, deploymentTask.MaxActiveDurationMs); err != nil {
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
	nextFireAt := pgvalue.Timestamptz(next)
	runOptionsJSON, err := json.Marshal(runOptions)
	if err != nil {
		return db.UpdateScheduleRow{}, err
	}
	return s.db.UpdateSchedule(ctx, db.UpdateScheduleParams{
		TaskID:        request.Task,
		ExternalID:    pgvalue.Text(strings.TrimSpace(request.ExternalID)),
		Cron:          cronExpression,
		Timezone:      timezone,
		RunOptions:    runOptionsJSON,
		Active:        active,
		OrgID:         pgvalue.UUID(actor.OrgID),
		ProjectID:     current.ProjectID,
		EnvironmentID: current.EnvironmentID,
		ScheduleID:    current.ScheduleID,
		NextFireAt:    nextFireAt,
	})
}

func (s *Server) createScheduleForActor(ctx context.Context, actor auth.Actor, request api.CreateScheduleRequest) (db.CreateScheduleRow, error) {
	request.Task = strings.TrimSpace(request.Task)
	if err := api.ValidateTaskID(request.Task); err != nil {
		return db.CreateScheduleRow{}, err
	}
	cronExpression := strings.TrimSpace(request.Cron)
	userDedupKey := strings.TrimSpace(request.DeduplicationKey)
	if userDedupKey == "" {
		return db.CreateScheduleRow{}, errors.New("deduplication_key is required")
	}
	runOptions := request.Options.CreateRunOptions()
	if err := api.ValidateScheduleID(userDedupKey); err != nil {
		return db.CreateScheduleRow{}, err
	}
	userDedupKeyParam := pgtype.Text{String: userDedupKey, Valid: true}
	scope, projectID, environmentID, err := s.requestEnvironmentScope(ctx, actor, request.ProjectID, request.EnvironmentID)
	if err != nil {
		return db.CreateScheduleRow{}, err
	}
	if !actor.HasPermission(auth.PermissionRunsCreate, scope) {
		return db.CreateScheduleRow{}, errPermissionRequired
	}
	deploymentTask, err := s.deploymentTaskForRunRequest(ctx, actor.OrgID, projectID, environmentID, request.Task, runDeploymentSelection{})
	if isNoRows(err) {
		return db.CreateScheduleRow{}, fmt.Errorf("task %q is not deployed in the selected deployment", request.Task)
	}
	if err != nil {
		return db.CreateScheduleRow{}, err
	}
	if _, err := runMaxDurationSeconds(runOptions.MaxDurationSeconds, deploymentTask.MaxActiveDurationMs); err != nil {
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
	scheduleID := uuid.Must(uuid.NewV7())
	instanceID := uuid.Must(uuid.NewV7())
	nextFireAt := pgvalue.Timestamptz(next)
	runOptionsJSON, err := json.Marshal(runOptions)
	if err != nil {
		return db.CreateScheduleRow{}, err
	}
	return s.db.CreateSchedule(ctx, db.CreateScheduleParams{
		ScheduleID:    pgvalue.UUID(scheduleID),
		OrgID:         pgvalue.UUID(actor.OrgID),
		ProjectID:     projectID,
		ScheduleType:  db.TaskScheduleTypeImperative,
		TaskID:        request.Task,
		DedupKey:      userDedupKey,
		UserDedupKey:  userDedupKeyParam,
		ExternalID:    pgvalue.Text(strings.TrimSpace(request.ExternalID)),
		Cron:          cronExpression,
		Timezone:      timezone,
		RunOptions:    runOptionsJSON,
		Active:        active,
		InstanceID:    pgvalue.UUID(instanceID),
		EnvironmentID: environmentID,
		NextFireAt:    nextFireAt,
	})
}

func (s *Server) listSchedules(w http.ResponseWriter, r *http.Request) {
	actor := actorFromContext(r.Context())
	scope, projectID, environmentID, err := s.requestEnvironmentScopeFromRequest(r, actor, r.URL.Query().Get("project_id"), r.URL.Query().Get("environment_id"))
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	if !actor.HasPermission(auth.PermissionRunsRead, scope) {
		writeError(w, forbidden(errors.New("permission is required")))
		return
	}
	rows, err := s.db.ListScheduleSummaries(r.Context(), db.ListScheduleSummariesParams{
		OrgID:         pgvalue.UUID(actor.OrgID),
		ProjectID:     projectID,
		EnvironmentID: environmentID,
		RowLimit:      scheduleListPageSize,
	})
	if err != nil {
		writeError(w, errors.New("list schedules"))
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
		writeError(w, badRequest(errors.New("declarative schedules are managed by task definitions")))
		return
	}
	affected, err := s.db.DeleteSchedule(r.Context(), db.DeleteScheduleParams{
		OrgID:         row.OrgID,
		ProjectID:     row.ProjectID,
		EnvironmentID: row.EnvironmentID,
		ScheduleID:    row.ScheduleID,
	})
	if err != nil {
		writeError(w, errors.New("delete schedule"))
		return
	}
	if affected == 0 {
		writeError(w, notFound(errors.New("schedule not found")))
		return
	}
	s.deleteScheduleIndexEntry(r.Context(), row.ScheduleID, row.InstanceID)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) setScheduleState(w http.ResponseWriter, r *http.Request, active bool) {
	row, ok := s.loadScheduleForRequest(w, r, auth.PermissionRunsCreate)
	if !ok {
		return
	}
	if row.ScheduleType == db.TaskScheduleTypeDeclarative {
		writeError(w, badRequest(errors.New("declarative schedules are managed by task definitions")))
		return
	}
	var nextFireAt pgtype.Timestamptz
	if active {
		next, err := schedule.NextCronTime(row.Cron, row.Timezone, time.Now())
		if err != nil {
			writeError(w, badRequest(err))
			return
		}
		nextFireAt = pgvalue.Timestamptz(next)
	}
	updated, err := s.db.UpdateScheduleState(r.Context(), db.UpdateScheduleStateParams{
		Active:        active,
		OrgID:         row.OrgID,
		ProjectID:     row.ProjectID,
		ScheduleID:    row.ScheduleID,
		NextFireAt:    nextFireAt,
		EnvironmentID: row.EnvironmentID,
	})
	if err != nil {
		if isNoRows(err) {
			writeError(w, notFound(errors.New("schedule not found")))
			return
		}
		writeError(w, errors.New("update schedule"))
		return
	}
	view := updateScheduleView(updated)
	s.registerScheduleInstances(r.Context(), row.OrgID, row.ProjectID, row.ScheduleID)
	writeJSON(w, http.StatusOK, scheduleResponse(view))
}

func (s *Server) registerScheduleInstances(ctx context.Context, orgID pgtype.UUID, projectID pgtype.UUID, scheduleID pgtype.UUID) {
	if s.scheduleEngine == nil || s.db == nil {
		return
	}
	rows, err := s.db.ListScheduleInstancesForRegistration(ctx, db.ListScheduleInstancesForRegistrationParams{
		OrgID:      orgID,
		ProjectID:  projectID,
		ScheduleID: scheduleID,
	})
	if err != nil {
		s.log.Warn("list schedule instances for registration failed", "schedule_id", pgvalue.MustUUIDValue(scheduleID).String(), "error", err)
		return
	}
	for _, row := range rows {
		if err := s.scheduleEngine.RegisterNext(ctx, schedule.Instance{
			InstanceID: row.InstanceID,
			Generation: row.Generation,
			Active:     row.ScheduleActive && row.InstanceActive,
			NextFireAt: row.NextFireAt,
			RetryAfter: row.RetryAfter,
		}); err != nil {
			s.log.Warn("register schedule next fire failed", "schedule_id", pgvalue.MustUUIDValue(scheduleID).String(), "instance_id", pgvalue.MustUUIDValue(row.InstanceID).String(), "error", err)
		}
	}
}

func (s *Server) deleteScheduleIndexEntry(ctx context.Context, scheduleID pgtype.UUID, instanceID pgtype.UUID) {
	if s.scheduleEngine == nil {
		return
	}
	if err := s.scheduleEngine.DeleteInstance(ctx, instanceID); err != nil {
		s.log.Warn("delete schedule index entry failed", "schedule_id", pgvalue.MustUUIDValue(scheduleID).String(), "instance_id", pgvalue.MustUUIDValue(instanceID).String(), "error", err)
	}
}

func (s *Server) registerChangedScheduleInstances(ctx context.Context, orgID pgtype.UUID, projectID pgtype.UUID, schedules []scheduleView) {
	seen := make(map[string]struct{}, len(schedules))
	for _, row := range schedules {
		key := pgvalue.MustUUIDValue(row.ScheduleID).String()
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		s.registerScheduleInstances(ctx, orgID, projectID, row.ScheduleID)
	}
}

func (s *Server) loadScheduleForRequest(w http.ResponseWriter, r *http.Request, permission auth.Permission) (db.GetScheduleSummaryRow, bool) {
	actor := actorFromContext(r.Context())
	scheduleID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, badRequest(errors.New("schedule id must be a UUID")))
		return db.GetScheduleSummaryRow{}, false
	}
	scope, projectID, environmentID, err := s.requestEnvironmentScopeFromRequest(r, actor, r.URL.Query().Get("project_id"), r.URL.Query().Get("environment_id"))
	if err != nil {
		writeError(w, badRequest(err))
		return db.GetScheduleSummaryRow{}, false
	}
	if !actor.HasPermission(permission, scope) {
		writeError(w, forbidden(errors.New("permission is required")))
		return db.GetScheduleSummaryRow{}, false
	}
	row, err := s.db.GetScheduleSummary(r.Context(), db.GetScheduleSummaryParams{
		OrgID:         pgvalue.UUID(actor.OrgID),
		ProjectID:     projectID,
		EnvironmentID: environmentID,
		ScheduleID:    pgvalue.UUID(scheduleID),
	})
	if isNoRows(err) {
		writeError(w, notFound(errors.New("schedule not found")))
		return db.GetScheduleSummaryRow{}, false
	}
	if err != nil {
		writeError(w, errors.New("load schedule"))
		return db.GetScheduleSummaryRow{}, false
	}
	return row, true
}

type scheduleView struct {
	ScheduleID      pgtype.UUID
	InstanceID      pgtype.UUID
	ScheduleType    db.TaskScheduleType
	ProjectID       pgtype.UUID
	EnvironmentID   pgtype.UUID
	TaskID          string
	DedupKey        string
	UserDedupKey    pgtype.Text
	ExternalID      pgtype.Text
	Cron            string
	Timezone        string
	ScheduleActive  bool
	InstanceActive  bool
	Generation      int64
	NextFireAt      pgtype.Timestamptz
	LastFireAt      pgtype.Timestamptz
	TriggerAttempts int32
	TriggerError    string
	CreatedAt       pgtype.Timestamptz
	UpdatedAt       pgtype.Timestamptz
}

func createScheduleView(row db.CreateScheduleRow) scheduleView {
	return scheduleView{
		ScheduleID:      row.ScheduleID,
		InstanceID:      row.InstanceID,
		ScheduleType:    row.ScheduleType,
		ProjectID:       row.ProjectID,
		EnvironmentID:   row.EnvironmentID,
		TaskID:          row.TaskID,
		DedupKey:        row.DedupKey,
		UserDedupKey:    row.UserDedupKey,
		ExternalID:      row.ExternalID,
		Cron:            row.Cron,
		Timezone:        row.Timezone,
		ScheduleActive:  row.ScheduleActive,
		InstanceActive:  row.InstanceActive,
		Generation:      row.Generation,
		NextFireAt:      row.NextFireAt,
		LastFireAt:      row.LastFireAt,
		TriggerAttempts: row.TriggerAttemptCount,
		TriggerError:    row.TriggerErrorMessage,
		CreatedAt:       row.CreatedAt,
		UpdatedAt:       row.UpdatedAt,
	}
}

func createDeclarativeScheduleView(row db.CreateDeclarativeScheduleRow) scheduleView {
	return scheduleView{
		ScheduleID:      row.ScheduleID,
		InstanceID:      row.InstanceID,
		ScheduleType:    row.ScheduleType,
		ProjectID:       row.ProjectID,
		EnvironmentID:   row.EnvironmentID,
		TaskID:          row.TaskID,
		DedupKey:        row.DedupKey,
		UserDedupKey:    row.UserDedupKey,
		ExternalID:      row.ExternalID,
		Cron:            row.Cron,
		Timezone:        row.Timezone,
		ScheduleActive:  row.ScheduleActive,
		InstanceActive:  row.InstanceActive,
		Generation:      row.Generation,
		NextFireAt:      row.NextFireAt,
		LastFireAt:      row.LastFireAt,
		TriggerAttempts: row.TriggerAttemptCount,
		TriggerError:    row.TriggerErrorMessage,
		CreatedAt:       row.CreatedAt,
		UpdatedAt:       row.UpdatedAt,
	}
}

func listScheduleView(row db.ListScheduleSummariesRow) scheduleView {
	return scheduleView{
		ScheduleID:      row.ScheduleID,
		InstanceID:      row.InstanceID,
		ScheduleType:    row.ScheduleType,
		ProjectID:       row.ProjectID,
		EnvironmentID:   row.EnvironmentID,
		TaskID:          row.TaskID,
		DedupKey:        row.DedupKey,
		UserDedupKey:    row.UserDedupKey,
		ExternalID:      row.ExternalID,
		Cron:            row.Cron,
		Timezone:        row.Timezone,
		ScheduleActive:  row.ScheduleActive,
		InstanceActive:  row.InstanceActive,
		Generation:      row.Generation,
		NextFireAt:      row.NextFireAt,
		LastFireAt:      row.LastFireAt,
		TriggerAttempts: row.TriggerAttemptCount,
		TriggerError:    row.TriggerErrorMessage,
		CreatedAt:       row.CreatedAt,
		UpdatedAt:       row.UpdatedAt,
	}
}

func getScheduleView(row db.GetScheduleSummaryRow) scheduleView {
	return scheduleView{
		ScheduleID:      row.ScheduleID,
		InstanceID:      row.InstanceID,
		ScheduleType:    row.ScheduleType,
		ProjectID:       row.ProjectID,
		EnvironmentID:   row.EnvironmentID,
		TaskID:          row.TaskID,
		DedupKey:        row.DedupKey,
		UserDedupKey:    row.UserDedupKey,
		ExternalID:      row.ExternalID,
		Cron:            row.Cron,
		Timezone:        row.Timezone,
		ScheduleActive:  row.ScheduleActive,
		InstanceActive:  row.InstanceActive,
		Generation:      row.Generation,
		NextFireAt:      row.NextFireAt,
		LastFireAt:      row.LastFireAt,
		TriggerAttempts: row.TriggerAttemptCount,
		TriggerError:    row.TriggerErrorMessage,
		CreatedAt:       row.CreatedAt,
		UpdatedAt:       row.UpdatedAt,
	}
}

func updateScheduleView(row db.UpdateScheduleStateRow) scheduleView {
	return scheduleView{
		ScheduleID:      row.ScheduleID,
		InstanceID:      row.InstanceID,
		ScheduleType:    row.ScheduleType,
		ProjectID:       row.ProjectID,
		EnvironmentID:   row.EnvironmentID,
		TaskID:          row.TaskID,
		DedupKey:        row.DedupKey,
		UserDedupKey:    row.UserDedupKey,
		ExternalID:      row.ExternalID,
		Cron:            row.Cron,
		Timezone:        row.Timezone,
		ScheduleActive:  row.ScheduleActive,
		InstanceActive:  row.InstanceActive,
		Generation:      row.Generation,
		NextFireAt:      row.NextFireAt,
		LastFireAt:      row.LastFireAt,
		TriggerAttempts: row.TriggerAttemptCount,
		TriggerError:    row.TriggerErrorMessage,
		CreatedAt:       row.CreatedAt,
		UpdatedAt:       row.UpdatedAt,
	}
}

func updatedScheduleView(row db.UpdateScheduleRow) scheduleView {
	return scheduleView{
		ScheduleID:      row.ScheduleID,
		InstanceID:      row.InstanceID,
		ScheduleType:    row.ScheduleType,
		ProjectID:       row.ProjectID,
		EnvironmentID:   row.EnvironmentID,
		TaskID:          row.TaskID,
		DedupKey:        row.DedupKey,
		UserDedupKey:    row.UserDedupKey,
		ExternalID:      row.ExternalID,
		Cron:            row.Cron,
		Timezone:        row.Timezone,
		ScheduleActive:  row.ScheduleActive,
		InstanceActive:  row.InstanceActive,
		Generation:      row.Generation,
		NextFireAt:      row.NextFireAt,
		LastFireAt:      row.LastFireAt,
		TriggerAttempts: row.TriggerAttemptCount,
		TriggerError:    row.TriggerErrorMessage,
		CreatedAt:       row.CreatedAt,
		UpdatedAt:       row.UpdatedAt,
	}
}

func scheduleResponse(row scheduleView) api.ScheduleResponse {
	deduplicationKey := ""
	if row.ScheduleType == db.TaskScheduleTypeImperative {
		deduplicationKey = pgvalue.TextValue(row.UserDedupKey)
	}
	response := api.ScheduleResponse{
		ID:               pgvalue.MustUUIDValue(row.ScheduleID).String(),
		Type:             string(row.ScheduleType),
		ProjectID:        pgvalue.MustUUIDValue(row.ProjectID).String(),
		EnvironmentID:    pgvalue.MustUUIDValue(row.EnvironmentID).String(),
		Task:             row.TaskID,
		DeduplicationKey: deduplicationKey,
		ExternalID:       pgvalue.TextValue(row.ExternalID),
		Cron:             row.Cron,
		Timezone:         row.Timezone,
		Active:           row.ScheduleActive && row.InstanceActive,
		Status:           scheduleStatus(row),
		LastError:        row.TriggerError,
		CreatedAt:        pgvalue.Time(row.CreatedAt),
		UpdatedAt:        pgvalue.Time(row.UpdatedAt),
	}
	response.NextFireAt = pgvalue.TimePtr(row.NextFireAt)
	response.LastFireAt = pgvalue.TimePtr(row.LastFireAt)
	return response
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
