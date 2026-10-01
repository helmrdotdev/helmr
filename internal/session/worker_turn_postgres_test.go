package session

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run"
	"github.com/helmrdotdev/helmr/internal/session/sessiontest"
)

func TestSessionMessageSettlementBarrierPostgres(t *testing.T) {
	f := newExecution(t, json.RawMessage(`{"sequence":1}`), true)
	scope := receiveTurn(t, f, 1)
	request := AdmissionRequest{Target: executionTarget(f), Mode: SendMessageOrEnqueue, Data: json.RawMessage(`null`), IdempotencyKey: "not-ready"}
	first, err := ApplyAdmission(t.Context(), f.Pool, request)
	if err != nil || first.Kind != "messaged" {
		t.Fatalf("pre-handler admission: %+v %v", first, err)
	}
	view, err := GetTurn(t.Context(), db.New(f.Pool), request.Target, scope.TurnID)
	if err != nil || !view.AcceptsMessages {
		t.Fatalf("pre-handler read view: %+v %v", view, err)
	}
	readyMessages(t, f, scope)
	repeated, err := ApplyAdmission(t.Context(), f.Pool, request)
	if err != nil || repeated.ID != first.ID || *repeated.MessageID != *first.MessageID {
		t.Fatalf("accepted receipt changed after readiness: %+v %v", repeated, err)
	}
	second := admitMessage(t, f, "message-2")
	queued, err := ApplyAdmission(t.Context(), f.Pool, AdmissionRequest{Target: request.Target, Mode: EnqueueOnly, Data: json.RawMessage(`{"next":true}`)})
	if err != nil {
		t.Fatal(err)
	}
	if queued.Kind != "enqueued" || first.Kind != "messaged" {
		t.Fatalf("routing: %+v %+v", first, queued)
	}
	delivered := claimMessage(t, f, scope)
	if delivered.ID != pgvalue.UUID(*first.MessageID) {
		t.Fatalf("delivery order: %+v", delivered)
	}
	commit := turnCommit(t, f, scope) // Begins settlement before committing the result.
	request.IdempotencyKey = "settling"
	var operation *OperationError
	if _, err = ApplyAdmission(t.Context(), f.Pool, request); !errors.As(err, &operation) || operation.Code != "turn_settling" {
		t.Fatalf("admission after settlement cutoff: %v", err)
	}
	var secondStatus string
	if err = f.Pool.QueryRow(t.Context(), `SELECT status FROM session_messages WHERE id=$1`, *second.MessageID).Scan(&secondStatus); err != nil || secondStatus != "rejected" {
		t.Fatalf("queued callback at barrier: %s %v", secondStatus, err)
	}
	root := turnOutput(scope)
	root.IdempotencyKey = "root-after-barrier"
	if _, code := appendTurnOutput(t, f, root); code != "turn_unsettled" {
		t.Fatalf("root output after barrier: %s", code)
	}
	cleanup := root
	cleanup.IdempotencyKey = "admitted-callback-cleanup"
	cleanup.MessageDeliveryID = pgvalue.MustUUIDValue(delivered.DeliveryID)
	if event, code := appendTurnOutput(t, f, cleanup); code != "" || !event.Valid {
		t.Fatalf("admitted cleanup: %v %s", event, code)
	}
	if _, err = CommitTurnFromRun(t.Context(), f.Pool, f.Fence(), commit); !errors.Is(err, ErrStaleTurnCommit) {
		t.Fatalf("settled with active delivery: %v", err)
	}
	finishDelivery(t, f, scope, delivered, "handled", "")
	if _, code := appendTurnOutput(t, f, cleanup); code != "stale_execution" {
		t.Fatalf("historical cleanup receipt admitted after callback lifetime: %s", code)
	}
	if _, err = CommitTurnFromRun(t.Context(), f.Pool, f.Fence(), commit); err != nil {
		t.Fatal(err)
	}
	next := receiveTurn(t, f, 2)
	if next.TurnID != queued.TurnID {
		t.Fatalf("queued identity changed: %+v %+v", next, queued)
	}
}

func TestSessionMessageWithoutHandlerRejectedAtSettlementPostgres(t *testing.T) {
	f := newExecution(t, json.RawMessage(`{"sequence":1}`), true)
	scope := receiveTurn(t, f, 1)
	accepted := admitMessage(t, f, "no-handler")
	turnCommit(t, f, scope)
	var status string
	var outcome []byte
	if err := f.Pool.QueryRow(t.Context(), `SELECT status,outcome FROM session_messages WHERE id=$1`, *accepted.MessageID).Scan(&status, &outcome); err != nil {
		t.Fatal(err)
	}
	if status != "rejected" || !bytes.Contains(outcome, []byte("turn_settling")) {
		t.Fatalf("unhandled input lost: %s %s", status, outcome)
	}
}

func assertTurnStopped(t *testing.T, f *sessiontest.Execution, scope run.TurnScope) {
	t.Helper()
	var status, hold, runStatus string
	var active, head uuid.UUID
	var cursor, terminals int64
	if err := f.Pool.QueryRow(t.Context(), `SELECT r.status,s.dispatch_hold_reason,s.active_turn_id,s.committed_input_sequence,w.head_disk_version_id,x.status,(SELECT count(*) FROM session_events e WHERE e.session_id=s.id AND e.kind IN ('turn.completed','turn.failed')) FROM sessions s JOIN session_turns r ON r.id=$2 JOIN computers w ON w.id=s.computer_id JOIN runs x ON x.id=s.current_run_id WHERE s.id=$1`, f.SessionID, scope.TurnID).Scan(&status, &hold, &active, &cursor, &head, &runStatus, &terminals); err != nil {
		t.Fatal(err)
	}
	if status != "running" || hold != "interrupt_requested" || active != scope.TurnID || cursor != 0 || head != f.RootID || runStatus != "running" || terminals != 0 {
		t.Fatalf("stop falsely converged or committed: %s %s %s %d %s %s %d", status, hold, active, cursor, head, runStatus, terminals)
	}
}

func TestSessionTurnStopSettlementPostgres(t *testing.T) {
	t.Run("stop wins", func(t *testing.T) {
		f := newExecution(t, json.RawMessage(`{"sequence":1}`), true)
		scope := receiveTurn(t, f, 1)
		commit := turnCommit(t, f, scope)
		// Hold the Session owner's interruption, after it locked the Session,
		// across the competing worker call.
		gate, err := f.Pool.Begin(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		defer gate.Rollback(context.Background())
		var gatePID int
		if err := gate.QueryRow(t.Context(), `SELECT pg_backend_pid(),pg_advisory_xact_lock(918274)`).Scan(&gatePID, new(any)); err != nil {
			t.Fatal(err)
		}
		dbtest.MustExec(t, t.Context(), f.Pool, `CREATE FUNCTION block_interrupt_event() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.kind='turn.interrupt_requested' THEN PERFORM pg_advisory_xact_lock(918274); END IF; RETURN NEW; END $$; CREATE TRIGGER block_interrupt_event BEFORE INSERT ON session_events FOR EACH ROW EXECUTE FUNCTION block_interrupt_event()`)
		type interruptResult struct {
			receipt ControlReceipt
			err     error
		}
		stopped := make(chan interruptResult, 1)
		go func() {
			receipt, err := interruptTurn(t.Context(), f, scope, "stop-1")
			stopped <- interruptResult{receipt, err}
		}()
		interruptPID := waitForBlockedBy(t, f.Pool, gatePID)
		settled := make(chan error, 1)
		go func() { _, err := CommitTurnFromRun(t.Context(), f.Pool, f.Fence(), commit); settled <- err }()
		waitForBlockedBy(t, f.Pool, interruptPID)
		if err := gate.Commit(t.Context()); err != nil {
			t.Fatal(err)
		}
		out := <-stopped
		receipt := out.receipt
		if out.err != nil || receipt.Status != "accepted" || receipt.HoldID == nil {
			t.Fatalf("interrupt: %+v %v", receipt, out.err)
		}
		if err := <-settled; !errors.Is(err, ErrStaleTurnCommit) {
			t.Fatalf("settlement after stop: %v", err)
		}
		assertTurnStopped(t, f, scope)
		var holdID string
		if err := f.Pool.QueryRow(t.Context(), `SELECT data->>'hold_id' FROM session_events WHERE turn_id=$1 AND kind='turn.interrupt_requested'`, scope.TurnID).Scan(&holdID); err != nil || holdID != receipt.HoldID.String() {
			t.Fatalf("interrupt event hold: %s %v", holdID, err)
		}
		if _, err := CommitTurnFromRun(t.Context(), f.Pool, f.Fence(), commit); !errors.Is(err, ErrStaleTurnCommit) {
			t.Fatalf("stopped settlement: %v", err)
		}
		replay, err := interruptTurn(t.Context(), f, scope, "stop-1")
		if err != nil || !reflect.DeepEqual(replay, receipt) {
			t.Fatalf("stop replay: %+v %v", replay, err)
		}
	})
	t.Run("settlement wins", func(t *testing.T) {
		f := newExecution(t, json.RawMessage(`{"sequence":1}`), true)
		scope := receiveTurn(t, f, 1)
		commit := turnCommit(t, f, scope)
		gate, err := f.Pool.Begin(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		defer gate.Rollback(context.Background())
		var gatePID int
		if err := gate.QueryRow(t.Context(), `SELECT pg_backend_pid(),pg_advisory_xact_lock(918273)`).Scan(&gatePID, new(any)); err != nil {
			t.Fatal(err)
		}
		dbtest.MustExec(t, t.Context(), f.Pool, `CREATE FUNCTION block_terminal_turn_event() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.kind='turn.completed' THEN PERFORM pg_advisory_xact_lock(918273); END IF; RETURN NEW; END $$; CREATE TRIGGER block_terminal_turn_event BEFORE INSERT ON session_events FOR EACH ROW EXECUTE FUNCTION block_terminal_turn_event()`)
		settled := make(chan error, 1)
		go func() { _, err := CommitTurnFromRun(t.Context(), f.Pool, f.Fence(), commit); settled <- err }()
		settlementPID := waitForBlockedBy(t, f.Pool, gatePID)
		type interruptResult struct {
			receipt ControlReceipt
			err     error
		}
		stopped := make(chan interruptResult, 1)
		go func() {
			receipt, err := interruptTurn(t.Context(), f, scope, "late-stop")
			stopped <- interruptResult{receipt, err}
		}()
		waitForBlockedBy(t, f.Pool, settlementPID)
		if err := gate.Commit(t.Context()); err != nil {
			t.Fatal(err)
		}
		if err := <-settled; err != nil {
			t.Fatal(err)
		}
		out := <-stopped
		receipt, err := out.receipt, out.err
		if err != nil || receipt.Status != "rejected" || receipt.Code != "turn_not_active" {
			t.Fatalf("late stop: %+v %v", receipt, err)
		}
		replay, err := interruptTurn(t.Context(), f, scope, "late-stop")
		if err != nil || !reflect.DeepEqual(replay, receipt) {
			t.Fatalf("rejected replay: %+v %v", replay, err)
		}
		var status string
		var cursor, terminal int
		var hold bool
		var head uuid.UUID
		if err := f.Pool.QueryRow(t.Context(), `SELECT r.status,s.committed_input_sequence,s.dispatch_hold_id IS NOT NULL,w.head_disk_version_id,(SELECT count(*) FROM session_events WHERE turn_id=r.id AND kind='turn.completed') FROM sessions s JOIN session_turns r ON r.id=$2 JOIN computers w ON w.id=s.computer_id WHERE s.id=$1`, f.SessionID, scope.TurnID).Scan(&status, &cursor, &hold, &head, &terminal); err != nil {
			t.Fatal(err)
		}
		if status != "completed" || cursor != 1 || hold || head != f.RootID || terminal != 1 {
			t.Fatalf("settlement state: %s %d %v %s %d", status, cursor, hold, head, terminal)
		}
	})
}

func TestSessionTurnOutputAuthorityPostgres(t *testing.T) {
	f := newExecution(t, json.RawMessage(`{"sequence":1}`), true)
	scope := receiveTurn(t, f, 1)
	output := turnOutput(scope)
	first, code := appendTurnOutput(t, f, output)
	if code != "" || !first.Valid {
		t.Fatalf("output: %v %s", first, code)
	}
	replay, code := appendTurnOutput(t, f, output)
	if code != "" || replay != first {
		t.Fatalf("output replay: %v %s", replay, code)
	}
	wrong := output
	wrong.RunGeneration++
	if _, code = appendTurnOutput(t, f, wrong); code != "idempotency_conflict" {
		t.Fatalf("producer replay mismatch: %s", code)
	}
	receipt, err := interruptTurn(t.Context(), f, scope, "stop")
	if err != nil || receipt.Status != "accepted" {
		t.Fatalf("interrupt: %+v %v", receipt, err)
	}
	// No local AbortSignal has been delivered. Durable authority alone rejects this write/replay.
	if _, code = appendTurnOutput(t, f, output); code != "turn_stopping" {
		t.Fatalf("stopped historical replay: %s", code)
	}
	output.IdempotencyKey = "new-after-stop"
	if _, code = appendTurnOutput(t, f, output); code != "turn_stopping" {
		t.Fatalf("new output after stop: %s", code)
	}
	var outputs, rejections int
	if err := f.Pool.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM session_events WHERE session_id=$1 AND kind='output'),(SELECT count(*) FROM idempotency_claims WHERE receipt->>'code'='turn_stopping')`, f.SessionID).Scan(&outputs, &rejections); err != nil {
		t.Fatal(err)
	}
	if outputs != 1 || rejections != 1 {
		t.Fatalf("output/rejection counts: %d %d", outputs, rejections)
	}
	assertTurnStopped(t, f, scope)
}

func TestSessionTurnIdentityAndRejectedReceiptPostgres(t *testing.T) {
	f := newExecution(t, json.RawMessage(`{"sequence":1}`), true)
	first := receiveTurn(t, f, 1)
	queued, err := ApplyAdmission(t.Context(), f.Pool, AdmissionRequest{Target: executionTarget(f), Mode: EnqueueOnly, Data: json.RawMessage(`{"sequence":2}`)})
	if err != nil {
		t.Fatal(err)
	}
	second := run.TurnScope{EnvironmentID: f.EnvironmentID, SessionID: f.SessionID, TurnID: queued.TurnID, RunID: f.RunID, AttemptNumber: first.AttemptNumber, RunGeneration: first.RunGeneration}
	receipt, err := interruptTurn(t.Context(), f, second, "queued-stop")
	if err != nil || receipt.Status != "rejected" || receipt.Code != "turn_not_active" {
		t.Fatalf("queued stop: %+v %v", receipt, err)
	}
	output := turnOutput(first)
	if event, code := appendTurnOutput(t, f, output); code != "" || !event.Valid {
		t.Fatalf("first output: %v %s", event, code)
	}
	commitTurn(t, f, 1)
	second = receiveTurn(t, f, 2)
	if second.TurnID == first.TurnID || second.RunGeneration != first.RunGeneration || second.RunID != first.RunID {
		t.Fatalf("ordinary next input incorrectly changes execution: %+v %+v", first, second)
	}
	replay, err := interruptTurn(t.Context(), f, second, "queued-stop")
	if err != nil || !reflect.DeepEqual(replay, receipt) {
		t.Fatalf("rejected operation became accepted after activation: %+v %v", replay, err)
	}
	output.TurnID = second.TurnID
	if _, code := appendTurnOutput(t, f, output); code != "idempotency_conflict" {
		t.Fatalf("cross-Turn producer replay: %s", code)
	}
}

func TestSessionTurnSettlementRollsBackTerminalEventFailure(t *testing.T) {
	f := newExecution(t, json.RawMessage(`{"sequence":1}`), true)
	scope := receiveTurn(t, f, 1)
	commit := turnCommit(t, f, scope)
	snapshot := func() string {
		t.Helper()
		var value string
		if err := f.Pool.QueryRow(t.Context(), `SELECT jsonb_build_array(to_jsonb(s),to_jsonb(t),(SELECT jsonb_agg(e ORDER BY e.id) FROM session_events e WHERE e.session_id=s.id))::text FROM sessions s JOIN session_turns t ON t.session_id=s.id WHERE s.id=$1 AND t.id=$2`, f.SessionID, scope.TurnID).Scan(&value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	before := snapshot()
	dbtest.MustExec(t, t.Context(), f.Pool, `CREATE FUNCTION reject_terminal_turn_event() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.kind='turn.completed' THEN RAISE EXCEPTION 'injected terminal event failure'; END IF; RETURN NEW; END $$; CREATE TRIGGER reject_terminal_turn_event BEFORE INSERT ON session_events FOR EACH ROW EXECUTE FUNCTION reject_terminal_turn_event()`)
	if _, err := CommitTurnFromRun(t.Context(), f.Pool, f.Fence(), commit); err == nil || !strings.Contains(err.Error(), "injected terminal event failure") {
		t.Fatalf("expected terminal event failure, got %v", err)
	}
	if after := snapshot(); after != before {
		t.Fatalf("failed settlement changed Session, Turn or events\nbefore=%s\nafter=%s", before, after)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `DROP TRIGGER reject_terminal_turn_event ON session_events`)
	first, err := CommitTurnFromRun(t.Context(), f.Pool, f.Fence(), commit)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := CommitTurnFromRun(t.Context(), f.Pool, f.Fence(), commit)
	if err != nil || replay != first {
		t.Fatalf("retry event=%v replay=%v err=%v", first, replay, err)
	}
	var complete bool
	if err = f.Pool.QueryRow(t.Context(), `SELECT s.committed_input_sequence=1 AND s.active_turn_id IS NULL AND t.status='completed' AND (SELECT count(*) FROM session_events WHERE turn_id=t.id AND kind='turn.completed')=1 FROM sessions s JOIN session_turns t ON t.session_id=s.id WHERE s.id=$1 AND t.id=$2`, f.SessionID, scope.TurnID).Scan(&complete); err != nil || !complete {
		t.Fatalf("committed settlement=%v err=%v", complete, err)
	}
}
