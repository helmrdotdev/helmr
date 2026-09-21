package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/deployment"
	"github.com/helmrdotdev/helmr/internal/idempotency"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/helmrdotdev/helmr/internal/telemetry"
	"github.com/helmrdotdev/helmr/internal/tracing"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

type parsedCheckpointReady struct {
	lease          parsedRunLeaseFence
	waitID         uuid.UUID
	checkpointID   uuid.UUID
	computer       workerapi.CheckpointComputer
	manifest       []byte
	fingerprint    string
	artifacts      checkpointArtifactProofs
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
	role      string
	kind      db.ArtifactKind
	mediaType string
	artifact  workerapi.CheckpointArtifact
}

type checkpointArtifactProofs struct {
	runtimeConfig checkpointArtifactProof
	vmState       checkpointArtifactProof
	memory        checkpointArtifactProof
	scratchDisk   checkpointArtifactProof
}

func (proofs checkpointArtifactProofs) all() [4]checkpointArtifactProof {
	return [4]checkpointArtifactProof{
		proofs.runtimeConfig,
		proofs.vmState,
		proofs.memory,
		proofs.scratchDisk,
	}
}

type checkpointArtifactIDs struct {
	runtimeConfig pgtype.UUID
	vmState       pgtype.UUID
	memory        pgtype.UUID
	scratchDisk   pgtype.UUID
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
	if err := s.verifyCheckpointArtifacts(r.Context(), parsed.computer, parsed.artifacts); err != nil {
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
		if writeStaleWorkerClaims(w, err) {
			return
		}
		if errors.Is(err, errStaleRunLeaseClaim) || errors.Is(err, pgx.ErrNoRows) {
			writeError(w, conflict(errors.New("worker checkpoint-ready receipt is stale")))
			return
		}
		if isDeterministicWorkerAdmission(err) {
			s.log.Warn("worker checkpoint-ready admission rejected", "run_lease_id", request.Lease.ID, "error", err)
			writeError(w, apiError{kind: errUnprocessable, err: errors.New("worker checkpoint-ready admission is invalid")})
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
			WorkerGroupID: pgvalue.UUID(worker.WorkerGroupID), WorkerInstanceID: pgvalue.UUID(worker.WorkerInstanceID),
			WorkerEpoch: worker.WorkerEpoch})
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
		authority, err := lockCheckpointFailureAuthority(
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
			authority.runLease.Status != db.RunLeaseStatusCheckpointing {
			return errStaleRunLeaseClaim
		}
		wait, err := work.q.LockRunLeaseClaimWait(r.Context(), db.LockRunLeaseClaimWaitParams{
			ID: pgvalue.UUID(parsed.waitID), EnvironmentID: authority.run.EnvironmentID, RunID: authority.run.ID,
			AttemptNumber: authority.attempt.Number, WorkspaceID: authority.workspace.ID,
			CurrentRunLeaseID: authority.runLease.ID,
		})
		if err != nil || wait.SuspensionStatus != db.RunWaitStatusCheckpointing ||
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
				r.Context(), work.q, ownedGraph, worker, authority, wait, parsed,
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
	if writeStaleWorkerClaims(w, err) {
		return
	}
	if errors.Is(err, errStaleRunLeaseClaim) || errors.Is(err, pgx.ErrNoRows) {
		writeError(w, conflict(errors.New("worker checkpoint-failed receipt is stale")))
		return
	}
	if isDeterministicWorkerAdmission(err) {
		s.log.Warn("worker checkpoint-failed admission rejected", "run_lease_id", request.Lease.ID, "error", err)
		writeError(w, apiError{kind: errUnprocessable, err: errors.New("worker checkpoint-failed admission is invalid")})
		return
	}
	if err != nil {
		s.log.Error("commit worker checkpoint-failed failed", "run_lease_id", request.Lease.ID, "error", err)
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
			return deterministicWorkerAdmission(err)
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
		BaseWorkspaceVersionID: authority.workspaceLease.BaseWorkspaceVersionID,
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
	return finishCheckpointFailedTask(ctx, store, authority, failedAt, reason)
}

func failCheckpointActorAttempt(
	ctx context.Context,
	store db.Querier,
	ownedGraph run.OwnedFinalization,
	worker workerActor,
	authority runLeaseClaimAuthority,
	wait db.RunWait,
	failed parsedCheckpointFailed,
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

	reason := "checkpoint_failed"
	if activeElapsed >= authority.run.MaxActiveDurationMs {
		reason = "max_active_duration_exceeded"
	}
	if _, err := run.HoldSessionExecution(ctx, store, authority.actor, authority.attempt.Number, "recovery_required"); err != nil {
		return err
	}
	if _, err := ownedGraph.CancelDescendants(ctx); err != nil {
		return fmt.Errorf("cancel child tasks after actor checkpoint failure: %w", err)
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
		ReasonCode: pgvalue.Text(reason), Error: failed.errorPayload, CompletedAt: failedAt,
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
		BaseWorkspaceVersionID: authority.workspaceLease.BaseWorkspaceVersionID,
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
	return finishCheckpointFailedActor(ctx, store, authority, failedAt, reason)
}

func finishCheckpointFailedActor(
	ctx context.Context,
	store db.Querier,
	authority runLeaseClaimAuthority,
	failedAt pgtype.Timestamptz,
	reason string,
) error {
	status := db.RunStatusSystemFailed
	eventKind := api.RunEventKindFailed
	if reason == "max_active_duration_exceeded" {
		status = db.RunStatusExpired
		eventKind = api.RunEventKindExpired
	}
	runFailureValue, err := runFailure(reason, "Run failed during checkpoint recovery")
	if err != nil {
		return err
	}
	if _, err := store.FinishCheckpointFailedActorRun(ctx, db.FinishCheckpointFailedActorRunParams{
		Status: status, Failure: runFailureValue, FailedAt: failedAt,
		ID: authority.run.ID, WorkspaceID: authority.workspace.ID, SessionID: authority.actor.ID,
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
	if err := telemetry.ValidateEvent(eventKind, payload); err != nil {
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
	failure, err := runFailure(reason, "Run failed during checkpoint recovery")
	if err != nil {
		return err
	}
	if _, err := store.FinishCheckpointFailedTaskRun(ctx, db.FinishCheckpointFailedTaskRunParams{
		Status: status, Failure: failure,
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
	if err := telemetry.ValidateEvent(eventKind, payload); err != nil {
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
		terminalRun.Failure = failure
		if err := resolveParentOwnedChildWait(
			ctx, store, authority, terminalRun,
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
		computer: *request.Manifest.RuntimeState.Computer,
		manifest: manifest, fingerprint: fingerprint, artifacts: artifacts,
		requestVersion: request.RequestVersion,
	}, normalized, nil
}

func validateCheckpointReadyManifest(request workerapi.CheckpointReadyRequest) ([]byte, checkpointArtifactProofs, error) {
	disk := request.Manifest.RuntimeState.Computer
	if disk == nil {
		return nil, checkpointArtifactProofs{}, errors.New("checkpoint requires a Computer disk")
	}
	if _, err := parseCanonicalUUID("computer_id", disk.ComputerID); err != nil {
		return nil, checkpointArtifactProofs{}, err
	}
	if err := checkpointDiskArtifact(*disk).Validate(disk.LogicalBytes); err != nil {
		return nil, checkpointArtifactProofs{}, err
	}
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
) ([]byte, checkpointArtifactProofs, error) {
	recovery := manifest.RecoveryPoint
	if recovery.ID != checkpointID || recovery.RunID != runID ||
		recovery.AttemptNumber != attemptNumber || recovery.RunWaitID != runWaitID ||
		strings.TrimSpace(recovery.CorrelationID) == "" {
		return nil, checkpointArtifactProofs{}, errors.New("manifest recovery_point does not match checkpoint request")
	}
	identity := recovery.Runtime
	if identity.Backend != "firecracker" ||
		deployment.ValidateRuntimeArchitecture(deployment.RuntimeArchitecture(identity.Arch)) != nil ||
		identity.ID != runtimeIdentityID || strings.TrimSpace(identity.Contract) == "" ||
		!taskWorkspaceDigestPattern.MatchString(identity.KernelDigest) ||
		!taskWorkspaceDigestPattern.MatchString(identity.InitramfsDigest) ||
		!taskWorkspaceDigestPattern.MatchString(identity.RootfsDigest) ||
		!taskWorkspaceDigestPattern.MatchString(identity.ConfigDigest) ||
		identity.VMVCPUCount <= 0 ||
		!taskWorkspaceDigestPattern.MatchString(identity.CPUConfigDigest) {
		return nil, checkpointArtifactProofs{}, errors.New("manifest runtime identity is invalid")
	}
	if len(manifest.RuntimeState.MemoryArtifacts) != 1 {
		return nil, checkpointArtifactProofs{}, errors.New("manifest runtime_state.memory_artifacts must contain exactly one artifact")
	}
	proofs := checkpointArtifactProofs{
		runtimeConfig: checkpointArtifactProof{
			role: "runtime_config", kind: db.ArtifactKindRunCheckpointConfig,
			mediaType: cas.CheckpointRuntimeConfigMediaType, artifact: manifest.RuntimeState.ConfigArtifact,
		},
		vmState: checkpointArtifactProof{
			role: "vm_state", kind: db.ArtifactKindRunCheckpointVMState,
			mediaType: cas.CheckpointVMStateMediaType, artifact: manifest.RuntimeState.VMStateArtifact,
		},
		memory: checkpointArtifactProof{
			role: "memory", kind: db.ArtifactKindRunCheckpointMemory,
			mediaType: cas.CheckpointMemoryMediaType, artifact: manifest.RuntimeState.MemoryArtifacts[0],
		},
		scratchDisk: checkpointArtifactProof{
			role: "scratch_disk", kind: db.ArtifactKindRunCheckpointScratchDisk,
			mediaType: cas.CheckpointScratchDiskMediaType, artifact: manifest.RuntimeState.ScratchDiskArtifact,
		},
	}
	for _, proof := range proofs.all() {
		if !taskWorkspaceDigestPattern.MatchString(proof.artifact.Digest) || proof.artifact.SizeBytes <= 0 ||
			proof.artifact.MediaType != proof.mediaType {
			return nil, checkpointArtifactProofs{}, fmt.Errorf("manifest checkpoint artifact %s is invalid", proof.role)
		}
	}
	if len(manifest.RuntimeState.Config) == 0 || !json.Valid(manifest.RuntimeState.Config) {
		return nil, checkpointArtifactProofs{}, errors.New("manifest runtime_state.config must be valid JSON")
	}
	if identity.Substrate != nil &&
		(!taskWorkspaceDigestPattern.MatchString(identity.Substrate.Digest) ||
			strings.TrimSpace(identity.Substrate.Format) == "" ||
			strings.TrimSpace(identity.Substrate.Contract) == "") {
		return nil, checkpointArtifactProofs{}, errors.New("manifest runtime substrate identity is invalid")
	}
	encoded, err := json.Marshal(manifest)
	if err != nil {
		return nil, checkpointArtifactProofs{}, fmt.Errorf("encode checkpoint manifest: %w", err)
	}
	encoded, err = canonicalJSON(encoded)
	if err != nil || len(encoded) > 65536 {
		if err == nil {
			err = errors.New("checkpoint manifest exceeds 64 KiB")
		}
		return nil, checkpointArtifactProofs{}, err
	}
	return encoded, proofs, nil
}

func (s *Server) verifyCheckpointArtifacts(
	ctx context.Context,
	disk workerapi.CheckpointComputer,
	proofs checkpointArtifactProofs,
) error {
	if s.cas == nil {
		return errors.New("checkpoint CAS is not configured")
	}
	runtime := proofs.all()
	objects := append([]checkpointArtifactProof{{role: "computer", artifact: disk.Artifact}}, runtime[:]...)
	for _, proof := range objects {
		object, err := s.cas.Stat(ctx, proof.artifact.Digest)
		if err != nil {
			return fmt.Errorf("checkpoint artifact %s is missing from CAS: %w", proof.role, err)
		}
		if object.Digest != proof.artifact.Digest || object.SizeBytes != proof.artifact.SizeBytes || object.MediaType != proof.artifact.MediaType {
			return fmt.Errorf("checkpoint artifact %s does not match CAS authority", proof.role)
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
		source, err := lockCheckpointSource(ctx, work, worker, ready.lease, request.Lease.LeaseSequence,
			ready.waitID, ready.checkpointID, request.RequestVersion, request.Manifest)
		if err != nil {
			return err
		}
		authority, wait, checkpointedAt := source.authority, source.wait, source.checkpointedAt
		if ready.computer.ComputerID != pgvalue.UUIDString(authority.workspace.ID) || checkpointDiskArtifact(ready.computer).Validate(authority.runtime.ReservedGuestEphemeralDiskBytes) != nil {
			return errStaleRunLeaseClaim
		}
		candidate := request.Manifest
		candidate.Phases = nil
		encoded, err := json.Marshal(candidate)
		if err != nil {
			return err
		}
		if _, err := work.q.RequireRegisteredCheckpointManifest(ctx, db.RequireRegisteredCheckpointManifestParams{ID: pgvalue.UUID(ready.checkpointID), Manifest: encoded}); err != nil {
			return staleRunLeaseClaim(err)
		}
		workspaceVersionID, err := recordCheckpointComputerVersion(ctx, work.q, worker, authority, ready.computer)
		if err != nil {
			return err
		}
		artifactIDs, err := recordCheckpointRuntimeArtifacts(ctx, work.q, worker, authority, ready.artifacts)
		if err != nil {
			return err
		}
		if _, err := work.q.MarkRunCheckpointReady(ctx, db.MarkRunCheckpointReadyParams{
			PrivateWorkspaceVersionID: workspaceVersionID, RestoreManifest: ready.manifest,
			RuntimeConfigArtifactID: artifactIDs.runtimeConfig, VMStateArtifactID: artifactIDs.vmState,
			MemoryArtifactID: artifactIDs.memory, ScratchDiskArtifactID: artifactIDs.scratchDisk,
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
			OwnerRunLeaseID: authority.runLease.ID, BaseWorkspaceVersionID: authority.workspaceLease.BaseWorkspaceVersionID,
			OwnershipGeneration:    authority.workspaceLease.OwnershipGeneration,
			WriterGeneration:       authority.workspaceLease.WriterGeneration,
			MountFencingGeneration: authority.workspaceLease.MountFencingGeneration,
		}); err != nil {
			return staleRunLeaseClaim(err)
		}
		if _, err := work.q.DetachCheckpointSource(ctx, db.DetachCheckpointSourceParams{
			CheckpointedAt:          checkpointedAt,
			WorkspaceMountID:        authority.workspaceMount.ID,
			RuntimeInstanceID:       authority.runtime.ID,
			WorkerInstanceID:        authority.runtime.WorkerInstanceID,
			WorkerEpoch:             authority.runtime.WorkerEpoch,
			MountFencingGeneration:  authority.workspaceMount.FencingGeneration,
			ExpectedDesiredVersion:  authority.runtime.DesiredVersion,
			ExpectedObservedVersion: authority.runtime.ObservedVersion,
		}); err != nil {
			return staleRunLeaseClaim(err)
		}
		if wait.Kind == db.WaitKindChild && !wait.ChildRunID.Valid {
			if wait.ConditionStatus != db.WaitStatusPending {
				return errStaleRunLeaseClaim
			}
			if err := s.commitSameWorkspaceChildCheckpointReady(
				ctx,
				work.q,
				authority,
				wait,
				workspaceVersionID,
				ready.computer.Artifact.Digest,
				checkpointedAt,
				request.RequestVersion,
				source.workspaceBindings,
			); err != nil {
				return err
			}
		} else if wait.ConditionStatus == db.WaitStatusPending {
			if _, err := work.q.CommitPendingCheckpointReady(ctx, db.CommitPendingCheckpointReadyParams{
				CheckpointedAt: checkpointedAt, RunID: authority.run.ID, WorkspaceID: authority.workspace.ID,
				AttemptNumber: authority.attempt.Number, RunLeaseID: authority.runLease.ID,
				ExpectedRunRevision: wait.ExpectedRunRevision, CheckpointRequestVersion: request.RequestVersion,
				RunWaitID: wait.ID, CheckpointID: pgvalue.UUID(ready.checkpointID),
			}); err != nil {
				return staleRunLeaseClaim(err)
			}
		} else {
			_, err := work.q.CommitTerminalCheckpointReady(ctx, db.CommitTerminalCheckpointReadyParams{
				CheckpointedAt: checkpointedAt, RunID: authority.run.ID, WorkspaceID: authority.workspace.ID,
				AttemptNumber: authority.attempt.Number, RunLeaseID: authority.runLease.ID,
				ExpectedRunRevision: wait.ExpectedRunRevision, CheckpointRequestVersion: request.RequestVersion,
				RunWaitID: wait.ID, CheckpointID: pgvalue.UUID(ready.checkpointID),
			})
			if err != nil {
				return staleRunLeaseClaim(err)
			}
		}
		now, err := work.q.GetRunLeaseRenewalTime(ctx)
		if err != nil {
			return err
		}
		if !now.Valid || !now.Time.Before(authority.runLease.ExpiresAt.Time) || source.expiresAt.Valid && !now.Time.Before(source.expiresAt.Time) {
			return errStaleRunLeaseClaim
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
		wait.BaseWorkspaceVersionID.Valid {
		return errStaleRunLeaseClaim
	}
	var request idempotency.TaskChildInvokeFingerprint
	if err := decodeClosedJSON(wait.ChildRequest, &request); err != nil {
		return deterministicWorkerAdmission(fmt.Errorf("decode pinned child task request: %w", err))
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
		if errors.Is(err, errTaskNotDeployed) || errors.Is(err, errTaskStartAuthority) ||
			errors.Is(err, errTaskPayloadPresenceInvalid) {
			return deterministicWorkerAdmission(err)
		}
		return err
	}
	for _, binding := range bindings {
		if binding.SecretStatus != "active" ||
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
		claim.Status != "pending" ||
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
	childRunID := uuid.NewV7()
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
			OwnershipGeneration: pgtype.Int8{
				Int64: authority.workspaceLease.OwnershipGeneration,
				Valid: true,
			},
			ParentWriterGeneration: pgtype.Int8{
				Int64: authority.workspaceLease.WriterGeneration,
				Valid: true,
			},
			CheckpointedAt:      checkpointedAt,
			RunWaitID:           wait.ID,
			EnvironmentID:       authority.run.EnvironmentID,
			ParentRunID:         authority.run.ID,
			WorkspaceID:         authority.workspace.ID,
			ParentAttemptNumber: authority.attempt.Number,
			ChildClaimID:        wait.ChildClaimID,
			ParentRunLeaseID:    authority.runLease.ID,
			SuspendCheckpointID: wait.SuspendCheckpointID,
			ChildRunID:          child.ID,
			ExpectedRunRevision: wait.ExpectedRunRevision,
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
		substrate.SubstrateContract != identity.Contract ||
		substrate.SubstrateSizeBytes != identity.SizeBytes ||
		identity.SizeBytes <= 0 {
		return errStaleRunLeaseClaim
	}
	return nil
}

func validateCheckpointRuntimeShapeAuthority(
	runtime db.RuntimeInstance,
	manifest workerapi.CheckpointManifest,
) error {
	shape := manifest.RecoveryPoint.Runtime
	if shape.VMVCPUCount != runtime.VMVCPUCount ||
		shape.CPUConfigDigest != runtime.CPUConfigDigest {
		return errStaleRunLeaseClaim
	}
	return nil
}

func recordCheckpointComputerVersion(
	ctx context.Context,
	store db.Querier,
	worker workerActor,
	authority runLeaseClaimAuthority,
	disk workerapi.CheckpointComputer,
) (pgtype.UUID, error) {
	artifact := disk.Artifact
	if _, err := store.UpsertCasObject(ctx, db.UpsertCasObjectParams{
		OrgID: authority.run.OrgID, Digest: artifact.Digest, SizeBytes: artifact.SizeBytes, MediaType: artifact.MediaType,
	}); err != nil {
		return pgtype.UUID{}, fmt.Errorf("record checkpoint Computer CAS object: %w", err)
	}
	artifactRow, err := store.CreateArtifact(ctx, db.CreateArtifactParams{
		ID: pgvalue.UUID(uuid.NewV7()), OrgID: authority.run.OrgID,
		ProjectID: authority.run.ProjectID, EnvironmentID: authority.run.EnvironmentID,
		Digest: artifact.Digest, Kind: db.ArtifactKindWorkspaceVersion, SizeBytes: artifact.SizeBytes,
		MediaType: artifact.MediaType, CreatedByWorkerInstanceID: pgvalue.UUID(worker.WorkerInstanceID),
	})
	if err != nil {
		return pgtype.UUID{}, fmt.Errorf("record checkpoint Computer artifact: %w", err)
	}
	version, err := store.CreatePrivateCheckpointWorkspaceVersion(ctx, db.CreatePrivateCheckpointWorkspaceVersionParams{
		ID:            pgvalue.UUID(uuid.NewV7()),
		EnvironmentID: authority.run.EnvironmentID,
		WorkspaceID:   authority.workspace.ID, ParentVersionID: authority.workspaceLease.BaseWorkspaceVersionID,
		ArtifactID: artifactRow.ID, ContentDigest: pgvalue.Text(artifact.Digest),
		SizeBytes: disk.LogicalBytes, EntryCount: 0,
		SourceWorkspaceLeaseID: authority.workspaceLease.ID,
		OwnershipGeneration:    authority.workspace.OwnershipGeneration, WriterGeneration: authority.workspace.WriterGeneration,
	})
	if err != nil {
		return pgtype.UUID{}, fmt.Errorf("record private checkpoint Computer version: %w", err)
	}
	return version.ID, nil
}

func recordCheckpointRuntimeArtifacts(
	ctx context.Context,
	store db.Querier,
	worker workerActor,
	authority runLeaseClaimAuthority,
	proofs checkpointArtifactProofs,
) (checkpointArtifactIDs, error) {
	var ids checkpointArtifactIDs
	artifacts := [4]struct {
		proof checkpointArtifactProof
		id    *pgtype.UUID
	}{
		{proof: proofs.runtimeConfig, id: &ids.runtimeConfig},
		{proof: proofs.vmState, id: &ids.vmState},
		{proof: proofs.memory, id: &ids.memory},
		{proof: proofs.scratchDisk, id: &ids.scratchDisk},
	}
	for _, item := range artifacts {
		artifactID, err := recordCheckpointRuntimeArtifact(ctx, store, worker, authority, item.proof)
		if err != nil {
			return checkpointArtifactIDs{}, err
		}
		*item.id = artifactID
	}
	return ids, nil
}

func recordCheckpointRuntimeArtifact(
	ctx context.Context,
	store db.Querier,
	worker workerActor,
	authority runLeaseClaimAuthority,
	proof checkpointArtifactProof,
) (pgtype.UUID, error) {
	if _, err := store.UpsertCasObject(ctx, db.UpsertCasObjectParams{
		OrgID: authority.run.OrgID, Digest: proof.artifact.Digest,
		SizeBytes: proof.artifact.SizeBytes, MediaType: proof.artifact.MediaType,
	}); err != nil {
		return pgtype.UUID{}, fmt.Errorf("record checkpoint CAS object %s: %w", proof.role, err)
	}
	artifact, err := store.CreateArtifact(ctx, db.CreateArtifactParams{
		ID: pgvalue.UUID(uuid.NewV7()), OrgID: authority.run.OrgID,
		ProjectID: authority.run.ProjectID, EnvironmentID: authority.run.EnvironmentID,
		Digest: proof.artifact.Digest, Kind: proof.kind, SizeBytes: proof.artifact.SizeBytes,
		MediaType: proof.artifact.MediaType, CreatedByWorkerInstanceID: pgvalue.UUID(worker.WorkerInstanceID),
	})
	if err != nil {
		return pgtype.UUID{}, fmt.Errorf("record checkpoint artifact %s: %w", proof.role, err)
	}
	return artifact.ID, nil
}

type checkpointSource struct {
	expiresAt         pgtype.Timestamptz
	authority         runLeaseClaimAuthority
	wait              db.RunWait
	workspaceBindings []db.LockWorkspaceSecretsForAdmissionRow
	checkpointedAt    pgtype.Timestamptz
}

func lockCheckpointSource(ctx context.Context, work *txWork, worker workerActor,
	lease parsedRunLeaseFence, leaseSequence int64, waitID, checkpointID uuid.UUID,
	requestVersion int64, manifest workerapi.CheckpointManifest,
) (checkpointSource, error) {
	locators, err := work.q.GetLiveRunLeaseLocators(ctx, db.GetLiveRunLeaseLocatorsParams{
		ID: pgvalue.UUID(lease.leaseID), LeaseSequence: leaseSequence,
		WorkerGroupID: pgvalue.UUID(worker.WorkerGroupID), WorkerInstanceID: pgvalue.UUID(worker.WorkerInstanceID),
		WorkerEpoch: worker.WorkerEpoch})
	if err != nil {
		return checkpointSource{}, staleRunLeaseClaim(err)
	}
	if _, err := secret.LockAttemptDelivery(ctx, work.q, locators.RunID, locators.AttemptNumber, locators.WorkspaceID); err != nil {
		return checkpointSource{}, fmt.Errorf("lock checkpoint-ready secret authority: %w", err)
	}
	workspaceBindings, err := work.q.LockWorkspaceSecretsForAdmission(ctx, locators.WorkspaceID)
	if err != nil {
		return checkpointSource{}, fmt.Errorf("lock checkpoint-ready workspace secrets: %w", err)
	}
	owner, err := lockRunFinalizationOwner(ctx, work.q, locators)
	if err != nil {
		return checkpointSource{}, err
	}
	authority, err := lockRenewableRunLeaseAuthority(
		ctx, work.q, worker, pgvalue.UUID(lease.leaseID), leaseSequence, locators,
	)
	if err != nil {
		return checkpointSource{}, err
	}
	sourcePoolID := authority.worker.WorkerPoolID
	sourcePool, err := work.q.LockWorkerPool(ctx, db.LockWorkerPoolParams{
		WorkerGroupID: pgvalue.UUID(worker.WorkerGroupID),
		WorkerPoolID:  sourcePoolID,
	})
	if err != nil {
		return checkpointSource{}, staleRunLeaseClaim(err)
	}
	if sourcePool.Status != "active" && sourcePool.Status != "draining" {
		return checkpointSource{}, errStaleRunLeaseClaim
	}
	authority.actor = owner.actor
	authority.parentRun = owner.parent
	if err := validateRunFinalizationOwner(authority, locators); err != nil {
		return checkpointSource{}, staleRunLeaseClaim(err)
	}
	if authority.run.Status != db.RunStatusWaiting ||
		authority.runLease.Status != db.RunLeaseStatusCheckpointing {
		return checkpointSource{}, errStaleRunLeaseClaim
	}
	wait, err := work.q.LockRunLeaseClaimWait(ctx, db.LockRunLeaseClaimWaitParams{
		ID: pgvalue.UUID(waitID), EnvironmentID: authority.run.EnvironmentID, RunID: authority.run.ID,
		AttemptNumber: authority.attempt.Number, WorkspaceID: authority.workspace.ID,
		CurrentRunLeaseID: authority.runLease.ID,
	})
	if err != nil || wait.SuspensionStatus != db.RunWaitStatusCheckpointing ||
		wait.CheckpointRequestVersion != requestVersion || wait.SuspendCheckpointID != pgvalue.UUID(checkpointID) {
		return checkpointSource{}, staleRunLeaseClaim(err)
	}
	if err := validateRunWaitActorCursor(authority, wait); err != nil {
		return checkpointSource{}, err
	}
	checkpoint, err := work.q.LockCreatingRunCheckpoint(ctx, db.LockCreatingRunCheckpointParams{
		ID: pgvalue.UUID(checkpointID), RunID: authority.run.ID, AttemptNumber: authority.attempt.Number,
		RunWaitID: wait.ID, SourceRunLeaseID: authority.runLease.ID,
		SourceWorkspaceLeaseID: authority.workspaceLease.ID, WorkspaceID: authority.workspace.ID,
	})
	if err != nil || checkpoint.BaseWorkspaceVersionID != authority.workspaceLease.BaseWorkspaceVersionID ||
		checkpoint.ActorSpeculativeInputSequence != wait.ActorSpeculativeInputSequence {
		return checkpointSource{}, staleRunLeaseClaim(err)
	}
	identity, err := work.q.GetRuntimeIdentityForCheckpoint(ctx, authority.runtime.RuntimeIdentityID)
	if err != nil || identity.ID != manifest.RecoveryPoint.Runtime.ID ||
		identity.RuntimeArch != manifest.RecoveryPoint.Runtime.Arch || identity.VMRuntimeContract != manifest.RecoveryPoint.Runtime.Contract ||
		identity.KernelDigest != manifest.RecoveryPoint.Runtime.KernelDigest ||
		identity.InitramfsDigest != manifest.RecoveryPoint.Runtime.InitramfsDigest ||
		identity.RootfsDigest != manifest.RecoveryPoint.Runtime.RootfsDigest {
		return checkpointSource{}, staleRunLeaseClaim(err)
	}
	if err := validateCheckpointRuntimeShapeAuthority(authority.runtime, manifest); err != nil {
		return checkpointSource{}, err
	}
	if err := validateCheckpointSubstrateAuthority(
		ctx,
		work.q,
		authority,
		manifest,
	); err != nil {
		return checkpointSource{}, err
	}
	if _, err := work.q.RequireCheckpointRestoreSupplier(ctx, db.RequireCheckpointRestoreSupplierParams{
		SourceRunLeaseID:   authority.runLease.ID,
		WorkerGroupID:      pgvalue.UUID(worker.WorkerGroupID),
		WorkerInstanceID:   pgvalue.UUID(worker.WorkerInstanceID),
		WorkerEpoch:        worker.WorkerEpoch,
		SourceWorkerPoolID: sourcePoolID,
	}); err != nil {
		return checkpointSource{}, staleRunLeaseClaim(err)
	}
	checkpointedAt, err := work.q.GetRunLeaseRenewalTime(ctx)
	if err != nil || !checkpointedAt.Valid || !checkpointedAt.Time.Before(authority.runLease.ExpiresAt.Time) || (checkpoint.ExpiresAt.Valid && !checkpointedAt.Time.Before(checkpoint.ExpiresAt.Time)) {
		return checkpointSource{}, staleRunLeaseClaim(err)
	}

	if _, _, err := validateCheckpointManifest(manifest, checkpointID.String(),
		pgvalue.UUIDString(authority.run.ID), authority.attempt.Number, waitID.String(), authority.runtime.RuntimeIdentityID); err != nil {
		return checkpointSource{}, errStaleRunLeaseClaim
	}
	return checkpointSource{authority: authority, wait: wait, checkpointedAt: checkpointedAt, workspaceBindings: workspaceBindings, expiresAt: checkpoint.ExpiresAt}, nil
}

func checkpointDiskArtifact(disk workerapi.CheckpointComputer) computer.DiskArtifact {
	return computer.DiskArtifact{Object: cas.Descriptor{Digest: disk.Artifact.Digest, SizeBytes: disk.Artifact.SizeBytes, MediaType: disk.Artifact.MediaType}, LogicalBytes: disk.LogicalBytes}
}
