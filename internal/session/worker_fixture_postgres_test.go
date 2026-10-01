package session

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/idempotency"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run"
	"github.com/helmrdotdev/helmr/internal/run/runtest"
	"github.com/helmrdotdev/helmr/internal/session/sessiontest"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// newExecution builds an Actor execution whose Session holds the input as
// its first enqueued Turn, when there is one, and whose lease the worker
// claimed, started and entered when start is set.
func newExecution(t *testing.T, input json.RawMessage, start bool) *sessiontest.Execution {
	t.Helper()
	return prepareExecution(t, sessiontest.NewExecution(t), input, start)
}

// executionOn adds such an Actor execution to the run test database.
func executionOn(t *testing.T, base runtest.Fixture, input json.RawMessage, start bool) *sessiontest.Execution {
	t.Helper()
	return prepareExecution(t, sessiontest.ExecutionOn(t, base), input, start)
}

func prepareExecution(t *testing.T, f *sessiontest.Execution, input json.RawMessage, start bool) *sessiontest.Execution {
	t.Helper()
	if input != nil {
		if _, err := ApplyAdmission(t.Context(), f.Pool, AdmissionRequest{Target: executionTarget(f), Mode: EnqueueOnly, Data: input}); err != nil {
			t.Fatal(err)
		}
	}
	if start {
		f.ClaimAndStart(t)
	}
	return f
}

// executionTarget addresses the execution's Session.
func executionTarget(f *sessiontest.Execution) Target {
	return Target{EnvironmentID: f.EnvironmentID, SessionID: f.SessionID}
}

// receiveTurn activates the Turn at the input sequence, registering the
// worker's Actor input wait after the previous sequence while it is queued,
// and returns the Turn's producer scope.
func receiveTurn(t *testing.T, f *sessiontest.Execution, sequence int64) run.TurnScope {
	t.Helper()
	q := db.New(f.Pool)
	params := db.GetSessionTurnAtSequenceForUpdateParams{EnvironmentID: pgvalue.UUID(f.EnvironmentID), SessionID: pgvalue.UUID(f.SessionID), Sequence: sequence}
	input, err := q.GetSessionTurnAtSequenceForUpdate(t.Context(), params)
	if err != nil {
		t.Fatal(err)
	}
	if input.Status == "queued" {
		if _, err := RegisterInputWait(t.Context(), f.Pool, f.Fence(), inputWait(f, sequence-1)); err != nil {
			t.Fatal(err)
		}
		input, err = q.GetSessionTurnAtSequenceForUpdate(t.Context(), params)
		if err != nil {
			t.Fatal(err)
		}
	}
	if input.Status != "running" || !input.RunGeneration.Valid {
		t.Fatalf("input was not activated: %+v", input)
	}
	return run.TurnScope{EnvironmentID: f.EnvironmentID, SessionID: f.SessionID, TurnID: pgvalue.MustUUIDValue(input.ID), RunID: f.RunID, AttemptNumber: input.AttemptNumber.Int32, RunGeneration: input.RunGeneration.Int64}
}

// inputWait is a new Actor input wait after the cursor with the Actor's
// one-second idle timeout and empty annotations.
func inputWait(f *sessiontest.Execution, after int64) InputWait {
	waitID := uuid.NewV7()
	return InputWait{
		WaitID: waitID, SessionID: f.SessionID, AfterInputSequence: after,
		Fingerprint: dbtest.Digest("input wait " + waitID.String()), Metadata: []byte(`{}`), Tags: []string{},
		IdleTimeout: pgtype.Int8{Int64: 1000, Valid: true},
	}
}

// turnWork addresses the scope's Turn work.
func turnWork(scope run.TurnScope) TurnWork {
	return TurnWork{TurnID: scope.TurnID, RunGeneration: scope.RunGeneration}
}

// beginSettlement begins the Turn's settlement as the worker does.
func beginSettlement(t *testing.T, f *sessiontest.Execution, scope run.TurnScope) {
	t.Helper()
	if err := BeginSettlementFromRun(t.Context(), f.Pool, f.Fence(), turnWork(scope)); err != nil {
		t.Fatal(err)
	}
}

// turnCommit begins the Turn's settlement and returns the worker's commit of
// a completed result at the first input sequence.
func turnCommit(t *testing.T, f *sessiontest.Execution, scope run.TurnScope) TurnCommit {
	t.Helper()
	beginSettlement(t, f, scope)
	return completedCommit(t, scope, 1, json.RawMessage(`{"answer":42}`))
}

// completedCommit is a commit of the completed result to the input sequence,
// fingerprinted by its result.
func completedCommit(t *testing.T, scope run.TurnScope, sequence int64, result json.RawMessage) TurnCommit {
	t.Helper()
	return TurnCommit{
		TurnID: scope.TurnID, RunGeneration: scope.RunGeneration, Disposition: "completed", Result: result,
		Fingerprint: dbtest.Digest("turn commit " + scope.TurnID.String() + " " + string(result)), TargetInputSequence: sequence,
	}
}

// commitTurn activates, settles and commits the Turn at the input sequence
// with a null result, checks that the commit replays and that a different
// result conflicts, and that the Computer's head and the attempt's base are
// unchanged while the Session's input cursor advanced.
func commitTurn(t *testing.T, f *sessiontest.Execution, sequence int64) pgtype.UUID {
	t.Helper()
	var headBefore, baseBefore uuid.UUID
	if err := f.Pool.QueryRow(t.Context(), `SELECT w.head_disk_version_id,a.base_computer_disk_version_id FROM computers w JOIN run_leases l ON l.computer_id=w.id JOIN run_attempts a ON a.run_id=l.run_id AND a.number=l.attempt_number WHERE w.id=$1 AND l.id=$2`, f.ComputerID, f.Claim.Lease().ID).Scan(&headBefore, &baseBefore); err != nil {
		t.Fatal(err)
	}
	scope := receiveTurn(t, f, sequence)
	beginSettlement(t, f, scope)
	commit := completedCommit(t, scope, sequence, json.RawMessage(`null`))
	eventID, err := CommitTurnFromRun(t.Context(), f.Pool, f.Fence(), commit)
	if err != nil {
		t.Fatalf("commit Turn %d: %v", sequence, err)
	}
	replayed, err := CommitTurnFromRun(t.Context(), f.Pool, f.Fence(), commit)
	if err != nil || replayed != eventID {
		t.Fatalf("Turn replay: %+v %v", replayed, err)
	}
	conflicting := completedCommit(t, scope, sequence, json.RawMessage(`{"different":true}`))
	if _, err := CommitTurnFromRun(t.Context(), f.Pool, f.Fence(), conflicting); !errors.Is(err, ErrStaleTurnCommit) {
		t.Fatalf("conflicting replay: %v", err)
	}

	var head, base uuid.UUID
	var cursor int64
	var version pgtype.UUID
	var data []byte
	if err := f.Pool.QueryRow(t.Context(), `SELECT w.head_disk_version_id,a.base_computer_disk_version_id,s.committed_input_sequence,e.computer_disk_version_id,e.data FROM computers w JOIN run_leases l ON l.computer_id=w.id JOIN run_attempts a ON a.run_id=l.run_id AND a.number=l.attempt_number JOIN sessions s ON s.id=$3 JOIN session_events e ON e.id=$4 WHERE w.id=$1 AND l.id=$2`, f.ComputerID, f.Claim.Lease().ID, f.SessionID, eventID).Scan(&head, &base, &cursor, &version, &data); err != nil {
		t.Fatal(err)
	}
	if head != headBefore || base != baseBefore || cursor != sequence || version.Valid || strings.Contains(string(data), "computer_disk_version_id") {
		t.Fatalf("Turn changed persistence or failed to advance cursor: head=%s base=%s cursor=%d version=%v data=%s", head, base, cursor, version, data)
	}
	return eventID
}

// interruptTurn interrupts the Turn through the public Session operation and
// returns a committed rejection as its receipt.
func interruptTurn(ctx context.Context, f *sessiontest.Execution, scope run.TurnScope, key string) (ControlReceipt, error) {
	receipt, err := ApplyInterrupt(ctx, f.Pool, InterruptRequest{ControlRequest: ControlRequest{Target: Target{EnvironmentID: scope.EnvironmentID, SessionID: scope.SessionID}, IdempotencyKey: key}, TurnID: scope.TurnID})
	var rejection *OperationError
	if errors.As(err, &rejection) && rejection.Code == receipt.Code {
		err = nil
	}
	return receipt, err
}

// turnOutput is the worker's keyed output on the scope's Turn.
func turnOutput(scope run.TurnScope) TurnOutput {
	return TurnOutput{TurnID: scope.TurnID, RunGeneration: scope.RunGeneration, CorrelationID: uuid.NewV7(), Data: json.RawMessage(`{"actionBinding":"command-1","requestId":"native-1","type":"permission_granted"}`), IdempotencyKey: "permission-1"}
}

// appendTurnOutput appends the worker's Turn output and returns its event
// ID, or its committed rejection's code.
func appendTurnOutput(t *testing.T, f *sessiontest.Execution, output TurnOutput) (pgtype.UUID, string) {
	t.Helper()
	written, err := AppendTurnOutputFromRun(t.Context(), f.Pool, db.New(f.Pool), f.Fence(), output)
	var rejection *OperationError
	var collision idempotency.ConflictError
	switch {
	case errors.As(err, &rejection):
		return pgtype.UUID{}, rejection.Code
	case errors.As(err, &collision):
		return pgtype.UUID{}, "idempotency_conflict"
	case err != nil:
		t.Fatalf("append Turn output: %v", err)
	}
	return written.Event().ID, ""
}

// admitMessage sends a message to the execution's Session through the
// public Session operation.
func admitMessage(t *testing.T, f *sessiontest.Execution, key string) AdmissionReceipt {
	t.Helper()
	r, err := ApplyAdmission(t.Context(), f.Pool, AdmissionRequest{Target: executionTarget(f), Mode: SendMessageOrEnqueue, Data: json.RawMessage(`{"text":"steer"}`), IdempotencyKey: key})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// readyMessages declares the Turn ready for messages as the worker does.
func readyMessages(t *testing.T, f *sessiontest.Execution, scope run.TurnScope) {
	t.Helper()
	if err := DeclareMessageReadyFromRun(t.Context(), f.Pool, f.Fence(), turnWork(scope)); err != nil {
		t.Fatalf("readiness: %v", err)
	}
}

// claimMessage claims the Turn's next message for a new delivery.
func claimMessage(t *testing.T, f *sessiontest.Execution, scope run.TurnScope) db.SessionMessage {
	t.Helper()
	message, err := ClaimMessageFromRun(t.Context(), f.Pool, f.Fence(), turnWork(scope), uuid.NewV7())
	if err != nil || !message.ID.Valid {
		t.Fatalf("claim: %+v %v", message, err)
	}
	return message
}

// finishDelivery records the delivered message's outcome.
func finishDelivery(t *testing.T, f *sessiontest.Execution, scope run.TurnScope, message db.SessionMessage, status, code string) {
	t.Helper()
	if err := CompleteMessageFromRun(t.Context(), f.Pool, f.Fence(), turnWork(scope), pgvalue.MustUUIDValue(message.ID), pgvalue.MustUUIDValue(message.DeliveryID), MessageOutcome{Status: status, Code: code}); err != nil {
		t.Fatalf("message completion: %v", err)
	}
}

func waitForPostgresBlock(t *testing.T, pool *pgxpool.Pool, backendPID int32) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var blocked bool
		if err := pool.QueryRow(t.Context(), `
SELECT cardinality(pg_blocking_pids($1)) > 0`, backendPID).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out waiting for the concurrent transaction to block")
}

// waitForBlockedBy returns the backend that the transaction blocks.
func waitForBlockedBy(t *testing.T, pool *pgxpool.Pool, blockerPID int) int {
	t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for {
		var pid int
		if err := pool.QueryRow(t.Context(), `SELECT coalesce(min(pid),0) FROM pg_stat_activity WHERE datname=current_database() AND $1=ANY(pg_blocking_pids(pid))`, blockerPID).Scan(&pid); err != nil {
			t.Fatal(err)
		}
		if pid != 0 {
			return pid
		}
		select {
		case <-deadline.C:
			t.Fatalf("no operation blocked by transaction %d", blockerPID)
		case <-tick.C:
		}
	}
}
