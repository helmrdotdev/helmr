package run

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/definition"
	"github.com/helmrdotdev/helmr/internal/idempotency"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/jsoncanon"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
	"github.com/helmrdotdev/helmr/internal/tracing"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

var (
	// ErrChildInvokeStale reports a child Task invocation whose receipt no
	// longer addresses the live invoking execution, or whose execution, call
	// or replay no longer admits it.
	ErrChildInvokeStale = errors.New("child task invocation authority is stale")
	// ErrChildInvokeSourceScope reports an invoking execution that is not the
	// one the invocation was normalized for. It is ErrChildInvokeStale.
	ErrChildInvokeSourceScope = fmt.Errorf("%w: source scope", ErrChildInvokeStale)
)

// ChildInvokeLocators reads the unlocked locators of a live Run lease.
type ChildInvokeLocators interface {
	GetLiveRunLeaseLocators(context.Context, db.GetLiveRunLeaseLocatorsParams) (db.GetLiveRunLeaseLocatorsRow, error)
}

// LocateChildInvocation reads, without locking, the scope of the fenced live
// lease a child Task invocation comes from. A lease the fence does not
// address is ErrChildInvokeStale.
func LocateChildInvocation(ctx context.Context, store ChildInvokeLocators, fence ExecutionFence) (db.GetLiveRunLeaseLocatorsRow, error) {
	locators, err := store.GetLiveRunLeaseLocators(ctx, fence.liveLocators())
	if err != nil {
		return db.GetLiveRunLeaseLocatorsRow{}, staleChildInvoke(err)
	}
	return locators, nil
}

// ChildInvoke is a worker's request to start a child Task Run from its live
// execution: method "start" detaches the child, "call" makes the parent own
// its lifecycle and wait for it.
type ChildInvoke struct {
	Fence  ExecutionFence
	Method string
	// SourceComputerID is the invoking execution's Computer, as located
	// before the transaction.
	SourceComputerID uuid.UUID
	// Task is the normalized child start; its scope is the invoking
	// execution's.
	Task           TaskStart
	IdempotencyKey string
	// Fingerprint identifies the invocation for its idempotency claim and a
	// call's wait.
	Fingerprint idempotency.TaskChildInvokeFingerprint
	// Cursor is an Actor execution's speculative input sequence; a Task
	// execution sends none.
	Cursor        *int64
	TurnID        pgtype.UUID
	RunGeneration pgtype.Int8
	// RunWaitID and ResumeAttachID identify a call's wait.
	RunWaitID, ResumeAttachID uuid.UUID
	// ChildResult projects a terminal child Run's result for a call that
	// finds its child already finished.
	ChildResult func(db.Run) (json.RawMessage, error)
}

// ChildInvoked is the child Run an invocation started or replayed, and for a
// call the wait the parent registered.
type ChildInvoked struct {
	RunID    uuid.UUID
	Replayed bool
	Call     *ChildCall
}

// ChildCall is the wait a call registered on its parent execution.
type ChildCall struct {
	ParentRunID        pgtype.UUID
	RunWaitID          uuid.UUID
	ResumeAttachID     uuid.UUID
	ComputerInstanceID pgtype.UUID
	WorkerEpoch        int64
	// Completed reports a child that already finished; Resolution is then
	// its result.
	Completed  bool
	Resolution json.RawMessage
}

type childTaskReceipt struct {
	RunID      string `json:"runId"`
	ComputerID string `json:"computerId"`
}

// InvokeChild starts or replays a child Task Run in its own transaction. It
// locates the fenced lease and acquires the invocation's idempotency claim,
// then locks the target Computer's Secrets, checks that the source Computer
// may address the target's Secrets, and locks the live execution with the
// target Computer. After the execution, input cursor, Turn and source scope
// checks, a completed claim replays its receipt {"runId","computerId"} (a
// call re-registers its wait); a new invocation resolves the Task from the
// parent's deployment, locks the target Computer through the computer
// owner's admission lock, creates the child Run with its Secret resolutions,
// registers a call's wait and completes the claim. The live execution is
// locked again before commit.
func InvokeChild(ctx context.Context, txb db.TxBeginner, invoke ChildInvoke) (ChildInvoked, error) {
	var result ChildInvoked
	err := db.RunTx(ctx, txb, func(tx pgx.Tx) (operationErr error) {
		q := db.New(tx)
		locators, err := LocateChildInvocation(ctx, q, invoke.Fence)
		if err != nil {
			return err
		}
		environmentID := invoke.Task.EnvironmentID
		var claim *db.IdempotencyClaim
		var edgeClaim *db.IdempotencyClaim
		var replay *childTaskReceipt
		if invoke.IdempotencyKey != "" {
			claims, err := idempotency.TransactionFor(tx)
			if err != nil {
				return err
			}
			request, err := idempotency.NewTaskChildInvokeRequest(
				environmentID,
				pgvalue.MustUUIDValue(locators.RunID),
				invoke.Task.TaskDeclaredID,
				invoke.IdempotencyKey,
				invoke.Fingerprint,
			)
			if err != nil {
				return err
			}
			acquired, err := claims.Acquire(ctx, request)
			if err != nil {
				return err
			}
			switch acquired.Claim.Status {
			case "completed":
				edgeClaim = &acquired.Claim
				value, err := decodeChildTaskReceipt(acquired.Claim.Receipt)
				if err != nil {
					return err
				}
				replay = &value
			case "pending":
				claim = &acquired.Claim
				edgeClaim = &acquired.Claim
			default:
				return ErrTaskStartReceiptInvalid
			}
		}

		targetComputerID := invoke.Task.ComputerID
		if replay != nil {
			targetComputerID = uuid.MustParse(replay.ComputerID)
		}
		// This also precedes source authority on same-Computer and replay paths.
		bindings, err := q.LockComputerSecretsForAdmission(ctx, pgvalue.UUID(targetComputerID))
		if err != nil {
			return err
		}
		if err := computer.CheckSecretTarget(ctx, q, pgvalue.UUID(invoke.SourceComputerID), pgvalue.UUID(targetComputerID)); err != nil {
			return err
		}
		authority, err := LockLiveExecutionForComputer(ctx, tx, invoke.Fence, pgvalue.UUID(targetComputerID))
		if errors.Is(err, ErrExecutionTargetNotFound) {
			return ErrTaskComputerNotFound
		}
		if err != nil {
			return staleChildInvoke(err)
		}
		parent := authority.run
		if (parent.Status != db.RunStatusRunning && (invoke.Method != "call" || parent.Status != db.RunStatusWaiting)) ||
			!parent.ActiveStartedAt.Valid || !authority.attempt.EntrypointEnteredAt.Valid || authority.lease.FinalizationOperationID.Valid {
			return ErrChildInvokeStale
		}
		defer func() {
			if operationErr != nil {
				return
			}
			if _, err := LockLiveExecution(ctx, tx, invoke.Fence); err != nil {
				operationErr = staleChildInvoke(err)
			}
		}()
		if parent.EntrypointKind == "actor" {
			want := authority.session.CommittedInputSequence
			if authority.session.ActiveTurnID.Valid {
				want++
			}
			if invoke.Cursor == nil || *invoke.Cursor != want {
				return ErrChildInvokeStale
			}
		} else if invoke.Cursor != nil {
			return ErrChildInvokeStale
		}
		if err := authority.validateWaitTurn(ctx, tx, invoke.TurnID, invoke.RunGeneration); err != nil {
			return err
		}
		if parent.OrgID != pgvalue.UUID(invoke.Task.OrgID) ||
			parent.ProjectID != pgvalue.UUID(invoke.Task.ProjectID) ||
			parent.EnvironmentID != pgvalue.UUID(invoke.Task.EnvironmentID) ||
			parent.ComputerID != pgvalue.UUID(invoke.SourceComputerID) {
			return ErrChildInvokeSourceScope
		}
		if replay != nil {
			result = ChildInvoked{RunID: uuid.MustParse(replay.RunID), Replayed: true}
			if invoke.Method == "call" {
				if edgeClaim == nil {
					return ErrTaskStartReceiptInvalid
				}
				call, err := registerChildCall(ctx, q, invoke, authority, *edgeClaim, result.RunID, targetComputerID)
				if err != nil {
					return err
				}
				if err := bindOrCheckChildWaitTurn(ctx, q, authority, invoke); err != nil {
					return err
				}
				result.Call = &call
			}
			return nil
		}

		admission, err := loadChildTaskAdmission(ctx, q, parent, invoke.Task)
		if err != nil {
			return err
		}
		if !admissionSecretsAvailable(bindings) {
			return ErrTaskSecretUnavailable
		}
		locked, err := computer.LockForAdmission(ctx, tx, pgvalue.MustUUIDValue(parent.EnvironmentID), targetComputerID)
		if errors.Is(err, computer.ErrNotFound) {
			return ErrTaskComputerUnavailable
		}
		if err != nil {
			return err
		}
		admitted := locked.Row()
		if admitted.OrgID != parent.OrgID || admitted.ProjectID != parent.ProjectID ||
			admitted.Status != db.ComputerStatusActive ||
			(admitted.DesiredState != db.ComputerDesiredStateActive &&
				admitted.DesiredState != db.ComputerDesiredStateStopped) ||
			admitted.DirtyState == db.ComputerDirtyStateDirtyStateLost ||
			!admitted.HeadDiskVersionID.Valid || len(admitted.PreparationFailure) > 0 || len(admitted.RecoveryFailure) > 0 {
			return ErrTaskComputerUnavailable
		}
		compatible, err := computer.CanAdmitProgram(ctx, q, admitted.EnvironmentID, admitted.ID, admitted.ComputerSpecID, parent.DeploymentID)
		if err != nil {
			return err
		}
		if !compatible {
			return ErrTaskComputerUnavailable
		}
		nowValue, err := q.GetRunAdmissionTime(ctx)
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
		parentOwnsLifecycle := invoke.Method == "call"
		queueOriginAt := pgvalue.Timestamptz(now)
		if parentOwnsLifecycle {
			queueOriginAt = parent.QueueOriginAt
		}
		queueScoreAt := pgvalue.Timestamptz(
			queueOriginAt.Time.Add(-time.Duration(invoke.Task.Priority) * time.Second),
		)
		child, err := q.CreateChildRunFromParentDeployment(ctx, db.CreateChildRunFromParentDeploymentParams{
			EntrypointDeclaredID: invoke.Task.TaskDeclaredID,
			ComputerID:           pgvalue.UUID(targetComputerID), BaseComputerDiskVersionID: admitted.HeadDiskVersionID,
			ClaimID: claimID, EnvironmentID: parent.EnvironmentID, ParentRunID: parent.ID,
			ID:                  pgvalue.UUID(runID),
			ParentOwnsLifecycle: pgtype.Bool{Bool: parentOwnsLifecycle, Valid: true},
			Payload:             invoke.Task.Payload, Metadata: invoke.Task.Metadata, Tags: invoke.Task.Tags,
			QueueName: admission.QueueName, ConcurrencyKey: pgvalue.TextPtr(invoke.Task.ConcurrencyKey),
			QueueConcurrencyLimit: optionalInt8(admission.QueueConcurrencyLimit), Priority: invoke.Task.Priority,
			QueueOriginAt:   queueOriginAt,
			QueueScoreAt:    queueScoreAt,
			QueuedExpiresAt: queuedExpiresAt, MaxActiveDurationMs: admission.MaxActiveDurationMS,
			RetryPolicy: admission.RetryPolicy, TraceID: parent.TraceID, RootSpanID: rootSpanID,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrChildInvokeStale
		}
		if err != nil {
			return fmt.Errorf("create child task run: %w", err)
		}
		if err := secret.CreateAttemptResolutions(
			ctx, q, admitted.ID, child.ID, 1, SecretResolutions(bindings),
		); err != nil {
			return fmt.Errorf("record child task secret resolutions: %w", err)
		}
		result = ChildInvoked{RunID: runID}
		if invoke.Method == "call" {
			if edgeClaim == nil {
				return errors.New("child task call claim is unavailable")
			}
			call, err := registerChildCall(ctx, q, invoke, authority, *edgeClaim, runID, targetComputerID)
			if err != nil {
				return err
			}
			result.Call = &call
		}
		if claim == nil {
			return nil
		}
		receipt, err := json.Marshal(childTaskReceipt{RunID: runID.String(), ComputerID: targetComputerID.String()})
		if err != nil {
			return err
		}
		claims, err := idempotency.TransactionFor(tx)
		if err != nil {
			return err
		}
		_, err = claims.Complete(ctx, *claim, receipt)
		return err
	})
	if err != nil {
		return ChildInvoked{}, err
	}
	return result, nil
}

// loadChildTaskAdmission resolves the child Task's Run admission from the
// parent Run's deployment.
func loadChildTaskAdmission(ctx context.Context, q db.Querier, parent db.Run, task TaskStart) (definition.TaskRunAdmission, error) {
	deploymentDefinition, err := q.GetDeploymentDefinition(ctx, db.GetDeploymentDefinitionParams{
		EnvironmentID: parent.EnvironmentID,
		DeploymentID:  parent.DeploymentID,
		Kind:          "task",
		DeclaredID:    task.TaskDeclaredID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return definition.TaskRunAdmission{}, ErrTaskNotDeployed
	}
	if err != nil {
		return definition.TaskRunAdmission{}, fmt.Errorf("load child task definition: %w", err)
	}
	program, err := q.GetDeploymentProgramAuthority(ctx, db.GetDeploymentProgramAuthorityParams{
		EnvironmentID: parent.EnvironmentID,
		DeploymentID:  parent.DeploymentID,
	})
	if err != nil {
		return definition.TaskRunAdmission{}, fmt.Errorf("load child task deployment authority: %w", err)
	}
	admission, err := definition.ResolveTaskRunAdmission(
		deploymentDefinition.ManifestVersion,
		deploymentDefinition.DeclaredID,
		deploymentDefinition.Manifest,
		deploymentDefinition.ManifestDigest,
		program.QueueConfig,
		task.QueueName,
		task.QueuedTTLMS,
		task.RetryPolicy,
	)
	if err != nil {
		return definition.TaskRunAdmission{}, fmt.Errorf("%w: %v", ErrTaskStartAuthority, err)
	}
	if admission.HasPayload != task.PayloadPresent {
		return definition.TaskRunAdmission{}, ErrTaskPayloadPresenceInvalid
	}
	return admission, nil
}

// registerChildCall registers, or replays, a call's wait for its child Run on
// the locked parent execution. A child that already finished registers its
// wait resolved with the child's result.
func registerChildCall(
	ctx context.Context,
	q db.Querier,
	invoke ChildInvoke,
	authority Execution,
	claim db.IdempotencyClaim,
	childRunID uuid.UUID,
	childComputerID uuid.UUID,
) (ChildCall, error) {
	parent, attempt := authority.run, authority.attempt
	existing, waitErr := q.GetChildCallAttemptWait(ctx, db.GetChildCallAttemptWaitParams{EnvironmentID: parent.EnvironmentID, RunID: parent.ID, AttemptNumber: attempt.Number, ChildClaimID: claim.ID})
	if waitErr != nil && !errors.Is(waitErr, pgx.ErrNoRows) {
		return ChildCall{}, waitErr
	}
	if waitErr == nil && (existing.ID != pgvalue.UUID(invoke.RunWaitID)) {
		return ChildCall{}, ErrChildInvokeStale
	}
	requestFingerprint, err := RequestFingerprint("worker.child-call.wait", struct {
		Claim      string
		WaitID     string
		AttachID   string
		TurnID     string
		Generation int64
	}{fmt.Sprintf("%x", claim.RequestFingerprint), invoke.RunWaitID.String(), invoke.ResumeAttachID.String(), pgvalue.UUIDString(invoke.TurnID), invoke.RunGeneration.Int64})
	if err != nil {
		return ChildCall{}, err
	}
	if waitErr == nil && existing.RegistrationRequestFingerprint.String != requestFingerprint {
		return ChildCall{}, ErrChildInvokeStale
	}
	childRequest, err := idempotency.EncodeTaskChildInvokeFingerprint(invoke.Fingerprint)
	if err != nil {
		return ChildCall{}, fmt.Errorf("encode child task call request: %w", err)
	}
	instance := authority.Instance()
	call := ChildCall{
		ParentRunID: parent.ID, RunWaitID: invoke.RunWaitID,
		ResumeAttachID:     invoke.ResumeAttachID,
		ComputerInstanceID: instance.ID,
		WorkerEpoch:        instance.WorkerEpoch,
	}
	replayed, err := q.GetChildCallRunWaitReplay(ctx, db.GetChildCallRunWaitReplayParams{
		EnvironmentID: parent.EnvironmentID, RunID: parent.ID,
		AttemptNumber: attempt.Number, ID: pgvalue.UUID(invoke.RunWaitID),
		ChildRunID: pgvalue.UUID(childRunID), ChildClaimID: claim.ID,
		RegistrationRequestFingerprint: pgvalue.Text(requestFingerprint),
	})
	if err == nil {
		if err := authority.validateChildWaitScope(replayed); err != nil {
			return ChildCall{}, err
		}
		if replayed.SuspensionStatus == db.RunWaitStatusReleased {
			if replayed.ConditionStatus != db.WaitStatusCompleted || replayed.ConditionResult == nil {
				return ChildCall{}, ErrChildInvokeStale
			}
			call.Completed = true
			call.Resolution = append(json.RawMessage(nil), replayed.ConditionResult...)
		}
		return call, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return ChildCall{}, fmt.Errorf("load child task call replay: %w", err)
	}
	childRun, err := q.GetRun(ctx, db.GetRunParams{
		EnvironmentID: parent.EnvironmentID, ID: pgvalue.UUID(childRunID),
	})
	if err != nil || childRun.ParentRunID != parent.ID ||
		!childRun.ParentOwnsLifecycle.Valid || !childRun.ParentOwnsLifecycle.Bool ||
		childRun.ComputerID != pgvalue.UUID(childComputerID) ||
		childRun.ClaimID != claim.ID {
		return ChildCall{}, staleChildInvoke(err)
	}
	params := db.RegisterChildCallParams{
		RunID: parent.ID, EnvironmentID: parent.EnvironmentID,
		ExpectedRunningRevision: parent.Revision,
		AttemptNumber:           attempt.Number,
		CurrentRunLeaseID:       authority.lease.ID,
		ChildComputerID:         pgvalue.UUID(childComputerID), ID: pgvalue.UUID(invoke.RunWaitID),
		ChildRunID: pgvalue.UUID(childRunID), ChildTargetDeclaredID: pgvalue.Text(invoke.Task.TaskDeclaredID),
		ChildClaimID: claim.ID, ChildRequest: childRequest,
		RegistrationRequestFingerprint: pgvalue.Text(requestFingerprint),
	}
	if childRun.Status == db.RunStatusSucceeded || childRun.Status == db.RunStatusFailed ||
		childRun.Status == db.RunStatusCancelled || childRun.Status == db.RunStatusExpired ||
		childRun.Status == db.RunStatusSystemFailed {
		resolution, err := invoke.ChildResult(childRun)
		if err != nil {
			return ChildCall{}, err
		}
		_, err = q.RegisterResolvedChildCall(ctx, db.RegisterResolvedChildCallParams{
			ID: params.ID, EnvironmentID: params.EnvironmentID, RunID: params.RunID,
			ChildRunID: params.ChildRunID, ChildTargetDeclaredID: params.ChildTargetDeclaredID,
			ChildClaimID: params.ChildClaimID, ChildRequest: params.ChildRequest,
			ConditionResult:                resolution,
			RegistrationRequestFingerprint: params.RegistrationRequestFingerprint,
			ExpectedRunningRevision:        params.ExpectedRunningRevision,
			AttemptNumber:                  params.AttemptNumber,
			CurrentRunLeaseID:              params.CurrentRunLeaseID,
		})
		if err != nil {
			return ChildCall{}, staleChildInvoke(err)
		}
		if err := bindOrCheckChildWaitTurn(ctx, q, authority, invoke); err != nil {
			return ChildCall{}, err
		}
		call.Completed = true
		call.Resolution = resolution
		return call, nil
	}
	if childRun.Status != db.RunStatusQueued && childRun.Status != db.RunStatusRunning &&
		childRun.Status != db.RunStatusWaiting && childRun.Status != db.RunStatusRetryDelayed &&
		childRun.Status != db.RunStatusCancelRequested {
		return ChildCall{}, ErrChildInvokeStale
	}
	if _, err := q.RegisterChildCall(ctx, params); err != nil {
		return ChildCall{}, staleChildInvoke(err)
	}
	if err := bindOrCheckChildWaitTurn(ctx, q, authority, invoke); err != nil {
		return ChildCall{}, err
	}
	return call, nil
}

// bindOrCheckChildWaitTurn binds a call's wait to the invocation's Turn, or
// checks the Turn it is already bound to, against the locked execution.
func bindOrCheckChildWaitTurn(ctx context.Context, q db.Querier, a Execution, invoke ChildInvoke) error {
	wait, err := q.GetRunWait(ctx, db.GetRunWaitParams{AttemptNumber: a.attempt.Number, RunID: a.run.ID, ID: pgvalue.UUID(invoke.RunWaitID)})
	if err != nil {
		return err
	}
	if wait.TurnID.Valid {
		return a.validateChildWaitScope(wait)
	}
	wait.TurnID = invoke.TurnID
	wait.TurnRunGeneration = invoke.RunGeneration
	if invoke.TurnID.Valid {
		wait.TurnSessionID = a.session.ID
	}
	if err := a.validateChildWaitScope(wait); err != nil {
		return err
	}
	// The checks above validated the binding under the Session lock; the
	// conditional write independently rejects a changed owner.
	if !invoke.TurnID.Valid {
		return nil
	}
	_, err = q.BindRunWaitTurn(ctx, db.BindRunWaitTurnParams{SessionID: a.session.ID, TurnID: invoke.TurnID, RunGeneration: invoke.RunGeneration, WaitID: wait.ID})
	return err
}

// validateChildWaitScope checks a child wait's Turn binding against the
// locked execution's Session membership.
func (e Execution) validateChildWaitScope(wait db.RunWait) error {
	r, session := e.run, e.session
	if r.EntrypointKind == "task" {
		if r.SessionID.Valid || wait.TurnID.Valid || wait.TurnSessionID.Valid || wait.TurnRunGeneration.Valid {
			return ErrTurnScope
		}
		return nil
	}
	if r.EntrypointKind != "actor" || !session.ID.Valid || r.SessionID != session.ID || session.CurrentRunID != r.ID || session.ComputerID != r.ComputerID || (session.Status != "open" && session.Status != "closing") {
		return ErrTurnScope
	}
	if session.DispatchHoldID.Valid {
		return ErrTurnStopped
	}
	if session.ActiveTurnID != wait.TurnID {
		return ErrTurnScope
	}
	if wait.TurnID.Valid && (wait.TurnSessionID != session.ID || !wait.TurnRunGeneration.Valid || wait.TurnRunGeneration.Int64 != session.RunGeneration) {
		return ErrTurnScope
	}
	if !wait.TurnID.Valid && (wait.TurnSessionID.Valid || wait.TurnRunGeneration.Valid) {
		return ErrTurnScope
	}
	return nil
}

// validateWaitTurn checks the Turn new wait work names against the locked
// execution: a Task execution names none, an Actor execution its Session's
// active Turn, validated for new work.
func (e Execution) validateWaitTurn(ctx context.Context, tx pgx.Tx, turnID pgtype.UUID, generation pgtype.Int8) error {
	if e.run.EntrypointKind != "actor" {
		if turnID.Valid {
			return ErrTurnScope
		}
		return nil
	}
	if e.session.DispatchHoldID.Valid {
		return ErrTurnStopped
	}
	if e.session.ActiveTurnID != turnID {
		return ErrTurnScope
	}
	if !turnID.Valid {
		return nil
	}
	return e.ValidateTurnWork(ctx, tx, pgvalue.MustUUIDValue(turnID), generation.Int64)
}

// ParseWaitTurn parses the Turn a worker names for wait work. Neither value
// names no Turn; a Turn needs both and a positive generation, or it is
// ErrTurnScope.
func ParseWaitTurn(id *string, generation *int64) (pgtype.UUID, pgtype.Int8, error) {
	if id == nil && generation == nil {
		return pgtype.UUID{}, pgtype.Int8{}, nil
	}
	if id == nil || generation == nil || *generation <= 0 {
		return pgtype.UUID{}, pgtype.Int8{}, ErrTurnScope
	}
	parsed, err := ids.Parse(*id)
	if err != nil {
		return pgtype.UUID{}, pgtype.Int8{}, errors.New("turn_id must be a canonical UUIDv7")
	}
	return pgvalue.UUID(parsed), pgtype.Int8{Int64: *generation, Valid: true}, nil
}

func decodeChildTaskReceipt(raw []byte) (childTaskReceipt, error) {
	var receipt childTaskReceipt
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if len(raw) == 0 || decoder.Decode(&receipt) != nil {
		return childTaskReceipt{}, ErrTaskStartReceiptInvalid
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return childTaskReceipt{}, ErrTaskStartReceiptInvalid
	}
	if _, err := ids.Parse(receipt.RunID); err != nil {
		return childTaskReceipt{}, ErrTaskStartReceiptInvalid
	}
	if _, err := ids.Parse(receipt.ComputerID); err != nil {
		return childTaskReceipt{}, ErrTaskStartReceiptInvalid
	}
	return receipt, nil
}

// RequestFingerprint is the digest of a request scope and its canonical
// payload that an operation records to recognize its replay.
func RequestFingerprint(scope string, payload any) (string, error) {
	body, err := json.Marshal(struct {
		Scope   string `json:"scope"`
		Payload any    `json:"payload"`
	}{Scope: scope, Payload: payload})
	if err != nil {
		return "", err
	}
	canonical, err := jsoncanon.Transform(body)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canonical)
	return sha256sum.FormatDigest(sum[:]), nil
}

// staleChildInvoke reports a statement that addressed nothing, or a missing
// cause, as ErrChildInvokeStale.
func staleChildInvoke(err error) error {
	if err == nil || errors.Is(err, pgx.ErrNoRows) {
		return ErrChildInvokeStale
	}
	return err
}
