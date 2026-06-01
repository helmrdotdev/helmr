package control

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/ghapp"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/robfig/cron/v3"
)

const (
	scheduleListPageSize      = int32(200)
	defaultScheduleSweepEvery = 5 * time.Second
	defaultScheduleSweepLimit = int32(100)
	defaultScheduleFireLease  = 5 * time.Minute
	defaultScheduleJitter     = 30 * time.Second
)

var scheduleCronParser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)

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

func (s *Server) createScheduleForActor(ctx context.Context, actor auth.Actor, request api.CreateScheduleRequest) (db.CreateImperativeScheduleRow, error) {
	request.TaskID = strings.TrimSpace(request.TaskID)
	if err := api.ValidateTaskID(request.TaskID); err != nil {
		return db.CreateImperativeScheduleRow{}, err
	}
	dedupKey := strings.TrimSpace(request.ID)
	if dedupKey == "" {
		dedupKey = defaultScheduleDedupKey(request.TaskID, request.Cron)
	}
	if err := api.ValidateScheduleID(dedupKey); err != nil {
		return db.CreateImperativeScheduleRow{}, err
	}
	scope, projectID, environmentID, err := s.createRunRequestScope(ctx, actor, request.ProjectID, request.EnvironmentID)
	if err != nil {
		return db.CreateImperativeScheduleRow{}, err
	}
	if !actor.HasPermission(auth.PermissionRunsCreate, scope) {
		return db.CreateImperativeScheduleRow{}, errors.New("permission is required")
	}
	payload := request.Payload
	if len(payload) == 0 {
		payload = json.RawMessage(`{}`)
	}
	if !json.Valid(payload) {
		return db.CreateImperativeScheduleRow{}, errors.New("payload must be valid JSON")
	}
	if request.Secrets == nil {
		request.Secrets = api.SecretBindings{}
	}
	if err := secret.ValidateBindings(request.Secrets); err != nil {
		return db.CreateImperativeScheduleRow{}, err
	}
	if len(request.Secrets) > 0 && !actor.HasPermission(auth.PermissionSecretsUse, scope) {
		return db.CreateImperativeScheduleRow{}, errors.New("permission is required to bind secrets")
	}
	if len(request.Secrets) > 0 && s.secrets == nil {
		return db.CreateImperativeScheduleRow{}, errors.New("secret store is not configured")
	}
	if len(request.Secrets) > 0 {
		if err := s.secrets.CheckScoped(ctx, actor.OrgID, ids.MustFromPG(projectID), ids.MustFromPG(environmentID), request.Secrets); err != nil {
			return db.CreateImperativeScheduleRow{}, err
		}
	}
	workspace, err := ghapp.NormalizeSource(api.GitHubSource{
		Repository: request.Workspace.Repository,
		Ref:        request.Workspace.Ref,
		SHA:        request.Workspace.SHA,
		Subpath:    request.Workspace.Subpath,
	})
	if err != nil {
		return db.CreateImperativeScheduleRow{}, relabelGitHubSourceError(err, "workspace")
	}
	if _, err := s.db.GetActiveProjectGitHubRepositoryByFullName(ctx, db.GetActiveProjectGitHubRepositoryByFullNameParams{
		OrgID:     ids.ToPG(actor.OrgID),
		ProjectID: projectID,
		FullName:  workspace.Repository,
	}); errors.Is(err, pgx.ErrNoRows) {
		return db.CreateImperativeScheduleRow{}, relabelGitHubSourceError(ghapp.InvalidSourceError{Err: errors.New("github repository is not enabled for this project workspace")}, "workspace")
	} else if err != nil {
		return db.CreateImperativeScheduleRow{}, fmt.Errorf("authorize github workspace repository: %w", err)
	}
	deploymentSelection, err := normalizeRunDeploymentSelection(request.Options.DeploymentID, request.Options.Version)
	if err != nil {
		return db.CreateImperativeScheduleRow{}, err
	}
	deploymentTask, err := s.deploymentTaskForRunRequest(ctx, actor.OrgID, projectID, environmentID, request.TaskID, deploymentSelection)
	if errors.Is(err, pgx.ErrNoRows) {
		return db.CreateImperativeScheduleRow{}, fmt.Errorf("task %q is not deployed in the selected deployment", request.TaskID)
	}
	if err != nil {
		return db.CreateImperativeScheduleRow{}, err
	}
	if _, err := runMaxDurationSeconds(request.Options.MaxDurationSeconds, deploymentTask.MaxDurationSeconds); err != nil {
		return db.CreateImperativeScheduleRow{}, err
	}
	scheduling, err := s.resolveRunScheduling(request.Options, deploymentTask)
	if err != nil {
		return db.CreateImperativeScheduleRow{}, err
	}
	if _, err := s.validateRunQueueOverride(ctx, actor.OrgID, projectID, environmentID, request.Options, deploymentTask, scheduling); err != nil {
		return db.CreateImperativeScheduleRow{}, err
	}
	timezone := api.NormalizeTimezone(request.Timezone)
	loc, err := time.LoadLocation(timezone)
	if err != nil {
		return db.CreateImperativeScheduleRow{}, fmt.Errorf("timezone must be an IANA timezone")
	}
	spec, err := scheduleCronParser.Parse(strings.TrimSpace(request.Cron))
	if err != nil {
		return db.CreateImperativeScheduleRow{}, fmt.Errorf("cron must be a valid 5-field expression: %w", err)
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
		next := spec.Next(time.Now().In(loc)).UTC()
		nextScheduledAt = pgTimeToPG(next)
		nextDueAt = pgTimeToPG(next.Add(scheduleJitter(instanceID, defaultScheduleJitter)))
	}
	workspaceJSON, err := json.Marshal(api.ScheduleWorkspace{
		Repository: workspace.Repository,
		Ref:        workspace.Ref,
		SHA:        workspace.SHA,
		Subpath:    workspace.Subpath,
	})
	if err != nil {
		return db.CreateImperativeScheduleRow{}, err
	}
	secretBindingsJSON, err := json.Marshal(request.Secrets)
	if err != nil {
		return db.CreateImperativeScheduleRow{}, err
	}
	runOptionsJSON, err := json.Marshal(request.Options)
	if err != nil {
		return db.CreateImperativeScheduleRow{}, err
	}
	return s.db.CreateImperativeSchedule(ctx, db.CreateImperativeScheduleParams{
		ScheduleID:      ids.ToPG(scheduleID),
		OrgID:           ids.ToPG(actor.OrgID),
		ProjectID:       projectID,
		TaskID:          request.TaskID,
		DedupKey:        dedupKey,
		ExternalID:      pgText(strings.TrimSpace(request.ExternalID)),
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
		CatchUpPolicy:   db.TaskScheduleCatchUpPolicySkipToNext,
	})
}

func (s *Server) listSchedules(w http.ResponseWriter, r *http.Request) {
	actor := actorFromContext(r.Context())
	scope, projectID, environmentID, err := s.createRunRequestScope(r.Context(), actor, r.URL.Query().Get("project_id"), r.URL.Query().Get("environment_id"))
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
		next, err := nextCronTime(row.CronExpression, row.Timezone, time.Now())
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		nextScheduledAt = pgTimeToPG(next)
		nextDueAt = pgTimeToPG(next.Add(scheduleJitter(ids.MustFromPG(row.InstanceID), defaultScheduleJitter)))
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
	writeJSON(w, http.StatusOK, scheduleResponse(updateScheduleView(updated)))
}

func (s *Server) loadScheduleForRequest(w http.ResponseWriter, r *http.Request, permission auth.Permission) (db.GetScheduleSummaryRow, bool) {
	actor := actorFromContext(r.Context())
	scheduleID, err := ids.Parse(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, errors.New("schedule id must be a UUID"))
		return db.GetScheduleSummaryRow{}, false
	}
	scope, projectID, environmentID, err := s.createRunRequestScope(r.Context(), actor, r.URL.Query().Get("project_id"), r.URL.Query().Get("environment_id"))
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
	Type            db.TaskScheduleType
	TaskID          string
	DedupKey        string
	ExternalID      pgtype.Text
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

func createScheduleView(row db.CreateImperativeScheduleRow) scheduleView {
	return scheduleView{ScheduleID: row.ScheduleID, ProjectID: row.ProjectID, EnvironmentID: row.EnvironmentID, Type: row.Type, TaskID: row.TaskID, DedupKey: row.DedupKey, ExternalID: row.ExternalID, CronExpression: row.CronExpression, Timezone: row.Timezone, Payload: row.Payload, Workspace: row.Workspace, ScheduleActive: row.ScheduleActive, InstanceActive: row.InstanceActive, NextScheduledAt: row.NextScheduledAt, NextDueAt: row.NextDueAt, LastScheduledAt: row.LastScheduledAt, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt}
}

func listScheduleView(row db.ListScheduleSummariesRow) scheduleView {
	return scheduleView{ScheduleID: row.ScheduleID, ProjectID: row.ProjectID, EnvironmentID: row.EnvironmentID, Type: row.Type, TaskID: row.TaskID, DedupKey: row.DedupKey, ExternalID: row.ExternalID, CronExpression: row.CronExpression, Timezone: row.Timezone, Payload: row.Payload, Workspace: row.Workspace, ScheduleActive: row.ScheduleActive, InstanceActive: row.InstanceActive, NextScheduledAt: row.NextScheduledAt, NextDueAt: row.NextDueAt, LastScheduledAt: row.LastScheduledAt, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt}
}

func getScheduleView(row db.GetScheduleSummaryRow) scheduleView {
	return scheduleView{ScheduleID: row.ScheduleID, ProjectID: row.ProjectID, EnvironmentID: row.EnvironmentID, Type: row.Type, TaskID: row.TaskID, DedupKey: row.DedupKey, ExternalID: row.ExternalID, CronExpression: row.CronExpression, Timezone: row.Timezone, Payload: row.Payload, Workspace: row.Workspace, ScheduleActive: row.ScheduleActive, InstanceActive: row.InstanceActive, NextScheduledAt: row.NextScheduledAt, NextDueAt: row.NextDueAt, LastScheduledAt: row.LastScheduledAt, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt}
}

func updateScheduleView(row db.UpdateScheduleStateRow) scheduleView {
	return scheduleView{ScheduleID: row.ScheduleID, ProjectID: row.ProjectID, EnvironmentID: row.EnvironmentID, Type: row.Type, TaskID: row.TaskID, DedupKey: row.DedupKey, ExternalID: row.ExternalID, CronExpression: row.CronExpression, Timezone: row.Timezone, Payload: row.Payload, Workspace: row.Workspace, ScheduleActive: row.ScheduleActive, InstanceActive: row.InstanceActive, NextScheduledAt: row.NextScheduledAt, NextDueAt: row.NextDueAt, LastScheduledAt: row.LastScheduledAt, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt}
}

func scheduleResponse(row scheduleView) api.ScheduleResponse {
	response := api.ScheduleResponse{
		ID:            ids.MustFromPG(row.ScheduleID).String(),
		ProjectID:     apiKeyScopeID(row.ProjectID, auth.DefaultProjectID),
		EnvironmentID: apiKeyScopeID(row.EnvironmentID, auth.DefaultEnvironmentID),
		Type:          string(row.Type),
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
	if row.ExternalID.Valid {
		response.ExternalID = row.ExternalID.String
	}
	response.NextScheduledAt = pgTimePtr(row.NextScheduledAt)
	response.NextDueAt = pgTimePtr(row.NextDueAt)
	response.LastScheduledAt = pgTimePtr(row.LastScheduledAt)
	return response
}

type ScheduleWorker struct {
	log      *slog.Logger
	db       *db.Queries
	tx       txBeginner
	runner   *Server
	interval time.Duration
	limit    int32
	lease    time.Duration
	jitter   time.Duration
	now      func() time.Time
}

type ScheduleWorkerOption func(*ScheduleWorker)

func NewScheduleWorker(log *slog.Logger, database dbTXBeginner, resolver githubCommitResolver, secrets secretManager, enqueuer runEnqueuer, opts ...ScheduleWorkerOption) (*ScheduleWorker, error) {
	if log == nil {
		log = slog.Default()
	}
	if database == nil {
		return nil, errors.New("database is required")
	}
	queries := db.New(database)
	worker := &ScheduleWorker{
		log:      log,
		db:       queries,
		tx:       database,
		interval: defaultScheduleSweepEvery,
		limit:    defaultScheduleSweepLimit,
		lease:    defaultScheduleFireLease,
		jitter:   defaultScheduleJitter,
		now:      func() time.Time { return time.Now().UTC() },
	}
	worker.runner = &Server{
		log:         log,
		db:          queries,
		tx:          database,
		auth:        auth.NewDBAuthenticator(queries),
		github:      resolver,
		secrets:     secrets,
		runEnqueuer: enqueuer,
	}
	for _, opt := range opts {
		opt(worker)
	}
	if worker.interval <= 0 || worker.limit <= 0 || worker.lease <= 0 || worker.now == nil {
		return nil, errors.New("invalid schedule worker configuration")
	}
	return worker, nil
}

func (w *ScheduleWorker) Run(ctx context.Context) error {
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		if err := w.tick(ctx); err != nil && !errors.Is(err, context.Canceled) {
			w.log.Error("schedule worker tick failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (w *ScheduleWorker) tick(ctx context.Context) error {
	if err := w.materialize(ctx); err != nil {
		return err
	}
	return w.runFires(ctx)
}

func (w *ScheduleWorker) materialize(ctx context.Context) error {
	tx, err := w.tx.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	q := w.db.WithTx(tx)
	rows, err := q.ClaimDueScheduleInstances(ctx, w.limit)
	if err != nil {
		return err
	}
	now := w.now()
	for _, row := range rows {
		if !row.NextScheduledAt.Valid {
			continue
		}
		scheduledAt := row.NextScheduledAt.Time.UTC()
		if _, err := q.InsertScheduleFire(ctx, db.InsertScheduleFireParams{
			ScheduleInstanceID: row.InstanceID,
			ScheduledAt:        pgTimeToPG(scheduledAt),
			ScheduleID:         row.ScheduleID,
			OrgID:              row.OrgID,
			ProjectID:          row.ProjectID,
			EnvironmentID:      row.EnvironmentID,
			Generation:         row.Generation,
		}); err != nil {
			return err
		}
		anchor := scheduledAt
		if row.CatchUpPolicy == db.TaskScheduleCatchUpPolicySkipToNext && anchor.Before(now) {
			anchor = now
		}
		next, err := nextCronTime(row.CronExpression, row.Timezone, anchor)
		if err != nil {
			return err
		}
		if err := q.AdvanceScheduleInstance(ctx, db.AdvanceScheduleInstanceParams{
			NextScheduledAt: pgTimeToPG(next),
			NextDueAt:       pgTimeToPG(next.Add(scheduleJitter(ids.MustFromPG(row.InstanceID), w.jitter))),
			LastScheduledAt: pgTimeToPG(scheduledAt),
			InstanceID:      row.InstanceID,
			Generation:      row.Generation,
		}); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func (w *ScheduleWorker) runFires(ctx context.Context) error {
	leaseID := ids.New()
	rows, err := w.db.ClaimDueScheduleFires(ctx, db.ClaimDueScheduleFiresParams{
		LeaseID:        ids.ToPG(leaseID),
		LeaseExpiresAt: pgTimeToPG(w.now().Add(w.lease)),
		RowLimit:       w.limit,
	})
	if err != nil {
		return err
	}
	for _, row := range rows {
		if err := w.runFire(ctx, leaseID, row); err != nil {
			w.log.Error("schedule fire failed", "schedule_id", ids.MustFromPG(row.ScheduleID).String(), "error", err)
		}
	}
	return nil
}

func (w *ScheduleWorker) runFire(ctx context.Context, leaseID uuid.UUID, row db.ClaimDueScheduleFiresRow) error {
	request, err := scheduleRunRequest(row)
	if err != nil {
		return w.markFireFailed(ctx, leaseID, row, err)
	}
	scheduledAt := row.ScheduledAt.Time.UTC()
	request.Options.IdempotencyKey = fmt.Sprintf("schedule:%s:%s", ids.MustFromPG(row.ScheduleInstanceID), scheduledAt.Format(time.RFC3339Nano))
	request.Options.IdempotencyKeyTTL = "30d"
	run, _, err := w.runner.createRunFromRequest(ctx, auth.Actor{
		OrgID: ids.MustFromPG(row.OrgID),
		Kind:  auth.ActorKindSession,
		Role:  auth.RoleOwner,
	}, request, runSource{
		scheduleID:         row.ScheduleID,
		scheduleInstanceID: row.ScheduleInstanceID,
		scheduledAt:        row.ScheduledAt,
	})
	if err != nil {
		return w.markFireFailed(ctx, leaseID, row, err)
	}
	return w.db.MarkScheduleFireCreated(ctx, db.MarkScheduleFireCreatedParams{
		RunID:              run.ID,
		ScheduleInstanceID: row.ScheduleInstanceID,
		ScheduledAt:        row.ScheduledAt,
		LeaseID:            ids.ToPG(leaseID),
	})
}

func (w *ScheduleWorker) markFireFailed(ctx context.Context, leaseID uuid.UUID, row db.ClaimDueScheduleFiresRow, cause error) error {
	nextAttempt := w.now().Add(scheduleRetryDelay(row.AttemptCount))
	return w.db.MarkScheduleFireFailed(ctx, db.MarkScheduleFireFailedParams{
		ErrorMessage:       cause.Error(),
		NextAttemptAt:      pgTimeToPG(nextAttempt),
		ScheduleInstanceID: row.ScheduleInstanceID,
		ScheduledAt:        row.ScheduledAt,
		LeaseID:            ids.ToPG(leaseID),
	})
}

func scheduleRunRequest(row db.ClaimDueScheduleFiresRow) (api.CreateRunRequest, error) {
	var workspace api.ScheduleWorkspace
	if err := json.Unmarshal(row.Workspace, &workspace); err != nil {
		return api.CreateRunRequest{}, err
	}
	var options api.CreateRunOptions
	if len(row.RunOptions) > 0 {
		if err := json.Unmarshal(row.RunOptions, &options); err != nil {
			return api.CreateRunRequest{}, err
		}
	}
	var secrets api.SecretBindings
	if len(row.SecretBindings) > 0 {
		if err := json.Unmarshal(row.SecretBindings, &secrets); err != nil {
			return api.CreateRunRequest{}, err
		}
	}
	return api.CreateRunRequest{
		ProjectID:     ids.MustFromPG(row.ProjectID).String(),
		EnvironmentID: ids.MustFromPG(row.EnvironmentID).String(),
		TaskID:        row.TaskID,
		Secrets:       secrets,
		Payload:       append(json.RawMessage(nil), row.Payload...),
		Workspace: api.RunWorkspace{
			Repository: workspace.Repository,
			Ref:        workspace.Ref,
			SHA:        workspace.SHA,
			Subpath:    workspace.Subpath,
		},
		Options: options,
	}, nil
}

func nextCronTime(expression string, timezone string, anchor time.Time) (time.Time, error) {
	loc, err := time.LoadLocation(api.NormalizeTimezone(timezone))
	if err != nil {
		return time.Time{}, fmt.Errorf("timezone must be an IANA timezone")
	}
	spec, err := scheduleCronParser.Parse(strings.TrimSpace(expression))
	if err != nil {
		return time.Time{}, fmt.Errorf("cron must be a valid 5-field expression: %w", err)
	}
	return spec.Next(anchor.In(loc)).UTC(), nil
}

func scheduleJitter(id uuid.UUID, window time.Duration) time.Duration {
	if window <= 0 {
		return 0
	}
	sum := sha256.Sum256([]byte(id.String()))
	n := binary.BigEndian.Uint64(sum[:8])
	return time.Duration(n % uint64(window))
}

func scheduleRetryDelay(attempt int32) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	delay := time.Duration(attempt*attempt) * time.Minute
	if delay > time.Hour {
		return time.Hour
	}
	return delay
}

func defaultScheduleDedupKey(taskID string, expression string) string {
	sum := sha256.Sum256([]byte(taskID + "\n" + expression))
	return fmt.Sprintf("%s-%x", taskID, sum[:6])
}
