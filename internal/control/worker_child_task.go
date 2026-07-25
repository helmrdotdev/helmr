package control

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
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
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/publicid"
	"github.com/helmrdotdev/helmr/internal/tracing"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

var (
	errChildTaskInvokeStale       = errors.New("child Task invocation authority is stale")
	errChildTaskInvokeUnsupported = errors.New("child Task invocation method is unsupported")
	errChildTaskSameWorkspace     = errors.New("same-Workspace child Task start is unsupported")
	errWorkspaceHandoffConflict   = errors.New("same-Workspace call reached a different Workspace frontier")
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
	RunPublicID            string `json:"runPublicId"`
	WorkspaceID            string `json:"workspaceId"`
	RunWaitID              string `json:"runWaitId,omitempty"`
	BaseWorkspaceVersionID string `json:"baseWorkspaceVersionId,omitempty"`
	BaseWorkspaceDigest    string `json:"baseWorkspaceDigest,omitempty"`
}

type childTaskInvokeInput struct {
	Request           api.WorkerInvokeChildTaskRequest
	Parsed            parsedRunLeaseReceipt
	Worker            workerActor
	SourceWorkspaceID uuid.UUID
	Normalized        normalizedTaskStart
}

type childTaskInvokeResult struct {
	taskStartResult
	openedWait *api.WorkerCreateRunWaitResponse
}

func (s *Server) workerInvokeChildTask(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, unavailable(errors.New("run storage is not configured")))
		return
	}
	var request api.WorkerInvokeChildTaskRequest
	if err := decodeClosedWorkerRequest(r, &request); err != nil {
		writeError(w, badRequest(codedError{code: "invalid_child_task_start", message: err.Error()}))
		return
	}
	parsed, err := parseRunLeaseReceipt(request.Lease)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	if _, err := parseCanonicalUUID("correlation_id", request.CorrelationID); err != nil {
		writeError(w, badRequest(err))
		return
	}
	if request.Method != "start" && request.Method != "call" {
		writeError(w, badRequest(codedError{
			code: "child_task_method_unsupported", message: errChildTaskInvokeUnsupported.Error(),
		}))
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
	if request.Lease.WorkerGroupID != worker.WorkerGroupID ||
		parsed.workerInstanceID != worker.WorkerInstanceID {
		writeError(w, forbidden(errors.New("child Task invocation belongs to another worker")))
		return
	}
	if request.Lease.WorkerEpoch != worker.WorkerEpoch ||
		request.Lease.WorkerProtocolVersion != worker.ProtocolVersion {
		writeError(w, conflict(errChildTaskInvokeStale))
		return
	}
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
	})
	if err != nil {
		s.writeChildTaskInvokeError(w, request.CorrelationID, request.Method, err)
		return
	}
	response := api.WorkerInvokeChildTaskResponse{
		CorrelationID: request.CorrelationID,
		OpenedWait:    result.openedWait,
	}
	if result.RunPublicID != "" {
		response.Completed = &api.WorkerChildTaskStartResult{RunID: result.RunPublicID}
	}
	writeJSON(w, http.StatusOK, response)
}

func normalizeWorkerChildTaskRequest(
	request api.WorkerInvokeChildTaskRequest,
	locators db.GetLiveRunLeaseLocatorsRow,
) (normalizedTaskStart, error) {
	if err := api.ValidateTaskID(request.TaskDeclaredID); err != nil {
		return normalizedTaskStart{}, err
	}
	var workspace api.WorkspaceTarget
	if err := decodeClosedJSON(request.Workspace, &workspace); err != nil {
		return normalizedTaskStart{}, fmt.Errorf("invalid Workspace: %w", err)
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
		environmentID := input.Normalized.EnvironmentID
		var claim *db.IdempotencyClaim
		var edgeClaim *db.IdempotencyClaim
		var replay *childTaskReceipt
		var invocationFingerprint idempotency.TaskChildInvokeFingerprint
		if input.Normalized.IdempotencyKey != "" {
			claims, err := s.claims.TransactionForQueries(work.q)
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
				input.Parsed.runID,
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
			resolved, err := work.q.ResolveWorkspaceTarget(ctx, db.ResolveWorkspaceTargetParams{
				OrgID: pgvalue.UUID(input.Normalized.OrgID), ProjectID: pgvalue.UUID(input.Normalized.ProjectID),
				EnvironmentID: pgvalue.UUID(environmentID),
				PublicID:      pgvalue.TextPtr(input.Normalized.Workspace.ID),
				Key:           pgvalue.TextPtr(input.Normalized.Workspace.Key),
			})
			if errors.Is(err, pgx.ErrNoRows) {
				return errTaskWorkspaceNotFound
			}
			if err != nil {
				return fmt.Errorf("resolve child Task Workspace: %w", err)
			}
			targetWorkspaceID = pgvalue.MustUUIDValue(resolved)
		}
		sameWorkspace := targetWorkspaceID == input.SourceWorkspaceID
		if sameWorkspace && input.Request.Method != "call" {
			return errChildTaskSameWorkspace
		}
		if !sameWorkspace {
			if err := work.q.LockChildWorkspacePair(
				ctx, childWorkspacePairLock(input.SourceWorkspaceID, targetWorkspaceID),
			); err != nil {
				return fmt.Errorf("lock child Task Workspace pair: %w", err)
			}
		}
		authority, err := lockChildTaskInvokeAuthority(ctx, work.q, input)
		if err != nil {
			return err
		}
		if authority.run.OrgID != pgvalue.UUID(input.Normalized.OrgID) ||
			authority.run.ProjectID != pgvalue.UUID(input.Normalized.ProjectID) ||
			authority.run.EnvironmentID != pgvalue.UUID(input.Normalized.EnvironmentID) ||
			authority.run.WorkspaceID != pgvalue.UUID(input.SourceWorkspaceID) {
			return errChildTaskInvokeStale
		}
		if sameWorkspace {
			if edgeClaim == nil {
				return errors.New("same-Workspace child Task call claim is unavailable")
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
					RunID:       uuid.MustParse(replay.RunID),
					RunPublicID: replay.RunPublicID,
					Replayed:    true,
				}
			}
			return nil
		}
		if replay != nil {
			result.taskStartResult = taskStartResult{
				RunID: uuid.MustParse(replay.RunID), RunPublicID: replay.RunPublicID, Replayed: true,
			}
			if input.Request.Method == "call" {
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
			return fmt.Errorf("load child Task definition: %w", err)
		}
		program, err := work.q.GetDeploymentProgramAuthority(ctx, db.GetDeploymentProgramAuthorityParams{
			EnvironmentID: authority.run.EnvironmentID, DeploymentID: authority.run.DeploymentID,
		})
		if err != nil {
			return fmt.Errorf("load child Task deployment authority: %w", err)
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
			return fmt.Errorf("lock child Task Workspace Secrets: %w", err)
		}
		for _, binding := range bindings {
			if binding.SecretState != "active" || !binding.CurrentVersionID.Valid {
				return errTaskSecretUnavailable
			}
		}
		workspace, err := work.q.LockWorkspaceAdmissionAuthority(ctx, db.LockWorkspaceAdmissionAuthorityParams{
			EnvironmentID: authority.run.EnvironmentID, ID: pgvalue.UUID(targetWorkspaceID),
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return errTaskWorkspaceUnavailable
		}
		if err != nil {
			return fmt.Errorf("lock child Task Workspace authority: %w", err)
		}
		if workspace.OrgID != authority.run.OrgID || workspace.ProjectID != authority.run.ProjectID ||
			workspace.State != db.WorkspaceStateActive ||
			(workspace.DesiredState != db.WorkspaceDesiredStateActive &&
				workspace.DesiredState != db.WorkspaceDesiredStateStopped) ||
			workspace.DirtyState != db.WorkspaceDirtyStateClean || !workspace.HeadVersionID.Valid ||
			workspace.OwnerActorID.Valid || workspace.OwnerRunID.Valid ||
			workspace.HasActiveLease || workspace.HasActiveProcess ||
			!program.ProgramArchitecture.Valid || !workspace.WorkspaceArchitecture.Valid ||
			program.ProgramArchitecture.String != workspace.WorkspaceArchitecture.String {
			return errTaskWorkspaceUnavailable
		}
		nowValue, err := work.q.GetRunAdmissionTime(ctx)
		if err != nil || !nowValue.Valid {
			return fmt.Errorf("load child Task admission time: %w", err)
		}
		now := nowValue.Time.UTC()
		queuedExpiresAt := pgtype.Timestamptz{}
		if admission.QueuedTTLMS != nil {
			queuedExpiresAt = pgvalue.Timestamptz(now.Add(time.Duration(*admission.QueuedTTLMS) * time.Millisecond))
		}
		runID := uuid.Must(uuid.NewV7())
		runPublicID, err := publicid.New(publicid.Run)
		if err != nil {
			return err
		}
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
			ID: pgvalue.UUID(runID), PublicID: runPublicID,
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
			return fmt.Errorf("create child Task Run: %w", err)
		}
		if _, err := work.q.ReserveWorkspaceForRun(ctx, db.ReserveWorkspaceForRunParams{
			RunID: run.ID, EnvironmentID: run.EnvironmentID, ID: workspace.ID,
			ExpectedStateVersion: workspace.StateVersion, ExpectedHeadVersionID: workspace.HeadVersionID,
		}); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return errTaskWorkspaceUnavailable
			}
			return fmt.Errorf("reserve child Task Workspace: %w", err)
		}
		for _, binding := range bindings {
			if _, err := work.q.CreateSecretResolution(ctx, db.CreateSecretResolutionParams{
				ID: pgvalue.UUID(uuid.Must(uuid.NewV7())), WorkspaceID: workspace.ID, RunID: run.ID,
				AttemptNumber: pgtype.Int4{Int32: 1, Valid: true}, PlacementKind: binding.PlacementKind,
				PlacementTarget: binding.PlacementTarget, SecretID: binding.SecretID,
				SecretVersionID: binding.CurrentVersionID, RevocationGeneration: binding.RevocationGeneration,
			}); err != nil {
				return fmt.Errorf("record child Task Secret resolution: %w", err)
			}
		}
		if _, err := work.q.CreateRunAdmissionOutbox(ctx, db.CreateRunAdmissionOutboxParams{
			ID: pgvalue.UUID(uuid.Must(uuid.NewV7())), WorkspaceID: workspace.ID,
			EnvironmentID: run.EnvironmentID, RunID: run.ID,
		}); err != nil {
			return fmt.Errorf("create child Task admission outbox: %w", err)
		}
		result.taskStartResult = taskStartResult{RunID: runID, RunPublicID: runPublicID}
		if input.Request.Method == "call" {
			if edgeClaim == nil {
				return errors.New("child Task call claim is unavailable")
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
			receipt, err := json.Marshal(childTaskReceipt{
				RunID: runID.String(), RunPublicID: runPublicID, WorkspaceID: targetWorkspaceID.String(),
			})
			if err != nil {
				return err
			}
			claims, err := s.claims.TransactionForQueries(work.q)
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
			"load child Task definition: %w",
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
			"load child Task deployment authority: %w",
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
) (api.WorkerCreateRunWaitResponse, error) {
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
	correlationID, err := uuid.Parse(input.Request.CorrelationID)
	if err != nil {
		return api.WorkerCreateRunWaitResponse{}, errChildTaskInvokeStale
	}
	waitID := derivedRunWaitID(
		input.Parsed.runID,
		input.Request.Lease.AttemptNumber,
		correlationID,
		"child-call",
	)
	resumeAttachID := derivedRunWaitID(
		input.Parsed.runID,
		input.Request.Lease.AttemptNumber,
		correlationID,
		"resume-attach",
	)
	childRequest, err := json.Marshal(fingerprint)
	if err != nil {
		return api.WorkerCreateRunWaitResponse{}, fmt.Errorf(
			"encode same-Workspace child Task call request: %w",
			err,
		)
	}
	response := api.WorkerCreateRunWaitResponse{
		RunID:             input.Request.Lease.RunID,
		RunWaitID:         waitID.String(),
		ResumeAttachID:    resumeAttachID.String(),
		RuntimeInstanceID: input.Request.Lease.RuntimeInstanceID,
		RuntimeEpoch:      input.Request.Lease.WorkerEpoch,
		CheckpointDelayMs: 0,
	}
	replayed, err := store.GetSameWorkspaceChildCallReplay(
		ctx,
		db.GetSameWorkspaceChildCallReplayParams{
			EnvironmentID:                  authority.run.EnvironmentID,
			RunID:                          authority.run.ID,
			WorkspaceID:                    authority.run.WorkspaceID,
			AttemptNumber:                  input.Request.Lease.AttemptNumber,
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
			return api.WorkerCreateRunWaitResponse{}, err
		}
		if replayed.SuspensionState == db.RunWaitStateReleased {
			if replayed.ConditionState != db.WaitStateCompleted ||
				replayed.ConditionResult == nil {
				return api.WorkerCreateRunWaitResponse{}, errChildTaskInvokeStale
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
		return api.WorkerCreateRunWaitResponse{}, fmt.Errorf(
			"load same-Workspace child Task call replay: %w",
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
			AttemptNumber:                  input.Request.Lease.AttemptNumber,
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
		return api.WorkerCreateRunWaitResponse{}, staleChildTaskInvoke(err)
	}
	if err := validateRunWaitActorCursor(authority, registered); err != nil {
		return api.WorkerCreateRunWaitResponse{}, err
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
) (api.WorkerCreateRunWaitResponse, error) {
	if receipt.RunWaitID == "" ||
		receipt.BaseWorkspaceVersionID == "" ||
		receipt.BaseWorkspaceDigest == "" {
		return api.WorkerCreateRunWaitResponse{}, errWorkspaceHandoffConflict
	}
	waitID := uuid.MustParse(receipt.RunWaitID)
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
		return api.WorkerCreateRunWaitResponse{}, errWorkspaceHandoffConflict
	}
	if err != nil {
		return api.WorkerCreateRunWaitResponse{}, fmt.Errorf(
			"load bound same-Workspace child Task call: %w",
			err,
		)
	}
	if replayed.SuspensionState != db.RunWaitStateReleased ||
		replayed.ConditionState != db.WaitStateCompleted ||
		replayed.ConditionResult == nil ||
		!replayed.ResumeWorkspaceVersionID.Valid {
		return api.WorkerCreateRunWaitResponse{}, errWorkspaceHandoffConflict
	}
	return api.WorkerCreateRunWaitResponse{
		RunID:             input.Request.Lease.RunID,
		RunWaitID:         waitID.String(),
		ResumeAttachID:    pgvalue.MustUUIDValue(replayed.ResumeAttachID).String(),
		RuntimeInstanceID: input.Request.Lease.RuntimeInstanceID,
		RuntimeEpoch:      input.Request.Lease.WorkerEpoch,
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
	lease api.WorkerRunLeaseReceipt,
	parsed parsedRunLeaseReceipt,
) (db.GetLiveRunLeaseLocatorsRow, error) {
	locators, err := q.GetLiveRunLeaseLocators(ctx, db.GetLiveRunLeaseLocatorsParams{
		ID: pgvalue.UUID(parsed.leaseID), LeaseSequence: lease.LeaseSequence,
		WorkerGroupID: worker.WorkerGroupID, WorkerInstanceID: pgvalue.UUID(worker.WorkerInstanceID),
		WorkerEpoch: worker.WorkerEpoch, WorkerProtocolVersion: worker.ProtocolVersion,
	})
	if err != nil || locators.RunID != pgvalue.UUID(parsed.runID) {
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
) (api.WorkerCreateRunWaitResponse, error) {
	correlationID, err := uuid.Parse(input.Request.CorrelationID)
	if err != nil {
		return api.WorkerCreateRunWaitResponse{}, errChildTaskInvokeStale
	}
	waitID := derivedRunWaitID(
		input.Parsed.runID, input.Request.Lease.AttemptNumber, correlationID, "child-call",
	)
	resumeAttachID := derivedRunWaitID(
		input.Parsed.runID, input.Request.Lease.AttemptNumber, correlationID, "resume-attach",
	)
	requestFingerprint := fmt.Sprintf("sha256:%x", claim.RequestFingerprint)
	childRequest, err := json.Marshal(fingerprint)
	if err != nil {
		return api.WorkerCreateRunWaitResponse{}, fmt.Errorf("encode child Task call request: %w", err)
	}
	response := api.WorkerCreateRunWaitResponse{
		RunID: input.Request.Lease.RunID, RunWaitID: waitID.String(),
		ResumeAttachID:    resumeAttachID.String(),
		RuntimeInstanceID: input.Request.Lease.RuntimeInstanceID,
		RuntimeEpoch:      input.Request.Lease.WorkerEpoch,
		CheckpointDelayMs: 0,
	}
	replayed, err := store.GetChildCallRunWaitReplay(ctx, db.GetChildCallRunWaitReplayParams{
		EnvironmentID: authority.run.EnvironmentID, RunID: authority.run.ID,
		AttemptNumber: input.Request.Lease.AttemptNumber, ID: pgvalue.UUID(waitID),
		ChildRunID: pgvalue.UUID(child.RunID), ChildClaimID: claim.ID,
		RegistrationRequestFingerprint: pgvalue.Text(requestFingerprint),
		ResumeAttachID:                 pgvalue.UUID(resumeAttachID),
	})
	if err == nil {
		if err := validateRunWaitActorCursor(authority, replayed); err != nil {
			return api.WorkerCreateRunWaitResponse{}, err
		}
		if replayed.SuspensionState == db.RunWaitStateReleased {
			if replayed.ConditionState != db.WaitStateCompleted || replayed.ConditionResult == nil {
				return api.WorkerCreateRunWaitResponse{}, errChildTaskInvokeStale
			}
			response.ResolutionKind = "completed"
			response.Resolution = append(json.RawMessage(nil), replayed.ConditionResult...)
		}
		return response, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return api.WorkerCreateRunWaitResponse{}, fmt.Errorf("load child Task call replay: %w", err)
	}
	childRun, err := store.GetRun(ctx, db.GetRunParams{
		EnvironmentID: authority.run.EnvironmentID, ID: pgvalue.UUID(child.RunID),
	})
	if err != nil || childRun.ParentRunID != authority.run.ID ||
		!childRun.ParentOwnsLifecycle.Valid || !childRun.ParentOwnsLifecycle.Bool ||
		childRun.WorkspaceID != pgvalue.UUID(childWorkspaceID) ||
		childRun.WorkspaceID == authority.run.WorkspaceID ||
		childRun.ClaimID != claim.ID {
		return api.WorkerCreateRunWaitResponse{}, staleChildTaskInvoke(err)
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
		AttemptNumber:               input.Request.Lease.AttemptNumber,
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
			return api.WorkerCreateRunWaitResponse{}, err
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
			return api.WorkerCreateRunWaitResponse{}, staleChildTaskInvoke(err)
		}
		if err := validateRunWaitActorCursor(authority, resolved); err != nil {
			return api.WorkerCreateRunWaitResponse{}, err
		}
		response.ResolutionKind = "completed"
		response.Resolution = resolution
		return response, nil
	}
	if childRun.Status != db.RunStatusQueued && childRun.Status != db.RunStatusRunning &&
		childRun.Status != db.RunStatusWaiting && childRun.Status != db.RunStatusRetryDelayed &&
		childRun.Status != db.RunStatusCancelRequested {
		return api.WorkerCreateRunWaitResponse{}, errChildTaskInvokeStale
	}
	registered, err := store.RegisterDifferentWorkspaceChildCall(ctx, params)
	if err != nil {
		return api.WorkerCreateRunWaitResponse{}, staleChildTaskInvoke(err)
	}
	if err := validateRunWaitActorCursor(authority, registered); err != nil {
		return api.WorkerCreateRunWaitResponse{}, err
	}
	return response, nil
}

func childTaskResult(run db.Run) (json.RawMessage, error) {
	if err := publicid.ValidateFor(publicid.Run, run.PublicID); err != nil {
		return nil, err
	}
	if run.Status == db.RunStatusSucceeded {
		if run.Output == nil || !json.Valid(run.Output) {
			return nil, errors.New("succeeded child Task has invalid output")
		}
		return json.Marshal(struct {
			OK     bool            `json:"ok"`
			Output json.RawMessage `json:"output"`
			Run    struct {
				ID string `json:"id"`
			} `json:"run"`
		}{OK: true, Output: run.Output, Run: struct {
			ID string `json:"id"`
		}{ID: run.PublicID}})
	}
	if !run.TerminalReasonCode.Valid {
		return nil, errors.New("failed child Task has no terminal reason")
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
	}{ID: run.PublicID}})
}

func staleChildTaskInvoke(err error) error {
	if err == nil || errors.Is(err, pgx.ErrNoRows) {
		return errChildTaskInvokeStale
	}
	return err
}

func lockChildTaskInvokeAuthority(
	ctx context.Context,
	q db.Querier,
	input childTaskInvokeInput,
) (runLeaseClaimAuthority, error) {
	locators, err := loadChildTaskInvokeLocators(
		ctx, q, input.Worker, input.Request.Lease, input.Parsed,
	)
	if err != nil {
		return runLeaseClaimAuthority{}, err
	}
	owner, err := lockRunFinalizationOwner(ctx, q, locators)
	if err != nil {
		return runLeaseClaimAuthority{}, errChildTaskInvokeStale
	}
	var authority runLeaseClaimAuthority
	if input.Request.Method == "call" {
		authority, err = lockRenewableRunLeaseAuthority(
			ctx, q, input.Worker, pgvalue.UUID(input.Parsed.leaseID),
			input.Request.Lease.LeaseSequence, locators,
		)
	} else {
		authority, err = lockLiveRunLeaseAuthority(
			ctx, q, input.Worker, pgvalue.UUID(input.Parsed.leaseID),
			input.Request.Lease.LeaseSequence, locators,
		)
	}
	authority.actor = owner.actor
	authority.parentRun = owner.parent
	if err != nil ||
		validateRunFinalizationOwner(authority, locators) != nil ||
		authority.run.ID != pgvalue.UUID(input.Parsed.runID) ||
		(authority.run.Status != db.RunStatusRunning &&
			(input.Request.Method != "call" || authority.run.Status != db.RunStatusWaiting)) ||
		authority.runLease.State != db.RunLeaseStateRunning ||
		!authority.run.ActiveStartedAt.Valid || !authority.attempt.EntrypointEnteredAt.Valid ||
		authority.attempt.TerminalAt.Valid || authority.runLease.FinalizationOperationID.Valid {
		return runLeaseClaimAuthority{}, errChildTaskInvokeStale
	}
	current, err := projectActorTurnLease(authority)
	if err != nil || !equalRunLeaseReceipt(current, input.Request.Lease) {
		return runLeaseClaimAuthority{}, errChildTaskInvokeStale
	}
	return authority, nil
}

func decodeChildTaskReceipt(raw []byte) (childTaskReceipt, error) {
	var receipt childTaskReceipt
	if err := decodeClosedJSON(raw, &receipt); err != nil {
		return childTaskReceipt{}, errTaskStartReceiptInvalid
	}
	runID, err := uuid.Parse(receipt.RunID)
	if err != nil || runID == uuid.Nil || runID.String() != receipt.RunID {
		return childTaskReceipt{}, errTaskStartReceiptInvalid
	}
	workspaceID, err := uuid.Parse(receipt.WorkspaceID)
	if err != nil || workspaceID == uuid.Nil || workspaceID.String() != receipt.WorkspaceID {
		return childTaskReceipt{}, errTaskStartReceiptInvalid
	}
	if err := publicid.ValidateFor(publicid.Run, receipt.RunPublicID); err != nil {
		return childTaskReceipt{}, errTaskStartReceiptInvalid
	}
	hasHandoff := receipt.RunWaitID != "" ||
		receipt.BaseWorkspaceVersionID != "" ||
		receipt.BaseWorkspaceDigest != ""
	if hasHandoff {
		waitID, waitErr := uuid.Parse(receipt.RunWaitID)
		baseID, baseErr := uuid.Parse(receipt.BaseWorkspaceVersionID)
		if waitErr != nil || waitID == uuid.Nil ||
			waitID.String() != receipt.RunWaitID ||
			baseErr != nil || baseID == uuid.Nil ||
			baseID.String() != receipt.BaseWorkspaceVersionID ||
			!taskWorkspaceDigestPattern.MatchString(receipt.BaseWorkspaceDigest) {
			return childTaskReceipt{}, errTaskStartReceiptInvalid
		}
	}
	return receipt, nil
}

func childWorkspacePairLock(left, right uuid.UUID) int64 {
	if string(left[:]) > string(right[:]) {
		left, right = right, left
	}
	hash := sha256.New()
	hash.Write([]byte("helmr.child-workspace-pair.v1\x00"))
	hash.Write(left[:])
	hash.Write(right[:])
	return int64(binary.BigEndian.Uint64(hash.Sum(nil)[:8]))
}

func (s *Server) writeChildTaskInvokeError(
	w http.ResponseWriter,
	correlationID string,
	method string,
	err error,
) {
	var idempotencyConflict idempotency.ConflictError
	var failure api.WorkerRuntimeOperationFailure
	switch {
	case errors.As(err, &idempotencyConflict):
		failure = api.WorkerRuntimeOperationFailure{
			Code: "idempotency_conflict", Message: "idempotency key conflicts with an earlier child Task invocation",
		}
	case errors.Is(err, errChildTaskSameWorkspace):
		failure = api.WorkerRuntimeOperationFailure{
			Code: "same_workspace_" + method + "_unsupported", Message: err.Error(),
		}
	case errors.Is(err, errWorkspaceHandoffConflict):
		failure = api.WorkerRuntimeOperationFailure{
			Code: "workspace_handoff_conflict", Message: err.Error(),
		}
	case errors.Is(err, errTaskNotDeployed):
		failure = api.WorkerRuntimeOperationFailure{Code: "task_not_deployed", Message: err.Error()}
	case errors.Is(err, errTaskWorkspaceNotFound):
		failure = api.WorkerRuntimeOperationFailure{Code: "workspace_not_found", Message: err.Error()}
	case errors.Is(err, errTaskWorkspaceUnavailable):
		failure = api.WorkerRuntimeOperationFailure{Code: "workspace_unavailable", Message: err.Error(), Retryable: true}
	case errors.Is(err, errTaskSecretUnavailable):
		failure = api.WorkerRuntimeOperationFailure{Code: "secret_unavailable", Message: err.Error()}
	case errors.Is(err, errTaskPayloadPresenceInvalid), errors.Is(err, errTaskStartInvalid):
		failure = api.WorkerRuntimeOperationFailure{Code: "invalid_child_task_invoke", Message: err.Error()}
	case errors.Is(err, errChildTaskInvokeStale):
		writeError(w, conflict(errChildTaskInvokeStale))
		return
	default:
		s.log.Error("invoke child Task", "error", err)
		writeError(w, unavailable(codedError{
			code:    "child_task_invoke_authority_unavailable",
			message: "child Task invocation authority is unavailable", retryable: true,
		}))
		return
	}
	writeJSON(w, http.StatusOK, api.WorkerInvokeChildTaskResponse{
		CorrelationID: correlationID, Failed: &failure,
	})
}
