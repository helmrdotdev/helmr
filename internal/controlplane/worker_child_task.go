package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/definition"
	"github.com/helmrdotdev/helmr/internal/idempotency"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/helmrdotdev/helmr/internal/tracing"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

var (
	errChildTaskInvokeUnsupported = errors.New("child task invocation method is unsupported")
)

type childTaskInvokeFailurePoint string

const (
	childTaskInvokePointLoadLease   childTaskInvokeFailurePoint = "load_live_run_lease"
	childTaskInvokePointSourceScope childTaskInvokeFailurePoint = "source_scope"
	childTaskInvokePointTransaction childTaskInvokeFailurePoint = "transaction_authority"
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
	RunID      string `json:"runId"`
	ComputerID string `json:"computerId"`
}

type childTaskInvokeInput struct {
	turnID           pgtype.UUID
	runGeneration    pgtype.Int8
	Request          workerapi.InvokeChildTaskRequest
	Parsed           parsedRunLeaseFence
	Worker           workergroup.HostPrincipal
	SourceComputerID uuid.UUID
	Normalized       normalizedTaskStart
	RunWaitID        uuid.UUID
	ResumeAttachID   uuid.UUID
}

type childTaskInvokeResult struct {
	taskStartResult
	openedWait *workerapi.CreateRunWaitResponse
}

func (s *Server) workerInvokeChildTask(w http.ResponseWriter, r *http.Request) {
	var request workerapi.InvokeChildTaskRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		if isRequestBodyTooLarge(err) {
			writeError(w, err)
			return
		}
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
		if !errors.Is(err, errChildTaskInvokeStale) {
			s.writeChildTaskInvokeError(w, request.CorrelationID, request.Method, err)
			return
		}
		stale := staleAuthority(staleAuthorityChildTask, childTaskInvokePointLoadLease, errChildTaskInvokeStale)
		s.log.Warn(
			"reject stale child Task invocation",
			"failure_point", childTaskInvokePointLoadLease,
			"run_lease_id", request.Lease.ID,
			"lease_sequence", request.Lease.LeaseSequence,
			"worker_host_id", worker.HostID,
		)
		writeError(w, conflict(stale))
		return
	}
	normalized, err := normalizeWorkerChildTaskRequest(request, locators)
	if err != nil {
		writeError(w, badRequest(codedError{code: "invalid_child_task_start", message: err.Error()}))
		return
	}
	result, err := s.invokeChildTask(r.Context(), childTaskInvokeInput{
		Request: request, Parsed: parsed, Worker: worker,
		SourceComputerID: pgvalue.MustUUIDValue(locators.ComputerID),
		Normalized:       normalized,
		RunWaitID:        runWaitID,
		ResumeAttachID:   resumeAttachID,
	})
	if err != nil {
		s.writeChildTaskInvokeError(w, request.CorrelationID, request.Method, err)
		return
	}
	response := workerapi.InvokeChildTaskResponse{
		CorrelationID: request.CorrelationID,
		OpenedWait:    result.openedWait,
	}
	if result.RunID != uuid.Nil() {
		response.Completed = &workerapi.ChildTaskStartResult{RunID: result.RunID.String()}
	}
	writeJSON(w, http.StatusOK, response)
}

func normalizeWorkerChildTaskRequest(
	request workerapi.InvokeChildTaskRequest,
	locators db.GetLiveRunLeaseLocatorsRow,
) (normalizedTaskStart, error) {
	if err := api.ValidateDefinitionID(request.TaskDeclaredID); err != nil {
		return normalizedTaskStart{}, err
	}
	var computer api.ComputerIDTarget
	if err := decodeClosedJSON(request.Computer, &computer); err != nil {
		return normalizedTaskStart{}, fmt.Errorf("invalid computer: %w", err)
	}
	computerID, err := ids.Parse(computer.ID)
	if err != nil {
		return normalizedTaskStart{}, fmt.Errorf("invalid computer: %w", err)
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
		Payload: request.Payload, ComputerID: computerID, IdempotencyKey: request.IdempotencyKey,
		QueueName: options.Queue, ConcurrencyKey: options.ConcurrencyKey, Priority: options.Priority,
		QueuedTTLMS: ttl, RetryPolicy: retry, Metadata: options.Metadata, Tags: options.Tags,
	})
}

func (s *Server) invokeChildTask(
	ctx context.Context,
	input childTaskInvokeInput,
) (childTaskInvokeResult, error) {
	var parseErr error
	input.turnID, input.runGeneration, parseErr = parseWorkerWaitTurn(input.Request.TurnID, input.Request.RunGeneration)
	if parseErr != nil {
		return childTaskInvokeResult{}, parseErr
	}

	var result childTaskInvokeResult
	err := s.inTx(ctx, func(work *txWork) (operationErr error) {
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
			claims, err := idempotency.TransactionFor(work.tx)
			if err != nil {
				return err
			}
			invocationFingerprint = idempotency.TaskChildInvokeFingerprint{
				Method: input.Request.Method, PayloadPresent: input.Normalized.PayloadPresent,
				Payload: input.Normalized.Payload, Computer: input.Normalized.fingerprint.Computer,
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
			if acquired.Claim.Status == "completed" {
				edgeClaim = &acquired.Claim
				value, err := decodeChildTaskReceipt(acquired.Claim.Receipt)
				if err != nil {
					return err
				}
				replay = &value
			} else if acquired.Claim.Status == "pending" {
				claim = &acquired.Claim
				edgeClaim = &acquired.Claim
			} else {
				return errTaskStartReceiptInvalid
			}
		}

		var targetComputerID uuid.UUID
		if replay != nil {
			targetComputerID = uuid.MustParse(replay.ComputerID)
		} else {
			targetComputerID = input.Normalized.ComputerID
		}
		// This also precedes source authority on same-Computer and replay paths.
		bindings, err := work.q.LockComputerSecretsForAdmission(ctx, pgvalue.UUID(targetComputerID))
		if err != nil {
			return err
		}
		if err := authorizeComputerSecretTarget(ctx, work.q, pgvalue.UUID(input.SourceComputerID), pgvalue.UUID(targetComputerID)); err != nil {
			return err
		}
		authority, err := run.LockLiveExecutionForComputer(ctx, work.tx, workerExecutionFence(input.Worker, input.Parsed, input.Request.Lease), pgvalue.UUID(targetComputerID))
		if errors.Is(err, run.ErrExecutionTargetNotFound) {
			return errTaskComputerNotFound
		}
		if err != nil {
			return staleChildTaskInvoke(err)
		}
		if (authority.Run.Status != db.RunStatusRunning && (input.Request.Method != "call" || authority.Run.Status != db.RunStatusWaiting)) ||
			!authority.Run.ActiveStartedAt.Valid || !authority.Attempt.EntrypointEnteredAt.Valid || authority.Lease.FinalizationOperationID.Valid {
			return errChildTaskInvokeStale
		}
		defer func() {
			if operationErr != nil {
				return
			}
			_, err := run.LockLiveExecution(ctx, work.tx, workerExecutionFence(input.Worker, input.Parsed, input.Request.Lease))
			if err != nil {
				operationErr = staleChildTaskInvoke(err)
			}
		}()
		cursor := input.Request.ActorSpeculativeInputSequence
		if authority.Run.EntrypointKind == "actor" {
			want := authority.Session.CommittedInputSequence
			if authority.Session.ActiveTurnID.Valid {
				want++
			}
			if cursor == nil || *cursor != want {
				return errChildTaskInvokeStale
			}
		} else if cursor != nil {
			return errChildTaskInvokeStale
		}
		if err := validateWorkerWaitTurn(ctx, work.q, authority, input.turnID, input.runGeneration); err != nil {
			return err
		}
		if !childTaskInvokeScopeMatches(authority, input) {
			return staleAuthority(staleAuthorityChildTask, childTaskInvokePointSourceScope, errChildTaskInvokeStale)
		}
		if replay != nil {
			result.taskStartResult = taskStartResult{
				RunID: uuid.MustParse(replay.RunID), Replayed: true,
			}
			if input.Request.Method == "call" {
				if edgeClaim == nil {
					return errTaskStartReceiptInvalid
				}
				opened, err := registerChildCall(
					ctx, work.q, input.callRegistration(), authority, *edgeClaim, invocationFingerprint,
					result.RunID, targetComputerID,
				)
				if err != nil {
					return err
				}
				if err := bindOrCheckChildWaitTurn(ctx, work.q, authority, input.callRegistration()); err != nil {
					return err
				}
				result.openedWait = &opened
			}
			return nil
		}

		admission, err := loadChildTaskAdmission(ctx, work.q, authority.Run, input.Normalized)
		if err != nil {
			return err
		}
		for _, binding := range bindings {
			if binding.SecretStatus != "active" || !binding.CurrentVersionID.Valid {
				return errTaskSecretUnavailable
			}
		}
		computer, err := work.q.LockComputerAdmissionAuthority(ctx, db.LockComputerAdmissionAuthorityParams{
			EnvironmentID: authority.Run.EnvironmentID, ID: pgvalue.UUID(targetComputerID),
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return errTaskComputerUnavailable
		}
		if err != nil {
			return fmt.Errorf("lock child task computer authority: %w", err)
		}
		if computer.OrgID != authority.Run.OrgID || computer.ProjectID != authority.Run.ProjectID ||
			computer.Status != db.ComputerStatusActive ||
			(computer.DesiredState != db.ComputerDesiredStateActive &&
				computer.DesiredState != db.ComputerDesiredStateStopped) ||
			computer.DirtyState == db.ComputerDirtyStateCaptureFailed || computer.DirtyState == db.ComputerDirtyStateDirtyStateLost || !computer.HeadDiskVersionID.Valid || len(computer.PreparationFailure) > 0 || len(computer.RecoveryFailure) > 0 {
			return errTaskComputerUnavailable
		}
		compatible, err := computerCanAdmitProgram(ctx, work.q, computer.EnvironmentID, computer.ID, computer.ComputerSpecID, authority.Run.DeploymentID)
		if err != nil {
			return err
		}
		if !compatible {
			return errTaskComputerUnavailable
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
		runID := uuid.NewV7()
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
			queueOriginAt = authority.Run.QueueOriginAt
		}
		queueScoreAt := pgvalue.Timestamptz(
			queueOriginAt.Time.Add(-time.Duration(input.Normalized.Priority) * time.Second),
		)
		run, err := work.q.CreateChildRunFromParentDeployment(ctx, db.CreateChildRunFromParentDeploymentParams{
			EntrypointDeclaredID: input.Normalized.TaskDeclaredID,
			ComputerID:           pgvalue.UUID(targetComputerID), BaseComputerDiskVersionID: computer.HeadDiskVersionID,
			ClaimID: claimID, EnvironmentID: authority.Run.EnvironmentID, ParentRunID: authority.Run.ID,
			ID:                  pgvalue.UUID(runID),
			ParentOwnsLifecycle: pgtype.Bool{Bool: parentOwnsLifecycle, Valid: true},
			Payload:             input.Normalized.Payload, Metadata: input.Normalized.Metadata, Tags: input.Normalized.Tags,
			QueueName: admission.QueueName, ConcurrencyKey: pgvalue.TextPtr(input.Normalized.ConcurrencyKey),
			QueueConcurrencyLimit: int8Ptr(admission.QueueConcurrencyLimit), Priority: input.Normalized.Priority,
			QueueOriginAt:   queueOriginAt,
			QueueScoreAt:    queueScoreAt,
			QueuedExpiresAt: queuedExpiresAt, MaxActiveDurationMs: admission.MaxActiveDurationMS,
			RetryPolicy: admission.RetryPolicy, TraceID: authority.Run.TraceID, RootSpanID: rootSpanID,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return errChildTaskInvokeStale
		}
		if err != nil {
			return fmt.Errorf("create child task run: %w", err)
		}
		if err := secret.CreateAttemptResolutions(
			ctx, work.q, computer.ID, run.ID, 1, computerSecretResolutions(bindings),
		); err != nil {
			return fmt.Errorf("record child task secret resolutions: %w", err)
		}
		result.taskStartResult = taskStartResult{RunID: runID}
		if input.Request.Method == "call" {
			if edgeClaim == nil {
				return errors.New("child task call claim is unavailable")
			}
			opened, err := registerChildCall(
				ctx, work.q, input.callRegistration(), authority, *edgeClaim, invocationFingerprint,
				result.RunID, targetComputerID,
			)
			if err != nil {
				return err
			}
			result.openedWait = &opened
		}
		if claim != nil {
			receiptValue := childTaskReceipt{
				RunID: runID.String(), ComputerID: targetComputerID.String(),
			}

			receipt, err := json.Marshal(receiptValue)
			if err != nil {
				return err
			}
			claims, err := idempotency.TransactionFor(work.tx)
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
) (definition.TaskRunAdmission, error) {
	deploymentDefinition, err := store.GetDeploymentDefinition(ctx, db.GetDeploymentDefinitionParams{
		EnvironmentID: parent.EnvironmentID,
		DeploymentID:  parent.DeploymentID,
		Kind:          "task",
		DeclaredID:    request.TaskDeclaredID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return definition.TaskRunAdmission{}, errTaskNotDeployed
	}
	if err != nil {
		return definition.TaskRunAdmission{}, fmt.Errorf(
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
		return definition.TaskRunAdmission{}, fmt.Errorf(
			"load child task deployment authority: %w",
			err,
		)
	}
	admission, err := definition.ResolveTaskRunAdmission(
		deploymentDefinition.ManifestVersion,
		deploymentDefinition.DeclaredID,
		deploymentDefinition.Manifest,
		deploymentDefinition.ManifestDigest,
		program.QueueConfig,
		request.QueueName,
		request.QueuedTTLMS,
		request.RetryPolicy,
	)
	if err != nil {
		return definition.TaskRunAdmission{}, fmt.Errorf(
			"%w: %v",
			errTaskStartAuthority,
			err,
		)
	}
	if admission.HasPayload != request.PayloadPresent {
		return definition.TaskRunAdmission{}, errTaskPayloadPresenceInvalid
	}
	return admission, nil
}

func loadChildTaskInvokeLocators(
	ctx context.Context,
	q db.Querier,
	worker workergroup.HostPrincipal,
	lease workerapi.RunLeaseFence,
	parsed parsedRunLeaseFence,
) (db.GetLiveRunLeaseLocatorsRow, error) {
	locators, err := q.GetLiveRunLeaseLocators(ctx, db.GetLiveRunLeaseLocatorsParams{
		ID: pgvalue.UUID(parsed.leaseID), LeaseSequence: lease.LeaseSequence,
		WorkerGroupID: pgvalue.UUID(worker.GroupID), WorkerHostID: pgvalue.UUID(worker.HostID),
		WorkerEpoch: worker.Epoch})
	if err != nil {
		return db.GetLiveRunLeaseLocatorsRow{}, staleChildTaskInvoke(err)
	}
	return locators, nil
}

func childTaskInvokeScopeMatches(
	authority run.ExecutionAuthority,
	input childTaskInvokeInput,
) bool {
	return authority.Run.OrgID == pgvalue.UUID(input.Normalized.OrgID) &&
		authority.Run.ProjectID == pgvalue.UUID(input.Normalized.ProjectID) &&
		authority.Run.EnvironmentID == pgvalue.UUID(input.Normalized.EnvironmentID) &&
		authority.Run.ComputerID == pgvalue.UUID(input.SourceComputerID)
}

func decodeChildTaskReceipt(raw []byte) (childTaskReceipt, error) {
	var receipt childTaskReceipt
	if err := decodeClosedJSON(raw, &receipt); err != nil {
		return childTaskReceipt{}, errTaskStartReceiptInvalid
	}
	if _, err := ids.Parse(receipt.RunID); err != nil {
		return childTaskReceipt{}, errTaskStartReceiptInvalid
	}
	if _, err := ids.Parse(receipt.ComputerID); err != nil {
		return childTaskReceipt{}, errTaskStartReceiptInvalid
	}

	return receipt, nil
}

func (s *Server) writeChildTaskInvokeError(
	w http.ResponseWriter,
	correlationID string,
	method string,
	err error,
) {
	if writeStaleWorkerClaims(w, err) {
		return
	}
	if failure, ok := actorOutputAppendFailure(err); ok {
		writeJSON(w, http.StatusOK, workerapi.InvokeChildTaskResponse{CorrelationID: correlationID, Failed: &failure})
		return
	}
	var expired idempotency.ExpiredError
	if errors.As(err, &expired) {
		writeError(w, gone(expired))
		return
	}
	var idempotencyConflict idempotency.ConflictError
	var failure workerapi.RuntimeOperationFailure
	switch {
	case errors.As(err, &idempotencyConflict):
		failure = workerapi.RuntimeOperationFailure{
			Code: "idempotency_conflict", Message: "idempotency key conflicts with an earlier child Task invocation",
		}
	case errors.Is(err, errTaskNotDeployed):
		failure = workerapi.RuntimeOperationFailure{Code: "task_not_deployed", Message: err.Error()}
	case errors.Is(err, errTaskComputerNotFound):
		failure = workerapi.RuntimeOperationFailure{Code: "computer_not_found", Message: err.Error()}
	case errors.Is(err, errTaskComputerUnavailable):
		failure = workerapi.RuntimeOperationFailure{Code: "computer_unavailable", Message: err.Error(), Retryable: true}
	case errors.Is(err, errTaskSecretUnavailable), errors.Is(err, errComputerSecretUnavailable):
		failure = workerapi.RuntimeOperationFailure{Code: "secret_unavailable", Message: err.Error()}
	case errors.Is(err, errTaskPayloadPresenceInvalid), errors.Is(err, errTaskStartInvalid):
		failure = workerapi.RuntimeOperationFailure{Code: "invalid_child_task_invoke", Message: err.Error()}
	case errors.Is(err, errChildTaskInvokeStale):
		err = staleAuthority(staleAuthorityChildTask, childTaskInvokePointTransaction, err)
		point, _ := staleAuthorityPointOf(err)
		s.log.Warn(
			"reject stale child Task invocation",
			"failure_point", point,
			"correlation_id", correlationID,
		)
		writeError(w, conflict(err))
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

func (input childTaskInvokeInput) callRegistration() childCallRegistration {
	return childCallRegistration{RunWaitID: input.RunWaitID, ResumeAttachID: input.ResumeAttachID, TurnID: input.turnID, RunGeneration: input.runGeneration, TaskDeclaredID: input.Normalized.TaskDeclaredID}
}
