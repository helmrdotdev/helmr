package controlplane

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
	"github.com/helmrdotdev/helmr/internal/idempotency"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/helmrdotdev/helmr/internal/tracing"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

type parsedCheckpointReady struct {
	lease          parsedRunLeaseFence
	waitID         uuid.UUID
	checkpointID   uuid.UUID
	capture        parsedTaskWorkspaceCapture
	manifest       []byte
	fingerprint    string
	artifacts      []checkpointArtifactProof
	requestVersion int64
}

type parsedCheckpointFailed struct {
	lease          parsedRunLeaseFence
	waitID         uuid.UUID
	checkpointID   uuid.UUID
	requestVersion int64
	errorPayload   []byte
	fingerprint    string
}

type checkpointArtifactProof struct {
	role     db.RunCheckpointArtifactRole
	ordinal  int32
	kind     db.ArtifactKind
	artifact workerapi.CheckpointArtifact
}

func (s *Server) workerMarkCheckpointReady(w http.ResponseWriter, r *http.Request) {
	var request workerapi.CheckpointReadyRequest
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
		writeError(w, badRequest(fmt.Errorf("verify checkpoint workspace capture: %w", err)))
		return
	}
	parsed.capture = verified
	if err := s.verifyCheckpointRuntimeArtifacts(r.Context(), parsed.artifacts); err != nil {
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
		s.log.Error("commit worker checkpoint-ready failed", "run_lease_id", request.Lease.ID, "error", err)
		writeError(w, errors.New("commit worker checkpoint-ready"))
		return
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) workerMarkCheckpointFailed(w http.ResponseWriter, r *http.Request) {
	var request workerapi.CheckpointFailedRequest
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
	if response, replayed, replayErr := s.checkpointFailedReplay(r.Context(), parsed); replayErr != nil {
		writeError(w, replayErr)
		return
	} else if replayed {
		writeJSON(w, http.StatusOK, response)
		return
	}
	err = s.inTx(r.Context(), func(work *txWork) error {
		locators, err := work.q.GetLiveRunLeaseLocators(r.Context(), db.GetLiveRunLeaseLocatorsParams{
			ID: pgvalue.UUID(parsed.lease.leaseID), LeaseSequence: normalized.Lease.LeaseSequence,
			WorkerGroupID: worker.WorkerGroupID, WorkerInstanceID: pgvalue.UUID(worker.WorkerInstanceID),
			WorkerEpoch: worker.WorkerEpoch, WorkerProtocolVersion: worker.ProtocolVersion,
		})
		if err != nil {
			return staleRunLeaseClaim(err)
		}
		secrets, err := secret.LockAttemptDelivery(r.Context(), work.q, locators.RunID, locators.AttemptNumber, locators.WorkspaceID)
		if err != nil {
			return fmt.Errorf("lock checkpoint-failed secret authority: %w", err)
		}
		tx, ok := work.tx.(pgx.Tx)
		if !ok {
			return errors.New("checkpoint failure transaction does not expose PostgreSQL authority")
		}
		ownedGraph, err := run.LockOwnedFinalization(
			r.Context(),
			tx,
			run.OwnedFinalizationRequest{
				OrgID:         pgvalue.MustUUIDValue(locators.OrgID),
				ProjectID:     pgvalue.MustUUIDValue(locators.ProjectID),
				EnvironmentID: pgvalue.MustUUIDValue(locators.EnvironmentID),
				RunID:         pgvalue.MustUUIDValue(locators.RunID),
			},
		)
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
		authority.parentRun = owner.parent
		if err := validateRunFinalizationOwner(authority, locators); err != nil {
			return staleRunLeaseClaim(err)
		}
		if authority.run.ParentRunID.Valid && authority.run.ParentOwnsLifecycle.Valid &&
			authority.run.ParentOwnsLifecycle.Bool {
			authority.enclosingWait, err = lockParentOwnedChildWaitIfActive(
				r.Context(),
				work.q,
				authority.parentRun,
				db.LockParentOwnedChildWaitParams{
					EnvironmentID: authority.run.EnvironmentID,
					ParentRunID:   authority.run.ParentRunID,
					ChildRunID:    authority.run.ID,
				},
			)
			if err != nil {
				return staleRunLeaseClaim(err)
			}
		}
		if authority.run.Status != db.RunStatusWaiting ||
			authority.runLease.State != db.RunLeaseStateCheckpointing {
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
		if err := validateRunWaitActorCursor(authority, wait); err != nil {
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
			return failCheckpointActorAttempt(
				r.Context(), work.q, ownedGraph, worker, authority, wait, parsed, secrets,
			)
		}
		return failCheckpointTaskAttempt(
			r.Context(), work.q, ownedGraph, worker, authority, wait, parsed, secrets,
		)
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
	writeJSON(w, http.StatusOK, workerapi.CheckpointResponse{
		RunWaitID: normalized.RunWaitID, CheckpointID: normalized.CheckpointID,
	})
}

func failCheckpointTaskAttempt(
	ctx context.Context,
	store db.Querier,
	ownedGraph run.OwnedFinalization,
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
	if !retry {
		if _, err := ownedGraph.CancelDescendants(ctx); err != nil {
			return fmt.Errorf(
				"cancel child tasks after exhausted parent checkpoint failure: %w",
				err,
			)
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
	ownedGraph run.OwnedFinalization,
	worker workerActor,
	authority runLeaseClaimAuthority,
	wait db.RunWait,
	failed parsedCheckpointFailed,
	secrets []secret.DeliveryEnvelope,
) error {
	failedAt, err := store.GetTaskCompletionTime(ctx)
	if err != nil || !failedAt.Valid {
		if err == nil {
			err = errors.New("database actor checkpoint failure time is unavailable")
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
	if !decision.retry {
		if _, err := ownedGraph.CancelDescendants(ctx); err != nil {
			return fmt.Errorf(
				"cancel child tasks after exhausted actor checkpoint failure: %w",
				err,
			)
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
	if _, err := store.CompleteActorAttempt(ctx, db.CompleteActorAttemptParams{
		TerminalSessionInputSequence: pgtype.Int8{}, TerminalOutcome: pgvalue.Text("failed"),
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
		return actorCheckpointFailureDecision{}, fmt.Errorf("parse pinned actor checkpoint retry policy: %w", err)
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
		ID: authority.run.ID, WorkspaceID: authority.workspace.ID, SessionID: authority.actor.ID,
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
	failureCode := "platform_failure"
	if reason == "max_active_duration_exceeded" {
		status = db.RunStatusExpired
		eventKind = api.RunEventKindExpired
		failureCode = "run_expired"
	}
	failureRunID := authority.run.ID
	actorFailureCode := pgvalue.Text(failureCode)
	if _, err := store.FinishCheckpointFailedActorRun(ctx, db.FinishCheckpointFailedActorRunParams{
		Status: status, ReasonCode: pgvalue.Text(reason), Error: errorPayload, FailedAt: failedAt,
		ID: authority.run.ID, WorkspaceID: authority.workspace.ID, SessionID: authority.actor.ID,
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
		CompletedAt: failedAt, ID: authority.workspace.ID,
		EnvironmentID: authority.run.EnvironmentID,
		SessionID:     actor.ID, OwnershipGeneration: authority.workspace.OwnershipGeneration,
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
		return fmt.Errorf("append checkpoint-failed actor terminal event: %w", err)
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
	resolutions, err := activeSecretResolutions(secrets)
	if err != nil {
		return err
	}
	if err := secret.CreateAttemptResolutions(
		ctx, store, authority.workspace.ID, authority.run.ID, nextAttempt, resolutions,
	); err != nil {
		return fmt.Errorf("record checkpoint retry secret resolutions: %w", err)
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
		return fmt.Errorf("append checkpoint-failed task terminal event: %w", err)
	}
	if authority.run.ParentRunID.Valid && authority.run.ParentOwnsLifecycle.Valid &&
		authority.run.ParentOwnsLifecycle.Bool && authority.enclosingWait.ID.Valid {
		terminalRun := authority.run
		terminalRun.Status = status
		terminalRun.TerminalReasonCode = pgvalue.Text(reason)
		terminalRun.Error = errorPayload
		if err := resolveParentOwnedChildWait(
			ctx, store, authority, terminalRun, failedAt,
		); err != nil {
			return err
		}
	}
	return nil
}

func parseCheckpointFailedRequest(request workerapi.CheckpointFailedRequest) (parsedCheckpointFailed, workerapi.CheckpointFailedRequest, error) {
	lease, err := parseRunLeaseFence(request.Lease)
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
	normalized.Error = message
	fingerprint, err := terminalRequestFingerprint("worker.checkpoint-failed.v1", normalized)
	if err != nil {
		return parsedCheckpointFailed{}, request, fmt.Errorf("fingerprint checkpoint-failed: %w", err)
	}
	return parsedCheckpointFailed{
		lease: lease, waitID: waitID, checkpointID: checkpointID, requestVersion: request.RequestVersion,
		errorPayload: errorPayload, fingerprint: fingerprint,
	}, normalized, nil
}

func parseCheckpointReadyRequest(request workerapi.CheckpointReadyRequest) (parsedCheckpointReady, workerapi.CheckpointReadyRequest, error) {
	lease, err := parseRunLeaseFence(request.Lease)
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
	manifest, artifacts, err := validateCheckpointReadyManifest(request)
	if err != nil {
		return parsedCheckpointReady{}, request, err
	}
	normalized := request
	normalized.Manifest = request.Manifest
	fingerprint, err := terminalRequestFingerprint("worker.checkpoint-ready.v1", normalized)
	if err != nil {
		return parsedCheckpointReady{}, request, fmt.Errorf("fingerprint checkpoint-ready: %w", err)
	}
	return parsedCheckpointReady{
		lease: lease, waitID: waitID, checkpointID: checkpointID,
		capture:  parsedTaskWorkspaceCapture{tree: tree, artifact: request.WorkspaceCapture.Artifact},
		manifest: manifest, fingerprint: fingerprint, artifacts: artifacts,
		requestVersion: request.RequestVersion,
	}, normalized, nil
}

func validateCheckpointReadyManifest(request workerapi.CheckpointReadyRequest) ([]byte, []checkpointArtifactProof, error) {
	return validateCheckpointManifest(
		request.Manifest,
		request.CheckpointID,
		request.Manifest.RecoveryPoint.RunID,
		request.Manifest.RecoveryPoint.AttemptNumber,
		request.RunWaitID,
		request.Manifest.RecoveryPoint.Runtime.ID,
	)
}

func validateCheckpointManifest(
	manifest workerapi.CheckpointManifest,
	checkpointID string,
	runID string,
	attemptNumber int32,
	runWaitID string,
	runtimeIdentityID string,
) ([]byte, []checkpointArtifactProof, error) {
	recovery := manifest.RecoveryPoint
	if recovery.ID != checkpointID || recovery.RunID != runID ||
		recovery.AttemptNumber != attemptNumber || recovery.RunWaitID != runWaitID ||
		strings.TrimSpace(recovery.CorrelationID) == "" {
		return nil, nil, errors.New("manifest recovery_point does not match checkpoint request")
	}
	identity := recovery.Runtime
	if identity.Backend != "firecracker" ||
		deployment.ValidateRuntimeArchitecture(deployment.RuntimeArchitecture(identity.Arch)) != nil ||
		identity.ID != runtimeIdentityID || strings.TrimSpace(identity.ABI) == "" ||
		!taskWorkspaceDigestPattern.MatchString(identity.KernelDigest) ||
		!taskWorkspaceDigestPattern.MatchString(identity.InitramfsDigest) ||
		!taskWorkspaceDigestPattern.MatchString(identity.RootfsDigest) ||
		!taskWorkspaceDigestPattern.MatchString(identity.ConfigDigest) {
		return nil, nil, errors.New("manifest runtime identity is invalid")
	}
	proofs := []checkpointArtifactProof{
		{role: db.RunCheckpointArtifactRoleRuntimeConfig, kind: db.ArtifactKindRunCheckpointConfig, artifact: manifest.RuntimeState.ConfigArtifact},
		{role: db.RunCheckpointArtifactRoleVMState, kind: db.ArtifactKindRunCheckpointVMState, artifact: manifest.RuntimeState.VMStateArtifact},
		{role: db.RunCheckpointArtifactRoleScratchDisk, kind: db.ArtifactKindRunCheckpointScratchDisk, artifact: manifest.RuntimeState.ScratchDiskArtifact},
	}
	for index, artifact := range manifest.RuntimeState.MemoryArtifacts {
		proofs = append(proofs, checkpointArtifactProof{
			role: db.RunCheckpointArtifactRoleMemory, ordinal: int32(index),
			kind: db.ArtifactKindRunCheckpointMemory, artifact: artifact,
		})
	}
	if len(manifest.RuntimeState.MemoryArtifacts) == 0 {
		return nil, nil, errors.New("manifest runtime_state.memory_artifacts is required")
	}
	expectedMedia := map[db.RunCheckpointArtifactRole]string{
		db.RunCheckpointArtifactRoleRuntimeConfig: cas.CheckpointRuntimeConfigMediaType,
		db.RunCheckpointArtifactRoleVMState:       cas.CheckpointVMStateMediaType,
		db.RunCheckpointArtifactRoleScratchDisk:   cas.CheckpointScratchDiskMediaType,
		db.RunCheckpointArtifactRoleMemory:        cas.CheckpointMemoryMediaType,
	}
	for _, proof := range proofs {
		if !taskWorkspaceDigestPattern.MatchString(proof.artifact.Digest) || proof.artifact.SizeBytes <= 0 ||
			proof.artifact.MediaType != expectedMedia[proof.role] {
			return nil, nil, fmt.Errorf("manifest checkpoint artifact %s/%d is invalid", proof.role, proof.ordinal)
		}
	}
	if len(manifest.RuntimeState.Config) == 0 || !json.Valid(manifest.RuntimeState.Config) {
		return nil, nil, errors.New("manifest runtime_state.config must be valid JSON")
	}
	if identity.Substrate != nil &&
		(!taskWorkspaceDigestPattern.MatchString(identity.Substrate.Digest) ||
			strings.TrimSpace(identity.Substrate.Format) == "" ||
			strings.TrimSpace(identity.Substrate.BuilderABI) == "" ||
			strings.TrimSpace(identity.Substrate.LayoutABI) == "") {
		return nil, nil, errors.New("manifest runtime substrate identity is invalid")
	}
	encoded, err := json.Marshal(manifest)
	if err != nil {
		return nil, nil, fmt.Errorf("encode checkpoint manifest: %w", err)
	}
	encoded, err = canonicalJSON(encoded)
	if err != nil || len(encoded) > 65536 {
		if err == nil {
			err = errors.New("checkpoint manifest exceeds 64 KiB")
		}
		return nil, nil, err
	}
	return encoded, proofs, nil
}

func (s *Server) verifyCheckpointRuntimeArtifacts(
	ctx context.Context,
	proofs []checkpointArtifactProof,
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
	return nil
}

func (s *Server) checkpointReadyReplay(ctx context.Context, ready parsedCheckpointReady) (workerapi.CheckpointResponse, bool, error) {
	replay, err := s.db.GetCheckpointReadyReplay(ctx, pgvalue.UUID(ready.checkpointID))
	if errors.Is(err, pgx.ErrNoRows) {
		return workerapi.CheckpointResponse{}, false, nil
	}
	if err != nil {
		return workerapi.CheckpointResponse{}, false, errors.New("load checkpoint-ready replay")
	}
	if replay.RunWaitID != pgvalue.UUID(ready.waitID) || replay.SourceRunLeaseID != pgvalue.UUID(ready.lease.leaseID) ||
		!replay.PrivateWorkspaceVersionID.Valid || !replay.ReadyRequestFingerprint.Valid ||
		replay.ReadyRequestFingerprint.String != ready.fingerprint {
		return workerapi.CheckpointResponse{}, false, conflict(errors.New("checkpoint-ready replay does not match the committed request"))
	}
	return workerapi.CheckpointResponse{
		RunID: pgvalue.UUIDString(replay.RunID), RunWaitID: ready.waitID.String(), CheckpointID: ready.checkpointID.String(),
		WorkspaceVersionID: pgvalue.UUIDString(replay.PrivateWorkspaceVersionID),
	}, true, nil
}

func (s *Server) checkpointFailedReplay(ctx context.Context, failed parsedCheckpointFailed) (workerapi.CheckpointResponse, bool, error) {
	replay, err := s.db.GetCheckpointFailedReplay(ctx, pgvalue.UUID(failed.checkpointID))
	if errors.Is(err, pgx.ErrNoRows) {
		return workerapi.CheckpointResponse{}, false, nil
	}
	if err != nil {
		return workerapi.CheckpointResponse{}, false, errors.New("load checkpoint-failed replay")
	}
	if replay.RunWaitID != pgvalue.UUID(failed.waitID) || replay.SourceRunLeaseID != pgvalue.UUID(failed.lease.leaseID) ||
		!replay.FailedRequestFingerprint.Valid ||
		replay.FailedRequestFingerprint.String != failed.fingerprint {
		return workerapi.CheckpointResponse{}, false, conflict(errors.New("checkpoint-failed replay does not match the committed request"))
	}
	return workerapi.CheckpointResponse{
		RunID: pgvalue.UUIDString(replay.RunID), RunWaitID: failed.waitID.String(), CheckpointID: failed.checkpointID.String(),
	}, true, nil
}

func (s *Server) commitCheckpointReady(
	ctx context.Context,
	worker workerActor,
	request workerapi.CheckpointReadyRequest,
	ready parsedCheckpointReady,
) (workerapi.CheckpointResponse, error) {
	var response workerapi.CheckpointResponse
	err := s.inTx(ctx, func(work *txWork) error {
		locators, err := work.q.GetLiveRunLeaseLocators(ctx, db.GetLiveRunLeaseLocatorsParams{
			ID: pgvalue.UUID(ready.lease.leaseID), LeaseSequence: request.Lease.LeaseSequence,
			WorkerGroupID: worker.WorkerGroupID, WorkerInstanceID: pgvalue.UUID(worker.WorkerInstanceID),
			WorkerEpoch: worker.WorkerEpoch, WorkerProtocolVersion: worker.ProtocolVersion,
		})
		if err != nil {
			return staleRunLeaseClaim(err)
		}
		if _, err := secret.LockAttemptDelivery(ctx, work.q, locators.RunID, locators.AttemptNumber, locators.WorkspaceID); err != nil {
			return fmt.Errorf("lock checkpoint-ready secret authority: %w", err)
		}
		handoffBindings, err := work.q.LockWorkspaceSecretsForAdmission(ctx, locators.WorkspaceID)
		if err != nil {
			return fmt.Errorf("lock checkpoint-ready workspace secrets: %w", err)
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
		authority.parentRun = owner.parent
		if err := validateRunFinalizationOwner(authority, locators); err != nil {
			return staleRunLeaseClaim(err)
		}
		if authority.run.Status != db.RunStatusWaiting ||
			authority.runLease.State != db.RunLeaseStateCheckpointing {
			return errStaleRunLeaseClaim
		}
		wait, err := work.q.LockRunLeaseClaimWait(ctx, db.LockRunLeaseClaimWaitParams{
			ID: pgvalue.UUID(ready.waitID), EnvironmentID: authority.run.EnvironmentID, RunID: authority.run.ID,
			AttemptNumber: authority.attempt.Number, WorkspaceID: authority.workspace.ID,
			CurrentRunLeaseID: authority.runLease.ID,
		})
		if err != nil || (wait.Kind != db.WaitKindToken && wait.Kind != db.WaitKindActorInput &&
			wait.Kind != db.WaitKindChild) ||
			wait.SuspensionState != db.RunWaitStateCheckpointing ||
			wait.CheckpointRequestVersion != request.RequestVersion || wait.SuspendCheckpointID != pgvalue.UUID(ready.checkpointID) {
			return staleRunLeaseClaim(err)
		}
		if err := validateRunWaitActorCursor(authority, wait); err != nil {
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
		if err := validateCheckpointSubstrateAuthority(
			ctx,
			work.q,
			authority,
			request.Manifest,
		); err != nil {
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
		if wait.Kind == db.WaitKindChild && !wait.ChildRunID.Valid {
			if wait.ConditionState != db.WaitStatePending {
				return errStaleRunLeaseClaim
			}
			if err := s.commitSameWorkspaceChildCheckpointReady(
				ctx,
				work.q,
				authority,
				wait,
				workspaceVersionID,
				ready.capture.tree.Digest,
				checkpointedAt,
				request.RequestVersion,
				handoffBindings,
			); err != nil {
				return err
			}
		} else if wait.ConditionState == db.WaitStatePending {
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
				"environmentId": pgvalue.UUIDString(authority.run.EnvironmentID), "runId": pgvalue.UUIDString(authority.run.ID),
				"runWaitId": request.RunWaitID, "resumeRequestVersion": committed.ResumeRequestVersion,
			})
			if _, err := work.q.CreateOutboxMessage(ctx, db.CreateOutboxMessageParams{
				ID: pgvalue.UUID(uuid.Must(uuid.NewV7())), Lane: "control", Topic: "run.resume",
				PartitionKey: pgvalue.UUIDString(authority.workspace.ID), Payload: payload, AvailableAt: checkpointedAt,
			}); err != nil {
				return fmt.Errorf("publish checkpoint-ready resume: %w", err)
			}
		}
		response = workerapi.CheckpointResponse{
			RunID: pgvalue.UUIDString(authority.run.ID), RunWaitID: request.RunWaitID, CheckpointID: request.CheckpointID,
			WorkspaceVersionID: pgvalue.UUIDString(workspaceVersionID),
		}
		return nil
	})
	return response, err
}

func (s *Server) commitSameWorkspaceChildCheckpointReady(
	ctx context.Context,
	store db.Querier,
	authority runLeaseClaimAuthority,
	wait db.RunWait,
	baseWorkspaceVersionID pgtype.UUID,
	baseWorkspaceContentDigest string,
	checkpointedAt pgtype.Timestamptz,
	checkpointRequestVersion int64,
	bindings []db.LockWorkspaceSecretsForAdmissionRow,
) error {
	if !wait.ChildClaimID.Valid ||
		!wait.ChildTargetDeclaredID.Valid ||
		wait.ChildTargetDeclaredID.String == "" ||
		!wait.SuspendCheckpointID.Valid ||
		wait.ChildRunID.Valid ||
		wait.BaseWorkspaceVersionID.Valid ||
		wait.HandoffRuntimeInstanceID.Valid {
		return errStaleRunLeaseClaim
	}
	var request idempotency.TaskChildInvokeFingerprint
	if err := decodeClosedJSON(wait.ChildRequest, &request); err != nil {
		return staleRunLeaseClaim(err)
	}
	if request.Method != "call" {
		return errStaleRunLeaseClaim
	}
	normalized := normalizedTaskStart{
		taskStartRequest: taskStartRequest{
			OrgID:          pgvalue.MustUUIDValue(authority.run.OrgID),
			ProjectID:      pgvalue.MustUUIDValue(authority.run.ProjectID),
			EnvironmentID:  pgvalue.MustUUIDValue(authority.run.EnvironmentID),
			TaskDeclaredID: wait.ChildTargetDeclaredID.String,
			PayloadPresent: request.PayloadPresent,
			Payload:        request.Payload,
			QueueName:      request.QueueName,
			ConcurrencyKey: request.ConcurrencyKey,
			Priority:       request.Priority,
			QueuedTTLMS:    request.QueuedTTLMS,
			RetryPolicy:    request.RetryPolicy,
			Metadata:       request.Metadata,
			Tags:           request.Tags,
		},
	}
	admission, err := loadChildTaskAdmission(
		ctx,
		store,
		authority.run,
		normalized,
	)
	if err != nil {
		return err
	}
	for _, binding := range bindings {
		if binding.SecretState != "active" ||
			!binding.CurrentVersionID.Valid {
			return errTaskSecretUnavailable
		}
	}
	claim, err := store.GetIdempotencyClaim(
		ctx,
		db.GetIdempotencyClaimParams{
			EnvironmentID: authority.run.EnvironmentID,
			ID:            wait.ChildClaimID,
		},
	)
	if err != nil ||
		claim.Operation != "task.child.invoke" ||
		claim.State != "pending" ||
		claim.RetiredAt.Valid {
		return staleRunLeaseClaim(err)
	}
	if !authority.run.QueueOriginAt.Valid || !checkpointedAt.Valid {
		return errStaleRunLeaseClaim
	}
	queuedExpiresAt := pgtype.Timestamptz{}
	if admission.QueuedTTLMS != nil {
		queuedExpiresAt = pgvalue.Timestamptz(
			checkpointedAt.Time.Add(
				time.Duration(*admission.QueuedTTLMS) * time.Millisecond,
			),
		)
	}
	queueScoreAt := pgvalue.Timestamptz(
		authority.run.QueueOriginAt.Time.Add(
			-time.Duration(request.Priority) * time.Second,
		),
	)
	childRunID := uuid.Must(uuid.NewV7())
	rootSpanID, err := tracing.NewSpanID()
	if err != nil {
		return err
	}
	child, err := store.CreateSameWorkspaceChildRunFromParentDeployment(
		ctx,
		db.CreateSameWorkspaceChildRunFromParentDeploymentParams{
			RunWaitID:              wait.ID,
			EntrypointDeclaredID:   wait.ChildTargetDeclaredID,
			ClaimID:                wait.ChildClaimID,
			ParentRunLeaseID:       authority.runLease.ID,
			SuspendCheckpointID:    wait.SuspendCheckpointID,
			BaseWorkspaceVersionID: baseWorkspaceVersionID,
			EnvironmentID:          authority.run.EnvironmentID,
			ParentRunID:            authority.run.ID,
			ParentAttemptNumber:    authority.attempt.Number,
			ID:                     pgvalue.UUID(childRunID),
			Payload:                request.Payload,
			Metadata:               request.Metadata,
			Tags:                   request.Tags,
			QueueName:              admission.QueueName,
			ConcurrencyKey:         pgvalue.TextPtr(request.ConcurrencyKey),
			QueueConcurrencyLimit:  int8Ptr(admission.QueueConcurrencyLimit),
			Priority:               request.Priority,
			QueueOriginAt:          authority.run.QueueOriginAt,
			QueueScoreAt:           queueScoreAt,
			QueuedExpiresAt:        queuedExpiresAt,
			MaxActiveDurationMs:    admission.MaxActiveDurationMS,
			RetryPolicy:            admission.RetryPolicy,
			TraceID:                authority.run.TraceID,
			RootSpanID:             rootSpanID,
		},
	)
	if err != nil {
		return staleRunLeaseClaim(err)
	}
	if err := secret.CreateAttemptResolutions(
		ctx, store, authority.workspace.ID, child.ID, 1, workspaceSecretResolutions(bindings),
	); err != nil {
		return fmt.Errorf(
			"record same-workspace child task secret resolutions: %w",
			err,
		)
	}
	if _, err := store.CommitSameWorkspaceChildCheckpointReady(
		ctx,
		db.CommitSameWorkspaceChildCheckpointReadyParams{
			CheckpointRequestVersion:   checkpointRequestVersion,
			BaseWorkspaceVersionID:     baseWorkspaceVersionID,
			BaseWorkspaceContentDigest: pgvalue.Text(baseWorkspaceContentDigest),
			RuntimeInstanceID:          authority.runtime.ID,
			WorkspaceMountID:           authority.workspaceMount.ID,
			MountGeneration: pgtype.Int8{
				Int64: authority.workspaceLease.MountFencingGeneration,
				Valid: true,
			},
			OwnershipGeneration: pgtype.Int8{
				Int64: authority.workspaceLease.OwnershipGeneration,
				Valid: true,
			},
			ParentWriterGeneration: pgtype.Int8{
				Int64: authority.workspaceLease.WriterGeneration,
				Valid: true,
			},
			CheckpointedAt:          checkpointedAt,
			RunWaitID:               wait.ID,
			EnvironmentID:           authority.run.EnvironmentID,
			ParentRunID:             authority.run.ID,
			WorkspaceID:             authority.workspace.ID,
			ParentAttemptNumber:     authority.attempt.Number,
			ChildClaimID:            wait.ChildClaimID,
			ParentRunLeaseID:        authority.runLease.ID,
			SuspendCheckpointID:     wait.SuspendCheckpointID,
			ChildRunID:              child.ID,
			ExpectedRunStateVersion: wait.ExpectedRunStateVersion,
		},
	); err != nil {
		return staleRunLeaseClaim(err)
	}
	receipt, err := json.Marshal(childTaskReceipt{
		RunID:                  childRunID.String(),
		WorkspaceID:            pgvalue.UUIDString(authority.workspace.ID),
		RunWaitID:              pgvalue.UUIDString(wait.ID),
		ResumeAttachID:         pgvalue.UUIDString(wait.ResumeAttachID),
		BaseWorkspaceVersionID: pgvalue.UUIDString(baseWorkspaceVersionID),
		BaseWorkspaceDigest:    baseWorkspaceContentDigest,
	})
	if err != nil {
		return err
	}
	claims, err := idempotency.TransactionForQueries(store)
	if err != nil {
		return err
	}
	if _, err := claims.Complete(ctx, claim, receipt); err != nil {
		return err
	}
	if _, err := store.CreateRunAdmissionOutbox(
		ctx,
		db.CreateRunAdmissionOutboxParams{
			ID:            pgvalue.UUID(uuid.Must(uuid.NewV7())),
			WorkspaceID:   authority.workspace.ID,
			EnvironmentID: authority.run.EnvironmentID,
			RunID:         child.ID,
		},
	); err != nil {
		return fmt.Errorf(
			"create same-workspace child task admission outbox: %w",
			err,
		)
	}
	return nil
}

func validateCheckpointSubstrateAuthority(
	ctx context.Context,
	store interface {
		GetRuntimeSubstrateForCheckpoint(context.Context, pgtype.UUID) (db.RuntimeSubstrate, error)
	},
	authority runLeaseClaimAuthority,
	manifest workerapi.CheckpointManifest,
) error {
	identity := manifest.RecoveryPoint.Runtime.Substrate
	if !authority.runtime.RuntimeSubstrateID.Valid {
		if identity != nil {
			return errStaleRunLeaseClaim
		}
		return nil
	}
	if identity == nil {
		return errStaleRunLeaseClaim
	}
	substrate, err := store.GetRuntimeSubstrateForCheckpoint(
		ctx,
		authority.runtime.RuntimeSubstrateID,
	)
	if err != nil {
		return staleRunLeaseClaim(err)
	}
	if substrate.ID != authority.runtime.RuntimeSubstrateID ||
		substrate.OrgID != authority.run.OrgID ||
		substrate.ProjectID != authority.run.ProjectID ||
		substrate.EnvironmentID != authority.run.EnvironmentID ||
		substrate.DeploymentDefinitionID != authority.runtime.DeploymentDefinitionID ||
		substrate.SubstrateDigest != identity.Digest ||
		substrate.SubstrateFormat != identity.Format ||
		substrate.BuilderAbi != identity.BuilderABI ||
		substrate.LayoutAbi != identity.LayoutABI {
		return errStaleRunLeaseClaim
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
		return pgtype.UUID{}, fmt.Errorf("record checkpoint workspace CAS object: %w", err)
	}
	artifactRow, err := store.CreateArtifact(ctx, db.CreateArtifactParams{
		ID: pgvalue.UUID(uuid.Must(uuid.NewV7())), OrgID: authority.run.OrgID,
		ProjectID: authority.run.ProjectID, EnvironmentID: authority.run.EnvironmentID,
		Digest: artifact.Digest, Kind: db.ArtifactKindWorkspaceVersion, SizeBytes: artifact.SizeBytes,
		MediaType: artifact.MediaType, CreatedByWorkerInstanceID: pgvalue.UUID(worker.WorkerInstanceID),
	})
	if err != nil {
		return pgtype.UUID{}, fmt.Errorf("record checkpoint workspace artifact: %w", err)
	}
	version, err := store.CreatePrivateCheckpointWorkspaceVersion(ctx, db.CreatePrivateCheckpointWorkspaceVersionParams{
		ID:            pgvalue.UUID(uuid.Must(uuid.NewV7())),
		EnvironmentID: authority.run.EnvironmentID,
		WorkspaceID:   authority.workspace.ID, ParentVersionID: authority.workspaceLease.BaseVersionID,
		ArtifactID: artifactRow.ID, ContentDigest: capture.tree.Digest,
		SizeBytes: capture.tree.SizeBytes, EntryCount: int32(capture.tree.EntryCount),
		SourceWorkspaceLeaseID: authority.workspaceLease.ID,
		OwnershipGeneration:    authority.workspace.OwnershipGeneration, WriterGeneration: authority.workspace.WriterGeneration,
	})
	if err != nil {
		return pgtype.UUID{}, fmt.Errorf("record private checkpoint workspace version: %w", err)
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
			return fmt.Errorf("record checkpoint artifact %s/%d: %w", proof.role, proof.ordinal, err)
		}
		if _, err := store.AddRunCheckpointArtifact(ctx, db.AddRunCheckpointArtifactParams{
			RunCheckpointID: pgvalue.UUID(checkpointID), Role: proof.role, Ordinal: proof.ordinal, ArtifactID: artifact.ID,
		}); err != nil {
			return fmt.Errorf("record checkpoint artifact membership %s/%d: %w", proof.role, proof.ordinal, err)
		}
	}
	return nil
}
