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
	"math"
	"net/http"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/schedule"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/helmrdotdev/helmr/internal/tracing"
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
)

var errIdempotencyKeyConflict = errors.New("idempotency_key was already used with different run parameters")

func (s *Server) createRun(w http.ResponseWriter, r *http.Request) {
	actor := actorFromContext(r.Context())
	var request api.CreateRunRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid run request JSON: %w", err))
		return
	}
	projectID, environmentID, err := environmentScopeRefsFromRequest(r, actor, request.ProjectID, request.EnvironmentID)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	request.ProjectID = projectID
	request.EnvironmentID = environmentID
	run, idempotencyHit, err := s.createRunFromRequest(contextWithRequestVersionMetadata(r.Context(), r), actor, request, runSource{})
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
		strings.Contains(message, "not accepted") ||
		strings.Contains(message, "not bound") ||
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
	replayedFromRunID     pgtype.UUID
	replayOperationID     pgtype.UUID
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
	request.Options.IdempotencyKey = schedule.TriggerIdempotencyKey(row.InstanceID, row.Generation, row.NextFireAt)
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
		scheduledAt:           row.NextFireAt,
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
	scope, projectID, environmentID, err := s.requestEnvironmentScope(ctx, actor, request.ProjectID, request.EnvironmentID)
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
	if request.Options.MaxDurationSeconds != 0 {
		if _, err := runMaxDurationSeconds(request.Options.MaxDurationSeconds, defaultRunMaxDurationSeconds); err != nil {
			return runSummary{}, false, err
		}
	}
	idempotencyRequestHash := pgtype.Text{}
	deploymentTask, err := s.deploymentTaskForRunRequest(ctx, actor.OrgID, projectID, environmentID, request.TaskID, deploymentSelection)
	if errors.Is(err, pgx.ErrNoRows) {
		return runSummary{}, false, fmt.Errorf("task %q is not deployed in the selected deployment", request.TaskID)
	}
	if err != nil {
		return runSummary{}, false, err
	}
	secretNames, err := deploymentTaskSecretNames(deploymentTask.SecretDeclarations)
	if err != nil {
		return runSummary{}, false, err
	}
	if len(secretNames) > 0 {
		if s.secrets == nil {
			return runSummary{}, false, errors.New("secret store is not configured")
		}
		if err := s.secrets.CheckScopedNames(ctx, actor.OrgID, ids.MustFromPG(projectID), ids.MustFromPG(environmentID), secretNames); err != nil {
			return runSummary{}, false, err
		}
	}
	maxDurationSeconds, err := runMaxDurationSeconds(request.Options.MaxDurationSeconds, deploymentTask.MaxDurationSeconds)
	if err != nil {
		return runSummary{}, false, err
	}
	lockedRetryPolicy, err := resolvedRetryPolicy(request.Options.Retry, deploymentTask.RetryPolicy)
	if err != nil {
		return runSummary{}, false, err
	}
	metadata, err := normalizedJSONObject(request.Options.Metadata, "metadata")
	if err != nil {
		return runSummary{}, false, err
	}
	tags, err := normalizedRunTags(request.Options.Tags)
	if err != nil {
		return runSummary{}, false, err
	}
	scheduling, err := s.resolveRunScheduling(request.Options, deploymentTask)
	if err != nil {
		return runSummary{}, false, err
	}
	if idempotency.key.Valid {
		idempotencyRequestHash, err = runIdempotencyRequestHash(request, payload, deploymentTask, maxDurationSeconds, lockedRetryPolicy, metadata, tags, scheduling)
		if err != nil {
			return runSummary{}, false, err
		}
		existing, hit, err := s.existingIdempotentRun(ctx, actor.OrgID, projectID, environmentID, request.TaskID, idempotency.key.String, idempotencyRequestHash.String, source, !source.scheduleInstanceID.Valid && !source.replayOperationID.Valid)
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
	versionMetadata := requestVersionMetadataFromContext(ctx)
	runID := ids.New()
	traceID, err := tracing.NewTraceID()
	if err != nil {
		return runSummary{}, false, fmt.Errorf("generate run trace id: %w", err)
	}
	rootSpanID, err := tracing.NewSpanID()
	if err != nil {
		return runSummary{}, false, fmt.Errorf("generate run root span id: %w", err)
	}
	createdPayload, err := runCreatedEventPayload(request.TaskID, payload, maxDurationSeconds, secretNames, lockedRetryPolicy, metadata, tags)
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
		DeploymentVersion:       deploymentTask.DeploymentVersion,
		ApiVersion:              versionMetadata.APIVersion,
		SdkVersion:              firstNonEmptyString(versionMetadata.SDKVersion, deploymentTask.SdkVersion),
		CliVersion:              firstNonEmptyString(versionMetadata.CLIVersion, deploymentTask.CliVersion),
		TaskID:                  request.TaskID,
		Payload:                 payload,
		Metadata:                metadata,
		Tags:                    tags,
		IdempotencyKey:          idempotency.key,
		IdempotencyKeyExpiresAt: idempotency.expiresAt,
		IdempotencyKeyOptions:   idempotency.options,
		IdempotencyRequestHash:  idempotencyRequestHash,
		LockedRetryPolicy:       lockedRetryPolicy,
		ReplayedFromRunID:       source.replayedFromRunID,
		ReplayOperationID:       source.replayOperationID,
		QueueName:               scheduling.queueName,
		QueueConcurrencyLimit:   scheduling.queueConcurrencyLimit,
		ConcurrencyKey:          scheduling.concurrencyKey,
		Priority:                scheduling.priority,
		QueueTimestamp:          scheduling.queueTimestamp,
		Ttl:                     scheduling.ttl,
		QueuedExpiresAt:         scheduling.queuedExpiresAt,
		MaxDurationSeconds:      maxDurationSeconds,
		TraceID:                 traceID,
		RootSpanID:              rootSpanID,
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
			existing, hit, lookupErr := s.existingIdempotentRun(ctx, actor.OrgID, projectID, environmentID, request.TaskID, idempotency.key.String, idempotencyRequestHash.String, source, !source.scheduleInstanceID.Valid && !source.replayOperationID.Valid)
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
		DeploymentVersion:      task.DeploymentVersion,
		ApiVersion:             task.ApiVersion,
		SdkVersion:             task.SdkVersion,
		CliVersion:             task.CliVersion,
		TaskID:                 task.TaskID,
		FilePath:               task.FilePath,
		ExportName:             task.ExportName,
		HandlerEntrypoint:      task.HandlerEntrypoint,
		BundleDigest:           task.BundleDigest,
		BundleFormatVersion:    task.BundleFormatVersion,
		RequestedMilliCpu:      task.RequestedMilliCpu,
		RequestedMemoryMib:     task.RequestedMemoryMib,
		SecretDeclarations:     task.SecretDeclarations,
		ResourceRequirements:   task.ResourceRequirements,
		QueueName:              task.QueueName,
		QueueConcurrencyLimit:  task.QueueConcurrencyLimit,
		Ttl:                    task.Ttl,
		MaxDurationSeconds:     task.MaxDurationSeconds,
		RetryPolicy:            task.RetryPolicy,
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

func (s *Server) requestEnvironmentScope(ctx context.Context, actor auth.Actor, projectID string, environmentID string) (auth.Scope, pgtype.UUID, pgtype.UUID, error) {
	projectID = strings.TrimSpace(projectID)
	environmentID = strings.TrimSpace(environmentID)
	if actor.Kind == auth.ActorKindAPIKey {
		if projectID != "" || environmentID != "" {
			return auth.Scope{}, pgtype.UUID{}, pgtype.UUID{}, errors.New("project_id and environment_id are not accepted with API keys")
		}
		scope, ok := actor.EnvironmentScope()
		if !ok {
			return auth.Scope{}, pgtype.UUID{}, pgtype.UUID{}, errors.New("API key is not bound to an environment")
		}
		scopeProjectID, scopeEnvironmentID, err := s.runScopeIDs(ctx, actor.OrgID, scope)
		if err != nil {
			return auth.Scope{}, pgtype.UUID{}, pgtype.UUID{}, err
		}
		return scope, scopeProjectID, scopeEnvironmentID, nil
	}
	return s.secretRequestScope(ctx, actor.OrgID, projectID, environmentID)
}

func (s *Server) requestEnvironmentScopeFromRequest(r *http.Request, actor auth.Actor, projectID string, environmentID string) (auth.Scope, pgtype.UUID, pgtype.UUID, error) {
	projectID, environmentID, err := environmentScopeRefsFromRequest(r, actor, projectID, environmentID)
	if err != nil {
		return auth.Scope{}, pgtype.UUID{}, pgtype.UUID{}, err
	}
	return s.requestEnvironmentScope(r.Context(), actor, projectID, environmentID)
}

func environmentScopeRefsFromRequest(r *http.Request, actor auth.Actor, projectID string, environmentID string) (string, string, error) {
	projectID = strings.TrimSpace(projectID)
	environmentID = strings.TrimSpace(environmentID)
	pathProjectID := strings.TrimSpace(chi.URLParam(r, "projectID"))
	pathEnvironmentID := strings.TrimSpace(chi.URLParam(r, "environmentID"))
	hasPathScope := pathProjectID != "" || pathEnvironmentID != ""
	if hasPathScope && (pathProjectID == "" || pathEnvironmentID == "") {
		return "", "", errors.New("project_id and environment_id must be provided together")
	}
	switch actor.Kind {
	case auth.ActorKindSession:
		if !hasPathScope {
			return "", "", errors.New("session environment scoped requests must use the project environment path")
		}
		if projectID != "" || environmentID != "" {
			return "", "", errors.New("project_id and environment_id are not accepted in session request payloads")
		}
		return pathProjectID, pathEnvironmentID, nil
	case auth.ActorKindAPIKey:
		if hasPathScope {
			return "", "", errors.New("API key requests must use API key routes")
		}
		if projectID != "" || environmentID != "" {
			return "", "", errors.New("project_id and environment_id are not accepted with API keys")
		}
	}
	return projectID, environmentID, nil
}

func (s *Server) requireActorScopeForRecord(r *http.Request, actor auth.Actor, projectID pgtype.UUID, environmentID pgtype.UUID) error {
	switch actor.Kind {
	case auth.ActorKindSession:
		_, pathProjectID, pathEnvironmentID, err := s.requestEnvironmentScopeFromRequest(r, actor, "", "")
		if err != nil {
			return err
		}
		if pathProjectID != projectID || pathEnvironmentID != environmentID {
			return pgx.ErrNoRows
		}
	case auth.ActorKindAPIKey:
		scope, ok := actor.EnvironmentScope()
		if !ok {
			return errors.New("API key is not bound to an environment")
		}
		recordScope := auth.Scope{
			OrgID:         actor.OrgID,
			ProjectID:     ids.MustFromPG(projectID).String(),
			EnvironmentID: ids.MustFromPG(environmentID).String(),
		}
		if scope.ProjectID != recordScope.ProjectID || scope.EnvironmentID != recordScope.EnvironmentID {
			return pgx.ErrNoRows
		}
	default:
		return nil
	}
	return nil
}

func runCreatedEventPayload(taskID string, payload json.RawMessage, maxDurationSeconds int32, secretNames []string, retryPolicy []byte, metadata []byte, tags []string) ([]byte, error) {
	secretNames = append([]string{}, secretNames...)
	sort.Strings(secretNames)
	tags = append([]string{}, tags...)
	return json.Marshal(runCreatedPayload{
		TaskID:             taskID,
		Payload:            payload,
		MaxDurationSeconds: maxDurationSeconds,
		SecretNames:        secretNames,
		RetryPolicy:        json.RawMessage(retryPolicy),
		Metadata:           json.RawMessage(metadata),
		Tags:               tags,
	})
}

type runCreatedPayload struct {
	MaxDurationSeconds int32           `json:"max_duration_seconds"`
	Metadata           json.RawMessage `json:"metadata"`
	Payload            json.RawMessage `json:"payload"`
	RetryPolicy        json.RawMessage `json:"retry_policy"`
	SecretNames        []string        `json:"secret_names"`
	Tags               []string        `json:"tags"`
	TaskID             string          `json:"task_id"`
}

func deploymentTaskSecretNames(raw []byte) ([]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var declarations []api.SecretDeclaration
	if err := json.Unmarshal(raw, &declarations); err != nil {
		return nil, fmt.Errorf("decode deployment task secret declarations: %w", err)
	}
	names := make([]string, 0, len(declarations))
	seen := map[string]struct{}{}
	for _, declaration := range declarations {
		name := strings.TrimSpace(declaration.Name)
		if err := secret.ValidateName(name); err != nil {
			return nil, fmt.Errorf("deployment task secret declaration name: %w", err)
		}
		if _, ok := seen[name]; ok {
			return nil, fmt.Errorf("deployment task has duplicate secret declaration %q", name)
		}
		seen[name] = struct{}{}
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
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
	scope := auth.Scope{
		OrgID:         actor.OrgID,
		ProjectID:     ids.MustFromPG(summary.ProjectID).String(),
		EnvironmentID: ids.MustFromPG(summary.EnvironmentID).String(),
	}
	if err := s.requireActorScopeForRecord(r, actor, summary.ProjectID, summary.EnvironmentID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, errors.New("run not found"))
			return
		}
		writeError(w, http.StatusBadRequest, err)
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

func (s *Server) cancelRun(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("run storage is not configured"))
		return
	}
	runID, err := parseUUIDParam(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	var request api.CancelRunRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid cancel request JSON: %w", err))
		return
	}
	request.Reason = strings.TrimSpace(request.Reason)
	idempotencyKey, err := normalizeRunOperationIdempotencyKey(request.IdempotencyKey)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	actor := actorFromContext(r.Context())
	runRow, err := s.db.GetRunSummary(r.Context(), db.GetRunSummaryParams{OrgID: ids.ToPG(actor.OrgID), ID: ids.ToPG(runID)})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, errors.New("run not found"))
		return
	}
	if err != nil {
		s.log.Error("get run before cancel failed", "run_id", runID.String(), "error", err)
		writeError(w, http.StatusInternalServerError, errors.New("cancel run"))
		return
	}
	summary := getRunSummary(runRow)
	scope := auth.Scope{
		OrgID:         actor.OrgID,
		ProjectID:     ids.MustFromPG(summary.ProjectID).String(),
		EnvironmentID: ids.MustFromPG(summary.EnvironmentID).String(),
	}
	if err := s.requireActorScopeForRecord(r, actor, summary.ProjectID, summary.EnvironmentID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, errors.New("run not found"))
			return
		}
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if !actor.HasPermission(auth.PermissionRunsManage, scope) {
		writeError(w, http.StatusForbidden, errors.New("permission is required"))
		return
	}
	requestBody, err := json.Marshal(request)
	if err != nil {
		writeError(w, http.StatusInternalServerError, errors.New("encode cancel request"))
		return
	}
	operation, err := s.createRunOperation(r.Context(), actor, summary, db.RunOperationKindCancel, request.Reason, requestBody, idempotencyKey)
	if err != nil {
		s.log.Error("create cancel operation failed", "run_id", runID.String(), "error", err)
		writeError(w, http.StatusInternalServerError, errors.New("cancel run"))
		return
	}
	sameCancelRequest, err := sameJSONValue(operation.Request, requestBody)
	if err != nil {
		s.log.Error("compare cancel operation request failed", "run_id", runID.String(), "operation_id", ids.MustFromPG(operation.ID).String(), "error", err)
		writeError(w, http.StatusInternalServerError, errors.New("cancel run"))
		return
	}
	if idempotencyKey != "" && !sameCancelRequest {
		writeError(w, http.StatusConflict, errors.New("cancel idempotency key was used with a different request"))
		return
	}
	if operation.Status != db.RunOperationStatusRequested {
		response, err := s.runResponse(r.Context(), summary)
		if err != nil {
			s.log.Error("build idempotent cancel response failed", "run_id", runID.String(), "error", err)
			writeError(w, http.StatusInternalServerError, errors.New("cancel run"))
			return
		}
		writeJSON(w, http.StatusOK, api.CancelRunResponse{Run: response, Operation: runOperationResponse(operation)})
		return
	}
	cancelled, err := s.db.CancelRun(r.Context(), db.CancelRunParams{
		OrgID:       ids.ToPG(actor.OrgID),
		RunID:       ids.ToPG(runID),
		Reason:      request.Reason,
		Force:       request.Force,
		OperationID: operation.ID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			operationID := operation.ID
			operation, err = s.db.GetRunOperation(r.Context(), db.GetRunOperationParams{OrgID: ids.ToPG(actor.OrgID), ID: operationID})
			if err != nil {
				s.log.Error("get idempotent cancel operation failed", "run_id", runID.String(), "operation_id", ids.MustFromPG(operationID).String(), "error", err)
				writeError(w, http.StatusInternalServerError, errors.New("cancel run"))
				return
			}
			if operation.Status != db.RunOperationStatusRequested {
				runRow, err := s.db.GetRunSummary(r.Context(), db.GetRunSummaryParams{OrgID: ids.ToPG(actor.OrgID), ID: ids.ToPG(runID)})
				if err != nil {
					s.log.Error("get idempotent cancel run failed", "run_id", runID.String(), "operation_id", ids.MustFromPG(operationID).String(), "error", err)
					writeError(w, http.StatusInternalServerError, errors.New("cancel run"))
					return
				}
				response, err := s.runResponse(r.Context(), getRunSummary(runRow))
				if err != nil {
					s.log.Error("build idempotent cancel response failed", "run_id", runID.String(), "operation_id", ids.MustFromPG(operationID).String(), "error", err)
					writeError(w, http.StatusInternalServerError, errors.New("cancel run"))
					return
				}
				writeJSON(w, http.StatusOK, api.CancelRunResponse{Run: response, Operation: runOperationResponse(operation)})
				return
			}
		}
		s.log.Error("cancel run failed", "run_id", runID.String(), "error", err)
		writeError(w, http.StatusInternalServerError, errors.New("cancel run"))
		return
	}
	operation, err = s.db.GetRunOperation(r.Context(), db.GetRunOperationParams{OrgID: ids.ToPG(actor.OrgID), ID: operation.ID})
	if err != nil {
		s.log.Error("get cancel operation failed", "run_id", runID.String(), "operation_id", ids.MustFromPG(operation.ID).String(), "error", err)
		writeError(w, http.StatusInternalServerError, errors.New("cancel run"))
		return
	}
	response, err := s.runResponse(r.Context(), cancelRunSummary(cancelled))
	if err != nil {
		s.log.Error("build cancel response failed", "run_id", runID.String(), "error", err)
		writeError(w, http.StatusInternalServerError, errors.New("cancel run"))
		return
	}
	writeJSON(w, http.StatusOK, api.CancelRunResponse{Run: response, Operation: runOperationResponse(operation)})
}

func (s *Server) replayRun(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("run storage is not configured"))
		return
	}
	runID, err := parseUUIDParam(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	var request api.ReplayRunRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid replay request JSON: %w", err))
		return
	}
	request.Version = strings.TrimSpace(request.Version)
	request.Reason = strings.TrimSpace(request.Reason)
	idempotencyKey, err := normalizeRunOperationIdempotencyKey(request.IdempotencyKey)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	actor := actorFromContext(r.Context())
	original, err := s.db.GetRun(r.Context(), db.GetRunParams{OrgID: ids.ToPG(actor.OrgID), ID: ids.ToPG(runID)})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, errors.New("run not found"))
		return
	}
	if err != nil {
		s.log.Error("get run before replay failed", "run_id", runID.String(), "error", err)
		writeError(w, http.StatusInternalServerError, errors.New("replay run"))
		return
	}
	originalSummary := runRecordSummary(original)
	scope := auth.Scope{
		OrgID:         actor.OrgID,
		ProjectID:     ids.MustFromPG(originalSummary.ProjectID).String(),
		EnvironmentID: ids.MustFromPG(originalSummary.EnvironmentID).String(),
	}
	if err := s.requireActorScopeForRecord(r, actor, originalSummary.ProjectID, originalSummary.EnvironmentID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, errors.New("run not found"))
			return
		}
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if !actor.HasPermission(auth.PermissionRunsManage, scope) {
		writeError(w, http.StatusForbidden, errors.New("permission is required"))
		return
	}
	replayRequest, err := replayCreateRunRequest(original, request)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if idempotencyKey != "" {
		replayRequest.Options.IdempotencyKey = "replay:" + runID.String() + ":" + idempotencyKey
	}
	requestBody, err := json.Marshal(request)
	if err != nil {
		writeError(w, http.StatusInternalServerError, errors.New("encode replay request"))
		return
	}
	operation, err := s.createRunOperation(r.Context(), actor, originalSummary, db.RunOperationKindReplay, request.Reason, requestBody, idempotencyKey)
	if err != nil {
		s.log.Error("create replay operation failed", "run_id", runID.String(), "error", err)
		writeError(w, http.StatusInternalServerError, errors.New("replay run"))
		return
	}
	sameReplayRequest, err := sameJSONValue(operation.Request, requestBody)
	if err != nil {
		s.log.Error("compare replay operation request failed", "run_id", runID.String(), "operation_id", ids.MustFromPG(operation.ID).String(), "error", err)
		writeError(w, http.StatusInternalServerError, errors.New("replay run"))
		return
	}
	if idempotencyKey != "" && !sameReplayRequest {
		writeError(w, http.StatusConflict, errors.New("replay idempotency key was used with a different request"))
		return
	}
	if operation.Status != db.RunOperationStatusRequested {
		replayed, err := s.idempotentReplayRun(r.Context(), actor, operation, requestBody)
		if err != nil {
			if errors.Is(err, errIdempotencyKeyConflict) {
				writeError(w, http.StatusConflict, err)
				return
			}
			s.log.Error("resolve idempotent replay failed", "run_id", runID.String(), "operation_id", ids.MustFromPG(operation.ID).String(), "error", err)
			writeError(w, http.StatusInternalServerError, errors.New("replay run"))
			return
		}
		response, err := s.runResponse(r.Context(), replayed)
		if err != nil {
			s.log.Error("build idempotent replay response failed", "run_id", ids.MustFromPG(replayed.ID).String(), "error", err)
			writeError(w, http.StatusInternalServerError, errors.New("replay run"))
			return
		}
		writeJSON(w, http.StatusOK, api.ReplayRunResponse{
			Run:       response,
			Operation: runOperationResponse(operation),
		})
		return
	}
	replayed, _, err := s.createRunFromRequest(contextWithRequestVersionMetadata(r.Context(), r), actor, replayRequest, runSource{
		replayedFromRunID: original.ID,
		replayOperationID: operation.ID,
	})
	if err != nil {
		_, _ = s.db.MarkRunOperationRejected(r.Context(), db.MarkRunOperationRejectedParams{
			Result: fmt.Appendf(nil, `{"error":%q}`, err.Error()),
			ID:     operation.ID,
			OrgID:  ids.ToPG(actor.OrgID),
		})
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
		s.log.Error("replay run failed", "run_id", runID.String(), "error", err)
		writeError(w, http.StatusInternalServerError, errors.New("replay run"))
		return
	}
	operationResult, err := json.Marshal(map[string]string{
		"source_run_id": runID.String(),
		"run_id":        ids.MustFromPG(replayed.ID).String(),
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, errors.New("encode replay result"))
		return
	}
	operationID := operation.ID
	operation, err = s.db.MarkRunOperationApplied(r.Context(), db.MarkRunOperationAppliedParams{
		Result: operationResult,
		ID:     operationID,
		OrgID:  ids.ToPG(actor.OrgID),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		operation, err = s.db.GetRunOperation(r.Context(), db.GetRunOperationParams{OrgID: ids.ToPG(actor.OrgID), ID: operationID})
		if err == nil && operation.Status != db.RunOperationStatusRequested {
			replayed, replayErr := s.idempotentReplayRun(r.Context(), actor, operation, requestBody)
			if replayErr == nil {
				response, responseErr := s.runResponse(r.Context(), replayed)
				if responseErr != nil {
					s.log.Error("build raced replay response failed", "run_id", ids.MustFromPG(replayed.ID).String(), "error", responseErr)
					writeError(w, http.StatusInternalServerError, errors.New("replay run"))
					return
				}
				writeJSON(w, http.StatusOK, api.ReplayRunResponse{
					Run:       response,
					Operation: runOperationResponse(operation),
				})
				return
			}
		}
	}
	if err != nil {
		s.log.Error("mark replay operation applied failed", "run_id", runID.String(), "operation_id", ids.MustFromPG(operationID).String(), "error", err)
		writeError(w, http.StatusInternalServerError, errors.New("replay run"))
		return
	}
	response, err := s.runResponse(r.Context(), replayed)
	if err != nil {
		s.log.Error("build replay response failed", "run_id", ids.MustFromPG(replayed.ID).String(), "error", err)
		writeError(w, http.StatusInternalServerError, errors.New("replay run"))
		return
	}
	writeJSON(w, http.StatusCreated, api.ReplayRunResponse{
		Run:       response,
		Operation: runOperationResponse(operation),
	})
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
	summary := getRunSummary(run)
	scope := auth.Scope{
		OrgID:         actor.OrgID,
		ProjectID:     ids.MustFromPG(summary.ProjectID).String(),
		EnvironmentID: ids.MustFromPG(summary.EnvironmentID).String(),
	}
	if err := s.requireActorScopeForRecord(r, actor, summary.ProjectID, summary.EnvironmentID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, errors.New("run not found"))
			return
		}
		writeError(w, http.StatusBadRequest, err)
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
	summary := getRunSummary(run)
	scope := auth.Scope{
		OrgID:         actor.OrgID,
		ProjectID:     ids.MustFromPG(summary.ProjectID).String(),
		EnvironmentID: ids.MustFromPG(summary.EnvironmentID).String(),
	}
	if err := s.requireActorScopeForRecord(r, actor, summary.ProjectID, summary.EnvironmentID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, errors.New("run not found"))
			return
		}
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if !actor.HasPermission(auth.PermissionRunsRead, scope) {
		writeError(w, http.StatusForbidden, errors.New("permission is required"))
		return
	}
	if r.URL.Query().Get("follow") == "1" || strings.Contains(r.Header.Get("accept"), "text/event-stream") {
		s.followRunEvents(w, r, actor.OrgID, runID, cursor)
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
		value := rows[len(rows)-1].Seq
		nextCursor = &value
	}
	writeJSON(w, http.StatusOK, api.RunEventPage{Events: events, Cursor: cursor, NextCursor: nextCursor})
}

func (s *Server) listRunEvents(r *http.Request, orgID pgtype.UUID, runID pgtype.UUID, cursor int64, limit int32) ([]db.Event, error) {
	return s.db.ListSubjectEvents(r.Context(), db.ListSubjectEventsParams{
		OrgID:       orgID,
		SubjectType: db.EventSubjectTypeRun,
		SubjectID:   runID,
		Seq:         cursor,
		RowLimit:    limit + 1,
	})
}

func (s *Server) listRunSummaries(r *http.Request, actor auth.Actor, statusFilter string, limit int32) ([]runSummary, error) {
	requestedScope, err := s.requestedRunListScope(r, actor)
	if err != nil {
		return nil, err
	}
	if !actor.HasPermission(auth.PermissionRunsRead, requestedScope) {
		return nil, errPermissionRequired
	}
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

func (s *Server) countRunStatuses(r *http.Request, actor auth.Actor) (api.RunCountsResponse, error) {
	requestedScope, err := s.requestedRunListScope(r, actor)
	if err != nil {
		return api.RunCountsResponse{}, err
	}
	if !actor.HasPermission(auth.PermissionRunsRead, requestedScope) {
		return api.RunCountsResponse{}, errPermissionRequired
	}
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

var errPermissionRequired = errors.New("permission is required")

func isScopeRequestError(err error) bool {
	if err == nil {
		return false
	}
	message := err.Error()
	return strings.Contains(message, "project_id") || strings.Contains(message, "environment_id")
}

func (s *Server) requestedRunListScope(r *http.Request, actor auth.Actor) (auth.Scope, error) {
	projectID := strings.TrimSpace(r.URL.Query().Get("project_id"))
	environmentID := strings.TrimSpace(r.URL.Query().Get("environment_id"))
	pathProjectID, pathEnvironmentID, err := environmentScopeRefsFromRequest(r, actor, projectID, environmentID)
	if err != nil {
		return auth.Scope{}, err
	}
	if pathProjectID != "" || pathEnvironmentID != "" {
		scope, _, _, err := s.requestEnvironmentScope(r.Context(), actor, pathProjectID, pathEnvironmentID)
		return scope, err
	}
	if actor.Kind == auth.ActorKindAPIKey {
		scope, ok := actor.EnvironmentScope()
		if !ok {
			return auth.Scope{}, errors.New("API key is not bound to an environment")
		}
		return scope, nil
	}
	return auth.Scope{}, errors.New("session environment scoped requests must use the project environment path")
}

func (s *Server) runScopeIDs(ctx context.Context, orgID uuid.UUID, scope auth.Scope) (pgtype.UUID, pgtype.UUID, error) {
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
	project, err := s.resolveProjectRef(ctx, orgID, projectID)
	if err != nil {
		return auth.Scope{}, pgtype.UUID{}, pgtype.UUID{}, err
	}
	environment, err := s.resolveEnvironmentRef(ctx, orgID, project.ID, environmentID)
	if err != nil {
		return auth.Scope{}, pgtype.UUID{}, pgtype.UUID{}, err
	}
	return auth.Scope{OrgID: orgID, ProjectID: ids.MustFromPG(project.ID).String(), EnvironmentID: ids.MustFromPG(environment.ID).String()}, project.ID, environment.ID, nil
}

func (s *Server) resolveProjectRef(ctx context.Context, orgID uuid.UUID, projectRef string) (db.Project, error) {
	projectRef = strings.TrimSpace(projectRef)
	if projectRef == "" {
		defaultScope, err := s.db.GetDefaultProjectEnvironment(ctx, ids.ToPG(orgID))
		if err != nil {
			return db.Project{}, fmt.Errorf("load project selection: %w", err)
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
		return db.Project{}, errors.New("project_id must be a project UUID or a project slug")
	}
	if err != nil {
		return db.Project{}, fmt.Errorf("load project: %w", err)
	}
	return project, nil
}

func (s *Server) resolveEnvironmentRef(ctx context.Context, orgID uuid.UUID, projectID pgtype.UUID, environmentRef string) (db.Environment, error) {
	environmentRef = strings.TrimSpace(environmentRef)
	if environmentRef == "" {
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
		return db.Environment{}, errors.New("environment_id must be an environment UUID or an environment slug")
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

func (s *Server) followRunEvents(w http.ResponseWriter, r *http.Request, orgID uuid.UUID, runID uuid.UUID, cursor int64) {
	if s.eventStream == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("event stream is not configured"))
		return
	}
	flusher, _ := w.(http.Flusher)
	w.Header().Set("content-type", "text/event-stream")
	w.Header().Set("cache-control", "no-cache")
	w.WriteHeader(http.StatusOK)
	encoder := json.NewEncoder(w)
	ctx, cancel := context.WithTimeout(r.Context(), runEventsFollowMaxDuration)
	defer cancel()
	err := s.eventStream.ReadSubject(ctx, orgID, db.EventSubjectTypeRun, runID, cursor, func(event api.RunEvent) error {
		_, _ = fmt.Fprintf(w, "id: %s\n", event.ID)
		_, _ = fmt.Fprint(w, "event: run_event\n")
		_, _ = fmt.Fprint(w, "data: ")
		if err := encoder.Encode(event); err != nil {
			return err
		}
		_, _ = fmt.Fprint(w, "\n")
		if flusher != nil {
			flusher.Flush()
		}
		if runEventKindIsTerminal(event.Kind) {
			cancel()
		}
		return nil
	}, func() error {
		_, _ = fmt.Fprint(w, ": keep-alive\n\n")
		if flusher != nil {
			flusher.Flush()
		}
		return nil
	})
	if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		s.log.Warn("follow run events failed", "error", err)
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

func runEventResponse(event db.Event) api.RunEvent {
	return eventResponseFromRecord(event)
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

func resolvedRetryPolicy(runPolicy json.RawMessage, taskPolicy []byte) ([]byte, error) {
	raw := bytes.TrimSpace(runPolicy)
	if len(raw) == 0 {
		raw = bytes.TrimSpace(taskPolicy)
	}
	if len(raw) == 0 {
		raw = []byte("false")
	}
	return validatedRetryPolicyJSON(raw, "retry")
}

func validatedRetryPolicyJSON(raw []byte, label string) ([]byte, error) {
	raw = bytes.TrimSpace(raw)
	if !json.Valid(raw) {
		return nil, fmt.Errorf("%s must be valid JSON", label)
	}
	if bytes.Equal(raw, []byte("false")) {
		return []byte("false"), nil
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, fmt.Errorf("%s decode failed: %w", label, err)
	}
	object, ok := value.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s must be false or an object", label)
	}
	for field := range object {
		switch field {
		case "maxAttempts", "backoff":
		default:
			return nil, fmt.Errorf("%s.%s is not supported", label, field)
		}
	}
	maxAttempts, ok := object["maxAttempts"].(float64)
	if !ok || maxAttempts != float64(int(maxAttempts)) || maxAttempts < 1 || maxAttempts > 10 {
		return nil, fmt.Errorf("%s.maxAttempts must be an integer between 1 and 10", label)
	}
	if backoff, ok := object["backoff"]; ok {
		backoffObject, ok := backoff.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("%s.backoff must be an object", label)
		}
		for field := range backoffObject {
			switch field {
			case "minMs", "maxMs", "factor", "jitter":
			default:
				return nil, fmt.Errorf("%s.backoff.%s is not supported", label, field)
			}
		}
		for _, field := range []string{"minMs", "maxMs"} {
			if value, ok := backoffObject[field]; ok && !isPositiveIntegerJSONNumber(value) {
				return nil, fmt.Errorf("%s.backoff.%s must be a positive integer", label, field)
			}
		}
		if factor, ok := backoffObject["factor"]; ok {
			number, ok := factor.(float64)
			if !ok || !isFinite(number) || number <= 0 {
				return nil, fmt.Errorf("%s.backoff.factor must be a positive number", label)
			}
		}
		if jitter, ok := backoffObject["jitter"]; ok {
			value, ok := jitter.(string)
			if !ok || (value != "none" && value != "full") {
				return nil, fmt.Errorf("%s.backoff.jitter must be \"none\" or \"full\"", label)
			}
		}
	}
	canonical, err := json.Marshal(object)
	if err != nil {
		return nil, fmt.Errorf("%s canonicalization failed: %w", label, err)
	}
	return canonical, nil
}

func isPositiveIntegerJSONNumber(value any) bool {
	number, ok := value.(float64)
	return ok && isFinite(number) && number == float64(int64(number)) && number > 0
}

func isFinite(number float64) bool {
	return !math.IsNaN(number) && !math.IsInf(number, 0)
}

func normalizedJSONObject(raw json.RawMessage, label string) ([]byte, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return []byte("{}"), nil
	}
	if !json.Valid(raw) {
		return nil, fmt.Errorf("%s must be valid JSON", label)
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, fmt.Errorf("%s decode failed: %w", label, err)
	}
	if _, ok := value.(map[string]any); !ok {
		return nil, fmt.Errorf("%s must be a JSON object", label)
	}
	canonical, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("%s canonicalization failed: %w", label, err)
	}
	return canonical, nil
}

func normalizedRunTags(tags []string) ([]string, error) {
	if len(tags) == 0 {
		return []string{}, nil
	}
	if len(tags) > 10 {
		return nil, errors.New("tags must contain at most 10 items")
	}
	out := make([]string, 0, len(tags))
	seen := map[string]struct{}{}
	for _, tag := range tags {
		tag = strings.TrimSpace(tag)
		if tag == "" {
			return nil, errors.New("tags must not contain empty values")
		}
		if len(tag) > 128 {
			return nil, errors.New("tags must be 128 characters or less")
		}
		if _, ok := seen[tag]; ok {
			continue
		}
		seen[tag] = struct{}{}
		out = append(out, tag)
	}
	sort.Strings(out)
	return out, nil
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

func runIdempotencyRequestHash(request api.CreateRunRequest, payload json.RawMessage, deploymentTask db.GetDeploymentTaskRow, maxDurationSeconds int32, lockedRetryPolicy []byte, metadata []byte, tags []string, scheduling runScheduling) (pgtype.Text, error) {
	canonicalPayload, err := canonicalJSON(payload)
	if err != nil {
		return pgtype.Text{}, fmt.Errorf("payload canonicalization failed: %w", err)
	}
	fingerprint := struct {
		TaskID     string          `json:"task_id"`
		Payload    json.RawMessage `json:"payload"`
		Metadata   json.RawMessage `json:"metadata"`
		Tags       []string        `json:"tags"`
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
		RetryPolicy json.RawMessage `json:"retry_policy"`
	}{
		TaskID:      request.TaskID,
		Payload:     canonicalPayload,
		Metadata:    json.RawMessage(metadata),
		Tags:        append([]string(nil), tags...),
		RetryPolicy: json.RawMessage(lockedRetryPolicy),
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

func normalizeRunOperationIdempotencyKey(key string) (string, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return "", nil
	}
	if len(key) > maxIdempotencyKeyLength {
		return "", fmt.Errorf("idempotency_key must be at most %d characters", maxIdempotencyKeyLength)
	}
	return key, nil
}

func (s *Server) createRunOperation(ctx context.Context, actor auth.Actor, run runSummary, kind db.RunOperationKind, reason string, requestBody []byte, idempotencyKey string) (db.RunOperation, error) {
	actorID, err := auth.ActorPrincipalAllowSystem(actor)
	if err != nil {
		return db.RunOperation{}, err
	}
	apiKeyID := pgtype.UUID{}
	if actor.Kind == auth.ActorKindAPIKey {
		apiKeyID = ids.ToPG(actor.APIKeyID)
	}
	return s.db.CreateRunOperation(ctx, db.CreateRunOperationParams{
		ID:             ids.ToPG(ids.New()),
		OrgID:          run.OrgID,
		ProjectID:      run.ProjectID,
		EnvironmentID:  run.EnvironmentID,
		RunID:          run.ID,
		Kind:           kind,
		ActorKind:      string(actor.Kind),
		ActorID:        actorID,
		ApiKeyID:       apiKeyID,
		Reason:         reason,
		Request:        requestBody,
		IdempotencyKey: idempotencyKey,
	})
}

func (s *Server) idempotentReplayRun(ctx context.Context, actor auth.Actor, operation db.RunOperation, requestBody []byte) (runSummary, error) {
	sameRequest, err := sameJSONValue(operation.Request, requestBody)
	if err != nil {
		return runSummary{}, fmt.Errorf("compare replay operation request: %w", err)
	}
	if !sameRequest {
		return runSummary{}, errIdempotencyKeyConflict
	}
	if operation.Status != db.RunOperationStatusApplied {
		return runSummary{}, errIdempotencyKeyConflict
	}
	var result struct {
		RunID string `json:"run_id"`
	}
	if err := json.Unmarshal(operation.Result, &result); err != nil {
		return runSummary{}, fmt.Errorf("decode replay operation result: %w", err)
	}
	runID, err := ids.Parse(strings.TrimSpace(result.RunID))
	if err != nil {
		return runSummary{}, fmt.Errorf("decode replay operation run_id: %w", err)
	}
	run, err := s.db.GetRunSummary(ctx, db.GetRunSummaryParams{OrgID: ids.ToPG(actor.OrgID), ID: ids.ToPG(runID)})
	if err != nil {
		return runSummary{}, err
	}
	return getRunSummary(run), nil
}

func sameJSONValue(left []byte, right []byte) (bool, error) {
	leftValue, err := decodeJSONValueForComparison(left)
	if err != nil {
		return false, err
	}
	rightValue, err := decodeJSONValueForComparison(right)
	if err != nil {
		return false, err
	}
	return reflect.DeepEqual(leftValue, rightValue), nil
}

func decodeJSONValueForComparison(raw []byte) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	return value, nil
}

func replayCreateRunRequest(original db.Run, replay api.ReplayRunRequest) (api.CreateRunRequest, error) {
	payload := json.RawMessage(original.Payload)
	if len(replay.Payload) > 0 {
		payload = replay.Payload
	}
	if len(payload) == 0 {
		payload = json.RawMessage(`{}`)
	}
	if !json.Valid(payload) {
		return api.CreateRunRequest{}, errors.New("payload must be valid JSON")
	}
	metadata := json.RawMessage(original.Metadata)
	if len(replay.Metadata) > 0 {
		metadata = replay.Metadata
	}
	if len(metadata) == 0 {
		metadata = json.RawMessage(`{}`)
	}
	tags := append([]string(nil), original.Tags...)
	if replay.Tags != nil {
		tags = append([]string(nil), replay.Tags...)
	}
	request := api.CreateRunRequest{
		ProjectID:     ids.MustFromPG(original.ProjectID).String(),
		EnvironmentID: ids.MustFromPG(original.EnvironmentID).String(),
		TaskID:        original.TaskID,
		Payload:       payload,
		Options: api.CreateRunOptions{
			Queue:              &api.RunQueueOption{Name: original.QueueName},
			Priority:           original.Priority,
			TTL:                original.Ttl,
			MaxDurationSeconds: original.MaxDurationSeconds,
			Retry:              json.RawMessage(original.LockedRetryPolicy),
			Metadata:           metadata,
			Tags:               tags,
		},
	}
	if original.ConcurrencyKey.Valid {
		request.Options.ConcurrencyKey = original.ConcurrencyKey.String
	}
	switch replay.Version {
	case "", "original":
		request.Options.DeploymentID = ids.MustFromPG(original.DeploymentID).String()
	case "latest":
	default:
		request.Options.Version = replay.Version
	}
	return request, nil
}

type runSummary struct {
	ID                   pgtype.UUID
	OrgID                pgtype.UUID
	ProjectID            pgtype.UUID
	EnvironmentID        pgtype.UUID
	DeploymentID         pgtype.UUID
	DeploymentTaskID     pgtype.UUID
	DeploymentVersion    string
	APIVersion           string
	SDKVersion           string
	CLIVersion           string
	TaskID               string
	Status               db.RunStatus
	ExecutionStatus      db.RunExecutionStatus
	TerminalOutcome      db.NullRunTerminalOutcome
	Metadata             []byte
	Tags                 []string
	LockedRetryPolicy    []byte
	ReplayedFromRunID    pgtype.UUID
	CurrentAttemptNumber pgtype.Int4
	ExitCode             pgtype.Int4
	Output               []byte
	CreatedAt            pgtype.Timestamptz
	UpdatedAt            pgtype.Timestamptz
}

func idempotentRunSummary(run db.GetScopedRunByIdempotencyKeyRow) runSummary {
	return runSummary{
		ID:                   run.ID,
		OrgID:                run.OrgID,
		ProjectID:            run.ProjectID,
		EnvironmentID:        run.EnvironmentID,
		DeploymentID:         run.DeploymentID,
		DeploymentTaskID:     run.DeploymentTaskID,
		DeploymentVersion:    run.DeploymentVersion,
		APIVersion:           run.ApiVersion,
		SDKVersion:           run.SdkVersion,
		CLIVersion:           run.CliVersion,
		TaskID:               run.TaskID,
		Status:               run.Status,
		ExecutionStatus:      run.ExecutionStatus,
		TerminalOutcome:      run.TerminalOutcome,
		Metadata:             run.Metadata,
		Tags:                 run.Tags,
		LockedRetryPolicy:    run.LockedRetryPolicy,
		ReplayedFromRunID:    run.ReplayedFromRunID,
		CurrentAttemptNumber: run.CurrentAttemptNumber,
		ExitCode:             run.ExitCode,
		Output:               run.Output,
		CreatedAt:            run.CreatedAt,
		UpdatedAt:            run.UpdatedAt,
	}
}

func createScopedRunSummary(run db.CreateScopedRunRow) runSummary {
	return runSummary{
		ID:                   run.ID,
		OrgID:                run.OrgID,
		ProjectID:            run.ProjectID,
		EnvironmentID:        run.EnvironmentID,
		DeploymentID:         run.DeploymentID,
		DeploymentTaskID:     run.DeploymentTaskID,
		DeploymentVersion:    run.DeploymentVersion,
		APIVersion:           run.ApiVersion,
		SDKVersion:           run.SdkVersion,
		CLIVersion:           run.CliVersion,
		TaskID:               run.TaskID,
		Status:               run.Status,
		ExecutionStatus:      run.ExecutionStatus,
		TerminalOutcome:      run.TerminalOutcome,
		Metadata:             run.Metadata,
		Tags:                 run.Tags,
		LockedRetryPolicy:    run.LockedRetryPolicy,
		ReplayedFromRunID:    run.ReplayedFromRunID,
		CurrentAttemptNumber: run.CurrentAttemptNumber,
		ExitCode:             run.ExitCode,
		Output:               run.Output,
		CreatedAt:            run.CreatedAt,
		UpdatedAt:            run.UpdatedAt,
	}
}

func getRunSummary(run db.GetRunSummaryRow) runSummary {
	return runSummary{
		ID:                   run.ID,
		OrgID:                run.OrgID,
		ProjectID:            run.ProjectID,
		EnvironmentID:        run.EnvironmentID,
		DeploymentID:         run.DeploymentID,
		DeploymentTaskID:     run.DeploymentTaskID,
		DeploymentVersion:    run.DeploymentVersion,
		APIVersion:           run.ApiVersion,
		SDKVersion:           run.SdkVersion,
		CLIVersion:           run.CliVersion,
		TaskID:               run.TaskID,
		Status:               run.Status,
		ExecutionStatus:      run.ExecutionStatus,
		TerminalOutcome:      run.TerminalOutcome,
		Metadata:             run.Metadata,
		Tags:                 run.Tags,
		LockedRetryPolicy:    run.LockedRetryPolicy,
		ReplayedFromRunID:    run.ReplayedFromRunID,
		CurrentAttemptNumber: run.CurrentAttemptNumber,
		ExitCode:             run.ExitCode,
		Output:               run.Output,
		CreatedAt:            run.CreatedAt,
		UpdatedAt:            run.UpdatedAt,
	}
}

func listRunSummary(run db.ListRunSummariesRow) runSummary {
	return runSummary{
		ID:                   run.ID,
		OrgID:                run.OrgID,
		ProjectID:            run.ProjectID,
		EnvironmentID:        run.EnvironmentID,
		DeploymentID:         run.DeploymentID,
		DeploymentTaskID:     run.DeploymentTaskID,
		DeploymentVersion:    run.DeploymentVersion,
		APIVersion:           run.ApiVersion,
		SDKVersion:           run.SdkVersion,
		CLIVersion:           run.CliVersion,
		TaskID:               run.TaskID,
		Status:               run.Status,
		ExecutionStatus:      run.ExecutionStatus,
		TerminalOutcome:      run.TerminalOutcome,
		Metadata:             run.Metadata,
		Tags:                 run.Tags,
		LockedRetryPolicy:    run.LockedRetryPolicy,
		ReplayedFromRunID:    run.ReplayedFromRunID,
		CurrentAttemptNumber: run.CurrentAttemptNumber,
		ExitCode:             run.ExitCode,
		Output:               run.Output,
		CreatedAt:            run.CreatedAt,
		UpdatedAt:            run.UpdatedAt,
	}
}

func listScopedRunSummary(run db.ListScopedRunSummariesRow) runSummary {
	return runSummary{
		ID:                   run.ID,
		OrgID:                run.OrgID,
		ProjectID:            run.ProjectID,
		EnvironmentID:        run.EnvironmentID,
		DeploymentID:         run.DeploymentID,
		DeploymentTaskID:     run.DeploymentTaskID,
		DeploymentVersion:    run.DeploymentVersion,
		APIVersion:           run.ApiVersion,
		SDKVersion:           run.SdkVersion,
		CLIVersion:           run.CliVersion,
		TaskID:               run.TaskID,
		Status:               run.Status,
		ExecutionStatus:      run.ExecutionStatus,
		TerminalOutcome:      run.TerminalOutcome,
		Metadata:             run.Metadata,
		Tags:                 run.Tags,
		LockedRetryPolicy:    run.LockedRetryPolicy,
		ReplayedFromRunID:    run.ReplayedFromRunID,
		CurrentAttemptNumber: run.CurrentAttemptNumber,
		ExitCode:             run.ExitCode,
		Output:               run.Output,
		CreatedAt:            run.CreatedAt,
		UpdatedAt:            run.UpdatedAt,
	}
}

func cancelRunSummary(run db.CancelRunRow) runSummary {
	return runSummary{
		ID:                   run.ID,
		OrgID:                run.OrgID,
		ProjectID:            run.ProjectID,
		EnvironmentID:        run.EnvironmentID,
		DeploymentID:         run.DeploymentID,
		DeploymentTaskID:     run.DeploymentTaskID,
		DeploymentVersion:    run.DeploymentVersion,
		APIVersion:           run.ApiVersion,
		SDKVersion:           run.SdkVersion,
		CLIVersion:           run.CliVersion,
		TaskID:               run.TaskID,
		Status:               run.Status,
		ExecutionStatus:      run.ExecutionStatus,
		TerminalOutcome:      run.TerminalOutcome,
		Metadata:             run.Metadata,
		Tags:                 run.Tags,
		LockedRetryPolicy:    run.LockedRetryPolicy,
		ReplayedFromRunID:    run.ReplayedFromRunID,
		CurrentAttemptNumber: run.CurrentAttemptNumber,
		ExitCode:             run.ExitCode,
		Output:               run.Output,
		CreatedAt:            run.CreatedAt,
		UpdatedAt:            run.UpdatedAt,
	}
}

func runRecordSummary(run db.Run) runSummary {
	return runSummary{
		ID:                   run.ID,
		OrgID:                run.OrgID,
		ProjectID:            run.ProjectID,
		EnvironmentID:        run.EnvironmentID,
		DeploymentID:         run.DeploymentID,
		DeploymentTaskID:     run.DeploymentTaskID,
		DeploymentVersion:    run.DeploymentVersion,
		APIVersion:           run.ApiVersion,
		SDKVersion:           run.SdkVersion,
		CLIVersion:           run.CliVersion,
		TaskID:               run.TaskID,
		Status:               run.Status,
		ExecutionStatus:      run.ExecutionStatus,
		TerminalOutcome:      run.TerminalOutcome,
		Metadata:             run.Metadata,
		Tags:                 run.Tags,
		LockedRetryPolicy:    run.LockedRetryPolicy,
		ReplayedFromRunID:    run.ReplayedFromRunID,
		CurrentAttemptNumber: run.CurrentAttemptNumber,
		ExitCode:             run.ExitCode,
		Output:               run.Output,
		CreatedAt:            run.CreatedAt,
		UpdatedAt:            run.UpdatedAt,
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
	var attemptNumber *int32
	if run.CurrentAttemptNumber.Valid {
		attemptNumber = &run.CurrentAttemptNumber.Int32
	}
	var output json.RawMessage
	if len(run.Output) > 0 {
		output = append(json.RawMessage(nil), run.Output...)
	}
	return api.RunResponse{
		ID:                runID.String(),
		ProjectID:         ids.MustFromPG(run.ProjectID).String(),
		EnvironmentID:     ids.MustFromPG(run.EnvironmentID).String(),
		DeploymentID:      ids.MustFromPG(run.DeploymentID).String(),
		DeploymentTaskID:  ids.MustFromPG(run.DeploymentTaskID).String(),
		Version:           run.DeploymentVersion,
		DeploymentVersion: run.DeploymentVersion,
		APIVersion:        run.APIVersion,
		SDKVersion:        run.SDKVersion,
		CLIVersion:        run.CLIVersion,
		TaskID:            run.TaskID,
		Status:            publicRunStatus(run.Status),
		AttemptNumber:     attemptNumber,
		ExitCode:          exitCode,
		Output:            output,
		CreatedAt:         pgTime(run.CreatedAt),
		UpdatedAt:         pgTime(run.UpdatedAt),
	}
}

func runOperationResponse(operation db.RunOperation) api.RunOperationResponse {
	var appliedAt *time.Time
	if operation.AppliedAt.Valid {
		value := operation.AppliedAt.Time
		appliedAt = &value
	}
	return api.RunOperationResponse{
		ID:        ids.MustFromPG(operation.ID).String(),
		RunID:     ids.MustFromPG(operation.RunID).String(),
		Kind:      string(operation.Kind),
		Status:    string(operation.Status),
		Reason:    operation.Reason,
		CreatedAt: pgTime(operation.CreatedAt),
		AppliedAt: appliedAt,
	}
}

func publicRunStatus(status db.RunStatus) string {
	return string(status)
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
	case db.WaitpointKindHuman, db.WaitpointKindDelay:
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
		SessionID:      waitpoint.SessionID,
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
