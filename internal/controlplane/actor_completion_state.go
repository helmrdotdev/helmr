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
	// actor is the locked Session. Before the terminal writes, an interruption
	// settles the held Turn and advances its input cursor.
	actor, lease, instance := authority.Session(), authority.Lease(), authority.Instance()

	if err := validateActorCompletionAuthority(ctx, q, completion, authority); err != nil {
		return err
	}
	// Stop admission can win after the worker has frozen its completion receipt.
	// Keep that receipt's fingerprint, but honor the matching stop authority.
	if completion.kind != actorCompletionInterrupted && actor.DispatchHoldID.Valid {
		completion.kind = actorCompletionInterrupted
	}
	if completion.kind == actorCompletionInterrupted {
		excluded, err := q.OwnedRunScopesReconciled(ctx, authority.Run().ID)
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
	if !completedAt.Time.Before(lease.ExpiresAt.Time) || !completedAt.Time.Before(instance.WriterExpiresAt.Time) || lease.FinalizationStartedAt.Time.After(completedAt.Time) {
		return errStaleActorCompletion
	}

	decision := decideActorRunTerminal(actorRunTerminalStateOf(authority), completion)
	failed := decision.runStatus == db.RunStatusFailed
	if failed {
		if _, err := ownedGraph.CancelDescendants(ctx); err != nil {
			return err
		}
	}

	if completion.kind == actorCompletionInterrupted {
		if err := session.CompleteInterruption(ctx, tx, actor, authority.Computer().HeadDiskVersionID, completion.fingerprint); err != nil {
			return err
		}
		settled, err := q.GetActor(ctx, db.GetActorParams{EnvironmentID: actor.EnvironmentID, ID: actor.ID})
		if err != nil {
			return err
		}
		actor.CommittedInputSequence = settled.CommittedInputSequence
	}
	if err := terminalizeActorAttempt(ctx, q, authority, actor, completion, decision, completedAt); err != nil {
		return err
	}

	if err := finishActorRun(ctx, tx, authority, actor, completion, decision, completedAt); err != nil {
		return err
	}
	now, err := q.GetTaskCompletionTime(ctx)
	if err != nil {
		return err
	}
	if !now.Valid || !now.Time.Before(lease.ExpiresAt.Time) || !now.Time.Before(instance.WriterExpiresAt.Time) {
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
	authority run.Execution,
) error {
	runRow, attempt, actor, lease, computerRow := authority.Run(), authority.Attempt(), authority.Session(), authority.Lease(), authority.Computer()
	if completion.runGeneration <= 0 || completion.runGeneration != actor.RunGeneration {
		return errStaleActorCompletion
	}
	if completion.kind == actorCompletionInterrupted {
		var turnID pgtype.UUID
		if completion.turnID != nil {
			turnID = pgvalue.UUID(*completion.turnID)
		}
		if actor.DispatchHoldID != pgvalue.UUID(completion.holdID) || actor.DispatchHoldReason.String != "interrupt_requested" ||
			actor.DispatchHoldRunID != runRow.ID || !actor.DispatchHoldAttemptNumber.Valid || actor.DispatchHoldAttemptNumber.Int32 != attempt.Number ||
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
			actor.DispatchHoldRunID != runRow.ID ||
			!actor.DispatchHoldAttemptNumber.Valid || actor.DispatchHoldAttemptNumber.Int32 != attempt.Number ||
			!actor.DispatchHoldRunGeneration.Valid || actor.DispatchHoldRunGeneration.Int64 != completion.runGeneration) {
			return errStaleActorCompletion
		}
	}
	if runRow.EntrypointKind != "actor" || !runRow.SessionID.Valid || runRow.SessionID != actor.ID ||
		runRow.ParentRunID.Valid || runRow.ParentOwnsLifecycle.Valid ||
		lease.Status != db.RunLeaseStatusFinalizing || !attempt.EntrypointEnteredAt.Valid ||
		runRow.ActiveStartedAt.Valid || !lease.FinalizationOperationID.Valid ||
		!lease.FinalizationStartedAt.Valid ||
		!lease.FinalizationRequestFingerprint.Valid || !actor.CurrentRunID.Valid || actor.CurrentRunID != runRow.ID ||
		(actor.Status != "open" && actor.Status != "closing") ||
		!computerRow.HeadDiskVersionID.Valid ||
		!attempt.SessionInputStartSequence.Valid || !runRow.SessionInputStartSequence.Valid || !runRow.SessionInputHighWatermark.Valid {
		return errStaleActorCompletion
	}

	cursor := actor.CommittedInputSequence
	if cursor < attempt.SessionInputStartSequence.Int64 || cursor < actor.CommittedInputSequence || cursor >= actor.NextInputSequence {
		return errStaleActorCompletion
	}
	if lease.FinalizationOperationID != pgvalue.UUID(completion.operationID) {
		return errStaleActorCompletion
	}

	clear, err := store.RunFinalizationScopeIsClear(ctx, db.RunFinalizationScopeIsClearParams{RunID: runRow.ID, AttemptNumber: attempt.Number, ComputerID: computerRow.ID})
	if err != nil {
		return err
	}
	if !clear {
		return errStaleActorCompletion
	}
	return nil
}

func terminalizeActorAttempt(ctx context.Context, store db.Querier, authority run.Execution, actor db.Session, completion parsedActorCompletion, decision actorRunTerminalDecision, completedAt pgtype.Timestamptz) error {
	runRow, attempt, lease, computerRow := authority.Run(), authority.Attempt(), authority.Lease(), authority.Computer()
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
		TerminalRequestFingerprint: pgvalue.Text(completion.fingerprint), ID: lease.ID,
		RunID: runRow.ID, ComputerID: computerRow.ID, AttemptNumber: attempt.Number,
		LeaseSequence: lease.LeaseSequence,
	}); err != nil {
		return staleActorCompletion(err)
	}
	if _, err := store.CompleteActorAttempt(ctx, db.CompleteActorAttemptParams{
		TerminalSessionInputSequence: pgtype.Int8{Int64: actor.CommittedInputSequence, Valid: true}, TerminalOutcome: outcome,
		ReasonCode: pgvalue.Text(reason), Error: terminalError, CompletedAt: completedAt,
		RunID: runRow.ID, Number: attempt.Number, ComputerID: computerRow.ID,
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

// actorRunTerminalState is the part of a locked Actor execution that decides
// its Run's terminal status.
type actorRunTerminalState struct {
	run     db.Run
	session db.Session
}

func actorRunTerminalStateOf(execution run.Execution) actorRunTerminalState {
	return actorRunTerminalState{run: execution.Run(), session: execution.Session()}
}

func decideActorRunTerminal(state actorRunTerminalState, completion parsedActorCompletion) actorRunTerminalDecision {
	decision := actorRunTerminalDecision{
		runStatus:   db.RunStatusSucceeded,
		actorStatus: state.session.Status,
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
	if state.run.SessionInputHighWatermark.Int64 > state.run.SessionInputStartSequence.Int64 &&
		state.session.CommittedInputSequence <= state.run.SessionInputStartSequence.Int64 {
		decision.runStatus = db.RunStatusFailed
		decision.runReason = pgvalue.Text("no_progress")
		return decision
	}

	return decision
}

func finishActorRun(ctx context.Context, tx pgx.Tx, authority run.Execution, actor db.Session, completion parsedActorCompletion, decision actorRunTerminalDecision, completedAt pgtype.Timestamptz) error {
	runRow, attempt, lease, computerRow := authority.Run(), authority.Attempt(), authority.Lease(), authority.Computer()
	store := db.New(tx)
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
		ID: runRow.ID, ComputerID: computerRow.ID, SessionID: actor.ID,
		AttemptNumber: attempt.Number, RunLeaseID: lease.ID,
	}); err != nil {
		return staleActorCompletion(err)
	}
	if decision.runStatus == db.RunStatusFailed {
		if err := session.FailExecution(ctx, tx, actor, failure, completion.fingerprint, completedAt); err != nil {
			return err
		}
	}
	if decision.runStatus == db.RunStatusSucceeded {
		reconciled, err := store.ReconcileActorTerminalRun(ctx, db.ReconcileActorTerminalRunParams{
			Status:      decision.actorStatus,
			CompletedAt: completedAt, EnvironmentID: actor.EnvironmentID, ID: actor.ID,
			ComputerID: computerRow.ID, RunID: runRow.ID, ExpectedRunGeneration: actor.RunGeneration,
		})
		if err != nil {
			return staleActorCompletion(err)
		}
		if reconciled.Status == "closing" || session.CanStartContinuation(reconciled) {
			if err := store.CreateSessionLifecycleReconcileOutbox(ctx, db.CreateSessionLifecycleReconcileOutboxParams{ID: pgvalue.UUID(uuid.NewV7()), EnvironmentID: reconciled.EnvironmentID, SessionID: reconciled.ID}); err != nil {
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
	if _, err := store.AppendRunEvent(ctx, db.AppendRunEventParams{OrgID: runRow.OrgID, RunID: runRow.ID, Kind: eventKind, Payload: payload}); err != nil {
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
