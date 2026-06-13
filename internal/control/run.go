package control

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/schedule"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/helmrdotdev/helmr/internal/tracing"
	"github.com/jackc/pgx/v5/pgtype"
)

const (
	defaultRunMaxDurationSeconds  = int32(900)
	minRunMaxDurationSeconds      = int32(5)
	maxRunDurationSeconds         = int32(86400)
	defaultIdempotencyKeyTTL      = 30 * 24 * time.Hour
	maxIdempotencyKeyLength       = 512
	maxRunLogSnapshotBytes        = int64(1 << 20)
	runLogStreamBatchSize         = int32(100)
	runLogStreamPollInterval      = time.Second
	runLogStreamFollowMaxDuration = 30 * time.Minute
	runEventsPageSize             = int32(200)
	runEventsFollowMaxDuration    = 30 * time.Minute
)

var errIdempotencyKeyConflict = errors.New("idempotency_key was already used with different run parameters")

func (s *Server) createRun(w http.ResponseWriter, r *http.Request) {
	actor := actorFromContext(r.Context())
	var request api.CreateRunRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid run request JSON: %w", err)))
		return
	}
	projectID, environmentID, err := environmentScopeRefsFromRequest(r, actor, request.ProjectID, request.EnvironmentID)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	request.ProjectID = projectID
	request.EnvironmentID = environmentID
	run, idempotencyHit, err := s.createRunFromRequest(contextWithRequestVersionMetadata(r.Context(), r), actor, request, runSource{})
	if err != nil {
		if errors.Is(err, errIdempotencyKeyConflict) {
			writeError(w, conflict(err))
			return
		}
		var upstreamErr createRunUpstreamError
		if errors.As(err, &upstreamErr) {
			writeError(w, badGateway(upstreamErr))
			return
		}
		if isCreateRunConfigError(err) {
			writeError(w, unavailable(err))
			return
		}
		if errors.Is(err, errPermissionRequired) {
			writeError(w, forbidden(err))
			return
		}
		var runDeploymentErr runDeploymentSelectionError
		if errors.As(err, &runDeploymentErr) {
			writeError(w, badRequest(runDeploymentErr))
			return
		}
		if isCreateRunClientError(err) {
			writeError(w, badRequest(err))
			return
		}
		s.log.Error("create run failed", "error", err)
		writeError(w, errors.New("create run"))
		return
	}
	response, err := s.runResponse(r.Context(), run)
	if err != nil {
		s.log.Error("build run response failed", "error", err)
		writeError(w, errors.New("build run response"))
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
	return isNoRows(err) ||
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
	orgID, err := ids.FromPG(row.OrgID)
	if err != nil {
		return pgtype.UUID{}, fmt.Errorf("schedule trigger org id is invalid: %v", err)
	}
	run, _, err := s.createRunFromRequest(ctx, auth.Actor{
		OrgID: orgID,
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
	if isNoRows(err) {
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
		projectUUID, err := ids.FromPG(projectID)
		if err != nil {
			return runSummary{}, false, fmt.Errorf("project id is invalid: %v", err)
		}
		environmentUUID, err := ids.FromPG(environmentID)
		if err != nil {
			return runSummary{}, false, fmt.Errorf("environment id is invalid: %v", err)
		}
		if err := s.secrets.CheckScopedNames(ctx, actor.OrgID, projectUUID, environmentUUID, secretNames); err != nil {
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
		if isNoRows(err) && source.scheduleInstanceID.Valid {
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
		if isNoRows(err) {
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
		if isNoRows(err) {
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
		writeError(w, unavailable(errors.New("run storage is not configured")))
		return
	}
	runID, err := parseUUIDParam(r, "id")
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	run, err := s.db.GetRunSummary(r.Context(), db.GetRunSummaryParams{
		OrgID: ids.ToPG(actorFromContext(r.Context()).OrgID),
		ID:    ids.ToPG(runID),
	})
	if isNoRows(err) {
		writeError(w, notFound(errors.New("run not found")))
		return
	}
	if err != nil {
		s.log.Error("get run failed", "run_id", runID.String(), "error", err)
		writeError(w, errors.New("get run"))
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
		if isNoRows(err) {
			writeError(w, notFound(errors.New("run not found")))
			return
		}
		writeError(w, badRequest(err))
		return
	}
	if !actor.HasPermission(auth.PermissionRunsRead, scope) {
		writeError(w, forbidden(errors.New("permission is required")))
		return
	}
	response, err := s.runResponse(r.Context(), summary)
	if err != nil {
		s.log.Error("get pending waitpoint failed", "run_id", runID.String(), "error", err)
		writeError(w, errors.New("get run"))
		return
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) listRuns(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, unavailable(errors.New("run storage is not configured")))
		return
	}
	statusFilter, limit, err := listRunsQuery(r)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	actor := actorFromContext(r.Context())
	summaries, err := s.listRunSummaries(r, actor, statusFilter, limit)
	if errors.Is(err, errPermissionRequired) {
		writeError(w, forbidden(errors.New("permission is required")))
		return
	}
	if isScopeRequestError(err) {
		writeError(w, badRequest(err))
		return
	}
	if err != nil {
		s.log.Error("list runs failed", "error", err)
		writeError(w, errors.New("list runs"))
		return
	}
	runs, err := s.runResponses(r.Context(), ids.ToPG(actor.OrgID), summaries)
	if err != nil {
		s.log.Error("list pending waitpoints failed", "error", err)
		writeError(w, errors.New("list runs"))
		return
	}
	writeJSON(w, http.StatusOK, api.ListRunsResponse{Runs: runs})
}

func (s *Server) countRuns(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, unavailable(errors.New("run storage is not configured")))
		return
	}
	actor := actorFromContext(r.Context())
	counts, err := s.countRunStatuses(r, actor)
	if errors.Is(err, errPermissionRequired) {
		writeError(w, forbidden(errors.New("permission is required")))
		return
	}
	if isScopeRequestError(err) {
		writeError(w, badRequest(err))
		return
	}
	if err != nil {
		s.log.Error("count runs failed", "error", err)
		writeError(w, errors.New("count runs"))
		return
	}
	writeJSON(w, http.StatusOK, counts)
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
		duration, err := api.ParsePositiveDuration(ttl, "ttl")
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
	if isNoRows(err) {
		return runScheduling{}, fmt.Errorf("queue %q is not declared in the selected deployment", scheduling.queueName)
	}
	if err != nil {
		return runScheduling{}, err
	}
	scheduling.queueConcurrencyLimit = queueConfig.QueueConcurrencyLimit
	return scheduling, nil
}

func publicRunStatus(status db.RunStatus) string {
	return string(status)
}
