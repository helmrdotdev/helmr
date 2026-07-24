package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/deployment"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/publicid"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

type parsedCheckpointReady struct {
	lease          parsedRunLeaseReceipt
	waitID         uuid.UUID
	checkpointID   uuid.UUID
	capture        parsedTaskWorkspaceCapture
	manifest       []byte
	fingerprint    string
	artifacts      []checkpointArtifactProof
	substrateID    uuid.UUID
	hasSubstrate   bool
	requestVersion int64
	attemptNumber  int32
}

type parsedCheckpointFailed struct {
	lease          parsedRunLeaseReceipt
	waitID         uuid.UUID
	checkpointID   uuid.UUID
	requestVersion int64
	attemptNumber  int32
	errorPayload   []byte
	fingerprint    string
}

type checkpointArtifactProof struct {
	role     db.RunCheckpointArtifactRole
	ordinal  int32
	kind     db.ArtifactKind
	artifact api.WorkerCheckpointArtifact
}

func (s *Server) workerMarkCheckpointReady(w http.ResponseWriter, r *http.Request) {
	var request api.WorkerCheckpointReadyRequest
	if err := decodeClosedWorkerRequest(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid worker checkpoint-ready JSON: %w", err)))
		return
	}
	parsed, normalized, err := parseCheckpointReadyRequest(request)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	worker := workerFromContext(r.Context())
	if normalized.Lease.WorkerGroupID != worker.WorkerGroupID || parsed.lease.workerInstanceID != worker.WorkerInstanceID ||
		normalized.Lease.WorkerEpoch != worker.WorkerEpoch || normalized.Lease.WorkerProtocolVersion != worker.ProtocolVersion {
		writeError(w, forbidden(errors.New("worker checkpoint-ready receipt belongs to another worker epoch")))
		return
	}
	if response, replayed, err := s.checkpointReadyReplay(r.Context(), parsed); err != nil {
		writeError(w, err)
		return
	} else if replayed {
		writeJSON(w, http.StatusOK, response)
		return
	}
	verified, err := s.verifyTaskWorkspaceCapture(r.Context(), parsed.capture)
	if err != nil {
		if response, replayed, replayErr := s.checkpointReadyReplay(r.Context(), parsed); replayErr == nil && replayed {
			writeJSON(w, http.StatusOK, response)
			return
		}
		writeError(w, badRequest(fmt.Errorf("verify checkpoint Workspace capture: %w", err)))
		return
	}
	parsed.capture = verified
	var substrateArtifact *api.CASObject
	if normalized.Manifest.RuntimeState.RuntimeSubstrate != nil {
		substrateArtifact = &normalized.Manifest.RuntimeState.RuntimeSubstrate.Artifact
	}
	if err := s.verifyCheckpointRuntimeArtifacts(r.Context(), parsed.artifacts, substrateArtifact); err != nil {
		if response, replayed, replayErr := s.checkpointReadyReplay(r.Context(), parsed); replayErr == nil && replayed {
			writeJSON(w, http.StatusOK, response)
			return
		}
		writeError(w, badRequest(err))
		return
	}
	response, err := s.commitCheckpointReady(r.Context(), worker, normalized, parsed)
	if err != nil {
		if replay, replayed, replayErr := s.checkpointReadyReplay(r.Context(), parsed); replayErr == nil && replayed {
			writeJSON(w, http.StatusOK, replay)
			return
		}
		if errors.Is(err, errStaleRunLeaseClaim) || errors.Is(err, pgx.ErrNoRows) {
			writeError(w, conflict(errors.New("worker checkpoint-ready receipt is stale")))
			return
		}
		s.log.Error("commit worker checkpoint-ready failed", "run_id", request.Lease.RunID, "error", err)
		writeError(w, errors.New("commit worker checkpoint-ready"))
		return
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) workerMarkCheckpointFailed(w http.ResponseWriter, r *http.Request) {
	var request api.WorkerCheckpointFailedRequest
	if err := decodeClosedWorkerRequest(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid worker checkpoint-failed JSON: %w", err)))
		return
	}
	parsed, normalized, err := parseCheckpointFailedRequest(request)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	worker := workerFromContext(r.Context())
	if normalized.Lease.WorkerGroupID != worker.WorkerGroupID || parsed.lease.workerInstanceID != worker.WorkerInstanceID ||
		normalized.Lease.WorkerEpoch != worker.WorkerEpoch || normalized.Lease.WorkerProtocolVersion != worker.ProtocolVersion {
		writeError(w, forbidden(errors.New("worker checkpoint-failed receipt belongs to another worker epoch")))
		return
	}
	if response, replayed, replayErr := s.checkpointFailedReplay(r.Context(), parsed); replayErr != nil {
		writeError(w, replayErr)
		return
	} else if replayed {
		writeJSON(w, http.StatusOK, response)
		return
	}
	err = s.inTx(r.Context(), func(work *txWork) error {
		secrets, err := secret.LockAttemptDelivery(r.Context(), work.q, pgvalue.UUID(parsed.lease.runID), normalized.Lease.AttemptNumber, pgvalue.UUID(parsed.lease.workspaceID))
		if err != nil {
			return fmt.Errorf("lock checkpoint-failed Secret authority: %w", err)
		}
		locators, err := work.q.GetLiveRunLeaseLocators(r.Context(), db.GetLiveRunLeaseLocatorsParams{
			ID: pgvalue.UUID(parsed.lease.leaseID), LeaseSequence: normalized.Lease.LeaseSequence,
			WorkerGroupID: worker.WorkerGroupID, WorkerInstanceID: pgvalue.UUID(worker.WorkerInstanceID),
			WorkerEpoch: worker.WorkerEpoch, WorkerProtocolVersion: worker.ProtocolVersion,
		})
		if err != nil {
			return staleRunLeaseClaim(err)
		}
		owner, err := lockRunFinalizationOwner(r.Context(), work.q, locators)
		if err != nil {
			return err
		}
		authority, err := lockRenewableRunLeaseAuthority(
			r.Context(), work.q, worker, pgvalue.UUID(parsed.lease.leaseID), normalized.Lease.LeaseSequence, locators,
		)
		if err != nil {
			return err
		}
		authority.actor = owner.actor
		if authority.run.Status != db.RunStatusWaiting || authority.run.ParentRunID.Valid ||
			authority.runLease.State != db.RunLeaseStateCheckpointing {
			return errStaleRunLeaseClaim
		}
		current, err := projectRunLeaseReceipt(runLeaseProjectionAuthority{
			run: authority.run, attempt: authority.attempt, runtime: authority.runtime,
			networkSlot: authority.networkSlot, runLease: authority.runLease, workspace: authority.workspace,
			workspaceMount: authority.workspaceMount, workspaceLease: authority.workspaceLease,
		})
		if err != nil || !equalCurrentOrPreviousRunLeaseReceipt(current, normalized.Lease, authority.runLease.PreviousExpiresAt) {
			return errStaleRunLeaseClaim
		}
		wait, err := work.q.LockRunLeaseClaimWait(r.Context(), db.LockRunLeaseClaimWaitParams{
			ID: pgvalue.UUID(parsed.waitID), EnvironmentID: authority.run.EnvironmentID, RunID: authority.run.ID,
			AttemptNumber: authority.attempt.Number, WorkspaceID: authority.workspace.ID,
			CurrentRunLeaseID: authority.runLease.ID,
		})
		if err != nil || wait.SuspensionState != db.RunWaitStateCheckpointing ||
			wait.CheckpointRequestVersion != parsed.requestVersion || wait.SuspendCheckpointID != pgvalue.UUID(parsed.checkpointID) {
			return staleRunLeaseClaim(err)
		}
		if err := validateRootRunWaitActorCursor(authority, wait); err != nil {
			return err
		}
		checkpoint, err := work.q.LockCreatingRunCheckpoint(r.Context(), db.LockCreatingRunCheckpointParams{
			ID: pgvalue.UUID(parsed.checkpointID), RunID: authority.run.ID, AttemptNumber: authority.attempt.Number,
			RunWaitID: wait.ID, SourceRunLeaseID: authority.runLease.ID,
			SourceWorkspaceLeaseID: authority.workspaceLease.ID, WorkspaceID: authority.workspace.ID,
		})
		if err != nil || checkpoint.ActorSpeculativeInputSequence != wait.ActorSpeculativeInputSequence {
			return staleRunLeaseClaim(err)
		}
		if authority.run.EntrypointKind == "actor" {
			return failCheckpointActorAttempt(r.Context(), work.q, worker, authority, wait, parsed, secrets)
		}
		return failCheckpointTaskAttempt(r.Context(), work.q, worker, authority, wait, parsed, secrets)
	})
	if err != nil {
		if response, replayed, replayErr := s.checkpointFailedReplay(r.Context(), parsed); replayErr == nil && replayed {
			writeJSON(w, http.StatusOK, response)
			return
		}
	}
	if errors.Is(err, errStaleRunLeaseClaim) || errors.Is(err, pgx.ErrNoRows) {
		writeError(w, conflict(errors.New("worker checkpoint-failed receipt is stale")))
		return
	}
	if err != nil {
		writeError(w, errors.New("commit worker checkpoint-failed"))
		return
	}
	writeJSON(w, http.StatusOK, api.WorkerCheckpointResponse{
		RunID: normalized.Lease.RunID, RunWaitID: normalized.RunWaitID, CheckpointID: normalized.CheckpointID,
	})
}

func failCheckpointTaskAttempt(
	ctx context.Context,
	store db.Querier,
	worker workerActor,
	authority runLeaseClaimAuthority,
	wait db.RunWait,
	failed parsedCheckpointFailed,
	secrets []secret.DeliveryEnvelope,
) error {
	failedAt, err := store.GetTaskCompletionTime(ctx)
	if err != nil || !failedAt.Valid {
		if err == nil {
			err = errors.New("database checkpoint failure time is unavailable")
		}
		return err
	}
	activeElapsed, err := store.CloseRunActiveIntervalForCheckpointFailure(ctx, db.CloseRunActiveIntervalForCheckpointFailureParams{
		FailedAt: failedAt, ID: authority.run.ID, OrgID: authority.run.OrgID,
		ProjectID: authority.run.ProjectID, EnvironmentID: authority.run.EnvironmentID,
		WorkspaceID: authority.workspace.ID, AttemptNumber: authority.attempt.Number,
		RunLeaseID: authority.runLease.ID,
	})
	if err != nil {
		return staleRunLeaseClaim(err)
	}

	reason := "checkpoint_failed"
	var retryAt time.Time
	var retry bool
	if activeElapsed >= authority.run.MaxActiveDurationMs {
		reason = "max_active_duration_exceeded"
	} else {
		retryAt, retry, err = taskCompletionRetryAt(
			authority.run,
			authority.attempt,
			parsedTaskCompletion{kind: taskCompletionFailed},
			failedAt.Time,
		)
		if err != nil {
			return err
		}
	}

	if _, err := store.InvalidateFailedRunCheckpoint(ctx, db.InvalidateFailedRunCheckpointParams{
		FailedAt: failedAt, FailedRequestFingerprint: pgvalue.Text(failed.fingerprint),
		CheckpointID: pgvalue.UUID(failed.checkpointID), RunID: authority.run.ID,
		AttemptNumber: authority.attempt.Number, RunWaitID: wait.ID,
		RunLeaseID: authority.runLease.ID, WorkspaceID: authority.workspace.ID,
	}); err != nil {
		return staleRunLeaseClaim(err)
	}
	if _, err := store.FailCheckpointRunLease(ctx, db.FailCheckpointRunLeaseParams{
		FailedAt: failedAt, Error: failed.errorPayload,
		FailedRequestFingerprint: pgvalue.Text(failed.fingerprint),
		RunLeaseID:               authority.runLease.ID, RunID: authority.run.ID,
		WorkspaceID: authority.workspace.ID, AttemptNumber: authority.attempt.Number,
		LeaseSequence: authority.runLease.LeaseSequence,
	}); err != nil {
		return staleRunLeaseClaim(err)
	}
	if _, err := store.CompleteTaskAttempt(ctx, db.CompleteTaskAttemptParams{
		TerminalOutcome: pgvalue.Text("failed"), ReasonCode: pgvalue.Text(reason),
		Error: failed.errorPayload, CompletedAt: failedAt, RunID: authority.run.ID,
		Number: authority.attempt.Number, WorkspaceID: authority.workspace.ID,
	}); err != nil {
		return staleRunLeaseClaim(err)
	}
	if _, err := store.FailCheckpointRunWait(ctx, db.FailCheckpointRunWaitParams{
		CheckpointRequestVersion: failed.requestVersion, FailedAt: failedAt,
		Error: failed.errorPayload, RunWaitID: wait.ID, RunID: authority.run.ID,
		WorkspaceID: authority.workspace.ID, AttemptNumber: authority.attempt.Number,
		RunLeaseID: authority.runLease.ID, CheckpointID: pgvalue.UUID(failed.checkpointID),
	}); err != nil {
		return staleRunLeaseClaim(err)
	}
	if _, err := store.ReleaseTaskWorkspaceLease(ctx, db.ReleaseTaskWorkspaceLeaseParams{
		CompletedAt: failedAt, ID: authority.workspaceLease.ID,
		WorkspaceID: authority.workspace.ID, WorkspaceMountID: authority.workspaceMount.ID,
		RuntimeInstanceID: authority.runtime.ID, OwnerRunLeaseID: authority.runLease.ID,
		BaseVersionID:          authority.workspaceLease.BaseVersionID,
		OwnershipGeneration:    authority.workspace.OwnershipGeneration,
		WriterGeneration:       authority.workspace.WriterGeneration,
		MountFencingGeneration: authority.workspaceMount.FencingGeneration,
	}); err != nil {
		return staleRunLeaseClaim(err)
	}
	if _, err := store.RequestCheckpointFailureRuntimeClose(ctx, db.RequestCheckpointFailureRuntimeCloseParams{
		FailedAt: failedAt, WorkspaceMountID: authority.workspaceMount.ID,
		OrgID: authority.run.OrgID, ProjectID: authority.run.ProjectID,
		EnvironmentID: authority.run.EnvironmentID, WorkspaceID: authority.workspace.ID,
		WorkerInstanceID: pgvalue.UUID(worker.WorkerInstanceID), WorkerEpoch: worker.WorkerEpoch,
		MountFencingGeneration: authority.workspaceMount.FencingGeneration,
		RuntimeInstanceID:      authority.runtime.ID,
	}); err != nil {
		return staleRunLeaseClaim(err)
	}
	if retry {
		return scheduleCheckpointFailureRetry(ctx, store, authority, secrets, failedAt, retryAt)
	}
	return finishCheckpointFailedTask(ctx, store, authority, failedAt, failed.errorPayload, reason)
}

func failCheckpointActorAttempt(
	ctx context.Context,
	store db.Querier,
	worker workerActor,
	authority runLeaseClaimAuthority,
	wait db.RunWait,
	failed parsedCheckpointFailed,
	secrets []secret.DeliveryEnvelope,
) error {
	failedAt, err := store.GetTaskCompletionTime(ctx)
	if err != nil || !failedAt.Valid {
		if err == nil {
			err = errors.New("database Actor checkpoint failure time is unavailable")
		}
		return err
	}
	activeElapsed, err := store.CloseRunActiveIntervalForCheckpointFailure(ctx, db.CloseRunActiveIntervalForCheckpointFailureParams{
		FailedAt: failedAt, ID: authority.run.ID, OrgID: authority.run.OrgID,
		ProjectID: authority.run.ProjectID, EnvironmentID: authority.run.EnvironmentID,
		WorkspaceID: authority.workspace.ID, AttemptNumber: authority.attempt.Number,
		RunLeaseID: authority.runLease.ID,
	})
	if err != nil {
		return staleRunLeaseClaim(err)
	}

	decision, err := decideActorCheckpointFailure(authority, failedAt.Time, activeElapsed)
	if err != nil {
		return err
	}

	if _, err := store.InvalidateFailedRunCheckpoint(ctx, db.InvalidateFailedRunCheckpointParams{
		FailedAt: failedAt, FailedRequestFingerprint: pgvalue.Text(failed.fingerprint),
		CheckpointID: pgvalue.UUID(failed.checkpointID), RunID: authority.run.ID,
		AttemptNumber: authority.attempt.Number, RunWaitID: wait.ID,
		RunLeaseID: authority.runLease.ID, WorkspaceID: authority.workspace.ID,
	}); err != nil {
		return staleRunLeaseClaim(err)
	}
	if _, err := store.FailCheckpointRunLease(ctx, db.FailCheckpointRunLeaseParams{
		FailedAt: failedAt, Error: failed.errorPayload,
		FailedRequestFingerprint: pgvalue.Text(failed.fingerprint),
		RunLeaseID:               authority.runLease.ID, RunID: authority.run.ID,
		WorkspaceID: authority.workspace.ID, AttemptNumber: authority.attempt.Number,
		LeaseSequence: authority.runLease.LeaseSequence,
	}); err != nil {
		return staleRunLeaseClaim(err)
	}
	if _, err := store.CompleteActorAttempt(ctx, db.CompleteActorAttemptParams{
		TerminalActorInputSequence: pgtype.Int8{}, TerminalOutcome: pgvalue.Text("failed"),
		ReasonCode: pgvalue.Text(decision.reason), Error: failed.errorPayload, CompletedAt: failedAt,
		RunID: authority.run.ID, Number: authority.attempt.Number, WorkspaceID: authority.workspace.ID,
	}); err != nil {
		return staleRunLeaseClaim(err)
	}
	if _, err := store.FailCheckpointRunWait(ctx, db.FailCheckpointRunWaitParams{
		CheckpointRequestVersion: failed.requestVersion, FailedAt: failedAt,
		Error: failed.errorPayload, RunWaitID: wait.ID, RunID: authority.run.ID,
		WorkspaceID: authority.workspace.ID, AttemptNumber: authority.attempt.Number,
		RunLeaseID: authority.runLease.ID, CheckpointID: pgvalue.UUID(failed.checkpointID),
	}); err != nil {
		return staleRunLeaseClaim(err)
	}
	if _, err := store.ReleaseTaskWorkspaceLease(ctx, db.ReleaseTaskWorkspaceLeaseParams{
		CompletedAt: failedAt, ID: authority.workspaceLease.ID,
		WorkspaceID: authority.workspace.ID, WorkspaceMountID: authority.workspaceMount.ID,
		RuntimeInstanceID: authority.runtime.ID, OwnerRunLeaseID: authority.runLease.ID,
		BaseVersionID:          authority.workspaceLease.BaseVersionID,
		OwnershipGeneration:    authority.workspace.OwnershipGeneration,
		WriterGeneration:       authority.workspace.WriterGeneration,
		MountFencingGeneration: authority.workspaceMount.FencingGeneration,
	}); err != nil {
		return staleRunLeaseClaim(err)
	}
	if _, err := store.RequestCheckpointFailureRuntimeClose(ctx, db.RequestCheckpointFailureRuntimeCloseParams{
		FailedAt: failedAt, WorkspaceMountID: authority.workspaceMount.ID,
		OrgID: authority.run.OrgID, ProjectID: authority.run.ProjectID,
		EnvironmentID: authority.run.EnvironmentID, WorkspaceID: authority.workspace.ID,
		WorkerInstanceID: pgvalue.UUID(worker.WorkerInstanceID), WorkerEpoch: worker.WorkerEpoch,
		MountFencingGeneration: authority.workspaceMount.FencingGeneration,
		RuntimeInstanceID:      authority.runtime.ID,
	}); err != nil {
		return staleRunLeaseClaim(err)
	}
	if decision.retry {
		return scheduleActorCheckpointFailureRetry(ctx, store, authority, secrets, failedAt, decision.retryAt)
	}
	return finishCheckpointFailedActor(
		ctx, store, authority, failedAt, failed.errorPayload, decision.reason,
	)
}

type actorCheckpointFailureDecision struct {
	reason  string
	retry   bool
	retryAt time.Time
}

func decideActorCheckpointFailure(
	authority runLeaseClaimAuthority,
	failedAt time.Time,
	activeElapsed int64,
) (actorCheckpointFailureDecision, error) {
	decision := actorCheckpointFailureDecision{
		reason: "checkpoint_failed",
	}
	if activeElapsed >= authority.run.MaxActiveDurationMs {
		decision.reason = "max_active_duration_exceeded"
		return decision, nil
	}
	policy, err := deployment.ParseRetryManifest(authority.run.RetryPolicy)
	if err != nil {
		return actorCheckpointFailureDecision{}, fmt.Errorf("parse pinned Actor checkpoint retry policy: %w", err)
	}
	delay, allowed, err := taskRetryDelay(policy, authority.attempt.Number, nil)
	if err != nil {
		return actorCheckpointFailureDecision{}, err
	}
	decision.retry = allowed
	if allowed {
		decision.retryAt = failedAt.Add(delay)
	}
	return decision, nil
}

func scheduleActorCheckpointFailureRetry(
	ctx context.Context,
	store db.Querier,
	authority runLeaseClaimAuthority,
	secrets []secret.DeliveryEnvelope,
	failedAt pgtype.Timestamptz,
	retryAt time.Time,
) error {
	nextAttempt := authority.attempt.Number + 1
	if _, err := store.CreateActorCheckpointFailureRetryAttempt(ctx, db.CreateActorCheckpointFailureRetryAttemptParams{
		Number: nextAttempt, ExpectedRunGeneration: authority.actor.RunGeneration,
		RunID: authority.run.ID, WorkspaceID: authority.workspace.ID,
		PreviousAttemptNumber: authority.attempt.Number, RunLeaseID: authority.runLease.ID,
	}); err != nil {
		return staleRunLeaseClaim(err)
	}
	if err := createActorAttemptSecretResolutions(
		ctx, store, authority.workspace.ID, authority.run.ID, nextAttempt, secrets,
	); err != nil {
		return err
	}
	if _, err := store.DelayActorCheckpointFailureRetry(ctx, db.DelayActorCheckpointFailureRetryParams{
		NextAttemptNumber: nextAttempt, RetryAt: pgvalue.Timestamptz(retryAt), FailedAt: failedAt,
		ID: authority.run.ID, WorkspaceID: authority.workspace.ID, ActorID: authority.actor.ID,
		PreviousAttemptNumber: authority.attempt.Number, RunLeaseID: authority.runLease.ID,
	}); err != nil {
		return staleRunLeaseClaim(err)
	}
	return nil
}

func finishCheckpointFailedActor(
	ctx context.Context,
	store db.Querier,
	authority runLeaseClaimAuthority,
	failedAt pgtype.Timestamptz,
	errorPayload []byte,
	reason string,
) error {
	status := db.RunStatusSystemFailed
	eventKind := api.RunEventKindFailed
	actorState := "failed"
	failureCode := "platform-failure"
	if reason == "max_active_duration_exceeded" {
		status = db.RunStatusExpired
		eventKind = api.RunEventKindExpired
		failureCode = "run-expired"
	}
	failureRunID := authority.run.ID
	actorFailureCode := pgvalue.Text(failureCode)
	if _, err := store.FinishCheckpointFailedActorRun(ctx, db.FinishCheckpointFailedActorRunParams{
		Status: status, ReasonCode: pgvalue.Text(reason), Error: errorPayload, FailedAt: failedAt,
		ID: authority.run.ID, WorkspaceID: authority.workspace.ID, ActorID: authority.actor.ID,
		AttemptNumber: authority.attempt.Number, RunLeaseID: authority.runLease.ID,
	}); err != nil {
		return staleRunLeaseClaim(err)
	}
	actor, err := store.ReconcileActorTerminalRun(ctx, db.ReconcileActorTerminalRunParams{
		State: actorState, CommittedInputSequence: pgtype.Int8{}, FailureCode: actorFailureCode,
		FailureRunID: failureRunID, CompletedAt: failedAt,
		EnvironmentID: authority.actor.EnvironmentID, ID: authority.actor.ID,
		WorkspaceID: authority.workspace.ID, RunID: authority.run.ID,
		ExpectedRunGeneration: authority.actor.RunGeneration,
	})
	if err != nil {
		return staleRunLeaseClaim(err)
	}
	if _, err := store.ReleaseActorWorkspaceOwner(ctx, db.ReleaseActorWorkspaceOwnerParams{
		CompletedAt: failedAt, ID: authority.workspace.ID, OrgID: authority.run.OrgID,
		ProjectID: authority.run.ProjectID, EnvironmentID: authority.run.EnvironmentID,
		ActorID: actor.ID, OwnershipGeneration: authority.workspace.OwnershipGeneration,
		WriterGeneration: authority.workspace.WriterGeneration,
	}); err != nil {
		return staleRunLeaseClaim(err)
	}
	payload, err := json.Marshal(struct {
		Reason string `json:"reason"`
	}{Reason: reason})
	if err != nil {
		return err
	}
	if _, err := store.AppendRunEvent(ctx, db.AppendRunEventParams{
		OrgID: authority.run.OrgID, RunID: authority.run.ID, Kind: eventKind, Payload: payload,
	}); err != nil {
		return fmt.Errorf("append checkpoint-failed Actor terminal event: %w", err)
	}
	return nil
}

func scheduleCheckpointFailureRetry(
	ctx context.Context,
	store db.Querier,
	authority runLeaseClaimAuthority,
	secrets []secret.DeliveryEnvelope,
	failedAt pgtype.Timestamptz,
	retryAtTime time.Time,
) error {
	nextAttempt := authority.attempt.Number + 1
	if _, err := store.CreateCheckpointFailureRetryAttempt(ctx, db.CreateCheckpointFailureRetryAttemptParams{
		Number: nextAttempt, RunID: authority.run.ID, WorkspaceID: authority.workspace.ID,
		PreviousAttemptNumber: authority.attempt.Number, RunLeaseID: authority.runLease.ID,
	}); err != nil {
		return staleRunLeaseClaim(err)
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
			return fmt.Errorf("record checkpoint retry Secret resolution: %w", err)
		}
	}
	if _, err := store.DelayCheckpointFailureRetry(ctx, db.DelayCheckpointFailureRetryParams{
		NextAttemptNumber: nextAttempt, RetryAt: pgvalue.Timestamptz(retryAtTime),
		FailedAt: failedAt, ID: authority.run.ID, WorkspaceID: authority.workspace.ID,
		PreviousAttemptNumber: authority.attempt.Number, RunLeaseID: authority.runLease.ID,
	}); err != nil {
		return staleRunLeaseClaim(err)
	}
	return nil
}

func finishCheckpointFailedTask(
	ctx context.Context,
	store db.Querier,
	authority runLeaseClaimAuthority,
	failedAt pgtype.Timestamptz,
	errorPayload []byte,
	reason string,
) error {
	if _, err := store.ReleaseTaskWorkspaceOwner(ctx, db.ReleaseTaskWorkspaceOwnerParams{
		CompletedAt: failedAt, ID: authority.workspace.ID, OrgID: authority.run.OrgID,
		ProjectID: authority.run.ProjectID, EnvironmentID: authority.run.EnvironmentID,
		RunID: authority.run.ID, OwnershipGeneration: authority.workspace.OwnershipGeneration,
		WriterGeneration:      authority.workspace.WriterGeneration,
		ExpectedHeadVersionID: authority.run.BaseWorkspaceVersionID,
	}); err != nil {
		return staleRunLeaseClaim(err)
	}
	status := db.RunStatusSystemFailed
	eventKind := api.RunEventKindFailed
	if reason == "max_active_duration_exceeded" {
		status = db.RunStatusExpired
		eventKind = api.RunEventKindExpired
	}
	if _, err := store.FinishCheckpointFailedTaskRun(ctx, db.FinishCheckpointFailedTaskRunParams{
		Status: status, ReasonCode: pgvalue.Text(reason), Error: errorPayload,
		FailedAt: failedAt, ID: authority.run.ID, WorkspaceID: authority.workspace.ID,
		AttemptNumber: authority.attempt.Number, RunLeaseID: authority.runLease.ID,
	}); err != nil {
		return staleRunLeaseClaim(err)
	}
	payload, err := json.Marshal(struct {
		Reason string `json:"reason"`
	}{Reason: reason})
	if err != nil {
		return err
	}
	if _, err := store.AppendRunEvent(ctx, db.AppendRunEventParams{
		OrgID: authority.run.OrgID, RunID: authority.run.ID, Kind: eventKind, Payload: payload,
	}); err != nil {
		return fmt.Errorf("append checkpoint-failed Task terminal event: %w", err)
	}
	return nil
}

func parseCheckpointFailedRequest(request api.WorkerCheckpointFailedRequest) (parsedCheckpointFailed, api.WorkerCheckpointFailedRequest, error) {
	lease, err := parseRunLeaseReceipt(request.Lease)
	if err != nil {
		return parsedCheckpointFailed{}, request, err
	}
	if request.RequestVersion <= 0 {
		return parsedCheckpointFailed{}, request, errors.New("request_version must be positive")
	}
	waitID, err := parseCanonicalUUID("run_wait_id", request.RunWaitID)
	if err != nil {
		return parsedCheckpointFailed{}, request, err
	}
	checkpointID, err := parseCanonicalUUID("checkpoint_id", request.CheckpointID)
	if err != nil {
		return parsedCheckpointFailed{}, request, err
	}
	message := strings.TrimSpace(request.Error)
	if message == "" || len(message) > 1024 {
		return parsedCheckpointFailed{}, request, errors.New("error must be nonempty and no larger than 1024 bytes")
	}
	errorPayload, err := json.Marshal(map[string]any{"code": "checkpoint_failed", "message": message, "retryable": false})
	if err != nil {
		return parsedCheckpointFailed{}, request, fmt.Errorf("encode checkpoint failure: %w", err)
	}
	normalized := request
	normalized.Lease.StartDeadlineAt = request.Lease.StartDeadlineAt.UTC()
	normalized.Lease.ExpiresAt = request.Lease.ExpiresAt.UTC()
	normalized.Error = message
	fingerprint, err := terminalRequestFingerprint("worker.checkpoint-failed.v1", normalized)
	if err != nil {
		return parsedCheckpointFailed{}, request, fmt.Errorf("fingerprint checkpoint-failed: %w", err)
	}
	return parsedCheckpointFailed{
		lease: lease, waitID: waitID, checkpointID: checkpointID, requestVersion: request.RequestVersion,
		attemptNumber: request.Lease.AttemptNumber,
		errorPayload:  errorPayload, fingerprint: fingerprint,
	}, normalized, nil
}

func parseCheckpointReadyRequest(request api.WorkerCheckpointReadyRequest) (parsedCheckpointReady, api.WorkerCheckpointReadyRequest, error) {
	lease, err := parseRunLeaseReceipt(request.Lease)
	if err != nil {
		return parsedCheckpointReady{}, request, err
	}
	if request.RequestVersion <= 0 {
		return parsedCheckpointReady{}, request, errors.New("request_version must be positive")
	}
	waitID, err := parseCanonicalUUID("run_wait_id", request.RunWaitID)
	if err != nil {
		return parsedCheckpointReady{}, request, err
	}
	checkpointID, err := parseCanonicalUUID("checkpoint_id", request.CheckpointID)
	if err != nil {
		return parsedCheckpointReady{}, request, err
	}
	tree, err := parseTaskWorkspaceTree("workspace_capture.tree", request.WorkspaceCapture.Tree)
	if err != nil {
		return parsedCheckpointReady{}, request, err
	}
	if err := validateTaskWorkspaceArtifact("workspace_capture.artifact", request.WorkspaceCapture.Artifact); err != nil {
		return parsedCheckpointReady{}, request, err
	}
	manifest, artifacts, substrateID, hasSubstrate, err := validateCheckpointReadyManifest(request)
	if err != nil {
		return parsedCheckpointReady{}, request, err
	}
	normalized := request
	normalized.Lease.StartDeadlineAt = request.Lease.StartDeadlineAt.UTC()
	normalized.Lease.ExpiresAt = request.Lease.ExpiresAt.UTC()
	normalized.Manifest = request.Manifest
	fingerprint, err := terminalRequestFingerprint("worker.checkpoint-ready.v1", normalized)
	if err != nil {
		return parsedCheckpointReady{}, request, fmt.Errorf("fingerprint checkpoint-ready: %w", err)
	}
	return parsedCheckpointReady{
		lease: lease, waitID: waitID, checkpointID: checkpointID,
		capture:  parsedTaskWorkspaceCapture{tree: tree, artifact: request.WorkspaceCapture.Artifact},
		manifest: manifest, fingerprint: fingerprint, artifacts: artifacts,
		substrateID: substrateID, hasSubstrate: hasSubstrate, requestVersion: request.RequestVersion,
		attemptNumber: request.Lease.AttemptNumber,
	}, normalized, nil
}

func validateCheckpointReadyManifest(request api.WorkerCheckpointReadyRequest) ([]byte, []checkpointArtifactProof, uuid.UUID, bool, error) {
	manifest := request.Manifest
	recovery := manifest.RecoveryPoint
	if recovery.ID != request.CheckpointID || recovery.RunID != request.Lease.RunID ||
		recovery.AttemptNumber != request.Lease.AttemptNumber || recovery.RunWaitID != request.RunWaitID ||
		strings.TrimSpace(recovery.CorrelationID) == "" {
		return nil, nil, uuid.Nil, false, errors.New("manifest recovery_point does not match checkpoint request")
	}
	identity := recovery.Runtime
	if identity.Backend != "firecracker" ||
		deployment.ValidateRuntimeArchitecture(deployment.RuntimeArchitecture(identity.Arch)) != nil ||
		identity.ID != request.Lease.RuntimeIdentityID || strings.TrimSpace(identity.ABI) == "" ||
		!taskWorkspaceDigestPattern.MatchString(identity.KernelDigest) ||
		!taskWorkspaceDigestPattern.MatchString(identity.InitramfsDigest) ||
		!taskWorkspaceDigestPattern.MatchString(identity.RootfsDigest) ||
		!taskWorkspaceDigestPattern.MatchString(identity.ConfigDigest) {
		return nil, nil, uuid.Nil, false, errors.New("manifest runtime identity is invalid")
	}
	proofs := []checkpointArtifactProof{
		{role: db.RunCheckpointArtifactRoleRuntimeConfig, kind: db.ArtifactKindRunCheckpointConfig, artifact: manifest.RuntimeState.ConfigArtifact},
		{role: db.RunCheckpointArtifactRoleVmState, kind: db.ArtifactKindRunCheckpointVmState, artifact: manifest.RuntimeState.VMStateArtifact},
		{role: db.RunCheckpointArtifactRoleScratchDisk, kind: db.ArtifactKindRunCheckpointScratchDisk, artifact: manifest.RuntimeState.ScratchDiskArtifact},
	}
	for index, artifact := range manifest.RuntimeState.MemoryArtifacts {
		proofs = append(proofs, checkpointArtifactProof{
			role: db.RunCheckpointArtifactRoleMemory, ordinal: int32(index),
			kind: db.ArtifactKindRunCheckpointMemory, artifact: artifact,
		})
	}
	if len(manifest.RuntimeState.MemoryArtifacts) == 0 {
		return nil, nil, uuid.Nil, false, errors.New("manifest runtime_state.memory_artifacts is required")
	}
	expectedMedia := map[db.RunCheckpointArtifactRole]string{
		db.RunCheckpointArtifactRoleRuntimeConfig: cas.CheckpointRuntimeConfigMediaType,
		db.RunCheckpointArtifactRoleVmState:       cas.CheckpointVMStateMediaType,
		db.RunCheckpointArtifactRoleScratchDisk:   cas.CheckpointScratchDiskMediaType,
		db.RunCheckpointArtifactRoleMemory:        cas.CheckpointMemoryMediaType,
	}
	for _, proof := range proofs {
		if !taskWorkspaceDigestPattern.MatchString(proof.artifact.Digest) || proof.artifact.SizeBytes <= 0 ||
			proof.artifact.MediaType != expectedMedia[proof.role] {
			return nil, nil, uuid.Nil, false, fmt.Errorf("manifest checkpoint artifact %s/%d is invalid", proof.role, proof.ordinal)
		}
	}
	if len(manifest.RuntimeState.Config) == 0 || !json.Valid(manifest.RuntimeState.Config) {
		return nil, nil, uuid.Nil, false, errors.New("manifest runtime_state.config must be valid JSON")
	}
	var substrateID uuid.UUID
	hasSubstrate := identity.Substrate != nil || manifest.RuntimeState.RuntimeSubstrate != nil
	if hasSubstrate {
		if identity.Substrate == nil || manifest.RuntimeState.RuntimeSubstrate == nil {
			return nil, nil, uuid.Nil, false, errors.New("manifest runtime substrate proof is incomplete")
		}
		substrateID, _ = uuid.Parse(manifest.RuntimeState.RuntimeSubstrate.ID)
		artifact := manifest.RuntimeState.RuntimeSubstrate.Artifact
		if substrateID == uuid.Nil || substrateID.String() != manifest.RuntimeState.RuntimeSubstrate.ID ||
			identity.Substrate.Digest != manifest.RuntimeState.RuntimeSubstrate.SubstrateDigest ||
			identity.Substrate.Format != manifest.RuntimeState.RuntimeSubstrate.Format ||
			identity.Substrate.BuilderABI != manifest.RuntimeState.RuntimeSubstrate.BuilderABI ||
			identity.Substrate.LayoutABI != manifest.RuntimeState.RuntimeSubstrate.LayoutABI ||
			!taskWorkspaceDigestPattern.MatchString(artifact.Digest) || artifact.SizeBytes <= 0 || strings.TrimSpace(artifact.MediaType) == "" {
			return nil, nil, uuid.Nil, false, errors.New("manifest runtime substrate proof is invalid")
		}
	}
	encoded, err := json.Marshal(manifest)
	if err != nil {
		return nil, nil, uuid.Nil, false, fmt.Errorf("encode checkpoint manifest: %w", err)
	}
	encoded, err = canonicalJSON(encoded)
	if err != nil || len(encoded) > 65536 {
		if err == nil {
			err = errors.New("checkpoint manifest exceeds 64 KiB")
		}
		return nil, nil, uuid.Nil, false, err
	}
	return encoded, proofs, substrateID, hasSubstrate, nil
}

func (s *Server) verifyCheckpointRuntimeArtifacts(
	ctx context.Context,
	proofs []checkpointArtifactProof,
	substrate *api.CASObject,
) error {
	if s.cas == nil {
		return errors.New("checkpoint CAS is not configured")
	}
	for _, proof := range proofs {
		object, err := s.cas.Stat(ctx, proof.artifact.Digest)
		if err != nil {
			return fmt.Errorf("checkpoint artifact %s/%d is missing from CAS: %w", proof.role, proof.ordinal, err)
		}
		if object.Digest != proof.artifact.Digest || object.SizeBytes != proof.artifact.SizeBytes || object.MediaType != proof.artifact.MediaType {
			return fmt.Errorf("checkpoint artifact %s/%d does not match CAS authority", proof.role, proof.ordinal)
		}
	}
	if substrate != nil {
		object, err := s.cas.Stat(ctx, substrate.Digest)
		if err != nil {
			return fmt.Errorf("runtime substrate artifact is missing from CAS: %w", err)
		}
		if object.Digest != substrate.Digest || object.SizeBytes != substrate.SizeBytes || object.MediaType != substrate.MediaType {
			return errors.New("runtime substrate artifact does not match CAS authority")
		}
	}
	return nil
}

func (s *Server) checkpointReadyReplay(ctx context.Context, ready parsedCheckpointReady) (api.WorkerCheckpointResponse, bool, error) {
	replay, err := s.db.GetCheckpointReadyReplay(ctx, pgvalue.UUID(ready.checkpointID))
	if errors.Is(err, pgx.ErrNoRows) {
		return api.WorkerCheckpointResponse{}, false, nil
	}
	if err != nil {
		return api.WorkerCheckpointResponse{}, false, errors.New("load checkpoint-ready replay")
	}
	if replay.RunID != pgvalue.UUID(ready.lease.runID) || replay.AttemptNumber != ready.attemptNumber ||
		replay.RunWaitID != pgvalue.UUID(ready.waitID) || replay.SourceRunLeaseID != pgvalue.UUID(ready.lease.leaseID) ||
		!replay.PrivateWorkspaceVersionID.Valid || !replay.ReadyRequestFingerprint.Valid ||
		replay.ReadyRequestFingerprint.String != ready.fingerprint {
		return api.WorkerCheckpointResponse{}, false, conflict(errors.New("checkpoint-ready replay does not match the committed request"))
	}
	return api.WorkerCheckpointResponse{
		RunID: ready.lease.runID.String(), RunWaitID: ready.waitID.String(), CheckpointID: ready.checkpointID.String(),
		WorkspaceVersionID: pgvalue.UUIDString(replay.PrivateWorkspaceVersionID),
	}, true, nil
}

func (s *Server) checkpointFailedReplay(ctx context.Context, failed parsedCheckpointFailed) (api.WorkerCheckpointResponse, bool, error) {
	replay, err := s.db.GetCheckpointFailedReplay(ctx, pgvalue.UUID(failed.checkpointID))
	if errors.Is(err, pgx.ErrNoRows) {
		return api.WorkerCheckpointResponse{}, false, nil
	}
	if err != nil {
		return api.WorkerCheckpointResponse{}, false, errors.New("load checkpoint-failed replay")
	}
	if replay.RunID != pgvalue.UUID(failed.lease.runID) || replay.AttemptNumber != failed.attemptNumber ||
		replay.RunWaitID != pgvalue.UUID(failed.waitID) || replay.SourceRunLeaseID != pgvalue.UUID(failed.lease.leaseID) ||
		replay.WorkspaceID != pgvalue.UUID(failed.lease.workspaceID) || !replay.FailedRequestFingerprint.Valid ||
		replay.FailedRequestFingerprint.String != failed.fingerprint {
		return api.WorkerCheckpointResponse{}, false, conflict(errors.New("checkpoint-failed replay does not match the committed request"))
	}
	return api.WorkerCheckpointResponse{
		RunID: failed.lease.runID.String(), RunWaitID: failed.waitID.String(), CheckpointID: failed.checkpointID.String(),
	}, true, nil
}

func (s *Server) commitCheckpointReady(
	ctx context.Context,
	worker workerActor,
	request api.WorkerCheckpointReadyRequest,
	ready parsedCheckpointReady,
) (api.WorkerCheckpointResponse, error) {
	var response api.WorkerCheckpointResponse
	err := s.inTx(ctx, func(work *txWork) error {
		if _, err := secret.LockAttemptDelivery(ctx, work.q, pgvalue.UUID(ready.lease.runID), request.Lease.AttemptNumber, pgvalue.UUID(ready.lease.workspaceID)); err != nil {
			return fmt.Errorf("lock checkpoint-ready Secret authority: %w", err)
		}
		locators, err := work.q.GetLiveRunLeaseLocators(ctx, db.GetLiveRunLeaseLocatorsParams{
			ID: pgvalue.UUID(ready.lease.leaseID), LeaseSequence: request.Lease.LeaseSequence,
			WorkerGroupID: worker.WorkerGroupID, WorkerInstanceID: pgvalue.UUID(worker.WorkerInstanceID),
			WorkerEpoch: worker.WorkerEpoch, WorkerProtocolVersion: worker.ProtocolVersion,
		})
		if err != nil {
			return staleRunLeaseClaim(err)
		}
		owner, err := lockRunFinalizationOwner(ctx, work.q, locators)
		if err != nil {
			return err
		}
		authority, err := lockRenewableRunLeaseAuthority(
			ctx, work.q, worker, pgvalue.UUID(ready.lease.leaseID), request.Lease.LeaseSequence, locators,
		)
		if err != nil {
			return err
		}
		authority.actor = owner.actor
		if authority.run.Status != db.RunStatusWaiting || authority.run.ParentRunID.Valid ||
			authority.runLease.State != db.RunLeaseStateCheckpointing {
			return errStaleRunLeaseClaim
		}
		current, err := projectRunLeaseReceipt(runLeaseProjectionAuthority{
			run: authority.run, attempt: authority.attempt, runtime: authority.runtime,
			networkSlot: authority.networkSlot, runLease: authority.runLease, workspace: authority.workspace,
			workspaceMount: authority.workspaceMount, workspaceLease: authority.workspaceLease,
		})
		if err != nil || !equalCurrentOrPreviousRunLeaseReceipt(current, request.Lease, authority.runLease.PreviousExpiresAt) {
			return errStaleRunLeaseClaim
		}
		wait, err := work.q.LockRunLeaseClaimWait(ctx, db.LockRunLeaseClaimWaitParams{
			ID: pgvalue.UUID(ready.waitID), EnvironmentID: authority.run.EnvironmentID, RunID: authority.run.ID,
			AttemptNumber: authority.attempt.Number, WorkspaceID: authority.workspace.ID,
			CurrentRunLeaseID: authority.runLease.ID,
		})
		if err != nil || (wait.Kind != db.WaitKindToken && wait.Kind != db.WaitKindActorInput) ||
			wait.SuspensionState != db.RunWaitStateCheckpointing ||
			wait.CheckpointRequestVersion != request.RequestVersion || wait.SuspendCheckpointID != pgvalue.UUID(ready.checkpointID) {
			return staleRunLeaseClaim(err)
		}
		if err := validateRootRunWaitActorCursor(authority, wait); err != nil {
			return err
		}
		checkpoint, err := work.q.LockCreatingRunCheckpoint(ctx, db.LockCreatingRunCheckpointParams{
			ID: pgvalue.UUID(ready.checkpointID), RunID: authority.run.ID, AttemptNumber: authority.attempt.Number,
			RunWaitID: wait.ID, SourceRunLeaseID: authority.runLease.ID,
			SourceWorkspaceLeaseID: authority.workspaceLease.ID, WorkspaceID: authority.workspace.ID,
		})
		if err != nil || checkpoint.BaseWorkspaceVersionID != authority.workspaceLease.BaseVersionID ||
			checkpoint.ActorSpeculativeInputSequence != wait.ActorSpeculativeInputSequence {
			return staleRunLeaseClaim(err)
		}
		identity, err := work.q.GetRuntimeIdentityForCheckpoint(ctx, authority.runtime.RuntimeIdentityID)
		if err != nil || identity.ID != request.Manifest.RecoveryPoint.Runtime.ID ||
			identity.RuntimeArch != request.Manifest.RecoveryPoint.Runtime.Arch || identity.RuntimeABI != request.Manifest.RecoveryPoint.Runtime.ABI ||
			identity.KernelDigest != request.Manifest.RecoveryPoint.Runtime.KernelDigest ||
			identity.InitramfsDigest != request.Manifest.RecoveryPoint.Runtime.InitramfsDigest ||
			identity.RootfsDigest != request.Manifest.RecoveryPoint.Runtime.RootfsDigest {
			return staleRunLeaseClaim(err)
		}
		if err := validateCheckpointSubstrateAuthority(ctx, work.q, authority, request, ready); err != nil {
			return err
		}
		checkpointedAt, err := work.q.GetRunLeaseRenewalTime(ctx)
		if err != nil || !checkpointedAt.Valid || !checkpointedAt.Time.Before(authority.runLease.ExpiresAt.Time) {
			return staleRunLeaseClaim(err)
		}
		workspaceVersionID, err := recordCheckpointWorkspaceVersion(ctx, work.q, worker, authority, ready.capture)
		if err != nil {
			return err
		}
		if err := recordCheckpointRuntimeArtifacts(ctx, work.q, worker, authority, ready.checkpointID, ready.artifacts); err != nil {
			return err
		}
		if _, err := work.q.MarkRunCheckpointReady(ctx, db.MarkRunCheckpointReadyParams{
			PrivateWorkspaceVersionID: workspaceVersionID, RestoreManifest: ready.manifest,
			ReadyRequestFingerprint: pgvalue.Text(ready.fingerprint), RunID: authority.run.ID,
			AttemptNumber: authority.attempt.Number, ID: pgvalue.UUID(ready.checkpointID),
		}); err != nil {
			return staleRunLeaseClaim(err)
		}
		if _, err := work.q.CloseRunActiveIntervalForCheckpoint(ctx, db.CloseRunActiveIntervalForCheckpointParams{
			ID: authority.run.ID, OrgID: authority.run.OrgID, ProjectID: authority.run.ProjectID,
			EnvironmentID: authority.run.EnvironmentID, WorkspaceID: authority.workspace.ID,
			AttemptNumber: authority.attempt.Number, RunLeaseID: authority.runLease.ID,
		}); err != nil {
			return staleRunLeaseClaim(err)
		}
		if err := updateTaskWorkspaceMountFrontier(ctx, work.q, authority, workspaceVersionID, checkpointedAt); err != nil {
			return err
		}
		if _, err := work.q.CheckpointRunLease(ctx, db.CheckpointRunLeaseParams{
			CheckpointedAt: checkpointedAt, ID: authority.runLease.ID, RunID: authority.run.ID,
			WorkspaceID: authority.workspace.ID, AttemptNumber: authority.attempt.Number,
			LeaseSequence: authority.runLease.LeaseSequence,
		}); err != nil {
			return staleRunLeaseClaim(err)
		}
		if _, err := work.q.ReleaseCheckpointWorkspaceLease(ctx, db.ReleaseCheckpointWorkspaceLeaseParams{
			CheckpointedAt: checkpointedAt, ID: authority.workspaceLease.ID, WorkspaceID: authority.workspace.ID,
			WorkspaceMountID: authority.workspaceMount.ID, RuntimeInstanceID: authority.runtime.ID,
			OwnerRunLeaseID: authority.runLease.ID, BaseVersionID: authority.workspaceLease.BaseVersionID,
			OwnershipGeneration:    authority.workspaceLease.OwnershipGeneration,
			WriterGeneration:       authority.workspaceLease.WriterGeneration,
			MountFencingGeneration: authority.workspaceLease.MountFencingGeneration,
		}); err != nil {
			return staleRunLeaseClaim(err)
		}
		if wait.ConditionState == db.WaitStatePending {
			if _, err := work.q.CommitPendingCheckpointReady(ctx, db.CommitPendingCheckpointReadyParams{
				CheckpointedAt: checkpointedAt, RunID: authority.run.ID, WorkspaceID: authority.workspace.ID,
				AttemptNumber: authority.attempt.Number, RunLeaseID: authority.runLease.ID,
				ExpectedRunStateVersion: wait.ExpectedRunStateVersion, CheckpointRequestVersion: request.RequestVersion,
				RunWaitID: wait.ID, CheckpointID: pgvalue.UUID(ready.checkpointID),
			}); err != nil {
				return staleRunLeaseClaim(err)
			}
		} else {
			committed, err := work.q.CommitTerminalCheckpointReady(ctx, db.CommitTerminalCheckpointReadyParams{
				CheckpointedAt: checkpointedAt, RunID: authority.run.ID, WorkspaceID: authority.workspace.ID,
				AttemptNumber: authority.attempt.Number, RunLeaseID: authority.runLease.ID,
				ExpectedRunStateVersion: wait.ExpectedRunStateVersion, CheckpointRequestVersion: request.RequestVersion,
				RunWaitID: wait.ID, CheckpointID: pgvalue.UUID(ready.checkpointID),
			})
			if err != nil {
				return staleRunLeaseClaim(err)
			}
			payload, _ := json.Marshal(map[string]any{
				"environmentId": pgvalue.UUIDString(authority.run.EnvironmentID), "runId": request.Lease.RunID,
				"runWaitId": request.RunWaitID, "resumeRequestVersion": committed.ResumeRequestVersion,
			})
			if _, err := work.q.CreateOutboxMessage(ctx, db.CreateOutboxMessageParams{
				ID: pgvalue.UUID(uuid.Must(uuid.NewV7())), Lane: "control", Topic: "run.resume",
				PartitionKey: pgvalue.UUIDString(authority.workspace.ID), Payload: payload, AvailableAt: checkpointedAt,
			}); err != nil {
				return fmt.Errorf("publish checkpoint-ready resume: %w", err)
			}
		}
		response = api.WorkerCheckpointResponse{
			RunID: request.Lease.RunID, RunWaitID: request.RunWaitID, CheckpointID: request.CheckpointID,
			WorkspaceVersionID: pgvalue.UUIDString(workspaceVersionID),
		}
		return nil
	})
	return response, err
}

func validateCheckpointSubstrateAuthority(
	ctx context.Context,
	store db.Querier,
	authority runLeaseClaimAuthority,
	request api.WorkerCheckpointReadyRequest,
	ready parsedCheckpointReady,
) error {
	if !authority.runtime.RuntimeSubstrateID.Valid {
		if ready.hasSubstrate {
			return errStaleRunLeaseClaim
		}
		return nil
	}
	if !ready.hasSubstrate || authority.runtime.RuntimeSubstrateID != pgvalue.UUID(ready.substrateID) {
		return errStaleRunLeaseClaim
	}
	row, err := store.GetRuntimeSubstrateForCheckpoint(ctx, authority.runtime.RuntimeSubstrateID)
	proof := request.Manifest.RuntimeState.RuntimeSubstrate
	identity := request.Manifest.RecoveryPoint.Runtime.Substrate
	substrate := row.RuntimeSubstrate
	if err != nil || proof == nil || identity == nil ||
		substrate.ID != authority.runtime.RuntimeSubstrateID ||
		substrate.OrgID != authority.run.OrgID || substrate.ProjectID != authority.run.ProjectID ||
		substrate.EnvironmentID != authority.run.EnvironmentID ||
		substrate.DeploymentDefinitionID != authority.runtime.DeploymentDefinitionID ||
		proof.ID != pgvalue.UUIDString(substrate.ID) ||
		proof.DeploymentDefinitionID != pgvalue.UUIDString(substrate.DeploymentDefinitionID) ||
		proof.SubstrateDigest != substrate.SubstrateDigest || proof.SubstrateDigest != identity.Digest ||
		proof.Format != substrate.SubstrateFormat || proof.Format != identity.Format ||
		proof.BuilderABI != substrate.BuilderAbi || proof.BuilderABI != identity.BuilderABI ||
		proof.LayoutABI != substrate.LayoutAbi || proof.LayoutABI != identity.LayoutABI ||
		proof.SizeBytes != substrate.SubstrateSizeBytes ||
		proof.Artifact.Digest != row.ArtifactDigest || proof.Artifact.SizeBytes != row.ArtifactSizeBytes ||
		proof.Artifact.MediaType != row.ArtifactMediaType {
		return staleRunLeaseClaim(err)
	}
	return nil
}

func recordCheckpointWorkspaceVersion(
	ctx context.Context,
	store db.Querier,
	worker workerActor,
	authority runLeaseClaimAuthority,
	capture parsedTaskWorkspaceCapture,
) (pgtype.UUID, error) {
	artifact := capture.artifact
	if _, err := store.UpsertCasObject(ctx, db.UpsertCasObjectParams{
		OrgID: authority.run.OrgID, Digest: artifact.Digest, SizeBytes: artifact.SizeBytes, MediaType: artifact.MediaType,
	}); err != nil {
		return pgtype.UUID{}, fmt.Errorf("record checkpoint Workspace CAS object: %w", err)
	}
	artifactRow, err := store.CreateArtifact(ctx, db.CreateArtifactParams{
		ID: pgvalue.UUID(uuid.Must(uuid.NewV7())), OrgID: authority.run.OrgID,
		ProjectID: authority.run.ProjectID, EnvironmentID: authority.run.EnvironmentID,
		Digest: artifact.Digest, Kind: db.ArtifactKindWorkspaceVersion, SizeBytes: artifact.SizeBytes,
		MediaType: artifact.MediaType, CreatedByWorkerInstanceID: pgvalue.UUID(worker.WorkerInstanceID),
	})
	if err != nil {
		return pgtype.UUID{}, fmt.Errorf("record checkpoint Workspace Artifact: %w", err)
	}
	publicID, err := newPublicID(publicid.WorkspaceVersion)
	if err != nil {
		return pgtype.UUID{}, err
	}
	version, err := store.CreatePrivateCheckpointWorkspaceVersion(ctx, db.CreatePrivateCheckpointWorkspaceVersionParams{
		ID: pgvalue.UUID(uuid.Must(uuid.NewV7())), PublicID: publicID, OrgID: authority.run.OrgID,
		ProjectID: authority.run.ProjectID, EnvironmentID: authority.run.EnvironmentID,
		WorkspaceID: authority.workspace.ID, ParentVersionID: authority.workspaceLease.BaseVersionID,
		ArtifactID: artifactRow.ID, ContentDigest: capture.tree.Digest,
		SizeBytes: capture.tree.SizeBytes, EntryCount: int32(capture.tree.EntryCount),
		SourceWorkspaceLeaseID: authority.workspaceLease.ID,
		OwnershipGeneration:    authority.workspace.OwnershipGeneration, WriterGeneration: authority.workspace.WriterGeneration,
	})
	if err != nil {
		return pgtype.UUID{}, fmt.Errorf("record private checkpoint Workspace version: %w", err)
	}
	return version.ID, nil
}

func recordCheckpointRuntimeArtifacts(
	ctx context.Context,
	store db.Querier,
	worker workerActor,
	authority runLeaseClaimAuthority,
	checkpointID uuid.UUID,
	proofs []checkpointArtifactProof,
) error {
	for _, proof := range proofs {
		if _, err := store.UpsertCasObject(ctx, db.UpsertCasObjectParams{
			OrgID: authority.run.OrgID, Digest: proof.artifact.Digest,
			SizeBytes: proof.artifact.SizeBytes, MediaType: proof.artifact.MediaType,
		}); err != nil {
			return fmt.Errorf("record checkpoint CAS object %s/%d: %w", proof.role, proof.ordinal, err)
		}
		artifact, err := store.CreateArtifact(ctx, db.CreateArtifactParams{
			ID: pgvalue.UUID(uuid.Must(uuid.NewV7())), OrgID: authority.run.OrgID,
			ProjectID: authority.run.ProjectID, EnvironmentID: authority.run.EnvironmentID,
			Digest: proof.artifact.Digest, Kind: proof.kind, SizeBytes: proof.artifact.SizeBytes,
			MediaType: proof.artifact.MediaType, CreatedByWorkerInstanceID: pgvalue.UUID(worker.WorkerInstanceID),
		})
		if err != nil {
			return fmt.Errorf("record checkpoint Artifact %s/%d: %w", proof.role, proof.ordinal, err)
		}
		if _, err := store.AddRunCheckpointArtifact(ctx, db.AddRunCheckpointArtifactParams{
			RunCheckpointID: pgvalue.UUID(checkpointID), Role: proof.role, Ordinal: proof.ordinal, ArtifactID: artifact.ID,
		}); err != nil {
			return fmt.Errorf("record checkpoint Artifact membership %s/%d: %w", proof.role, proof.ordinal, err)
		}
	}
	return nil
}
