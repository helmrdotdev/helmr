package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/helmrdotdev/helmr/internal/session"
	"github.com/helmrdotdev/helmr/internal/telemetry"
	"github.com/helmrdotdev/helmr/internal/tracing"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

var errStaleActorCompletion = errors.New("actor completion receipt is stale")
var errActorStopCleanupPending = errors.New("owned execution cleanup is pending")

type actorCompletionReplayStore interface {
	GetActorCompletionReplay(context.Context, db.GetActorCompletionReplayParams) (pgtype.Text, error)
}

func (s *Server) completeActor(ctx context.Context, worker workerActor, request workerapi.CompleteActorRequest, completion parsedActorCompletion) error {
	replayed, err := actorCompletionWasReplayed(ctx, s.db, worker, request, completion)
	if err != nil || replayed {
		return err
	}
	verified, err := s.verifyTaskComputerCapture(ctx, *completion.capture)
	if err != nil {
		return actorCompletionReplayAfterError(ctx, s.db, worker, request, completion, err)
	}
	completion.capture = &verified

	err = s.inTx(ctx, func(work *txWork) error {
		replayed, err := actorCompletionWasReplayed(ctx, work.q, worker, request, completion)
		if err != nil || replayed {
			return err
		}
		locators, err := work.q.GetLiveRunLeaseLocators(ctx, db.GetLiveRunLeaseLocatorsParams{
			ID: pgvalue.UUID(completion.lease.leaseID), LeaseSequence: request.Lease.LeaseSequence,
			WorkerGroupID: pgvalue.UUID(worker.WorkerGroupID), WorkerInstanceID: pgvalue.UUID(worker.WorkerInstanceID),
			WorkerEpoch: worker.WorkerEpoch})
		if err != nil {
			return staleActorCompletion(err)
		}
		secrets, err := secret.LockAttemptDelivery(ctx, work.q, locators.RunID, locators.AttemptNumber, locators.WorkspaceID)
		if err != nil {
			return fmt.Errorf("lock actor completion secret authority: %w", err)
		}
		tx, ok := work.tx.(pgx.Tx)
		if !ok {
			return errors.New("actor completion transaction does not expose PostgreSQL authority")
		}
		var authority runLeaseClaimAuthority
		ownedGraph, err := run.LockOwnedFinalizationWithRuntimeFence(ctx, tx, run.OwnedFinalizationRequest{
			OrgID: pgvalue.MustUUIDValue(locators.OrgID), ProjectID: pgvalue.MustUUIDValue(locators.ProjectID),
			EnvironmentID: pgvalue.MustUUIDValue(locators.EnvironmentID), RunID: pgvalue.MustUUIDValue(locators.RunID),
		}, func() error {
			owner, err := lockRunFinalizationOwner(ctx, work.q, locators)
			if err != nil || !owner.actor.ID.Valid {
				return staleActorCompletion(err)
			}
			authority, err = lockLiveRunLeaseAuthority(ctx, work.q, worker, pgvalue.UUID(completion.lease.leaseID), request.Lease.LeaseSequence, locators)
			if err != nil {
				return staleActorCompletion(err)
			}
			authority.actor = owner.actor
			return nil
		})
		if err != nil {
			return staleActorCompletion(err)
		}

		if err := validateActorCompletionAuthority(ctx, work.q, completion, authority); err != nil {
			return err
		}
		if completion.kind == actorCompletionInterrupted {
			excluded, err := work.q.SessionOwnedExecutionsExcluded(ctx, authority.run.ID)
			if err != nil {
				return err
			}
			if !excluded {
				return errActorStopCleanupPending
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
		decision := decideActorRunTerminal(authority, completion)
		failed := decision.runStatus == db.RunStatusFailed
		if failed {
			if _, err := run.HoldSessionExecution(ctx, work.q, authority.actor, authority.attempt.Number, "recovery_required"); err != nil {
				return err
			}
			if _, err := ownedGraph.CancelDescendants(ctx); err != nil {
				return err
			}
		}
		if err := requireFinalizationComputer(ctx, work.q, authority, *completion.capture); err != nil {
			return staleActorCompletion(err)
		}
		versionID, err := recordTaskWorkspaceVersion(ctx, work.q, worker, authority, completion.capture.version(), completedAt)
		if err != nil {
			return err
		}
		if _, err := work.q.AdvanceActorWorkspaceHead(ctx, db.AdvanceActorWorkspaceHeadParams{
			NewHeadVersionID: versionID, CompletedAt: completedAt, ID: authority.workspace.ID,
			OrgID: authority.run.OrgID, ProjectID: authority.run.ProjectID, EnvironmentID: authority.run.EnvironmentID,
			SessionID: authority.actor.ID, OwnershipGeneration: authority.workspace.OwnershipGeneration,
			WriterGeneration: authority.workspace.WriterGeneration, ExpectedHeadVersionID: authority.workspace.HeadVersionID,
		}); err != nil {
			return staleActorCompletion(err)
		}
		if completion.kind == actorCompletionInterrupted {
			if err := session.CompleteInterruption(ctx, work.q, authority.actor, versionID, completion.fingerprint); err != nil {
				return err
			}
		}
		if err := terminalizeActorAttempt(ctx, work.q, authority, completion, decision, completedAt); err != nil {
			return err
		}
		if _, err := work.q.ReleaseTaskWorkspaceLease(ctx, db.ReleaseTaskWorkspaceLeaseParams{
			CompletedAt: completedAt, ID: authority.workspaceLease.ID, WorkspaceID: authority.workspace.ID,
			WorkspaceMountID: authority.workspaceMount.ID, RuntimeInstanceID: authority.runtime.ID,
			OwnerRunLeaseID: authority.runLease.ID, BaseWorkspaceVersionID: authority.workspaceLease.BaseWorkspaceVersionID,
			OwnershipGeneration: authority.workspace.OwnershipGeneration, WriterGeneration: authority.workspace.WriterGeneration,
			MountFencingGeneration: authority.workspaceMount.FencingGeneration,
		}); err != nil {
			return staleActorCompletion(err)
		}
		if failed || completion.kind == actorCompletionInterrupted {
			if err := work.q.CloseRunRuntimes(ctx, db.CloseRunRuntimesParams{RunID: authority.run.ID, RunLeaseID: authority.runLease.ID, ReasonCode: decision.runReason.String}); err != nil {
				return err
			}
		}
		if err := finishActorRun(ctx, work.q, authority, secrets, completion, decision, completedAt); err != nil {
			return err
		}
		return staleActorCompletionPublicationDeadline(ctx, work.q, authority)
	})
	if err != nil {
		return actorCompletionReplayAfterError(ctx, s.db, worker, request, completion, err)
	}
	return nil
}

func actorCompletionWasReplayed(ctx context.Context, store actorCompletionReplayStore, worker workerActor, request workerapi.CompleteActorRequest, completion parsedActorCompletion) (bool, error) {
	fingerprint, err := store.GetActorCompletionReplay(ctx, db.GetActorCompletionReplayParams{
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
	if completion.runGeneration <= 0 || completion.runGeneration != actor.RunGeneration {
		return errStaleActorCompletion
	}
	if completion.kind == actorCompletionInterrupted {
		var turnID pgtype.UUID
		if completion.turnID != nil {
			turnID = pgvalue.UUID(*completion.turnID)
		}
		if actor.DispatchHoldID != pgvalue.UUID(completion.holdID) || actor.DispatchHoldReason.String != "interrupt_requested" ||
			actor.DispatchHoldRunID != authority.run.ID || !actor.DispatchHoldAttemptNumber.Valid || actor.DispatchHoldAttemptNumber.Int32 != authority.attempt.Number ||
			!actor.DispatchHoldRunGeneration.Valid || actor.DispatchHoldRunGeneration.Int64 != completion.runGeneration || actor.ActiveTurnID != turnID || completion.capture == nil {
			return errStaleActorCompletion
		}
		if turnID.Valid {
			unsettled, err := store.SessionTurnHasUnsettledWork(ctx, db.SessionTurnHasUnsettledWorkParams{SessionID: actor.ID, TurnID: turnID})
			if err != nil {
				return err
			}
			if !unsettled.Valid || unsettled.Bool {
				return errStaleActorCompletion
			}
		}
	} else if (actor.ActiveTurnID.Valid && completion.kind != actorCompletionFailed) || actor.DispatchHoldID.Valid {
		return errStaleActorCompletion
	}
	if authority.run.EntrypointKind != "actor" || !authority.run.SessionID.Valid || authority.run.SessionID != actor.ID ||
		authority.run.ParentRunID.Valid || authority.run.ParentOwnsLifecycle.Valid ||
		authority.runLease.Status != db.RunLeaseStatusFinalizing || !authority.attempt.EntrypointEnteredAt.Valid ||
		authority.run.ActiveStartedAt.Valid || !authority.runLease.FinalizationOperationID.Valid ||
		!authority.runLease.FinalizationKind.Valid || !authority.runLease.FinalizationStartedAt.Valid ||
		!authority.runLease.FinalizationRequestFingerprint.Valid || !actor.CurrentRunID.Valid || actor.CurrentRunID != authority.run.ID ||
		(actor.Status != "open" && actor.Status != "closing") || authority.workspace.OwnerSessionID != actor.ID || authority.workspace.OwnerRunID.Valid ||
		!authority.workspace.HeadVersionID.Valid ||
		!authority.attempt.SessionInputStartSequence.Valid || !authority.run.SessionInputStartSequence.Valid || !authority.run.SessionInputHighWatermark.Valid {
		return errStaleActorCompletion
	}
	if authority.workspaceLease.BaseWorkspaceVersionID != authority.workspace.HeadVersionID {
		base, err := getActorWorkspaceVersion(ctx, store, authority, authority.workspaceLease.BaseWorkspaceVersionID)
		if err != nil {
			return staleActorCompletion(err)
		}
		if _, err := validateRestoredActorBase(ctx, store, authority, base); err != nil {
			return err
		}
	}
	cursor := actor.CommittedInputSequence
	if cursor < authority.attempt.SessionInputStartSequence.Int64 || cursor < actor.CommittedInputSequence || cursor >= actor.NextInputSequence {
		return errStaleActorCompletion
	}
	if completion.capture == nil {
		return errStaleActorCompletion
	}
	finalization := completion.capture.receipt
	wantKind := string(workerapi.RunFinalizationCapture)
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

func validateRestoredActorBase(
	ctx context.Context,
	store db.Querier,
	authority runLeaseClaimAuthority,
	base db.GetComputerVersionAuthorityRow,
) (db.RunCheckpoint, error) {
	if !authority.runtime.RestoreCheckpointID.Valid ||
		!base.SourceWorkspaceLeaseID.Valid ||
		base.OwnershipGeneration != authority.workspace.OwnershipGeneration {
		return db.RunCheckpoint{}, errStaleActorCompletion
	}
	checkpointRow, err := store.GetReadyRunCheckpoint(ctx, db.GetReadyRunCheckpointParams{
		RunID: authority.run.ID, AttemptNumber: authority.attempt.Number,
		ID: authority.runtime.RestoreCheckpointID,
	})
	if err != nil {
		return db.RunCheckpoint{}, staleActorCompletion(err)
	}
	checkpoint := checkpointRow.RunCheckpoint
	wait, err := store.GetRunWait(ctx, db.GetRunWaitParams{
		RunID: authority.run.ID, AttemptNumber: authority.attempt.Number, ID: checkpoint.RunWaitID,
	})
	if err != nil {
		return db.RunCheckpoint{}, staleActorCompletion(err)
	}
	if checkpoint.ID != authority.runtime.RestoreCheckpointID ||
		checkpoint.WorkspaceID != authority.workspace.ID ||
		wait.SuspensionStatus != db.RunWaitStatusReleased ||
		wait.ResumeRequestVersion <= 0 || wait.ResumeRequestVersion != wait.ResumeAckVersion {
		return db.RunCheckpoint{}, errStaleActorCompletion
	}

	validLineage, err := store.ActorCheckpointLineageIsValid(ctx, db.ActorCheckpointLineageIsValidParams{
		RunID: authority.run.ID, AttemptNumber: authority.attempt.Number, WorkspaceID: authority.workspace.ID,
		CheckpointID: checkpoint.ID, CommittedHeadVersionID: authority.workspace.HeadVersionID,
		OwnershipGeneration: authority.workspace.OwnershipGeneration,
	})
	if err != nil {
		return db.RunCheckpoint{}, err
	}
	if !validLineage {
		return db.RunCheckpoint{}, errStaleActorCompletion
	}

	checkpointBase, err := getActorWorkspaceVersion(ctx, store, authority, checkpoint.PrivateWorkspaceVersionID)
	if err != nil {
		return db.RunCheckpoint{}, staleActorCompletion(err)
	}

	// The released wait describes the execution that produced the checkpoint.
	// Later Turns can commit results without publishing this private disk base.
	// The upper bounds restate restore admission; both Session counters are monotonic.
	cursor := wait.ActorSpeculativeInputSequence
	if !cursor.Valid ||
		cursor.Int64 > authority.actor.CommittedInputSequence+1 ||
		cursor.Int64 >= authority.actor.NextInputSequence || !authority.attempt.SessionInputStartSequence.Valid ||
		cursor.Int64 < authority.attempt.SessionInputStartSequence.Int64 ||
		(wait.TurnID.Valid && (wait.TurnSessionID != authority.actor.ID || !wait.TurnRunGeneration.Valid || wait.TurnRunGeneration.Int64 != authority.actor.RunGeneration)) {
		return db.RunCheckpoint{}, errStaleActorCompletion
	}
	source, err := store.GetRunCheckpointSource(ctx, db.GetRunCheckpointSourceParams{
		SourceWorkspaceLeaseID: checkpoint.SourceWorkspaceLeaseID, SourceRunLeaseID: checkpoint.SourceRunLeaseID,
		RunID: authority.run.ID, AttemptNumber: authority.attempt.Number, WorkspaceID: authority.workspace.ID,
	})
	if err != nil {
		return db.RunCheckpoint{}, staleActorCompletion(err)
	}
	sourceAuthority := authority
	sourceAuthority.checkpoint = checkpoint
	sourceAuthority.sourceRunLease = source.RunLease
	sourceAuthority.sourceWorkspaceLease = source.WorkspaceLease
	sourceAuthority.sourceRuntime = source.RuntimeInstance
	if validateCheckpointSource(sourceAuthority) != nil {
		return db.RunCheckpoint{}, errStaleActorCompletion
	}

	if sameWorkspaceParentResumeWait(wait) {
		predecessorWriter := wait.ChildWriterGeneration.Int64
		if !wait.ChildWriterGeneration.Valid {
			valid, err := store.SameWorkspaceChildHasNoExecution(ctx, db.SameWorkspaceChildHasNoExecutionParams{
				ChildRunID: wait.ChildRunID, ParentRunID: authority.run.ID,
				WorkspaceID: authority.workspace.ID, BaseWorkspaceVersionID: checkpoint.PrivateWorkspaceVersionID,
			})
			if err != nil {
				return db.RunCheckpoint{}, err
			}
			if !valid {
				return db.RunCheckpoint{}, errStaleActorCompletion
			}
			predecessorWriter = wait.ParentWriterGeneration.Int64
		}
		if !wait.BaseWorkspaceVersionID.Valid || wait.BaseWorkspaceVersionID != checkpoint.PrivateWorkspaceVersionID ||
			!wait.ResumeWorkspaceVersionID.Valid || wait.ResumeWorkspaceVersionID != authority.workspaceLease.BaseWorkspaceVersionID ||
			!wait.OwnershipGeneration.Valid || wait.OwnershipGeneration.Int64 != authority.workspace.OwnershipGeneration ||
			!wait.ParentWriterGeneration.Valid || wait.ParentWriterGeneration.Int64 != checkpointBase.WriterGeneration ||
			!wait.ResumeWriterGeneration.Valid || wait.ResumeWriterGeneration.Int64 != authority.workspace.WriterGeneration ||
			(wait.ChildWriterGeneration.Valid && wait.ParentWriterGeneration.Int64 >= wait.ChildWriterGeneration.Int64) ||
			predecessorWriter >= wait.ResumeWriterGeneration.Int64 ||
			authority.workspaceLease.WriterGeneration != wait.ResumeWriterGeneration.Int64 {
			return db.RunCheckpoint{}, errStaleActorCompletion
		}
		if wait.ConditionStatus == db.WaitStatusFailed || wait.ConditionStatus == db.WaitStatusCancelled {
			// A failed child hands back the parent's original private checkpoint.
			// The acknowledged restore owns the later writer and physical mount.
			if wait.ResumeWorkspaceVersionID != checkpoint.PrivateWorkspaceVersionID ||
				base.VersionID != checkpointBase.VersionID {
				return db.RunCheckpoint{}, errStaleActorCompletion
			}
			return checkpoint, nil
		}
		if wait.ConditionStatus != db.WaitStatusCompleted || wait.ChildWriterGeneration.Int64 != base.WriterGeneration {
			return db.RunCheckpoint{}, errStaleActorCompletion
		}
		childSource, err := store.GetWorkspaceLease(ctx, db.GetWorkspaceLeaseParams{
			EnvironmentID: authority.run.EnvironmentID, WorkspaceID: authority.workspace.ID,
			ID: base.SourceWorkspaceLeaseID,
		})
		if err != nil {
			return db.RunCheckpoint{}, staleActorCompletion(err)
		}
		if !validActorCompletionVersionSource(
			base, childSource, childSource.BaseWorkspaceVersionID,
			authority.workspace.OwnershipGeneration,
		) {
			return db.RunCheckpoint{}, errStaleActorCompletion
		}
		child, err := store.GetRun(ctx, db.GetRunParams{EnvironmentID: authority.run.EnvironmentID, ID: wait.ChildRunID})
		if err != nil {
			return db.RunCheckpoint{}, staleActorCompletion(err)
		}
		if child.ParentRunID != authority.run.ID || !child.ParentOwnsLifecycle.Valid || !child.ParentOwnsLifecycle.Bool ||
			child.EntrypointKind != "task" || child.WorkspaceID != authority.workspace.ID ||
			child.BaseWorkspaceVersionID != checkpoint.PrivateWorkspaceVersionID || child.Status != db.RunStatusSucceeded ||
			child.CurrentRunLeaseID.Valid {
			return db.RunCheckpoint{}, errStaleActorCompletion
		}
		childReceipt, err := store.GetRunCheckpointSource(ctx, db.GetRunCheckpointSourceParams{
			SourceWorkspaceLeaseID: childSource.ID, SourceRunLeaseID: childSource.OwnerRunLeaseID,
			RunID: child.ID, AttemptNumber: child.CurrentAttemptNumber, WorkspaceID: authority.workspace.ID,
		})
		if err != nil {
			return db.RunCheckpoint{}, staleActorCompletion(err)
		}
		if childReceipt.RunLease.Status != db.RunLeaseStatusCompleted ||
			childReceipt.RuntimeInstance.RuntimeIdentityID != childReceipt.RunLease.RuntimeIdentityID ||
			!runtimeHasExclusionProof(childReceipt.RuntimeInstance) {
			return db.RunCheckpoint{}, errStaleActorCompletion
		}
		return checkpoint, nil
	}

	if wait.BaseWorkspaceVersionID.Valid || wait.ResumeWorkspaceVersionID.Valid ||
		wait.OwnershipGeneration.Valid || wait.ParentWriterGeneration.Valid ||
		wait.ChildWriterGeneration.Valid || wait.ResumeWriterGeneration.Valid ||
		checkpoint.PrivateWorkspaceVersionID != authority.workspaceLease.BaseWorkspaceVersionID ||
		base.VersionID != checkpointBase.VersionID ||
		base.WriterGeneration >= authority.workspace.WriterGeneration ||
		authority.workspaceLease.WriterGeneration != authority.workspace.WriterGeneration {
		return db.RunCheckpoint{}, errStaleActorCompletion
	}
	return checkpoint, nil
}

func validActorCompletionVersionSource(
	version db.GetComputerVersionAuthorityRow,
	source db.WorkspaceLease,
	expectedParent pgtype.UUID,
	expectedOwnership int64,
) bool {
	return version.ParentVersionID == expectedParent &&
		version.SourceWorkspaceLeaseID == source.ID &&
		version.OwnershipGeneration == expectedOwnership &&
		version.WriterGeneration == source.WriterGeneration &&
		source.WorkspaceID.Valid &&
		source.OwnershipGeneration == expectedOwnership &&
		source.BaseWorkspaceVersionID == expectedParent &&
		(source.Status == db.WorkspaceLeaseStatusReleased || source.Status == db.WorkspaceLeaseStatusFenced)
}

func terminalizeActorAttempt(ctx context.Context, store db.Querier, authority runLeaseClaimAuthority, completion parsedActorCompletion, decision actorRunTerminalDecision, completedAt pgtype.Timestamptz) error {
	leaseStatus := db.RunLeaseStatusFailed
	outcome := pgvalue.Text("failed")
	reason := "actor_failed"
	var terminalError []byte
	if decision.runStatus == db.RunStatusSucceeded {
		leaseStatus = db.RunLeaseStatusCompleted
		outcome = pgvalue.Text("succeeded")
		reason = "completed"
	} else if decision.runStatus == db.RunStatusCancelled {
		leaseStatus = db.RunLeaseStatusCancelled
		outcome = pgvalue.Text("cancelled")
		reason = "session_interrupted"
	} else {
		reason = decision.runReason.String
		terminalError = completion.errorObject
	}
	if _, err := store.CompleteTaskRunLease(ctx, db.CompleteTaskRunLeaseParams{
		Status: leaseStatus, CompletedAt: completedAt, ReasonCode: pgvalue.Text(reason), Error: terminalError,
		TerminalRequestFingerprint: pgvalue.Text(completion.fingerprint), ID: authority.runLease.ID,
		RunID: authority.run.ID, WorkspaceID: authority.workspace.ID, AttemptNumber: authority.attempt.Number,
		LeaseSequence: authority.runLease.LeaseSequence,
	}); err != nil {
		return staleActorCompletion(err)
	}
	if _, err := store.CompleteActorAttempt(ctx, db.CompleteActorAttemptParams{
		TerminalSessionInputSequence: pgtype.Int8{Int64: authority.actor.CommittedInputSequence, Valid: true}, TerminalOutcome: outcome,
		ReasonCode: pgvalue.Text(reason), Error: terminalError, CompletedAt: completedAt,
		RunID: authority.run.ID, Number: authority.attempt.Number, WorkspaceID: authority.workspace.ID,
	}); err != nil {
		return staleActorCompletion(err)
	}
	return nil
}

type actorRunTerminalDecision struct {
	runStatus   db.RunStatus
	runReason   pgtype.Text
	actorStatus string
}

func decideActorRunTerminal(authority runLeaseClaimAuthority, completion parsedActorCompletion) actorRunTerminalDecision {
	decision := actorRunTerminalDecision{
		runStatus:   db.RunStatusSucceeded,
		actorStatus: authority.actor.Status,
	}
	if completion.kind == actorCompletionInterrupted {
		decision.runStatus = db.RunStatusCancelled
		decision.runReason = pgvalue.Text("session_interrupted")
		return decision
	}
	if completion.kind == actorCompletionFailed {
		decision.runStatus = db.RunStatusFailed
		decision.runReason = pgvalue.Text("actor_failed")
		return decision
	}
	if authority.actor.NextInputSequence-1 > authority.actor.CommittedInputSequence &&
		authority.actor.CommittedInputSequence <= authority.run.SessionInputStartSequence.Int64 {
		decision.runStatus = db.RunStatusFailed
		decision.runReason = pgvalue.Text("no_progress")
		return decision
	}
	if authority.actor.Status == "closing" && authority.actor.CloseSequence.Valid &&
		authority.actor.CommittedInputSequence >= authority.actor.CloseSequence.Int64 {
		decision.actorStatus = "closed"
		return decision
	}
	return decision
}

func finishActorRun(ctx context.Context, store db.Querier, authority runLeaseClaimAuthority, secrets []secret.DeliveryEnvelope, completion parsedActorCompletion, decision actorRunTerminalDecision, completedAt pgtype.Timestamptz) error {
	var failure []byte
	if decision.runStatus == db.RunStatusCancelled {
		var err error
		failure, err = runFailure("session_interrupted", "Session execution was interrupted")
		if err != nil {
			return err
		}
	}
	if decision.runStatus == db.RunStatusFailed {
		var err error
		if completion.kind == actorCompletionFailed {
			failure, err = runFailureFromCompletion(decision.runReason.String, completion.errorObject)
		} else {
			failure, err = runFailure(decision.runReason.String, "Actor returned without processing queued input")
		}
		if err != nil {
			return err
		}
	}
	if _, err := store.FinishActorRun(ctx, db.FinishActorRunParams{
		Status: decision.runStatus, Failure: failure, CompletedAt: completedAt,
		ID: authority.run.ID, WorkspaceID: authority.workspace.ID, SessionID: authority.actor.ID,
		AttemptNumber: authority.attempt.Number, RunLeaseID: authority.runLease.ID,
	}); err != nil {
		return staleActorCompletion(err)
	}
	if decision.runStatus == db.RunStatusSucceeded {
		actor, err := store.ReconcileActorTerminalRun(ctx, db.ReconcileActorTerminalRunParams{
			Status:      decision.actorStatus,
			CompletedAt: completedAt, EnvironmentID: authority.actor.EnvironmentID, ID: authority.actor.ID,
			WorkspaceID: authority.workspace.ID, RunID: authority.run.ID, ExpectedRunGeneration: authority.actor.RunGeneration,
		})
		if err != nil {
			return staleActorCompletion(err)
		}
		terminalActor := actor.Status == "closed"
		if terminalActor {
			body := []byte(`{}`)
			if _, err := store.AppendSessionEvent(ctx, db.AppendSessionEventParams{ID: pgvalue.UUID(uuid.NewV7()), EnvironmentID: actor.EnvironmentID, SessionID: actor.ID, Kind: "session." + actor.Status, Data: body, ProducerRunID: authority.run.ID, ProducerAttemptNumber: pgtype.Int4{Int32: authority.attempt.Number, Valid: true}, RunGeneration: pgtype.Int8{Int64: authority.actor.RunGeneration, Valid: true}}); err != nil {
				return err
			}

			if _, err := store.ReleaseActorWorkspaceOwner(ctx, db.ReleaseActorWorkspaceOwnerParams{
				CompletedAt: completedAt, ID: authority.workspace.ID,
				EnvironmentID: authority.run.EnvironmentID, SessionID: actor.ID,
				OwnershipGeneration: authority.workspace.OwnershipGeneration, WriterGeneration: authority.workspace.WriterGeneration,
			}); err != nil {
				return staleActorCompletion(err)
			}
		} else if actorNeedsContinuation(actor) {
			if err := createActorContinuation(ctx, store, actor, db.LockActorInputWorkspaceRow(authority.workspace), secrets, completedAt); err != nil {
				return err
			}
		}
	}
	eventKind := api.RunEventKindCompleted
	if decision.runStatus == db.RunStatusFailed {
		eventKind = api.RunEventKindFailed
	} else if decision.runStatus == db.RunStatusCancelled {
		eventKind = api.RunEventKindCancelled
	}
	payload, err := json.Marshal(struct {
		Reason string `json:"reason,omitempty"`
	}{Reason: decision.runReason.String})
	if err != nil {
		return err
	}
	if err := telemetry.ValidateEvent(eventKind, payload); err != nil {
		return err
	}
	if _, err := store.AppendRunEvent(ctx, db.AppendRunEventParams{OrgID: authority.run.OrgID, RunID: authority.run.ID, Kind: eventKind, Payload: payload}); err != nil {
		return fmt.Errorf("append actor terminal event: %w", err)
	}
	return nil
}

func actorNeedsContinuation(actor db.Session) bool {
	return (actor.Status == "open" || actor.Status == "closing") &&
		!actor.DispatchHoldID.Valid &&
		actor.CommittedInputSequence < actor.NextInputSequence-1
}

func createActorContinuation(ctx context.Context, store db.Querier, actor db.Session, ws db.LockActorInputWorkspaceRow, secrets []secret.DeliveryEnvelope, now pgtype.Timestamptz) error {
	runID := pgvalue.UUID(uuid.NewV7())
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
		EnvironmentID: actor.EnvironmentID, SessionID: actor.ID, WorkspaceID: ws.ID, ExpectedRunGeneration: actor.RunGeneration,
	})
	if err != nil {
		return staleActorCompletion(err)
	}
	if err := createActorAttemptSecretResolutions(ctx, store, ws.ID, run.ID, 1, secrets); err != nil {
		return err
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
	if errors.Is(err, errStaleWorkerClaims) {
		return err
	}
	if err == nil || errors.Is(err, pgx.ErrNoRows) || errors.Is(err, errStaleRunLeaseClaim) || errors.Is(err, errStaleRunFinalization) {
		return errStaleActorCompletion
	}
	return err
}

func getActorWorkspaceVersion(
	ctx context.Context,
	store db.Querier,
	authority runLeaseClaimAuthority,
	versionID pgtype.UUID,
) (db.GetComputerVersionAuthorityRow, error) {
	return store.GetComputerVersionAuthority(ctx, db.GetComputerVersionAuthorityParams{
		OrgID: authority.run.OrgID, ProjectID: authority.run.ProjectID,
		EnvironmentID: authority.run.EnvironmentID, WorkspaceID: authority.workspace.ID, VersionID: versionID,
	})
}
