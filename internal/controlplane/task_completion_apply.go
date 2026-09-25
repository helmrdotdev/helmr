package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/deployment"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/helmrdotdev/helmr/internal/telemetry"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

var errStaleTaskCompletion = errors.New("task completion receipt is stale")

type taskCompletionFailurePoint string

const (
	taskCompletionPointReplay           taskCompletionFailurePoint = "replay"
	taskCompletionPointLocators         taskCompletionFailurePoint = "locators"
	taskCompletionPointAuthority        taskCompletionFailurePoint = "authority"
	taskCompletionPointOwner            taskCompletionFailurePoint = "owner"
	taskCompletionPointParentWait       taskCompletionFailurePoint = "parent_wait"
	taskCompletionPointCore             taskCompletionFailurePoint = "core"
	taskCompletionPointWorkspaceOwner   taskCompletionFailurePoint = "workspace_owner"
	taskCompletionPointParentAuthority  taskCompletionFailurePoint = "parent_authority"
	taskCompletionPointOutcome          taskCompletionFailurePoint = "outcome"
	taskCompletionPointOperation        taskCompletionFailurePoint = "operation"
	taskCompletionPointFence            taskCompletionFailurePoint = "fence"
	taskCompletionPointScope            taskCompletionFailurePoint = "scope"
	taskCompletionPointRuntime          taskCompletionFailurePoint = "runtime"
	taskCompletionPointDeadline         taskCompletionFailurePoint = "deadline"
	taskCompletionPointWorkspaceVersion taskCompletionFailurePoint = "workspace_version"
	taskCompletionPointAttempt          taskCompletionFailurePoint = "attempt"
	taskCompletionPointWorkspaceLease   taskCompletionFailurePoint = "workspace_lease"
	taskCompletionPointFinish           taskCompletionFailurePoint = "finish"
)

type taskCompletionReplayStore interface {
	GetTaskCompletionReplay(context.Context, db.GetTaskCompletionReplayParams) (pgtype.Text, error)
}

func (s *Server) completeTask(
	ctx context.Context,
	worker workerActor,
	request workerapi.CompleteTaskRequest,
	completion parsedTaskCompletion,
) error {
	replayed, err := taskCompletionWasReplayed(ctx, s.db, worker, request, completion)
	if err != nil || replayed {
		return staleAuthority(staleAuthorityTaskCompletion, taskCompletionPointReplay, err)
	}
	verified, err := s.verifyTaskComputerCapture(ctx, *completion.capture)
	if err != nil {
		return taskCompletionReplayAfterError(ctx, s.db, worker, request, completion, err)
	}
	completion.capture = &verified

	failurePoint := taskCompletionPointReplay
	err = s.inTx(ctx, func(work *txWork) error {
		replayed, err := taskCompletionWasReplayed(ctx, work.q, worker, request, completion)
		if err != nil || replayed {
			return staleAuthority(staleAuthorityTaskCompletion, taskCompletionPointReplay, err)
		}
		failurePoint = taskCompletionPointLocators
		locators, err := work.q.GetLiveRunLeaseLocators(ctx, db.GetLiveRunLeaseLocatorsParams{
			ID:               pgvalue.UUID(completion.lease.leaseID),
			LeaseSequence:    request.Lease.LeaseSequence,
			WorkerGroupID:    pgvalue.UUID(worker.WorkerGroupID),
			WorkerInstanceID: pgvalue.UUID(worker.WorkerInstanceID),
			WorkerEpoch:      worker.WorkerEpoch,
		})
		if err != nil {
			return staleTaskCompletion(err)
		}
		secrets, err := secret.LockAttemptDelivery(
			ctx,
			work.q,
			locators.RunID,
			locators.AttemptNumber,
			locators.WorkspaceID,
		)
		if err != nil {
			return fmt.Errorf("lock task completion secret authority: %w", err)
		}
		failurePoint = taskCompletionPointAuthority
		tx, ok := work.tx.(pgx.Tx)
		if !ok {
			return errors.New("task completion transaction does not expose PostgreSQL authority")
		}
		var authority runLeaseClaimAuthority
		ownedGraph, err := run.LockOwnedFinalizationWithRuntimeFence(ctx, tx, run.OwnedFinalizationRequest{OrgID: pgvalue.MustUUIDValue(locators.OrgID), ProjectID: pgvalue.MustUUIDValue(locators.ProjectID), EnvironmentID: pgvalue.MustUUIDValue(locators.EnvironmentID), RunID: pgvalue.MustUUIDValue(locators.RunID)}, func() error {
			var err error
			authority, err = lockRunPublicationAuthority(ctx, work.q, worker, pgvalue.UUID(completion.lease.leaseID), request.Lease.LeaseSequence, locators, db.RunStatusRunning)
			return err
		})
		if err != nil {
			return staleTaskCompletion(err)
		}
		failurePoint = taskCompletionPointOwner
		if err := validateRunFinalizationOwner(authority, locators); err != nil {
			return staleTaskCompletion(err)
		}
		failurePoint = taskCompletionPointParentWait
		if err := lockSameWorkspaceChildFinalization(ctx, work.q, &authority); err != nil {
			return staleTaskCompletion(err)
		}
		if !authority.enclosingWait.ID.Valid &&
			authority.run.ParentRunID.Valid && authority.run.ParentOwnsLifecycle.Valid &&
			authority.run.ParentOwnsLifecycle.Bool {
			failurePoint = taskCompletionPointParentWait
			authority.enclosingWait, err = lockParentOwnedChildWaitIfActive(
				ctx,
				work.q,
				authority.parentRun,
				db.LockParentOwnedChildWaitParams{
					EnvironmentID: authority.run.EnvironmentID,
					ParentRunID:   authority.run.ParentRunID,
					ChildRunID:    authority.run.ID,
				},
			)
			if err != nil {
				return staleTaskCompletion(err)
			}
		}
		failurePoint = taskCompletionPointCore
		if err := validateTaskCompletionAuthority(ctx, work.q, completion, authority); err != nil {
			return err
		}

		failurePoint = taskCompletionPointDeadline
		completedAt, err := work.q.GetTaskCompletionTime(ctx)
		if err != nil || !completedAt.Valid {
			if err == nil {
				err = errors.New("database task completion time is unavailable")
			}
			return err
		}
		if err := validateTaskCompletionDeadline(authority, completedAt.Time); err != nil {
			return err
		}

		retryAt, retry, err := taskCompletionRetryAt(authority.run, authority.attempt, completion, completedAt.Time)
		if err != nil {
			return deterministicWorkerAdmission(err)
		}
		if !retry {
			if _, err := ownedGraph.CancelDescendants(ctx); err != nil {
				return err
			}
		}
		if err := requireFinalizationComputer(ctx, work.q, authority, *completion.capture); err != nil {
			return staleTaskCompletion(err)
		}
		var versionID pgtype.UUID
		failurePoint = taskCompletionPointWorkspaceVersion
		if sameWorkspaceChildFinalization(authority) {
			versionID, err = recordChildTaskWorkspaceVersion(
				ctx, work.q, worker, authority, completion.capture.version(),
			)
			if err == nil {
				err = updateTaskWorkspaceMountFrontier(
					ctx, work.q, authority, versionID, completedAt,
				)
			}
		} else {
			versionID, err = recordTaskWorkspaceVersion(
				ctx,
				work.q,
				worker,
				authority,
				completion.capture.version(),
				completedAt,
			)
		}
		if err != nil {
			return err
		}

		failurePoint = taskCompletionPointAttempt
		if err := terminalizeTaskAttempt(
			ctx,
			work.q,
			authority,
			completion,
			completedAt,
		); err != nil {
			return err
		}
		failurePoint = taskCompletionPointWorkspaceLease
		if _, err := work.q.ReleaseTaskWorkspaceLease(ctx, db.ReleaseTaskWorkspaceLeaseParams{
			CompletedAt: completedAt,
			ID:          authority.workspaceLease.ID, WorkspaceID: authority.workspace.ID,
			WorkspaceMountID: authority.workspaceMount.ID, RuntimeInstanceID: authority.runtime.ID,
			OwnerRunLeaseID: authority.runLease.ID, BaseWorkspaceVersionID: authority.workspaceLease.BaseWorkspaceVersionID,
			OwnershipGeneration:    authority.workspace.OwnershipGeneration,
			WriterGeneration:       authority.workspace.WriterGeneration,
			MountFencingGeneration: authority.workspaceMount.FencingGeneration,
		}); err != nil {
			return staleTaskCompletion(err)
		}
		if sameWorkspaceChildFinalization(authority) {
			if _, err := work.q.RequestSameWorkspaceChildAttemptRuntimeDiscard(
				ctx,
				db.RequestSameWorkspaceChildAttemptRuntimeDiscardParams{
					CompletedAt: completedAt,
					OrgID:       authority.run.OrgID, ProjectID: authority.run.ProjectID,
					EnvironmentID: authority.run.EnvironmentID,
					WorkspaceID:   authority.workspace.ID, RunID: authority.run.ID,
					AttemptNumber:          authority.attempt.Number,
					RunLeaseID:             authority.runLease.ID,
					WorkspaceLeaseID:       authority.workspaceLease.ID,
					RuntimeInstanceID:      authority.runtime.ID,
					WorkspaceMountID:       authority.workspaceMount.ID,
					WorkerGroupID:          authority.worker.WorkerGroupID,
					WorkerInstanceID:       authority.worker.ID,
					WorkerEpoch:            authority.worker.CurrentEpoch.Int64,
					OwnershipGeneration:    authority.workspace.OwnershipGeneration,
					WriterGeneration:       authority.workspace.WriterGeneration,
					MountFencingGeneration: authority.workspaceMount.FencingGeneration,
				},
			); err != nil {
				return staleTaskCompletion(err)
			}
		}
		failurePoint = taskCompletionPointFinish
		switch {
		case retry:
			err = scheduleTaskRetry(ctx, work.q, authority, secrets, completedAt, retryAt, versionID)
		case sameWorkspaceChildFinalization(authority):
			err = finishSameWorkspaceChild(ctx, work.q, authority, completion, completedAt, versionID)
		default:
			err = finishTask(ctx, work.q, authority, completion, completedAt, versionID)
		}
		if err != nil {
			return err
		}
		return checkFinalizationPublicationDeadline(ctx, work.q, authority)
	})
	if err != nil {
		resolved := taskCompletionReplayAfterError(ctx, s.db, worker, request, completion, err)
		return staleAuthority(staleAuthorityTaskCompletion, failurePoint, resolved)
	}
	return nil
}

func taskCompletionWasReplayed(
	ctx context.Context,
	store taskCompletionReplayStore,
	worker workerActor,
	request workerapi.CompleteTaskRequest,
	completion parsedTaskCompletion,
) (bool, error) {
	fingerprint, err := store.GetTaskCompletionReplay(ctx, db.GetTaskCompletionReplayParams{
		RunLeaseID:    pgvalue.UUID(completion.lease.leaseID),
		LeaseSequence: request.Lease.LeaseSequence, WorkerGroupID: pgvalue.UUID(worker.WorkerGroupID),
		WorkerInstanceID: pgvalue.UUID(worker.WorkerInstanceID),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !fingerprint.Valid || fingerprint.String != completion.fingerprint {
		return false, errStaleTaskCompletion
	}
	return true, nil
}

func taskCompletionReplayAfterError(
	ctx context.Context,
	store taskCompletionReplayStore,
	worker workerActor,
	request workerapi.CompleteTaskRequest,
	completion parsedTaskCompletion,
	operationErr error,
) error {
	replayed, replayErr := taskCompletionWasReplayed(ctx, store, worker, request, completion)
	if replayed {
		return nil
	}
	if errors.Is(replayErr, errStaleTaskCompletion) {
		return errStaleTaskCompletion
	}
	if replayErr != nil {
		return errors.Join(operationErr, fmt.Errorf("check task completion replay: %w", replayErr))
	}
	return operationErr
}

func validateTaskCompletionAuthority(
	ctx context.Context,
	store db.Querier,
	completion parsedTaskCompletion,
	authority runLeaseClaimAuthority,
) error {
	if authority.run.EntrypointKind != "task" || authority.run.SessionID.Valid ||
		authority.runLease.Status != db.RunLeaseStatusFinalizing ||
		!authority.attempt.EntrypointEnteredAt.Valid ||
		authority.run.ActiveStartedAt.Valid ||
		!authority.runLease.FinalizationOperationID.Valid ||
		!authority.runLease.FinalizationKind.Valid ||
		!authority.runLease.FinalizationStartedAt.Valid ||
		!authority.runLease.FinalizationRequestFingerprint.Valid ||
		authority.attempt.BaseWorkspaceVersionID != authority.run.BaseWorkspaceVersionID ||
		(authority.run.ParentRunID.Valid != authority.run.ParentOwnsLifecycle.Valid) {
		return staleAuthority(staleAuthorityTaskCompletion, taskCompletionPointCore, errStaleTaskCompletion)
	}
	sameWorkspaceChild := sameWorkspaceChildFinalization(authority)
	if sameWorkspaceChild {
		if !authority.enclosingWait.ID.Valid {
			return staleAuthority(staleAuthorityTaskCompletion, taskCompletionPointParentWait, errStaleTaskCompletion)
		}
	} else if !authority.workspace.HeadVersionID.Valid ||
		authority.workspace.OwnerRunID != authority.run.ID ||
		authority.workspace.OwnerSessionID.Valid {
		return staleAuthority(staleAuthorityTaskCompletion, taskCompletionPointWorkspaceOwner, errStaleTaskCompletion)
	}
	if authority.run.ParentRunID.Valid && authority.run.ParentOwnsLifecycle.Bool {
		if authority.parentRun.ID != authority.run.ParentRunID {
			return staleAuthority(staleAuthorityTaskCompletion, taskCompletionPointParentAuthority, errStaleTaskCompletion)
		}
		if !sameWorkspaceChild && authority.enclosingWait.ID.Valid {
			if authority.enclosingWait.RunID != authority.parentRun.ID ||
				authority.enclosingWait.ChildRunID != authority.run.ID ||
				authority.enclosingWait.Kind != db.WaitKindChild ||
				authority.enclosingWait.ConditionStatus != db.WaitStatusPending {
				return staleAuthority(staleAuthorityTaskCompletion, taskCompletionPointParentAuthority, errStaleTaskCompletion)
			}
		} else if authority.parentRun.Status == db.RunStatusCancelRequested {
			return staleAuthority(staleAuthorityTaskCompletion, taskCompletionPointParentAuthority, errStaleTaskCompletion)
		}
	}
	if completion.capture == nil {
		return errStaleTaskCompletion
	}
	finalization := completion.capture.receipt
	wantKind := string(workerapi.RunFinalizationCapture)
	operationID, err := uuid.Parse(finalization.OperationID)
	if err != nil ||
		authority.runLease.FinalizationOperationID != pgvalue.UUID(operationID) ||
		authority.runLease.FinalizationKind.String != wantKind {
		return staleAuthority(staleAuthorityTaskCompletion, taskCompletionPointOperation, errStaleTaskCompletion)
	}
	assignment, err := projectRunLeaseAssignment(runLeaseProjectionAuthority{
		run: authority.run, attempt: authority.attempt, runtime: authority.runtime,
		runLease:  authority.runLease,
		workspace: authority.workspace, workspaceMount: authority.workspaceMount,
		workspaceLease: authority.workspaceLease,
	})
	if err != nil {
		return err
	}
	if !finalizationFenceMatchesLease(finalization.Fence, assignment) {
		return staleAuthority(staleAuthorityTaskCompletion, taskCompletionPointFence, errStaleTaskCompletion)
	}
	clear, err := store.RunFinalizationScopeIsClear(ctx, db.RunFinalizationScopeIsClearParams{
		RunID: authority.run.ID, AttemptNumber: authority.attempt.Number, WorkspaceID: authority.workspace.ID,
	})
	if err != nil {
		return err
	}
	if !clear.Valid || !clear.Bool {
		return staleAuthority(staleAuthorityTaskCompletion, taskCompletionPointScope, errStaleTaskCompletion)
	}
	return nil
}

func sameWorkspaceChildFinalization(authority runLeaseClaimAuthority) bool {
	return authority.run.ParentRunID.Valid &&
		authority.run.ParentOwnsLifecycle.Valid &&
		authority.run.ParentOwnsLifecycle.Bool &&
		authority.parentRun.ID == authority.run.ParentRunID &&
		authority.parentRun.WorkspaceID == authority.run.WorkspaceID
}

func validateTaskCompletionDeadline(authority runLeaseClaimAuthority, completedAt time.Time) error {
	if !completedAt.Before(authority.runLease.ExpiresAt.Time) ||
		!completedAt.Before(authority.workspaceLease.ExpiresAt.Time) ||
		!authority.runLease.ExpiresAt.Time.Equal(authority.workspaceLease.ExpiresAt.Time) ||
		authority.runLease.Status != db.RunLeaseStatusFinalizing ||
		authority.run.ActiveStartedAt.Valid ||
		!authority.runLease.FinalizationStartedAt.Valid ||
		authority.runLease.FinalizationStartedAt.Time.After(completedAt) {
		return errStaleTaskCompletion
	}
	return nil
}

func taskCompletionRetryAt(
	run db.Run,
	attempt db.RunAttempt,
	completion parsedTaskCompletion,
	completedAt time.Time,
) (time.Time, bool, error) {
	if completion.kind != taskCompletionFailed {
		return time.Time{}, false, nil
	}
	policy, err := deployment.ParseRetryManifest(run.RetryPolicy)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("parse pinned task retry policy: %w", err)
	}
	delay, retry, err := taskRetryDelay(policy, attempt.Number, nil)
	if err != nil || !retry {
		return time.Time{}, retry, err
	}
	return completedAt.Add(delay), true, nil
}

// Committed versions advance the saved head. The lease and Attempt keep the
// execution origin, including a restored private checkpoint, unchanged.
func recordTaskWorkspaceVersion(
	ctx context.Context,
	store taskWorkspaceVersionStore,
	worker workerActor,
	authority runLeaseClaimAuthority,
	capture workspaceVersionCapture,
	completedAt pgtype.Timestamptz,
) (pgtype.UUID, error) {
	version, err := store.PublishTaskWorkspaceVersion(ctx, db.PublishTaskWorkspaceVersionParams{
		ID:            pgvalue.UUID(uuid.NewV7()),
		EnvironmentID: authority.run.EnvironmentID, WorkspaceID: authority.workspace.ID,
		ParentVersionID: authority.workspace.HeadVersionID,
		ContentDigest:   pgvalue.Text(capture.root.Pack.Digest), SizeBytes: capture.root.LogicalBytes, EntryCount: 0,
		SourceWorkspaceLeaseID: authority.workspaceLease.ID,
		OwnershipGeneration:    authority.workspace.OwnershipGeneration,
		WriterGeneration:       authority.workspace.WriterGeneration, PublishedAt: completedAt,
	})
	if err != nil {
		return pgtype.UUID{}, fmt.Errorf("publish task workspace version: %w", err)
	}
	if err := updateTaskWorkspaceMountFrontier(ctx, store, authority, version.ID, completedAt); err != nil {
		return pgtype.UUID{}, err
	}
	if err := recordComputerVersionRoot(ctx, store, authority, version.ID, capture.root); err != nil {
		return pgtype.UUID{}, err
	}
	return version.ID, nil
}

type taskWorkspaceVersionStore interface {
	CreateComputerVersionRoot(context.Context, db.CreateComputerVersionRootParams) error
	PublishTaskWorkspaceVersion(context.Context, db.PublishTaskWorkspaceVersionParams) (db.ComputerVersion, error)
	UpdateTaskWorkspaceMountFrontier(context.Context, db.UpdateTaskWorkspaceMountFrontierParams) (db.WorkspaceMount, error)
}

func updateTaskWorkspaceMountFrontier(
	ctx context.Context,
	store taskWorkspaceMountUpdater,
	authority runLeaseClaimAuthority,
	newVersionID pgtype.UUID,
	completedAt pgtype.Timestamptz,
) error {
	if _, err := store.UpdateTaskWorkspaceMountFrontier(ctx, db.UpdateTaskWorkspaceMountFrontierParams{
		NewVersionID: newVersionID, CompletedAt: completedAt,
		ID: authority.workspaceMount.ID, OrgID: authority.run.OrgID,
		ProjectID: authority.run.ProjectID, EnvironmentID: authority.run.EnvironmentID,
		WorkspaceID: authority.workspace.ID, RuntimeInstanceID: authority.runtime.ID,
		BaseWorkspaceVersionID: authority.workspaceLease.BaseWorkspaceVersionID,
		MountFencingGeneration: authority.workspaceMount.FencingGeneration,
	}); err != nil {
		return staleTaskCompletion(err)
	}
	return nil
}

type taskWorkspaceMountUpdater interface {
	UpdateTaskWorkspaceMountFrontier(context.Context, db.UpdateTaskWorkspaceMountFrontierParams) (db.WorkspaceMount, error)
}

func terminalizeTaskAttempt(
	ctx context.Context,
	store db.Querier,
	authority runLeaseClaimAuthority,
	completion parsedTaskCompletion,
	completedAt pgtype.Timestamptz,
) error {
	leaseStatus := db.RunLeaseStatusFailed
	outcome := pgvalue.Text("failed")
	reason := "task_failed"
	var terminalError []byte
	switch completion.kind {
	case taskCompletionSucceeded:
		leaseStatus = db.RunLeaseStatusCompleted
		outcome = pgvalue.Text("succeeded")
		reason = "completed"
	case taskCompletionFailed:
		terminalError = completion.errorObject
	case taskCompletionPayloadInvalid:
		reason = "task_payload_invalid"
		terminalError = completion.errorObject
	default:
		return errors.New("task completion outcome is unsupported")
	}
	if _, err := store.CompleteTaskRunLease(ctx, db.CompleteTaskRunLeaseParams{
		Status: leaseStatus, CompletedAt: completedAt, ReasonCode: pgvalue.Text(reason), Error: terminalError,
		TerminalRequestFingerprint: pgvalue.Text(completion.fingerprint),
		ID:                         authority.runLease.ID, RunID: authority.run.ID, WorkspaceID: authority.workspace.ID,
		AttemptNumber: authority.attempt.Number, LeaseSequence: authority.runLease.LeaseSequence,
	}); err != nil {
		return staleTaskCompletion(err)
	}
	if _, err := store.CompleteTaskAttempt(ctx, db.CompleteTaskAttemptParams{
		TerminalOutcome: outcome, ReasonCode: pgvalue.Text(reason), Error: terminalError,
		CompletedAt: completedAt, RunID: authority.run.ID, Number: authority.attempt.Number,
		WorkspaceID: authority.workspace.ID,
	}); err != nil {
		return staleTaskCompletion(err)
	}
	return nil
}

func scheduleTaskRetry(
	ctx context.Context,
	store db.Querier,
	authority runLeaseClaimAuthority,
	secrets []secret.DeliveryEnvelope,
	completedAt pgtype.Timestamptz,
	retryAt time.Time,
	versionID pgtype.UUID,
) error {
	if !versionID.Valid {
		return errStaleTaskCompletion
	}
	if !sameWorkspaceChildFinalization(authority) {
		if _, err := store.AdvanceTaskRetryWorkspaceHead(ctx, db.AdvanceTaskRetryWorkspaceHeadParams{
			ResultWorkspaceVersionID: versionID, CompletedAt: completedAt,
			WorkspaceID: authority.workspace.ID, RunID: authority.run.ID,
			ExpectedHeadVersionID: authority.workspace.HeadVersionID,
			OwnershipGeneration:   authority.workspace.OwnershipGeneration, WriterGeneration: authority.workspace.WriterGeneration,
		}); err != nil {
			return staleTaskCompletion(err)
		}
	}
	nextAttempt := authority.attempt.Number + 1
	if _, err := store.CreateTaskRetryAttempt(ctx, db.CreateTaskRetryAttemptParams{
		ResultWorkspaceVersionID: versionID, Number: nextAttempt, RunID: authority.run.ID, WorkspaceID: authority.workspace.ID,
		PreviousAttemptNumber: authority.attempt.Number, RunLeaseID: authority.runLease.ID,
	}); err != nil {
		return staleTaskCompletion(err)
	}
	resolutions, err := activeSecretResolutions(secrets)
	if err != nil {
		return err
	}
	if err := secret.CreateAttemptResolutions(
		ctx, store, authority.workspace.ID, authority.run.ID, nextAttempt, resolutions,
	); err != nil {
		return fmt.Errorf("record retry secret resolutions: %w", err)
	}
	if _, err := store.DelayTaskRunRetry(ctx, db.DelayTaskRunRetryParams{
		ResultWorkspaceVersionID: versionID, NextAttemptNumber: nextAttempt, CompletedAt: completedAt, RetryAt: pgvalue.Timestamptz(retryAt),
		ID: authority.run.ID, WorkspaceID: authority.workspace.ID,
		PreviousAttemptNumber: authority.attempt.Number, RunLeaseID: authority.runLease.ID,
	}); err != nil {
		return staleTaskCompletion(err)
	}
	return nil
}

func finishTask(
	ctx context.Context,
	store db.Querier,
	authority runLeaseClaimAuthority,
	completion parsedTaskCompletion,
	completedAt pgtype.Timestamptz,
	versionID pgtype.UUID,
) error {
	status := db.RunStatusFailed
	reason := pgvalue.Text("task_failed")
	var output, failure []byte
	eventKind := api.RunEventKindFailed
	switch completion.kind {
	case taskCompletionSucceeded:
		status = db.RunStatusSucceeded
		reason = pgtype.Text{}
		output = completion.output
		eventKind = api.RunEventKindCompleted
	case taskCompletionFailed:
		var err error
		failure, err = runFailureFromCompletion(reason.String, completion.errorObject)
		if err != nil {
			return err
		}
	case taskCompletionPayloadInvalid:
		reason = pgvalue.Text("task_payload_invalid")
		var err error
		failure, err = runFailureFromCompletion(reason.String, completion.errorObject)
		if err != nil {
			return err
		}
	default:
		return errors.New("task completion outcome is unsupported")
	}
	if _, err := store.ReleaseTaskWorkspaceOwner(ctx, db.ReleaseTaskWorkspaceOwnerParams{
		NewHeadVersionID: versionID, CompletedAt: completedAt,
		ID: authority.workspace.ID, OrgID: authority.run.OrgID,
		ProjectID: authority.run.ProjectID, EnvironmentID: authority.run.EnvironmentID,
		RunID: authority.run.ID, OwnershipGeneration: authority.workspace.OwnershipGeneration,
		WriterGeneration:      authority.workspace.WriterGeneration,
		ExpectedHeadVersionID: authority.workspace.HeadVersionID,
	}); err != nil {
		return staleTaskCompletion(err)
	}
	if _, err := store.FinishTaskRun(ctx, db.FinishTaskRunParams{
		Status: status, Output: output, Failure: failure,
		CompletedAt: completedAt, ID: authority.run.ID, WorkspaceID: authority.workspace.ID,
		AttemptNumber: authority.attempt.Number, RunLeaseID: authority.runLease.ID,
	}); err != nil {
		return staleTaskCompletion(err)
	}
	payload, err := json.Marshal(struct {
		Reason string `json:"reason,omitempty"`
	}{Reason: reason.String})
	if err != nil {
		return err
	}
	if err := telemetry.ValidateEvent(eventKind, payload); err != nil {
		return err
	}
	if _, err := store.AppendRunEvent(ctx, db.AppendRunEventParams{
		OrgID: authority.run.OrgID, RunID: authority.run.ID, Kind: eventKind, Payload: payload,
	}); err != nil {
		return fmt.Errorf("append task terminal event: %w", err)
	}
	if authority.run.ParentRunID.Valid && authority.run.ParentOwnsLifecycle.Valid &&
		authority.run.ParentOwnsLifecycle.Bool && authority.enclosingWait.ID.Valid {
		terminalRun := authority.run
		terminalRun.Status = status
		terminalRun.Output = output
		terminalRun.Failure = failure
		if err := resolveParentOwnedChildWait(
			ctx, store, authority, terminalRun,
		); err != nil {
			return err
		}
	}
	return nil
}

func finishSameWorkspaceChild(
	ctx context.Context,
	store db.Querier,
	authority runLeaseClaimAuthority,
	completion parsedTaskCompletion,
	completedAt pgtype.Timestamptz,
	versionID pgtype.UUID,
) error {
	wait := authority.enclosingWait
	status := db.RunStatusFailed
	reason := pgvalue.Text("task_failed")
	var output, terminalError, failure []byte
	eventKind := api.RunEventKindFailed
	switch completion.kind {
	case taskCompletionSucceeded:
		if !versionID.Valid {
			return errStaleTaskCompletion
		}
		status = db.RunStatusSucceeded
		reason = pgtype.Text{}
		output = completion.output
		eventKind = api.RunEventKindCompleted
	case taskCompletionFailed:
		terminalError = completion.errorObject
		var err error
		failure, err = runFailureFromCompletion(reason.String, completion.errorObject)
		if err != nil {
			return err
		}
	case taskCompletionPayloadInvalid:
		reason = pgvalue.Text("task_payload_invalid")
		terminalError = completion.errorObject
		var err error
		failure, err = runFailureFromCompletion(reason.String, completion.errorObject)
		if err != nil {
			return err
		}
	default:
		return errors.New("same-workspace child outcome is unsupported")
	}

	child, err := store.FinishTaskRun(ctx, db.FinishTaskRunParams{
		Status: status, Output: output, Failure: failure,
		CompletedAt: completedAt, ID: authority.run.ID, WorkspaceID: authority.workspace.ID,
		AttemptNumber: authority.attempt.Number, RunLeaseID: authority.runLease.ID,
	})
	if err != nil {
		return staleTaskCompletion(err)
	}
	payload, err := json.Marshal(struct {
		Reason string `json:"reason,omitempty"`
	}{Reason: reason.String})
	if err != nil {
		return err
	}
	if err := telemetry.ValidateEvent(eventKind, payload); err != nil {
		return err
	}
	if _, err := store.AppendRunEvent(ctx, db.AppendRunEventParams{
		OrgID: authority.run.OrgID, RunID: authority.run.ID, Kind: eventKind, Payload: payload,
	}); err != nil {
		return fmt.Errorf("append same-workspace child terminal event: %w", err)
	}
	if completion.kind == taskCompletionSucceeded {
		var result []byte
		result, err = childTaskResult(child)
		if err != nil {
			return err
		}
		_, err = store.CompleteSameWorkspaceChildSuccess(
			ctx,
			db.CompleteSameWorkspaceChildSuccessParams{
				CompletedAt: completedAt, ConditionResult: result,
				ResumeWorkspaceVersionID: versionID,
				RunWaitID:                wait.ID, EnvironmentID: wait.EnvironmentID,
				ParentRunID: authority.parentRun.ID, WorkspaceID: authority.workspace.ID,
				ParentAttemptNumber:    authority.parentAttempt.Number,
				ChildRunID:             authority.run.ID,
				ExpectedParentRevision: wait.ExpectedRunRevision,
				ParentRunLeaseID:       wait.PriorRunLeaseID,
				SuspendCheckpointID:    wait.SuspendCheckpointID,
				ChildWriterGeneration:  wait.ChildWriterGeneration,
			},
		)
	} else {
		_, err = store.CompleteSameWorkspaceChildFailure(
			ctx,
			db.CompleteSameWorkspaceChildFailureParams{
				CompletedAt: completedAt, ConditionStatus: db.WaitStatusFailed,
				ConditionError: terminalError, ReasonCode: reason,
				RunWaitID: wait.ID, EnvironmentID: wait.EnvironmentID,
				ParentRunID: authority.parentRun.ID, WorkspaceID: authority.workspace.ID,
				ParentAttemptNumber:    authority.parentAttempt.Number,
				ChildRunID:             authority.run.ID,
				ExpectedParentRevision: wait.ExpectedRunRevision,
				ParentRunLeaseID:       wait.PriorRunLeaseID,
				SuspendCheckpointID:    wait.SuspendCheckpointID,
				ChildWriterGeneration:  wait.ChildWriterGeneration,
			},
		)
	}
	if err != nil {
		return staleTaskCompletion(err)
	}
	return nil
}

func lockParentOwnedChildWaitIfActive(
	ctx context.Context,
	store db.Querier,
	parent db.Run,
	params db.LockParentOwnedChildWaitParams,
) (db.RunWait, error) {
	wait, err := store.LockParentOwnedChildWait(ctx, params)
	if err == nil {
		return wait, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return db.RunWait{}, err
	}
	if parent.Status == db.RunStatusQueued ||
		parent.Status == db.RunStatusRunning ||
		parent.Status == db.RunStatusWaiting ||
		parent.Status == db.RunStatusRetryDelayed {
		return db.RunWait{}, nil
	}
	return db.RunWait{}, errStaleTaskCompletion
}

func resolveParentOwnedChildWait(
	ctx context.Context,
	store db.Querier,
	authority runLeaseClaimAuthority,
	child db.Run,
) error {
	wait := authority.enclosingWait
	result, err := childTaskResult(child)
	if err != nil {
		return err
	}
	switch wait.SuspensionStatus {
	case db.RunWaitStatusHot:
		_, err = store.CompleteHotChildRunWait(ctx, db.CompleteHotChildRunWaitParams{
			RunID: wait.RunID, EnvironmentID: wait.EnvironmentID,
			ExpectedRunRevision: wait.ExpectedRunRevision,
			AttemptNumber:       wait.AttemptNumber, CurrentRunLeaseID: wait.CurrentRunLeaseID,
			ConditionResult: result, ID: wait.ID, ChildRunID: child.ID,
		})
	case db.RunWaitStatusCheckpointing:
		_, err = store.CompleteCheckpointingChildRunWait(
			ctx,
			db.CompleteCheckpointingChildRunWaitParams{
				ConditionResult: result, ID: wait.ID, RunID: wait.RunID,
				ChildRunID: child.ID, ExpectedRunRevision: wait.ExpectedRunRevision,
				CurrentRunLeaseID: wait.CurrentRunLeaseID,
			},
		)
	case db.RunWaitStatusParked:
		_, err = store.CompleteParkedChildRunWait(
			ctx,
			db.CompleteParkedChildRunWaitParams{
				RunID: wait.RunID, EnvironmentID: wait.EnvironmentID,
				ExpectedRunRevision: wait.ExpectedRunRevision,
				AttemptNumber:       wait.AttemptNumber, ConditionResult: result,
				ID: wait.ID, ChildRunID: child.ID, PriorRunLeaseID: wait.PriorRunLeaseID,
				SuspendCheckpointID: wait.SuspendCheckpointID,
			},
		)
	default:
		return errStaleTaskCompletion
	}
	if err != nil {
		return staleTaskCompletion(err)
	}
	return nil
}

func staleTaskCompletion(err error) error {
	if errors.Is(err, errStaleWorkerClaims) {
		return err
	}
	if err == nil || errors.Is(err, pgx.ErrNoRows) || errors.Is(err, errStaleRunLeaseClaim) || errors.Is(err, errStaleRunFinalization) {
		return errStaleTaskCompletion
	}
	return err
}

func recordChildTaskWorkspaceVersion(
	ctx context.Context,
	store db.Querier,
	worker workerActor,
	authority runLeaseClaimAuthority,
	capture workspaceVersionCapture,
) (pgtype.UUID, error) {
	version, err := store.CreatePrivateCheckpointWorkspaceVersion(ctx, db.CreatePrivateCheckpointWorkspaceVersionParams{
		ID:            pgvalue.UUID(uuid.NewV7()),
		EnvironmentID: authority.run.EnvironmentID,
		WorkspaceID:   authority.workspace.ID, ParentVersionID: authority.workspaceLease.BaseWorkspaceVersionID,
		ContentDigest: pgvalue.Text(capture.root.Pack.Digest),
		SizeBytes:     capture.root.LogicalBytes, EntryCount: 0,
		SourceWorkspaceLeaseID: authority.workspaceLease.ID,
		OwnershipGeneration:    authority.workspace.OwnershipGeneration, WriterGeneration: authority.workspace.WriterGeneration,
	})
	if err != nil {
		return pgtype.UUID{}, fmt.Errorf("record private checkpoint workspace version: %w", err)
	}
	if err := recordComputerVersionRoot(ctx, store, authority, version.ID, capture.root); err != nil {
		return pgtype.UUID{}, err
	}
	return version.ID, nil
}
