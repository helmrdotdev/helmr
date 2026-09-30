package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/session"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func turnCommitRequest(t *testing.T, f *actorExecutionFixture, scope session.TurnScope) workerapi.CommitActorTurnRequest {
	t.Helper()
	f.beginSettlement(t, scope)
	return workerapi.CommitActorTurnRequest{Lease: f.fence(), CorrelationID: uuid.NewV7().String(), TurnID: scope.TurnID.String(), RunGeneration: scope.RunGeneration, Disposition: "completed", Result: json.RawMessage(`{"answer":42}`), TargetInputSequence: 1}
}
func interruptTurn(ctx context.Context, f *actorExecutionFixture, scope session.TurnScope, key string) (session.InterruptReceipt, error) {
	var receipt session.InterruptReceipt
	err := f.server.inTx(ctx, func(w *txWork) error {
		graph, err := lockSessionControlGraph(ctx, w, session.Target{EnvironmentID: scope.EnvironmentID, SessionID: scope.SessionID})
		if err != nil {
			return err
		}
		receipt, err = session.InterruptTurn(ctx, w.tx, scope.EnvironmentID, scope.SessionID, scope.TurnID, key, graph)
		return err
	})
	return receipt, err
}
func assertTurnStopped(t *testing.T, f *actorExecutionFixture, scope session.TurnScope) {
	t.Helper()
	var status, hold, runStatus string
	var active, head uuid.UUID
	var cursor, terminals int64
	if err := f.Pool.QueryRow(t.Context(), `SELECT r.status,s.dispatch_hold_reason,s.active_turn_id,s.committed_input_sequence,w.head_disk_version_id,x.status,(SELECT count(*) FROM session_events e WHERE e.session_id=s.id AND e.kind IN ('turn.completed','turn.failed')) FROM sessions s JOIN session_turns r ON r.id=$2 JOIN computers w ON w.id=s.computer_id JOIN runs x ON x.id=s.current_run_id WHERE s.id=$1`, f.sessionID, scope.TurnID).Scan(&status, &hold, &active, &cursor, &head, &runStatus, &terminals); err != nil {
		t.Fatal(err)
	}
	if status != "running" || hold != "interrupt_requested" || active != scope.TurnID || cursor != 0 || head != f.rootID || runStatus != "running" || terminals != 0 {
		t.Fatalf("stop falsely converged or committed: %s %s %s %d %s %s %d", status, hold, active, cursor, head, runStatus, terminals)
	}
}

func TestSessionTurnStopSettlementPostgres(t *testing.T) {
	t.Run("stop wins", func(t *testing.T) {
		f := newActorExecutionFixture(t, json.RawMessage(`{"sequence":1}`), true)
		scope := f.receiveTurn(t, 1)
		req := turnCommitRequest(t, f, scope)
		parsed, err := parseActorTurnCommitRequest(req)
		if err != nil {
			t.Fatal(err)
		}
		// Hold the actual Session owner transaction across the competing worker call.
		tx, err := f.Pool.Begin(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(context.Background())
		graph, err := lockSessionControlGraph(t.Context(), &txWork{q: db.New(tx), tx: tx}, session.Target{EnvironmentID: f.EnvironmentID, SessionID: f.sessionID})
		if err != nil {
			t.Fatal(err)
		}
		receipt, err := session.InterruptTurn(t.Context(), tx, scope.EnvironmentID, scope.SessionID, scope.TurnID, "stop-1", graph)
		if err != nil || receipt.Status != "accepted" {
			t.Fatalf("interrupt: %+v %v", receipt, err)
		}
		settled := make(chan error, 1)
		go func() { _, err := f.server.commitActorTurn(t.Context(), f.worker, req, parsed); settled <- err }()
		var blockerPID int
		if err := tx.QueryRow(t.Context(), `SELECT pg_backend_pid()`).Scan(&blockerPID); err != nil {
			t.Fatal(err)
		}
		waitForTurnAuthorityBlock(t, f, blockerPID)
		if err := tx.Commit(t.Context()); err != nil {
			t.Fatal(err)
		}
		if err := <-settled; !errors.Is(err, errStaleActorTurnCommit) {
			t.Fatalf("settlement after stop: %v", err)
		}
		assertTurnStopped(t, f, scope)
		var holdID string
		if err := f.Pool.QueryRow(t.Context(), `SELECT data->>'hold_id' FROM session_events WHERE id=$1`, receipt.EventID).Scan(&holdID); err != nil || holdID != receipt.HoldID.String() {
			t.Fatalf("interrupt event hold: %s %v", holdID, err)
		}
		body, _ := json.Marshal(req)
		httpRequest := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
		httpRequest = httpRequest.WithContext(context.WithValue(t.Context(), workerContextKey{}, f.worker))
		response := httptest.NewRecorder()
		f.server.workerCommitActorTurn(response, httpRequest)
		if response.Code != http.StatusConflict {
			t.Fatalf("stopped settlement HTTP %d: %s", response.Code, response.Body.String())
		}
		replay, err := interruptTurn(t.Context(), f, scope, "stop-1")
		if err != nil || replay != receipt {
			t.Fatalf("stop replay: %+v %v", replay, err)
		}
	})
	t.Run("settlement wins", func(t *testing.T) {
		f := newActorExecutionFixture(t, json.RawMessage(`{"sequence":1}`), true)
		scope := f.receiveTurn(t, 1)
		req := turnCommitRequest(t, f, scope)
		parsed, err := parseActorTurnCommitRequest(req)
		if err != nil {
			t.Fatal(err)
		}
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
		go func() { _, err := f.server.commitActorTurn(t.Context(), f.worker, req, parsed); settled <- err }()
		settlementPID := waitForTurnAuthorityBlock(t, f, gatePID)
		type interruptResult struct {
			receipt session.InterruptReceipt
			err     error
		}
		stopped := make(chan interruptResult, 1)
		go func() {
			receipt, err := interruptTurn(t.Context(), f, scope, "late-stop")
			stopped <- interruptResult{receipt, err}
		}()
		waitForTurnAuthorityBlock(t, f, settlementPID)
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
		if err != nil || replay != receipt {
			t.Fatalf("rejected replay: %+v %v", replay, err)
		}
		var status string
		var cursor, terminal int
		var hold bool
		var head uuid.UUID
		if err := f.Pool.QueryRow(t.Context(), `SELECT r.status,s.committed_input_sequence,s.dispatch_hold_id IS NOT NULL,w.head_disk_version_id,(SELECT count(*) FROM session_events WHERE turn_id=r.id AND kind='turn.completed') FROM sessions s JOIN session_turns r ON r.id=$2 JOIN computers w ON w.id=s.computer_id WHERE s.id=$1`, f.sessionID, scope.TurnID).Scan(&status, &cursor, &hold, &head, &terminal); err != nil {
			t.Fatal(err)
		}
		if status != "completed" || cursor != 1 || hold || head != f.rootID || terminal != 1 {
			t.Fatalf("settlement state: %s %d %v %s %d", status, cursor, hold, head, terminal)
		}
	})
}

func outputRequest(f *actorExecutionFixture, scope session.TurnScope) workerapi.WriteTurnOutputRequest {
	return workerapi.WriteTurnOutputRequest{Lease: f.fence(), CorrelationID: uuid.NewV7().String(), TurnID: scope.TurnID.String(), RunGeneration: scope.RunGeneration, Data: json.RawMessage(`{"type":"permission_granted","requestId":"native-1","actionBinding":"command-1"}`), IdempotencyKey: "permission-1"}
}
func outputHTTP(t *testing.T, f *actorExecutionFixture, req workerapi.WriteTurnOutputRequest, w http.ResponseWriter) {
	t.Helper()
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(raw))
	r = r.WithContext(context.WithValue(t.Context(), workerContextKey{}, f.worker))
	f.server.workerWriteTurnOutput(w, r)
}
func appendOutput(t *testing.T, f *actorExecutionFixture, req workerapi.WriteTurnOutputRequest) workerapi.WriteOutputResponse {
	t.Helper()
	w := httptest.NewRecorder()
	outputHTTP(t, f, req, w)
	if w.Code != 200 {
		t.Fatalf("output HTTP %d: %s", w.Code, w.Body.String())
	}
	var response workerapi.WriteOutputResponse
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	return response
}

func TestSessionTurnOutputAuthorityPostgres(t *testing.T) {
	f := newActorExecutionFixture(t, json.RawMessage(`{"sequence":1}`), true)
	scope := f.receiveTurn(t, 1)
	req := outputRequest(f, scope)
	first := appendOutput(t, f, req)
	if first.Completed == nil {
		t.Fatalf("output: %+v", first)
	}
	replay := appendOutput(t, f, req)
	if replay.Completed == nil || replay.Completed.ID != first.Completed.ID {
		t.Fatalf("output replay: %+v", replay)
	}
	wrong := req
	wrong.RunGeneration++
	rejected := appendOutput(t, f, wrong)
	if rejected.Failed == nil || rejected.Failed.Code != "idempotency_conflict" {
		t.Fatalf("producer replay mismatch: %+v", rejected)
	}
	receipt, err := interruptTurn(t.Context(), f, scope, "stop")
	if err != nil || receipt.Status != "accepted" {
		t.Fatalf("interrupt: %+v %v", receipt, err)
	}
	// No local AbortSignal has been delivered. Durable authority alone rejects this write/replay.
	rejected = appendOutput(t, f, req)
	if rejected.Failed == nil || rejected.Failed.Code != "turn_stopping" {
		t.Fatalf("stopped historical replay: %+v", rejected)
	}
	req.IdempotencyKey = "new-after-stop"
	rejected = appendOutput(t, f, req)
	if rejected.Failed == nil || rejected.Failed.Code != "turn_stopping" {
		t.Fatalf("new output after stop: %+v", rejected)
	}
	var outputs, rejections int
	if err := f.Pool.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM session_events WHERE session_id=$1 AND kind='output'),(SELECT count(*) FROM idempotency_claims WHERE receipt->>'code'='turn_stopping')`, f.sessionID).Scan(&outputs, &rejections); err != nil {
		t.Fatal(err)
	}
	if outputs != 1 || rejections != 1 {
		t.Fatalf("output/rejection counts: %d %d", outputs, rejections)
	}
	assertTurnStopped(t, f, scope)
}

type delayedTurnOutputWriter struct {
	http.ResponseWriter
	reached chan struct{}
	release chan struct{}
	drop    bool
}

func (w delayedTurnOutputWriter) Write(b []byte) (int, error) {
	close(w.reached)
	<-w.release
	if w.drop {
		return 0, io.ErrClosedPipe
	}
	return w.ResponseWriter.Write(b)
}
func TestSessionTurnDelayedOutputResponsePostgres(t *testing.T) {
	for _, drop := range []bool{false, true} {
		name := "delayed"
		if drop {
			name = "lost"
		}
		t.Run(name, func(t *testing.T) {
			f := newActorExecutionFixture(t, json.RawMessage(`{"sequence":1}`), true)
			scope := f.receiveTurn(t, 1)
			req := outputRequest(f, scope)
			reached, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
			recorder := httptest.NewRecorder()
			go func() {
				outputHTTP(t, f, req, delayedTurnOutputWriter{ResponseWriter: recorder, reached: reached, release: release, drop: drop})
				close(done)
			}()
			<-reached
			receipt, err := interruptTurn(t.Context(), f, scope, "stop")
			if err != nil || receipt.Status != "accepted" {
				t.Fatalf("stop: %+v %v", receipt, err)
			}
			assertTurnStopped(t, f, scope)
			close(release)
			<-done
			if !drop {
				var r workerapi.WriteOutputResponse
				if err := json.Unmarshal(recorder.Body.Bytes(), &r); err != nil || r.Completed == nil {
					t.Fatalf("delayed receipt: %+v %v", r, err)
				}
			}
			retry := appendOutput(t, f, req)
			if retry.Failed == nil || retry.Failed.Code != "turn_stopping" {
				t.Fatalf("uncertain retry after stop: %+v", retry)
			}
			assertTurnStopped(t, f, scope)
		})
	}
}

func TestSessionTurnIdentityAndRejectedReceiptPostgres(t *testing.T) {
	f := newActorExecutionFixture(t, json.RawMessage(`{"sequence":1}`), true)
	first := f.receiveTurn(t, 1)
	queued, err := f.server.applySessionAdmission(t.Context(), session.AdmissionRequest{Target: session.Target{EnvironmentID: f.EnvironmentID, SessionID: f.sessionID}, Mode: session.EnqueueOnly, Data: json.RawMessage(`{"sequence":2}`)})
	if err != nil {
		t.Fatal(err)
	}
	second := session.TurnScope{EnvironmentID: f.EnvironmentID, SessionID: f.sessionID, TurnID: queued.TurnID, RunID: f.runID, AttemptNumber: first.AttemptNumber, RunGeneration: first.RunGeneration}
	receipt, err := interruptTurn(t.Context(), f, second, "queued-stop")
	if err != nil || receipt.Status != "rejected" || receipt.Code != "turn_not_active" {
		t.Fatalf("queued stop: %+v %v", receipt, err)
	}
	req := outputRequest(f, first)
	if response := appendOutput(t, f, req); response.Completed == nil {
		t.Fatalf("first output: %+v", response)
	}
	f.turn(t, 1)
	second = f.receiveTurn(t, 2)
	if second.TurnID == first.TurnID || second.RunGeneration != first.RunGeneration || second.RunID != first.RunID {
		t.Fatalf("ordinary next input incorrectly changes execution: %+v %+v", first, second)
	}
	replay, err := interruptTurn(t.Context(), f, second, "queued-stop")
	if err != nil || replay != receipt {
		t.Fatalf("rejected operation became accepted after activation: %+v %v", replay, err)
	}
	req.TurnID = second.TurnID.String()
	if response := appendOutput(t, f, req); response.Failed == nil || response.Failed.Code != "idempotency_conflict" {
		t.Fatalf("cross-Turn producer replay: %+v", response)
	}
}

func TestSessionTurnCompletionResultPresencePostgres(t *testing.T) {
	for _, present := range []bool{false, true} {
		name := "absent"
		if present {
			name = "null"
		}
		t.Run(name, func(t *testing.T) {
			f := newActorExecutionFixture(t, json.RawMessage(`{"sequence":1}`), true)
			scope := f.receiveTurn(t, 1)
			req := turnCommitRequest(t, f, scope)
			req.Result = nil
			if present {
				req.Result = json.RawMessage(`null`)
			}
			// Exercise actual JSON encoding/decoding, including omitting an absent result.
			var response workerapi.CommitActorTurnResponse
			f.workerCall(t, f.server.workerCommitActorTurn, req, &response)
			event, err := f.server.db.GetSessionEvent(t.Context(), db.GetSessionEventParams{EnvironmentID: pgvalue.UUID(f.EnvironmentID), SessionID: pgvalue.UUID(f.sessionID), ID: pgvalue.UUID(uuid.MustParse(response.EventID))})
			if err != nil {
				t.Fatal(err)
			}
			var data map[string]json.RawMessage
			if err := json.Unmarshal(event.Data, &data); err != nil {
				t.Fatal(err)
			}
			value, ok := data["result"]
			if ok != present || (present && string(value) != "null") {
				t.Fatalf("result presence lost: %s", event.Data)
			}
			if len(data["computer_disk_version_id"]) != 0 || event.ComputerDiskVersionID.Valid || len(data["error"]) != 0 {
				t.Fatalf("terminal envelope: %s", event.Data)
			}
		})
	}
}

func (f *actorExecutionFixture) beginSettlement(t *testing.T, scope session.TurnScope) {
	t.Helper()
	f.workerCall(t, f.server.workerBeginTurnSettlement, workerapi.TurnExecutionRequest{Lease: f.fence(), CorrelationID: uuid.NewV7().String(), TurnID: scope.TurnID.String(), RunGeneration: scope.RunGeneration}, nil)
}

func TestSessionTurnSettlementRollsBackTerminalEventFailure(t *testing.T) {
	f := newActorExecutionFixture(t, json.RawMessage(`{"sequence":1}`), true)
	scope := f.receiveTurn(t, 1)
	request := turnCommitRequest(t, f, scope)
	parsed, err := parseActorTurnCommitRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := func() string {
		t.Helper()
		var value string
		if err := f.Pool.QueryRow(t.Context(), `SELECT jsonb_build_array(to_jsonb(s),to_jsonb(t),(SELECT jsonb_agg(e ORDER BY e.id) FROM session_events e WHERE e.session_id=s.id))::text FROM sessions s JOIN session_turns t ON t.session_id=s.id WHERE s.id=$1 AND t.id=$2`, f.sessionID, scope.TurnID).Scan(&value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	before := snapshot()
	dbtest.MustExec(t, t.Context(), f.Pool, `CREATE FUNCTION reject_terminal_turn_event() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.kind='turn.completed' THEN RAISE EXCEPTION 'injected terminal event failure'; END IF; RETURN NEW; END $$; CREATE TRIGGER reject_terminal_turn_event BEFORE INSERT ON session_events FOR EACH ROW EXECUTE FUNCTION reject_terminal_turn_event()`)
	if _, err = f.server.commitActorTurn(t.Context(), f.worker, request, parsed); err == nil || !strings.Contains(err.Error(), "injected terminal event failure") {
		t.Fatalf("expected terminal event failure, got %v", err)
	}
	if after := snapshot(); after != before {
		t.Fatalf("failed settlement changed Session, Turn or events\nbefore=%s\nafter=%s", before, after)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `DROP TRIGGER reject_terminal_turn_event ON session_events`)
	first, err := f.server.commitActorTurn(t.Context(), f.worker, request, parsed)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := f.server.commitActorTurn(t.Context(), f.worker, request, parsed)
	if err != nil || replay != first {
		t.Fatalf("retry receipt=%+v replay=%+v err=%v", first, replay, err)
	}
	var complete bool
	if err = f.Pool.QueryRow(t.Context(), `SELECT s.committed_input_sequence=1 AND s.active_turn_id IS NULL AND t.status='completed' AND (SELECT count(*) FROM session_events WHERE turn_id=t.id AND kind='turn.completed')=1 FROM sessions s JOIN session_turns t ON t.session_id=s.id WHERE s.id=$1 AND t.id=$2`, f.sessionID, scope.TurnID).Scan(&complete); err != nil || !complete {
		t.Fatalf("committed settlement=%v err=%v", complete, err)
	}
}

func waitForTurnAuthorityBlock(t *testing.T, f *actorExecutionFixture, blockerPID int) int {
	t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for {
		var pid int
		if err := f.Pool.QueryRow(t.Context(), `SELECT coalesce(min(pid),0) FROM pg_stat_activity WHERE datname=current_database() AND $1=ANY(pg_blocking_pids(pid))`, blockerPID).Scan(&pid); err != nil {
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
