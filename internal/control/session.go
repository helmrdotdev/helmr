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
	"github.com/helmrdotdev/helmr/internal/publicid"
	"github.com/helmrdotdev/helmr/internal/schedule"
	"github.com/helmrdotdev/helmr/internal/tracing"
	"github.com/jackc/pgx/v5/pgtype"
)

const (
	defaultSessionWaitTimeout = 30 * time.Second
	maxSessionWaitTimeout     = 5 * time.Minute
	defaultSessionListLimit   = int32(100)
	maxSessionListLimit       = int32(200)
	sessionWaitPollEvery      = 250 * time.Millisecond
	maxSessionExternalIDBytes = 512
)

var (
	errTaskArchived                       = codedError{code: "task_archived"}
	errTaskNotDeployed                    = codedError{code: "task_not_deployed"}
	errSessionStartSessionFingerprint     = codedError{code: "session_fingerprint_mismatch", message: "session start fingerprint mismatch"}
	errSessionStartIdempotencyFingerprint = codedError{code: "idempotency_fingerprint_mismatch", message: "idempotency_key was already used with different session start parameters"}
	errSessionStartIdempotencyExternalID  = codedError{code: "idempotency_external_id_mismatch", message: "idempotency_key resolves to a different session"}
	errSessionTerminated                  = codedError{code: "session_terminal", message: "session is terminal"}
	errSessionExpired                     = codedError{code: "session_expired", message: "session is expired"}
	errSessionNoCurrentRun                = codedError{code: "session_has_no_current_run"}
	errCloseRunActive                     = codedError{code: "close_run_active"}
	errSessionExpiresAtPatch              = codedError{code: "session_expires_at_not_extendable", message: "session expires_at can only extend an existing future expiry"}
	errSandboxNotDeployed                 = codedError{code: "sandbox_not_deployed", message: "task sandbox is not deployed"}
	errWorkspaceSandboxIncompatible       = codedError{code: "workspace_sandbox_incompatible", message: "workspace sandbox is incompatible with this task"}
	errWorkspaceResourceFloor             = codedError{code: "workspace_resource_floor_unsatisfied", message: "workspace resource floor is lower than this task requires"}
	errAPIKeyEnvironmentScopeRequired     = errors.New("API key is not bound to an environment")
	errSessionExternalIDScopeRequired     = errors.New("external_id session addressing requires project_id and environment_id")
	errSessionStartExistingHitRollback    = errors.New("session start idempotency hit")
	errSessionStartExternalIDRace         = errors.New("session start external id race")
)

type sessionStartSource struct {
	scheduleID            pgtype.UUID
	scheduleInstanceID    pgtype.UUID
	scheduleGeneration    int64
	scheduleOrgID         pgtype.UUID
	scheduleWorkerGroupID string
	scheduleProjectID     pgtype.UUID
	scheduleEnvironmentID pgtype.UUID
	scheduledAt           pgtype.Timestamptz
}

type sessionStartResult struct {
	session        db.Session
	run            runSummary
	idempotencyHit bool
	sessionReused  bool
}

type sessionStartIdempotencyBinding struct {
	ID                 pgtype.UUID
	OrgID              pgtype.UUID
	WorkerGroupID      string
	ProjectID          pgtype.UUID
	EnvironmentID      pgtype.UUID
	TaskID             string
	IdempotencyKey     string
	RequestFingerprint string
	SessionID          pgtype.UUID
	FirstRunID         pgtype.UUID
	ExpiresAt          pgtype.Timestamptz
}

type sessionAddress struct {
	kind       string
	id         uuid.UUID
	externalID string
}

const (
	sessionAddressID         = "id"
	sessionAddressExternalID = "external_id"
)

func (s *Server) startSession(w http.ResponseWriter, r *http.Request) {
	actor := actorFromContext(r.Context())
	var request api.SessionStartRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid session start request JSON: %w", err)))
		return
	}
	taskID := strings.TrimSpace(request.TaskID)
	if err := api.ValidateTaskID(taskID); err != nil {
		writeError(w, badRequest(err))
		return
	}
	scope, projectID, environmentID, err := s.requestEnvironmentScopeFromRequest(r, actor, request.ProjectID, request.EnvironmentID)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	result, err := s.startSessionFromRequestInScope(contextWithRequestVersionMetadata(r.Context(), r), actor, scope, projectID, environmentID, taskID, request, sessionStartSource{})
	if err != nil {
		s.writeSessionStartError(w, err)
		return
	}
	runResponse := runResponse(result.run)
	sessionActivity, err := s.sessionDerivedState(r.Context(), result.session)
	if err != nil {
		writeError(w, errors.New("load session activity"))
		return
	}
	status := http.StatusCreated
	if result.idempotencyHit || result.sessionReused {
		status = http.StatusOK
	}
	writeJSON(w, status, api.SessionStartResponse{
		Session:  sessionResponseWithDerived(result.session, sessionActivity),
		Run:      runResponse,
		IsCached: result.idempotencyHit || result.sessionReused,
	})
}

func (s *Server) startSessionAndWait(w http.ResponseWriter, r *http.Request) {
	actor := actorFromContext(r.Context())
	var request api.SessionStartAndWaitRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid session start-and-wait request JSON: %w", err)))
		return
	}
	taskID := strings.TrimSpace(request.TaskID)
	if err := api.ValidateTaskID(taskID); err != nil {
		writeError(w, badRequest(err))
		return
	}
	scope, projectID, environmentID, err := s.requestEnvironmentScopeFromRequest(r, actor, request.ProjectID, request.EnvironmentID)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	result, err := s.startSessionFromRequestInScope(contextWithRequestVersionMetadata(r.Context(), r), actor, scope, projectID, environmentID, taskID, request.SessionStartRequest, sessionStartSource{})
	if err != nil {
		s.writeSessionStartError(w, err)
		return
	}
	run, timedOut, err := s.waitForRunTerminal(r.Context(), actor, result.run.ID, waitTimeout(request.TimeoutSeconds))
	if err != nil {
		writeError(w, err)
		return
	}
	session, err := s.db.GetSession(r.Context(), db.GetSessionParams{
		OrgID:         pgvalue.UUID(actor.OrgID),
		ProjectID:     result.session.ProjectID,
		EnvironmentID: result.session.EnvironmentID,
		ID:            result.session.ID,
	})
	if err != nil {
		writeError(w, err)
		return
	}
	sessionActivity, err := s.sessionDerivedState(r.Context(), session)
	if err != nil {
		writeError(w, errors.New("load session activity"))
		return
	}
	writeJSON(w, http.StatusOK, api.SessionStartResponse{
		Session:  sessionResponseWithDerived(session, sessionActivity),
		Run:      runResponse(run),
		IsCached: result.idempotencyHit || result.sessionReused,
		TimedOut: timedOut,
	})
}

func (s *Server) startSessionFromRequest(ctx context.Context, actor auth.Actor, taskID string, request api.SessionStartRequest, source sessionStartSource) (sessionStartResult, error) {
	if s.db == nil {
		return sessionStartResult{}, errors.New("session storage is not configured")
	}
	taskID = strings.TrimSpace(taskID)
	if err := api.ValidateTaskID(taskID); err != nil {
		return sessionStartResult{}, err
	}
	scope, projectID, environmentID, err := s.requestEnvironmentScope(ctx, actor, request.ProjectID, request.EnvironmentID)
	if err != nil {
		return sessionStartResult{}, err
	}
	return s.startSessionFromRequestInScope(ctx, actor, scope, projectID, environmentID, taskID, request, source)
}

func (s *Server) startSessionFromRequestInScope(ctx context.Context, actor auth.Actor, scope auth.Scope, projectID pgtype.UUID, environmentID pgtype.UUID, taskID string, request api.SessionStartRequest, source sessionStartSource) (sessionStartResult, error) {
	if !actor.HasPermission(auth.PermissionRunsCreate, scope) {
		return sessionStartResult{}, errPermissionRequired
	}
	runOptions := sessionStartRunOptions(request.Options)
	idempotency, err := normalizeRunIdempotency(runOptions)
	if err != nil {
		return sessionStartResult{}, err
	}
	payload := request.Payload
	if len(payload) == 0 {
		payload = json.RawMessage(`{}`)
	}
	if !json.Valid(payload) {
		return sessionStartResult{}, errors.New("payload must be valid JSON")
	}
	externalID := strings.TrimSpace(request.ExternalID)
	if err := validateSessionExternalID(externalID); err != nil {
		return sessionStartResult{}, err
	}
	requestedWorkspaceID, err := parseOptionalWorkspaceID(request.Options.WorkspaceID)
	if err != nil {
		return sessionStartResult{}, err
	}
	metadata, err := normalizedJSONObject(request.Options.Metadata, "metadata")
	if err != nil {
		return sessionStartResult{}, err
	}
	tags, err := normalizedRunTags(request.Options.Tags)
	if err != nil {
		return sessionStartResult{}, err
	}
	startFingerprint, err := sessionStartRequestFingerprint(taskID, payload, request.Options, externalID, request.Options.ExpiresAt)
	if err != nil {
		return sessionStartResult{}, err
	}
	idempotencyFingerprint := pgtype.Text{}
	if idempotency.key.Valid {
		idempotencyFingerprint = startFingerprint
	}
	var placementWorkerGroupID string
	var attachedWorkspace db.GetWorkspaceSourceForSessionStartRow
	if requestedWorkspaceID.Valid {
		workspace, err := s.db.GetWorkspaceSourceForSessionStart(ctx, db.GetWorkspaceSourceForSessionStartParams{
			OrgID:         pgvalue.UUID(actor.OrgID),
			ProjectID:     projectID,
			EnvironmentID: environmentID,
			WorkspaceID:   requestedWorkspaceID,
		})
		if isNoRows(err) {
			return sessionStartResult{}, errWorkspaceSandboxIncompatible
		}
		if err != nil {
			return sessionStartResult{}, err
		}
		if err := s.requireRoutableRecordWorkerGroup(ctx, s.db, actor.OrgID, projectID, environmentID, workspace.WorkerGroupID); err != nil {
			return sessionStartResult{}, err
		}
		placementWorkerGroupID = workspace.WorkerGroupID
		attachedWorkspace = workspace
	} else {
		placement, err := s.resolveEnvironmentPlacement(ctx, s.db, actor.OrgID, projectID, environmentID)
		if err != nil {
			return sessionStartResult{}, err
		}
		placementWorkerGroupID = placement.WorkerGroupID
	}
	if idempotency.key.Valid {
		if existing, hit, err := s.existingSessionStartIdempotency(ctx, actor.OrgID, placementWorkerGroupID, projectID, environmentID, taskID, idempotency.key.String, idempotencyFingerprint.String, externalID); err != nil {
			return sessionStartResult{}, err
		} else if hit {
			if err := s.ensureSessionStartSourceCurrent(ctx, source); err != nil {
				return sessionStartResult{}, err
			}
			return existing, nil
		}
	}
	if externalID != "" && !idempotency.key.Valid {
		if existing, err := s.loadExistingSessionStart(ctx, s.db, actor.OrgID, placementWorkerGroupID, projectID, environmentID, taskID, externalID, startFingerprint.String, idempotency, idempotencyFingerprint.String, source); err == nil {
			return existing, nil
		} else if !isNoRows(err) {
			return sessionStartResult{}, err
		}
	}
	task, err := s.db.GetTaskForStart(ctx, db.GetTaskForStartParams{
		OrgID:         pgvalue.UUID(actor.OrgID),
		ProjectID:     projectID,
		EnvironmentID: environmentID,
		TaskID:        taskID,
	})
	if isNoRows(err) {
		return sessionStartResult{}, errTaskNotDeployed
	}
	if err != nil {
		return sessionStartResult{}, err
	}
	if task.ArchivedAt.Valid {
		return sessionStartResult{}, errTaskArchived
	}
	var deploymentTask db.GetDeploymentTaskRow
	if requestedWorkspaceID.Valid {
		deploymentTask, err = s.deploymentTask(ctx, placementWorkerGroupID, actor.OrgID, projectID, environmentID, attachedWorkspace.DeploymentID, taskID)
	} else {
		deploymentTask, err = s.deploymentTaskForRunRequest(ctx, placementWorkerGroupID, actor.OrgID, projectID, environmentID, taskID, runDeploymentSelection{})
	}
	if isNoRows(err) {
		return sessionStartResult{}, errTaskNotDeployed
	}
	if err != nil {
		return sessionStartResult{}, err
	}
	secretNames, err := deploymentTaskSecretNames(deploymentTask.SecretDeclarations)
	if err != nil {
		return sessionStartResult{}, err
	}
	if len(secretNames) > 0 {
		if s.secrets == nil {
			return sessionStartResult{}, errors.New("secret store is not configured")
		}
		projectUUID, err := pgvalue.UUIDValue(projectID)
		if err != nil {
			return sessionStartResult{}, fmt.Errorf("project id is invalid: %v", err)
		}
		environmentUUID, err := pgvalue.UUIDValue(environmentID)
		if err != nil {
			return sessionStartResult{}, fmt.Errorf("environment id is invalid: %v", err)
		}
		if err := s.secrets.CheckScopedNames(ctx, actor.OrgID, projectUUID, environmentUUID, secretNames); err != nil {
			return sessionStartResult{}, err
		}
	}
	maxDurationSeconds, err := runMaxDurationSeconds(request.Options.MaxDurationSeconds, deploymentTask.MaxActiveDurationMs)
	if err != nil {
		return sessionStartResult{}, err
	}
	lockedRetryPolicy, err := resolvedRetryPolicy(request.Options.Retry, deploymentTask.RetryPolicy)
	if err != nil {
		return sessionStartResult{}, err
	}
	scheduling, err := resolveRunScheduling(runOptions, deploymentTask)
	if err != nil {
		return sessionStartResult{}, err
	}
	scheduling, err = s.validateRunQueueOverride(ctx, actor.OrgID, projectID, environmentID, runOptions, deploymentTask, scheduling)
	if err != nil {
		return sessionStartResult{}, err
	}
	startClaim, err := s.claimSessionStart(ctx, actor.OrgID, projectID, environmentID, taskID, idempotency.key.String, idempotency.expiresAt)
	if err != nil {
		return sessionStartResult{}, err
	}
	claimResolved := false
	defer func() {
		if !claimResolved {
			startClaim.release(context.WithoutCancel(ctx))
		}
	}()
	resolvedClaimIsStale := false
	if startClaim.resolved && idempotency.key.Valid {
		if existing, hit, err := s.existingSessionStartIdempotency(ctx, actor.OrgID, placementWorkerGroupID, projectID, environmentID, taskID, idempotency.key.String, idempotencyFingerprint.String, externalID); err != nil {
			return sessionStartResult{}, err
		} else if hit {
			if err := s.ensureSessionStartSourceCurrent(ctx, source); err != nil {
				return sessionStartResult{}, err
			}
			return existing, nil
		}
		startClaim.clearResolved(context.WithoutCancel(ctx))
		startClaim, err = s.claimSessionStart(ctx, actor.OrgID, projectID, environmentID, taskID, idempotency.key.String, idempotency.expiresAt)
		if err != nil {
			return sessionStartResult{}, err
		}
		if startClaim.resolved {
			return sessionStartResult{}, errSessionStartPending
		}
		resolvedClaimIsStale = true
	}
	versionMetadata := requestVersionMetadataFromContext(ctx)
	runID := uuid.Must(uuid.NewV7())
	sessionID := uuid.Must(uuid.NewV7())
	traceID, err := tracing.NewTraceID()
	if err != nil {
		return sessionStartResult{}, fmt.Errorf("generate run trace id: %w", err)
	}
	rootSpanID, err := tracing.NewSpanID()
	if err != nil {
		return sessionStartResult{}, fmt.Errorf("generate run root span id: %w", err)
	}
	createdPayload, err := runCreatedEventPayload(taskID, payload, maxDurationSeconds, secretNames, lockedRetryPolicy, metadata, tags, "initial", json.RawMessage(`{}`))
	if err != nil {
		return sessionStartResult{}, fmt.Errorf("encode run created event: %w", err)
	}
	var result sessionStartResult
	err = s.inTx(ctx, func(work *txWork) error {
		if externalID != "" {
			if existing, err := work.q.GetSessionByExternalIDInWorkerGroup(ctx, db.GetSessionByExternalIDInWorkerGroupParams{
				OrgID:         pgvalue.UUID(actor.OrgID),
				WorkerGroupID: placementWorkerGroupID,
				ProjectID:     projectID,
				EnvironmentID: environmentID,
				ExternalID:    externalID,
			}); err == nil {
				if !sessionStartReusable(existing) {
					return errSessionTerminated
				}
				if existing.StartFingerprint != startFingerprint.String {
					return errSessionStartSessionFingerprint
				}
				if !existing.CurrentRunID.Valid {
					return errSessionNoCurrentRun
				}
				if idempotency.key.Valid {
					existingResult, existingHit, err := s.createSessionStartIdempotency(ctx, work.q, sessionStartIdempotencyBinding{
						ID:                 pgvalue.UUID(uuid.Must(uuid.NewV7())),
						OrgID:              pgvalue.UUID(actor.OrgID),
						WorkerGroupID:      existing.WorkerGroupID,
						ProjectID:          projectID,
						EnvironmentID:      environmentID,
						TaskID:             taskID,
						IdempotencyKey:     idempotency.key.String,
						RequestFingerprint: idempotencyFingerprint.String,
						SessionID:          existing.ID,
						FirstRunID:         existing.CurrentRunID,
						ExpiresAt:          idempotency.expiresAt,
					}, externalID, source)
					if err != nil {
						return err
					}
					if existingHit {
						result = existingResult
						return errSessionStartExistingHitRollback
					}
				}
				runRow, err := work.q.GetRunSummary(ctx, db.GetRunSummaryParams{OrgID: pgvalue.UUID(actor.OrgID), ID: existing.CurrentRunID})
				if err != nil {
					return err
				}
				work.AfterCommit(func(postCommitCtx context.Context) {
					startClaim.resolve(postCommitCtx)
					claimResolved = true
				})
				result = sessionStartResult{session: existing, run: getRunSummary(runRow), idempotencyHit: idempotency.key.Valid, sessionReused: true}
				return nil
			} else if !isNoRows(err) {
				return err
			}
		}
		if startClaim.resolved && !resolvedClaimIsStale {
			return errSessionStartPending
		}
		workspace, err := s.createOrAttachSessionStartWorkspace(ctx, work.q, actor.OrgID, projectID, environmentID, placementWorkerGroupID, deploymentTask, requestedWorkspaceID)
		if err != nil {
			return err
		}
		if workspace.WorkerGroupID != placementWorkerGroupID {
			return unavailable(errors.New("execution source row worker group does not match environment placement"))
		}
		var sessionPublicID string
		session, err := createWithPublicID(ctx, []publicIDSlot{{prefix: publicid.Session, value: &sessionPublicID}}, func() (db.Session, error) {
			return work.q.CreateSession(ctx, db.CreateSessionParams{
				ID:                  pgvalue.UUID(sessionID),
				PublicID:            sessionPublicID,
				OrgID:               pgvalue.UUID(actor.OrgID),
				WorkerGroupID:       placementWorkerGroupID,
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
		})
		if err != nil {
			if isUniqueViolation(err) && externalID != "" {
				return errSessionStartExternalIDRace
			}
			return err
		}
		var runPublicID string
		run, err := createWithPublicID(ctx, []publicIDSlot{{prefix: publicid.Run, value: &runPublicID}}, func() (db.CreateScopedRunRow, error) {
			return work.q.CreateScopedRun(ctx, db.CreateScopedRunParams{
				ID:                    pgvalue.UUID(runID),
				PublicID:              runPublicID,
				OrgID:                 pgvalue.UUID(actor.OrgID),
				WorkerGroupID:         placementWorkerGroupID,
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
				SessionID:             session.ID,
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
				TraceID:               pgtype.Text{String: traceID, Valid: true},
				RootSpanID:            rootSpanID,
				EventPayload:          createdPayload,
				ScheduleID:            source.scheduleID,
				ScheduleInstanceID:    source.scheduleInstanceID,
				ScheduleGeneration:    pgtype.Int8{Int64: source.scheduleGeneration, Valid: source.scheduleInstanceID.Valid},
				ScheduledAt:           source.scheduledAt,
				AllowDrainingRoute:    requestedWorkspaceID.Valid,
			})
		})
		if err != nil {
			if isNoRows(err) && source.scheduleInstanceID.Valid {
				return schedule.ErrTriggerSuperseded
			}
			if isNoRows(err) {
				return unavailable(errors.New("execution route is not available"))
			}
			return err
		}
		workspaceMountRequest, err := json.Marshal(map[string]string{
			"source": "session_start",
			"run_id": pgvalue.MustUUIDValue(run.ID).String(),
		})
		if err != nil {
			return err
		}
		mount, err := work.q.EnsureWorkspaceMountRequested(ctx, db.EnsureWorkspaceMountRequestedParams{
			ID:              pgvalue.UUID(uuid.Must(uuid.NewV7())),
			OrgID:           pgvalue.UUID(actor.OrgID),
			WorkerGroupID:   placementWorkerGroupID,
			ProjectID:       projectID,
			EnvironmentID:   environmentID,
			WorkspaceID:     workspace.ID,
			RequestPriority: scheduling.priority,
			Request:         workspaceMountRequest,
		})
		if err != nil {
			if isNoRows(err) {
				return workspaceMountPrerequisiteErrorWithStore(ctx, work.q, pgvalue.UUID(actor.OrgID), projectID, environmentID, workspace.ID)
			}
			return err
		}
		if err := work.q.SetQueuedRunWorkspaceMount(ctx, db.SetQueuedRunWorkspaceMountParams{
			OrgID:            pgvalue.UUID(actor.OrgID),
			RunID:            run.ID,
			WorkspaceID:      workspace.ID,
			WorkspaceMountID: mount.ID,
		}); err != nil {
			return err
		}
		var sessionRunPublicID string
		if _, err := createWithPublicID(ctx, []publicIDSlot{{prefix: publicid.SessionRun, value: &sessionRunPublicID}}, func() (db.SessionRun, error) {
			return work.q.CreateSessionRun(ctx, db.CreateSessionRunParams{
				ID:            pgvalue.UUID(uuid.Must(uuid.NewV7())),
				PublicID:      sessionRunPublicID,
				OrgID:         pgvalue.UUID(actor.OrgID),
				WorkerGroupID: session.WorkerGroupID,
				ProjectID:     projectID,
				EnvironmentID: environmentID,
				SessionID:     session.ID,
				RunID:         run.ID,
				DeploymentID:  deploymentTask.DeploymentID,
				TurnIndex:     0,
				Reason:        "initial",
			})
		}); err != nil {
			return err
		}
		session, err = work.q.SetSessionCurrentRun(ctx, db.SetSessionCurrentRunParams{
			OrgID:         pgvalue.UUID(actor.OrgID),
			WorkerGroupID: session.WorkerGroupID,
			ProjectID:     projectID,
			EnvironmentID: environmentID,
			SessionID:     session.ID,
			RunID:         run.ID,
		})
		if err != nil {
			return err
		}
		if idempotency.key.Valid {
			existingResult, existingHit, err := s.createSessionStartIdempotency(ctx, work.q, sessionStartIdempotencyBinding{
				ID:                 pgvalue.UUID(uuid.Must(uuid.NewV7())),
				OrgID:              pgvalue.UUID(actor.OrgID),
				WorkerGroupID:      session.WorkerGroupID,
				ProjectID:          projectID,
				EnvironmentID:      environmentID,
				TaskID:             taskID,
				IdempotencyKey:     idempotency.key.String,
				RequestFingerprint: idempotencyFingerprint.String,
				SessionID:          session.ID,
				FirstRunID:         run.ID,
				ExpiresAt:          idempotency.expiresAt,
			}, externalID, source)
			if err != nil {
				return err
			}
			if existingHit {
				result = existingResult
				return errSessionStartExistingHitRollback
			}
		}
		work.AfterCommit(func(postCommitCtx context.Context) {
			startClaim.resolve(postCommitCtx)
			claimResolved = true
			s.reconcilePreparedRuntimeSupplyForSandboxAsync(postCommitCtx, deploymentTask.DeploymentSandboxID, "session_start")
			if s.runEnqueuer != nil {
				if _, err := s.runEnqueuer.EnqueueRun(postCommitCtx, run.OrgID, run.ID); err != nil {
					s.log.Error("enqueue session run failed", "run_id", pgvalue.MustUUIDValue(run.ID).String(), "error", err)
				}
			}
		})
		result = sessionStartResult{session: session, run: createScopedRunSummary(run)}
		return nil
	})
	if errors.Is(err, errSessionStartExistingHitRollback) {
		return result, nil
	}
	if errors.Is(err, errSessionStartExternalIDRace) {
		existing, err := s.loadExistingSessionStart(ctx, s.db, actor.OrgID, placementWorkerGroupID, projectID, environmentID, taskID, externalID, startFingerprint.String, idempotency, idempotencyFingerprint.String, source)
		if err != nil {
			return sessionStartResult{}, err
		}
		startClaim.resolve(context.WithoutCancel(ctx))
		claimResolved = true
		return existing, nil
	}
	if err != nil {
		return sessionStartResult{}, err
	}
	return result, nil
}

func validateSessionExternalID(value string) error {
	if strings.ContainsRune(value, 0) {
		return errors.New("external_id must not contain NUL bytes")
	}
	if len(value) > maxSessionExternalIDBytes {
		return fmt.Errorf("external_id must be at most %d bytes", maxSessionExternalIDBytes)
	}
	return nil
}

func sessionAddressFromID(id uuid.UUID) sessionAddress {
	return sessionAddress{kind: sessionAddressID, id: id}
}

func sessionAddressFromExternalID(externalID string) (sessionAddress, error) {
	externalID = strings.TrimSpace(externalID)
	if externalID == "" {
		return sessionAddress{}, errors.New("external_id is required")
	}
	if err := validateSessionExternalID(externalID); err != nil {
		return sessionAddress{}, err
	}
	return sessionAddress{kind: sessionAddressExternalID, externalID: externalID}, nil
}

func sessionAddressFromAPIAddress(address api.SessionAddress) (sessionAddress, error) {
	switch strings.TrimSpace(address.Type) {
	case sessionAddressID:
		id, err := uuid.Parse(strings.TrimSpace(address.ID))
		if err != nil {
			return sessionAddress{}, errors.New("session.id must be a UUID")
		}
		return sessionAddressFromID(id), nil
	case sessionAddressExternalID:
		return sessionAddressFromExternalID(address.ExternalID)
	default:
		return sessionAddress{}, errors.New("session.type must be id or external_id")
	}
}

func sessionAddressFromRequest(r *http.Request) (sessionAddress, error) {
	rawSessionID := strings.TrimSpace(chi.URLParam(r, "sessionID"))
	if rawSessionID != "" {
		sessionID, err := parseUUIDString(rawSessionID, "sessionID")
		if err != nil {
			return sessionAddress{}, err
		}
		return sessionAddressFromID(sessionID), nil
	}
	return sessionAddressFromExternalID(r.URL.Query().Get("external_id"))
}

func sessionAddressResponse(session db.Session) api.SessionAddress {
	return api.SessionAddress{
		Type: sessionAddressID,
		ID:   pgvalue.MustUUIDValue(session.ID).String(),
	}
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

func (s *Server) createOrAttachSessionStartWorkspace(ctx context.Context, store db.Querier, orgID uuid.UUID, projectID pgtype.UUID, environmentID pgtype.UUID, placementWorkerGroupID string, task db.GetDeploymentTaskRow, requestedWorkspaceID pgtype.UUID) (db.Workspace, error) {
	if !requestedWorkspaceID.Valid {
		workspaceArtifact, initialWorkspace, err := s.createInitialWorkspaceArtifact(ctx, store, orgID, placementWorkerGroupID, projectID, environmentID)
		if err != nil {
			return db.Workspace{}, err
		}
		var workspacePublicID, initialVersionPublicID string
		workspace, err := createWithPublicID(ctx, []publicIDSlot{
			{prefix: publicid.Workspace, value: &workspacePublicID},
			{prefix: publicid.WorkspaceVersion, value: &initialVersionPublicID},
		}, func() (db.CreateWorkspaceFromSandboxRow, error) {
			return store.CreateWorkspaceFromSandbox(ctx, db.CreateWorkspaceFromSandboxParams{
				ID:                        pgvalue.UUID(uuid.Must(uuid.NewV7())),
				PublicID:                  workspacePublicID,
				OrgID:                     pgvalue.UUID(orgID),
				WorkerGroupID:             placementWorkerGroupID,
				ProjectID:                 projectID,
				EnvironmentID:             environmentID,
				DeploymentSandboxID:       task.DeploymentSandboxID,
				ExternalID:                "",
				Metadata:                  []byte(`{}`),
				Tags:                      []string{},
				RetentionPolicy:           []byte(`{}`),
				InitialVersionID:          pgvalue.UUID(uuid.Must(uuid.NewV7())),
				InitialVersionPublicID:    initialVersionPublicID,
				InitialArtifactID:         workspaceArtifact.ID,
				InitialArtifactEncoding:   initialWorkspace.Encoding,
				InitialArtifactEntryCount: int32(initialWorkspace.EntryCount),
				InitialContentDigest:      workspaceArtifact.Digest,
				InitialSizeBytes:          workspaceArtifact.SizeBytes,
			})
		})
		if isNoRows(err) {
			return db.Workspace{}, errSandboxNotDeployed
		}
		if err != nil {
			return db.Workspace{}, err
		}
		return workspaceFromCreateWorkspaceFromSandbox(workspace), nil
	}
	workspace, err := store.GetWorkspaceForSessionStart(ctx, db.GetWorkspaceForSessionStartParams{
		OrgID:         pgvalue.UUID(orgID),
		ProjectID:     projectID,
		EnvironmentID: environmentID,
		WorkspaceID:   requestedWorkspaceID,
	})
	if isNoRows(err) {
		return db.Workspace{}, unavailable(errors.New("record placement generation is not available"))
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
		WorkerGroupID:       workspace.WorkerGroupID,
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

func workspaceResourceFloorSatisfies(workspace db.GetWorkspaceForSessionStartRow, task db.GetDeploymentTaskRow) error {
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

func (s *Server) createSessionStartIdempotency(ctx context.Context, store db.Querier, binding sessionStartIdempotencyBinding, externalID string, source sessionStartSource) (sessionStartResult, bool, error) {
	created, existingResult, existingHit, err := s.tryCreateSessionStartIdempotency(ctx, store, binding, externalID, source)
	if err != nil || created.ID.Valid || existingHit {
		return existingResult, existingHit, err
	}
	if err := store.DeleteExpiredSessionStartIdempotency(ctx, db.DeleteExpiredSessionStartIdempotencyParams{
		OrgID:          binding.OrgID,
		ProjectID:      binding.ProjectID,
		EnvironmentID:  binding.EnvironmentID,
		TaskID:         binding.TaskID,
		IdempotencyKey: binding.IdempotencyKey,
	}); err != nil {
		return sessionStartResult{}, false, err
	}
	created, existingResult, existingHit, err = s.tryCreateSessionStartIdempotency(ctx, store, binding, externalID, source)
	if err != nil || created.ID.Valid || existingHit {
		return existingResult, existingHit, err
	}
	return sessionStartResult{}, false, errSessionStartPending
}

func (s *Server) tryCreateSessionStartIdempotency(ctx context.Context, store db.Querier, binding sessionStartIdempotencyBinding, externalID string, source sessionStartSource) (db.SessionStartIdempotency, sessionStartResult, bool, error) {
	created, err := store.CreateSessionStartIdempotency(ctx, db.CreateSessionStartIdempotencyParams{
		ID:                 binding.ID,
		OrgID:              binding.OrgID,
		WorkerGroupID:      binding.WorkerGroupID,
		ProjectID:          binding.ProjectID,
		EnvironmentID:      binding.EnvironmentID,
		TaskID:             binding.TaskID,
		IdempotencyKey:     binding.IdempotencyKey,
		RequestFingerprint: binding.RequestFingerprint,
		SessionID:          binding.SessionID,
		FirstRunID:         binding.FirstRunID,
		ExpiresAt:          binding.ExpiresAt,
	})
	if err == nil {
		return created, sessionStartResult{}, false, nil
	}
	if !isNoRows(err) {
		return db.SessionStartIdempotency{}, sessionStartResult{}, false, err
	}
	if existingResult, hit, hitErr := s.existingSessionStartIdempotency(ctx, pgvalue.MustUUIDValue(binding.OrgID), binding.WorkerGroupID, binding.ProjectID, binding.EnvironmentID, binding.TaskID, binding.IdempotencyKey, binding.RequestFingerprint, externalID); hitErr != nil {
		return db.SessionStartIdempotency{}, sessionStartResult{}, false, hitErr
	} else if hit {
		if err := s.ensureSessionStartSourceCurrent(ctx, source); err != nil {
			return db.SessionStartIdempotency{}, sessionStartResult{}, false, err
		}
		return db.SessionStartIdempotency{
			ID:                 binding.ID,
			OrgID:              binding.OrgID,
			WorkerGroupID:      binding.WorkerGroupID,
			ProjectID:          binding.ProjectID,
			EnvironmentID:      binding.EnvironmentID,
			TaskID:             binding.TaskID,
			IdempotencyKey:     binding.IdempotencyKey,
			RequestFingerprint: binding.RequestFingerprint,
			SessionID:          existingResult.session.ID,
			FirstRunID:         existingResult.run.ID,
			ExpiresAt:          binding.ExpiresAt,
		}, existingResult, true, nil
	}
	return db.SessionStartIdempotency{}, sessionStartResult{}, false, nil
}

func (s *Server) loadExistingSessionStart(ctx context.Context, store db.Querier, orgID uuid.UUID, workerGroupID string, projectID pgtype.UUID, environmentID pgtype.UUID, taskID string, externalID string, startFingerprint string, idempotency runIdempotency, idempotencyFingerprint string, source sessionStartSource) (sessionStartResult, error) {
	existing, err := store.GetSessionByExternalIDInWorkerGroup(ctx, db.GetSessionByExternalIDInWorkerGroupParams{
		OrgID:         pgvalue.UUID(orgID),
		WorkerGroupID: workerGroupID,
		ProjectID:     projectID,
		EnvironmentID: environmentID,
		ExternalID:    externalID,
	})
	if err != nil {
		return sessionStartResult{}, err
	}
	if !sessionStartReusable(existing) {
		return sessionStartResult{}, errSessionTerminated
	}
	if existing.StartFingerprint != startFingerprint {
		return sessionStartResult{}, errSessionStartSessionFingerprint
	}
	if !existing.CurrentRunID.Valid {
		return sessionStartResult{}, errSessionNoCurrentRun
	}
	if idempotency.key.Valid {
		if existingResult, existingHit, err := s.createSessionStartIdempotency(ctx, store, sessionStartIdempotencyBinding{
			ID:                 pgvalue.UUID(uuid.Must(uuid.NewV7())),
			OrgID:              pgvalue.UUID(orgID),
			WorkerGroupID:      existing.WorkerGroupID,
			ProjectID:          projectID,
			EnvironmentID:      environmentID,
			TaskID:             taskID,
			IdempotencyKey:     idempotency.key.String,
			RequestFingerprint: idempotencyFingerprint,
			SessionID:          existing.ID,
			FirstRunID:         existing.CurrentRunID,
			ExpiresAt:          idempotency.expiresAt,
		}, externalID, source); err != nil {
			return sessionStartResult{}, err
		} else if existingHit {
			return existingResult, nil
		}
	}
	runRow, err := store.GetRunSummary(ctx, db.GetRunSummaryParams{OrgID: pgvalue.UUID(orgID), ID: existing.CurrentRunID})
	if err != nil {
		return sessionStartResult{}, err
	}
	return sessionStartResult{session: existing, run: getRunSummary(runRow), idempotencyHit: idempotency.key.Valid, sessionReused: true}, nil
}

func (s *Server) ensureSessionStartSourceCurrent(ctx context.Context, source sessionStartSource) error {
	if !source.scheduleInstanceID.Valid {
		return nil
	}
	current, err := s.db.ScheduleInstanceTriggerIsCurrent(ctx, db.ScheduleInstanceTriggerIsCurrentParams{
		WorkerGroupID: source.scheduleWorkerGroupID,
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

func sessionStartRunOptions(options api.SessionStartOptions) api.CreateRunOptions {
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

func sessionStartRequestFingerprint(taskID string, payload json.RawMessage, options api.SessionStartOptions, externalID string, expiresAt *time.Time) (pgtype.Text, error) {
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
		return pgtype.Text{}, fmt.Errorf("session start fingerprint encode failed: %w", err)
	}
	digest := sha256.Sum256(body)
	return pgtype.Text{String: hex.EncodeToString(digest[:]), Valid: true}, nil
}

func (s *Server) existingSessionStartIdempotency(ctx context.Context, orgID uuid.UUID, placementWorkerGroupID string, projectID pgtype.UUID, environmentID pgtype.UUID, taskID string, key string, fingerprint string, externalID string) (sessionStartResult, bool, error) {
	existing, err := s.db.GetSessionStartIdempotency(ctx, db.GetSessionStartIdempotencyParams{
		OrgID:          pgvalue.UUID(orgID),
		ProjectID:      projectID,
		EnvironmentID:  environmentID,
		TaskID:         taskID,
		IdempotencyKey: key,
	})
	if isNoRows(err) {
		return sessionStartResult{}, false, nil
	}
	if err != nil {
		return sessionStartResult{}, false, err
	}
	if existing.RequestFingerprint != fingerprint {
		return sessionStartResult{}, false, errSessionStartIdempotencyFingerprint
	}
	if existing.WorkerGroupID != placementWorkerGroupID {
		return sessionStartResult{}, false, nil
	}
	session := sessionFromIdempotency(existing)
	if strings.TrimSpace(externalID) != "" && session.ExternalID != strings.TrimSpace(externalID) {
		return sessionStartResult{}, false, errSessionStartIdempotencyExternalID
	}
	_ = s.db.TouchSessionStartIdempotency(ctx, db.TouchSessionStartIdempotencyParams{OrgID: pgvalue.UUID(orgID), ID: existing.ID})
	return sessionStartResult{session: session, run: runSummaryFromIdempotency(existing), idempotencyHit: true}, true, nil
}

func sessionFromIdempotency(row db.GetSessionStartIdempotencyRow) db.Session {
	return db.Session{
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
		CancelledAt:         row.SessionCancelledAt,
		CreatedAt:           row.SessionCreatedAt,
		UpdatedAt:           row.SessionUpdatedAt,
	}
}

func runSummaryFromIdempotency(row db.GetSessionStartIdempotencyRow) runSummary {
	return runSummary{
		ID:                   row.RunID,
		OrgID:                row.RunOrgID,
		ProjectID:            row.RunProjectID,
		EnvironmentID:        row.RunEnvironmentID,
		DeploymentID:         row.RunDeploymentID,
		DeploymentTaskID:     row.RunDeploymentTaskID,
		SessionID:            row.SessionID,
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
		return defaultSessionWaitTimeout
	}
	timeout := time.Duration(seconds) * time.Second
	if timeout > maxSessionWaitTimeout {
		return maxSessionWaitTimeout
	}
	return timeout
}

func (s *Server) writeSessionStartError(w http.ResponseWriter, err error) {
	var apiErr apiError
	if errors.As(err, &apiErr) {
		writeError(w, err)
		return
	}
	switch {
	case errors.Is(err, errTaskArchived), errors.Is(err, errTaskNotDeployed), errors.Is(err, errSessionStartSessionFingerprint), errors.Is(err, errSessionStartIdempotencyFingerprint), errors.Is(err, errSessionStartIdempotencyExternalID), errors.Is(err, errSessionTerminated), errors.Is(err, errSessionNoCurrentRun), errors.Is(err, errSandboxNotDeployed), errors.Is(err, errWorkspaceSandboxIncompatible), errors.Is(err, errWorkspaceResourceFloor):
		writeError(w, conflict(err))
	case errors.Is(err, errSessionStartPending):
		w.Header().Set("Retry-After", "1")
		writeErrorStatus(w, http.StatusAccepted, err)
	case errors.Is(err, errSessionStartCoordinationUnavailable):
		writeError(w, unavailable(err))
	case errors.Is(err, errPermissionRequired):
		writeError(w, forbidden(err))
	case isCreateRunConfigError(err):
		writeError(w, unavailable(err))
	case isCreateRunClientError(err):
		writeError(w, badRequest(err))
	default:
		s.log.Error("session start failed", "error", err)
		writeError(w, errors.New("start task"))
	}
}

func sessionStartReusable(session db.Session) bool {
	return session.Status == db.SessionStatusOpen && (!session.ExpiresAt.Valid || session.ExpiresAt.Time.After(time.Now()))
}

const (
	sessionActivityIdle    = "idle"
	sessionActivityQueued  = "queued"
	sessionActivityRunning = "running"
	sessionActivityWaiting = "waiting"
)

type sessionDerivedStateValue struct {
	activity string
	canClose bool
}

func sessionResponseWithDerived(session db.Session, derived sessionDerivedStateValue) api.SessionResponse {
	return sessionResponseWithDerivedMode(session, derived, true, false)
}

func sessionResponseWithDerivedMode(session db.Session, derived sessionDerivedStateValue, unwrapResult bool, timedOut bool) api.SessionResponse {
	response := api.SessionResponse{
		ID:                  pgvalue.MustUUIDValue(session.ID).String(),
		ProjectID:           pgvalue.MustUUIDValue(session.ProjectID).String(),
		EnvironmentID:       pgvalue.MustUUIDValue(session.EnvironmentID).String(),
		TaskID:              session.TaskID,
		InitialDeploymentID: pgvalue.MustUUIDValue(session.InitialDeploymentID).String(),
		ActiveDeploymentID:  pgvalue.MustUUIDValue(session.ActiveDeploymentID).String(),
		ExternalID:          session.ExternalID,
		Status:              string(session.Status),
		Activity:            derived.activity,
		CanClose:            derived.canClose,
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
		result, resultErr, ok := unwrapStoredSessionResult(session.Result)
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
	if session.ExpiredAt.Valid {
		expiredAt := session.ExpiredAt.Time
		response.ExpiredAt = &expiredAt
	}
	return response
}

func defaultSessionDerivedState(session db.Session) sessionDerivedStateValue {
	if session.Status == db.SessionStatusOpen &&
		!session.CurrentRunID.Valid &&
		(!session.ExpiresAt.Valid || session.ExpiresAt.Time.After(time.Now())) {
		return sessionDerivedStateValue{activity: sessionActivityIdle, canClose: true}
	}
	return sessionDerivedStateValue{activity: sessionActivityIdle}
}

func sessionDerivedFromActivity(activity string, canClose bool) sessionDerivedStateValue {
	switch activity {
	case sessionActivityQueued, sessionActivityRunning, sessionActivityWaiting:
		return sessionDerivedStateValue{activity: activity, canClose: canClose}
	default:
		return sessionDerivedStateValue{activity: sessionActivityIdle, canClose: canClose}
	}
}

func (s *Server) sessionDerivedState(ctx context.Context, session db.Session) (sessionDerivedStateValue, error) {
	row, err := s.db.GetSessionActivity(ctx, db.GetSessionActivityParams{
		OrgID:         session.OrgID,
		ProjectID:     session.ProjectID,
		EnvironmentID: session.EnvironmentID,
		ID:            session.ID,
	})
	if err != nil {
		return sessionDerivedStateValue{}, err
	}
	return sessionDerivedFromActivity(row.Activity, row.CanClose), nil
}

func unwrapStoredSessionResult(raw []byte) (json.RawMessage, json.RawMessage, bool) {
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

func (s *Server) listSessions(w http.ResponseWriter, r *http.Request) {
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
	limit := defaultSessionListLimit
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		parsed, err := strconv.ParseInt(raw, 10, 32)
		if err != nil || parsed <= 0 || parsed > int64(maxSessionListLimit) {
			writeError(w, badRequest(fmt.Errorf("limit must be an integer between 1 and %d", maxSessionListLimit)))
			return
		}
		limit = int32(parsed)
	}
	externalID := strings.TrimSpace(r.URL.Query().Get("external_id"))
	if externalID != "" {
		if err := validateSessionExternalID(externalID); err != nil {
			writeError(w, badRequest(err))
			return
		}
	}
	sessions, err := s.db.ListSessions(r.Context(), db.ListSessionsParams{
		OrgID:            pgvalue.UUID(actor.OrgID),
		ProjectID:        projectID,
		EnvironmentID:    environmentID,
		StatusFilter:     strings.TrimSpace(r.URL.Query().Get("status")),
		TaskIDFilter:     strings.TrimSpace(r.URL.Query().Get("task_id")),
		ExternalIDFilter: externalID,
		RowLimit:         limit,
	})
	if err != nil {
		writeError(w, errors.New("list sessions"))
		return
	}
	response := make([]api.SessionResponse, 0, len(sessions))
	derived := map[pgtype.UUID]sessionDerivedStateValue{}
	if len(sessions) > 0 {
		ids := make([]pgtype.UUID, 0, len(sessions))
		for _, session := range sessions {
			ids = append(ids, session.ID)
		}
		activityRows, err := s.db.ListSessionActivities(r.Context(), db.ListSessionActivitiesParams{
			OrgID:         pgvalue.UUID(actor.OrgID),
			ProjectID:     projectID,
			EnvironmentID: environmentID,
			SessionIds:    ids,
		})
		if err != nil {
			writeError(w, errors.New("list session activities"))
			return
		}
		for _, row := range activityRows {
			derived[row.ID] = sessionDerivedFromActivity(row.Activity, row.CanClose)
		}
	}
	for _, session := range sessions {
		state, ok := derived[session.ID]
		if !ok {
			state = defaultSessionDerivedState(session)
		}
		response = append(response, sessionResponseWithDerived(session, state))
	}
	writeJSON(w, http.StatusOK, api.ListSessionsResponse{Sessions: response})
}

func (s *Server) getSession(w http.ResponseWriter, r *http.Request) {
	session, ok := s.loadSessionForRequest(w, r, auth.PermissionRunsRead)
	if !ok {
		return
	}
	derived, err := s.sessionDerivedState(r.Context(), session)
	if err != nil {
		writeError(w, errors.New("load session activity"))
		return
	}
	writeJSON(w, http.StatusOK, sessionResponseWithDerived(session, derived))
}

func (s *Server) patchSession(w http.ResponseWriter, r *http.Request) {
	session, ok := s.loadSessionForRequest(w, r, auth.PermissionRunsManage)
	if !ok {
		return
	}
	var request api.PatchSessionRequest
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
			writeError(w, badRequest(errSessionExpiresAtPatch))
			return
		}
	}
	updated, err := s.db.PatchSession(r.Context(), db.PatchSessionParams{
		OrgID:         session.OrgID,
		WorkerGroupID: session.WorkerGroupID,
		ProjectID:     session.ProjectID,
		EnvironmentID: session.EnvironmentID,
		ID:            session.ID,
		Metadata:      metadata,
		Tags:          tags,
		ExpiresAt:     timePtrToTimestamptz(request.ExpiresAt),
	})
	if isNoRows(err) {
		writeError(w, conflict(errSessionTerminated))
		return
	}
	if err != nil {
		writeError(w, errors.New("patch session"))
		return
	}
	derived, err := s.sessionDerivedState(r.Context(), updated)
	if err != nil {
		writeError(w, errors.New("load session activity"))
		return
	}
	writeJSON(w, http.StatusOK, sessionResponseWithDerived(updated, derived))
}

func (s *Server) waitForRunTerminal(ctx context.Context, actor auth.Actor, runID pgtype.UUID, timeout time.Duration) (runSummary, bool, error) {
	deadline := time.Now().Add(timeout)
	for {
		row, err := s.db.GetRunSummary(ctx, db.GetRunSummaryParams{
			OrgID: pgvalue.UUID(actor.OrgID),
			ID:    runID,
		})
		if err != nil {
			return runSummary{}, false, err
		}
		run := getRunSummary(row)
		if runStatusTerminal(run.Status) {
			return run, false, nil
		}
		if time.Now().After(deadline) {
			return run, true, nil
		}
		timer := time.NewTimer(sessionWaitPollEvery)
		select {
		case <-ctx.Done():
			timer.Stop()
			return runSummary{}, false, ctx.Err()
		case <-timer.C:
		}
	}
}

func (s *Server) closeSession(w http.ResponseWriter, r *http.Request) {
	session, ok := s.loadSessionForRequest(w, r, auth.PermissionRunsManage)
	if !ok {
		return
	}
	var request api.CloseSessionRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid session close JSON: %w", err)))
		return
	}
	reason := strings.TrimSpace(request.Reason)
	if reason == "" {
		reason = "closed"
	}
	closed, err := s.db.CloseSession(r.Context(), db.CloseSessionParams{
		OrgID:         session.OrgID,
		WorkerGroupID: session.WorkerGroupID,
		ProjectID:     session.ProjectID,
		EnvironmentID: session.EnvironmentID,
		ID:            session.ID,
		Reason:        reason,
	})
	if isNoRows(err) {
		if session.ExpiresAt.Valid && !session.ExpiresAt.Time.After(time.Now()) {
			expired, expireErr := s.db.ExpireSessionIfDue(r.Context(), db.ExpireSessionIfDueParams{
				OrgID:         session.OrgID,
				WorkerGroupID: session.WorkerGroupID,
				ProjectID:     session.ProjectID,
				EnvironmentID: session.EnvironmentID,
				ID:            session.ID,
			})
			if expireErr == nil {
				writeJSON(w, http.StatusOK, sessionResponseWithDerived(expired, sessionDerivedStateValue{activity: sessionActivityIdle}))
				return
			}
			if !isNoRows(expireErr) {
				writeError(w, errors.New("expire session"))
				return
			}
		}
		activeRun, canRetry, stateErr := s.closeSessionCurrentRunState(r.Context(), session)
		if stateErr != nil {
			writeError(w, errors.New("close session"))
			return
		}
		if activeRun {
			writeError(w, conflict(errCloseRunActive))
			return
		}
		if canRetry {
			closed, err = s.db.CloseSession(r.Context(), db.CloseSessionParams{
				OrgID:         session.OrgID,
				ProjectID:     session.ProjectID,
				EnvironmentID: session.EnvironmentID,
				ID:            session.ID,
				Reason:        reason,
			})
			if err == nil {
				derived, stateErr := s.sessionDerivedState(r.Context(), closed)
				if stateErr != nil {
					writeError(w, errors.New("load session activity"))
					return
				}
				writeJSON(w, http.StatusOK, sessionResponseWithDerived(closed, derived))
				return
			}
			if !isNoRows(err) {
				writeError(w, errors.New("close session"))
				return
			}
			activeRun, _, stateErr = s.closeSessionCurrentRunState(r.Context(), session)
			if stateErr != nil {
				writeError(w, errors.New("close session"))
				return
			}
			if activeRun {
				writeError(w, conflict(errCloseRunActive))
				return
			}
		}
		writeError(w, conflict(errSessionTerminated))
		return
	}
	if err != nil {
		writeError(w, errors.New("close session"))
		return
	}
	derived, err := s.sessionDerivedState(r.Context(), closed)
	if err != nil {
		writeError(w, errors.New("load session activity"))
		return
	}
	writeJSON(w, http.StatusOK, sessionResponseWithDerived(closed, derived))
}

func (s *Server) closeSessionCurrentRunState(ctx context.Context, session db.Session) (activeRun bool, canRetry bool, err error) {
	latest, err := s.db.GetSession(ctx, db.GetSessionParams{
		OrgID:         session.OrgID,
		ProjectID:     session.ProjectID,
		EnvironmentID: session.EnvironmentID,
		ID:            session.ID,
	})
	if isNoRows(err) {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	if latest.Status != db.SessionStatusOpen {
		return false, false, nil
	}
	derived, err := s.sessionDerivedState(ctx, latest)
	if err != nil {
		return false, false, err
	}
	if derived.canClose {
		return false, true, nil
	}
	return true, false, nil
}

func (s *Server) cancelSession(w http.ResponseWriter, r *http.Request) {
	session, ok := s.loadSessionForRequest(w, r, auth.PermissionRunsManage)
	if !ok {
		return
	}
	var request api.CancelSessionRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid session cancel JSON: %w", err)))
		return
	}
	reason := strings.TrimSpace(request.Reason)
	if reason == "" {
		reason = "cancelled"
	}
	actor := actorFromContext(r.Context())
	var response api.SessionResponse
	err := s.inTx(r.Context(), func(work *txWork) error {
		locked, err := work.q.LockSession(r.Context(), db.LockSessionParams{
			OrgID:         session.OrgID,
			WorkerGroupID: session.WorkerGroupID,
			ProjectID:     session.ProjectID,
			EnvironmentID: session.EnvironmentID,
			ID:            session.ID,
		})
		if isNoRows(err) {
			return notFound(errors.New("session not found"))
		}
		if err != nil {
			return errors.New("cancel session")
		}
		if locked.Status == db.SessionStatusCancelled {
			response = sessionResponseWithDerived(locked, sessionDerivedStateValue{activity: sessionActivityIdle})
			return nil
		}
		if locked.Status != db.SessionStatusOpen {
			return conflict(errSessionTerminated)
		}
		if locked.CurrentRunID.Valid {
			if err := s.cancelSessionRun(r.Context(), work.q, actor, locked, reason); err != nil {
				return errors.New("cancel session run")
			}
		}
		cancelled, err := work.q.CancelSession(r.Context(), db.CancelSessionParams{
			OrgID:         locked.OrgID,
			WorkerGroupID: locked.WorkerGroupID,
			ProjectID:     locked.ProjectID,
			EnvironmentID: locked.EnvironmentID,
			ID:            locked.ID,
			Reason:        reason,
		})
		if isNoRows(err) {
			return conflict(errSessionTerminated)
		}
		if err != nil {
			return errors.New("cancel session")
		}
		response = sessionResponseWithDerived(cancelled, sessionDerivedStateValue{activity: sessionActivityIdle})
		return nil
	})
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) cancelSessionRun(ctx context.Context, store db.Querier, actor auth.Actor, session db.Session, reason string) error {
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
		OrgID:         session.OrgID,
		WorkerGroupID: run.WorkerGroupID,
		RunID:         session.CurrentRunID,
		Reason:        reason,
		Force:         false,
		OperationID:   operation.ID,
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

func (s *Server) listSessionRuns(w http.ResponseWriter, r *http.Request) {
	session, ok := s.loadSessionForRequest(w, r, auth.PermissionRunsRead)
	if !ok {
		return
	}
	rows, err := s.db.ListSessionRuns(r.Context(), db.ListSessionRunsParams{
		OrgID:         session.OrgID,
		ProjectID:     session.ProjectID,
		EnvironmentID: session.EnvironmentID,
		SessionID:     session.ID,
	})
	if err != nil {
		writeError(w, errors.New("list session runs"))
		return
	}
	response := make([]api.SessionRunResponse, 0, len(rows))
	for _, row := range rows {
		item := api.SessionRunResponse{
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
	writeJSON(w, http.StatusOK, api.ListSessionRunsResponse{Runs: response})
}

func (s *Server) loadSessionForRequest(w http.ResponseWriter, r *http.Request, permission auth.Permission) (db.Session, bool) {
	address, err := sessionAddressFromRequest(r)
	if err != nil {
		writeError(w, badRequest(err))
		return db.Session{}, false
	}
	actor := actorFromContext(r.Context())
	var session db.Session
	var sessionErr error
	pathProjectID := strings.TrimSpace(chi.URLParam(r, "projectID"))
	pathEnvironmentID := strings.TrimSpace(chi.URLParam(r, "environmentID"))
	if pathProjectID != "" || pathEnvironmentID != "" {
		_, projectID, environmentID, scopeErr := s.requestEnvironmentScopeFromRequest(r, actor, "", "")
		if scopeErr != nil {
			writeError(w, badRequest(scopeErr))
			return db.Session{}, false
		}
		session, sessionErr = s.loadSessionAddressInScope(r.Context(), actor.OrgID, projectID, environmentID, address)
	} else {
		if actor.Kind == auth.ActorKindSession {
			writeError(w, forbidden(errors.New("session actor must use a project/environment scoped session route")))
			return db.Session{}, false
		}
		session, sessionErr = s.loadSessionByActorAddress(r.Context(), actor, address)
	}
	if isNoRows(sessionErr) {
		writeError(w, notFound(errors.New("session not found")))
		return db.Session{}, false
	}
	if sessionErr != nil {
		if isScopeRequestError(sessionErr) || errors.Is(sessionErr, errAPIKeyEnvironmentScopeRequired) || errors.Is(sessionErr, errSessionExternalIDScopeRequired) {
			writeError(w, badRequest(sessionErr))
			return db.Session{}, false
		}
		writeError(w, errors.New("get session"))
		return db.Session{}, false
	}
	scope := auth.Scope{
		OrgID:         actor.OrgID,
		ProjectID:     pgvalue.MustUUIDValue(session.ProjectID).String(),
		EnvironmentID: pgvalue.MustUUIDValue(session.EnvironmentID).String(),
	}
	if !actor.HasPermission(permission, scope) {
		writeError(w, forbidden(errPermissionRequired))
		return db.Session{}, false
	}
	if err := s.requireRoutableRecordWorkerGroup(r.Context(), s.db, actor.OrgID, session.ProjectID, session.EnvironmentID, session.WorkerGroupID); err != nil {
		writeError(w, err)
		return db.Session{}, false
	}
	return session, true
}

func (s *Server) loadSessionAddressInScope(ctx context.Context, orgID uuid.UUID, projectID pgtype.UUID, environmentID pgtype.UUID, address sessionAddress) (db.Session, error) {
	return loadSessionAddressInScope(ctx, s.db, orgID, projectID, environmentID, address)
}

func loadSessionAddressInScope(ctx context.Context, store db.Querier, orgID uuid.UUID, projectID pgtype.UUID, environmentID pgtype.UUID, address sessionAddress) (db.Session, error) {
	if address.kind == sessionAddressExternalID {
		return store.GetSessionByExternalID(ctx, db.GetSessionByExternalIDParams{
			OrgID:         pgvalue.UUID(orgID),
			ProjectID:     projectID,
			EnvironmentID: environmentID,
			ExternalID:    address.externalID,
		})
	}
	return store.GetSession(ctx, db.GetSessionParams{
		OrgID:         pgvalue.UUID(orgID),
		ProjectID:     projectID,
		EnvironmentID: environmentID,
		ID:            pgvalue.UUID(address.id),
	})
}

func loadSessionAddressInWorkerGroup(ctx context.Context, store db.Querier, workerGroupID string, orgID uuid.UUID, projectID pgtype.UUID, environmentID pgtype.UUID, address sessionAddress) (db.Session, error) {
	if address.kind == sessionAddressExternalID {
		return store.GetSessionByExternalIDInWorkerGroup(ctx, db.GetSessionByExternalIDInWorkerGroupParams{
			OrgID:         pgvalue.UUID(orgID),
			WorkerGroupID: workerGroupID,
			ProjectID:     projectID,
			EnvironmentID: environmentID,
			ExternalID:    address.externalID,
		})
	}
	return store.GetSessionInWorkerGroup(ctx, db.GetSessionInWorkerGroupParams{
		OrgID:         pgvalue.UUID(orgID),
		WorkerGroupID: workerGroupID,
		ProjectID:     projectID,
		EnvironmentID: environmentID,
		ID:            pgvalue.UUID(address.id),
	})
}

func (s *Server) loadSessionByActorAddress(ctx context.Context, actor auth.Actor, address sessionAddress) (db.Session, error) {
	if actor.Kind == auth.ActorKindAPIKey {
		scope, ok := actor.EnvironmentScope()
		if !ok {
			return db.Session{}, errAPIKeyEnvironmentScopeRequired
		}
		projectID, environmentID, err := runScopeIDs(scope)
		if err != nil {
			return db.Session{}, err
		}
		return s.loadSessionAddressInScope(ctx, actor.OrgID, projectID, environmentID, address)
	}
	if address.kind == sessionAddressExternalID {
		return db.Session{}, errSessionExternalIDScopeRequired
	}
	return s.db.GetSessionByOrgID(ctx, db.GetSessionByOrgIDParams{
		OrgID: pgvalue.UUID(actor.OrgID),
		ID:    pgvalue.UUID(address.id),
	})
}
