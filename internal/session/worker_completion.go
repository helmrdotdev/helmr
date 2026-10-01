package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

var (
	// ErrStaleCompletion reports an Actor completion whose receipt no longer
	// addresses the finalizing execution it names, or whose replay differs.
	ErrStaleCompletion = errors.New("actor completion receipt is stale")
	// ErrStopCleanupPending reports an interrupted Actor completion whose
	// owned execution scopes are not yet reconciled.
	ErrStopCleanupPending = errors.New("owned execution cleanup is pending")
	// ErrCompletionAdmission reports an Actor completion whose execution's
	// Secret deliveries are unavailable.
	ErrCompletionAdmission = errors.New("actor completion admission is invalid")
)

// ActorCompletionKind is the outcome an Actor execution reports.
type ActorCompletionKind string

const (
	ActorSucceeded   ActorCompletionKind = "succeeded"
	ActorFailed      ActorCompletionKind = "failed"
	ActorInterrupted ActorCompletionKind = "interrupted"
)

// ActorCompletion is a worker's report that its finalizing Actor execution
// ended in the Run generation: it succeeded, failed with an error, or was
// interrupted under the hold, on the Turn when it had one. The operation is
// the finalization the worker began; the fingerprint identifies the worker's
// normalized request for replay.
type ActorCompletion struct {
	Kind          ActorCompletionKind
	RunGeneration int64
	HoldID        uuid.UUID
	TurnID        *uuid.UUID
	Error         json.RawMessage
	OperationID   uuid.UUID
	Fingerprint   string
}

// CompleteActorFromRun terminates the worker's finalizing Actor execution in
// one transaction as completeActor does. When the transaction fails, it reads
// the completion's durable receipt from replays, outside the transaction, so
// an uncertain commit or a concurrent identical completion still succeeds; a
// different receipt is ErrStaleCompletion. Unavailable Secret deliveries are
// then ErrCompletionAdmission.
func CompleteActorFromRun(ctx context.Context, txb db.TxBeginner, replays CompletionReplays, fence run.ExecutionFence, completion ActorCompletion) error {
	err := db.RunTx(ctx, txb, func(tx pgx.Tx) error { return completeActor(ctx, tx, fence, completion) })
	if err == nil {
		return nil
	}
	// Preserve a durable receipt across uncertain commit or concurrent completion.
	replayed, replayErr := completionReplayed(ctx, replays, fence, completion.Fingerprint)
	if replayed {
		return nil
	}
	if errors.Is(replayErr, ErrStaleCompletion) {
		return replayErr
	}
	if replayErr != nil {
		return errors.Join(err, fmt.Errorf("check actor completion replay: %w", replayErr))
	}
	if errors.Is(err, secret.ErrDeliveryUnavailable) {
		return fmt.Errorf("%w: %v", ErrCompletionAdmission, err)
	}
	return err
}

// completeActor replays a completed receipt, or locks the finalizing
// execution and its owned graph and terminates it: it validates the
// completion against the execution and its Session, honors a stop that won
// after the worker froze its receipt, requires owned cleanup for an
// interruption, cancels a failed Run's descendants, settles an interrupted
// Turn, completes the lease and attempt, finishes the Run, settles the
// Session, appends the Run's terminal event and requires the lease and the
// Instance writer to be live before and after the writes.
func completeActor(ctx context.Context, tx pgx.Tx, fence run.ExecutionFence, completion ActorCompletion) error {
	q := db.New(tx)
	replayed, err := completionReplayed(ctx, q, fence, completion.Fingerprint)
	if err != nil || replayed {
		return err
	}
	authority, ownedGraph, err := run.LockFinalizingExecution(ctx, tx, fence)
	if err != nil {
		return staleCompletion(err)
	}
	// session is the locked Session. Before the terminal writes, an interruption
	// settles the held Turn and advances its input cursor.
	session, lease, instance := authority.Session(), authority.Lease(), authority.Instance()

	if err := validateCompletion(ctx, q, completion, authority); err != nil {
		return err
	}
	// Stop admission can win after the worker has frozen its completion receipt.
	// Keep that receipt's fingerprint, but honor the matching stop authority.
	if completion.Kind != ActorInterrupted && session.DispatchHoldID.Valid {
		completion.Kind = ActorInterrupted
	}
	if completion.Kind == ActorInterrupted {
		excluded, err := q.OwnedRunScopesReconciled(ctx, authority.Run().ID)
		if err != nil {
			return err
		}
		if !excluded {
			return ErrStopCleanupPending
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
		return ErrStaleCompletion
	}

	decision := decideTerminal(terminalStateOf(authority), completion)
	failed := decision.runStatus == db.RunStatusFailed
	if failed {
		if _, err := ownedGraph.CancelDescendants(ctx); err != nil {
			return err
		}
	}

	if completion.Kind == ActorInterrupted {
		if err := CompleteInterruption(ctx, tx, session, authority.Computer().HeadDiskVersionID, completion.Fingerprint); err != nil {
			return err
		}
		settled, err := q.GetSession(ctx, db.GetSessionParams{EnvironmentID: session.EnvironmentID, ID: session.ID})
		if err != nil {
			return err
		}
		session.CommittedInputSequence = settled.CommittedInputSequence
	}
	if err := run.CompleteActorAttempt(ctx, tx, authority, run.ActorAttemptTerminal{
		Status: decision.runStatus, Reason: decision.runReason.String, Error: completion.Error,
		InputSequence: session.CommittedInputSequence, Fingerprint: completion.Fingerprint, CompletedAt: completedAt,
	}); err != nil {
		return staleCompletion(err)
	}

	if err := finishRun(ctx, tx, authority, session, completion, decision, completedAt); err != nil {
		return err
	}
	now, err := q.GetTaskCompletionTime(ctx)
	if err != nil {
		return err
	}
	if !now.Valid || !now.Time.Before(lease.ExpiresAt.Time) || !now.Time.Before(instance.WriterExpiresAt.Time) {
		return ErrStaleCompletion
	}
	return nil
}

// CompletionReplays reads the Actor completion a Run lease recorded.
type CompletionReplays interface {
	GetActorCompletionReplay(context.Context, db.GetActorCompletionReplayParams) (pgtype.Text, error)
}

// completionReplayed reports whether the fenced lease already recorded the
// completion with the fingerprint. A different recorded completion is
// ErrStaleCompletion.
func completionReplayed(ctx context.Context, store CompletionReplays, fence run.ExecutionFence, fingerprint string) (bool, error) {
	receipt, err := store.GetActorCompletionReplay(ctx, db.GetActorCompletionReplayParams{RunLeaseID: fence.LeaseID, LeaseSequence: fence.LeaseSequence, WorkerGroupID: fence.WorkerGroupID, WorkerHostID: fence.WorkerHostID})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !receipt.Valid || receipt.String != fingerprint {
		return false, ErrStaleCompletion
	}
	return true, nil
}

func validateCompletion(
	ctx context.Context,
	store db.Querier,
	completion ActorCompletion,
	authority run.Execution,
) error {
	runRow, attempt, session, lease, computerRow := authority.Run(), authority.Attempt(), authority.Session(), authority.Lease(), authority.Computer()
	if completion.RunGeneration <= 0 || completion.RunGeneration != session.RunGeneration {
		return ErrStaleCompletion
	}
	if completion.Kind == ActorInterrupted {
		var turnID pgtype.UUID
		if completion.TurnID != nil {
			turnID = pgvalue.UUID(*completion.TurnID)
		}
		if session.DispatchHoldID != pgvalue.UUID(completion.HoldID) || session.DispatchHoldReason.String != "interrupt_requested" ||
			session.DispatchHoldRunID != runRow.ID || !session.DispatchHoldAttemptNumber.Valid || session.DispatchHoldAttemptNumber.Int32 != attempt.Number ||
			!session.DispatchHoldRunGeneration.Valid || session.DispatchHoldRunGeneration.Int64 != completion.RunGeneration || session.ActiveTurnID != turnID {
			return ErrStaleCompletion
		}
		if turnID.Valid {
			unsettled, err := store.SessionTurnHasUnsettledWork(ctx, db.SessionTurnHasUnsettledWorkParams{SessionID: session.ID, TurnID: turnID})
			if err != nil {
				return err
			}
			if !unsettled.Valid || unsettled.Bool {
				return ErrStaleCompletion
			}
		}
	} else {
		if session.ActiveTurnID.Valid && completion.Kind != ActorFailed {
			return ErrStaleCompletion
		}
		if session.DispatchHoldID.Valid && (session.DispatchHoldReason.String != "interrupt_requested" ||
			session.DispatchHoldRunID != runRow.ID ||
			!session.DispatchHoldAttemptNumber.Valid || session.DispatchHoldAttemptNumber.Int32 != attempt.Number ||
			!session.DispatchHoldRunGeneration.Valid || session.DispatchHoldRunGeneration.Int64 != completion.RunGeneration) {
			return ErrStaleCompletion
		}
	}
	if runRow.EntrypointKind != "actor" || !runRow.SessionID.Valid || runRow.SessionID != session.ID ||
		runRow.ParentRunID.Valid || runRow.ParentOwnsLifecycle.Valid ||
		lease.Status != db.RunLeaseStatusFinalizing || !attempt.EntrypointEnteredAt.Valid ||
		runRow.ActiveStartedAt.Valid || !lease.FinalizationOperationID.Valid ||
		!lease.FinalizationStartedAt.Valid ||
		!lease.FinalizationRequestFingerprint.Valid || !session.CurrentRunID.Valid || session.CurrentRunID != runRow.ID ||
		(session.Status != "open" && session.Status != "closing") ||
		!computerRow.HeadDiskVersionID.Valid ||
		!attempt.SessionInputStartSequence.Valid || !runRow.SessionInputStartSequence.Valid || !runRow.SessionInputHighWatermark.Valid {
		return ErrStaleCompletion
	}

	cursor := session.CommittedInputSequence
	if cursor < attempt.SessionInputStartSequence.Int64 || cursor < session.CommittedInputSequence || cursor >= session.NextInputSequence {
		return ErrStaleCompletion
	}
	if lease.FinalizationOperationID != pgvalue.UUID(completion.OperationID) {
		return ErrStaleCompletion
	}

	clear, err := store.RunFinalizationScopeIsClear(ctx, db.RunFinalizationScopeIsClearParams{RunID: runRow.ID, AttemptNumber: attempt.Number, ComputerID: computerRow.ID})
	if err != nil {
		return err
	}
	if !clear {
		return ErrStaleCompletion
	}
	return nil
}

type terminalDecision struct {
	runStatus   db.RunStatus
	runReason   pgtype.Text
	sessionStatus string
}

// terminalState is the part of a locked Actor execution that decides its
// Run's terminal status.
type terminalState struct {
	run     db.Run
	session db.Session
}

func terminalStateOf(execution run.Execution) terminalState {
	return terminalState{run: execution.Run(), session: execution.Session()}
}

func decideTerminal(state terminalState, completion ActorCompletion) terminalDecision {
	decision := terminalDecision{
		runStatus:   db.RunStatusSucceeded,
		sessionStatus: state.session.Status,
	}
	if completion.Kind == ActorInterrupted {
		decision.runStatus = db.RunStatusCancelled
		decision.runReason = pgvalue.Text("session_interrupted")
		return decision
	}
	if completion.Kind == ActorFailed {
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

// finishRun records the Run's terminal status and failure, settles the
// Session (a failed execution, or a succeeded Run's Session with its
// lifecycle reconciliation when it is closing or can continue), and appends
// the Run's terminal event.
func finishRun(ctx context.Context, tx pgx.Tx, authority run.Execution, session db.Session, completion ActorCompletion, decision terminalDecision, completedAt pgtype.Timestamptz) error {
	runRow, computerRow := authority.Run(), authority.Computer()
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
		if completion.Kind == ActorFailed {
			failure, err = runFailureFromCompletion(decision.runReason.String, completion.Error)
		} else {
			failure, err = runFailure(decision.runReason.String, "Actor returned without processing queued input")
		}
		if err != nil {
			return err
		}
	}
	if err := run.FinishActorRun(ctx, tx, authority, decision.runStatus, failure, completedAt); err != nil {
		return staleCompletion(err)
	}
	if decision.runStatus == db.RunStatusFailed {
		if err := FailExecution(ctx, tx, session, failure, completion.Fingerprint, completedAt); err != nil {
			return err
		}
	}
	if decision.runStatus == db.RunStatusSucceeded {
		reconciled, err := store.ReconcileSessionTerminalRun(ctx, db.ReconcileSessionTerminalRunParams{
			Status:      decision.sessionStatus,
			CompletedAt: completedAt, EnvironmentID: session.EnvironmentID, ID: session.ID,
			ComputerID: computerRow.ID, RunID: runRow.ID, ExpectedRunGeneration: session.RunGeneration,
		})
		if err != nil {
			return staleCompletion(err)
		}
		if reconciled.Status == "closing" || CanStartContinuation(reconciled) {
			if err := store.CreateSessionLifecycleReconcileOutbox(ctx, db.CreateSessionLifecycleReconcileOutboxParams{ID: pgvalue.UUID(uuid.NewV7()), EnvironmentID: reconciled.EnvironmentID, SessionID: reconciled.ID}); err != nil {
				return err
			}
		}

	}
	return run.AppendActorTerminalEvent(ctx, tx, authority, decision.runStatus, decision.runReason.String)
}

// runFailure is a Run failure body with the code, message and empty details.
func runFailure(code, message string) (json.RawMessage, error) {
	return json.Marshal(failureBody{Code: code, Message: message, Details: json.RawMessage("{}")})
}

// runFailureFromCompletion is the Run failure body of a failed completion's
// error: its message and object details, empty when absent.
func runFailureFromCompletion(code string, raw []byte) (json.RawMessage, error) {
	var completion struct {
		Message string          `json:"message"`
		Details json.RawMessage `json:"details"`
	}
	if err := json.Unmarshal(raw, &completion); err != nil || completion.Message == "" {
		return nil, errors.New("run completion failure is invalid")
	}
	if len(completion.Details) == 0 {
		completion.Details = json.RawMessage("{}")
	}
	var details map[string]json.RawMessage
	if err := json.Unmarshal(completion.Details, &details); err != nil || details == nil {
		return nil, errors.New("run completion failure details must be an object")
	}
	return json.Marshal(failureBody{Code: code, Message: completion.Message, Details: completion.Details})
}

// failureBody is a Run failure as Run reads present it.
type failureBody struct {
	Code    string          `json:"code"`
	Message string          `json:"message"`
	Details json.RawMessage `json:"details"`
}

func staleCompletion(err error) error {
	if err == nil || errors.Is(err, pgx.ErrNoRows) {
		return ErrStaleCompletion
	}
	return err
}
