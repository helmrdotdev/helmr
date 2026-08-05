package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/deployment"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/helmrdotdev/helmr/internal/workspace"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

var errStaleTaskCompletion = errors.New("task completion receipt is stale")

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
		return err
	}
	if completion.capture != nil {
		verified, err := s.verifyTaskWorkspaceCapture(ctx, *completion.capture)
		if err != nil {
			return taskCompletionReplayAfterError(ctx, s.db, worker, request, completion, err)
		}
		completion.capture = &verified
	}
	if completion.handoff != nil {
		if err := s.verifyCheckpointRuntimeArtifacts(
			ctx,
			completion.handoff.artifacts,
		); err != nil {
			return taskCompletionReplayAfterError(ctx, s.db, worker, request, completion, err)
		}
	}

	err = s.inTx(ctx, func(work *txWork) error {
		replayed, err := taskCompletionWasReplayed(ctx, work.q, worker, request, completion)
		if err != nil || replayed {
			return err
		}
		locators, err := work.q.GetLiveRunLeaseLocators(ctx, db.GetLiveRunLeaseLocatorsParams{
			ID:                    pgvalue.UUID(completion.lease.leaseID),
			LeaseSequence:         request.Lease.LeaseSequence,
			WorkerGroupID:         worker.WorkerGroupID,
			WorkerInstanceID:      pgvalue.UUID(worker.WorkerInstanceID),
			WorkerEpoch:           worker.WorkerEpoch,
			WorkerProtocolVersion: worker.ProtocolVersion,
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
		authority, err := lockLiveRunFinalizationAuthority(
			ctx,
			work.q,
			worker,
			pgvalue.UUID(completion.lease.leaseID),
			request.Lease.LeaseSequence,
			locators,
		)
		if err != nil {
			return staleTaskCompletion(err)
		}
		if err := validateRunFinalizationOwner(authority, locators); err != nil {
			return staleTaskCompletion(err)
		}
		handoff, err := lockSameWorkspaceChildFinalization(ctx, work.q, &authority)
		if err != nil {
			return staleTaskCompletion(err)
		}
		if handoff == nil &&
			authority.run.ParentRunID.Valid && authority.run.ParentOwnsLifecycle.Valid &&
			authority.run.ParentOwnsLifecycle.Bool {
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
		if err := validateTaskCompletionAuthority(ctx, work.q, request, completion, authority); err != nil {
			return err
		}
		if completion.handoff != nil {
			identity, err := work.q.GetRuntimeIdentityForCheckpoint(ctx, authority.runtime.RuntimeIdentityID)
			manifestIdentity := request.Handoff.Manifest.RecoveryPoint.Runtime
			if err != nil ||
				identity.ID != manifestIdentity.ID ||
				identity.RuntimeArch != manifestIdentity.Arch ||
				identity.RuntimeABI != manifestIdentity.ABI ||
				identity.KernelDigest != manifestIdentity.KernelDigest ||
				identity.InitramfsDigest != manifestIdentity.InitramfsDigest ||
				identity.RootfsDigest != manifestIdentity.RootfsDigest {
				return staleTaskCompletion(err)
			}
			if err := validateCheckpointSubstrateAuthority(
				ctx,
				work.q,
				authority,
				request.Handoff.Manifest,
			); err != nil {
				return staleTaskCompletion(err)
			}
		}
		if completion.rollback != nil {
			if err := validateTaskWorkspaceRollback(ctx, work.q, authority, *completion.rollback); err != nil {
				return err
			}
		}

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
			return err
		}
		var versionID pgtype.UUID
		if completion.capture != nil {
			if sameWorkspaceChildFinalization(authority) {
				versionID, err = recordCheckpointWorkspaceVersion(
					ctx, work.q, worker, authority, *completion.capture,
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
					*completion.capture,
					completedAt,
				)
			}
			if err != nil {
				return err
			}
		} else if authority.workspaceLease.BaseVersionID != authority.run.BaseWorkspaceVersionID {
			if err := updateTaskWorkspaceMountFrontier(
				ctx,
				work.q,
				authority,
				authority.run.BaseWorkspaceVersionID,
				completedAt,
			); err != nil {
				return err
			}
		}
		if err := terminalizeTaskAttempt(
			ctx,
			work.q,
			authority,
			completion,
			completedAt,
		); err != nil {
			return err
		}
		if _, err := work.q.ReleaseTaskWorkspaceLease(ctx, db.ReleaseTaskWorkspaceLeaseParams{
			CompletedAt: completedAt,
			ID:          authority.workspaceLease.ID, WorkspaceID: authority.workspace.ID,
			WorkspaceMountID: authority.workspaceMount.ID, RuntimeInstanceID: authority.runtime.ID,
			OwnerRunLeaseID: authority.runLease.ID, BaseVersionID: authority.workspaceLease.BaseVersionID,
			OwnershipGeneration:    authority.workspace.OwnershipGeneration,
			WriterGeneration:       authority.workspace.WriterGeneration,
			MountFencingGeneration: authority.workspaceMount.FencingGeneration,
		}); err != nil {
			return staleTaskCompletion(err)
		}
		if retry {
			if sameWorkspaceChildFinalization(authority) {
				if _, err := work.q.ClearSameWorkspaceChildWriter(
					ctx,
					db.ClearSameWorkspaceChildWriterParams{
						CompletedAt: completedAt, RunWaitID: authority.enclosingWait.ID,
						EnvironmentID: authority.run.EnvironmentID,
						ParentRunID:   authority.parentRun.ID, WorkspaceID: authority.workspace.ID,
						ChildRunID:            authority.run.ID,
						ChildWriterGeneration: authority.enclosingWait.ChildWriterGeneration,
					},
				); err != nil {
					return staleTaskCompletion(err)
				}
			}
			return scheduleTaskRetry(ctx, work.q, authority, secrets, completedAt, retryAt)
		}
		if sameWorkspaceChildFinalization(authority) {
			return finishSameWorkspaceChild(
				ctx, work.q, worker, authority, completion, completedAt, versionID,
			)
		}
		return finishTask(ctx, work.q, authority, completion, completedAt, versionID)
	})
	if err != nil {
		return taskCompletionReplayAfterError(ctx, s.db, worker, request, completion, err)
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
		LeaseSequence: request.Lease.LeaseSequence, WorkerGroupID: worker.WorkerGroupID,
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
	request workerapi.CompleteTaskRequest,
	completion parsedTaskCompletion,
	authority runLeaseClaimAuthority,
) error {
	if authority.run.EntrypointKind != "task" || authority.run.SessionID.Valid ||
		authority.runLease.State != db.RunLeaseStateFinalizing ||
		!authority.attempt.EntrypointEnteredAt.Valid ||
		authority.run.ActiveStartedAt.Valid ||
		!authority.runLease.FinalizationOperationID.Valid ||
		!authority.runLease.FinalizationKind.Valid ||
		!authority.runLease.FinalizationStartedAt.Valid ||
		!authority.runLease.FinalizationRequestFingerprint.Valid ||
		authority.attempt.BaseWorkspaceVersionID != authority.run.BaseWorkspaceVersionID ||
		(authority.run.ParentRunID.Valid != authority.run.ParentOwnsLifecycle.Valid) {
		return errStaleTaskCompletion
	}
	sameWorkspaceChild := sameWorkspaceChildFinalization(authority)
	if sameWorkspaceChild {
		if !authority.enclosingWait.ID.Valid ||
			(completion.kind == taskCompletionSucceeded) != (completion.handoff != nil) {
			return errStaleTaskCompletion
		}
		if completion.handoff != nil {
			if completion.handoff.parentRunID != pgvalue.MustUUIDValue(authority.parentRun.ID) ||
				completion.handoff.attemptNumber != authority.parentAttempt.Number ||
				completion.handoff.waitID != pgvalue.MustUUIDValue(authority.enclosingWait.ID) {
				return errStaleTaskCompletion
			}
			correlationID, err := checkpointCorrelationID(authority.checkpoint, authority.enclosingWait)
			if err != nil ||
				request.Handoff == nil ||
				request.Handoff.Manifest.RecoveryPoint.CorrelationID != correlationID {
				return errStaleTaskCompletion
			}
		}
	} else if !authority.workspace.HeadVersionID.Valid ||
		authority.workspace.HeadVersionID != authority.run.BaseWorkspaceVersionID ||
		authority.workspace.OwnerRunID != authority.run.ID ||
		authority.workspace.OwnerSessionID.Valid ||
		completion.handoff != nil {
		return errStaleTaskCompletion
	}
	if authority.run.ParentRunID.Valid && authority.run.ParentOwnsLifecycle.Bool {
		if authority.parentRun.ID != authority.run.ParentRunID {
			return errStaleTaskCompletion
		}
		if !sameWorkspaceChild && authority.enclosingWait.ID.Valid {
			if authority.enclosingWait.RunID != authority.parentRun.ID ||
				authority.enclosingWait.ChildRunID != authority.run.ID ||
				!authority.enclosingWait.ChildParentOwned.Valid ||
				!authority.enclosingWait.ChildParentOwned.Bool ||
				authority.enclosingWait.Kind != db.WaitKindChild ||
				authority.enclosingWait.ConditionState != db.WaitStatePending {
				return errStaleTaskCompletion
			}
		} else if authority.parentRun.Status == db.RunStatusCancelRequested {
			return errStaleTaskCompletion
		}
	}
	if completion.kind != taskCompletionSucceeded &&
		(completion.rollback == nil || pgvalue.UUID(completion.rollback.baseID) != authority.run.BaseWorkspaceVersionID) {
		return errStaleTaskCompletion
	}
	var finalization workspace.FinalizationRequest
	wantKind := string(workerapi.RunFinalizationReset)
	if completion.capture != nil {
		finalization = completion.capture.receipt
		wantKind = string(workerapi.RunFinalizationCapture)
	} else {
		finalization = completion.rollback.receipt
	}
	operationID, err := uuid.Parse(finalization.OperationID)
	if err != nil ||
		authority.runLease.FinalizationOperationID != pgvalue.UUID(operationID) ||
		authority.runLease.FinalizationKind.String != wantKind {
		return errStaleTaskCompletion
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
		return errStaleTaskCompletion
	}
	clear, err := store.RunFinalizationScopeIsClear(ctx, db.RunFinalizationScopeIsClearParams{
		RunID: authority.run.ID, AttemptNumber: authority.attempt.Number, WorkspaceID: authority.workspace.ID,
	})
	if err != nil {
		return err
	}
	if !clear.Valid || !clear.Bool {
		return errStaleTaskCompletion
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

func validateTaskWorkspaceRollback(
	ctx context.Context,
	store taskWorkspaceRollbackStore,
	authority runLeaseClaimAuthority,
	rollback parsedTaskWorkspaceRollback,
) error {
	version, err := store.GetTaskWorkspaceResetVersion(ctx, db.GetTaskWorkspaceResetVersionParams{
		EnvironmentID: authority.run.EnvironmentID, WorkspaceID: authority.workspace.ID,
		ID: authority.run.BaseWorkspaceVersionID,
	})
	if err != nil {
		return staleTaskCompletion(err)
	}
	if version.ID != authority.run.BaseWorkspaceVersionID ||
		rollback.target.BaseVersionID != pgvalue.UUIDString(version.ID) ||
		rollback.target.Tree.Digest != version.ContentDigest ||
		rollback.target.Tree.SizeBytes != version.SizeBytes ||
		rollback.target.Tree.EntryCount != int(version.EntryCount) {
		return errStaleTaskCompletion
	}
	switch rollback.target.Kind {
	case workspace.ResetTargetEmpty:
		if version.ParentVersionID.Valid || version.ArtifactID.Valid || version.ArtifactKind.Valid ||
			version.Kind != db.WorkspaceVersionKindSystem || version.SourceWorkspaceLeaseID.Valid ||
			version.OwnershipGeneration != 0 || version.WriterGeneration != 0 ||
			version.ContentDigest != workspace.CanonicalEmptyTreeDigest || version.SizeBytes != 0 || version.EntryCount != 0 {
			return errStaleTaskCompletion
		}
	case workspace.ResetTargetArtifact:
		if rollback.target.Artifact == nil || !version.ParentVersionID.Valid || !version.ArtifactID.Valid ||
			!version.ArtifactKind.Valid || version.ArtifactKind.ArtifactKind != db.ArtifactKindWorkspaceVersion ||
			!version.SourceWorkspaceLeaseID.Valid || version.Kind != db.WorkspaceVersionKindUser {
			return errStaleTaskCompletion
		}
		artifact, err := store.GetArtifact(ctx, db.GetArtifactParams{
			OrgID: authority.run.OrgID, ProjectID: authority.run.ProjectID,
			EnvironmentID: version.EnvironmentID, ID: version.ArtifactID,
		})
		if err != nil {
			return staleTaskCompletion(err)
		}
		if artifact.Kind != db.ArtifactKindWorkspaceVersion ||
			artifact.Digest != rollback.target.Artifact.Digest ||
			artifact.SizeBytes != rollback.target.Artifact.SizeBytes ||
			artifact.MediaType != rollback.target.Artifact.MediaType ||
			rollback.target.Artifact.Encoding != workspace.ArtifactEncoding ||
			rollback.target.Artifact.EntryCount != int(version.EntryCount) {
			return errStaleTaskCompletion
		}
	default:
		return errStaleTaskCompletion
	}
	return nil
}

type taskWorkspaceRollbackStore interface {
	GetTaskWorkspaceResetVersion(context.Context, db.GetTaskWorkspaceResetVersionParams) (db.WorkspaceVersion, error)
	GetArtifact(context.Context, db.GetArtifactParams) (db.Artifact, error)
}

func validateTaskCompletionDeadline(authority runLeaseClaimAuthority, completedAt time.Time) error {
	if !completedAt.Before(authority.runLease.ExpiresAt.Time) ||
		!completedAt.Before(authority.workspaceLease.ExpiresAt.Time) ||
		!authority.runLease.ExpiresAt.Time.Equal(authority.workspaceLease.ExpiresAt.Time) ||
		authority.runLease.State != db.RunLeaseStateFinalizing ||
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

func recordTaskWorkspaceVersion(
	ctx context.Context,
	store taskWorkspaceVersionStore,
	worker workerActor,
	authority runLeaseClaimAuthority,
	capture parsedTaskWorkspaceCapture,
	completedAt pgtype.Timestamptz,
) (pgtype.UUID, error) {
	artifact := capture.artifact
	if _, err := store.UpsertCasObject(ctx, db.UpsertCasObjectParams{
		OrgID: authority.run.OrgID, Digest: artifact.Digest,
		SizeBytes: artifact.SizeBytes, MediaType: artifact.MediaType,
	}); err != nil {
		return pgtype.UUID{}, fmt.Errorf("record task workspace CAS object: %w", err)
	}
	artifactRow, err := store.CreateArtifact(ctx, db.CreateArtifactParams{
		ID: pgvalue.UUID(uuid.Must(uuid.NewV7())), OrgID: authority.run.OrgID,
		ProjectID: authority.run.ProjectID, EnvironmentID: authority.run.EnvironmentID,
		Digest: artifact.Digest, Kind: db.ArtifactKindWorkspaceVersion,
		SizeBytes: artifact.SizeBytes, MediaType: artifact.MediaType,
		CreatedByWorkerInstanceID: pgvalue.UUID(worker.WorkerInstanceID),
	})
	if err != nil {
		return pgtype.UUID{}, fmt.Errorf("record task workspace artifact: %w", err)
	}
	version, err := store.PublishTaskWorkspaceVersion(ctx, db.PublishTaskWorkspaceVersionParams{
		ID:            pgvalue.UUID(uuid.Must(uuid.NewV7())),
		EnvironmentID: authority.run.EnvironmentID, WorkspaceID: authority.workspace.ID,
		ParentVersionID: authority.workspaceLease.BaseVersionID, ArtifactID: artifactRow.ID,
		ContentDigest: capture.tree.Digest, SizeBytes: capture.tree.SizeBytes, EntryCount: int32(capture.tree.EntryCount),
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
	return version.ID, nil
}

type taskWorkspaceVersionStore interface {
	UpsertCasObject(context.Context, db.UpsertCasObjectParams) (db.CasObject, error)
	CreateArtifact(context.Context, db.CreateArtifactParams) (db.Artifact, error)
	PublishTaskWorkspaceVersion(context.Context, db.PublishTaskWorkspaceVersionParams) (db.WorkspaceVersion, error)
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
		BaseVersionID:          authority.workspaceLease.BaseVersionID,
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
	leaseState := db.RunLeaseStateFailed
	outcome := pgvalue.Text("failed")
	reason := "task_failed"
	var terminalError []byte
	switch completion.kind {
	case taskCompletionSucceeded:
		leaseState = db.RunLeaseStateCompleted
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
		State: leaseState, CompletedAt: completedAt, ReasonCode: pgvalue.Text(reason), Error: terminalError,
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
) error {
	nextAttempt := authority.attempt.Number + 1
	if _, err := store.CreateTaskRetryAttempt(ctx, db.CreateTaskRetryAttemptParams{
		Number: nextAttempt, RunID: authority.run.ID, WorkspaceID: authority.workspace.ID,
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
		NextAttemptNumber: nextAttempt, CompletedAt: completedAt, RetryAt: pgvalue.Timestamptz(retryAt),
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
	var output, terminalError []byte
	eventKind := api.RunEventKindFailed
	switch completion.kind {
	case taskCompletionSucceeded:
		status = db.RunStatusSucceeded
		reason = pgtype.Text{}
		output = completion.output
		eventKind = api.RunEventKindCompleted
	case taskCompletionFailed:
		terminalError = completion.errorObject
	case taskCompletionPayloadInvalid:
		reason = pgvalue.Text("task_payload_invalid")
		terminalError = completion.errorObject
	default:
		return errors.New("task completion outcome is unsupported")
	}
	if _, err := store.ReleaseTaskWorkspaceOwner(ctx, db.ReleaseTaskWorkspaceOwnerParams{
		NewHeadVersionID: versionID, CompletedAt: completedAt,
		ID: authority.workspace.ID, OrgID: authority.run.OrgID,
		ProjectID: authority.run.ProjectID, EnvironmentID: authority.run.EnvironmentID,
		RunID: authority.run.ID, OwnershipGeneration: authority.workspace.OwnershipGeneration,
		WriterGeneration:      authority.workspace.WriterGeneration,
		ExpectedHeadVersionID: authority.run.BaseWorkspaceVersionID,
	}); err != nil {
		return staleTaskCompletion(err)
	}
	if _, err := store.FinishTaskRun(ctx, db.FinishTaskRunParams{
		Status: status, Output: output, ReasonCode: reason, Error: terminalError,
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
		terminalRun.TerminalReasonCode = reason
		terminalRun.Error = terminalError
		if err := resolveParentOwnedChildWait(
			ctx, store, authority, terminalRun, completedAt,
		); err != nil {
			return err
		}
	}
	return nil
}

func finishSameWorkspaceChild(
	ctx context.Context,
	store db.Querier,
	worker workerActor,
	authority runLeaseClaimAuthority,
	completion parsedTaskCompletion,
	completedAt pgtype.Timestamptz,
	versionID pgtype.UUID,
) error {
	wait := authority.enclosingWait
	status := db.RunStatusFailed
	reason := pgvalue.Text("task_failed")
	var output, terminalError []byte
	eventKind := api.RunEventKindFailed
	switch completion.kind {
	case taskCompletionSucceeded:
		if completion.handoff == nil || !versionID.Valid ||
			completion.handoff.checkpointID == pgvalue.MustUUIDValue(wait.SuspendCheckpointID) {
			return errStaleTaskCompletion
		}
		status = db.RunStatusSucceeded
		reason = pgtype.Text{}
		output = completion.output
		eventKind = api.RunEventKindCompleted
	case taskCompletionFailed:
		terminalError = completion.errorObject
	case taskCompletionPayloadInvalid:
		reason = pgvalue.Text("task_payload_invalid")
		terminalError = completion.errorObject
	default:
		return errors.New("same-workspace child outcome is unsupported")
	}

	if completion.handoff != nil {
		checkpoint := completion.handoff
		if _, err := store.CreateRunCheckpoint(ctx, db.CreateRunCheckpointParams{
			ID: pgvalue.UUID(checkpoint.checkpointID), Kind: db.RunCheckpointKindHandoffResume,
			RunID: authority.parentRun.ID, AttemptNumber: authority.parentAttempt.Number,
			RunWaitID: wait.ID, SourceRunLeaseID: authority.checkpoint.SourceRunLeaseID,
			SourceWorkspaceLeaseID:        authority.checkpoint.SourceWorkspaceLeaseID,
			WorkspaceID:                   authority.workspace.ID,
			BaseWorkspaceVersionID:        authority.parentAttempt.BaseWorkspaceVersionID,
			PrivateWorkspaceVersionID:     versionID,
			ActorSpeculativeInputSequence: authority.checkpoint.ActorSpeculativeInputSequence,
			RestoreManifest:               checkpoint.manifest,
		}); err != nil {
			return staleTaskCompletion(err)
		}
		if err := recordCheckpointRuntimeArtifacts(
			ctx, store, worker, authority, checkpoint.checkpointID, checkpoint.artifacts,
		); err != nil {
			return err
		}
		if _, err := store.MarkRunCheckpointReady(ctx, db.MarkRunCheckpointReadyParams{
			PrivateWorkspaceVersionID: versionID, RestoreManifest: checkpoint.manifest,
			ReadyRequestFingerprint: pgvalue.Text(completion.fingerprint),
			RunID:                   authority.parentRun.ID, AttemptNumber: authority.parentAttempt.Number,
			ID: pgvalue.UUID(checkpoint.checkpointID),
		}); err != nil {
			return staleTaskCompletion(err)
		}
	}

	child, err := store.FinishTaskRun(ctx, db.FinishTaskRunParams{
		Status: status, Output: output, ReasonCode: reason, Error: terminalError,
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
	if _, err := store.AppendRunEvent(ctx, db.AppendRunEventParams{
		OrgID: authority.run.OrgID, RunID: authority.run.ID, Kind: eventKind, Payload: payload,
	}); err != nil {
		return fmt.Errorf("append same-workspace child terminal event: %w", err)
	}
	if completion.kind == taskCompletionSucceeded &&
		authority.parentEnclosingWait.ID.Valid {
		outer := authority.parentEnclosingWait
		if _, err := store.ClearSameWorkspaceChildWriter(
			ctx,
			db.ClearSameWorkspaceChildWriterParams{
				CompletedAt: completedAt, RunWaitID: outer.ID,
				EnvironmentID: authority.run.EnvironmentID,
				ParentRunID:   outer.RunID, WorkspaceID: authority.workspace.ID,
				ChildRunID:            authority.parentRun.ID,
				ChildWriterGeneration: outer.ChildWriterGeneration,
			},
		); err != nil {
			return staleTaskCompletion(err)
		}
	}

	var completed db.RunWait
	if completion.kind == taskCompletionSucceeded {
		var result []byte
		result, err = childTaskResult(child)
		if err != nil {
			return err
		}
		completed, err = store.CompleteSameWorkspaceChildSuccess(
			ctx,
			db.CompleteSameWorkspaceChildSuccessParams{
				CompletedAt: completedAt, ConditionResult: result,
				ChildResultVersionID:      versionID,
				HandoffResumeCheckpointID: pgvalue.UUID(completion.handoff.checkpointID),
				RunWaitID:                 wait.ID, EnvironmentID: wait.EnvironmentID,
				ParentRunID: authority.parentRun.ID, WorkspaceID: authority.workspace.ID,
				ParentAttemptNumber:        authority.parentAttempt.Number,
				ChildRunID:                 authority.run.ID,
				ExpectedParentStateVersion: wait.ExpectedRunStateVersion,
				ParentRunLeaseID:           wait.PriorRunLeaseID,
				SuspendCheckpointID:        wait.SuspendCheckpointID,
				ChildWriterGeneration:      wait.ChildWriterGeneration,
			},
		)
	} else {
		if len(authority.handoffAncestors) != 0 {
			completed, err = cascadeSameWorkspaceChildFailure(
				ctx,
				store,
				authority,
				completedAt,
			)
		} else {
			completed, err = store.CompleteSameWorkspaceChildFailure(
				ctx,
				db.CompleteSameWorkspaceChildFailureParams{
					CompletedAt: completedAt, ConditionState: db.WaitStateFailed,
					ConditionError: terminalError, ReasonCode: reason,
					RunWaitID: wait.ID, EnvironmentID: wait.EnvironmentID,
					ParentRunID: authority.parentRun.ID, WorkspaceID: authority.workspace.ID,
					ParentAttemptNumber:        authority.parentAttempt.Number,
					ChildRunID:                 authority.run.ID,
					ExpectedParentStateVersion: wait.ExpectedRunStateVersion,
					ParentRunLeaseID:           wait.PriorRunLeaseID,
					SuspendCheckpointID:        wait.SuspendCheckpointID,
					ChildWriterGeneration:      wait.ChildWriterGeneration,
				},
			)
		}
		if err == nil {
			_, err = store.RequestHandoffFailureRuntimeClose(
				ctx,
				db.RequestHandoffFailureRuntimeCloseParams{
					FailedAt: completedAt, RuntimeInstanceID: authority.runtime.ID,
					OrgID: authority.run.OrgID, ProjectID: authority.run.ProjectID,
					EnvironmentID: authority.run.EnvironmentID, WorkspaceID: authority.workspace.ID,
					WorkerInstanceID:       authority.runtime.WorkerInstanceID,
					WorkerEpoch:            authority.runtime.WorkerEpoch,
					WorkspaceMountID:       authority.workspaceMount.ID,
					MountFencingGeneration: authority.workspaceMount.FencingGeneration,
				},
			)
		}
	}
	if err != nil {
		return staleTaskCompletion(err)
	}
	return enqueueRunResume(ctx, store, authority.parentRun.WorkspaceID, completed, completedAt)
}

func cascadeSameWorkspaceChildFailure(
	ctx context.Context,
	store db.Querier,
	authority runLeaseClaimAuthority,
	failedAt pgtype.Timestamptz,
) (db.RunWait, error) {
	ancestors := authority.handoffAncestors
	if len(ancestors) == 0 {
		return db.RunWait{}, errStaleTaskCompletion
	}
	errorObject, err := json.Marshal(map[string]any{
		"code":      "same_workspace_handoff_runtime_lost",
		"message":   "nested same-Workspace handoff runtime was discarded after descendant failure",
		"retryable": false,
	})
	if err != nil {
		return db.RunWait{}, err
	}
	reason := pgvalue.Text("same_workspace_handoff_runtime_lost")
	innerWait := authority.enclosingWait
	innerParent := authority.parentRun
	innerAttempt := authority.parentAttempt

	failParent := func() error {
		if _, err := store.FailNestedSameWorkspaceWait(
			ctx,
			db.FailNestedSameWorkspaceWaitParams{
				Error: errorObject, FailedAt: failedAt, ReasonCode: reason,
				RunWaitID: innerWait.ID, EnvironmentID: innerWait.EnvironmentID,
				RunID: innerParent.ID, AttemptNumber: innerAttempt.Number,
				WorkspaceID: authority.workspace.ID, ChildRunID: innerWait.ChildRunID,
				HandoffRuntimeInstanceID: innerWait.HandoffRuntimeInstanceID,
				HandoffWorkspaceMountID:  innerWait.HandoffWorkspaceMountID,
				HandoffMountGeneration:   innerWait.HandoffMountGeneration,
				OwnershipGeneration:      innerWait.OwnershipGeneration,
			},
		); err != nil {
			return staleTaskCompletion(err)
		}
		if _, err := store.FailNestedSameWorkspaceAttempt(
			ctx,
			db.FailNestedSameWorkspaceAttemptParams{
				Error: errorObject, FailedAt: failedAt, RunID: innerParent.ID,
				AttemptNumber: innerAttempt.Number, WorkspaceID: authority.workspace.ID,
			},
		); err != nil {
			return staleTaskCompletion(err)
		}
		if _, err := store.FailNestedSameWorkspaceRun(
			ctx,
			db.FailNestedSameWorkspaceRunParams{
				Error: errorObject, FailedAt: failedAt, RunID: innerParent.ID,
				EnvironmentID: innerParent.EnvironmentID, WorkspaceID: authority.workspace.ID,
				AttemptNumber: innerAttempt.Number,
			},
		); err != nil {
			return staleTaskCompletion(err)
		}
		payload, err := json.Marshal(struct {
			Reason string `json:"reason"`
		}{Reason: reason.String})
		if err != nil {
			return err
		}
		if _, err := store.AppendRunEvent(ctx, db.AppendRunEventParams{
			OrgID: innerParent.OrgID, RunID: innerParent.ID,
			Kind: api.RunEventKindFailed, Payload: payload,
		}); err != nil {
			return fmt.Errorf("append nested handoff failure event: %w", err)
		}
		return nil
	}

	if err := failParent(); err != nil {
		return db.RunWait{}, err
	}
	for index := len(ancestors) - 1; index > 0; index-- {
		row := ancestors[index]
		innerWait = row.RunWait
		innerParent = row.Run
		innerAttempt = row.RunAttempt
		if err := failParent(); err != nil {
			return db.RunWait{}, err
		}
	}

	root := ancestors[0]
	return store.CompleteSameWorkspaceChildFailure(
		ctx,
		db.CompleteSameWorkspaceChildFailureParams{
			CompletedAt: failedAt, ConditionState: db.WaitStateFailed,
			ConditionError: errorObject, ReasonCode: reason,
			RunWaitID: root.RunWait.ID, EnvironmentID: root.RunWait.EnvironmentID,
			ParentRunID: root.Run.ID, WorkspaceID: authority.workspace.ID,
			ParentAttemptNumber:        root.RunAttempt.Number,
			ChildRunID:                 root.RunWait.ChildRunID,
			ExpectedParentStateVersion: root.RunWait.ExpectedRunStateVersion,
			ParentRunLeaseID:           root.RunWait.PriorRunLeaseID,
			SuspendCheckpointID:        root.RunWait.SuspendCheckpointID,
			ChildWriterGeneration:      root.RunWait.ChildWriterGeneration,
		},
	)
}

func enqueueRunResume(
	ctx context.Context,
	store db.Querier,
	workspaceID pgtype.UUID,
	wait db.RunWait,
	availableAt pgtype.Timestamptz,
) error {
	payload, err := json.Marshal(map[string]any{
		"environmentId":        pgvalue.UUIDString(wait.EnvironmentID),
		"runId":                pgvalue.UUIDString(wait.RunID),
		"runWaitId":            pgvalue.UUIDString(wait.ID),
		"resumeRequestVersion": wait.ResumeRequestVersion,
	})
	if err != nil {
		return err
	}
	if _, err := store.CreateOutboxMessage(ctx, db.CreateOutboxMessageParams{
		ID: pgvalue.UUID(uuid.Must(uuid.NewV7())), Lane: "control", Topic: "run.resume",
		PartitionKey: pgvalue.UUIDString(workspaceID), Payload: payload, AvailableAt: availableAt,
	}); err != nil {
		return fmt.Errorf("enqueue run resume: %w", err)
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
	completedAt pgtype.Timestamptz,
) error {
	wait := authority.enclosingWait
	result, err := childTaskResult(child)
	if err != nil {
		return err
	}
	switch wait.SuspensionState {
	case db.RunWaitStateHot:
		_, err = store.CompleteHotChildRunWait(ctx, db.CompleteHotChildRunWaitParams{
			RunID: wait.RunID, EnvironmentID: wait.EnvironmentID,
			ExpectedRunStateVersion: wait.ExpectedRunStateVersion,
			AttemptNumber:           wait.AttemptNumber, CurrentRunLeaseID: wait.CurrentRunLeaseID,
			ConditionResult: result, ID: wait.ID, ChildRunID: child.ID,
		})
	case db.RunWaitStateCheckpointing:
		_, err = store.CompleteCheckpointingChildRunWait(
			ctx,
			db.CompleteCheckpointingChildRunWaitParams{
				ConditionResult: result, ID: wait.ID, RunID: wait.RunID,
				ChildRunID: child.ID, ExpectedRunStateVersion: wait.ExpectedRunStateVersion,
				CurrentRunLeaseID: wait.CurrentRunLeaseID,
			},
		)
	case db.RunWaitStateParked:
		var completed db.RunWait
		completed, err = store.CompleteParkedChildRunWait(
			ctx,
			db.CompleteParkedChildRunWaitParams{
				RunID: wait.RunID, EnvironmentID: wait.EnvironmentID,
				ExpectedRunStateVersion: wait.ExpectedRunStateVersion,
				AttemptNumber:           wait.AttemptNumber, ConditionResult: result,
				ID: wait.ID, ChildRunID: child.ID, PriorRunLeaseID: wait.PriorRunLeaseID,
				SuspendCheckpointID: wait.SuspendCheckpointID,
			},
		)
		if err == nil {
			payload, marshalErr := json.Marshal(map[string]any{
				"environmentId":        pgvalue.UUIDString(wait.EnvironmentID),
				"runId":                pgvalue.UUIDString(wait.RunID),
				"runWaitId":            pgvalue.UUIDString(wait.ID),
				"resumeRequestVersion": completed.ResumeRequestVersion,
			})
			if marshalErr != nil {
				return marshalErr
			}
			_, err = store.CreateOutboxMessage(ctx, db.CreateOutboxMessageParams{
				ID: pgvalue.UUID(uuid.Must(uuid.NewV7())), Lane: "control", Topic: "run.resume",
				PartitionKey: pgvalue.UUIDString(authority.parentRun.WorkspaceID),
				Payload:      payload, AvailableAt: completedAt,
			})
		}
	default:
		return errStaleTaskCompletion
	}
	if err != nil {
		return staleTaskCompletion(err)
	}
	return nil
}

func (s *Server) verifyTaskWorkspaceCapture(ctx context.Context, capture parsedTaskWorkspaceCapture) (parsedTaskWorkspaceCapture, error) {
	if s.cas == nil {
		return parsedTaskWorkspaceCapture{}, errors.New("workspace CAS is not configured")
	}
	artifact := capture.artifact
	object, err := s.cas.Stat(ctx, artifact.Digest)
	if err != nil {
		return parsedTaskWorkspaceCapture{}, fmt.Errorf("task workspace artifact is missing from CAS: %w", err)
	}
	if object.Digest != artifact.Digest || object.SizeBytes != artifact.SizeBytes ||
		object.MediaType != artifact.MediaType {
		return parsedTaskWorkspaceCapture{}, errors.New("task workspace artifact does not match CAS authority")
	}
	body, err := s.cas.Get(ctx, artifact.Digest)
	if err != nil {
		return parsedTaskWorkspaceCapture{}, fmt.Errorf("open task workspace artifact: %w", err)
	}
	defer body.Close()
	if err := workspace.VerifyArtifact(body, workspace.WorkspaceArtifact{
		Digest: artifact.Digest, MediaType: artifact.MediaType, Encoding: artifact.Encoding,
		SizeBytes: artifact.SizeBytes, EntryCount: int(artifact.EntryCount),
	}, capture.tree); err != nil {
		return parsedTaskWorkspaceCapture{}, fmt.Errorf("verify task workspace artifact: %w", err)
	}
	return capture, nil
}

func staleTaskCompletion(err error) error {
	if err == nil || errors.Is(err, pgx.ErrNoRows) || errors.Is(err, errStaleRunLeaseClaim) {
		return errStaleTaskCompletion
	}
	return err
}
