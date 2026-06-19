package control

import (
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
	"github.com/helmrdotdev/helmr/internal/publicaccess"
	"github.com/helmrdotdev/helmr/internal/schedule"
	"github.com/helmrdotdev/helmr/internal/tracing"
	"github.com/jackc/pgx/v5/pgtype"
)

const (
	defaultTaskSessionWaitTimeout = 30 * time.Second
	maxTaskSessionWaitTimeout     = 5 * time.Minute
	defaultTaskSessionListLimit   = int32(100)
	maxTaskSessionListLimit       = int32(200)
	defaultSessionChannelLimit    = int32(100)
	maxSessionChannelLimit        = int32(500)
	maxSessionInputsPerChannel    = int64(10000)
	sessionChannelStreamBatchSize = int32(100)
	sessionChannelStreamPollEvery = 250 * time.Millisecond
	sessionChannelStreamMax       = 5 * time.Minute
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
	errChannelFingerprintMismatch      = codedError{code: "channel_fingerprint_mismatch", message: "channel identity was already used with different input data"}
	errChannelInputLimitExceeded       = codedError{code: "channel_input_limit_exceeded", message: "channel input limit exceeded"}
	errSessionInputClosed              = codedError{code: "session_input_closed", message: "session current run is not accepting input"}
	errTaskSessionExpiresAtPatch       = codedError{code: "session_expires_at_not_extendable", message: "session expires_at can only extend an existing future expiry"}
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
	runResponse, err := s.runResponse(r.Context(), result.run)
	if err != nil {
		s.log.Error("build task start run response failed", "error", err)
		writeError(w, errors.New("build task start response"))
		return
	}
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
	maxDurationSeconds, err := runMaxDurationSeconds(request.Options.MaxDurationSeconds, deploymentTask.MaxDurationSeconds)
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
	workspaceID := uuid.Must(uuid.NewV7())
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
	session, err := store.CreateTaskSession(ctx, db.CreateTaskSessionParams{
		ID:                  pgvalue.UUID(sessionID),
		OrgID:               pgvalue.UUID(actor.OrgID),
		ProjectID:           projectID,
		EnvironmentID:       environmentID,
		TaskID:              taskID,
		InitialDeploymentID: deploymentTask.DeploymentID,
		ActiveDeploymentID:  deploymentTask.DeploymentID,
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
	if _, err := store.CreateTaskSessionWorkspace(ctx, db.CreateTaskSessionWorkspaceParams{
		ID:              pgvalue.UUID(workspaceID),
		OrgID:           pgvalue.UUID(actor.OrgID),
		ProjectID:       projectID,
		EnvironmentID:   environmentID,
		TaskSessionID:   session.ID,
		RetentionPolicy: []byte(`{}`),
	}); err != nil {
		return taskStartResult{}, err
	}
	run, err := store.CreateScopedRun(ctx, db.CreateScopedRunParams{
		ID:                    pgvalue.UUID(runID),
		OrgID:                 pgvalue.UUID(actor.OrgID),
		ProjectID:             projectID,
		EnvironmentID:         environmentID,
		DeploymentID:          deploymentTask.DeploymentID,
		DeploymentTaskID:      deploymentTask.ID,
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
		MaxDurationSeconds:    maxDurationSeconds,
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

func validateTaskSessionExternalID(value string) error {
	if strings.ContainsRune(value, 0) {
		return errors.New("external_id must not contain NUL bytes")
	}
	if len(value) > maxTaskSessionExternalIDBytes {
		return fmt.Errorf("external_id must be at most %d bytes", maxTaskSessionExternalIDBytes)
	}
	return nil
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
	case errors.Is(err, errTaskArchived), errors.Is(err, errTaskNotDeployed), errors.Is(err, errTaskStartSessionFingerprint), errors.Is(err, errTaskStartIdempotencyFingerprint), errors.Is(err, errTaskStartIdempotencyExternalID), errors.Is(err, errTaskSessionTerminated), errors.Is(err, errTaskSessionNoCurrentRun):
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
		timer := time.NewTimer(sessionChannelStreamPollEvery)
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

func (s *Server) getTaskSessionWorkspace(w http.ResponseWriter, r *http.Request) {
	session, ok := s.loadTaskSessionForRequest(w, r, auth.PermissionRunsRead)
	if !ok {
		return
	}
	workspace, err := s.db.GetTaskSessionWorkspace(r.Context(), db.GetTaskSessionWorkspaceParams{
		OrgID:         session.OrgID,
		ProjectID:     session.ProjectID,
		EnvironmentID: session.EnvironmentID,
		TaskSessionID: session.ID,
	})
	if isNoRows(err) {
		writeError(w, notFound(errors.New("workspace not found")))
		return
	}
	if err != nil {
		writeError(w, errors.New("get session workspace"))
		return
	}
	response := api.TaskSessionWorkspaceResponse{
		ID:              pgvalue.MustUUIDValue(workspace.ID).String(),
		TaskSessionID:   pgvalue.MustUUIDValue(workspace.TaskSessionID).String(),
		MountPath:       workspace.MountPath,
		State:           string(workspace.State),
		RetentionPolicy: json.RawMessage(workspace.RetentionPolicy),
		CreatedAt:       workspace.CreatedAt.Time,
		UpdatedAt:       workspace.UpdatedAt.Time,
	}
	if workspace.CurrentVersionID.Valid {
		response.CurrentVersionID = pgvalue.MustUUIDValue(workspace.CurrentVersionID).String()
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) listTaskSessionChannels(w http.ResponseWriter, r *http.Request) {
	session, ok := s.loadTaskSessionForRequest(w, r, auth.PermissionRunsRead)
	if !ok {
		return
	}
	channels, err := s.db.ListTaskSessionChannels(r.Context(), db.ListTaskSessionChannelsParams{
		OrgID:         session.OrgID,
		ProjectID:     session.ProjectID,
		EnvironmentID: session.EnvironmentID,
		TaskSessionID: session.ID,
	})
	if err != nil {
		writeError(w, errors.New("list session channels"))
		return
	}
	response := make([]api.TaskSessionChannelResponse, 0, len(channels))
	for _, channel := range channels {
		response = append(response, api.TaskSessionChannelResponse{
			ID:            pgvalue.MustUUIDValue(channel.ID).String(),
			TaskSessionID: pgvalue.MustUUIDValue(channel.TaskSessionID).String(),
			Name:          channel.Name,
			Direction:     string(channel.Direction),
			Backend:       string(channel.Backend),
			NextSequence:  channel.NextSequence,
			CreatedAt:     pgvalue.Time(channel.CreatedAt),
		})
	}
	writeJSON(w, http.StatusOK, api.ListTaskSessionChannelsResponse{Channels: response})
}

func (s *Server) appendTaskSessionChannelInputEntry(w http.ResponseWriter, r *http.Request) {
	if rawToken, ok := publicBearerToken(r); ok {
		s.appendTaskSessionChannelInputWithPublicToken(w, r, rawToken)
		return
	}
	s.requireActor(http.HandlerFunc(s.appendTaskSessionChannelInput)).ServeHTTP(w, r)
}

func (s *Server) listTaskSessionChannelInputsEntry(w http.ResponseWriter, r *http.Request) {
	s.requireActor(http.HandlerFunc(s.listTaskSessionChannelInputs)).ServeHTTP(w, r)
}

func (s *Server) listTaskSessionChannelOutputsEntry(w http.ResponseWriter, r *http.Request) {
	if rawToken, ok := publicBearerToken(r); ok {
		s.listTaskSessionChannelOutputsWithPublicToken(w, r, rawToken)
		return
	}
	s.requireActor(http.HandlerFunc(s.listTaskSessionChannelOutputs)).ServeHTTP(w, r)
}

func (s *Server) streamTaskSessionChannelOutputsEntry(w http.ResponseWriter, r *http.Request) {
	if rawToken, ok := publicBearerToken(r); ok {
		s.streamTaskSessionChannelOutputsWithPublicToken(w, r, rawToken)
		return
	}
	s.requireActor(http.HandlerFunc(s.streamTaskSessionChannelOutputs)).ServeHTTP(w, r)
}

func (s *Server) optionsTaskSessionChannelRecords(w http.ResponseWriter, r *http.Request) {
	writeChannelRecordsCORS(w)
	w.WriteHeader(http.StatusNoContent)
}

func publicBearerToken(r *http.Request) (string, bool) {
	rawToken, ok := bearerToken(r.Header.Get("authorization"))
	return rawToken, ok && publicaccess.IsToken(rawToken)
}

func (s *Server) createPublicAccessToken(w http.ResponseWriter, r *http.Request) {
	actor := actorFromContext(r.Context())
	r.Body = http.MaxBytesReader(w, r.Body, 16*1024)
	var request api.CreatePublicAccessTokenRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid public access token JSON: %w", err)))
		return
	}
	sessionID, err := uuid.Parse(strings.TrimSpace(request.Scope.SessionID))
	if err != nil {
		writeError(w, badRequest(errors.New("scope.session_id must be a UUID")))
		return
	}
	channelName := strings.TrimSpace(request.Scope.Channel)
	if err := validateChannelName(channelName); err != nil {
		writeError(w, badRequest(err))
		return
	}
	permission, direction, err := publicAccessTokenScopePermission(request.Scope.Type)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	session, err := s.loadTaskSessionByActor(r.Context(), actor, sessionID)
	if isNoRows(err) {
		writeError(w, notFound(errors.New("session not found")))
		return
	}
	if err != nil {
		if isScopeRequestError(err) || strings.Contains(err.Error(), "API key is not bound") {
			writeError(w, badRequest(err))
			return
		}
		writeError(w, errors.New("get session"))
		return
	}
	scope := auth.Scope{
		OrgID:         actor.OrgID,
		ProjectID:     pgvalue.MustUUIDValue(session.ProjectID).String(),
		EnvironmentID: pgvalue.MustUUIDValue(session.EnvironmentID).String(),
	}
	if !actor.HasPermission(permission, scope) {
		writeError(w, forbidden(errPermissionRequired))
		return
	}
	channel, err := s.db.GetTaskSessionChannelByName(r.Context(), db.GetTaskSessionChannelByNameParams{
		OrgID:         session.OrgID,
		ProjectID:     session.ProjectID,
		EnvironmentID: session.EnvironmentID,
		TaskSessionID: session.ID,
		Name:          channelName,
		Direction:     direction,
	})
	channelMissing := isNoRows(err)
	if channelMissing && request.Scope.Type != "session.output.read" {
		writeError(w, notFound(errors.New("session channel not found")))
		return
	}
	if err != nil && !channelMissing {
		writeError(w, errors.New("get session channel"))
		return
	}
	if request.MaxUses != nil && *request.MaxUses <= 0 {
		writeError(w, badRequest(errors.New("max_uses must be positive")))
		return
	}
	expiresAt := time.Time{}
	if request.ExpiresAt != nil {
		expiresAt = request.ExpiresAt.UTC()
	}
	createdBy, err := publicAccessTokenCreatedBy(actor)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	maxUses := pgtype.Int4{}
	if request.MaxUses != nil {
		maxUses = pgtype.Int4{Int32: *request.MaxUses, Valid: true}
	}
	grant := sessionChannelPublicAccessTokenGrant{
		Channel:       channel,
		ScopeType:     request.Scope.Type,
		CorrelationID: strings.TrimSpace(request.Scope.CorrelationID),
		ExpiresAt:     expiresAt,
		MaxUses:       maxUses,
		CreatedBy:     createdBy,
	}
	if channelMissing {
		grant.Session = session
		grant.ChannelName = channelName
		grant.Direction = direction
	}
	rawToken, row, err := createSessionChannelPublicAccessToken(r.Context(), s.db, s.authSecret, grant)
	if err != nil {
		writeError(w, err)
		return
	}
	response := api.PublicAccessTokenResponse{
		ID:                pgvalue.MustUUIDValue(row.ID).String(),
		PublicAccessToken: rawToken,
		Scope: api.PublicAccessTokenScopeRequest{
			Type:          request.Scope.Type,
			SessionID:     pgvalue.MustUUIDValue(session.ID).String(),
			Channel:       channelName,
			CorrelationID: strings.TrimSpace(request.Scope.CorrelationID),
		},
		ExpiresAt: pgvalue.Time(row.ExpiresAt),
		CreatedAt: pgvalue.Time(row.CreatedAt),
	}
	if row.MaxUses.Valid {
		value := row.MaxUses.Int32
		response.MaxUses = &value
	}
	writeJSON(w, http.StatusCreated, response)
}

func publicAccessTokenScopePermission(scopeType string) (auth.Permission, db.ChannelDirection, error) {
	switch strings.TrimSpace(scopeType) {
	case "session.input.append":
		return auth.PermissionChannelsWrite, db.ChannelDirectionInput, nil
	case "session.output.read":
		return auth.PermissionRunsRead, db.ChannelDirectionOutput, nil
	default:
		return "", "", errors.New("unsupported public access token scope")
	}
}

func publicAccessTokenCreatedBy(actor auth.Actor) (json.RawMessage, error) {
	subjectType, subjectID := channelInputAuthSubject(actor)
	createdBy, err := json.Marshal(struct {
		Type string `json:"type"`
		ID   string `json:"id,omitempty"`
	}{
		Type: subjectType,
		ID:   subjectID,
	})
	if err != nil {
		return nil, err
	}
	return createdBy, nil
}

func (s *Server) appendTaskSessionChannelInput(w http.ResponseWriter, r *http.Request) {
	session, ok := s.loadTaskSessionForRequest(w, r, auth.PermissionChannelsWrite)
	if !ok {
		return
	}
	channel := strings.TrimSpace(chi.URLParam(r, "channel"))
	if err := validateChannelName(channel); err != nil {
		writeError(w, badRequest(err))
		return
	}
	var request api.AppendChannelRecordRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid channel input JSON: %w", err)))
		return
	}
	data := request.Data
	if len(data) == 0 {
		data = json.RawMessage(`null`)
	}
	if !json.Valid(data) {
		writeError(w, badRequest(errors.New("data must be valid JSON")))
		return
	}
	idempotencyKey := strings.TrimSpace(r.Header.Get("idempotency-key"))
	if len([]byte(idempotencyKey)) > idempotencyKeyMaxBytes {
		writeError(w, badRequest(fmt.Errorf("Idempotency-Key is %d bytes, exceeds max %d", len([]byte(idempotencyKey)), idempotencyKeyMaxBytes)))
		return
	}
	contentType := "application/json"
	authSubjectType, authSubjectID := channelInputAuthSubject(actorFromContext(r.Context()))
	correlationID := strings.TrimSpace(request.CorrelationID)
	externalEventID := strings.TrimSpace(request.ExternalEventID)
	fingerprint, err := channelInputFingerprint(session, channel, data, correlationID, contentType, authSubjectType, authSubjectID, externalEventID)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	if idempotencyKey != "" || externalEventID != "" {
		existing, err := s.db.GetExistingSessionChannelInputRecord(r.Context(), db.GetExistingSessionChannelInputRecordParams{
			OrgID:                  session.OrgID,
			TaskSessionID:          session.ID,
			Channel:                channel,
			IdempotencyKey:         idempotencyKey,
			IdempotencyFingerprint: fingerprint,
			ExternalEventID:        externalEventID,
		})
		if err == nil {
			writeJSON(w, http.StatusCreated, api.AppendChannelRecordResponse{
				Record:            existingSessionChannelInputResponse(existing),
				IdempotencyStatus: "duplicate",
			})
			return
		}
		if err != nil && !isNoRows(err) {
			writeError(w, errors.New("append session channel input"))
			return
		}
	}
	if !session.CurrentRunID.Valid {
		writeError(w, conflict(errSessionInputClosed))
		return
	}
	store, tx, err := s.beginControlTransaction(r.Context())
	if err != nil {
		writeError(w, errors.New("append session channel input"))
		return
	}
	defer tx.Rollback(r.Context())
	record, err := store.AppendSessionChannelInput(r.Context(), db.AppendSessionChannelInputParams{
		ID:                     pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:                  session.OrgID,
		RunID:                  session.CurrentRunID,
		Channel:                channel,
		Data:                   data,
		CorrelationID:          correlationID,
		ContentType:            contentType,
		IdempotencyKey:         idempotencyKey,
		IdempotencyFingerprint: fingerprint,
		ExternalEventID:        externalEventID,
		AuthSubjectType:        db.ChannelRecordAuthSubjectType(authSubjectType),
		AuthSubjectID:          authSubjectID,
		MaxInputsPerChannel:    maxSessionInputsPerChannel,
	})
	if isNoRows(err) {
		writeError(w, s.sessionChannelInputAppendConflict(r.Context(), store, session.OrgID, session.CurrentRunID, channel, idempotencyKey, externalEventID, fingerprint))
		return
	}
	if err != nil {
		writeError(w, errors.New("append session channel input"))
		return
	}
	resumeRunIDs := []pgtype.UUID(nil)
	if record.Inserted {
		resumeRunIDs, err = resolveRunChannelWaitpointsWithQueries(r.Context(), store, session.OrgID, session.CurrentRunID)
		if err != nil {
			writeError(w, errors.New("append session channel input"))
			return
		}
	}
	if err := tx.Commit(r.Context()); err != nil {
		writeError(w, errors.New("append session channel input"))
		return
	}
	if record.Inserted && s.runEnqueuer != nil {
		enqueueCtx := context.WithoutCancel(r.Context())
		if _, err := s.runEnqueuer.EnqueueRun(enqueueCtx, session.OrgID, session.CurrentRunID); err != nil {
			s.log.Error("enqueue session after channel input failed", "session_id", pgvalue.MustUUIDValue(session.ID).String(), "error", err)
		}
		for _, runID := range resumeRunIDs {
			if _, err := s.runEnqueuer.EnqueueRun(enqueueCtx, session.OrgID, runID); err != nil {
				s.log.Error("enqueue dependent run after session channel input failed", "session_id", pgvalue.MustUUIDValue(session.ID).String(), "run_id", pgvalue.MustUUIDValue(runID).String(), "error", err)
			}
		}
	}
	idempotencyStatus := "duplicate"
	if record.Inserted {
		idempotencyStatus = "created"
	}
	writeJSON(w, http.StatusCreated, api.AppendChannelRecordResponse{
		Record:            appendSessionChannelInputResponse(record),
		IdempotencyStatus: idempotencyStatus,
	})
}

func (s *Server) appendTaskSessionChannelInputWithPublicToken(w http.ResponseWriter, r *http.Request, rawToken string) {
	writeChannelRecordsCORS(w)
	sessionID, err := parseUUIDParam(r, "sessionID")
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	channel := strings.TrimSpace(chi.URLParam(r, "channel"))
	if err := validateChannelName(channel); err != nil {
		writeError(w, badRequest(err))
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, channelRecordRequestJSONMaxBytes)
	var request api.AppendChannelRecordRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid channel input JSON: %w", err)))
		return
	}
	data := request.Data
	if len(data) == 0 {
		data = json.RawMessage(`null`)
	}
	if len(data) > channelRecordContentJSONMaxBytes {
		writeError(w, badRequest(fmt.Errorf("data is %d bytes, exceeds max %d", len(data), channelRecordContentJSONMaxBytes)))
		return
	}
	if !json.Valid(data) {
		writeError(w, badRequest(errors.New("data must be valid JSON")))
		return
	}
	idempotencyKey := strings.TrimSpace(r.Header.Get("idempotency-key"))
	if len([]byte(idempotencyKey)) > idempotencyKeyMaxBytes {
		writeError(w, badRequest(fmt.Errorf("Idempotency-Key is %d bytes, exceeds max %d", len([]byte(idempotencyKey)), idempotencyKeyMaxBytes)))
		return
	}
	tokenHash, err := publicaccess.HashToken(s.authSecret, rawToken)
	if err != nil {
		writeError(w, unauthorized(errors.New("invalid token")))
		return
	}
	record, status, err := s.appendSessionChannelInputWithPublicToken(r.Context(), sessionID, channel, tokenHash, data, strings.TrimSpace(request.CorrelationID), idempotencyKey, strings.TrimSpace(request.ExternalEventID))
	if isNoRows(err) {
		writeError(w, notFound(errors.New("session channel not found or token not authorized")))
		return
	}
	var apiErr apiError
	if errors.As(err, &apiErr) {
		writeError(w, err)
		return
	}
	if err != nil {
		s.log.Error("append session channel input with public token failed", "session_id", sessionID.String(), "channel", channel, "error", err)
		writeError(w, errors.New("append session channel input"))
		return
	}
	writeJSON(w, http.StatusCreated, api.AppendChannelRecordResponse{
		Record:            record,
		IdempotencyStatus: status,
	})
}

func (s *Server) sessionChannelInputAppendConflict(ctx context.Context, store db.Querier, orgID pgtype.UUID, runID pgtype.UUID, channel string, idempotencyKey string, externalEventID string, fingerprint string) error {
	reason, err := store.GetSessionChannelInputAppendConflictReason(ctx, db.GetSessionChannelInputAppendConflictReasonParams{
		OrgID:                  orgID,
		RunID:                  runID,
		Channel:                channel,
		IdempotencyKey:         idempotencyKey,
		IdempotencyFingerprint: fingerprint,
		ExternalEventID:        externalEventID,
		MaxInputsPerChannel:    maxSessionInputsPerChannel,
	})
	if err != nil {
		return errors.New("session channel input was not accepted")
	}
	switch reason {
	case "idempotency_conflict":
		return conflict(errChannelFingerprintMismatch)
	case "input_limit_exceeded":
		return conflict(errChannelInputLimitExceeded)
	default:
		return conflict(errSessionInputClosed)
	}
}

func appendSessionChannelInputResponse(row db.AppendSessionChannelInputRow) api.ChannelRecordResponse {
	return api.ChannelRecordResponse{
		ID:            pgvalue.MustUUIDValue(row.ID).String(),
		ChannelID:     pgvalue.MustUUIDValue(row.ChannelID).String(),
		Sequence:      row.Sequence,
		Data:          json.RawMessage(row.Data),
		CorrelationID: row.CorrelationID,
		ContentType:   row.ContentType,
		CreatedAt:     pgvalue.Time(row.CreatedAt),
	}
}

func existingSessionChannelInputResponse(row db.GetExistingSessionChannelInputRecordRow) api.ChannelRecordResponse {
	return api.ChannelRecordResponse{
		ID:            pgvalue.MustUUIDValue(row.ID).String(),
		ChannelID:     pgvalue.MustUUIDValue(row.ChannelID).String(),
		Sequence:      row.Sequence,
		Data:          json.RawMessage(row.Data),
		CorrelationID: row.CorrelationID,
		ContentType:   row.ContentType,
		CreatedAt:     pgvalue.Time(row.CreatedAt),
	}
}

func channelInputAuthSubject(actor auth.Actor) (string, string) {
	switch actor.Kind {
	case auth.ActorKindAPIKey:
		return "api_key", actor.APIKeyID.String()
	case auth.ActorKindSession:
		if actor.SessionID != uuid.Nil {
			return "session", actor.SessionID.String()
		}
		return "session", actor.UserID.String()
	default:
		return "system", ""
	}
}

func channelInputFingerprint(session db.TaskSession, channel string, data []byte, correlationID string, contentType string, authSubjectType string, authSubjectID string, externalEventID string) (string, error) {
	canonical, err := canonicalJSON(data)
	if err != nil {
		return "", err
	}
	return channelRecordFingerprint(channelRecordFingerprintInput{
		SessionID:       pgvalue.MustUUIDValue(session.ID).String(),
		Channel:         strings.TrimSpace(channel),
		Direction:       string(db.ChannelDirectionInput),
		ContentType:     strings.TrimSpace(contentType),
		CorrelationID:   strings.TrimSpace(correlationID),
		Source:          "control",
		AuthSubjectType: authSubjectType,
		AuthSubjectID:   authSubjectID,
		ExternalEventID: strings.TrimSpace(externalEventID),
		Actor:           json.RawMessage(`{}`),
		Data:            canonical,
	})
}

func (s *Server) listTaskSessionChannelRecords(w http.ResponseWriter, r *http.Request, direction db.ChannelDirection) {
	session, ok := s.loadTaskSessionForRequest(w, r, auth.PermissionRunsRead)
	if !ok {
		return
	}
	channelName := strings.TrimSpace(chi.URLParam(r, "channel"))
	channel, err := s.db.GetTaskSessionChannelByName(r.Context(), db.GetTaskSessionChannelByNameParams{
		OrgID:         session.OrgID,
		ProjectID:     session.ProjectID,
		EnvironmentID: session.EnvironmentID,
		TaskSessionID: session.ID,
		Name:          channelName,
		Direction:     direction,
	})
	if isNoRows(err) {
		writeJSON(w, http.StatusOK, api.ListChannelRecordsResponse{Records: []api.ChannelRecordResponse{}})
		return
	}
	if err != nil {
		writeError(w, errors.New("get session channel"))
		return
	}
	afterSequence := int64(0)
	if raw := strings.TrimSpace(r.URL.Query().Get("after_sequence")); raw != "" {
		parsed, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || parsed < 0 {
			writeError(w, badRequest(errors.New("after_sequence must be a non-negative integer")))
			return
		}
		afterSequence = parsed
	}
	limit := defaultSessionChannelLimit
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		parsed, err := strconv.ParseInt(raw, 10, 32)
		if err != nil || parsed <= 0 || parsed > int64(maxSessionChannelLimit) {
			writeError(w, badRequest(fmt.Errorf("limit must be an integer between 1 and %d", maxSessionChannelLimit)))
			return
		}
		limit = int32(parsed)
	}
	correlationID := strings.TrimSpace(r.URL.Query().Get("correlation_id"))
	rows, err := s.db.ListChannelRecords(r.Context(), db.ListChannelRecordsParams{
		OrgID:         session.OrgID,
		ProjectID:     session.ProjectID,
		EnvironmentID: session.EnvironmentID,
		ChannelID:     channel.ID,
		Direction:     direction,
		AfterSequence: afterSequence,
		CorrelationID: optionalText(correlationID),
		LimitCount:    limit,
	})
	if err != nil {
		writeError(w, errors.New("list session channel records"))
		return
	}
	writeJSON(w, http.StatusOK, api.ListChannelRecordsResponse{Records: channelRecordResponses(rows)})
}

func (s *Server) listTaskSessionChannelInputs(w http.ResponseWriter, r *http.Request) {
	s.listTaskSessionChannelRecords(w, r, db.ChannelDirectionInput)
}

func (s *Server) listTaskSessionChannelOutputs(w http.ResponseWriter, r *http.Request) {
	s.listTaskSessionChannelRecords(w, r, db.ChannelDirectionOutput)
}

func (s *Server) listTaskSessionChannelOutputsWithPublicToken(w http.ResponseWriter, r *http.Request, rawToken string) {
	writeChannelRecordsCORS(w)
	sessionID, err := parseUUIDParam(r, "sessionID")
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	channel := strings.TrimSpace(chi.URLParam(r, "channel"))
	if err := validateChannelName(channel); err != nil {
		writeError(w, badRequest(err))
		return
	}
	afterSequence := int64(0)
	if raw := strings.TrimSpace(r.URL.Query().Get("after_sequence")); raw != "" {
		afterSequence, err = strconv.ParseInt(raw, 10, 64)
		if err != nil || afterSequence < 0 {
			writeError(w, badRequest(errors.New("after_sequence must be a non-negative integer")))
			return
		}
	}
	limit, err := optionalLimitQuery(r, defaultControlPageSize)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	correlationID := strings.TrimSpace(r.URL.Query().Get("correlation_id"))
	tokenHash, err := publicaccess.HashToken(s.authSecret, rawToken)
	if err != nil {
		writeError(w, unauthorized(errors.New("invalid token")))
		return
	}
	records, err := s.listSessionChannelOutputsWithPublicToken(r.Context(), sessionID, channel, tokenHash, afterSequence, limit, correlationID)
	if isNoRows(err) {
		writeError(w, notFound(errors.New("session channel not found or token not authorized")))
		return
	}
	if err != nil {
		s.log.Error("list session channel outputs with public token failed", "session_id", sessionID.String(), "channel", channel, "error", err)
		writeError(w, errors.New("list session channel outputs"))
		return
	}
	writeJSON(w, http.StatusOK, api.ListChannelRecordsResponse{Records: channelRecordResponses(records)})
}

func (s *Server) streamTaskSessionChannelOutputs(w http.ResponseWriter, r *http.Request) {
	session, ok := s.loadTaskSessionForRequest(w, r, auth.PermissionRunsRead)
	if !ok {
		return
	}
	channelName := strings.TrimSpace(chi.URLParam(r, "channel"))
	afterSequence := int64(0)
	if raw := strings.TrimSpace(r.URL.Query().Get("after_sequence")); raw != "" {
		parsed, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || parsed < 0 {
			writeError(w, badRequest(errors.New("after_sequence must be a non-negative integer")))
			return
		}
		afterSequence = parsed
	}
	correlationID := strings.TrimSpace(r.URL.Query().Get("correlation_id"))
	s.streamTaskSessionChannelOutputsAuthorized(w, r, session, channelName, afterSequence, correlationID)
}

func (s *Server) streamTaskSessionChannelOutputsWithPublicToken(w http.ResponseWriter, r *http.Request, rawToken string) {
	writeChannelRecordsCORS(w)
	sessionID, err := parseUUIDParam(r, "sessionID")
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	channelName := strings.TrimSpace(chi.URLParam(r, "channel"))
	if err := validateChannelName(channelName); err != nil {
		writeError(w, badRequest(err))
		return
	}
	afterSequence := int64(0)
	if raw := strings.TrimSpace(r.URL.Query().Get("after_sequence")); raw != "" {
		afterSequence, err = strconv.ParseInt(raw, 10, 64)
		if err != nil || afterSequence < 0 {
			writeError(w, badRequest(errors.New("after_sequence must be a non-negative integer")))
			return
		}
	}
	correlationID := strings.TrimSpace(r.URL.Query().Get("correlation_id"))
	tokenHash, err := publicaccess.HashToken(s.authSecret, rawToken)
	if err != nil {
		writeError(w, unauthorized(errors.New("invalid token")))
		return
	}
	session, channel, err := s.openSessionChannelOutputStreamWithPublicToken(r.Context(), sessionID, channelName, tokenHash, correlationID)
	if isNoRows(err) {
		writeError(w, notFound(errors.New("session channel not found or token not authorized")))
		return
	}
	if err != nil {
		s.log.Error("open session channel output stream with public token failed", "session_id", sessionID.String(), "channel", channelName, "error", err)
		writeError(w, errors.New("open session channel output stream"))
		return
	}
	s.streamTaskSessionChannelOutputsAuthorized(w, r, session, channel, afterSequence, correlationID)
}

func (s *Server) streamTaskSessionChannelOutputsAuthorized(w http.ResponseWriter, r *http.Request, session db.TaskSession, channelName string, afterSequence int64, correlationID string) {
	flusher, _ := w.(http.Flusher)
	w.Header().Set("content-type", "text/event-stream")
	w.Header().Set("cache-control", "no-cache")
	w.WriteHeader(http.StatusOK)
	encoder := json.NewEncoder(w)
	ctx, cancel := context.WithTimeout(r.Context(), sessionChannelStreamMax)
	defer cancel()
	ticker := time.NewTicker(sessionChannelStreamPollEvery)
	defer ticker.Stop()
	cursor := afterSequence
	channelID := pgtype.UUID{}
	for {
		rowCount := 0
		if !channelID.Valid {
			channel, err := s.db.GetTaskSessionChannelByName(ctx, db.GetTaskSessionChannelByNameParams{
				OrgID:         session.OrgID,
				ProjectID:     session.ProjectID,
				EnvironmentID: session.EnvironmentID,
				TaskSessionID: session.ID,
				Name:          channelName,
				Direction:     db.ChannelDirectionOutput,
			})
			if err == nil {
				channelID = channel.ID
			} else if !isNoRows(err) {
				if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
					s.log.Warn("stream session channel outputs failed", "session_id", pgvalue.MustUUIDValue(session.ID).String(), "channel", channelName, "error", err)
				}
				return
			}
		}
		if channelID.Valid {
			nextCursor, count, err := s.writeTaskSessionChannelOutputEvents(ctx, w, flusher, encoder, session, channelID, cursor, correlationID)
			if err != nil {
				if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
					s.log.Warn("stream session channel outputs failed", "session_id", pgvalue.MustUUIDValue(session.ID).String(), "channel", channelName, "error", err)
				}
				return
			}
			cursor = nextCursor
			rowCount = count
			if rowCount == int(sessionChannelStreamBatchSize) {
				continue
			}
		}
		latest, err := s.db.GetTaskSession(ctx, db.GetTaskSessionParams{
			OrgID:         session.OrgID,
			ProjectID:     session.ProjectID,
			EnvironmentID: session.EnvironmentID,
			ID:            session.ID,
		})
		if isNoRows(err) || (err == nil && taskSessionTerminal(latest.Status)) {
			if channelID.Valid {
				for {
					nextCursor, rowCount, err := s.writeTaskSessionChannelOutputEvents(ctx, w, flusher, encoder, session, channelID, cursor, correlationID)
					if err != nil || rowCount < int(sessionChannelStreamBatchSize) {
						return
					}
					cursor = nextCursor
				}
			}
			return
		}
		if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			s.log.Warn("read session while streaming channel outputs failed", "session_id", pgvalue.MustUUIDValue(session.ID).String(), "error", err)
			return
		}
		_, _ = fmt.Fprint(w, ": keep-alive\n\n")
		if flusher != nil {
			flusher.Flush()
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *Server) writeTaskSessionChannelOutputEvents(ctx context.Context, w http.ResponseWriter, flusher http.Flusher, encoder *json.Encoder, session db.TaskSession, channelID pgtype.UUID, cursor int64, correlationID string) (int64, int, error) {
	rows, err := s.db.ListChannelRecords(ctx, db.ListChannelRecordsParams{
		OrgID:         session.OrgID,
		ProjectID:     session.ProjectID,
		EnvironmentID: session.EnvironmentID,
		ChannelID:     channelID,
		Direction:     db.ChannelDirectionOutput,
		AfterSequence: cursor,
		CorrelationID: optionalText(correlationID),
		LimitCount:    sessionChannelStreamBatchSize,
	})
	if err != nil {
		return cursor, 0, err
	}
	for _, row := range rows {
		response := channelRecordResponse(row)
		_, _ = fmt.Fprintf(w, "id: %d\n", row.Sequence)
		_, _ = fmt.Fprint(w, "event: channel_output\n")
		_, _ = fmt.Fprint(w, "data: ")
		if err := encoder.Encode(response); err != nil {
			return cursor, 0, err
		}
		_, _ = fmt.Fprint(w, "\n")
		cursor = row.Sequence
		if flusher != nil {
			flusher.Flush()
		}
	}
	return cursor, len(rows), nil
}

func channelRecordResponses(records []db.ChannelRecord) []api.ChannelRecordResponse {
	response := make([]api.ChannelRecordResponse, 0, len(records))
	for _, record := range records {
		response = append(response, channelRecordResponse(record))
	}
	return response
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
