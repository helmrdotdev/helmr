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
	"github.com/helmrdotdev/helmr/internal/tracing"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/helmrdotdev/helmr/internal/workspace"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

var errStaleActorCompletion = errors.New("actor completion receipt is stale")

type actorCompletionReplayStore interface {
	GetActorCompletionReplay(context.Context, db.GetActorCompletionReplayParams) (pgtype.Text, error)
}

func (s *Server) completeActor(ctx context.Context, worker workerActor, request workerapi.CompleteActorRequest, completion parsedActorCompletion) error {
	replayed, err := actorCompletionWasReplayed(ctx, s.db, worker, request, completion)
	if err != nil || replayed {
		return err
	}
	if completion.capture != nil {
		verified, err := s.verifyTaskWorkspaceCapture(ctx, *completion.capture)
		if err != nil {
			return actorCompletionReplayAfterError(ctx, s.db, worker, request, completion, err)
		}
		completion.capture = &verified
	}
	err = s.inTx(ctx, func(work *txWork) error {
		replayed, err := actorCompletionWasReplayed(ctx, work.q, worker, request, completion)
		if err != nil || replayed {
			return err
		}
		locators, err := work.q.GetLiveRunLeaseLocators(ctx, db.GetLiveRunLeaseLocatorsParams{
			ID: pgvalue.UUID(completion.lease.leaseID), LeaseSequence: request.Lease.LeaseSequence,
			WorkerGroupID: worker.WorkerGroupID, WorkerInstanceID: pgvalue.UUID(worker.WorkerInstanceID),
			WorkerEpoch: worker.WorkerEpoch, WorkerProtocolVersion: worker.ProtocolVersion,
		})
		if err != nil {
			return staleActorCompletion(err)
		}
		secrets, err := secret.LockAttemptDelivery(ctx, work.q, locators.RunID, locators.AttemptNumber, locators.WorkspaceID)
		if err != nil {
			return fmt.Errorf("lock actor completion secret authority: %w", err)
		}
		owner, err := lockRunFinalizationOwner(ctx, work.q, locators)
		if err != nil || !owner.actor.ID.Valid {
			return staleActorCompletion(err)
		}
		authority, err := lockLiveRunLeaseAuthority(ctx, work.q, worker, pgvalue.UUID(completion.lease.leaseID), request.Lease.LeaseSequence, locators)
		if err != nil {
			return staleActorCompletion(err)
		}
		authority.actor = owner.actor
		if err := validateActorCompletionAuthority(ctx, work.q, completion, authority); err != nil {
			return err
		}
		if completion.rollback != nil {
			rollbackAuthority := authority
			rollbackAuthority.run.BaseWorkspaceVersionID = authority.workspace.HeadVersionID
			if err := validateTaskWorkspaceRollback(ctx, work.q, rollbackAuthority, *completion.rollback); err != nil {
				return staleActorCompletion(err)
			}
		}
		completedAt, err := work.q.GetTaskCompletionTime(ctx)
		if err != nil || !completedAt.Valid {
			if err == nil {
				err = errors.New("database actor completion time is unavailable")
			}
			return err
		}
		if err := validateTaskCompletionDeadline(authority, completedAt.Time); err != nil {
			return staleActorCompletion(err)
		}
		retryAt, retry, err := actorCompletionRetryAt(authority.run, authority.attempt, completion, completedAt.Time)
		if err != nil {
			return err
		}
		var versionID pgtype.UUID
		if completion.capture != nil {
			versionID, err = recordTaskWorkspaceVersion(ctx, work.q, worker, authority, *completion.capture, completedAt)
			if err != nil {
				return err
			}
			if _, err := work.q.AdvanceActorWorkspaceHead(ctx, db.AdvanceActorWorkspaceHeadParams{
				NewHeadVersionID: versionID, CompletedAt: completedAt, ID: authority.workspace.ID,
				OrgID: authority.run.OrgID, ProjectID: authority.run.ProjectID, EnvironmentID: authority.run.EnvironmentID,
				ActorID: authority.actor.ID, OwnershipGeneration: authority.workspace.OwnershipGeneration,
				WriterGeneration: authority.workspace.WriterGeneration, ExpectedHeadVersionID: authority.workspace.HeadVersionID,
			}); err != nil {
				return staleActorCompletion(err)
			}
		} else if authority.workspaceMount.MaterializedVersionID != authority.workspace.HeadVersionID {
			if err := updateTaskWorkspaceMountFrontier(ctx, work.q, authority, authority.workspace.HeadVersionID, completedAt); err != nil {
				return err
			}
		}
		if err := terminalizeActorAttempt(ctx, work.q, authority, completion, completedAt); err != nil {
			return err
		}
		if _, err := work.q.ReleaseTaskWorkspaceLease(ctx, db.ReleaseTaskWorkspaceLeaseParams{
			CompletedAt: completedAt, ID: authority.workspaceLease.ID, WorkspaceID: authority.workspace.ID,
			WorkspaceMountID: authority.workspaceMount.ID, RuntimeInstanceID: authority.runtime.ID,
			OwnerRunLeaseID: authority.runLease.ID, BaseVersionID: authority.workspaceLease.BaseVersionID,
			OwnershipGeneration: authority.workspace.OwnershipGeneration, WriterGeneration: authority.workspace.WriterGeneration,
			MountFencingGeneration: authority.workspaceMount.FencingGeneration,
		}); err != nil {
			return staleActorCompletion(err)
		}
		if retry {
			return scheduleActorRetry(ctx, work.q, authority, secrets, completedAt, retryAt)
		}
		return finishActorRun(ctx, work.q, authority, secrets, completion, completedAt)
	})
	if err != nil {
		return actorCompletionReplayAfterError(ctx, s.db, worker, request, completion, err)
	}
	return nil
}

func actorCompletionWasReplayed(ctx context.Context, store actorCompletionReplayStore, worker workerActor, request workerapi.CompleteActorRequest, completion parsedActorCompletion) (bool, error) {
	fingerprint, err := store.GetActorCompletionReplay(ctx, db.GetActorCompletionReplayParams{
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
		return false, errStaleActorCompletion
	}
	return true, nil
}

func actorCompletionReplayAfterError(ctx context.Context, store actorCompletionReplayStore, worker workerActor, request workerapi.CompleteActorRequest, completion parsedActorCompletion, operationErr error) error {
	replayed, replayErr := actorCompletionWasReplayed(ctx, store, worker, request, completion)
	if replayed {
		return nil
	}
	if errors.Is(replayErr, errStaleActorCompletion) {
		return errStaleActorCompletion
	}
	if replayErr != nil {
		return errors.Join(operationErr, fmt.Errorf("check actor completion replay: %w", replayErr))
	}
	return operationErr
}

func validateActorCompletionAuthority(
	ctx context.Context,
	store db.Querier,
	completion parsedActorCompletion,
	authority runLeaseClaimAuthority,
) error {
	actor := authority.actor
	if authority.run.EntrypointKind != "actor" || !authority.run.ActorID.Valid || authority.run.ActorID != actor.ID ||
		authority.run.ParentRunID.Valid || authority.run.ParentOwnsLifecycle.Valid ||
		authority.runLease.State != db.RunLeaseStateFinalizing || !authority.attempt.EntrypointEnteredAt.Valid ||
		authority.run.ActiveStartedAt.Valid || !authority.runLease.FinalizationOperationID.Valid ||
		!authority.runLease.FinalizationKind.Valid || !authority.runLease.FinalizationStartedAt.Valid ||
		!authority.runLease.FinalizationRequestFingerprint.Valid || !actor.CurrentRunID.Valid || actor.CurrentRunID != authority.run.ID ||
		(actor.State != "open" && actor.State != "closing") || authority.workspace.OwnerActorID != actor.ID || authority.workspace.OwnerRunID.Valid ||
		!authority.workspace.HeadVersionID.Valid || authority.workspace.HeadVersionID != authority.workspaceLease.BaseVersionID ||
		!authority.attempt.ActorStartInputSequence.Valid || !authority.run.ActorStartInputSequence.Valid || !authority.run.ActorStartInputHighWatermark.Valid {
		return errStaleActorCompletion
	}
	cursor := completion.terminalInputSequence
	if cursor < authority.attempt.ActorStartInputSequence.Int64 || cursor < actor.CommittedInputSequence || cursor >= actor.NextInputSequence {
		return errStaleActorCompletion
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
	if err != nil || authority.runLease.FinalizationOperationID != pgvalue.UUID(operationID) || authority.runLease.FinalizationKind.String != wantKind {
		return errStaleActorCompletion
	}
	assignment, err := projectRunLeaseAssignment(runLeaseProjectionAuthority{
		run: authority.run, attempt: authority.attempt, runtime: authority.runtime,
		runLease: authority.runLease, workspace: authority.workspace, workspaceMount: authority.workspaceMount, workspaceLease: authority.workspaceLease,
	})
	if err != nil {
		return err
	}
	if !finalizationFenceMatchesLease(finalization.Fence, assignment) {
		return errStaleActorCompletion
	}
	clear, err := store.RunFinalizationScopeIsClear(ctx, db.RunFinalizationScopeIsClearParams{RunID: authority.run.ID, AttemptNumber: authority.attempt.Number, WorkspaceID: authority.workspace.ID})
	if err != nil {
		return err
	}
	if !clear.Valid || !clear.Bool {
		return errStaleActorCompletion
	}
	return nil
}

func actorCompletionRetryAt(run db.Run, attempt db.RunAttempt, completion parsedActorCompletion, completedAt time.Time) (time.Time, bool, error) {
	if completion.kind != actorCompletionFailed {
		return time.Time{}, false, nil
	}
	policy, err := deployment.ParseRetryManifest(run.RetryPolicy)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("parse pinned actor retry policy: %w", err)
	}
	delay, retry, err := taskRetryDelay(policy, attempt.Number, nil)
	if err != nil || !retry {
		return time.Time{}, retry, err
	}
	return completedAt.Add(delay), true, nil
}

func terminalizeActorAttempt(ctx context.Context, store db.Querier, authority runLeaseClaimAuthority, completion parsedActorCompletion, completedAt pgtype.Timestamptz) error {
	leaseState := db.RunLeaseStateFailed
	outcome := pgvalue.Text("failed")
	reason := "actor_failed"
	var terminalError []byte
	if completion.kind == actorCompletionSucceeded {
		leaseState = db.RunLeaseStateCompleted
		outcome = pgvalue.Text("succeeded")
		reason = "completed"
	} else {
		terminalError = completion.errorObject
	}
	if _, err := store.CompleteTaskRunLease(ctx, db.CompleteTaskRunLeaseParams{
		State: leaseState, CompletedAt: completedAt, ReasonCode: pgvalue.Text(reason), Error: terminalError,
		TerminalRequestFingerprint: pgvalue.Text(completion.fingerprint), ID: authority.runLease.ID,
		RunID: authority.run.ID, WorkspaceID: authority.workspace.ID, AttemptNumber: authority.attempt.Number,
		LeaseSequence: authority.runLease.LeaseSequence,
	}); err != nil {
		return staleActorCompletion(err)
	}
	if _, err := store.CompleteActorAttempt(ctx, db.CompleteActorAttemptParams{
		TerminalActorInputSequence: pgtype.Int8{Int64: completion.terminalInputSequence, Valid: true}, TerminalOutcome: outcome,
		ReasonCode: pgvalue.Text(reason), Error: terminalError, CompletedAt: completedAt,
		RunID: authority.run.ID, Number: authority.attempt.Number, WorkspaceID: authority.workspace.ID,
	}); err != nil {
		return staleActorCompletion(err)
	}
	return nil
}

type actorRunTerminalDecision struct {
	runStatus    db.RunStatus
	runReason    pgtype.Text
	actorState   string
	failureCode  pgtype.Text
	commitCursor bool
}

func decideActorRunTerminal(authority runLeaseClaimAuthority, completion parsedActorCompletion) actorRunTerminalDecision {
	decision := actorRunTerminalDecision{
		runStatus:    db.RunStatusSucceeded,
		actorState:   authority.actor.State,
		commitCursor: true,
	}
	if completion.kind == actorCompletionFailed {
		decision.runStatus = db.RunStatusFailed
		decision.runReason = pgvalue.Text("actor_failed")
		decision.commitCursor = false
		decision.actorState = "failed"
		decision.failureCode = pgvalue.Text("run_failed")
		return decision
	}
	if authority.run.ActorStartInputHighWatermark.Int64 > authority.run.ActorStartInputSequence.Int64 &&
		completion.terminalInputSequence <= authority.run.ActorStartInputSequence.Int64 {
		decision.actorState = "failed"
		decision.failureCode = pgvalue.Text("no_progress")
		return decision
	}
	if authority.actor.State == "closing" && authority.actor.CloseSequence.Valid &&
		completion.terminalInputSequence >= authority.actor.CloseSequence.Int64 {
		decision.actorState = "closed"
		return decision
	}
	return decision
}

func scheduleActorRetry(ctx context.Context, store db.Querier, authority runLeaseClaimAuthority, secrets []secret.DeliveryEnvelope, completedAt pgtype.Timestamptz, retryAt time.Time) error {
	next := authority.attempt.Number + 1
	if _, err := store.CreateActorRetryAttempt(ctx, db.CreateActorRetryAttemptParams{
		Number: next, ExpectedRunGeneration: authority.actor.RunGeneration, RunID: authority.run.ID,
		WorkspaceID: authority.workspace.ID, PreviousAttemptNumber: authority.attempt.Number, RunLeaseID: authority.runLease.ID,
	}); err != nil {
		return staleActorCompletion(err)
	}
	if err := createActorAttemptSecretResolutions(ctx, store, authority.workspace.ID, authority.run.ID, next, secrets); err != nil {
		return err
	}
	if _, err := store.DelayActorRunRetry(ctx, db.DelayActorRunRetryParams{
		NextAttemptNumber: next, RetryAt: pgvalue.Timestamptz(retryAt), CompletedAt: completedAt,
		ID: authority.run.ID, WorkspaceID: authority.workspace.ID, ActorID: authority.actor.ID,
		PreviousAttemptNumber: authority.attempt.Number, RunLeaseID: authority.runLease.ID,
	}); err != nil {
		return staleActorCompletion(err)
	}
	return nil
}

func finishActorRun(ctx context.Context, store db.Querier, authority runLeaseClaimAuthority, secrets []secret.DeliveryEnvelope, completion parsedActorCompletion, completedAt pgtype.Timestamptz) error {
	decision := decideActorRunTerminal(authority, completion)
	var terminalError []byte
	var failureRunID pgtype.UUID
	if completion.kind == actorCompletionFailed {
		terminalError = completion.errorObject
	}
	if decision.failureCode.Valid {
		failureRunID = authority.run.ID
	}
	commitCursor := pgtype.Int8{Int64: completion.terminalInputSequence, Valid: decision.commitCursor}
	if _, err := store.FinishActorRun(ctx, db.FinishActorRunParams{
		Status: decision.runStatus, ReasonCode: decision.runReason, Error: terminalError, CompletedAt: completedAt,
		ID: authority.run.ID, WorkspaceID: authority.workspace.ID, ActorID: authority.actor.ID,
		AttemptNumber: authority.attempt.Number, RunLeaseID: authority.runLease.ID,
	}); err != nil {
		return staleActorCompletion(err)
	}
	actor, err := store.ReconcileActorTerminalRun(ctx, db.ReconcileActorTerminalRunParams{
		State: decision.actorState, CommittedInputSequence: commitCursor, FailureCode: decision.failureCode, FailureRunID: failureRunID,
		CompletedAt: completedAt, EnvironmentID: authority.actor.EnvironmentID, ID: authority.actor.ID,
		WorkspaceID: authority.workspace.ID, RunID: authority.run.ID, ExpectedRunGeneration: authority.actor.RunGeneration,
	})
	if err != nil {
		return staleActorCompletion(err)
	}
	terminalActor := actor.State == "failed" || actor.State == "closed"
	if terminalActor {
		if _, err := store.ReleaseActorWorkspaceOwner(ctx, db.ReleaseActorWorkspaceOwnerParams{
			CompletedAt: completedAt, ID: authority.workspace.ID,
			EnvironmentID: authority.run.EnvironmentID, ActorID: actor.ID,
			OwnershipGeneration: authority.workspace.OwnershipGeneration, WriterGeneration: authority.workspace.WriterGeneration,
		}); err != nil {
			return staleActorCompletion(err)
		}
	} else if actorNeedsContinuation(actor) {
		if err := createActorContinuation(ctx, store, actor, authority.workspace, secrets, completedAt); err != nil {
			return err
		}
	}
	eventKind := api.RunEventKindCompleted
	if decision.runStatus == db.RunStatusFailed {
		eventKind = api.RunEventKindFailed
	}
	payload, err := json.Marshal(struct {
		Reason string `json:"reason,omitempty"`
	}{Reason: decision.runReason.String})
	if err != nil {
		return err
	}
	if _, err := store.AppendRunEvent(ctx, db.AppendRunEventParams{OrgID: authority.run.OrgID, RunID: authority.run.ID, Kind: eventKind, Payload: payload}); err != nil {
		return fmt.Errorf("append actor terminal event: %w", err)
	}
	return nil
}

func actorNeedsContinuation(actor db.Actor) bool {
	return (actor.State == "open" || actor.State == "closing") &&
		!actor.ManualRunCancelled &&
		actor.CommittedInputSequence < actor.NextInputSequence-1
}

func createActorContinuation(ctx context.Context, store db.Querier, actor db.Actor, ws db.Workspace, secrets []secret.DeliveryEnvelope, now pgtype.Timestamptz) error {
	runID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	traceID, err := tracing.NewTraceID()
	if err != nil {
		return err
	}
	rootSpanID, err := tracing.NewSpanID()
	if err != nil {
		return err
	}
	run, err := store.CreateActorContinuationRun(ctx, db.CreateActorContinuationRunParams{
		RunID: runID, QueueOriginAt: now, TraceID: pgvalue.Text(traceID), RootSpanID: rootSpanID,
		EnvironmentID: actor.EnvironmentID, ActorID: actor.ID, WorkspaceID: ws.ID, ExpectedRunGeneration: actor.RunGeneration,
	})
	if err != nil {
		return staleActorCompletion(err)
	}
	if err := createActorAttemptSecretResolutions(ctx, store, ws.ID, run.ID, 1, secrets); err != nil {
		return err
	}
	if _, err := store.CreateRunAdmissionOutbox(ctx, db.CreateRunAdmissionOutboxParams{
		ID: pgvalue.UUID(uuid.Must(uuid.NewV7())), WorkspaceID: ws.ID, EnvironmentID: actor.EnvironmentID, RunID: run.ID,
	}); err != nil {
		return fmt.Errorf("enqueue actor continuation: %w", err)
	}
	return nil
}

func createActorAttemptSecretResolutions(ctx context.Context, store db.Querier, workspaceID, runID pgtype.UUID, attempt int32, bindings []secret.DeliveryEnvelope) error {
	resolutions, err := activeSecretResolutions(bindings)
	if err != nil {
		return err
	}
	if err := secret.CreateAttemptResolutions(ctx, store, workspaceID, runID, attempt, resolutions); err != nil {
		return fmt.Errorf("record actor attempt secret resolutions: %w", err)
	}
	return nil
}

func staleActorCompletion(err error) error {
	if err == nil || errors.Is(err, pgx.ErrNoRows) || errors.Is(err, errStaleRunLeaseClaim) || errors.Is(err, errStaleRunFinalization) {
		return errStaleActorCompletion
	}
	return err
}
