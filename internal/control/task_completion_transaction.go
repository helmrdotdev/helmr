package control

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
	"github.com/helmrdotdev/helmr/internal/publicid"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/helmrdotdev/helmr/internal/workspace"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

var errStaleTaskCompletion = errors.New("Task completion receipt is stale")

type taskCompletionReplayStore interface {
	GetTaskCompletionReplay(context.Context, db.GetTaskCompletionReplayParams) (pgtype.Text, error)
}

func (s *Server) completeTask(
	ctx context.Context,
	worker workerActor,
	request api.WorkerCompleteTaskRequest,
	completion parsedTaskCompletion,
) error {
	replayed, err := taskCompletionWasReplayed(ctx, s.db, worker, request, completion)
	if err != nil || replayed {
		return err
	}
	if request.Lease.WorkerEpoch != worker.WorkerEpoch ||
		request.Lease.WorkerProtocolVersion != worker.ProtocolVersion {
		return errStaleTaskCompletion
	}
	if completion.capture != nil {
		verified, err := s.verifyTaskWorkspaceCapture(ctx, *completion.capture)
		if err != nil {
			return taskCompletionReplayAfterError(ctx, s.db, worker, request, completion, err)
		}
		completion.capture = &verified
	}

	err = s.inTx(ctx, func(work *txWork) error {
		replayed, err := taskCompletionWasReplayed(ctx, work.q, worker, request, completion)
		if err != nil || replayed {
			return err
		}
		if request.Lease.WorkerEpoch != worker.WorkerEpoch ||
			request.Lease.WorkerProtocolVersion != worker.ProtocolVersion {
			return errStaleTaskCompletion
		}

		secrets, err := secret.LockAttemptDelivery(
			ctx,
			work.q,
			pgvalue.UUID(completion.lease.runID),
			request.Lease.AttemptNumber,
			pgvalue.UUID(completion.lease.workspaceID),
		)
		if err != nil {
			return fmt.Errorf("lock Task completion Secret authority: %w", err)
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
		authority, err := lockLiveRunLeaseAuthority(
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
		if err := validateTaskCompletionAuthority(ctx, work.q, request, completion, authority); err != nil {
			return err
		}
		if completion.rollback != nil {
			if err := validateTaskWorkspaceRollback(ctx, work.q, authority, *completion.rollback); err != nil {
				return err
			}
		}

		completedAt, err := work.q.GetTaskCompletionTime(ctx)
		if err != nil || !completedAt.Valid {
			if err == nil {
				err = errors.New("database Task completion time is unavailable")
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
			versionID, err = recordTaskWorkspaceVersion(
				ctx,
				work.q,
				worker,
				authority,
				*completion.capture,
				completedAt,
			)
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
			return scheduleTaskRetry(ctx, work.q, authority, secrets, completedAt, retryAt)
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
	request api.WorkerCompleteTaskRequest,
	completion parsedTaskCompletion,
) (bool, error) {
	fingerprint, err := store.GetTaskCompletionReplay(ctx, db.GetTaskCompletionReplayParams{
		RunLeaseID: pgvalue.UUID(completion.lease.leaseID), RunID: pgvalue.UUID(completion.lease.runID),
		WorkspaceID: pgvalue.UUID(completion.lease.workspaceID), AttemptNumber: request.Lease.AttemptNumber,
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
	request api.WorkerCompleteTaskRequest,
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
		return errors.Join(operationErr, fmt.Errorf("check Task completion replay: %w", replayErr))
	}
	return operationErr
}

func validateTaskCompletionAuthority(
	ctx context.Context,
	store db.Querier,
	request api.WorkerCompleteTaskRequest,
	completion parsedTaskCompletion,
	authority runLeaseClaimAuthority,
) error {
	if authority.run.EntrypointKind != "task" || authority.run.ActorID.Valid ||
		authority.runLease.State != db.RunLeaseStateFinalizing ||
		!authority.attempt.EntrypointEnteredAt.Valid ||
		authority.run.ActiveStartedAt.Valid ||
		!authority.runLease.FinalizationOperationID.Valid ||
		!authority.runLease.FinalizationKind.Valid ||
		!authority.runLease.FinalizationStartedAt.Valid ||
		!authority.runLease.FinalizationRequestFingerprint.Valid ||
		authority.attempt.BaseWorkspaceVersionID != authority.run.BaseWorkspaceVersionID ||
		!authority.workspace.HeadVersionID.Valid ||
		authority.workspace.HeadVersionID != authority.run.BaseWorkspaceVersionID ||
		authority.workspace.OwnerRunID != authority.run.ID ||
		authority.workspace.OwnerActorID.Valid ||
		(authority.run.ParentRunID.Valid != authority.run.ParentOwnsLifecycle.Valid) ||
		(authority.run.ParentRunID.Valid && authority.run.ParentOwnsLifecycle.Bool) {
		return errStaleTaskCompletion
	}
	if completion.kind != taskCompletionSucceeded &&
		(completion.rollback == nil || pgvalue.UUID(completion.rollback.baseID) != authority.run.BaseWorkspaceVersionID) {
		return errStaleTaskCompletion
	}
	var finalization workspace.FinalizationRequest
	wantKind := string(api.WorkerRunFinalizationReset)
	if completion.capture != nil {
		finalization = completion.capture.receipt
		wantKind = string(api.WorkerRunFinalizationCapture)
	} else {
		finalization = completion.rollback.receipt
	}
	operationID, err := uuid.Parse(finalization.OperationID)
	if err != nil ||
		authority.runLease.FinalizationOperationID != pgvalue.UUID(operationID) ||
		authority.runLease.FinalizationKind.String != wantKind {
		return errStaleTaskCompletion
	}
	receipt, err := projectRunLeaseReceipt(runLeaseProjectionAuthority{
		run: authority.run, attempt: authority.attempt, runtime: authority.runtime,
		networkSlot: authority.networkSlot, runLease: authority.runLease,
		workspace: authority.workspace, workspaceMount: authority.workspaceMount,
		workspaceLease: authority.workspaceLease,
	})
	if err != nil {
		return err
	}
	if !equalRunLeaseReceipt(receipt, request.Lease) {
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

func validateTaskWorkspaceRollback(
	ctx context.Context,
	store taskWorkspaceRollbackStore,
	authority runLeaseClaimAuthority,
	rollback parsedTaskWorkspaceRollback,
) error {
	version, err := store.GetWorkspaceVersion(ctx, db.GetWorkspaceVersionParams{
		OrgID: authority.run.OrgID, ProjectID: authority.run.ProjectID,
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
			OrgID: version.OrgID, ProjectID: version.ProjectID,
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
	GetWorkspaceVersion(context.Context, db.GetWorkspaceVersionParams) (db.WorkspaceVersion, error)
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
		return time.Time{}, false, fmt.Errorf("parse pinned Task retry policy: %w", err)
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
		return pgtype.UUID{}, fmt.Errorf("record Task Workspace CAS object: %w", err)
	}
	artifactRow, err := store.CreateArtifact(ctx, db.CreateArtifactParams{
		ID: pgvalue.UUID(uuid.Must(uuid.NewV7())), OrgID: authority.run.OrgID,
		ProjectID: authority.run.ProjectID, EnvironmentID: authority.run.EnvironmentID,
		Digest: artifact.Digest, Kind: db.ArtifactKindWorkspaceVersion,
		SizeBytes: artifact.SizeBytes, MediaType: artifact.MediaType,
		CreatedByWorkerInstanceID: pgvalue.UUID(worker.WorkerInstanceID),
	})
	if err != nil {
		return pgtype.UUID{}, fmt.Errorf("record Task Workspace Artifact: %w", err)
	}
	publicID, err := newPublicID(publicid.WorkspaceVersion)
	if err != nil {
		return pgtype.UUID{}, err
	}
	version, err := store.PublishTaskWorkspaceVersion(ctx, db.PublishTaskWorkspaceVersionParams{
		ID: pgvalue.UUID(uuid.Must(uuid.NewV7())), PublicID: publicID,
		OrgID: authority.run.OrgID, ProjectID: authority.run.ProjectID,
		EnvironmentID: authority.run.EnvironmentID, WorkspaceID: authority.workspace.ID,
		ParentVersionID: authority.workspaceLease.BaseVersionID, ArtifactID: artifactRow.ID,
		ContentDigest: capture.tree.Digest, SizeBytes: capture.tree.SizeBytes, EntryCount: int32(capture.tree.EntryCount),
		SourceWorkspaceLeaseID: authority.workspaceLease.ID,
		OwnershipGeneration:    authority.workspace.OwnershipGeneration,
		WriterGeneration:       authority.workspace.WriterGeneration, PublishedAt: completedAt,
	})
	if err != nil {
		return pgtype.UUID{}, fmt.Errorf("publish Task Workspace version: %w", err)
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
		return errors.New("Task completion outcome is unsupported")
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
	for _, binding := range secrets {
		if !binding.Secret.CurrentVersionID.Valid || binding.Secret.State != "active" {
			return secret.ErrDeliveryUnavailable
		}
		if _, err := store.CreateSecretResolution(ctx, db.CreateSecretResolutionParams{
			ID: pgvalue.UUID(uuid.Must(uuid.NewV7())), WorkspaceID: authority.workspace.ID,
			RunID: authority.run.ID, AttemptNumber: pgtype.Int4{Int32: nextAttempt, Valid: true},
			PlacementKind: binding.PlacementKind, PlacementTarget: binding.PlacementTarget,
			SecretID: binding.Secret.ID, SecretVersionID: binding.Secret.CurrentVersionID,
			RevocationGeneration: binding.Secret.RevocationGeneration,
		}); err != nil {
			return fmt.Errorf("record retry Secret resolution: %w", err)
		}
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
		return errors.New("Task completion outcome is unsupported")
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
		return fmt.Errorf("append Task terminal event: %w", err)
	}
	return nil
}

func (s *Server) verifyTaskWorkspaceCapture(ctx context.Context, capture parsedTaskWorkspaceCapture) (parsedTaskWorkspaceCapture, error) {
	if s.cas == nil {
		return parsedTaskWorkspaceCapture{}, errors.New("Workspace CAS is not configured")
	}
	artifact := capture.artifact
	object, err := s.cas.Stat(ctx, artifact.Digest)
	if err != nil {
		return parsedTaskWorkspaceCapture{}, fmt.Errorf("Task Workspace Artifact is missing from CAS: %w", err)
	}
	if object.Digest != artifact.Digest || object.SizeBytes != artifact.SizeBytes ||
		object.MediaType != artifact.MediaType {
		return parsedTaskWorkspaceCapture{}, errors.New("Task Workspace Artifact does not match CAS authority")
	}
	body, err := s.cas.Get(ctx, artifact.Digest)
	if err != nil {
		return parsedTaskWorkspaceCapture{}, fmt.Errorf("open Task Workspace Artifact: %w", err)
	}
	defer body.Close()
	if err := workspace.VerifyArtifact(body, workspace.WorkspaceArtifact{
		Digest: artifact.Digest, MediaType: artifact.MediaType, Encoding: artifact.Encoding,
		SizeBytes: artifact.SizeBytes, EntryCount: int(artifact.EntryCount),
	}, capture.tree); err != nil {
		return parsedTaskWorkspaceCapture{}, fmt.Errorf("verify Task Workspace Artifact: %w", err)
	}
	return capture, nil
}

func staleTaskCompletion(err error) error {
	if err == nil || errors.Is(err, pgx.ErrNoRows) || errors.Is(err, errStaleRunLeaseClaim) {
		return errStaleTaskCompletion
	}
	return err
}
