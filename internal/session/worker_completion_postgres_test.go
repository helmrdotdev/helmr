package session

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run"
	"github.com/helmrdotdev/helmr/internal/session/sessiontest"
)

// interruptedCompletion begins the execution's finalization as the worker
// does and returns its interrupted completion under the hold, on the Turn
// when there is one.
func interruptedCompletion(t *testing.T, f *sessiontest.Execution, hold uuid.UUID, turn *uuid.UUID) ActorCompletion {
	t.Helper()
	operation := uuid.NewV7()
	if _, err := run.BeginFinalization(t.Context(), f.Pool, run.ExecutionFinalization{
		Fence: f.Fence(), RunID: pgvalue.UUID(f.RunID), AttemptNumber: f.Claim.Attempt().Number,
		OperationID: pgvalue.UUID(operation), Fingerprint: dbtest.Digest("finalization " + operation.String()),
	}); err != nil {
		t.Fatal(err)
	}
	return ActorCompletion{
		Kind: ActorInterrupted, RunGeneration: f.Claim.Session().RunGeneration, HoldID: hold, TurnID: turn,
		OperationID: operation, Fingerprint: dbtest.Digest("interrupted completion " + operation.String()),
	}
}

// completeActorFromRun completes the execution as the worker does.
func completeActorFromRun(t *testing.T, f *sessiontest.Execution, completion ActorCompletion) error {
	t.Helper()
	return CompleteActorFromRun(t.Context(), f.Pool, db.New(f.Pool), f.Fence(), completion)
}

func TestSessionInterruptedCompletionRejectsChangedHoldPostgres(t *testing.T) {
	f := newExecution(t, json.RawMessage(`{"sequence":1}`), true)
	scope := receiveTurn(t, f, 1)
	stopped, err := interruptTurn(t.Context(), f, scope, "stop")
	if err != nil {
		t.Fatal(err)
	}
	completion := interruptedCompletion(t, f, *stopped.HoldID, &scope.TurnID)
	for _, change := range []string{"hold", "generation", "turn", "success"} {
		t.Run(change, func(t *testing.T) {
			changed := completion
			switch change {
			case "hold":
				changed.HoldID = uuid.NewV7()
			case "generation":
				changed.RunGeneration++
			case "turn":
				id := uuid.NewV7()
				changed.TurnID = &id
			case "success":
				changed.Kind = ActorSucceeded
				changed.HoldID = uuid.Nil()
				changed.TurnID = nil
			}
			changed.Fingerprint = dbtest.Digest("changed completion " + change)
			if err = completeActorFromRun(t, f, changed); !errors.Is(err, ErrStaleCompletion) {
				t.Fatalf("unexpected completion: %v", err)
			}
		})
	}
	actor, err := db.New(f.Pool).GetActor(t.Context(), db.GetActorParams{EnvironmentID: pgvalue.UUID(f.EnvironmentID), ID: pgvalue.UUID(f.SessionID)})
	if err != nil || actor.CommittedInputSequence != 0 || actor.DispatchHoldReason.String != "interrupt_requested" {
		t.Fatalf("rejected proof changed state: %+v %v", actor, err)
	}
}

func TestSessionBetweenTurnsInterruptionPostgres(t *testing.T) {
	f := newExecution(t, json.RawMessage(`{"sequence":1}`), true)
	canceler, err := run.NewCanceler(f.Pool)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = canceler.Cancel(t.Context(), run.CancellationRequest{OrgID: f.OrgID, ProjectID: f.ProjectID, EnvironmentID: f.EnvironmentID, RunID: f.RunID}); err != nil {
		t.Fatal(err)
	}
	var hold uuid.UUID
	if err = f.Pool.QueryRow(t.Context(), `SELECT dispatch_hold_id FROM sessions WHERE id=$1`, f.SessionID).Scan(&hold); err != nil {
		t.Fatal(err)
	}
	if err = completeActorFromRun(t, f, interruptedCompletion(t, f, hold, nil)); err != nil {
		t.Fatal(err)
	}
	var cursor int64
	var active, current *uuid.UUID
	var reason string
	var terminalCount int
	if err = f.Pool.QueryRow(t.Context(), `SELECT committed_input_sequence,active_turn_id,current_run_id,dispatch_hold_reason,(SELECT count(*) FROM session_events WHERE session_id=$1 AND kind='turn.interrupted') FROM sessions WHERE id=$1`, f.SessionID).Scan(&cursor, &active, &current, &reason, &terminalCount); err != nil {
		t.Fatal(err)
	}
	if cursor != 0 || active != nil || current != nil || reason != "interrupted" || terminalCount != 0 {
		t.Fatalf("between-Turn stop fabricated Turn: %d %v %v %s %d", cursor, active, current, reason, terminalCount)
	}
}

func TestSessionInterruptedCompletionRejectsUnacknowledgedMessagePostgres(t *testing.T) {
	f := newExecution(t, json.RawMessage(`{"sequence":1}`), true)
	scope := receiveTurn(t, f, 1)
	readyMessages(t, f, scope)
	admitMessage(t, f, "pending")
	claimMessage(t, f, scope)
	stopped, err := interruptTurn(t.Context(), f, scope, "stop")
	if err != nil {
		t.Fatal(err)
	}
	if err = completeActorFromRun(t, f, interruptedCompletion(t, f, *stopped.HoldID, &scope.TurnID)); !errors.Is(err, ErrStaleCompletion) {
		t.Fatalf("unacknowledged callback settled: %v", err)
	}
}

func TestSessionHotChildCallStopConvergesPostgres(t *testing.T) {
	f := newExecution(t, json.RawMessage(`{"sequence":1}`), true)
	scope := receiveTurn(t, f, 1)
	declareNoPayloadTask(t, f)
	invoked := invokeChild(t, f, scope, f.ComputerID, false, "stop-child")
	if invoked.Call == nil {
		t.Fatalf("child call: %+v", invoked)
	}
	stopped, err := interruptTurn(t.Context(), f, scope, "stop")
	if err != nil {
		t.Fatal(err)
	}
	var childState, rootState string
	if err = f.Pool.QueryRow(t.Context(), `SELECT coalesce(c.status,'not_started'),r.status FROM run_waits w LEFT JOIN runs c ON c.id=w.child_run_id JOIN runs r ON r.id=w.run_id WHERE w.id=$1`, invoked.Call.RunWaitID).Scan(&childState, &rootState); err != nil {
		t.Fatal(err)
	}
	if childState != "cancelled" || rootState != "running" {
		t.Fatalf("owned stop: root=%s child=%s", rootState, childState)
	}
	if err = completeActorFromRun(t, f, interruptedCompletion(t, f, *stopped.HoldID, &scope.TurnID)); err != nil {
		t.Fatal(err)
	}
}

func TestSessionControlObservationDoesNotLockWorkerSupplyPostgres(t *testing.T) {
	f := newExecution(t, json.RawMessage(`{"sequence":1}`), true)
	scope := receiveTurn(t, f, 1)
	stopped, err := interruptTurn(t.Context(), f, scope, "stop")
	if err != nil {
		t.Fatal(err)
	}
	tx, err := f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if _, err = tx.Exec(t.Context(), `SELECT id FROM worker_groups WHERE id=$1 FOR UPDATE`, f.Worker.GroupID); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	state, err := ReadControl(ctx, db.New(f.Pool), f.Fence(), scope.RunGeneration)
	if err != nil {
		t.Fatalf("advisory read blocked on supply mutation: %v", err)
	}
	if state.DispatchHoldID != pgvalue.UUID(*stopped.HoldID) || state.ActiveTurnID != pgvalue.UUID(scope.TurnID) {
		t.Fatalf("wrong control: %+v", state)
	}
	stale := f.Fence()
	stale.LeaseSequence++
	if _, err = ReadControl(t.Context(), db.New(f.Pool), stale, scope.RunGeneration); !errors.Is(err, run.ErrTurnScope) {
		t.Fatalf("stale control read: %v", err)
	}
}

// A paused Worker Group stops admission only; a worker with refreshed
// claims keeps reading Session control for started work.
func TestSessionControlObservationOnPausedWorkerGroupPostgres(t *testing.T) {
	f := newExecution(t, json.RawMessage(`{"sequence":1}`), true)
	scope := receiveTurn(t, f, 1)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE worker_groups SET status='paused' WHERE id=$1`, f.Worker.GroupID)
	state, err := ReadControl(t.Context(), db.New(f.Pool), f.Fence(), scope.RunGeneration)
	if err != nil || state.ActiveTurnID != pgvalue.UUID(scope.TurnID) {
		t.Fatalf("Session control on paused Group: %+v %v", state, err)
	}
}
