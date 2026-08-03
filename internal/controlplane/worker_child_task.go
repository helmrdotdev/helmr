package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/deployment"
	"github.com/helmrdotdev/helmr/internal/idempotency"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/helmrdotdev/helmr/internal/tracing"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

var (
	errChildTaskInvokeStale       = errors.New("child task invocation authority is stale")
	errChildTaskInvokeUnsupported = errors.New("child task invocation method is unsupported")
	errChildTaskSameWorkspace     = errors.New("same-workspace child task start is unsupported")
	errWorkspaceHandoffConflict   = errors.New("same-workspace call reached a different workspace frontier")
)

type childTaskOptions struct {
	Queue          string                     `json:"queue,omitempty"`
	ConcurrencyKey *string                    `json:"concurrency_key,omitempty"`
	Priority       int32                      `json:"priority,omitempty"`
	TTL            string                     `json:"ttl,omitempty"`
	Retry          *api.StartActorRetryPolicy `json:"retry,omitempty"`
	Metadata       json.RawMessage            `json:"metadata,omitempty"`
	Tags           []string                   `json:"tags,omitempty"`
}

type childTaskReceipt struct {
	RunID                  string `json:"runId"`
	WorkspaceID            string `json:"workspaceId"`
	RunWaitID              string `json:"runWaitId,omitempty"`
	ResumeAttachID         string `json:"resumeAttachId,omitempty"`
	BaseWorkspaceVersionID string `json:"baseWorkspaceVersionId,omitempty"`
	BaseWorkspaceDigest    string `json:"baseWorkspaceDigest,omitempty"`
}

type childTaskInvokeInput struct {
	Request           workerapi.InvokeChildTaskRequest
	Parsed            parsedRunLeaseFence
	Worker            workerActor
	SourceWorkspaceID uuid.UUID
	Normalized        normalizedTaskStart
	RunWaitID         uuid.UUID
	ResumeAttachID    uuid.UUID
}

type childTaskInvokeResult struct {
	taskStartResult
	openedWait *workerapi.CreateRunWaitResponse
}

func (s *Server) workerInvokeChildTask(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, unavailable(errors.New("run storage is not configured")))
		return
	}
	var request workerapi.InvokeChildTaskRequest
	if err := decodeClosedWorkerRequest(r, &request); err != nil {
		writeError(w, badRequest(codedError{code: "invalid_child_task_start", message: err.Error()}))
		return
	}
	parsed, err := parseRunLeaseFence(request.Lease)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	if request.Method != "start" && request.Method != "call" {
		writeError(w, badRequest(codedError{
			code: "child_task_method_unsupported", message: errChildTaskInvokeUnsupported.Error(),
		}))
		return
	}
	correlationID, err := ids.Parse(request.CorrelationID)
	if err != nil {
		writeError(w, badRequest(errors.New("correlation_id must be a canonical UUIDv7")))
		return
	}
	var runWaitID uuid.UUID
	var resumeAttachID uuid.UUID
	if request.Method == "call" {
		runWaitID, err = ids.Parse(request.RunWaitID)
		if err != nil {
			writeError(w, badRequest(errors.New("run_wait_id must be a canonical UUIDv7 for task.call()")))
			return
		}
		resumeAttachID, err = ids.Parse(request.ResumeAttachID)
		if err != nil {
			writeError(w, badRequest(errors.New("resume_attach_id must be a canonical UUIDv7 for task.call()")))
			return
		}
		if correlationID == runWaitID || correlationID == resumeAttachID || runWaitID == resumeAttachID {
			writeError(w, badRequest(errors.New("correlation_id, run_wait_id, and resume_attach_id must be distinct")))
			return
		}
	} else if request.RunWaitID != "" || request.ResumeAttachID != "" {
		writeError(w, badRequest(errors.New("task.start() must not include run_wait_id or resume_attach_id")))
		return
	}
	idempotencyKey, err := normalizeIdempotencyKey(request.IdempotencyKey)
	if err != nil {
		writeError(w, badRequest(codedError{code: "invalid_idempotency_key", message: err.Error()}))
		return
	}
	request.IdempotencyKey = idempotencyKey
	if request.Method == "call" && request.IdempotencyKey == "" {
		writeError(w, badRequest(codedError{
			code: "invalid_idempotency_key", message: "task.call() requires an idempotency key",
		}))
		return
	}
	worker := workerFromContext(r.Context())
	locators, err := loadChildTaskInvokeLocators(r.Context(), s.db, worker, request.Lease, parsed)
	if err != nil {
		writeError(w, conflict(errChildTaskInvokeStale))
		return
	}
	normalized, err := normalizeWorkerChildTaskRequest(request, locators)
	if err != nil {
		writeError(w, badRequest(codedError{code: "invalid_child_task_start", message: err.Error()}))
		return
	}
	result, err := s.invokeChildTask(r.Context(), childTaskInvokeInput{
		Request: request, Parsed: parsed, Worker: worker,
		SourceWorkspaceID: pgvalue.MustUUIDValue(locators.WorkspaceID),
		Normalized:        normalized,
		RunWaitID:         runWaitID,
		ResumeAttachID:    resumeAttachID,
	})
	if err != nil {
		s.writeChildTaskInvokeError(w, request.CorrelationID, request.Method, err)
		return
	}
	response := workerapi.InvokeChildTaskResponse{
		CorrelationID: request.CorrelationID,
		OpenedWait:    result.openedWait,
	}
	if result.RunID != uuid.Nil {
		response.Completed = &workerapi.ChildTaskStartResult{RunID: result.RunID.String()}
	}
	writeJSON(w, http.StatusOK, response)
}

func normalizeWorkerChildTaskRequest(
	request workerapi.InvokeChildTaskRequest,
	locators db.GetLiveRunLeaseLocatorsRow,
) (normalizedTaskStart, error) {
	if err := api.ValidateTaskID(request.TaskDeclaredID); err != nil {
		return normalizedTaskStart{}, err
	}
	var workspace api.WorkspaceTarget
	if err := decodeClosedJSON(request.Workspace, &workspace); err != nil {
		return normalizedTaskStart{}, fmt.Errorf("invalid workspace: %w", err)
	}
	var options childTaskOptions
	if err := decodeClosedJSON(request.Options, &options); err != nil {
		return normalizedTaskStart{}, fmt.Errorf("invalid options: %w", err)
	}
	ttl, retry, err := taskStartPolicyFromAPI(api.StartTaskRequest{
		TTL: options.TTL, Retry: options.Retry,
	})
	if err != nil {
		return normalizedTaskStart{}, err
	}
	return normalizeTaskStart(taskStartRequest{
		OrgID: pgvalue.MustUUIDValue(locators.OrgID), ProjectID: pgvalue.MustUUIDValue(locators.ProjectID),
		EnvironmentID:  pgvalue.MustUUIDValue(locators.EnvironmentID),
		TaskDeclaredID: request.TaskDeclaredID, PayloadPresent: request.PayloadPresent,
		Payload: request.Payload, Workspace: workspace, IdempotencyKey: request.IdempotencyKey,
		QueueName: options.Queue, ConcurrencyKey: options.ConcurrencyKey, Priority: options.Priority,
		QueuedTTLMS: ttl, RetryPolicy: retry, Metadata: options.Metadata, Tags: options.Tags,
	})
}

func (s *Server) invokeChildTask(
	ctx context.Context,
	input childTaskInvokeInput,
) (childTaskInvokeResult, error) {
	var result childTaskInvokeResult
	err := s.inTx(ctx, func(work *txWork) error {
		locators, err := loadChildTaskInvokeLocators(
			ctx, work.q, input.Worker, input.Request.Lease, input.Parsed,
		)
		if err != nil {
			return err
		}
		environmentID := input.Normalized.EnvironmentID
		var claim *db.IdempotencyClaim
		var edgeClaim *db.IdempotencyClaim
		var replay *childTaskReceipt
		var invocationFingerprint idempotency.TaskChildInvokeFingerprint
		if input.Normalized.IdempotencyKey != "" {
			claims, err := idempotency.TransactionForQueries(work.q)
			if err != nil {
				return err
			}
			invocationFingerprint = idempotency.TaskChildInvokeFingerprint{
				Method: input.Request.Method, PayloadPresent: input.Normalized.PayloadPresent,
				Payload: input.Normalized.Payload, Workspace: input.Normalized.fingerprint.Workspace,
				QueueName: input.Normalized.QueueName, ConcurrencyKey: input.Normalized.ConcurrencyKey,
				Priority: input.Normalized.Priority, QueuedTTLMS: input.Normalized.QueuedTTLMS,
				RetryPolicy: input.Normalized.RetryPolicy, Metadata: input.Normalized.Metadata,
				Tags: input.Normalized.Tags,
			}
			request, err := idempotency.NewTaskChildInvokeRequest(
				environmentID,
				pgvalue.MustUUIDValue(locators.RunID),
				input.Normalized.TaskDeclaredID,
				input.Normalized.IdempotencyKey,
				invocationFingerprint,
			)
			if err != nil {
				return err
			}
			acquired, err := claims.Acquire(ctx, request)
			if err != nil {
				return err
			}
			if acquired.Claim.State == "completed" {
				edgeClaim = &acquired.Claim
				value, err := decodeChildTaskReceipt(acquired.Claim.Receipt)
				if err != nil {
					return err
				}
				replay = &value
			} else if acquired.Claim.State == "pending" {
				claim = &acquired.Claim
				edgeClaim = &acquired.Claim
			} else {
				return errTaskStartReceiptInvalid
			}
		}

		var targetWorkspaceID uuid.UUID
		if replay != nil {
			targetWorkspaceID = uuid.MustParse(replay.WorkspaceID)
		} else {
			var workspaceID pgtype.UUID
			if input.Normalized.Workspace.ID != nil {
				id, err := ids.Parse(*input.Normalized.Workspace.ID)
				if err != nil {
					return errTaskWorkspaceNotFound
				}
				workspaceID = pgvalue.UUID(id)
			}
			resolved, err := work.q.ResolveWorkspaceTarget(ctx, db.ResolveWorkspaceTargetParams{
				OrgID: pgvalue.UUID(input.Normalized.OrgID), ProjectID: pgvalue.UUID(input.Normalized.ProjectID),
				EnvironmentID: pgvalue.UUID(environmentID),
				ID:            workspaceID,
				Key:           pgvalue.TextPtr(input.Normalized.Workspace.Key),
			})
			if errors.Is(err, pgx.ErrNoRows) {
				return errTaskWorkspaceNotFound
			}
			if err != nil {
				return fmt.Errorf("resolve child task workspace: %w", err)
			}
			targetWorkspaceID = pgvalue.MustUUIDValue(resolved)
		}
		sameWorkspace := targetWorkspaceID == input.SourceWorkspaceID
		if sameWorkspace && input.Request.Method != "call" {
			return errChildTaskSameWorkspace
		}
		if sameWorkspace {
			authority, err := lockChildTaskInvokeRunAuthority(ctx, work.q, input, locators)
			if err == nil {
				err = completeChildTaskInvokeAuthority(
					ctx, work.q, input, locators, &authority, false,
				)
			}
			if err != nil || !childTaskInvokeScopeMatches(authority, input) {
				return errChildTaskInvokeStale
			}
			if edgeClaim == nil {
				return errors.New("same-workspace child task call claim is unavailable")
			}
			if _, err := loadChildTaskAdmission(
				ctx,
				work.q,
				authority.run,
				input.Normalized,
			); err != nil {
				return err
			}
			opened, err := registerSameWorkspaceChildCall(
				ctx,
				work.q,
				input,
				authority,
				*edgeClaim,
				invocationFingerprint,
				replay,
			)
			if err != nil {
				return err
			}
			result.openedWait = &opened
			if replay != nil {
				result.taskStartResult = taskStartResult{
					RunID: uuid.MustParse(replay.RunID), Replayed: true,
				}
			}
			return nil
		}

		authority, err := lockChildTaskInvokeRunAuthority(ctx, work.q, input, locators)
		if err != nil || !childTaskInvokeScopeMatches(authority, input) {
			return errChildTaskInvokeStale
		}
		if replay != nil {
			if err := completeChildTaskInvokeAuthority(
				ctx, work.q, input, locators, &authority, false,
			); err != nil {
				return err
			}
			result.taskStartResult = taskStartResult{
				RunID: uuid.MustParse(replay.RunID), Replayed: true,
			}
			if input.Request.Method == "call" {
				if replay.RunWaitID != input.RunWaitID.String() ||
					replay.ResumeAttachID != input.ResumeAttachID.String() {
					return errTaskStartReceiptInvalid
				}
				if edgeClaim == nil {
					return errTaskStartReceiptInvalid
				}
				opened, err := registerDifferentWorkspaceChildCall(
					ctx, work.q, input, authority, *edgeClaim, invocationFingerprint,
					result.taskStartResult, targetWorkspaceID,
				)
				if err != nil {
					return err
				}
				result.openedWait = &opened
			}
			return nil
		}

		definition, err := work.q.GetDeploymentDefinition(ctx, db.GetDeploymentDefinitionParams{
			EnvironmentID: authority.run.EnvironmentID, DeploymentID: authority.run.DeploymentID,
			Kind: "task", DeclaredID: input.Normalized.TaskDeclaredID,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return errTaskNotDeployed
		}
		if err != nil {
			return fmt.Errorf("load child task definition: %w", err)
		}
		program, err := work.q.GetDeploymentProgramAuthority(ctx, db.GetDeploymentProgramAuthorityParams{
			EnvironmentID: authority.run.EnvironmentID, DeploymentID: authority.run.DeploymentID,
		})
		if err != nil {
			return fmt.Errorf("load child task deployment authority: %w", err)
		}
		admission, err := deployment.ResolveTaskRunAdmission(
			definition.ManifestVersion, definition.DeclaredID, definition.Manifest,
			definition.ManifestDigest, program.QueueConfig, input.Normalized.QueueName,
			input.Normalized.QueuedTTLMS, input.Normalized.RetryPolicy,
		)
		if err != nil {
			return fmt.Errorf("%w: %v", errTaskStartAuthority, err)
		}
		if admission.HasPayload != input.Normalized.PayloadPresent {
			return errTaskPayloadPresenceInvalid
		}
		bindings, err := work.q.LockWorkspaceSecretsForAdmission(ctx, pgvalue.UUID(targetWorkspaceID))
		if err != nil {
			return fmt.Errorf("lock child task workspace secrets: %w", err)
		}
		for _, binding := range bindings {
			if binding.SecretState != "active" || !binding.CurrentVersionID.Valid {
				return errTaskSecretUnavailable
			}
		}
		lockedWorkspaces, err := work.q.LockChildWorkspacePair(
			ctx,
			db.LockChildWorkspacePairParams{
				EnvironmentID: pgvalue.UUID(environmentID),
				WorkspaceIds: []pgtype.UUID{
					pgvalue.UUID(input.SourceWorkspaceID),
					pgvalue.UUID(targetWorkspaceID),
				},
			},
		)
		if err != nil {
			return fmt.Errorf("lock child task workspace pair: %w", err)
		}
		sourceWorkspace, err := sourceChildWorkspace(
			lockedWorkspaces,
			input.SourceWorkspaceID,
			targetWorkspaceID,
			locators,
		)
		if err != nil {
			return err
		}
		authority.workspace = sourceWorkspace
		if err := completeChildTaskInvokeAuthority(
			ctx, work.q, input, locators, &authority, true,
		); err != nil {
			return err
		}
		workspace, err := work.q.LockWorkspaceAdmissionAuthority(ctx, db.LockWorkspaceAdmissionAuthorityParams{
			EnvironmentID: authority.run.EnvironmentID, ID: pgvalue.UUID(targetWorkspaceID),
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return errTaskWorkspaceUnavailable
		}
		if err != nil {
			return fmt.Errorf("lock child task workspace authority: %w", err)
		}
		if workspace.OrgID != authority.run.OrgID || workspace.ProjectID != authority.run.ProjectID ||
			workspace.State != db.WorkspaceStateActive ||
			(workspace.DesiredState != db.WorkspaceDesiredStateActive &&
				workspace.DesiredState != db.WorkspaceDesiredStateStopped) ||
			workspace.DirtyState != db.WorkspaceDirtyStateClean || !workspace.HeadVersionID.Valid ||
			workspace.OwnerActorID.Valid || workspace.OwnerRunID.Valid ||
			workspace.HasActiveLease || workspace.HasActiveProcess {
			return errTaskWorkspaceUnavailable
		}
		nowValue, err := work.q.GetRunAdmissionTime(ctx)
		if err != nil || !nowValue.Valid {
			return fmt.Errorf("load child task admission time: %w", err)
		}
		now := nowValue.Time.UTC()
		queuedExpiresAt := pgtype.Timestamptz{}
		if admission.QueuedTTLMS != nil {
			queuedExpiresAt = pgvalue.Timestamptz(now.Add(time.Duration(*admission.QueuedTTLMS) * time.Millisecond))
		}
		runID := uuid.Must(uuid.NewV7())
		rootSpanID, err := tracing.NewSpanID()
		if err != nil {
			return err
		}
		claimID := pgtype.UUID{}
		if claim != nil {
			claimID = claim.ID
		}
		parentOwnsLifecycle := input.Request.Method == "call"
		queueOriginAt := pgvalue.Timestamptz(now)
		if parentOwnsLifecycle {
			queueOriginAt = authority.run.QueueOriginAt
		}
		queueScoreAt := pgvalue.Timestamptz(
			queueOriginAt.Time.Add(-time.Duration(input.Normalized.Priority) * time.Second),
		)
		run, err := work.q.CreateChildRunFromParentDeployment(ctx, db.CreateChildRunFromParentDeploymentParams{
			EntrypointDeclaredID: input.Normalized.TaskDeclaredID,
			WorkspaceID:          pgvalue.UUID(targetWorkspaceID), BaseWorkspaceVersionID: workspace.HeadVersionID,
			ClaimID: claimID, EnvironmentID: authority.run.EnvironmentID, ParentRunID: authority.run.ID,
			ID:                  pgvalue.UUID(runID),
			ParentOwnsLifecycle: pgtype.Bool{Bool: parentOwnsLifecycle, Valid: true},
			Payload:             input.Normalized.Payload, Metadata: input.Normalized.Metadata, Tags: input.Normalized.Tags,
			QueueName: admission.QueueName, ConcurrencyKey: pgvalue.TextPtr(input.Normalized.ConcurrencyKey),
			QueueConcurrencyLimit: int8Ptr(admission.QueueConcurrencyLimit), Priority: input.Normalized.Priority,
			QueueOriginAt:   queueOriginAt,
			QueueScoreAt:    queueScoreAt,
			QueuedExpiresAt: queuedExpiresAt, MaxActiveDurationMs: admission.MaxActiveDurationMS,
			RetryPolicy: admission.RetryPolicy, TraceID: authority.run.TraceID, RootSpanID: rootSpanID,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return errChildTaskInvokeStale
		}
		if err != nil {
			return fmt.Errorf("create child task run: %w", err)
		}
		if _, err := work.q.ReserveWorkspaceForRun(ctx, db.ReserveWorkspaceForRunParams{
			RunID: run.ID, EnvironmentID: run.EnvironmentID, ID: workspace.ID,
			ExpectedStateVersion: workspace.StateVersion, ExpectedHeadVersionID: workspace.HeadVersionID,
		}); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return errTaskWorkspaceUnavailable
			}
			return fmt.Errorf("reserve child task workspace: %w", err)
		}
		if err := secret.CreateAttemptResolutions(
			ctx, work.q, workspace.ID, run.ID, 1, workspaceSecretResolutions(bindings),
		); err != nil {
			return fmt.Errorf("record child task secret resolutions: %w", err)
		}
		if _, err := work.q.CreateRunAdmissionOutbox(ctx, db.CreateRunAdmissionOutboxParams{
			ID: pgvalue.UUID(uuid.Must(uuid.NewV7())), WorkspaceID: workspace.ID,
			EnvironmentID: run.EnvironmentID, RunID: run.ID,
		}); err != nil {
			return fmt.Errorf("create child task admission outbox: %w", err)
		}
		result.taskStartResult = taskStartResult{RunID: runID}
		if input.Request.Method == "call" {
			if edgeClaim == nil {
				return errors.New("child task call claim is unavailable")
			}
			opened, err := registerDifferentWorkspaceChildCall(
				ctx, work.q, input, authority, *edgeClaim, invocationFingerprint,
				result.taskStartResult, targetWorkspaceID,
			)
			if err != nil {
				return err
			}
			result.openedWait = &opened
		}
		if claim != nil {
			receiptValue := childTaskReceipt{
				RunID: runID.String(), WorkspaceID: targetWorkspaceID.String(),
			}
			if input.Request.Method == "call" {
				receiptValue.RunWaitID = input.RunWaitID.String()
				receiptValue.ResumeAttachID = input.ResumeAttachID.String()
			}
			receipt, err := json.Marshal(receiptValue)
			if err != nil {
				return err
			}
			claims, err := idempotency.TransactionForQueries(work.q)
			if err != nil {
				return err
			}
			if _, err := claims.Complete(ctx, *claim, receipt); err != nil {
				return err
			}
		}
		return nil
	})
	return result, err
}

func loadChildTaskAdmission(
	ctx context.Context,
	store db.Querier,
	parent db.Run,
	request normalizedTaskStart,
) (deployment.TaskRunAdmission, error) {
	definition, err := store.GetDeploymentDefinition(ctx, db.GetDeploymentDefinitionParams{
		EnvironmentID: parent.EnvironmentID,
		DeploymentID:  parent.DeploymentID,
		Kind:          "task",
		DeclaredID:    request.TaskDeclaredID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return deployment.TaskRunAdmission{}, errTaskNotDeployed
	}
	if err != nil {
		return deployment.TaskRunAdmission{}, fmt.Errorf(
			"load child task definition: %w",
			err,
		)
	}
	program, err := store.GetDeploymentProgramAuthority(
		ctx,
		db.GetDeploymentProgramAuthorityParams{
			EnvironmentID: parent.EnvironmentID,
			DeploymentID:  parent.DeploymentID,
		},
	)
	if err != nil {
		return deployment.TaskRunAdmission{}, fmt.Errorf(
			"load child task deployment authority: %w",
			err,
		)
	}
	admission, err := deployment.ResolveTaskRunAdmission(
		definition.ManifestVersion,
		definition.DeclaredID,
		definition.Manifest,
		definition.ManifestDigest,
		program.QueueConfig,
		request.QueueName,
		request.QueuedTTLMS,
		request.RetryPolicy,
	)
	if err != nil {
		return deployment.TaskRunAdmission{}, fmt.Errorf(
			"%w: %v",
			errTaskStartAuthority,
			err,
		)
	}
	if admission.HasPayload != request.PayloadPresent {
		return deployment.TaskRunAdmission{}, errTaskPayloadPresenceInvalid
	}
	return admission, nil
}

func registerSameWorkspaceChildCall(
	ctx context.Context,
	store db.Querier,
	input childTaskInvokeInput,
	authority runLeaseClaimAuthority,
	claim db.IdempotencyClaim,
	fingerprint idempotency.TaskChildInvokeFingerprint,
	replay *childTaskReceipt,
) (workerapi.CreateRunWaitResponse, error) {
	requestFingerprint := fmt.Sprintf("sha256:%x", claim.RequestFingerprint)
	if replay != nil {
		return replayBoundSameWorkspaceChildCall(
			ctx,
			store,
			input,
			authority,
			claim,
			requestFingerprint,
			*replay,
		)
	}
	waitID := input.RunWaitID
	resumeAttachID := input.ResumeAttachID
	childRequest, err := json.Marshal(fingerprint)
	if err != nil {
		return workerapi.CreateRunWaitResponse{}, fmt.Errorf(
			"encode same-workspace child task call request: %w",
			err,
		)
	}
	response := workerapi.CreateRunWaitResponse{
		RunID:             pgvalue.UUIDString(authority.run.ID),
		RunWaitID:         waitID.String(),
		ResumeAttachID:    resumeAttachID.String(),
		RuntimeInstanceID: pgvalue.UUIDString(authority.runtime.ID),
		RuntimeEpoch:      input.Worker.WorkerEpoch,
		CheckpointDelayMs: 0,
	}
	replayed, err := store.GetSameWorkspaceChildCallReplay(
		ctx,
		db.GetSameWorkspaceChildCallReplayParams{
			EnvironmentID:                  authority.run.EnvironmentID,
			RunID:                          authority.run.ID,
			WorkspaceID:                    authority.run.WorkspaceID,
			AttemptNumber:                  authority.attempt.Number,
			ID:                             pgvalue.UUID(waitID),
			ChildTargetDeclaredID:          pgvalue.Text(input.Normalized.TaskDeclaredID),
			ChildClaimID:                   claim.ID,
			RegistrationRequestFingerprint: pgvalue.Text(requestFingerprint),
			ResumeAttachID:                 pgvalue.UUID(resumeAttachID),
			RunLeaseID:                     authority.runLease.ID,
		},
	)
	if err == nil {
		if err := validateRunWaitActorCursor(authority, replayed); err != nil {
			return workerapi.CreateRunWaitResponse{}, err
		}
		if replayed.SuspensionState == db.RunWaitStateReleased {
			if replayed.ConditionState != db.WaitStateCompleted ||
				replayed.ConditionResult == nil {
				return workerapi.CreateRunWaitResponse{}, errChildTaskInvokeStale
			}
			response.ResolutionKind = "completed"
			response.Resolution = append(
				json.RawMessage(nil),
				replayed.ConditionResult...,
			)
		}
		return response, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return workerapi.CreateRunWaitResponse{}, fmt.Errorf(
			"load same-workspace child task call replay: %w",
			err,
		)
	}
	actorCursor := pgtype.Int8{}
	if input.Request.ActorSpeculativeInputSequence != nil {
		actorCursor = pgtype.Int8{
			Int64: *input.Request.ActorSpeculativeInputSequence,
			Valid: true,
		}
	}
	registered, err := store.RegisterSameWorkspaceChildCall(
		ctx,
		db.RegisterSameWorkspaceChildCallParams{
			ID:                             pgvalue.UUID(waitID),
			ChildTargetDeclaredID:          pgvalue.Text(input.Normalized.TaskDeclaredID),
			ChildClaimID:                   claim.ID,
			ChildRequest:                   childRequest,
			RegistrationRequestFingerprint: pgvalue.Text(requestFingerprint),
			AttemptNumber:                  authority.attempt.Number,
			ActorSpeculativeInputSequence:  actorCursor,
			CurrentRunLeaseID:              authority.runLease.ID,
			ResumeAttachID:                 pgvalue.UUID(resumeAttachID),
			RunID:                          authority.run.ID,
			EnvironmentID:                  authority.run.EnvironmentID,
			WorkspaceID:                    authority.run.WorkspaceID,
			ExpectedRunningStateVersion:    authority.run.StateVersion,
		},
	)
	if err != nil {
		return workerapi.CreateRunWaitResponse{}, staleChildTaskInvoke(err)
	}
	if err := validateRunWaitActorCursor(authority, registered); err != nil {
		return workerapi.CreateRunWaitResponse{}, err
	}
	return response, nil
}

func replayBoundSameWorkspaceChildCall(
	ctx context.Context,
	store db.Querier,
	input childTaskInvokeInput,
	authority runLeaseClaimAuthority,
	claim db.IdempotencyClaim,
	requestFingerprint string,
	receipt childTaskReceipt,
) (workerapi.CreateRunWaitResponse, error) {
	if receipt.RunWaitID == "" ||
		receipt.ResumeAttachID == "" ||
		receipt.BaseWorkspaceVersionID == "" ||
		receipt.BaseWorkspaceDigest == "" {
		return workerapi.CreateRunWaitResponse{}, errWorkspaceHandoffConflict
	}
	waitID := uuid.MustParse(receipt.RunWaitID)
	resumeAttachID := uuid.MustParse(receipt.ResumeAttachID)
	if waitID != input.RunWaitID || resumeAttachID != input.ResumeAttachID {
		return workerapi.CreateRunWaitResponse{}, errWorkspaceHandoffConflict
	}
	childRunID := uuid.MustParse(receipt.RunID)
	baseID := uuid.MustParse(receipt.BaseWorkspaceVersionID)
	replayed, err := store.GetBoundSameWorkspaceChildCallReplay(
		ctx,
		db.GetBoundSameWorkspaceChildCallReplayParams{
			EnvironmentID:                  authority.run.EnvironmentID,
			RunID:                          authority.run.ID,
			WorkspaceID:                    authority.run.WorkspaceID,
			ID:                             pgvalue.UUID(waitID),
			ChildTargetDeclaredID:          pgvalue.Text(input.Normalized.TaskDeclaredID),
			ChildClaimID:                   claim.ID,
			RegistrationRequestFingerprint: pgvalue.Text(requestFingerprint),
			ChildRunID:                     pgvalue.UUID(childRunID),
			BaseWorkspaceVersionID:         pgvalue.UUID(baseID),
			BaseWorkspaceContentDigest:     pgvalue.Text(receipt.BaseWorkspaceDigest),
		},
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return workerapi.CreateRunWaitResponse{}, errWorkspaceHandoffConflict
	}
	if err != nil {
		return workerapi.CreateRunWaitResponse{}, fmt.Errorf(
			"load bound same-workspace child task call: %w",
			err,
		)
	}
	if replayed.SuspensionState != db.RunWaitStateReleased ||
		replayed.ConditionState != db.WaitStateCompleted ||
		replayed.ConditionResult == nil ||
		!replayed.ResumeWorkspaceVersionID.Valid {
		return workerapi.CreateRunWaitResponse{}, errWorkspaceHandoffConflict
	}
	return workerapi.CreateRunWaitResponse{
		RunID:             pgvalue.UUIDString(authority.run.ID),
		RunWaitID:         waitID.String(),
		ResumeAttachID:    resumeAttachID.String(),
		RuntimeInstanceID: pgvalue.UUIDString(authority.runtime.ID),
		RuntimeEpoch:      input.Worker.WorkerEpoch,
		ResolutionKind:    "completed",
		Resolution: append(
			json.RawMessage(nil),
			replayed.ConditionResult...,
		),
	}, nil
}

func loadChildTaskInvokeLocators(
	ctx context.Context,
	q db.Querier,
	worker workerActor,
	lease workerapi.RunLeaseFence,
	parsed parsedRunLeaseFence,
) (db.GetLiveRunLeaseLocatorsRow, error) {
	locators, err := q.GetLiveRunLeaseLocators(ctx, db.GetLiveRunLeaseLocatorsParams{
		ID: pgvalue.UUID(parsed.leaseID), LeaseSequence: lease.LeaseSequence,
		WorkerGroupID: worker.WorkerGroupID, WorkerInstanceID: pgvalue.UUID(worker.WorkerInstanceID),
		WorkerEpoch: worker.WorkerEpoch, WorkerProtocolVersion: worker.ProtocolVersion,
	})
	if err != nil {
		return db.GetLiveRunLeaseLocatorsRow{}, errChildTaskInvokeStale
	}
	return locators, nil
}

func registerDifferentWorkspaceChildCall(
	ctx context.Context,
	store db.Querier,
	input childTaskInvokeInput,
	authority runLeaseClaimAuthority,
	claim db.IdempotencyClaim,
	fingerprint idempotency.TaskChildInvokeFingerprint,
	child taskStartResult,
	childWorkspaceID uuid.UUID,
) (workerapi.CreateRunWaitResponse, error) {
	waitID := input.RunWaitID
	resumeAttachID := input.ResumeAttachID
	requestFingerprint := fmt.Sprintf("sha256:%x", claim.RequestFingerprint)
	childRequest, err := json.Marshal(fingerprint)
	if err != nil {
		return workerapi.CreateRunWaitResponse{}, fmt.Errorf("encode child task call request: %w", err)
	}
	response := workerapi.CreateRunWaitResponse{
		RunID: pgvalue.UUIDString(authority.run.ID), RunWaitID: waitID.String(),
		ResumeAttachID:    resumeAttachID.String(),
		RuntimeInstanceID: pgvalue.UUIDString(authority.runtime.ID),
		RuntimeEpoch:      input.Worker.WorkerEpoch,
		CheckpointDelayMs: 0,
	}
	replayed, err := store.GetChildCallRunWaitReplay(ctx, db.GetChildCallRunWaitReplayParams{
		EnvironmentID: authority.run.EnvironmentID, RunID: authority.run.ID,
		AttemptNumber: authority.attempt.Number, ID: pgvalue.UUID(waitID),
		ChildRunID: pgvalue.UUID(child.RunID), ChildClaimID: claim.ID,
		RegistrationRequestFingerprint: pgvalue.Text(requestFingerprint),
		ResumeAttachID:                 pgvalue.UUID(resumeAttachID),
	})
	if err == nil {
		if err := validateRunWaitActorCursor(authority, replayed); err != nil {
			return workerapi.CreateRunWaitResponse{}, err
		}
		if replayed.SuspensionState == db.RunWaitStateReleased {
			if replayed.ConditionState != db.WaitStateCompleted || replayed.ConditionResult == nil {
				return workerapi.CreateRunWaitResponse{}, errChildTaskInvokeStale
			}
			response.ResolutionKind = "completed"
			response.Resolution = append(json.RawMessage(nil), replayed.ConditionResult...)
		}
		return response, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return workerapi.CreateRunWaitResponse{}, fmt.Errorf("load child task call replay: %w", err)
	}
	childRun, err := store.GetRun(ctx, db.GetRunParams{
		EnvironmentID: authority.run.EnvironmentID, ID: pgvalue.UUID(child.RunID),
	})
	if err != nil || childRun.ParentRunID != authority.run.ID ||
		!childRun.ParentOwnsLifecycle.Valid || !childRun.ParentOwnsLifecycle.Bool ||
		childRun.WorkspaceID != pgvalue.UUID(childWorkspaceID) ||
		childRun.WorkspaceID == authority.run.WorkspaceID ||
		childRun.ClaimID != claim.ID {
		return workerapi.CreateRunWaitResponse{}, staleChildTaskInvoke(err)
	}
	actorCursor := pgtype.Int8{}
	if input.Request.ActorSpeculativeInputSequence != nil {
		actorCursor = pgtype.Int8{
			Int64: *input.Request.ActorSpeculativeInputSequence, Valid: true,
		}
	}
	params := db.RegisterDifferentWorkspaceChildCallParams{
		RunID: authority.run.ID, EnvironmentID: authority.run.EnvironmentID,
		ExpectedRunningStateVersion: authority.run.StateVersion,
		AttemptNumber:               authority.attempt.Number,
		CurrentRunLeaseID:           authority.runLease.ID,
		ChildWorkspaceID:            pgvalue.UUID(childWorkspaceID), ID: pgvalue.UUID(waitID),
		ChildRunID: pgvalue.UUID(child.RunID), ChildTargetDeclaredID: pgvalue.Text(input.Normalized.TaskDeclaredID),
		ChildClaimID: claim.ID, ChildRequest: childRequest,
		RegistrationRequestFingerprint: pgvalue.Text(requestFingerprint),
		ActorSpeculativeInputSequence:  actorCursor, ResumeAttachID: pgvalue.UUID(resumeAttachID),
	}
	if childRun.Status == db.RunStatusSucceeded || childRun.Status == db.RunStatusFailed ||
		childRun.Status == db.RunStatusCancelled || childRun.Status == db.RunStatusExpired ||
		childRun.Status == db.RunStatusSystemFailed {
		resolution, err := childTaskResult(childRun)
		if err != nil {
			return workerapi.CreateRunWaitResponse{}, err
		}
		resolved, err := store.RegisterResolvedDifferentWorkspaceChildCall(
			ctx,
			db.RegisterResolvedDifferentWorkspaceChildCallParams{
				ID: params.ID, EnvironmentID: params.EnvironmentID, RunID: params.RunID,
				ChildRunID: params.ChildRunID, ChildTargetDeclaredID: params.ChildTargetDeclaredID,
				ChildClaimID: params.ChildClaimID, ChildRequest: params.ChildRequest,
				ConditionResult:                resolution,
				RegistrationRequestFingerprint: params.RegistrationRequestFingerprint,
				ExpectedRunningStateVersion:    params.ExpectedRunningStateVersion,
				AttemptNumber:                  params.AttemptNumber,
				ActorSpeculativeInputSequence:  params.ActorSpeculativeInputSequence,
				CurrentRunLeaseID:              params.CurrentRunLeaseID, ResumeAttachID: params.ResumeAttachID,
			},
		)
		if err != nil {
			return workerapi.CreateRunWaitResponse{}, staleChildTaskInvoke(err)
		}
		if err := validateRunWaitActorCursor(authority, resolved); err != nil {
			return workerapi.CreateRunWaitResponse{}, err
		}
		response.ResolutionKind = "completed"
		response.Resolution = resolution
		return response, nil
	}
	if childRun.Status != db.RunStatusQueued && childRun.Status != db.RunStatusRunning &&
		childRun.Status != db.RunStatusWaiting && childRun.Status != db.RunStatusRetryDelayed &&
		childRun.Status != db.RunStatusCancelRequested {
		return workerapi.CreateRunWaitResponse{}, errChildTaskInvokeStale
	}
	registered, err := store.RegisterDifferentWorkspaceChildCall(ctx, params)
	if err != nil {
		return workerapi.CreateRunWaitResponse{}, staleChildTaskInvoke(err)
	}
	if err := validateRunWaitActorCursor(authority, registered); err != nil {
		return workerapi.CreateRunWaitResponse{}, err
	}
	return response, nil
}

func childTaskResult(run db.Run) (json.RawMessage, error) {
	runID := pgvalue.UUIDString(run.ID)
	if err := ids.Validate(runID); err != nil {
		return nil, err
	}
	if run.Status == db.RunStatusSucceeded {
		if run.Output == nil || !json.Valid(run.Output) {
			return nil, errors.New("succeeded child task has invalid output")
		}
		return json.Marshal(struct {
			OK     bool            `json:"ok"`
			Output json.RawMessage `json:"output"`
			Run    struct {
				ID string `json:"id"`
			} `json:"run"`
		}{OK: true, Output: run.Output, Run: struct {
			ID string `json:"id"`
		}{ID: runID}})
	}
	if !run.TerminalReasonCode.Valid {
		return nil, errors.New("failed child task has no terminal reason")
	}
	runError, err := projectRunError(run.TerminalReasonCode.String, run.Error)
	if err != nil {
		return nil, err
	}
	return json.Marshal(struct {
		OK    bool                 `json:"ok"`
		Error api.RunErrorResponse `json:"error"`
		Run   struct {
			ID string `json:"id"`
		} `json:"run"`
	}{OK: false, Error: runError, Run: struct {
		ID string `json:"id"`
	}{ID: runID}})
}

func staleChildTaskInvoke(err error) error {
	if err == nil || errors.Is(err, pgx.ErrNoRows) {
		return errChildTaskInvokeStale
	}
	return err
}

func lockChildTaskInvokeRunAuthority(
	ctx context.Context,
	q db.Querier,
	input childTaskInvokeInput,
	locators db.GetLiveRunLeaseLocatorsRow,
) (runLeaseClaimAuthority, error) {
	owner, err := lockRunFinalizationOwner(ctx, q, locators)
	if err != nil {
		return runLeaseClaimAuthority{}, errChildTaskInvokeStale
	}
	authority := runLeaseClaimAuthority{actor: owner.actor, parentRun: owner.parent}
	authority.run, err = q.LockRunLeaseClaimRun(ctx, db.LockRunLeaseClaimRunParams{
		ID: locators.RunID, OrgID: locators.OrgID, ProjectID: locators.ProjectID,
		EnvironmentID: locators.EnvironmentID, WorkspaceID: locators.WorkspaceID,
	})
	if err != nil {
		return runLeaseClaimAuthority{}, errChildTaskInvokeStale
	}
	allowedStatuses := []db.RunStatus{db.RunStatusRunning}
	if input.Request.Method == "call" {
		allowedStatuses = append(allowedStatuses, db.RunStatusWaiting)
	}
	if validateLockedRunLeaseRun(
		authority.run,
		pgvalue.UUID(input.Parsed.leaseID),
		locators,
		allowedStatuses...,
	) != nil ||
		validateRunFinalizationOwner(authority, locators) != nil {
		return runLeaseClaimAuthority{}, errChildTaskInvokeStale
	}
	return authority, nil
}

func completeChildTaskInvokeAuthority(
	ctx context.Context,
	q db.Querier,
	input childTaskInvokeInput,
	locators db.GetLiveRunLeaseLocatorsRow,
	authority *runLeaseClaimAuthority,
	workspaceLocked bool,
) error {
	if !workspaceLocked {
		if err := lockRunLeaseWorkspace(ctx, q, authority, locators); err != nil {
			return errChildTaskInvokeStale
		}
	} else if authority.workspace.ID != locators.WorkspaceID ||
		authority.workspace.EnvironmentID != locators.EnvironmentID ||
		authority.workspace.RegionID != locators.RegionID ||
		authority.workspace.State != db.WorkspaceStateActive ||
		authority.workspace.DesiredState != db.WorkspaceDesiredStateActive {
		return errChildTaskInvokeStale
	}
	if err := lockRunLeaseAttempt(ctx, q, authority, locators); err != nil {
		return errChildTaskInvokeStale
	}
	if err := lockRunLeasePhysicalAuthority(
		ctx,
		q,
		input.Worker,
		pgvalue.UUID(input.Parsed.leaseID),
		input.Request.Lease.LeaseSequence,
		locators,
		authority,
	); err != nil {
		return errChildTaskInvokeStale
	}
	if (authority.run.Status != db.RunStatusRunning &&
		(input.Request.Method != "call" || authority.run.Status != db.RunStatusWaiting)) ||
		authority.runLease.State != db.RunLeaseStateRunning ||
		!authority.run.ActiveStartedAt.Valid || !authority.attempt.EntrypointEnteredAt.Valid ||
		authority.attempt.TerminalAt.Valid || authority.runLease.FinalizationOperationID.Valid {
		return errChildTaskInvokeStale
	}
	return nil
}

func childTaskInvokeScopeMatches(
	authority runLeaseClaimAuthority,
	input childTaskInvokeInput,
) bool {
	return authority.run.OrgID == pgvalue.UUID(input.Normalized.OrgID) &&
		authority.run.ProjectID == pgvalue.UUID(input.Normalized.ProjectID) &&
		authority.run.EnvironmentID == pgvalue.UUID(input.Normalized.EnvironmentID) &&
		authority.run.WorkspaceID == pgvalue.UUID(input.SourceWorkspaceID)
}

func sourceChildWorkspace(
	workspaces []db.Workspace,
	sourceID uuid.UUID,
	targetID uuid.UUID,
	locators db.GetLiveRunLeaseLocatorsRow,
) (db.Workspace, error) {
	var sourceFound, targetFound bool
	var source db.Workspace
	for _, workspace := range workspaces {
		if workspace.EnvironmentID != locators.EnvironmentID {
			return db.Workspace{}, errTaskWorkspaceUnavailable
		}
		switch pgvalue.MustUUIDValue(workspace.ID) {
		case sourceID:
			source = workspace
			sourceFound = true
		case targetID:
			targetFound = true
		default:
			return db.Workspace{}, errTaskWorkspaceUnavailable
		}
	}
	if !sourceFound {
		return db.Workspace{}, errChildTaskInvokeStale
	}
	if !targetFound || len(workspaces) != 2 {
		return db.Workspace{}, errTaskWorkspaceUnavailable
	}
	return source, nil
}

func decodeChildTaskReceipt(raw []byte) (childTaskReceipt, error) {
	var receipt childTaskReceipt
	if err := decodeClosedJSON(raw, &receipt); err != nil {
		return childTaskReceipt{}, errTaskStartReceiptInvalid
	}
	if _, err := ids.Parse(receipt.RunID); err != nil {
		return childTaskReceipt{}, errTaskStartReceiptInvalid
	}
	if _, err := ids.Parse(receipt.WorkspaceID); err != nil {
		return childTaskReceipt{}, errTaskStartReceiptInvalid
	}
	hasWait := receipt.RunWaitID != "" || receipt.ResumeAttachID != ""
	if hasWait {
		_, waitErr := ids.Parse(receipt.RunWaitID)
		_, resumeAttachErr := ids.Parse(receipt.ResumeAttachID)
		if waitErr != nil || resumeAttachErr != nil {
			return childTaskReceipt{}, errTaskStartReceiptInvalid
		}
	}
	hasHandoff := receipt.BaseWorkspaceVersionID != "" || receipt.BaseWorkspaceDigest != ""
	if hasHandoff {
		_, baseErr := ids.Parse(receipt.BaseWorkspaceVersionID)
		if !hasWait || baseErr != nil ||
			!taskWorkspaceDigestPattern.MatchString(receipt.BaseWorkspaceDigest) {
			return childTaskReceipt{}, errTaskStartReceiptInvalid
		}
	}
	return receipt, nil
}

func (s *Server) writeChildTaskInvokeError(
	w http.ResponseWriter,
	correlationID string,
	method string,
	err error,
) {
	var idempotencyConflict idempotency.ConflictError
	var failure workerapi.RuntimeOperationFailure
	switch {
	case errors.As(err, &idempotencyConflict):
		failure = workerapi.RuntimeOperationFailure{
			Code: "idempotency_conflict", Message: "idempotency key conflicts with an earlier child Task invocation",
		}
	case errors.Is(err, errChildTaskSameWorkspace):
		failure = workerapi.RuntimeOperationFailure{
			Code: "same_workspace_" + method + "_unsupported", Message: err.Error(),
		}
	case errors.Is(err, errWorkspaceHandoffConflict):
		failure = workerapi.RuntimeOperationFailure{
			Code: "workspace_handoff_conflict", Message: err.Error(),
		}
	case errors.Is(err, errTaskNotDeployed):
		failure = workerapi.RuntimeOperationFailure{Code: "task_not_deployed", Message: err.Error()}
	case errors.Is(err, errTaskWorkspaceNotFound):
		failure = workerapi.RuntimeOperationFailure{Code: "workspace_not_found", Message: err.Error()}
	case errors.Is(err, errTaskWorkspaceUnavailable):
		failure = workerapi.RuntimeOperationFailure{Code: "workspace_unavailable", Message: err.Error(), Retryable: true}
	case errors.Is(err, errTaskSecretUnavailable):
		failure = workerapi.RuntimeOperationFailure{Code: "secret_unavailable", Message: err.Error()}
	case errors.Is(err, errTaskPayloadPresenceInvalid), errors.Is(err, errTaskStartInvalid):
		failure = workerapi.RuntimeOperationFailure{Code: "invalid_child_task_invoke", Message: err.Error()}
	case errors.Is(err, errChildTaskInvokeStale):
		writeError(w, conflict(errChildTaskInvokeStale))
		return
	default:
		s.log.Error("invoke child Task", "error", err)
		writeError(w, unavailable(codedError{
			code:    "child_task_invoke_authority_unavailable",
			message: "child task invocation authority is unavailable", retryable: true,
		}))
		return
	}
	writeJSON(w, http.StatusOK, workerapi.InvokeChildTaskResponse{
		CorrelationID: correlationID, Failed: &failure,
	})
}
