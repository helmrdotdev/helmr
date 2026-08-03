package executor

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/client"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

const (
	runLeaseRenewEvery     = 10 * time.Second
	runLeaseRequestTimeout = 5 * time.Second
	runLeaseRetryEvery     = 100 * time.Millisecond
	runLeaseTerminalTail   = workerapi.RunFinalizationTerminalTail
	runLeaseReplayTail     = workerapi.RunFinalizationReplayTail
)

type runLeaseTaskWait struct {
	result RunLeaseTaskResult
	err    error
}

func (e Executor) ExecuteRunLease(
	ctx context.Context,
	work workerapi.RunLeaseWork,
) error {
	if e.RunLeases == nil {
		return errors.New("Run Lease control is required")
	}
	if e.RunLeaseTasks == nil {
		return errors.New("Run Lease Task runner is required")
	}
	if work.LeaseID == "" || work.LeaseSequence <= 0 {
		return errors.New("Run Lease work identity is invalid")
	}
	claim, err := e.RunLeases.ClaimRunLease(ctx, work)
	if err != nil {
		return fmt.Errorf("claim Run Lease: %w", err)
	}
	if claim.Lease.ID != work.LeaseID || claim.Lease.LeaseSequence != work.LeaseSequence {
		return errors.New("Run Lease claim does not match discovered work")
	}
	task, err := e.RunLeaseTasks.StartRunLeaseTask(ctx, &claim, e.RunLeases)
	if err != nil {
		return fmt.Errorf("start Run Lease Task: %w", err)
	}
	defer task.Close()
	current := claim.Lease
	result, current, err := e.awaitRunLeaseTask(ctx, task, current)
	if err != nil {
		if errors.Is(err, ErrDetached) {
			return nil
		}
		return err
	}
	current, err = e.renewRunLease(ctx, task, current)
	if err != nil {
		return fmt.Errorf("renew Run Lease before finalization: %w", err)
	}

	operationID, err := uuid.NewV7()
	if err != nil {
		return fmt.Errorf("create Run finalization operation ID: %w", err)
	}
	kind := runFinalizationKind(result)
	beginRequest := workerapi.BeginRunFinalizationRequest{
		Lease: current.Fence(), ProgramQuiesced: result.ProgramQuiesced,
		OperationID: operationID.String(), Kind: kind,
	}
	var begun workerapi.BeginRunFinalizationResponse
	if err := retryRunLeaseRequest(ctx, func(requestCtx context.Context) error {
		var requestErr error
		begun, requestErr = e.RunLeases.BeginRunFinalization(requestCtx, beginRequest)
		return requestErr
	}); err != nil {
		return fmt.Errorf("begin Run finalization: %w", err)
	}
	if begun.Lease != current.Fence() ||
		begun.OperationID != beginRequest.OperationID ||
		begun.Kind != kind ||
		begun.BaseWorkspaceVersionID != current.BaseWorkspaceVersionID {
		return errors.New("Run finalization response changed its identity")
	}
	frozen := current
	frozen.ExpiresAt = begun.ExpiresAt
	if err := validateRunLeaseExpiryAdvance(current, frozen); err != nil {
		return fmt.Errorf("validate frozen Run Lease: %w", err)
	}
	if !begun.ExpiresAt.After(current.ExpiresAt) {
		return errors.New("Run finalization response did not advance the expiry")
	}
	stageDeadline, replayTail, err := runLeaseFinalizationDeadlines(
		time.Now(),
		begun.ExpiresAt,
	)
	if err != nil {
		return err
	}
	stageCtx, cancelStage := context.WithDeadline(context.Background(), stageDeadline)
	defer cancelStage()
	if err := retryRunLeaseRequest(stageCtx, func(requestCtx context.Context) error {
		return task.BeginWorkspaceFinalization(
			requestCtx,
			current,
			frozen,
			begun.OperationID,
			begun.Kind,
		)
	}); err != nil {
		return fmt.Errorf("begin Workspace finalization: %w", err)
	}

	completeCtx, cancelComplete := context.WithDeadline(
		context.Background(),
		begun.ExpiresAt,
	)
	defer cancelComplete()
	completion := workerapi.CompleteTaskRequest{Lease: begun.Lease, Outcome: result.Outcome}
	actorCompletion := workerapi.CompleteActorRequest{Lease: begun.Lease}
	if result.ActorOutcome != nil {
		actorCompletion.Outcome = *result.ActorOutcome
	}
	if kind == workerapi.RunFinalizationCapture {
		var capture workerapi.TaskWorkspaceCapture
		if err := retryRunLeaseOperation(stageCtx, func(requestCtx context.Context) error {
			var requestErr error
			capture, requestErr = task.CaptureWorkspace(requestCtx)
			return requestErr
		}); err != nil {
			return fmt.Errorf("capture Task Workspace: %w", err)
		}
		completion.Workspace.Captured = &capture
		actorCompletion.Workspace.Captured = &capture
		if begun.Handoff != nil {
			checkpointID, err := uuid.NewV7()
			if err != nil {
				return fmt.Errorf("create handoff checkpoint ID: %w", err)
			}
			var manifest workerapi.CheckpointManifest
			if err := retryRunLeaseOperation(stageCtx, func(requestCtx context.Context) error {
				var requestErr error
				manifest, requestErr = task.CreateHandoffCheckpoint(
					requestCtx,
					*begun.Handoff,
					checkpointID.String(),
					capture,
				)
				return requestErr
			}); err != nil {
				return fmt.Errorf("create handoff checkpoint: %w", err)
			}
			completion.Handoff = &workerapi.TaskHandoffCheckpoint{
				CheckpointID: checkpointID.String(),
				Manifest:     manifest,
			}
		}
	} else {
		var rollback workerapi.TaskWorkspaceRollback
		if err := retryRunLeaseOperation(stageCtx, func(requestCtx context.Context) error {
			var requestErr error
			rollback, requestErr = task.ResetWorkspace(requestCtx)
			return requestErr
		}); err != nil {
			return fmt.Errorf("reset Task Workspace: %w", err)
		}
		completion.Workspace.RolledBack = &rollback
		actorCompletion.Workspace.RolledBack = &rollback
	}
	if err := retryRunLeaseCompletion(completeCtx, replayTail, func(requestCtx context.Context) error {
		if result.ActorOutcome != nil {
			return e.RunLeases.CompleteActor(requestCtx, actorCompletion)
		}
		return e.RunLeases.CompleteTask(requestCtx, completion)
	}); err != nil {
		return fmt.Errorf("complete Task: %w", err)
	}
	return nil
}

func retryRunLeaseCompletion(
	ctx context.Context,
	replayTail time.Duration,
	request func(context.Context) error,
) error {
	deadline, ok := ctx.Deadline()
	if !ok {
		return errors.New("Task completion deadline is required")
	}
	verificationDeadline := deadline.Add(-replayTail)
	if !verificationDeadline.After(time.Now()) {
		return errors.New("Task completion authority does not reserve its replay tail")
	}
	delay := runLeaseRetryEvery
	for time.Now().Before(verificationDeadline) {
		requestCtx, cancel := context.WithDeadline(ctx, verificationDeadline)
		err := request(requestCtx)
		cancel()
		if err == nil {
			return nil
		}
		if permanentRunLeaseRequestError(err) {
			return err
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
		if delay < time.Second {
			delay *= 2
			if delay > time.Second {
				delay = time.Second
			}
		}
	}
	return retryRunLeaseRequest(ctx, request)
}

func runLeaseRenewDelay(expiresAt time.Time) time.Duration {
	remaining := time.Until(expiresAt)
	if remaining <= 0 {
		return 0
	}
	delay := remaining / 3
	if delay > runLeaseRenewEvery {
		return runLeaseRenewEvery
	}
	if delay < time.Millisecond {
		return time.Millisecond
	}
	return delay
}

func runLeaseLogContext(
	parent context.Context,
	expiresAt time.Time,
) (context.Context, context.CancelFunc, error) {
	remaining := time.Until(expiresAt)
	if remaining <= 0 {
		return nil, nil, errors.New("Run Lease expired before log append")
	}
	timeout := min(remaining/4, runLeaseRequestTimeout)
	ctx, cancel := context.WithTimeout(parent, timeout)
	return ctx, cancel, nil
}

func runLeaseFinalizationDeadlines(
	now time.Time,
	expiresAt time.Time,
) (time.Time, time.Duration, error) {
	remaining := expiresAt.Sub(now)
	if remaining <= 0 {
		return time.Time{}, 0, errors.New("Run finalization authority is expired")
	}
	if remaining <= runLeaseTerminalTail {
		return time.Time{}, 0, errors.New("Run finalization authority is below the required terminal window")
	}
	return expiresAt.Add(-runLeaseTerminalTail), runLeaseReplayTail, nil
}

func (e Executor) awaitRunLeaseTask(
	ctx context.Context,
	task RunLeaseTask,
	current workerapi.RunLeaseAssignment,
) (RunLeaseTaskResult, workerapi.RunLeaseAssignment, error) {
	waitCtx, cancelWait := context.WithCancel(ctx)
	defer cancelWait()
	waited := make(chan runLeaseTaskWait, 1)
	go func() {
		result, err := task.Wait(waitCtx)
		waited <- runLeaseTaskWait{result: result, err: err}
	}()
	renewTimer := time.NewTimer(runLeaseRenewDelay(current.ExpiresAt))
	defer renewTimer.Stop()
	for {
		select {
		case result := <-waited:
			if result.err != nil {
				return RunLeaseTaskResult{}, current, fmt.Errorf("wait for Run Lease Task: %w", result.err)
			}
			return result.result, current, nil
		case <-renewTimer.C:
			renewed, err := e.renewRunLease(ctx, task, current)
			if err != nil {
				cancelWait()
				<-waited
				return RunLeaseTaskResult{}, current, fmt.Errorf("renew Run Lease: %w", err)
			}
			current = renewed
			renewTimer.Reset(runLeaseRenewDelay(current.ExpiresAt))
		case <-ctx.Done():
			cancelWait()
			<-waited
			return RunLeaseTaskResult{}, current, ctx.Err()
		}
	}
}

func (e Executor) renewRunLease(
	ctx context.Context,
	task RunLeaseTask,
	current workerapi.RunLeaseAssignment,
) (workerapi.RunLeaseAssignment, error) {
	renewal, err := task.RenewRunLease(ctx)
	if err != nil {
		return current, err
	}
	currentAtRenewal := current
	currentAtRenewal.BaseWorkspaceVersionID = renewal.Previous.BaseWorkspaceVersionID
	if err := validateRunLeaseExpiryAdvance(currentAtRenewal, renewal.Previous); err != nil {
		return current, fmt.Errorf("Run Lease Task changed authority outside an Actor Workspace frontier: %w", err)
	}
	if err := validateRunLeaseExpiryAdvance(renewal.Previous, renewal.Lease); err != nil {
		return current, err
	}
	return renewal.Lease, nil
}

func runFinalizationKind(result RunLeaseTaskResult) workerapi.RunFinalizationKind {
	if result.ActorOutcome != nil {
		if result.ActorOutcome.Succeeded != nil {
			return workerapi.RunFinalizationCapture
		}
		return workerapi.RunFinalizationReset
	}
	if result.Outcome.Succeeded != nil {
		return workerapi.RunFinalizationCapture
	}
	return workerapi.RunFinalizationReset
}

func retryRunLeaseRequest(
	ctx context.Context,
	request func(context.Context) error,
) error {
	delay := runLeaseRetryEvery
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		requestCtx, cancel := context.WithTimeout(ctx, runLeaseRequestTimeout)
		err := request(requestCtx)
		cancel()
		if err == nil {
			return nil
		}
		if permanentRunLeaseRequestError(err) {
			return err
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
		if delay < time.Second {
			delay *= 2
			if delay > time.Second {
				delay = time.Second
			}
		}
	}
}

func permanentRunLeaseRequestError(err error) bool {
	if errors.Is(err, errRunLeaseAuthorityLapsed) ||
		errors.Is(err, errRunSourceOperationUnavailable) {
		return true
	}
	for _, status := range []int{
		http.StatusBadRequest,
		http.StatusUnauthorized,
		http.StatusForbidden,
		http.StatusNotFound,
		http.StatusConflict,
		http.StatusRequestEntityTooLarge,
		http.StatusUnprocessableEntity,
	} {
		if client.IsStatus(err, status) {
			return true
		}
	}
	return false
}

func retryRunLeaseOperation(
	ctx context.Context,
	operation func(context.Context) error,
) error {
	delay := runLeaseRetryEvery
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := operation(ctx); err == nil {
			return nil
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
		if delay < time.Second {
			delay *= 2
			if delay > time.Second {
				delay = time.Second
			}
		}
	}
}
