package control

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
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
	defaultRunMaxDurationSeconds = int32(900)
	minRunMaxDurationSeconds     = int32(5)
	maxRunDurationSeconds        = int32(86400)
	defaultIdempotencyKeyTTL     = 30 * 24 * time.Hour
	maxIdempotencyKeyLength      = 512
	maxRunLogSnapshotBytes       = int64(1 << 20)
	runEventsPageSize            = int32(200)
	runEventsFollowMaxDuration   = 30 * time.Minute
	runEventsFollowFallbackEvery = 15 * time.Second
)

var errIdempotencyKeyConflict = errors.New("idempotency_key was already used with different run parameters")

type githubCommitResolver interface {
	ResolveCommit(context.Context, int64, int64, api.GitHubSource) (ghapp.ResolvedSource, error)
	CreateRepositoryToken(context.Context, int64, int64) (ghapp.InstallationToken, error)
}

func (s *Server) createRun(w http.ResponseWriter, r *http.Request) {
	actor := actorFromContext(r.Context())
	var request api.CreateRunRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid run request JSON: %w", err))
		return
	}
	run, idempotencyHit, err := s.createRunFromRequest(r.Context(), actor, request, runSource{})
	if err != nil {
		if errors.Is(err, errIdempotencyKeyConflict) {
			writeError(w, http.StatusConflict, err)
			return
		}
		var upstreamErr createRunUpstreamError
		if errors.As(err, &upstreamErr) {
			writeError(w, http.StatusBadGateway, upstreamErr)
			return
		}
		if isCreateRunConfigError(err) {
			writeError(w, http.StatusServiceUnavailable, err)
			return
		}
		if errors.Is(err, errPermissionRequired) {
			writeError(w, http.StatusForbidden, err)
			return
		}
		var runDeploymentErr runDeploymentSelectionError
		if errors.As(err, &runDeploymentErr) {
			writeError(w, http.StatusBadRequest, runDeploymentErr)
			return
		}
		if isCreateRunClientError(err) {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		s.log.Error("create run failed", "error", err)
		writeError(w, http.StatusInternalServerError, errors.New("create run"))
		return
	}
	response, err := s.runResponse(r.Context(), run)
	if err != nil {
		s.log.Error("build run response failed", "error", err)
		writeError(w, http.StatusInternalServerError, errors.New("build run response"))
		return
	}
	if idempotencyHit {
		response.IdempotencyHit = true
		writeJSON(w, http.StatusOK, response)
		return
	}
	writeJSON(w, http.StatusCreated, response)
}

func isCreateRunConfigError(err error) bool {
	message := err.Error()
	return strings.Contains(message, "storage is not configured") ||
		strings.Contains(message, "resolver is not configured") ||
		strings.Contains(message, "secret store is not configured")
}

func isCreateRunClientError(err error) bool {
	message := err.Error()
	return errors.Is(err, pgx.ErrNoRows) ||
		strings.Contains(message, "must be") ||
		strings.Contains(message, "must match") ||
		strings.Contains(message, "cannot be") ||
		strings.Contains(message, "invalid") ||
		strings.Contains(message, "unsupported") ||
		strings.Contains(message, "exactly one") ||
		strings.Contains(message, "not deployed") ||
		strings.Contains(message, "not found") ||
		strings.Contains(message, "not enabled") ||
		strings.Contains(message, "not declared")
}

type runSource struct {
	scheduleID            pgtype.UUID
	scheduleInstanceID    pgtype.UUID
	scheduleGeneration    int64
	scheduleOrgID         pgtype.UUID
	scheduleProjectID     pgtype.UUID
	scheduleEnvironmentID pgtype.UUID
	scheduledAt           pgtype.Timestamptz
}

type createRunUpstreamError struct {
	err error
}

func (e createRunUpstreamError) Error() string {
	return e.err.Error()
}

func (e createRunUpstreamError) Unwrap() error {
	return e.err
}

func (s *Server) CreateScheduleRun(ctx context.Context, row db.GetScheduleTriggerCandidateRow) (pgtype.UUID, error) {
	request, err := schedule.RunRequestFromTriggerCandidate(row)
	if err != nil {
		return pgtype.UUID{}, err
	}
	request.Options.IdempotencyKey = schedule.TriggerIdempotencyKey(row.InstanceID, row.Generation, row.NextScheduledAt)
	request.Options.IdempotencyKeyTTL = schedule.TriggerIdempotencyKeyTTL
	run, _, err := s.createRunFromRequest(ctx, auth.Actor{
		OrgID: ids.MustFromPG(row.OrgID),
		Kind:  auth.ActorKindSystem,
		Role:  auth.RoleOwner,
	}, request, runSource{
		scheduleID:            row.ScheduleID,
		scheduleInstanceID:    row.InstanceID,
		scheduleGeneration:    row.Generation,
		scheduleOrgID:         row.OrgID,
		scheduleProjectID:     row.ProjectID,
		scheduleEnvironmentID: row.EnvironmentID,
		scheduledAt:           row.NextScheduledAt,
	})
	if err != nil {
		return pgtype.UUID{}, err
	}
	return run.ID, nil
}

func (s *Server) createRunFromRequest(ctx context.Context, actor auth.Actor, request api.CreateRunRequest, source runSource) (runSummary, bool, error) {
	if s.db == nil {
		return runSummary{}, false, errors.New("run storage is not configured")
	}
	request.TaskID = strings.TrimSpace(request.TaskID)
	if err := api.ValidateTaskID(request.TaskID); err != nil {
		return runSummary{}, false, err
	}
	deploymentSelection, err := normalizeRunDeploymentSelection(request.Options.DeploymentID, request.Options.Version)
	if err != nil {
		return runSummary{}, false, err
	}
	scope, projectID, environmentID, err := s.createRunRequestScope(ctx, actor, request.ProjectID, request.EnvironmentID)
	if err != nil {
		return runSummary{}, false, err
	}
	if !actor.HasPermission(auth.PermissionRunsCreate, scope) {
		return runSummary{}, false, errPermissionRequired
	}
	idempotency, err := normalizeRunIdempotency(request.Options)
	if err != nil {
		return runSummary{}, false, err
	}
	payload := request.Payload
	if len(payload) == 0 {
		payload = json.RawMessage(`{}`)
	}
	if !json.Valid(payload) {
		return runSummary{}, false, errors.New("payload must be valid JSON")
	}
	if request.Secrets == nil {
		request.Secrets = api.SecretBindings{}
	}
	if err := secret.ValidateBindings(request.Secrets); err != nil {
		return runSummary{}, false, err
	}
	if len(request.Secrets) > 0 && !actor.HasPermission(auth.PermissionSecretsUse, scope) {
		return runSummary{}, false, fmt.Errorf("%w to bind secrets", errPermissionRequired)
	}
	secretBindingsJSON, err := json.Marshal(request.Secrets)
	if err != nil {
		return runSummary{}, false, fmt.Errorf("secret bindings encode failed: %w", err)
	}

	if request.Options.MaxDurationSeconds != 0 {
		if _, err := runMaxDurationSeconds(request.Options.MaxDurationSeconds, defaultRunMaxDurationSeconds); err != nil {
			return runSummary{}, false, err
		}
	}
	idempotencyRequestHash := pgtype.Text{}
	if len(request.Secrets) > 0 && s.secrets == nil {
		return runSummary{}, false, errors.New("secret store is not configured")
	}
	if len(request.Secrets) > 0 {
		if err := s.secrets.CheckScoped(ctx, actor.OrgID, ids.MustFromPG(projectID), ids.MustFromPG(environmentID), request.Secrets); err != nil {
			return runSummary{}, false, err
		}
	}
	deploymentTask, err := s.deploymentTaskForRunRequest(ctx, actor.OrgID, projectID, environmentID, request.TaskID, deploymentSelection)
	if errors.Is(err, pgx.ErrNoRows) {
		return runSummary{}, false, fmt.Errorf("task %q is not deployed in the selected deployment", request.TaskID)
	}
	if err != nil {
		return runSummary{}, false, err
	}
	maxDurationSeconds, err := runMaxDurationSeconds(request.Options.MaxDurationSeconds, deploymentTask.MaxDurationSeconds)
	if err != nil {
		return runSummary{}, false, err
	}
	scheduling, err := s.resolveRunScheduling(request.Options, deploymentTask)
	if err != nil {
		return runSummary{}, false, err
	}
	if idempotency.key.Valid {
		idempotencyRequestHash, err = runIdempotencyRequestHash(request, payload, deploymentTask, maxDurationSeconds, scheduling)
		if err != nil {
			return runSummary{}, false, err
		}
		existing, hit, err := s.existingIdempotentRun(ctx, actor.OrgID, projectID, environmentID, request.TaskID, idempotency.key.String, idempotencyRequestHash.String, source, !source.scheduleInstanceID.Valid)
		if err != nil {
			return runSummary{}, false, err
		}
		if hit {
			current, err := s.scheduleRunSourceCurrent(ctx, source)
			if err != nil {
				return runSummary{}, false, err
			}
			if !current {
				return runSummary{}, false, schedule.ErrTriggerSuperseded
			}
			return existing, true, nil
		}
	}
	scheduling, err = s.validateRunQueueOverride(ctx, actor.OrgID, projectID, environmentID, request.Options, deploymentTask, scheduling)
	if err != nil {
		return runSummary{}, false, err
	}
	runID := ids.New()
	createdPayload, err := runCreatedEventPayload(request.TaskID, payload, maxDurationSeconds, request.Secrets)
	if err != nil {
		return runSummary{}, false, fmt.Errorf("encode run created event: %w", err)
	}
	run, err := s.db.CreateScopedRun(ctx, db.CreateScopedRunParams{
		ID:                      ids.ToPG(runID),
		OrgID:                   ids.ToPG(actor.OrgID),
		ProjectID:               projectID,
		EnvironmentID:           environmentID,
		DeploymentID:            deploymentTask.DeploymentID,
		DeploymentTaskID:        deploymentTask.ID,
		TaskID:                  request.TaskID,
		Payload:                 payload,
		SecretBindings:          secretBindingsJSON,
		IdempotencyKey:          idempotency.key,
		IdempotencyKeyExpiresAt: idempotency.expiresAt,
		IdempotencyKeyOptions:   idempotency.options,
		IdempotencyRequestHash:  idempotencyRequestHash,
		QueueName:               scheduling.queueName,
		QueueConcurrencyLimit:   scheduling.queueConcurrencyLimit,
		ConcurrencyKey:          scheduling.concurrencyKey,
		Priority:                scheduling.priority,
		QueueTimestamp:          scheduling.queueTimestamp,
		Ttl:                     scheduling.ttl,
		QueuedExpiresAt:         scheduling.queuedExpiresAt,
		MaxDurationSeconds:      maxDurationSeconds,
		EventPayload:            createdPayload,
		ScheduleID:              source.scheduleID,
		ScheduleInstanceID:      source.scheduleInstanceID,
		ScheduleGeneration:      pgtype.Int8{Int64: source.scheduleGeneration, Valid: source.scheduleInstanceID.Valid},
		ScheduledAt:             source.scheduledAt,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) && source.scheduleInstanceID.Valid {
			return runSummary{}, false, schedule.ErrTriggerSuperseded
		}
		if idempotency.key.Valid && isUniqueViolation(err) {
			existing, hit, lookupErr := s.existingIdempotentRun(ctx, actor.OrgID, projectID, environmentID, request.TaskID, idempotency.key.String, idempotencyRequestHash.String, source, !source.scheduleInstanceID.Valid)
			if lookupErr == nil && hit {
				current, currentErr := s.scheduleRunSourceCurrent(ctx, source)
				if currentErr != nil {
					return runSummary{}, false, currentErr
				}
				if !current {
					return runSummary{}, false, schedule.ErrTriggerSuperseded
				}
				return existing, true, nil
			}
			if lookupErr != nil {
				return runSummary{}, false, lookupErr
			}
		}
		return runSummary{}, false, err
	}
	if s.runEnqueuer != nil {
		if _, err := s.runEnqueuer.EnqueueRun(ctx, run.OrgID, run.ID); err != nil {
			s.log.Error("enqueue run queue item failed", "run_id", ids.MustFromPG(run.ID).String(), "error", err)
		}
	}
	return createScopedRunSummary(run), false, nil
}

func (s *Server) scheduleRunSourceCurrent(ctx context.Context, source runSource) (bool, error) {
	if !source.scheduleInstanceID.Valid {
		return true, nil
	}
	return s.db.ScheduleInstanceTriggerIsCurrent(ctx, db.ScheduleInstanceTriggerIsCurrentParams{
		InstanceID:    source.scheduleInstanceID,
		Generation:    source.scheduleGeneration,
		ScheduledAt:   source.scheduledAt,
		ScheduleID:    source.scheduleID,
		OrgID:         source.scheduleOrgID,
		ProjectID:     source.scheduleProjectID,
		EnvironmentID: source.scheduleEnvironmentID,
	})
}

type runDeploymentSelection struct {
	deploymentID pgtype.UUID
	version      string
}

func normalizeRunDeploymentSelection(deploymentID string, version string) (runDeploymentSelection, error) {
	deploymentID = strings.TrimSpace(deploymentID)
	version = strings.TrimSpace(version)
	if deploymentID != "" && version != "" {
		return runDeploymentSelection{}, errors.New("deployment_id and version cannot be combined")
	}
	if deploymentID != "" {
		parsedID, err := ids.Parse(deploymentID)
		if err != nil {
			return runDeploymentSelection{}, errors.New("deployment_id must be a UUID")
		}
		return runDeploymentSelection{deploymentID: ids.ToPG(parsedID)}, nil
	}
	return runDeploymentSelection{version: version}, nil
}

type runDeploymentSelectionError struct {
	err error
}

func (e runDeploymentSelectionError) Error() string {
	return e.err.Error()
}

func (e runDeploymentSelectionError) Unwrap() error {
	return e.err
}

func runDeploymentSelectionErrorf(format string, args ...any) error {
	return runDeploymentSelectionError{err: fmt.Errorf(format, args...)}
}

func (s *Server) deploymentTaskForRunRequest(ctx context.Context, orgID uuid.UUID, projectID pgtype.UUID, environmentID pgtype.UUID, taskID string, selection runDeploymentSelection) (db.GetDeploymentTaskRow, error) {
	deploymentID := selection.deploymentID
	if deploymentID.Valid {
		deployment, err := s.db.GetDeployment(ctx, db.GetDeploymentParams{
			OrgID:         ids.ToPG(orgID),
			ProjectID:     projectID,
			EnvironmentID: environmentID,
			ID:            deploymentID,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return db.GetDeploymentTaskRow{}, runDeploymentSelectionErrorf("deployment_id %s was not found in this environment", ids.MustFromPG(deploymentID).String())
		}
		if err != nil {
			return db.GetDeploymentTaskRow{}, err
		}
		if deployment.Status != db.DeploymentStatusDeployed {
			return db.GetDeploymentTaskRow{}, runDeploymentSelectionErrorf("deployment_id %s is not deployed", ids.MustFromPG(deploymentID).String())
		}
		return s.deploymentTask(ctx, orgID, projectID, environmentID, deployment.ID, taskID)
	}
	if selection.version != "" {
		deployment, err := s.db.GetDeploymentByVersion(ctx, db.GetDeploymentByVersionParams{
			OrgID:         ids.ToPG(orgID),
			ProjectID:     projectID,
			EnvironmentID: environmentID,
			Version:       selection.version,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return db.GetDeploymentTaskRow{}, runDeploymentSelectionErrorf("deployment version %q was not found in this environment", selection.version)
		}
		if err != nil {
			return db.GetDeploymentTaskRow{}, err
		}
		if deployment.Status != db.DeploymentStatusDeployed {
			return db.GetDeploymentTaskRow{}, runDeploymentSelectionErrorf("deployment version %q is not deployed", selection.version)
		}
		return s.deploymentTask(ctx, orgID, projectID, environmentID, deployment.ID, taskID)
	}
	task, err := s.db.GetCurrentDeploymentTask(ctx, db.GetCurrentDeploymentTaskParams{
		OrgID:         ids.ToPG(orgID),
		ProjectID:     projectID,
		EnvironmentID: environmentID,
		TaskID:        taskID,
	})
	if err != nil {
		return db.GetDeploymentTaskRow{}, err
	}
	return deploymentTaskRowFromCurrent(task), nil
}

func deploymentTaskRowFromCurrent(task db.GetCurrentDeploymentTaskRow) db.GetDeploymentTaskRow {
	return db.GetDeploymentTaskRow{
		ID:                     task.ID,
		OrgID:                  task.OrgID,
		ProjectID:              task.ProjectID,
		EnvironmentID:          task.EnvironmentID,
		DeploymentID:           task.DeploymentID,
		TaskID:                 task.TaskID,
		FilePath:               task.FilePath,
		ExportName:             task.ExportName,
		HandlerEntrypoint:      task.HandlerEntrypoint,
		BundleDigest:           task.BundleDigest,
		RequestedMilliCpu:      task.RequestedMilliCpu,
		RequestedMemoryMib:     task.RequestedMemoryMib,
		SecretDeclarations:     task.SecretDeclarations,
		ResourceRequirements:   task.ResourceRequirements,
		QueueName:              task.QueueName,
		QueueConcurrencyLimit:  task.QueueConcurrencyLimit,
		Ttl:                    task.Ttl,
		MaxDurationSeconds:     task.MaxDurationSeconds,
		CreatedAt:              task.CreatedAt,
		DeploymentSourceDigest: task.DeploymentSourceDigest,
	}
}

func (s *Server) deploymentTask(ctx context.Context, orgID uuid.UUID, projectID pgtype.UUID, environmentID pgtype.UUID, deploymentID pgtype.UUID, taskID string) (db.GetDeploymentTaskRow, error) {
	return s.db.GetDeploymentTask(ctx, db.GetDeploymentTaskParams{
		OrgID:         ids.ToPG(orgID),
		ProjectID:     projectID,
		EnvironmentID: environmentID,
		DeploymentID:  deploymentID,
		TaskID:        taskID,
	})
}

func (s *Server) createRunRequestScope(ctx context.Context, actor auth.Actor, projectID string, environmentID string) (auth.Scope, pgtype.UUID, pgtype.UUID, error) {
	return s.requestScopeForPermission(ctx, actor, projectID, environmentID, auth.PermissionRunsCreate, "run creation")
}

func (s *Server) requestScopeForPermission(ctx context.Context, actor auth.Actor, projectID string, environmentID string, permission auth.Permission, label string) (auth.Scope, pgtype.UUID, pgtype.UUID, error) {
	projectID = strings.TrimSpace(projectID)
	environmentID = strings.TrimSpace(environmentID)
	if actor.Kind != auth.ActorKindAPIKey || projectID != "" || environmentID != "" {
		return s.secretRequestScope(ctx, actor.OrgID, projectID, environmentID)
	}
	scope, err := inferAPIKeyPermissionScope(actor, permission, label)
	if err != nil {
		return auth.Scope{}, pgtype.UUID{}, pgtype.UUID{}, err
	}
	scopeProjectID, scopeEnvironmentID, err := s.runScopeIDs(ctx, actor.OrgID, scope)
	if err != nil {
		return auth.Scope{}, pgtype.UUID{}, pgtype.UUID{}, err
	}
	return scope, scopeProjectID, scopeEnvironmentID, nil
}

func inferAPIKeyCreateRunScope(actor auth.Actor) (auth.Scope, error) {
	return inferAPIKeyPermissionScope(actor, auth.PermissionRunsCreate, "run creation")
}

func inferAPIKeyPermissionScope(actor auth.Actor, permission auth.Permission, label string) (auth.Scope, error) {
	type scopeKey struct {
		projectID     string
		environmentID string
	}
	scopes := map[scopeKey]struct{}{}
	for _, grant := range actor.Permissions {
		if !permissionGrantIncludes(grant, permission) {
			continue
		}
		projectID, environmentID, ok := inferableAPIKeyRunScope(grant.ProjectID, grant.EnvironmentID)
		if !ok {
			continue
		}
		scopes[scopeKey{projectID: projectID, environmentID: environmentID}] = struct{}{}
	}
	if len(scopes) != 1 {
		return auth.Scope{}, fmt.Errorf("API key %s requires exactly one environment-scoped %s grant when project_id and environment_id are omitted", label, permission)
	}
	for scope := range scopes {
		return auth.Scope{OrgID: actor.OrgID, ProjectID: scope.projectID, EnvironmentID: scope.environmentID}, nil
	}
	return auth.Scope{}, fmt.Errorf("API key %s requires exactly one environment-scoped %s grant when project_id and environment_id are omitted", label, permission)
}

func permissionGrantIncludes(grant auth.PermissionGrant, permission auth.Permission) bool {
	for _, granted := range grant.Permissions {
		if granted == permission {
			return true
		}
	}
	return false
}

func inferableAPIKeyRunScope(projectValue string, environmentValue string) (string, string, bool) {
	projectValue = strings.TrimSpace(projectValue)
	environmentValue = strings.TrimSpace(environmentValue)
	if projectValue == "*" || environmentValue == "*" {
		return "", "", false
	}
	if (projectValue == "" || projectValue == auth.DefaultProjectID) &&
		(environmentValue == "" || environmentValue == auth.DefaultEnvironmentID) {
		return auth.DefaultProjectID, auth.DefaultEnvironmentID, true
	}
	if projectValue == "" || environmentValue == "" || projectValue == auth.DefaultProjectID || environmentValue == auth.DefaultEnvironmentID {
		return "", "", false
	}
	if _, err := ids.Parse(projectValue); err != nil {
		return "", "", false
	}
	if _, err := ids.Parse(environmentValue); err != nil {
		return "", "", false
	}
	return projectValue, environmentValue, true
}

func runCreatedEventPayload(taskID string, payload json.RawMessage, maxDurationSeconds int32, secrets api.SecretBindings) ([]byte, error) {
	secretNames := make([]string, 0, len(secrets))
	for name := range secrets {
		secretNames = append(secretNames, name)
	}
	sort.Strings(secretNames)
	return json.Marshal(map[string]any{
		"task_id":              taskID,
		"payload":              payload,
		"max_duration_seconds": maxDurationSeconds,
		"secret_names":         secretNames,
	})
}

func (s *Server) getRun(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("run storage is not configured"))
		return
	}
	runID, err := parseUUIDParam(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	run, err := s.db.GetRunSummary(r.Context(), db.GetRunSummaryParams{
		OrgID: ids.ToPG(actorFromContext(r.Context()).OrgID),
		ID:    ids.ToPG(runID),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, errors.New("run not found"))
		return
	}
	if err != nil {
		s.log.Error("get run failed", "run_id", runID.String(), "error", err)
		writeError(w, http.StatusInternalServerError, errors.New("get run"))
		return
	}
	summary := getRunSummary(run)
	actor := actorFromContext(r.Context())
	scope, err := s.runScope(r.Context(), actor.OrgID, summary)
	if err != nil {
		s.log.Error("resolve run scope failed", "run_id", runID.String(), "error", err)
		writeError(w, http.StatusInternalServerError, errors.New("get run"))
		return
	}
	if !actor.HasPermission(auth.PermissionRunsRead, scope) {
		writeError(w, http.StatusForbidden, errors.New("permission is required"))
		return
	}
	response, err := s.runResponse(r.Context(), summary)
	if err != nil {
		s.log.Error("get pending waitpoint failed", "run_id", runID.String(), "error", err)
		writeError(w, http.StatusInternalServerError, errors.New("get run"))
		return
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) listRuns(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("run storage is not configured"))
		return
	}
	statusFilter, limit, err := listRunsQuery(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	actor := actorFromContext(r.Context())
	summaries, err := s.listRunSummaries(r, actor, statusFilter, limit)
	if errors.Is(err, errPermissionRequired) {
		writeError(w, http.StatusForbidden, errors.New("permission is required"))
		return
	}
	if isScopeRequestError(err) {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err != nil {
		s.log.Error("list runs failed", "error", err)
		writeError(w, http.StatusInternalServerError, errors.New("list runs"))
		return
	}
	response := api.ListRunsResponse{Runs: make([]api.RunResponse, 0, len(summaries))}
	for _, run := range summaries {
		item, err := s.runResponse(r.Context(), run)
		if err != nil {
			s.log.Error("list pending waitpoint failed", "run_id", ids.MustFromPG(run.ID).String(), "error", err)
			writeError(w, http.StatusInternalServerError, errors.New("list runs"))
			return
		}
		response.Runs = append(response.Runs, item)
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) countRuns(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("run storage is not configured"))
		return
	}
	actor := actorFromContext(r.Context())
	counts, err := s.countRunStatuses(r, actor)
	if errors.Is(err, errPermissionRequired) {
		writeError(w, http.StatusForbidden, errors.New("permission is required"))
		return
	}
	if isScopeRequestError(err) {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err != nil {
		s.log.Error("count runs failed", "error", err)
		writeError(w, http.StatusInternalServerError, errors.New("count runs"))
		return
	}
	writeJSON(w, http.StatusOK, counts)
}

func (s *Server) getRunLogs(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("run storage is not configured"))
		return
	}
	runID, err := parseUUIDParam(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	actor := actorFromContext(r.Context())
	run, err := s.db.GetRunSummary(r.Context(), db.GetRunSummaryParams{OrgID: ids.ToPG(actor.OrgID), ID: ids.ToPG(runID)})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, errors.New("run not found"))
		return
	} else if err != nil {
		s.log.Error("get run before logs failed", "run_id", runID.String(), "error", err)
		writeError(w, http.StatusInternalServerError, errors.New("get run logs"))
		return
	}
	scope, err := s.runScope(r.Context(), actor.OrgID, getRunSummary(run))
	if err != nil {
		s.log.Error("resolve run scope before logs failed", "run_id", runID.String(), "error", err)
		writeError(w, http.StatusInternalServerError, errors.New("get run logs"))
		return
	}
	if !actor.HasPermission(auth.PermissionRunsRead, scope) {
		writeError(w, http.StatusForbidden, errors.New("permission is required"))
		return
	}
	logs, err := s.db.GetRunLogSnapshot(r.Context(), db.GetRunLogSnapshotParams{
		StdoutLimit: maxRunLogSnapshotBytes,
		StderrLimit: maxRunLogSnapshotBytes,
		OrgID:       ids.ToPG(actor.OrgID),
		RunID:       ids.ToPG(runID),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeJSON(w, http.StatusOK, api.LogSnapshotResponse{Cursor: "0:0"})
		return
	}
	if err != nil {
		s.log.Error("get run logs failed", "run_id", runID.String(), "error", err)
		writeError(w, http.StatusInternalServerError, errors.New("get run logs"))
		return
	}
	writeJSON(w, http.StatusOK, api.LogSnapshotResponse{
		StdoutBase64: base64.StdEncoding.EncodeToString(logs.Stdout),
		StderrBase64: base64.StdEncoding.EncodeToString(logs.Stderr),
		Cursor:       fmt.Sprintf("%d:%d", logs.StdoutCursor, logs.StderrCursor),
		Truncated:    logs.Truncated.Bool,
	})
}

func (s *Server) getRunEvents(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("run storage is not configured"))
		return
	}
	runID, err := parseUUIDParam(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	cursor, err := eventCursor(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	limit, err := eventLimit(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	actor := actorFromContext(r.Context())
	run, err := s.db.GetRunSummary(r.Context(), db.GetRunSummaryParams{OrgID: ids.ToPG(actor.OrgID), ID: ids.ToPG(runID)})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, errors.New("run not found"))
		return
	} else if err != nil {
		s.log.Error("get run before events failed", "run_id", runID.String(), "error", err)
		writeError(w, http.StatusInternalServerError, errors.New("list run events"))
		return
	}
	scope, err := s.runScope(r.Context(), actor.OrgID, getRunSummary(run))
	if err != nil {
		s.log.Error("resolve run scope before events failed", "run_id", runID.String(), "error", err)
		writeError(w, http.StatusInternalServerError, errors.New("list run events"))
		return
	}
	if !actor.HasPermission(auth.PermissionRunsRead, scope) {
		writeError(w, http.StatusForbidden, errors.New("permission is required"))
		return
	}
	if r.URL.Query().Get("follow") == "1" || strings.Contains(r.Header.Get("accept"), "text/event-stream") {
		s.followRunEvents(w, r, ids.ToPG(actor.OrgID), ids.ToPG(runID), cursor)
		return
	}
	rows, err := s.listRunEvents(r, ids.ToPG(actor.OrgID), ids.ToPG(runID), cursor, limit)
	if err != nil {
		s.log.Error("list run events failed", "run_id", runID.String(), "error", err)
		writeError(w, http.StatusInternalServerError, errors.New("list run events"))
		return
	}
	hasNext := len(rows) > int(limit)
	if hasNext {
		rows = rows[:limit]
	}
	events := make([]api.RunEvent, 0, len(rows))
	for _, row := range rows {
		events = append(events, runEventResponse(row))
	}
	var nextCursor *int64
	if hasNext {
		value := rows[len(rows)-1].ID
		nextCursor = &value
	}
	writeJSON(w, http.StatusOK, api.RunEventPage{Events: events, Cursor: cursor, NextCursor: nextCursor})
}

func (s *Server) listRunEvents(r *http.Request, orgID pgtype.UUID, runID pgtype.UUID, cursor int64, limit int32) ([]db.RunEvent, error) {
	return s.db.ListRunEvents(r.Context(), db.ListRunEventsParams{
		OrgID: orgID,
		RunID: runID,
		ID:    cursor,
		Limit: limit + 1,
	})
}

func (s *Server) listRunSummaries(r *http.Request, actor auth.Actor, statusFilter string, limit int32) ([]runSummary, error) {
	requestedScope, scopedQuery, err := s.requestedRunListScope(r, actor)
	if err != nil {
		return nil, err
	}
	if !actor.HasPermission(auth.PermissionRunsRead, requestedScope) {
		return nil, errPermissionRequired
	}
	if scopedQuery {
		projectID, environmentID, err := s.runScopeIDs(r.Context(), actor.OrgID, requestedScope)
		if err != nil {
			return nil, err
		}
		rows, err := s.db.ListScopedRunSummaries(r.Context(), db.ListScopedRunSummariesParams{
			OrgID:         ids.ToPG(actor.OrgID),
			ProjectID:     projectID,
			EnvironmentID: environmentID,
			StatusFilter:  statusFilter,
			RowLimit:      limit,
		})
		if err != nil {
			return nil, err
		}
		summaries := make([]runSummary, 0, len(rows))
		for _, row := range rows {
			summaries = append(summaries, listScopedRunSummary(row))
		}
		return summaries, nil
	}
	rows, err := s.db.ListRunSummaries(r.Context(), db.ListRunSummariesParams{
		OrgID:        ids.ToPG(actor.OrgID),
		StatusFilter: statusFilter,
		RowLimit:     limit,
	})
	if err != nil {
		return nil, err
	}
	summaries := make([]runSummary, 0, len(rows))
	for _, row := range rows {
		summaries = append(summaries, listRunSummary(row))
	}
	return summaries, nil
}

func (s *Server) countRunStatuses(r *http.Request, actor auth.Actor) (api.RunCountsResponse, error) {
	requestedScope, scopedQuery, err := s.requestedRunListScope(r, actor)
	if err != nil {
		return api.RunCountsResponse{}, err
	}
	if !actor.HasPermission(auth.PermissionRunsRead, requestedScope) {
		return api.RunCountsResponse{}, errPermissionRequired
	}
	if scopedQuery {
		projectID, environmentID, err := s.runScopeIDs(r.Context(), actor.OrgID, requestedScope)
		if err != nil {
			return api.RunCountsResponse{}, err
		}
		counts, err := s.db.CountScopedRunsByStatus(r.Context(), db.CountScopedRunsByStatusParams{
			OrgID:         ids.ToPG(actor.OrgID),
			ProjectID:     projectID,
			EnvironmentID: environmentID,
		})
		if err != nil {
			return api.RunCountsResponse{}, err
		}
		return scopedRunCountsResponse(counts), nil
	}
	counts, err := s.db.CountRunsByStatus(r.Context(), ids.ToPG(actor.OrgID))
	if err != nil {
		return api.RunCountsResponse{}, err
	}
	return runCountsResponse(counts), nil
}

var errPermissionRequired = errors.New("permission is required")

func isScopeRequestError(err error) bool {
	if err == nil {
		return false
	}
	message := err.Error()
	return strings.Contains(message, "project_id") || strings.Contains(message, "environment_id")
}

func (s *Server) requestedRunListScope(r *http.Request, actor auth.Actor) (auth.Scope, bool, error) {
	projectID := strings.TrimSpace(r.URL.Query().Get("project_id"))
	environmentID := strings.TrimSpace(r.URL.Query().Get("environment_id"))
	if projectID == "" && environmentID == "" {
		if actor.Kind == auth.ActorKindAPIKey {
			return auth.DefaultScope(actor.OrgID), true, nil
		}
		return auth.DefaultScope(actor.OrgID), false, nil
	}
	if projectID == "" || environmentID == "" {
		return auth.Scope{}, false, errors.New("project_id and environment_id must be provided together")
	}
	if projectID == auth.DefaultProjectID && environmentID == auth.DefaultEnvironmentID {
		return auth.DefaultScope(actor.OrgID), true, nil
	}
	scope, _, _, err := s.normalizeProjectEnvironmentScope(r.Context(), actor.OrgID, projectID, environmentID)
	if err != nil {
		return auth.Scope{}, false, err
	}
	return scope, true, nil
}

func (s *Server) runScopeIDs(ctx context.Context, orgID uuid.UUID, scope auth.Scope) (pgtype.UUID, pgtype.UUID, error) {
	if scope.ProjectID == auth.DefaultProjectID && scope.EnvironmentID == auth.DefaultEnvironmentID {
		defaultScope, err := s.db.GetDefaultProjectEnvironment(ctx, ids.ToPG(orgID))
		if err != nil {
			return pgtype.UUID{}, pgtype.UUID{}, err
		}
		return defaultScope.ProjectID, defaultScope.EnvironmentID, nil
	}
	projectID, err := ids.Parse(scope.ProjectID)
	if err != nil {
		return pgtype.UUID{}, pgtype.UUID{}, err
	}
	environmentID, err := ids.Parse(scope.EnvironmentID)
	if err != nil {
		return pgtype.UUID{}, pgtype.UUID{}, err
	}
	return ids.ToPG(projectID), ids.ToPG(environmentID), nil
}

func (s *Server) normalizeProjectEnvironmentScope(ctx context.Context, orgID uuid.UUID, projectID string, environmentID string) (auth.Scope, pgtype.UUID, pgtype.UUID, error) {
	if projectID == auth.DefaultProjectID && environmentID == auth.DefaultEnvironmentID {
		return auth.DefaultScope(orgID), pgtype.UUID{}, pgtype.UUID{}, nil
	}
	project, err := s.resolveProjectRef(ctx, orgID, projectID)
	if err != nil {
		return auth.Scope{}, pgtype.UUID{}, pgtype.UUID{}, err
	}
	environment, err := s.resolveEnvironmentRef(ctx, orgID, project.ID, environmentID)
	if err != nil {
		return auth.Scope{}, pgtype.UUID{}, pgtype.UUID{}, err
	}
	defaultScope, err := s.db.GetDefaultProjectEnvironment(ctx, ids.ToPG(orgID))
	if err != nil {
		return auth.Scope{}, pgtype.UUID{}, pgtype.UUID{}, fmt.Errorf("load default scope: %w", err)
	}
	if project.ID == defaultScope.ProjectID && environment.ID == defaultScope.EnvironmentID {
		return auth.DefaultScope(orgID), pgtype.UUID{}, pgtype.UUID{}, nil
	}
	return auth.Scope{OrgID: orgID, ProjectID: ids.MustFromPG(project.ID).String(), EnvironmentID: ids.MustFromPG(environment.ID).String()}, project.ID, environment.ID, nil
}

func (s *Server) resolveProjectRef(ctx context.Context, orgID uuid.UUID, projectRef string) (db.Project, error) {
	projectRef = strings.TrimSpace(projectRef)
	if projectRef == "" {
		projectRef = auth.DefaultProjectID
	}
	if projectRef == auth.DefaultProjectID {
		defaultScope, err := s.db.GetDefaultProjectEnvironment(ctx, ids.ToPG(orgID))
		if err != nil {
			return db.Project{}, fmt.Errorf("load default scope: %w", err)
		}
		return s.db.GetProject(ctx, db.GetProjectParams{OrgID: ids.ToPG(orgID), ID: defaultScope.ProjectID})
	}
	if parsed, err := ids.Parse(projectRef); err == nil {
		project, err := s.db.GetProject(ctx, db.GetProjectParams{OrgID: ids.ToPG(orgID), ID: ids.ToPG(parsed)})
		if errors.Is(err, pgx.ErrNoRows) {
			return db.Project{}, errors.New("project_id must reference an active project")
		}
		if err != nil {
			return db.Project{}, fmt.Errorf("load project: %w", err)
		}
		return project, nil
	}
	project, err := s.db.GetProjectBySlug(ctx, db.GetProjectBySlugParams{OrgID: ids.ToPG(orgID), Slug: strings.ToLower(projectRef)})
	if errors.Is(err, pgx.ErrNoRows) {
		return db.Project{}, errors.New("project_id must be \"default\", a project UUID, or a project slug")
	}
	if err != nil {
		return db.Project{}, fmt.Errorf("load project: %w", err)
	}
	return project, nil
}

func (s *Server) resolveEnvironmentRef(ctx context.Context, orgID uuid.UUID, projectID pgtype.UUID, environmentRef string) (db.Environment, error) {
	environmentRef = strings.TrimSpace(environmentRef)
	if environmentRef == "" {
		environmentRef = auth.DefaultEnvironmentID
	}
	if environmentRef == auth.DefaultEnvironmentID {
		environment, err := s.db.GetDefaultEnvironment(ctx, db.GetDefaultEnvironmentParams{OrgID: ids.ToPG(orgID), ProjectID: projectID})
		if errors.Is(err, pgx.ErrNoRows) {
			return db.Environment{}, errors.New("environment_id must reference an active environment")
		}
		if err != nil {
			return db.Environment{}, fmt.Errorf("load environment: %w", err)
		}
		return environment, nil
	}
	if parsed, err := ids.Parse(environmentRef); err == nil {
		environment, err := s.db.GetEnvironment(ctx, db.GetEnvironmentParams{OrgID: ids.ToPG(orgID), ProjectID: projectID, ID: ids.ToPG(parsed)})
		if errors.Is(err, pgx.ErrNoRows) {
			return db.Environment{}, errors.New("environment_id must reference an active environment")
		}
		if err != nil {
			return db.Environment{}, fmt.Errorf("load environment: %w", err)
		}
		return environment, nil
	}
	environment, err := s.db.GetEnvironmentBySlug(ctx, db.GetEnvironmentBySlugParams{OrgID: ids.ToPG(orgID), ProjectID: projectID, Slug: strings.ToLower(environmentRef)})
	if errors.Is(err, pgx.ErrNoRows) {
		return db.Environment{}, errors.New("environment_id must be \"default\", an environment UUID, or an environment slug")
	}
	if err != nil {
		return db.Environment{}, fmt.Errorf("load environment: %w", err)
	}
	return environment, nil
}

func eventCursor(r *http.Request) (int64, error) {
	value := strings.TrimSpace(r.Header.Get("Last-Event-ID"))
	if value == "" {
		value = strings.TrimSpace(r.URL.Query().Get("cursor"))
	}
	if value == "" {
		return 0, nil
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed < 0 {
		return 0, errors.New("cursor must be a non-negative integer")
	}
	return parsed, nil
}

func eventLimit(r *http.Request) (int32, error) {
	value := strings.TrimSpace(r.URL.Query().Get("limit"))
	if value == "" {
		return runEventsPageSize, nil
	}
	parsed, err := strconv.ParseInt(value, 10, 32)
	if err != nil || parsed < 1 || parsed > int64(runEventsPageSize) {
		return 0, fmt.Errorf("limit must be an integer between 1 and %d", runEventsPageSize)
	}
	return int32(parsed), nil
}

func (s *Server) followRunEvents(w http.ResponseWriter, r *http.Request, orgID pgtype.UUID, runID pgtype.UUID, cursor int64) {
	flusher, _ := w.(http.Flusher)
	w.Header().Set("content-type", "text/event-stream")
	w.Header().Set("cache-control", "no-cache")
	w.WriteHeader(http.StatusOK)
	encoder := json.NewEncoder(w)
	var events <-chan struct{} = make(chan struct{})
	unsubscribe := func() {}
	if s.runEvents != nil {
		events, unsubscribe = s.runEvents.SubscribeRunEvents(r.Context(), runID)
		defer unsubscribe()
	}
	fallback := time.NewTicker(runEventsFollowFallbackEvery)
	defer fallback.Stop()
	deadline := time.NewTimer(runEventsFollowMaxDuration)
	defer deadline.Stop()
	for {
		rows, err := s.listRunEvents(r, orgID, runID, cursor, runEventsPageSize)
		if err != nil {
			s.log.Warn("follow run events failed", "error", err)
			return
		}
		hasMore := int32(len(rows)) > runEventsPageSize
		if hasMore {
			rows = rows[:runEventsPageSize]
		}
		terminal := false
		for _, row := range rows {
			event := runEventResponse(row)
			cursor = row.ID
			terminal = terminal || runEventKindIsTerminal(row.Kind)
			_, _ = fmt.Fprintf(w, "id: %s\n", event.ID)
			_, _ = fmt.Fprint(w, "event: run_event\n")
			_, _ = fmt.Fprint(w, "data: ")
			_ = encoder.Encode(event)
			_, _ = fmt.Fprint(w, "\n")
		}
		if flusher != nil {
			flusher.Flush()
		}
		if terminal {
			return
		}
		if hasMore {
			continue
		}
		select {
		case <-r.Context().Done():
			return
		case <-deadline.C:
			return
		case <-events:
		case <-fallback.C:
			_, _ = fmt.Fprint(w, ": keep-alive\n\n")
			if flusher != nil {
				flusher.Flush()
			}
		}
	}
}

func runEventKindIsTerminal(kind string) bool {
	switch kind {
	case "run.completed", "run.failed", "run.cancelled", "run.expired":
		return true
	default:
		return false
	}
}

func runEventResponse(event db.RunEvent) api.RunEvent {
	runID := ids.MustFromPG(event.RunID).String()
	kind := "execution"
	if strings.HasPrefix(event.Kind, "emit.") {
		kind = "emit"
	}
	attributes := json.RawMessage(event.Payload)
	if len(attributes) == 0 || !json.Valid(attributes) {
		attributes = json.RawMessage(`{}`)
	}
	return api.RunEvent{
		ID:         strconv.FormatInt(event.ID, 10),
		RunID:      &runID,
		Kind:       kind,
		Message:    event.Kind,
		At:         pgTime(event.CreatedAt),
		Attributes: attributes,
	}
}

func listRunsQuery(r *http.Request) (string, int32, error) {
	status := strings.TrimSpace(r.URL.Query().Get("status"))
	if status == "" {
		status = "live"
	}
	switch status {
	case "all", "live", "queued", "running", "waiting", "succeeded", "failed", "cancelled", "expired":
	default:
		return "", 0, fmt.Errorf("status must be live, all, or a run status")
	}
	limit := int32(100)
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		parsed, err := strconv.ParseInt(raw, 10, 32)
		if err != nil || parsed < 1 || parsed > 200 {
			return "", 0, errors.New("limit must be an integer between 1 and 200")
		}
		limit = int32(parsed)
	}
	return status, limit, nil
}

func parseUUIDParam(r *http.Request, name string) (uuid.UUID, error) {
	id, err := ids.Parse(chi.URLParam(r, name))
	if err != nil {
		return uuid.Nil, fmt.Errorf("%s must be a UUID", name)
	}
	return id, nil
}

func runMaxDurationSeconds(value int32, defaultValue int32) (int32, error) {
	if value == 0 {
		value = defaultValue
	}
	if value == 0 {
		value = defaultRunMaxDurationSeconds
	}
	if value < minRunMaxDurationSeconds {
		return 0, fmt.Errorf("max_duration_seconds must be >= %d", minRunMaxDurationSeconds)
	}
	if value > maxRunDurationSeconds {
		return 0, fmt.Errorf("max_duration_seconds must be <= %d", maxRunDurationSeconds)
	}
	return value, nil
}

type runScheduling struct {
	queueName             string
	queueConcurrencyLimit pgtype.Int4
	concurrencyKey        pgtype.Text
	priority              int32
	queueTimestamp        pgtype.Timestamptz
	ttl                   string
	queuedExpiresAt       pgtype.Timestamptz
}

func (s *Server) resolveRunScheduling(options api.CreateRunOptions, task db.GetDeploymentTaskRow) (runScheduling, error) {
	now := time.Now().UTC()
	queueName := strings.TrimSpace(task.QueueName)
	queueLimit := task.QueueConcurrencyLimit
	if queueName == "" {
		queueName = "task/" + task.TaskID
	}
	if options.Queue != nil {
		queueName = strings.TrimSpace(options.Queue.Name)
		if queueName == "" {
			return runScheduling{}, errors.New("queue.name is required")
		}
		if err := api.ValidateQueueName(queueName); err != nil {
			return runScheduling{}, err
		}
	} else if err := api.ValidateQueueName(queueName); err != nil {
		return runScheduling{}, err
	}

	concurrencyKey := pgtype.Text{}
	if key := strings.TrimSpace(options.ConcurrencyKey); key != "" {
		if len(key) > 512 {
			return runScheduling{}, errors.New("concurrency_key must be 512 characters or less")
		}
		concurrencyKey = pgtype.Text{String: key, Valid: true}
	}

	ttl := strings.TrimSpace(options.TTL)
	if ttl == "" {
		ttl = strings.TrimSpace(task.Ttl)
	}
	queuedExpiresAt := pgtype.Timestamptz{}
	if ttl != "" {
		duration, err := parsePositiveDuration(ttl, "ttl")
		if err != nil {
			return runScheduling{}, err
		}
		queuedExpiresAt = pgtype.Timestamptz{Time: now.Add(duration), Valid: true}
	}

	return runScheduling{
		queueName:             queueName,
		queueConcurrencyLimit: queueLimit,
		concurrencyKey:        concurrencyKey,
		priority:              options.Priority,
		queueTimestamp:        pgtype.Timestamptz{Time: now, Valid: true},
		ttl:                   ttl,
		queuedExpiresAt:       queuedExpiresAt,
	}, nil
}

func (s *Server) validateRunQueueOverride(ctx context.Context, orgID uuid.UUID, projectID pgtype.UUID, environmentID pgtype.UUID, options api.CreateRunOptions, task db.GetDeploymentTaskRow, scheduling runScheduling) (runScheduling, error) {
	if options.Queue == nil {
		return scheduling, nil
	}
	queueConfig, err := s.db.GetDeploymentQueueConfig(ctx, db.GetDeploymentQueueConfigParams{
		OrgID:         ids.ToPG(orgID),
		ProjectID:     projectID,
		EnvironmentID: environmentID,
		DeploymentID:  task.DeploymentID,
		QueueName:     scheduling.queueName,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return runScheduling{}, fmt.Errorf("queue %q is not declared in the selected deployment", scheduling.queueName)
	}
	if err != nil {
		return runScheduling{}, err
	}
	scheduling.queueConcurrencyLimit = queueConfig.QueueConcurrencyLimit
	return scheduling, nil
}

type runIdempotency struct {
	key       pgtype.Text
	expiresAt pgtype.Timestamptz
	options   []byte
}

func normalizeRunIdempotency(options api.CreateRunOptions) (runIdempotency, error) {
	rawKey := strings.TrimSpace(options.IdempotencyKey)
	if rawKey == "" {
		if strings.TrimSpace(options.IdempotencyKeyTTL) != "" || len(options.IdempotencyKeyOptions) > 0 {
			return runIdempotency{}, errors.New("idempotency_key is required when idempotency options are set")
		}
		return runIdempotency{options: []byte(`{}`)}, nil
	}
	if len(rawKey) > maxIdempotencyKeyLength {
		return runIdempotency{}, fmt.Errorf("idempotency_key must be at most %d characters", maxIdempotencyKeyLength)
	}

	key := canonicalIdempotencyKey(rawKey)
	ttl, err := parseIdempotencyKeyTTL(options.IdempotencyKeyTTL)
	if err != nil {
		return runIdempotency{}, err
	}
	if ttl <= 0 {
		return runIdempotency{}, errors.New("idempotency_key_ttl must be positive")
	}
	idempotencyOptions := []byte(`{}`)
	if len(options.IdempotencyKeyOptions) > 0 {
		if !json.Valid(options.IdempotencyKeyOptions) {
			return runIdempotency{}, errors.New("idempotency_key_options must be valid JSON")
		}
		idempotencyOptions = append([]byte(nil), options.IdempotencyKeyOptions...)
	}
	return runIdempotency{
		key: pgtype.Text{
			String: key,
			Valid:  true,
		},
		expiresAt: pgtype.Timestamptz{
			Time:  time.Now().Add(ttl),
			Valid: true,
		},
		options: idempotencyOptions,
	}, nil
}

func canonicalIdempotencyKey(key string) string {
	digest := sha256.Sum256([]byte(key))
	return hex.EncodeToString(digest[:])
}

func runIdempotencyRequestHash(request api.CreateRunRequest, payload json.RawMessage, deploymentTask db.GetDeploymentTaskRow, maxDurationSeconds int32, scheduling runScheduling) (pgtype.Text, error) {
	canonicalPayload, err := canonicalJSON(payload)
	if err != nil {
		return pgtype.Text{}, fmt.Errorf("payload canonicalization failed: %w", err)
	}
	fingerprint := struct {
		TaskID     string             `json:"task_id"`
		Payload    json.RawMessage    `json:"payload"`
		Secrets    api.SecretBindings `json:"secrets"`
		Deployment struct {
			ID                 string `json:"id"`
			TaskID             string `json:"task_id"`
			BundleDigest       string `json:"bundle_digest,omitempty"`
			FilePath           string `json:"file_path,omitempty"`
			ExportName         string `json:"export_name,omitempty"`
			SourceDigest       string `json:"source_digest,omitempty"`
			MaxDurationSeconds int32  `json:"max_duration_seconds"`
		} `json:"deployment"`
		Scheduling struct {
			QueueName      string `json:"queue_name"`
			ConcurrencyKey string `json:"concurrency_key,omitempty"`
			Priority       int32  `json:"priority,omitempty"`
			TTL            string `json:"ttl,omitempty"`
		} `json:"options"`
	}{
		TaskID:  request.TaskID,
		Payload: canonicalPayload,
		Secrets: request.Secrets,
	}
	fingerprint.Deployment.ID = ids.MustFromPG(deploymentTask.DeploymentID).String()
	fingerprint.Deployment.TaskID = ids.MustFromPG(deploymentTask.ID).String()
	fingerprint.Deployment.BundleDigest = strings.TrimSpace(deploymentTask.BundleDigest)
	fingerprint.Deployment.FilePath = strings.TrimSpace(deploymentTask.FilePath)
	fingerprint.Deployment.ExportName = strings.TrimSpace(deploymentTask.ExportName)
	fingerprint.Deployment.SourceDigest = strings.TrimSpace(deploymentTask.DeploymentSourceDigest)
	fingerprint.Deployment.MaxDurationSeconds = maxDurationSeconds
	fingerprint.Scheduling.QueueName = scheduling.queueName
	if scheduling.concurrencyKey.Valid {
		fingerprint.Scheduling.ConcurrencyKey = scheduling.concurrencyKey.String
	}
	fingerprint.Scheduling.Priority = scheduling.priority
	fingerprint.Scheduling.TTL = scheduling.ttl

	body, err := json.Marshal(fingerprint)
	if err != nil {
		return pgtype.Text{}, fmt.Errorf("idempotency request fingerprint encode failed: %w", err)
	}
	digest := sha256.Sum256(body)
	return pgtype.Text{String: hex.EncodeToString(digest[:]), Valid: true}, nil
}

func canonicalJSON(raw json.RawMessage) (json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	canonical, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(canonical), nil
}

func parseIdempotencyKeyTTL(raw string) (time.Duration, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return defaultIdempotencyKeyTTL, nil
	}
	return parsePositiveDuration(raw, "idempotency_key_ttl")
}

func parsePositiveDuration(raw string, label string) (time.Duration, error) {
	return api.ParsePositiveDuration(raw, label)
}

func (s *Server) existingIdempotentRun(ctx context.Context, orgID uuid.UUID, projectID pgtype.UUID, environmentID pgtype.UUID, taskID string, key string, requestHash string, source runSource, allowTerminalClear bool) (runSummary, bool, error) {
	existing, err := s.db.GetScopedRunByIdempotencyKey(ctx, db.GetScopedRunByIdempotencyKeyParams{
		OrgID:          ids.ToPG(orgID),
		ProjectID:      projectID,
		EnvironmentID:  environmentID,
		TaskID:         taskID,
		IdempotencyKey: pgtype.Text{String: key, Valid: true},
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return runSummary{}, false, nil
	}
	if err != nil {
		return runSummary{}, false, err
	}
	expired := existing.IdempotencyKeyExpiresAt.Valid && !time.Now().Before(existing.IdempotencyKeyExpiresAt.Time)
	if allowTerminalClear && (existing.Status == db.RunStatusFailed || existing.Status == db.RunStatusExpired || (expired && isTerminalRunStatus(existing.Status))) {
		if err := s.db.ClearRunIdempotencyKey(ctx, db.ClearRunIdempotencyKeyParams{
			OrgID:         ids.ToPG(orgID),
			ProjectID:     projectID,
			EnvironmentID: environmentID,
			ID:            existing.ID,
		}); err != nil {
			return runSummary{}, false, err
		}
		return runSummary{}, false, nil
	}
	if source.scheduleInstanceID.Valid && !idempotentRunMatchesScheduleSource(existing, source) {
		return runSummary{}, false, errIdempotencyKeyConflict
	}
	if existing.IdempotencyRequestHash.Valid && existing.IdempotencyRequestHash.String != requestHash && !source.scheduleInstanceID.Valid {
		return runSummary{}, false, errIdempotencyKeyConflict
	}
	return idempotentRunSummary(existing), true, nil
}

func idempotentRunMatchesScheduleSource(run db.GetScopedRunByIdempotencyKeyRow, source runSource) bool {
	return run.ScheduleID == source.scheduleID &&
		run.ScheduleInstanceID == source.scheduleInstanceID &&
		run.ScheduledAt.Valid == source.scheduledAt.Valid &&
		(!run.ScheduledAt.Valid || run.ScheduledAt.Time.UTC().Equal(source.scheduledAt.Time.UTC()))
}

func isTerminalRunStatus(status db.RunStatus) bool {
	return status == db.RunStatusSucceeded || status == db.RunStatusFailed || status == db.RunStatusCancelled || status == db.RunStatusExpired
}

type runSummary struct {
	ID               pgtype.UUID
	OrgID            pgtype.UUID
	ProjectID        pgtype.UUID
	EnvironmentID    pgtype.UUID
	DeploymentID     pgtype.UUID
	DeploymentTaskID pgtype.UUID
	TaskID           string
	Status           db.RunStatus
	ExitCode         pgtype.Int4
	Output           []byte
	CreatedAt        pgtype.Timestamptz
	UpdatedAt        pgtype.Timestamptz
}

func idempotentRunSummary(run db.GetScopedRunByIdempotencyKeyRow) runSummary {
	return runSummary{
		ID:               run.ID,
		OrgID:            run.OrgID,
		ProjectID:        run.ProjectID,
		EnvironmentID:    run.EnvironmentID,
		DeploymentID:     run.DeploymentID,
		DeploymentTaskID: run.DeploymentTaskID,
		TaskID:           run.TaskID,
		Status:           run.Status,
		ExitCode:         run.ExitCode,
		Output:           run.Output,
		CreatedAt:        run.CreatedAt,
		UpdatedAt:        run.UpdatedAt,
	}
}

func createScopedRunSummary(run db.CreateScopedRunRow) runSummary {
	return runSummary{
		ID:               run.ID,
		OrgID:            run.OrgID,
		ProjectID:        run.ProjectID,
		EnvironmentID:    run.EnvironmentID,
		DeploymentID:     run.DeploymentID,
		DeploymentTaskID: run.DeploymentTaskID,
		TaskID:           run.TaskID,
		Status:           run.Status,
		ExitCode:         run.ExitCode,
		Output:           run.Output,
		CreatedAt:        run.CreatedAt,
		UpdatedAt:        run.UpdatedAt,
	}
}

func getRunSummary(run db.GetRunSummaryRow) runSummary {
	return runSummary{
		ID:               run.ID,
		OrgID:            run.OrgID,
		ProjectID:        run.ProjectID,
		EnvironmentID:    run.EnvironmentID,
		DeploymentID:     run.DeploymentID,
		DeploymentTaskID: run.DeploymentTaskID,
		TaskID:           run.TaskID,
		Status:           run.Status,
		ExitCode:         run.ExitCode,
		Output:           run.Output,
		CreatedAt:        run.CreatedAt,
		UpdatedAt:        run.UpdatedAt,
	}
}

func listRunSummary(run db.ListRunSummariesRow) runSummary {
	return runSummary{
		ID:               run.ID,
		OrgID:            run.OrgID,
		ProjectID:        run.ProjectID,
		EnvironmentID:    run.EnvironmentID,
		DeploymentID:     run.DeploymentID,
		DeploymentTaskID: run.DeploymentTaskID,
		TaskID:           run.TaskID,
		Status:           run.Status,
		ExitCode:         run.ExitCode,
		Output:           run.Output,
		CreatedAt:        run.CreatedAt,
		UpdatedAt:        run.UpdatedAt,
	}
}

func listScopedRunSummary(run db.ListScopedRunSummariesRow) runSummary {
	return runSummary{
		ID:               run.ID,
		OrgID:            run.OrgID,
		ProjectID:        run.ProjectID,
		EnvironmentID:    run.EnvironmentID,
		DeploymentID:     run.DeploymentID,
		DeploymentTaskID: run.DeploymentTaskID,
		TaskID:           run.TaskID,
		Status:           run.Status,
		ExitCode:         run.ExitCode,
		Output:           run.Output,
		CreatedAt:        run.CreatedAt,
		UpdatedAt:        run.UpdatedAt,
	}
}

func runCountsResponse(counts db.CountRunsByStatusRow) api.RunCountsResponse {
	return api.RunCountsResponse{
		Queued:    counts.Queued,
		Running:   counts.Running,
		Waiting:   counts.Waiting,
		Succeeded: counts.Succeeded,
		Failed:    counts.Failed,
		Cancelled: counts.Cancelled,
		Expired:   counts.Expired,
	}
}

func scopedRunCountsResponse(counts db.CountScopedRunsByStatusRow) api.RunCountsResponse {
	return api.RunCountsResponse{
		Queued:    counts.Queued,
		Running:   counts.Running,
		Waiting:   counts.Waiting,
		Succeeded: counts.Succeeded,
		Failed:    counts.Failed,
		Cancelled: counts.Cancelled,
		Expired:   counts.Expired,
	}
}

func runResponse(run runSummary) api.RunResponse {
	runID := ids.MustFromPG(run.ID)
	var exitCode *int32
	if run.ExitCode.Valid {
		exitCode = &run.ExitCode.Int32
	}
	var output json.RawMessage
	if len(run.Output) > 0 {
		output = append(json.RawMessage(nil), run.Output...)
	}
	return api.RunResponse{
		ID:               runID.String(),
		ProjectID:        apiKeyScopeID(run.ProjectID, auth.DefaultProjectID),
		EnvironmentID:    apiKeyScopeID(run.EnvironmentID, auth.DefaultEnvironmentID),
		DeploymentID:     ids.MustFromPG(run.DeploymentID).String(),
		DeploymentTaskID: ids.MustFromPG(run.DeploymentTaskID).String(),
		TaskID:           run.TaskID,
		Status:           publicRunStatus(run.Status),
		ExitCode:         exitCode,
		Output:           output,
		CreatedAt:        pgTime(run.CreatedAt),
		UpdatedAt:        pgTime(run.UpdatedAt),
	}
}

func publicRunStatus(status db.RunStatus) string {
	return string(status)
}

func (s *Server) runScope(ctx context.Context, orgID uuid.UUID, run runSummary) (auth.Scope, error) {
	scope := auth.Scope{
		OrgID:         orgID,
		ProjectID:     apiKeyScopeID(run.ProjectID, auth.DefaultProjectID),
		EnvironmentID: apiKeyScopeID(run.EnvironmentID, auth.DefaultEnvironmentID),
	}
	defaultScope, err := s.db.GetDefaultProjectEnvironment(ctx, ids.ToPG(orgID))
	if err != nil {
		return auth.Scope{}, err
	}
	if run.ProjectID == defaultScope.ProjectID && run.EnvironmentID == defaultScope.EnvironmentID {
		return auth.DefaultScope(orgID), nil
	}
	return scope, nil
}

func (s *Server) runResponse(ctx context.Context, run runSummary) (api.RunResponse, error) {
	response := runResponse(run)
	if run.Status != db.RunStatusWaiting {
		return response, nil
	}
	waitpoint, err := s.db.GetPendingWaitpointForRun(ctx, db.GetPendingWaitpointForRunParams{
		OrgID: run.OrgID,
		RunID: run.ID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return response, nil
	}
	if err != nil {
		return api.RunResponse{}, err
	}
	pending, err := pendingWaitpointResponse(pendingWaitpointView(waitpoint))
	if err != nil {
		return api.RunResponse{}, err
	}
	deliveries, err := s.db.ListWaitpointDeliveries(ctx, db.ListWaitpointDeliveriesParams{
		OrgID:       waitpoint.OrgID,
		RunID:       waitpoint.RunID,
		RunWaitID:   waitpoint.RunWaitID,
		WaitpointID: waitpoint.ID,
	})
	if err != nil {
		return api.RunResponse{}, err
	}
	pending.Deliveries = make([]api.WaitpointDeliveryResponse, 0, len(deliveries))
	for _, delivery := range deliveries {
		pending.Deliveries = append(pending.Deliveries, waitpointDeliveryResponse(delivery))
	}
	response.PendingWaitpoint = &pending
	return response, nil
}

func pendingWaitpointResponse(waitpoint waitpointView) (api.PendingWaitpoint, error) {
	response := api.PendingWaitpoint{
		Kind:        string(waitpoint.Kind),
		WaitpointID: ids.MustFromPG(waitpoint.ID).String(),
		Request:     waitpoint.Request,
		DisplayText: waitpoint.DisplayText,
		RequestedAt: pgTime(waitpoint.RequestedAt),
	}
	if waitpoint.TimeoutSeconds.Valid {
		response.Timeout = &waitpoint.TimeoutSeconds.Int32
	}
	if waitpoint.PolicyName.Valid {
		policy := waitpoint.PolicyName.String
		response.Policy = &policy
	}
	switch waitpoint.Kind {
	case db.WaitpointKindManual, db.WaitpointKindDelay:
	default:
		return api.PendingWaitpoint{}, fmt.Errorf("unsupported waitpoint kind %q", waitpoint.Kind)
	}
	return response, nil
}

func pendingWaitpointView(waitpoint db.GetPendingWaitpointForRunRow) waitpointView {
	return waitpointView{
		ID:             waitpoint.ID,
		RunWaitID:      waitpoint.RunWaitID,
		OrgID:          waitpoint.OrgID,
		RunID:          waitpoint.RunID,
		ExecutionID:    waitpoint.ExecutionID,
		CheckpointID:   waitpoint.CheckpointID,
		CorrelationID:  waitpoint.CorrelationID,
		Kind:           waitpoint.Kind,
		Request:        waitpoint.Request,
		DisplayText:    waitpoint.DisplayText,
		TimeoutSeconds: waitpoint.TimeoutSeconds,
		PolicyName:     waitpoint.PolicyName,
		PolicySnapshot: waitpoint.PolicySnapshot,
		Status:         waitpoint.Status,
		ResolutionKind: waitpoint.ResolutionKind,
		Resolution:     waitpoint.Resolution,
		CreatedAt:      waitpoint.CreatedAt,
		RequestedAt:    waitpoint.RequestedAt,
		ResolvedAt:     waitpoint.ResolvedAt,
	}
}

func waitpointDeliveryResponse(delivery db.WaitpointDelivery) api.WaitpointDeliveryResponse {
	var lastError *string
	if delivery.LastError.Valid {
		lastError = &delivery.LastError.String
	}
	var sentAt *time.Time
	if delivery.SentAt.Valid {
		value := pgTime(delivery.SentAt)
		sentAt = &value
	}
	return api.WaitpointDeliveryResponse{
		ID:            ids.MustFromPG(delivery.ID).String(),
		Channel:       delivery.Channel,
		RecipientKind: delivery.RecipientKind,
		Recipient:     delivery.Recipient,
		Status:        string(delivery.Status),
		LastError:     lastError,
		SentAt:        sentAt,
		CreatedAt:     pgTime(delivery.CreatedAt),
		UpdatedAt:     pgTime(delivery.UpdatedAt),
	}
}

func pgTime(value pgtype.Timestamptz) time.Time {
	if !value.Valid {
		return time.Time{}
	}
	return value.Time
}

func pgTimeToPG(value time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: value, Valid: true}
}
