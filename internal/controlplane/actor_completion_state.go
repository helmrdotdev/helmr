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
	"github.com/helmrdotdev/helmr/internal/session"
	"github.com/helmrdotdev/helmr/internal/telemetry"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

var errStaleActorCompletion = errors.New("actor completion receipt is stale")
var errActorStopCleanupPending = errors.New("owned execution cleanup is pending")

func completeActorExecution(ctx context.Context, tx pgx.Tx, fence run.ExecutionFence, completion parsedActorCompletion) error {
	q := db.New(tx)
	replayed, err := actorExecutionReplayed(ctx, q, fence, completion.fingerprint)
	if err != nil || replayed {
		return err
	}
	authority, ownedGraph, err := run.LockFinalizingExecution(ctx, tx, fence)
	if err != nil {
		return staleActorCompletion(err)
	}

	if err := validateActorCompletionAuthority(ctx, q, completion, authority); err != nil {
		return err
	}
	// Stop admission can win after the worker has frozen its completion receipt.
	// Keep that receipt's fingerprint, but honor the matching stop authority.
	if completion.kind != actorCompletionInterrupted && authority.Session.DispatchHoldID.Valid {
		completion.kind = actorCompletionInterrupted
	}
	if completion.kind == actorCompletionInterrupted {
		excluded, err := q.OwnedRunScopesReconciled(ctx, authority.Run.ID)
		if err != nil {
			return err
		}
		if !excluded {
			return errActorStopCleanupPending
		}
	}

	completedAt, err := q.GetTaskCompletionTime(ctx)
	if err != nil || !completedAt.Valid {
		if err == nil {
			err = errors.New("database actor completion time is unavailable")
		}
		return err
	}
	if !completedAt.Time.Before(authority.Lease.ExpiresAt.Time) || !completedAt.Time.Before(authority.Instance.WriterExpiresAt.Time) || authority.Lease.FinalizationStartedAt.Time.After(completedAt.Time) {
		return errStaleActorCompletion
	}

	decision := decideActorRunTerminal(authority, completion)
	failed := decision.runStatus == db.RunStatusFailed
	if failed {
		if _, err := ownedGraph.CancelDescendants(ctx); err != nil {
			return err
		}
	}

	if completion.kind == actorCompletionInterrupted {
		if err := session.CompleteInterruption(ctx, q, authority.Session, authority.Computer.HeadDiskVersionID, completion.fingerprint); err != nil {
			return err
		}
		settled, err := q.GetActor(ctx, db.GetActorParams{EnvironmentID: authority.Session.EnvironmentID, ID: authority.Session.ID})
		if err != nil {
			return err
		}
		authority.Session.CommittedInputSequence = settled.CommittedInputSequence
	}
	if err := terminalizeActorAttempt(ctx, q, authority, completion, decision, completedAt); err != nil {
		return err
	}

	if err := finishActorRun(ctx, q, authority, completion, decision, completedAt); err != nil {
		return err
	}
	now, err := q.GetTaskCompletionTime(ctx)
	if err != nil {
		return err
	}
	if !now.Valid || !now.Time.Before(authority.Lease.ExpiresAt.Time) || !now.Time.Before(authority.Instance.WriterExpiresAt.Time) {
		return errStaleActorCompletion
	}
	return nil
}

type actorCompletionReplayStore interface {
	GetActorCompletionReplay(context.Context, db.GetActorCompletionReplayParams) (pgtype.Text, error)
}

func actorExecutionReplayed(ctx context.Context, store actorCompletionReplayStore, fence run.ExecutionFence, fingerprint string) (bool, error) {
	receipt, err := store.GetActorCompletionReplay(ctx, db.GetActorCompletionReplayParams{RunLeaseID: fence.LeaseID, LeaseSequence: fence.LeaseSequence, WorkerGroupID: fence.WorkerGroupID, WorkerHostID: fence.WorkerHostID})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !receipt.Valid || receipt.String != fingerprint {
		return false, errStaleActorCompletion
	}
	return true, nil
}

func validateActorCompletionAuthority(
	ctx context.Context,
	store db.Querier,
	completion parsedActorCompletion,
	authority run.ExecutionAuthority,
) error {
	actor := authority.Session
	if completion.runGeneration <= 0 || completion.runGeneration != actor.RunGeneration {
		return errStaleActorCompletion
	}
	if completion.kind == actorCompletionInterrupted {
		var turnID pgtype.UUID
		if completion.turnID != nil {
			turnID = pgvalue.UUID(*completion.turnID)
		}
		if actor.DispatchHoldID != pgvalue.UUID(completion.holdID) || actor.DispatchHoldReason.String != "interrupt_requested" ||
			actor.DispatchHoldRunID != authority.Run.ID || !actor.DispatchHoldAttemptNumber.Valid || actor.DispatchHoldAttemptNumber.Int32 != authority.Attempt.Number ||
			!actor.DispatchHoldRunGeneration.Valid || actor.DispatchHoldRunGeneration.Int64 != completion.runGeneration || actor.ActiveTurnID != turnID {
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
	} else {
		if actor.ActiveTurnID.Valid && completion.kind != actorCompletionFailed {
			return errStaleActorCompletion
		}
		if actor.DispatchHoldID.Valid && (actor.DispatchHoldReason.String != "interrupt_requested" ||
			actor.DispatchHoldRunID != authority.Run.ID ||
			!actor.DispatchHoldAttemptNumber.Valid || actor.DispatchHoldAttemptNumber.Int32 != authority.Attempt.Number ||
			!actor.DispatchHoldRunGeneration.Valid || actor.DispatchHoldRunGeneration.Int64 != completion.runGeneration) {
			return errStaleActorCompletion
		}
	}
	if authority.Run.EntrypointKind != "actor" || !authority.Run.SessionID.Valid || authority.Run.SessionID != actor.ID ||
		authority.Run.ParentRunID.Valid || authority.Run.ParentOwnsLifecycle.Valid ||
		authority.Lease.Status != db.RunLeaseStatusFinalizing || !authority.Attempt.EntrypointEnteredAt.Valid ||
		authority.Run.ActiveStartedAt.Valid || !authority.Lease.FinalizationOperationID.Valid ||
		!authority.Lease.FinalizationStartedAt.Valid ||
		!authority.Lease.FinalizationRequestFingerprint.Valid || !actor.CurrentRunID.Valid || actor.CurrentRunID != authority.Run.ID ||
		(actor.Status != "open" && actor.Status != "closing") ||
		!authority.Computer.HeadDiskVersionID.Valid ||
		!authority.Attempt.SessionInputStartSequence.Valid || !authority.Run.SessionInputStartSequence.Valid || !authority.Run.SessionInputHighWatermark.Valid {
		return errStaleActorCompletion
	}

	cursor := actor.CommittedInputSequence
	if cursor < authority.Attempt.SessionInputStartSequence.Int64 || cursor < actor.CommittedInputSequence || cursor >= actor.NextInputSequence {
		return errStaleActorCompletion
	}
	if authority.Lease.FinalizationOperationID != pgvalue.UUID(completion.operationID) {
		return errStaleActorCompletion
	}

	clear, err := store.RunFinalizationScopeIsClear(ctx, db.RunFinalizationScopeIsClearParams{RunID: authority.Run.ID, AttemptNumber: authority.Attempt.Number, ComputerID: authority.Computer.ID})
	if err != nil {
		return err
	}
	if !clear {
		return errStaleActorCompletion
	}
	return nil
}

func terminalizeActorAttempt(ctx context.Context, store db.Querier, authority run.ExecutionAuthority, completion parsedActorCompletion, decision actorRunTerminalDecision, completedAt pgtype.Timestamptz) error {
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
		TerminalRequestFingerprint: pgvalue.Text(completion.fingerprint), ID: authority.Lease.ID,
		RunID: authority.Run.ID, ComputerID: authority.Computer.ID, AttemptNumber: authority.Attempt.Number,
		LeaseSequence: authority.Lease.LeaseSequence,
	}); err != nil {
		return staleActorCompletion(err)
	}
	if _, err := store.CompleteActorAttempt(ctx, db.CompleteActorAttemptParams{
		TerminalSessionInputSequence: pgtype.Int8{Int64: authority.Session.CommittedInputSequence, Valid: true}, TerminalOutcome: outcome,
		ReasonCode: pgvalue.Text(reason), Error: terminalError, CompletedAt: completedAt,
		RunID: authority.Run.ID, Number: authority.Attempt.Number, ComputerID: authority.Computer.ID,
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

func decideActorRunTerminal(authority run.ExecutionAuthority, completion parsedActorCompletion) actorRunTerminalDecision {
	decision := actorRunTerminalDecision{
		runStatus:   db.RunStatusSucceeded,
		actorStatus: authority.Session.Status,
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
	// Only work visible at admission can make a clean return a no-progress exit.
	// Input arriving while the handler returns belongs to the next continuation.
	if authority.Run.SessionInputHighWatermark.Int64 > authority.Run.SessionInputStartSequence.Int64 &&
		authority.Session.CommittedInputSequence <= authority.Run.SessionInputStartSequence.Int64 {
		decision.runStatus = db.RunStatusFailed
		decision.runReason = pgvalue.Text("no_progress")
		return decision
	}

	return decision
}

func finishActorRun(ctx context.Context, store db.Querier, authority run.ExecutionAuthority, completion parsedActorCompletion, decision actorRunTerminalDecision, completedAt pgtype.Timestamptz) error {
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
		ID: authority.Run.ID, ComputerID: authority.Computer.ID, SessionID: authority.Session.ID,
		AttemptNumber: authority.Attempt.Number, RunLeaseID: authority.Lease.ID,
	}); err != nil {
		return staleActorCompletion(err)
	}
	if decision.runStatus == db.RunStatusFailed {
		if err := session.FailExecution(ctx, store, authority.Session, failure, completion.fingerprint, completedAt); err != nil {
			return err
		}
	}
	if decision.runStatus == db.RunStatusSucceeded {
		actor, err := store.ReconcileActorTerminalRun(ctx, db.ReconcileActorTerminalRunParams{
			Status:      decision.actorStatus,
			CompletedAt: completedAt, EnvironmentID: authority.Session.EnvironmentID, ID: authority.Session.ID,
			ComputerID: authority.Computer.ID, RunID: authority.Run.ID, ExpectedRunGeneration: authority.Session.RunGeneration,
		})
		if err != nil {
			return staleActorCompletion(err)
		}
		if actor.Status == "closing" || session.CanStartContinuation(actor) {
			if err := store.CreateSessionLifecycleReconcileOutbox(ctx, db.CreateSessionLifecycleReconcileOutboxParams{ID: pgvalue.UUID(uuid.NewV7()), EnvironmentID: actor.EnvironmentID, SessionID: actor.ID}); err != nil {
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
	if _, err := store.AppendRunEvent(ctx, db.AppendRunEventParams{OrgID: authority.Run.OrgID, RunID: authority.Run.ID, Kind: eventKind, Payload: payload}); err != nil {
		return fmt.Errorf("append actor terminal event: %w", err)
	}
	return nil
}

func staleActorCompletion(err error) error {
	if err == nil || errors.Is(err, pgx.ErrNoRows) {
		return errStaleActorCompletion
	}
	return err
}
