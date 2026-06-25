package control

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/schedule"
	"github.com/helmrdotdev/helmr/internal/tracing"
	"github.com/jackc/pgx/v5/pgtype"
)

const (
	defaultTaskSessionWaitTimeout = 30 * time.Second
	maxTaskSessionWaitTimeout     = 5 * time.Minute
	defaultTaskSessionListLimit   = int32(100)
	maxTaskSessionListLimit       = int32(200)
	taskSessionWaitPollEvery      = 250 * time.Millisecond
	maxTaskSessionExternalIDBytes = 512
)

var (
	errTaskArchived                    = codedError{code: "task_archived"}
	errTaskNotDeployed                 = codedError{code: "task_not_deployed"}
	errTaskStartSessionFingerprint     = codedError{code: "session_fingerprint_mismatch", message: "task session start fingerprint mismatch"}
	errTaskStartIdempotencyFingerprint = codedError{code: "idempotency_fingerprint_mismatch", message: "idempotency_key was already used with different task start parameters"}
	errTaskStartIdempotencyExternalID  = codedError{code: "idempotency_external_id_mismatch", message: "idempotency_key resolves to a different task session"}
	errTaskSessionTerminated           = codedError{code: "session_terminal", message: "task session is terminal"}
	errTaskSessionNoCurrentRun         = codedError{code: "session_has_no_current_run"}
	errCloseRunActive                  = codedError{code: "close_run_active"}
	errTaskSessionExpiresAtPatch       = codedError{code: "session_expires_at_not_extendable", message: "session expires_at can only extend an existing future expiry"}
	errSandboxNotDeployed              = codedError{code: "sandbox_not_deployed", message: "task sandbox is not deployed"}
	errWorkspaceSandboxIncompatible    = codedError{code: "workspace_sandbox_incompatible", message: "workspace sandbox is incompatible with this task"}
	errWorkspaceResourceFloor          = codedError{code: "workspace_resource_floor_unsatisfied", message: "workspace resource floor is lower than this task requires"}
)

type taskStartSource struct {
	scheduleID            pgtype.UUID
	scheduleInstanceID    pgtype.UUID
	scheduleGeneration    int64
	scheduleOrgID         pgtype.UUID
	scheduleProjectID     pgtype.UUID
	scheduleEnvironmentID pgtype.UUID
	scheduledAt           pgtype.Timestamptz
}

type taskStartResult struct {
	session        db.TaskSession
	run            runSummary
	idempotencyHit bool
	sessionReused  bool
}

type taskStartIdempotencyBinding struct {
	ID                 pgtype.UUID
	OrgID              pgtype.UUID
	ProjectID          pgtype.UUID
	EnvironmentID      pgtype.UUID
	TaskID             string
	IdempotencyKey     string
	RequestFingerprint string
	TaskSessionID      pgtype.UUID
	FirstRunID         pgtype.UUID
	ExpiresAt          pgtype.Timestamptz
}

type controlTransaction interface {
	Commit(context.Context) error
	Rollback(context.Context) error
}

type queryTransactionBeginner interface {
	BeginQuerier(context.Context) (db.Querier, controlTransaction, error)
}

func (s *Server) beginControlTransaction(ctx context.Context) (db.Querier, controlTransaction, error) {
	if beginner, ok := s.db.(queryTransactionBeginner); ok {
		return beginner.BeginQuerier(ctx)
	}
	if s.tx == nil {
		return nil, nil, errors.New("transactional control database is required")
	}
	tx, err := s.tx.Begin(ctx)
	if err != nil {
		return nil, nil, err
	}
	return db.New(tx), tx, nil
}

func (s *Server) startTask(w http.ResponseWriter, r *http.Request) {
	taskID := strings.TrimSpace(chi.URLParam(r, "taskID"))
	if err := api.ValidateTaskID(taskID); err != nil {
		writeError(w, badRequest(err))
		return
	}
	actor := actorFromContext(r.Context())
	var request api.TaskStartRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid task start request JSON: %w", err)))
		return
	}
	scope, projectID, environmentID, err := s.requestEnvironmentScopeFromRequest(r, actor, request.ProjectID, request.EnvironmentID)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	result, err := s.startTaskSessionFromRequestInScope(contextWithRequestVersionMetadata(r.Context(), r), actor, scope, projectID, environmentID, taskID, request, taskStartSource{})
	if err != nil {
		s.writeTaskStartError(w, err)
		return
	}
	runResponse := runResponse(result.run)
	status := http.StatusCreated
	if result.idempotencyHit || result.sessionReused {
		status = http.StatusOK
	}
	writeJSON(w, status, api.TaskStartResponse{
		Session:  taskSessionResponse(result.session),
		Run:      runResponse,
		IsCached: result.idempotencyHit || result.sessionReused,
	})
}

func (s *Server) startTaskAndWait(w http.ResponseWriter, r *http.Request) {
	taskID := strings.TrimSpace(chi.URLParam(r, "taskID"))
	if err := api.ValidateTaskID(taskID); err != nil {
		writeError(w, badRequest(err))
		return
	}
	actor := actorFromContext(r.Context())
	var request api.TaskStartAndWaitRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid task start-and-wait request JSON: %w", err)))
		return
	}
	scope, projectID, environmentID, err := s.requestEnvironmentScopeFromRequest(r, actor, request.ProjectID, request.EnvironmentID)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	result, err := s.startTaskSessionFromRequestInScope(contextWithRequestVersionMetadata(r.Context(), r), actor, scope, projectID, environmentID, taskID, request.TaskStartRequest, taskStartSource{})
	if err != nil {
		s.writeTaskStartError(w, err)
		return
	}
	session, timedOut, err := s.waitForTaskSession(r.Context(), actor, result.session.ProjectID, result.session.EnvironmentID, pgvalue.MustUUIDValue(result.session.ID), waitTimeout(request.TimeoutSeconds))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, taskSessionWaitResponse(session, timedOut))
}

func (s *Server) startTaskSessionFromRequest(ctx context.Context, actor auth.Actor, taskID string, request api.TaskStartRequest, source taskStartSource) (taskStartResult, error) {
	if s.db == nil {
		return taskStartResult{}, errors.New("task session storage is not configured")
	}
	taskID = strings.TrimSpace(taskID)
	if err := api.ValidateTaskID(taskID); err != nil {
		return taskStartResult{}, err
	}
	scope, projectID, environmentID, err := s.requestEnvironmentScope(ctx, actor, request.ProjectID, request.EnvironmentID)
	if err != nil {
		return taskStartResult{}, err
	}
	return s.startTaskSessionFromRequestInScope(ctx, actor, scope, projectID, environmentID, taskID, request, source)
}

func (s *Server) startTaskSessionFromRequestInScope(ctx context.Context, actor auth.Actor, scope auth.Scope, projectID pgtype.UUID, environmentID pgtype.UUID, taskID string, request api.TaskStartRequest, source taskStartSource) (taskStartResult, error) {
	if !actor.HasPermission(auth.PermissionRunsCreate, scope) {
		return taskStartResult{}, errPermissionRequired
	}
	runOptions := taskStartRunOptions(request.Options)
	idempotency, err := normalizeRunIdempotency(runOptions)
	if err != nil {
		return taskStartResult{}, err
	}
	payload := request.Payload
	if len(payload) == 0 {
		payload = json.RawMessage(`{}`)
	}
	if !json.Valid(payload) {
		return taskStartResult{}, errors.New("payload must be valid JSON")
	}
	externalID := strings.TrimSpace(request.ExternalID)
	if err := validateTaskSessionExternalID(externalID); err != nil {
		return taskStartResult{}, err
	}
	requestedWorkspaceID, err := parseOptionalWorkspaceID(request.Options.WorkspaceID)
	if err != nil {
		return taskStartResult{}, err
	}
	metadata, err := normalizedJSONObject(request.Options.Metadata, "metadata")
	if err != nil {
		return taskStartResult{}, err
	}
	tags, err := normalizedRunTags(request.Options.Tags)
	if err != nil {
		return taskStartResult{}, err
	}
	startFingerprint, err := taskStartRequestFingerprint(taskID, payload, request.Options, metadata, tags, externalID, request.Options.ExpiresAt)
	if err != nil {
		return taskStartResult{}, err
	}
	idempotencyFingerprint := pgtype.Text{}
	if idempotency.key.Valid {
		idempotencyFingerprint = startFingerprint
		if existing, hit, err := s.existingTaskStartIdempotency(ctx, actor.OrgID, projectID, environmentID, taskID, idempotency.key.String, idempotencyFingerprint.String, externalID); err != nil {
			return taskStartResult{}, err
		} else if hit {
			if err := s.ensureTaskStartSourceCurrent(ctx, source); err != nil {
				return taskStartResult{}, err
			}
			return existing, nil
		}
	}
	if externalID != "" && !idempotency.key.Valid {
		if existing, err := s.loadExistingTaskSessionStart(ctx, s.db, actor.OrgID, projectID, environmentID, taskID, externalID, startFingerprint.String, idempotency, idempotencyFingerprint.String, source); err == nil {
			return existing, nil
		} else if !isNoRows(err) {
			return taskStartResult{}, err
		}
	}
	task, err := s.db.GetTaskForStart(ctx, db.GetTaskForStartParams{
		OrgID:         pgvalue.UUID(actor.OrgID),
		ProjectID:     projectID,
		EnvironmentID: environmentID,
		TaskID:        taskID,
	})
	if isNoRows(err) {
		return taskStartResult{}, errTaskNotDeployed
	}
	if err != nil {
		return taskStartResult{}, err
	}
	if task.ArchivedAt.Valid {
		return taskStartResult{}, errTaskArchived
	}
	deploymentTask, err := s.deploymentTaskForRunRequest(ctx, actor.OrgID, projectID, environmentID, taskID, runDeploymentSelection{})
	if isNoRows(err) {
		return taskStartResult{}, errTaskNotDeployed
	}
	if err != nil {
		return taskStartResult{}, err
	}
	secretNames, err := deploymentTaskSecretNames(deploymentTask.SecretDeclarations)
	if err != nil {
		return taskStartResult{}, err
	}
	if len(secretNames) > 0 {
		if s.secrets == nil {
			return taskStartResult{}, errors.New("secret store is not configured")
		}
		projectUUID, err := pgvalue.UUIDValue(projectID)
		if err != nil {
			return taskStartResult{}, fmt.Errorf("project id is invalid: %v", err)
		}
		environmentUUID, err := pgvalue.UUIDValue(environmentID)
		if err != nil {
			return taskStartResult{}, fmt.Errorf("environment id is invalid: %v", err)
		}
		if err := s.secrets.CheckScopedNames(ctx, actor.OrgID, projectUUID, environmentUUID, secretNames); err != nil {
			return taskStartResult{}, err
		}
	}
	maxDurationSeconds, err := runMaxDurationSeconds(request.Options.MaxDurationSeconds, deploymentTask.MaxActiveDurationMs)
	if err != nil {
		return taskStartResult{}, err
	}
	lockedRetryPolicy, err := resolvedRetryPolicy(request.Options.Retry, deploymentTask.RetryPolicy)
	if err != nil {
		return taskStartResult{}, err
	}
	scheduling, err := s.resolveRunScheduling(runOptions, deploymentTask)
	if err != nil {
		return taskStartResult{}, err
	}
	scheduling, err = s.validateRunQueueOverride(ctx, actor.OrgID, projectID, environmentID, runOptions, deploymentTask, scheduling)
	if err != nil {
		return taskStartResult{}, err
	}
	startClaim, err := s.claimTaskStart(ctx, actor.OrgID, projectID, environmentID, taskID, idempotency.key.String, idempotency.expiresAt)
	if err != nil {
		return taskStartResult{}, err
	}
	claimResolved := false
	defer func() {
		if !claimResolved {
			startClaim.release(context.WithoutCancel(ctx))
		}
	}()
	resolvedClaimIsStale := false
	if startClaim.resolved && idempotency.key.Valid {
		if existing, hit, err := s.existingTaskStartIdempotency(ctx, actor.OrgID, projectID, environmentID, taskID, idempotency.key.String, idempotencyFingerprint.String, externalID); err != nil {
			return taskStartResult{}, err
		} else if hit {
			if err := s.ensureTaskStartSourceCurrent(ctx, source); err != nil {
				return taskStartResult{}, err
			}
			return existing, nil
		}
		startClaim.clearResolved(context.WithoutCancel(ctx))
		startClaim, err = s.claimTaskStart(ctx, actor.OrgID, projectID, environmentID, taskID, idempotency.key.String, idempotency.expiresAt)
		if err != nil {
			return taskStartResult{}, err
		}
		if startClaim.resolved {
			return taskStartResult{}, errTaskStartPending
		}
		resolvedClaimIsStale = true
	}
	versionMetadata := requestVersionMetadataFromContext(ctx)
	runID := uuid.Must(uuid.NewV7())
	sessionID := uuid.Must(uuid.NewV7())
	traceID, err := tracing.NewTraceID()
	if err != nil {
		return taskStartResult{}, fmt.Errorf("generate run trace id: %w", err)
	}
	rootSpanID, err := tracing.NewSpanID()
	if err != nil {
		return taskStartResult{}, fmt.Errorf("generate run root span id: %w", err)
	}
	createdPayload, err := runCreatedEventPayload(taskID, payload, maxDurationSeconds, secretNames, lockedRetryPolicy, metadata, tags)
	if err != nil {
		return taskStartResult{}, fmt.Errorf("encode run created event: %w", err)
	}
	store, tx, err := s.beginControlTransaction(ctx)
	if err != nil {
		return taskStartResult{}, err
	}
	defer func() {
		if tx != nil {
			_ = tx.Rollback(ctx)
		}
	}()
	if externalID != "" {
		if existing, err := store.GetTaskSessionByExternalID(ctx, db.GetTaskSessionByExternalIDParams{
			OrgID:         pgvalue.UUID(actor.OrgID),
			ProjectID:     projectID,
			EnvironmentID: environmentID,
			TaskID:        taskID,
			ExternalID:    externalID,
		}); err == nil {
			if !taskSessionStartReusable(existing) {
				return taskStartResult{}, errTaskSessionTerminated
			}
			if existing.StartFingerprint != startFingerprint.String {
				return taskStartResult{}, errTaskStartSessionFingerprint
			}
			if !existing.CurrentRunID.Valid {
				return taskStartResult{}, errTaskSessionNoCurrentRun
			}
			if idempotency.key.Valid {
				if existingResult, existingHit, err := s.createTaskStartIdempotency(ctx, store, taskStartIdempotencyBinding{
					ID:                 pgvalue.UUID(uuid.Must(uuid.NewV7())),
					OrgID:              pgvalue.UUID(actor.OrgID),
					ProjectID:          projectID,
					EnvironmentID:      environmentID,
					TaskID:             taskID,
					IdempotencyKey:     idempotency.key.String,
					RequestFingerprint: idempotencyFingerprint.String,
					TaskSessionID:      existing.ID,
					FirstRunID:         existing.CurrentRunID,
					ExpiresAt:          idempotency.expiresAt,
				}, externalID, source); err != nil {
					return taskStartResult{}, err
				} else if existingHit {
					return existingResult, nil
				}
			}
			runRow, err := store.GetRunSummary(ctx, db.GetRunSummaryParams{OrgID: pgvalue.UUID(actor.OrgID), ID: existing.CurrentRunID})
			if err != nil {
				return taskStartResult{}, err
			}
			if err := tx.Commit(ctx); err != nil {
				return taskStartResult{}, err
			}
			tx = nil
			startClaim.resolve(context.WithoutCancel(ctx))
			claimResolved = true
			return taskStartResult{session: existing, run: getRunSummary(runRow), idempotencyHit: idempotency.key.Valid, sessionReused: true}, nil
		} else if !isNoRows(err) {
			return taskStartResult{}, err
		}
	}
	if startClaim.resolved && !resolvedClaimIsStale {
		return taskStartResult{}, errTaskStartPending
	}
	workspace, err := s.createOrAttachTaskStartWorkspace(ctx, store, actor.OrgID, projectID, environmentID, deploymentTask, requestedWorkspaceID)
	if err != nil {
		return taskStartResult{}, err
	}
	session, err := store.CreateTaskSession(ctx, db.CreateTaskSessionParams{
		ID:                  pgvalue.UUID(sessionID),
		OrgID:               pgvalue.UUID(actor.OrgID),
		ProjectID:           projectID,
		EnvironmentID:       environmentID,
		TaskID:              taskID,
		InitialDeploymentID: deploymentTask.DeploymentID,
		ActiveDeploymentID:  deploymentTask.DeploymentID,
		WorkspaceID:         workspace.ID,
		ExternalID:          externalID,
		StartFingerprint:    startFingerprint.String,
		Metadata:            metadata,
		Tags:                tags,
		ExpiresAt:           timePtrToTimestamptz(request.Options.ExpiresAt),
	})
	if err != nil {
		if isUniqueViolation(err) && externalID != "" {
			_ = tx.Rollback(ctx)
			tx = nil
			existing, err := s.loadExistingTaskSessionStart(ctx, s.db, actor.OrgID, projectID, environmentID, taskID, externalID, startFingerprint.String, idempotency, idempotencyFingerprint.String, source)
			if err != nil {
				return taskStartResult{}, err
			}
			startClaim.resolve(context.WithoutCancel(ctx))
			claimResolved = true
			return existing, nil
		}
		return taskStartResult{}, err
	}
	if err := s.ensureTaskSessionStreams(ctx, store, pgvalue.UUID(actor.OrgID), projectID, environmentID, deploymentTask.DeploymentID, taskID, session.ID); err != nil {
		return taskStartResult{}, err
	}
	run, err := store.CreateScopedRun(ctx, db.CreateScopedRunParams{
		ID:                    pgvalue.UUID(runID),
		OrgID:                 pgvalue.UUID(actor.OrgID),
		ProjectID:             projectID,
		EnvironmentID:         environmentID,
		DeploymentID:          deploymentTask.DeploymentID,
		DeploymentTaskID:      deploymentTask.ID,
		WorkspaceID:           workspace.ID,
		DeploymentVersion:     deploymentTask.DeploymentVersion,
		ApiVersion:            versionMetadata.APIVersion,
		SdkVersion:            firstNonEmptyString(versionMetadata.SDKVersion, deploymentTask.SdkVersion),
		CliVersion:            firstNonEmptyString(versionMetadata.CLIVersion, deploymentTask.CliVersion),
		TaskID:                taskID,
		TaskSessionID:         session.ID,
		Payload:               payload,
		Metadata:              metadata,
		Tags:                  tags,
		LockedRetryPolicy:     lockedRetryPolicy,
		QueueName:             scheduling.queueName,
		QueueConcurrencyLimit: scheduling.queueConcurrencyLimit,
		ConcurrencyKey:        scheduling.concurrencyKey,
		Priority:              scheduling.priority,
		QueueTimestamp:        scheduling.queueTimestamp,
		Ttl:                   scheduling.ttl,
		QueuedExpiresAt:       scheduling.queuedExpiresAt,
		MaxActiveDurationMs:   int64(maxDurationSeconds) * 1000,
		TraceID:               traceID,
		RootSpanID:            rootSpanID,
		EventPayload:          createdPayload,
		ScheduleID:            source.scheduleID,
		ScheduleInstanceID:    source.scheduleInstanceID,
		ScheduleGeneration:    pgtype.Int8{Int64: source.scheduleGeneration, Valid: source.scheduleInstanceID.Valid},
		ScheduledAt:           source.scheduledAt,
	})
	if err != nil {
		if isNoRows(err) && source.scheduleInstanceID.Valid {
			return taskStartResult{}, schedule.ErrTriggerSuperseded
		}
		return taskStartResult{}, err
	}
	materializationRequest, err := json.Marshal(map[string]string{
		"source": "task_start",
		"run_id": pgvalue.MustUUIDValue(run.ID).String(),
	})
	if err != nil {
		return taskStartResult{}, err
	}
	materialization, err := store.EnsureWorkspaceMaterializationRequested(ctx, db.EnsureWorkspaceMaterializationRequestedParams{
		ID:            pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:         pgvalue.UUID(actor.OrgID),
		ProjectID:     projectID,
		EnvironmentID: environmentID,
		WorkspaceID:   workspace.ID,
		Priority:      scheduling.priority,
		Request:       materializationRequest,
	})
	if err != nil {
		if isNoRows(err) {
			return taskStartResult{}, s.workspaceMaterializationPrerequisiteErrorWithStore(ctx, store, pgvalue.UUID(actor.OrgID), projectID, environmentID, workspace.ID)
		}
		return taskStartResult{}, err
	}
	if err := store.SetQueuedRunWorkspaceMaterialization(ctx, db.SetQueuedRunWorkspaceMaterializationParams{
		OrgID:                      pgvalue.UUID(actor.OrgID),
		RunID:                      run.ID,
		WorkspaceID:                workspace.ID,
		WorkspaceMaterializationID: materialization.ID,
	}); err != nil {
		return taskStartResult{}, err
	}
	if _, err := store.CreateTaskSessionRun(ctx, db.CreateTaskSessionRunParams{
		ID:            pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:         pgvalue.UUID(actor.OrgID),
		ProjectID:     projectID,
		EnvironmentID: environmentID,
		TaskSessionID: session.ID,
		RunID:         run.ID,
		DeploymentID:  deploymentTask.DeploymentID,
		TurnIndex:     0,
	}); err != nil {
		return taskStartResult{}, err
	}
	session, err = store.SetTaskSessionCurrentRun(ctx, db.SetTaskSessionCurrentRunParams{
		OrgID:         pgvalue.UUID(actor.OrgID),
		ProjectID:     projectID,
		EnvironmentID: environmentID,
		TaskSessionID: session.ID,
		RunID:         run.ID,
	})
	if err != nil {
		return taskStartResult{}, err
	}
	if idempotency.key.Valid {
		if existingResult, existingHit, err := s.createTaskStartIdempotency(ctx, store, taskStartIdempotencyBinding{
			ID:                 pgvalue.UUID(uuid.Must(uuid.NewV7())),
			OrgID:              pgvalue.UUID(actor.OrgID),
			ProjectID:          projectID,
			EnvironmentID:      environmentID,
			TaskID:             taskID,
			IdempotencyKey:     idempotency.key.String,
			RequestFingerprint: idempotencyFingerprint.String,
			TaskSessionID:      session.ID,
			FirstRunID:         run.ID,
			ExpiresAt:          idempotency.expiresAt,
		}, externalID, source); err != nil {
			return taskStartResult{}, err
		} else if existingHit {
			return existingResult, nil
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return taskStartResult{}, err
	}
	tx = nil
	startClaim.resolve(context.WithoutCancel(ctx))
	claimResolved = true
	if s.runEnqueuer != nil {
		if _, err := s.runEnqueuer.EnqueueRun(ctx, run.OrgID, run.ID); err != nil {
			s.log.Error("enqueue task session run failed", "run_id", pgvalue.MustUUIDValue(run.ID).String(), "error", err)
		}
	}
	return taskStartResult{session: session, run: createScopedRunSummary(run)}, nil
}

func (s *Server) ensureTaskSessionStreams(ctx context.Context, store db.Querier, orgID pgtype.UUID, projectID pgtype.UUID, environmentID pgtype.UUID, deploymentID pgtype.UUID, taskID string, sessionID pgtype.UUID) error {
	streams, err := store.ListDeploymentStreamsForTask(ctx, db.ListDeploymentStreamsForTaskParams{
		OrgID:         orgID,
		ProjectID:     projectID,
		EnvironmentID: environmentID,
		DeploymentID:  deploymentID,
		TaskID:        taskID,
	})
	if err != nil {
		return err
	}
	for _, stream := range streams {
		if _, err := store.EnsureSessionStream(ctx, db.EnsureSessionStreamParams{
			ID:                 pgvalue.UUID(uuid.Must(uuid.NewV7())),
			Metadata:           []byte("{}"),
			DeploymentStreamID: stream.ID,
			OrgID:              orgID,
			ProjectID:          projectID,
			EnvironmentID:      environmentID,
			SessionID:          sessionID,
		}); err != nil {
			return err
		}
	}
	return nil
}

func validateTaskSessionExternalID(value string) error {
	if strings.ContainsRune(value, 0) {
		return errors.New("external_id must not contain NUL bytes")
	}
	if len(value) > maxTaskSessionExternalIDBytes {
		return fmt.Errorf("external_id must be at most %d bytes", maxTaskSessionExternalIDBytes)
	}
	return nil
}

func parseOptionalWorkspaceID(raw string) (pgtype.UUID, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return pgtype.UUID{}, nil
	}
	parsed, err := uuid.Parse(raw)
	if err != nil {
		return pgtype.UUID{}, fmt.Errorf("workspace_id is invalid: %w", err)
	}
	return pgvalue.UUID(parsed), nil
}

func (s *Server) createOrAttachTaskStartWorkspace(ctx context.Context, store db.Querier, orgID uuid.UUID, projectID pgtype.UUID, environmentID pgtype.UUID, task db.GetDeploymentTaskRow, requestedWorkspaceID pgtype.UUID) (db.Workspace, error) {
	if !requestedWorkspaceID.Valid {
		workspaceArtifact, initialWorkspace, err := s.createInitialWorkspaceArtifact(ctx, store, orgID, projectID, environmentID)
		if err != nil {
			return db.Workspace{}, err
		}
		workspace, err := store.CreateWorkspaceFromSandbox(ctx, db.CreateWorkspaceFromSandboxParams{
			ID:                        pgvalue.UUID(uuid.Must(uuid.NewV7())),
			OrgID:                     pgvalue.UUID(orgID),
			ProjectID:                 projectID,
			EnvironmentID:             environmentID,
			DeploymentSandboxID:       task.DeploymentSandboxID,
			ExternalID:                "",
			Metadata:                  []byte(`{}`),
			Tags:                      []string{},
			RetentionPolicy:           []byte(`{}`),
			InitialVersionID:          pgvalue.UUID(uuid.Must(uuid.NewV7())),
			InitialArtifactID:         workspaceArtifact.ID,
			InitialArtifactEncoding:   initialWorkspace.Encoding,
			InitialArtifactEntryCount: int32(initialWorkspace.EntryCount),
			InitialContentDigest:      workspaceArtifact.Digest,
			InitialSizeBytes:          workspaceArtifact.SizeBytes,
		})
		if isNoRows(err) {
			return db.Workspace{}, errSandboxNotDeployed
		}
		if err != nil {
			return db.Workspace{}, err
		}
		return workspaceFromCreateWorkspaceFromSandbox(workspace), nil
	}
	workspace, err := store.GetWorkspaceForTaskStart(ctx, db.GetWorkspaceForTaskStartParams{
		OrgID:         pgvalue.UUID(orgID),
		ProjectID:     projectID,
		EnvironmentID: environmentID,
		WorkspaceID:   requestedWorkspaceID,
	})
	if isNoRows(err) {
		return db.Workspace{}, errWorkspaceSandboxIncompatible
	}
	if err != nil {
		return db.Workspace{}, err
	}
	if !workspace.DeploymentSandboxID.Valid {
		return db.Workspace{}, errSandboxNotDeployed
	}
	if workspace.ArchivedAt.Valid || workspace.DeletedAt.Valid || workspace.State != db.WorkspaceStateActive {
		return db.Workspace{}, errWorkspaceSandboxIncompatible
	}
	if workspace.SandboxID != task.SandboxID ||
		workspace.SandboxFingerprint != task.SandboxFingerprint ||
		workspace.DeploymentSandboxFingerprint != task.SandboxFingerprint {
		return db.Workspace{}, errWorkspaceSandboxIncompatible
	}
	if workspace.WorkspaceMountPath != task.WorkspaceMountPath ||
		workspace.DeploymentSandboxRuntimeAbi != task.DeploymentSandboxRuntimeAbi ||
		workspace.DeploymentSandboxGuestdAbi != task.DeploymentSandboxGuestdAbi ||
		workspace.DeploymentSandboxAdapterAbi != task.DeploymentSandboxAdapterAbi ||
		workspace.DeploymentSandboxFilesystemFormat != task.DeploymentSandboxFilesystemFormat ||
		workspace.DeploymentSandboxContractVersion != task.DeploymentSandboxContractVersion {
		return db.Workspace{}, errWorkspaceSandboxIncompatible
	}
	if !jsonPayloadsEqual(workspace.DeploymentSandboxNetworkPolicy, task.DeploymentSandboxNetworkPolicy) {
		return db.Workspace{}, errWorkspaceSandboxIncompatible
	}
	if err := workspaceResourceFloorSatisfies(workspace, task); err != nil {
		return db.Workspace{}, err
	}
	return db.Workspace{
		ID:                  workspace.ID,
		OrgID:               workspace.OrgID,
		ProjectID:           workspace.ProjectID,
		EnvironmentID:       workspace.EnvironmentID,
		DeploymentSandboxID: workspace.DeploymentSandboxID,
		SandboxID:           workspace.SandboxID,
		SandboxFingerprint:  workspace.SandboxFingerprint,
		State:               workspace.State,
		ArchivedAt:          workspace.ArchivedAt,
		DeletedAt:           workspace.DeletedAt,
	}, nil
}

func jsonPayloadsEqual(left []byte, right []byte) bool {
	leftCanonical, err := canonicalJSON(left)
	if err != nil {
		return false
	}
	rightCanonical, err := canonicalJSON(right)
	if err != nil {
		return false
	}
	return bytes.Equal(leftCanonical, rightCanonical)
}

func workspaceResourceFloorSatisfies(workspace db.GetWorkspaceForTaskStartRow, task db.GetDeploymentTaskRow) error {
	floor, err := decodeWorkspaceResourceFloor(workspace.DeploymentSandboxResourceFloor)
	if err != nil {
		return errWorkspaceResourceFloor
	}
	if task.RequestedMilliCpu > floor.milliCPU ||
		task.RequestedMemoryMib > floor.memoryMiB ||
		task.RequestedDiskMib > workspace.DeploymentSandboxDiskFloorMib {
		return errWorkspaceResourceFloor
	}
	return nil
}

type workspaceResourceFloor struct {
	milliCPU  int64
	memoryMiB int64
}

func decodeWorkspaceResourceFloor(raw []byte) (workspaceResourceFloor, error) {
	var decoded struct {
		MilliCPU  int64 `json:"milli_cpu"`
		MemoryMiB int64 `json:"memory_mib"`
	}
	if len(raw) == 0 {
		return workspaceResourceFloor{}, errors.New("workspace resource floor is empty")
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return workspaceResourceFloor{}, err
	}
	return workspaceResourceFloor{milliCPU: decoded.MilliCPU, memoryMiB: decoded.MemoryMiB}, nil
}

func (s *Server) createTaskStartIdempotency(ctx context.Context, store db.Querier, binding taskStartIdempotencyBinding, externalID string, source taskStartSource) (taskStartResult, bool, error) {
	created, existingResult, existingHit, err := s.tryCreateTaskStartIdempotency(ctx, store, binding, externalID, source)
	if err != nil || created.ID.Valid || existingHit {
		return existingResult, existingHit, err
	}
	if err := store.DeleteExpiredTaskStartIdempotency(ctx, db.DeleteExpiredTaskStartIdempotencyParams{
		OrgID:          binding.OrgID,
		ProjectID:      binding.ProjectID,
		EnvironmentID:  binding.EnvironmentID,
		TaskID:         binding.TaskID,
		IdempotencyKey: binding.IdempotencyKey,
	}); err != nil {
		return taskStartResult{}, false, err
	}
	created, existingResult, existingHit, err = s.tryCreateTaskStartIdempotency(ctx, store, binding, externalID, source)
	if err != nil || created.ID.Valid || existingHit {
		return existingResult, existingHit, err
	}
	return taskStartResult{}, false, errTaskStartPending
}

func (s *Server) tryCreateTaskStartIdempotency(ctx context.Context, store db.Querier, binding taskStartIdempotencyBinding, externalID string, source taskStartSource) (db.TaskStartIdempotency, taskStartResult, bool, error) {
	created, err := store.CreateTaskStartIdempotency(ctx, db.CreateTaskStartIdempotencyParams{
		ID:                 binding.ID,
		OrgID:              binding.OrgID,
		ProjectID:          binding.ProjectID,
		EnvironmentID:      binding.EnvironmentID,
		TaskID:             binding.TaskID,
		IdempotencyKey:     binding.IdempotencyKey,
		RequestFingerprint: binding.RequestFingerprint,
		TaskSessionID:      binding.TaskSessionID,
		FirstRunID:         binding.FirstRunID,
		ExpiresAt:          binding.ExpiresAt,
	})
	if err == nil {
		return created, taskStartResult{}, false, nil
	}
	if !isNoRows(err) {
		return db.TaskStartIdempotency{}, taskStartResult{}, false, err
	}
	if existingResult, hit, hitErr := s.existingTaskStartIdempotency(ctx, pgvalue.MustUUIDValue(binding.OrgID), binding.ProjectID, binding.EnvironmentID, binding.TaskID, binding.IdempotencyKey, binding.RequestFingerprint, externalID); hitErr != nil {
		return db.TaskStartIdempotency{}, taskStartResult{}, false, hitErr
	} else if hit {
		if err := s.ensureTaskStartSourceCurrent(ctx, source); err != nil {
			return db.TaskStartIdempotency{}, taskStartResult{}, false, err
		}
		return db.TaskStartIdempotency{
			ID:                 binding.ID,
			OrgID:              binding.OrgID,
			ProjectID:          binding.ProjectID,
			EnvironmentID:      binding.EnvironmentID,
			TaskID:             binding.TaskID,
			IdempotencyKey:     binding.IdempotencyKey,
			RequestFingerprint: binding.RequestFingerprint,
			TaskSessionID:      existingResult.session.ID,
			FirstRunID:         existingResult.run.ID,
			ExpiresAt:          binding.ExpiresAt,
		}, existingResult, true, nil
	}
	return db.TaskStartIdempotency{}, taskStartResult{}, false, nil
}

func (s *Server) loadExistingTaskSessionStart(ctx context.Context, store db.Querier, orgID uuid.UUID, projectID pgtype.UUID, environmentID pgtype.UUID, taskID string, externalID string, startFingerprint string, idempotency runIdempotency, idempotencyFingerprint string, source taskStartSource) (taskStartResult, error) {
	existing, err := store.GetTaskSessionByExternalID(ctx, db.GetTaskSessionByExternalIDParams{
		OrgID:         pgvalue.UUID(orgID),
		ProjectID:     projectID,
		EnvironmentID: environmentID,
		TaskID:        taskID,
		ExternalID:    externalID,
	})
	if err != nil {
		return taskStartResult{}, err
	}
	if !taskSessionStartReusable(existing) {
		return taskStartResult{}, errTaskSessionTerminated
	}
	if existing.StartFingerprint != startFingerprint {
		return taskStartResult{}, errTaskStartSessionFingerprint
	}
	if !existing.CurrentRunID.Valid {
		return taskStartResult{}, errTaskSessionNoCurrentRun
	}
	if idempotency.key.Valid {
		if existingResult, existingHit, err := s.createTaskStartIdempotency(ctx, store, taskStartIdempotencyBinding{
			ID:                 pgvalue.UUID(uuid.Must(uuid.NewV7())),
			OrgID:              pgvalue.UUID(orgID),
			ProjectID:          projectID,
			EnvironmentID:      environmentID,
			TaskID:             taskID,
			IdempotencyKey:     idempotency.key.String,
			RequestFingerprint: idempotencyFingerprint,
			TaskSessionID:      existing.ID,
			FirstRunID:         existing.CurrentRunID,
			ExpiresAt:          idempotency.expiresAt,
		}, externalID, source); err != nil {
			return taskStartResult{}, err
		} else if existingHit {
			return existingResult, nil
		}
	}
	runRow, err := store.GetRunSummary(ctx, db.GetRunSummaryParams{OrgID: pgvalue.UUID(orgID), ID: existing.CurrentRunID})
	if err != nil {
		return taskStartResult{}, err
	}
	return taskStartResult{session: existing, run: getRunSummary(runRow), idempotencyHit: idempotency.key.Valid, sessionReused: true}, nil
}

func (s *Server) ensureTaskStartSourceCurrent(ctx context.Context, source taskStartSource) error {
	if !source.scheduleInstanceID.Valid {
		return nil
	}
	current, err := s.db.ScheduleInstanceTriggerIsCurrent(ctx, db.ScheduleInstanceTriggerIsCurrentParams{
		InstanceID:    source.scheduleInstanceID,
		Generation:    source.scheduleGeneration,
		ScheduledAt:   source.scheduledAt,
		ScheduleID:    source.scheduleID,
		OrgID:         source.scheduleOrgID,
		ProjectID:     source.scheduleProjectID,
		EnvironmentID: source.scheduleEnvironmentID,
	})
	if err != nil {
		return err
	}
	if !current {
		return schedule.ErrTriggerSuperseded
	}
	return nil
}

func taskStartRunOptions(options api.TaskStartOptions) api.CreateRunOptions {
	return api.CreateRunOptions{
		Queue:              options.Queue,
		ConcurrencyKey:     options.ConcurrencyKey,
		Priority:           options.Priority,
		TTL:                options.TTL,
		MaxDurationSeconds: options.MaxDurationSeconds,
		Retry:              options.Retry,
		Metadata:           options.Metadata,
		Tags:               options.Tags,
		IdempotencyKey:     options.IdempotencyKey,
		IdempotencyKeyTTL:  options.IdempotencyKeyTTL,
	}
}

func taskStartRequestFingerprint(taskID string, payload json.RawMessage, options api.TaskStartOptions, metadata []byte, tags []string, externalID string, expiresAt *time.Time) (pgtype.Text, error) {
	canonicalPayload, err := canonicalJSON(payload)
	if err != nil {
		return pgtype.Text{}, fmt.Errorf("payload canonicalization failed: %w", err)
	}
	var retry json.RawMessage
	if len(options.Retry) > 0 {
		retry, err = canonicalJSON(options.Retry)
		if err != nil {
			return pgtype.Text{}, fmt.Errorf("retry canonicalization failed: %w", err)
		}
	}
	fingerprint := struct {
		TaskID     string          `json:"task_id"`
		Payload    json.RawMessage `json:"payload"`
		ExternalID string          `json:"external_id,omitempty"`
		ExpiresAt  string          `json:"expires_at,omitempty"`
		Metadata   json.RawMessage `json:"metadata"`
		Tags       []string        `json:"tags"`
		Options    struct {
			QueueName          string `json:"queue_name,omitempty"`
			ConcurrencyKey     string `json:"concurrency_key,omitempty"`
			Priority           int32  `json:"priority,omitempty"`
			TTL                string `json:"ttl,omitempty"`
			MaxDurationSeconds int32  `json:"max_duration_seconds,omitempty"`
			WorkspaceID        string `json:"workspace_id,omitempty"`
		} `json:"options"`
		RetryPolicy json.RawMessage `json:"retry_policy,omitempty"`
	}{
		TaskID:      taskID,
		Payload:     canonicalPayload,
		ExternalID:  strings.TrimSpace(externalID),
		Metadata:    json.RawMessage(metadata),
		Tags:        append([]string(nil), tags...),
		RetryPolicy: retry,
	}
	if expiresAt != nil {
		fingerprint.ExpiresAt = expiresAt.UTC().Format(time.RFC3339Nano)
	}
	if options.Queue != nil {
		fingerprint.Options.QueueName = strings.TrimSpace(options.Queue.Name)
	}
	fingerprint.Options.ConcurrencyKey = strings.TrimSpace(options.ConcurrencyKey)
	fingerprint.Options.Priority = options.Priority
	fingerprint.Options.TTL = strings.TrimSpace(options.TTL)
	fingerprint.Options.MaxDurationSeconds = options.MaxDurationSeconds
	fingerprint.Options.WorkspaceID = strings.TrimSpace(options.WorkspaceID)
	body, err := json.Marshal(fingerprint)
	if err != nil {
		return pgtype.Text{}, fmt.Errorf("task start fingerprint encode failed: %w", err)
	}
	digest := sha256.Sum256(body)
	return pgtype.Text{String: hex.EncodeToString(digest[:]), Valid: true}, nil
}

func (s *Server) existingTaskStartIdempotency(ctx context.Context, orgID uuid.UUID, projectID pgtype.UUID, environmentID pgtype.UUID, taskID string, key string, fingerprint string, externalID string) (taskStartResult, bool, error) {
	existing, err := s.db.GetTaskStartIdempotency(ctx, db.GetTaskStartIdempotencyParams{
		OrgID:          pgvalue.UUID(orgID),
		ProjectID:      projectID,
		EnvironmentID:  environmentID,
		TaskID:         taskID,
		IdempotencyKey: key,
	})
	if isNoRows(err) {
		return taskStartResult{}, false, nil
	}
	if err != nil {
		return taskStartResult{}, false, err
	}
	if existing.RequestFingerprint != fingerprint {
		return taskStartResult{}, false, errTaskStartIdempotencyFingerprint
	}
	session := taskSessionFromIdempotency(existing)
	if strings.TrimSpace(externalID) != "" && session.ExternalID != strings.TrimSpace(externalID) {
		return taskStartResult{}, false, errTaskStartIdempotencyExternalID
	}
	_ = s.db.TouchTaskStartIdempotency(ctx, db.TouchTaskStartIdempotencyParams{OrgID: pgvalue.UUID(orgID), ID: existing.ID})
	return taskStartResult{session: session, run: runSummaryFromIdempotency(existing), idempotencyHit: true}, true, nil
}

func taskSessionFromIdempotency(row db.GetTaskStartIdempotencyRow) db.TaskSession {
	return db.TaskSession{
		ID:                  row.SessionID,
		OrgID:               row.SessionOrgID,
		ProjectID:           row.SessionProjectID,
		EnvironmentID:       row.SessionEnvironmentID,
		TaskID:              row.SessionTaskID,
		InitialDeploymentID: row.SessionInitialDeploymentID,
		ActiveDeploymentID:  row.SessionActiveDeploymentID,
		ExternalID:          row.SessionExternalID,
		StartFingerprint:    row.SessionStartFingerprint,
		Status:              row.SessionStatus,
		CurrentRunID:        row.SessionCurrentRunID,
		CurrentRunVersion:   row.SessionCurrentRunVersion,
		WorkspaceID:         row.SessionWorkspaceID,
		Metadata:            row.SessionMetadata,
		Tags:                row.SessionTags,
		Result:              row.SessionResult,
		TerminalReason:      row.SessionTerminalReason,
		ExpiresAt:           row.SessionExpiresAt,
		CompletedAt:         row.SessionCompletedAt,
		FailedAt:            row.SessionFailedAt,
		CancelledAt:         row.SessionCancelledAt,
		CreatedAt:           row.SessionCreatedAt,
		UpdatedAt:           row.SessionUpdatedAt,
	}
}

func runSummaryFromIdempotency(row db.GetTaskStartIdempotencyRow) runSummary {
	return runSummary{
		ID:                   row.RunID,
		OrgID:                row.RunOrgID,
		ProjectID:            row.RunProjectID,
		EnvironmentID:        row.RunEnvironmentID,
		DeploymentID:         row.RunDeploymentID,
		DeploymentTaskID:     row.RunDeploymentTaskID,
		TaskSessionID:        row.TaskSessionID,
		DeploymentVersion:    row.RunDeploymentVersion,
		APIVersion:           row.RunApiVersion,
		SDKVersion:           row.RunSdkVersion,
		CLIVersion:           row.RunCliVersion,
		TaskID:               row.RunTaskID,
		Status:               row.RunStatus,
		ExecutionStatus:      row.RunExecutionStatus,
		TerminalOutcome:      row.RunTerminalOutcome,
		Metadata:             row.RunMetadata,
		Tags:                 row.RunTags,
		CurrentAttemptNumber: row.RunAttemptNumber,
		ExitCode:             row.RunExitCode,
		Output:               row.RunOutput,
		CreatedAt:            row.RunCreatedAt,
		UpdatedAt:            row.RunUpdatedAt,
	}
}

func timePtrToTimestamptz(value *time.Time) pgtype.Timestamptz {
	if value == nil {
		return pgtype.Timestamptz{}
	}
	return pgtype.Timestamptz{Time: value.UTC(), Valid: true}
}

func waitTimeout(seconds int32) time.Duration {
	if seconds <= 0 {
		return defaultTaskSessionWaitTimeout
	}
	timeout := time.Duration(seconds) * time.Second
	if timeout > maxTaskSessionWaitTimeout {
		return maxTaskSessionWaitTimeout
	}
	return timeout
}

func (s *Server) writeTaskStartError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errTaskArchived), errors.Is(err, errTaskNotDeployed), errors.Is(err, errTaskStartSessionFingerprint), errors.Is(err, errTaskStartIdempotencyFingerprint), errors.Is(err, errTaskStartIdempotencyExternalID), errors.Is(err, errTaskSessionTerminated), errors.Is(err, errTaskSessionNoCurrentRun), errors.Is(err, errSandboxNotDeployed), errors.Is(err, errWorkspaceSandboxIncompatible), errors.Is(err, errWorkspaceResourceFloor):
		writeError(w, conflict(err))
	case errors.Is(err, errTaskStartPending):
		w.Header().Set("Retry-After", "1")
		writeErrorStatus(w, http.StatusAccepted, err)
	case errors.Is(err, errTaskStartCoordinationUnavailable):
		writeError(w, unavailable(err))
	case errors.Is(err, errPermissionRequired):
		writeError(w, forbidden(err))
	case isCreateRunConfigError(err):
		writeError(w, unavailable(err))
	case isCreateRunClientError(err):
		writeError(w, badRequest(err))
	default:
		s.log.Error("task start failed", "error", err)
		writeError(w, errors.New("start task"))
	}
}

func taskSessionStartReusable(session db.TaskSession) bool {
	return session.Status == db.TaskSessionStatusOpen && (!session.ExpiresAt.Valid || session.ExpiresAt.Time.After(time.Now()))
}

func taskSessionResponse(session db.TaskSession) api.TaskSessionResponse {
	return taskSessionResponseWithMode(session, true, false)
}

func taskSessionWaitResponse(session db.TaskSession, timedOut bool) api.TaskSessionResponse {
	return taskSessionResponseWithMode(session, true, timedOut)
}

func taskSessionResponseWithMode(session db.TaskSession, unwrapResult bool, timedOut bool) api.TaskSessionResponse {
	response := api.TaskSessionResponse{
		ID:                  pgvalue.MustUUIDValue(session.ID).String(),
		ProjectID:           pgvalue.MustUUIDValue(session.ProjectID).String(),
		EnvironmentID:       pgvalue.MustUUIDValue(session.EnvironmentID).String(),
		TaskID:              session.TaskID,
		InitialDeploymentID: pgvalue.MustUUIDValue(session.InitialDeploymentID).String(),
		ActiveDeploymentID:  pgvalue.MustUUIDValue(session.ActiveDeploymentID).String(),
		ExternalID:          session.ExternalID,
		Status:              string(session.Status),
		Metadata:            json.RawMessage(session.Metadata),
		Tags:                append([]string(nil), session.Tags...),
		TerminalReason:      json.RawMessage(session.TerminalReason),
		CreatedAt:           session.CreatedAt.Time,
		UpdatedAt:           session.UpdatedAt.Time,
		TimedOut:            timedOut,
	}
	if session.CurrentRunID.Valid {
		response.CurrentRunID = pgvalue.MustUUIDValue(session.CurrentRunID).String()
	}
	if session.WorkspaceID.Valid {
		response.WorkspaceID = pgvalue.MustUUIDValue(session.WorkspaceID).String()
	}
	if len(session.Result) > 0 && unwrapResult {
		result, resultErr, ok := unwrapStoredTaskSessionResult(session.Result)
		if ok {
			response.Result = result
			response.Error = resultErr
		} else {
			response.Result = json.RawMessage(session.Result)
		}
	} else if len(session.Result) > 0 {
		response.Result = json.RawMessage(session.Result)
	}
	if session.ExpiresAt.Valid {
		expiresAt := session.ExpiresAt.Time
		response.ExpiresAt = &expiresAt
	}
	return response
}

func unwrapStoredTaskSessionResult(raw []byte) (json.RawMessage, json.RawMessage, bool) {
	var envelope struct {
		OK    *bool           `json:"ok"`
		Value json.RawMessage `json:"value"`
		Error json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil || envelope.OK == nil {
		return nil, nil, false
	}
	if *envelope.OK {
		if len(envelope.Value) == 0 {
			return json.RawMessage(`null`), nil, true
		}
		return envelope.Value, nil, true
	}
	if len(envelope.Error) == 0 {
		return nil, json.RawMessage(`{"name":"TaskFailed","message":"task failed"}`), true
	}
	return nil, envelope.Error, true
}

func (s *Server) listTaskSessions(w http.ResponseWriter, r *http.Request) {
	actor := actorFromContext(r.Context())
	scope, projectID, environmentID, err := s.requestEnvironmentScopeFromRequest(r, actor, r.URL.Query().Get("project_id"), r.URL.Query().Get("environment_id"))
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	if !actor.HasPermission(auth.PermissionRunsRead, scope) {
		writeError(w, forbidden(errPermissionRequired))
		return
	}
	limit := defaultTaskSessionListLimit
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		parsed, err := strconv.ParseInt(raw, 10, 32)
		if err != nil || parsed <= 0 || parsed > int64(maxTaskSessionListLimit) {
			writeError(w, badRequest(fmt.Errorf("limit must be an integer between 1 and %d", maxTaskSessionListLimit)))
			return
		}
		limit = int32(parsed)
	}
	sessions, err := s.db.ListTaskSessions(r.Context(), db.ListTaskSessionsParams{
		OrgID:         pgvalue.UUID(actor.OrgID),
		ProjectID:     projectID,
		EnvironmentID: environmentID,
		StatusFilter:  strings.TrimSpace(r.URL.Query().Get("status")),
		TaskIDFilter:  strings.TrimSpace(r.URL.Query().Get("task_id")),
		RowLimit:      limit,
	})
	if err != nil {
		writeError(w, errors.New("list sessions"))
		return
	}
	response := make([]api.TaskSessionResponse, 0, len(sessions))
	for _, session := range sessions {
		response = append(response, taskSessionResponse(session))
	}
	writeJSON(w, http.StatusOK, api.ListTaskSessionsResponse{Sessions: response})
}

func (s *Server) getTaskSession(w http.ResponseWriter, r *http.Request) {
	session, ok := s.loadTaskSessionForRequest(w, r, auth.PermissionRunsRead)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, taskSessionResponse(session))
}

func (s *Server) patchTaskSession(w http.ResponseWriter, r *http.Request) {
	session, ok := s.loadTaskSessionForRequest(w, r, auth.PermissionRunsManage)
	if !ok {
		return
	}
	var request api.PatchTaskSessionRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid session patch JSON: %w", err)))
		return
	}
	metadata := json.RawMessage(nil)
	if len(request.Metadata) > 0 {
		normalized, err := normalizedJSONObject(request.Metadata, "metadata")
		if err != nil {
			writeError(w, badRequest(err))
			return
		}
		metadata = normalized
	}
	tags := []string(nil)
	if request.Tags != nil {
		normalized, err := normalizedRunTags(request.Tags)
		if err != nil {
			writeError(w, badRequest(err))
			return
		}
		tags = normalized
	}
	if request.ExpiresAt != nil {
		if !session.ExpiresAt.Valid || !request.ExpiresAt.After(time.Now()) || !request.ExpiresAt.After(session.ExpiresAt.Time) {
			writeError(w, badRequest(errTaskSessionExpiresAtPatch))
			return
		}
	}
	updated, err := s.db.PatchTaskSession(r.Context(), db.PatchTaskSessionParams{
		OrgID:         session.OrgID,
		ProjectID:     session.ProjectID,
		EnvironmentID: session.EnvironmentID,
		ID:            session.ID,
		Metadata:      metadata,
		Tags:          tags,
		ExpiresAt:     timePtrToTimestamptz(request.ExpiresAt),
	})
	if isNoRows(err) {
		writeError(w, conflict(errTaskSessionTerminated))
		return
	}
	if err != nil {
		writeError(w, errors.New("patch session"))
		return
	}
	writeJSON(w, http.StatusOK, taskSessionResponse(updated))
}

func (s *Server) waitTaskSession(w http.ResponseWriter, r *http.Request) {
	session, ok := s.loadTaskSessionForRequest(w, r, auth.PermissionRunsRead)
	if !ok {
		return
	}
	var request api.TaskWaitRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid session wait JSON: %w", err)))
		return
	}
	actor := actorFromContext(r.Context())
	updated, timedOut, err := s.waitForTaskSession(r.Context(), actor, session.ProjectID, session.EnvironmentID, pgvalue.MustUUIDValue(session.ID), waitTimeout(request.TimeoutSeconds))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, taskSessionWaitResponse(updated, timedOut))
}

func (s *Server) waitForTaskSession(ctx context.Context, actor auth.Actor, projectID pgtype.UUID, environmentID pgtype.UUID, sessionID uuid.UUID, timeout time.Duration) (db.TaskSession, bool, error) {
	deadline := time.Now().Add(timeout)
	for {
		session, err := s.db.GetTaskSession(ctx, db.GetTaskSessionParams{
			OrgID:         pgvalue.UUID(actor.OrgID),
			ProjectID:     projectID,
			EnvironmentID: environmentID,
			ID:            pgvalue.UUID(sessionID),
		})
		if err != nil {
			return db.TaskSession{}, false, err
		}
		if taskSessionTerminal(session.Status) {
			return session, false, nil
		}
		if time.Now().After(deadline) {
			return session, true, nil
		}
		timer := time.NewTimer(taskSessionWaitPollEvery)
		select {
		case <-ctx.Done():
			timer.Stop()
			return db.TaskSession{}, false, ctx.Err()
		case <-timer.C:
		}
	}
}

func taskSessionTerminal(status db.TaskSessionStatus) bool {
	switch status {
	case db.TaskSessionStatusCompleted, db.TaskSessionStatusFailed, db.TaskSessionStatusClosed, db.TaskSessionStatusCancelled, db.TaskSessionStatusExpired:
		return true
	default:
		return false
	}
}

func (s *Server) closeTaskSession(w http.ResponseWriter, r *http.Request) {
	session, ok := s.loadTaskSessionForRequest(w, r, auth.PermissionRunsManage)
	if !ok {
		return
	}
	var request api.CloseTaskSessionRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid session close JSON: %w", err)))
		return
	}
	if session.CurrentRunID.Valid {
		writeError(w, conflict(errCloseRunActive))
		return
	}
	reason := strings.TrimSpace(request.Reason)
	if reason == "" {
		reason = "closed"
	}
	closed, err := s.db.CloseTaskSession(r.Context(), db.CloseTaskSessionParams{
		OrgID:         session.OrgID,
		ProjectID:     session.ProjectID,
		EnvironmentID: session.EnvironmentID,
		ID:            session.ID,
		Reason:        reason,
	})
	if isNoRows(err) {
		latest, latestErr := s.db.GetTaskSession(r.Context(), db.GetTaskSessionParams{
			OrgID:         session.OrgID,
			ProjectID:     session.ProjectID,
			EnvironmentID: session.EnvironmentID,
			ID:            session.ID,
		})
		if latestErr == nil && latest.Status == db.TaskSessionStatusOpen && latest.CurrentRunID.Valid {
			writeError(w, conflict(errCloseRunActive))
			return
		}
		writeError(w, conflict(errTaskSessionTerminated))
		return
	}
	if err != nil {
		writeError(w, errors.New("close session"))
		return
	}
	writeJSON(w, http.StatusOK, taskSessionResponse(closed))
}

func (s *Server) cancelTaskSession(w http.ResponseWriter, r *http.Request) {
	session, ok := s.loadTaskSessionForRequest(w, r, auth.PermissionRunsManage)
	if !ok {
		return
	}
	var request api.CancelTaskSessionRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid session cancel JSON: %w", err)))
		return
	}
	reason := strings.TrimSpace(request.Reason)
	if reason == "" {
		reason = "cancelled"
	}
	actor := actorFromContext(r.Context())
	store, tx, err := s.beginControlTransaction(r.Context())
	if err != nil {
		writeError(w, errors.New("cancel session"))
		return
	}
	defer tx.Rollback(r.Context())
	locked, err := store.LockTaskSession(r.Context(), db.LockTaskSessionParams{
		OrgID:         session.OrgID,
		ProjectID:     session.ProjectID,
		EnvironmentID: session.EnvironmentID,
		ID:            session.ID,
	})
	if isNoRows(err) {
		writeError(w, notFound(errors.New("session not found")))
		return
	}
	if err != nil {
		writeError(w, errors.New("cancel session"))
		return
	}
	if locked.Status == db.TaskSessionStatusCancelled {
		if err := tx.Commit(r.Context()); err != nil {
			writeError(w, errors.New("cancel session"))
			return
		}
		writeJSON(w, http.StatusOK, taskSessionResponse(locked))
		return
	}
	if locked.Status != db.TaskSessionStatusOpen {
		writeError(w, conflict(errTaskSessionTerminated))
		return
	}
	if locked.CurrentRunID.Valid {
		if err := s.cancelTaskSessionRun(r.Context(), store, actor, locked, reason); err != nil {
			writeError(w, errors.New("cancel task session run"))
			return
		}
	}
	cancelled, err := store.CancelTaskSession(r.Context(), db.CancelTaskSessionParams{
		OrgID:         locked.OrgID,
		ProjectID:     locked.ProjectID,
		EnvironmentID: locked.EnvironmentID,
		ID:            locked.ID,
		Reason:        reason,
	})
	if isNoRows(err) {
		writeError(w, conflict(errTaskSessionTerminated))
		return
	}
	if err != nil {
		writeError(w, errors.New("cancel session"))
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		writeError(w, errors.New("cancel session"))
		return
	}
	writeJSON(w, http.StatusOK, taskSessionResponse(cancelled))
}

func (s *Server) cancelTaskSessionRun(ctx context.Context, store db.Querier, actor auth.Actor, session db.TaskSession, reason string) error {
	runRow, err := store.GetRunSummary(ctx, db.GetRunSummaryParams{OrgID: session.OrgID, ID: session.CurrentRunID})
	if err != nil {
		if isNoRows(err) {
			return nil
		}
		return err
	}
	run := getRunSummary(runRow)
	requestBody, err := json.Marshal(api.CancelRunRequest{Reason: reason})
	if err != nil {
		return err
	}
	operation, err := createRunOperationWithStore(ctx, store, actor, run, db.RunOperationKindCancel, reason, requestBody, "")
	if err != nil {
		return err
	}
	_, err = store.CancelRun(ctx, db.CancelRunParams{
		OrgID:       session.OrgID,
		RunID:       session.CurrentRunID,
		Reason:      reason,
		Force:       false,
		OperationID: operation.ID,
	})
	if isNoRows(err) {
		_, _ = store.MarkRunOperationRejected(ctx, db.MarkRunOperationRejectedParams{
			Result: fmt.Appendf(nil, `{"error":%q}`, "run is already terminal"),
			ID:     operation.ID,
			OrgID:  session.OrgID,
		})
		return nil
	}
	return err
}

func (s *Server) listTaskSessionRuns(w http.ResponseWriter, r *http.Request) {
	session, ok := s.loadTaskSessionForRequest(w, r, auth.PermissionRunsRead)
	if !ok {
		return
	}
	rows, err := s.db.ListTaskSessionRuns(r.Context(), db.ListTaskSessionRunsParams{
		OrgID:         session.OrgID,
		ProjectID:     session.ProjectID,
		EnvironmentID: session.EnvironmentID,
		TaskSessionID: session.ID,
	})
	if err != nil {
		writeError(w, errors.New("list task session runs"))
		return
	}
	response := make([]api.TaskSessionRunResponse, 0, len(rows))
	for _, row := range rows {
		item := api.TaskSessionRunResponse{
			ID:              pgvalue.MustUUIDValue(row.ID).String(),
			RunID:           pgvalue.MustUUIDValue(row.RunID).String(),
			DeploymentID:    pgvalue.MustUUIDValue(row.DeploymentID).String(),
			TurnIndex:       row.TurnIndex,
			Status:          string(row.Status),
			ExecutionStatus: string(row.ExecutionStatus),
			CreatedAt:       row.CreatedAt.Time,
		}
		if row.PreviousRunID.Valid {
			item.PreviousRunID = pgvalue.MustUUIDValue(row.PreviousRunID).String()
		}
		if row.TerminalOutcome.Valid {
			item.TerminalOutcome = string(row.TerminalOutcome.RunTerminalOutcome)
		}
		if row.EndedAt.Valid {
			ended := row.EndedAt.Time
			item.EndedAt = &ended
		}
		response = append(response, item)
	}
	writeJSON(w, http.StatusOK, api.ListTaskSessionRunsResponse{Runs: response})
}

func (s *Server) loadTaskSessionForRequest(w http.ResponseWriter, r *http.Request, permission auth.Permission) (db.TaskSession, bool) {
	sessionID, err := parseUUIDParam(r, "sessionID")
	if err != nil {
		writeError(w, badRequest(err))
		return db.TaskSession{}, false
	}
	actor := actorFromContext(r.Context())
	var session db.TaskSession
	var sessionErr error
	pathProjectID := strings.TrimSpace(chi.URLParam(r, "projectID"))
	pathEnvironmentID := strings.TrimSpace(chi.URLParam(r, "environmentID"))
	if actor.Kind == auth.ActorKindSession {
		if pathProjectID == "" || pathEnvironmentID == "" {
			writeError(w, forbidden(errors.New("session actor must use a project/environment scoped session route")))
			return db.TaskSession{}, false
		}
		_, projectID, environmentID, scopeErr := s.requestEnvironmentScopeFromRequest(r, actor, "", "")
		if scopeErr != nil {
			writeError(w, badRequest(scopeErr))
			return db.TaskSession{}, false
		}
		session, sessionErr = s.db.GetTaskSession(r.Context(), db.GetTaskSessionParams{
			OrgID:         pgvalue.UUID(actor.OrgID),
			ProjectID:     projectID,
			EnvironmentID: environmentID,
			ID:            pgvalue.UUID(sessionID),
		})
	} else {
		session, sessionErr = s.loadTaskSessionByActor(r.Context(), actor, sessionID)
	}
	if isNoRows(sessionErr) {
		writeError(w, notFound(errors.New("session not found")))
		return db.TaskSession{}, false
	}
	if sessionErr != nil {
		if isScopeRequestError(sessionErr) || strings.Contains(sessionErr.Error(), "API key is not bound") {
			writeError(w, badRequest(sessionErr))
			return db.TaskSession{}, false
		}
		writeError(w, errors.New("get session"))
		return db.TaskSession{}, false
	}
	scope := auth.Scope{
		OrgID:         actor.OrgID,
		ProjectID:     pgvalue.MustUUIDValue(session.ProjectID).String(),
		EnvironmentID: pgvalue.MustUUIDValue(session.EnvironmentID).String(),
	}
	if !actor.HasPermission(permission, scope) {
		writeError(w, forbidden(errPermissionRequired))
		return db.TaskSession{}, false
	}
	return session, true
}

func (s *Server) loadTaskSessionByActor(ctx context.Context, actor auth.Actor, sessionID uuid.UUID) (db.TaskSession, error) {
	if actor.Kind == auth.ActorKindAPIKey {
		scope, ok := actor.EnvironmentScope()
		if !ok {
			return db.TaskSession{}, errors.New("API key is not bound to an environment")
		}
		projectID, environmentID, err := runScopeIDs(scope)
		if err != nil {
			return db.TaskSession{}, err
		}
		return s.db.GetTaskSession(ctx, db.GetTaskSessionParams{
			OrgID:         pgvalue.UUID(actor.OrgID),
			ProjectID:     projectID,
			EnvironmentID: environmentID,
			ID:            pgvalue.UUID(sessionID),
		})
	}
	return s.db.GetTaskSessionByOrgID(ctx, db.GetTaskSessionByOrgIDParams{
		OrgID: pgvalue.UUID(actor.OrgID),
		ID:    pgvalue.UUID(sessionID),
	})
}
