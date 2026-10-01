package session

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/jsoncanon"
	"github.com/helmrdotdev/helmr/internal/run"
	"github.com/helmrdotdev/helmr/internal/session/sessiontest"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestApplyCancelBeforeStart(t *testing.T) {
	f := sessiontest.New(t, 1)
	started, err := Start(t.Context(), f.Pool, nil, startRequest(f, 0, nil))
	if err != nil {
		t.Fatal(err)
	}
	target := Target{EnvironmentID: f.EnvironmentID, SessionID: started.SessionID}
	var queuedIDs []uuid.UUID
	for range 2 {
		receipt, admitErr := ApplyAdmission(t.Context(), f.Pool, AdmissionRequest{Target: target, Mode: EnqueueOnly, Data: json.RawMessage(`"queued"`)})
		if admitErr != nil {
			t.Fatal(admitErr)
		}
		queuedIDs = append(queuedIDs, receipt.TurnID)
	}

	if _, err = ApplyClose(t.Context(), f.Pool, ControlRequest{Target: target, IdempotencyKey: "drain-first"}); err != nil {
		t.Fatal(err)
	}
	request := ControlRequest{Target: target, IdempotencyKey: "cancel"}
	first, err := ApplyCancel(t.Context(), f.Pool, request)
	if err != nil {
		t.Fatal(err)
	}
	again, err := ApplyCancel(t.Context(), f.Pool, request)
	if err != nil || first.ID != again.ID {
		t.Fatalf("replay: %+v %v", again, err)
	}
	if _, err = ApplyAdmission(t.Context(), f.Pool, AdmissionRequest{Target: target, Mode: EnqueueOnly, Data: json.RawMessage(`"rejected"`)}); err == nil {
		t.Fatal("admitted input after cancellation")
	}
	reconciler, err := NewReconciler(f.Pool)
	if err != nil {
		t.Fatal(err)
	}
	if pending, err := reconciler.ReconcileLifecycle(t.Context(), f.EnvironmentID, started.SessionID); err != nil || pending {
		t.Fatalf("close: pending=%v err=%v", pending, err)
	}
	for _, turnID := range queuedIDs {
		if pending, err := reconciler.ReconcileInput(t.Context(), f.EnvironmentID, started.SessionID, turnID); err != nil || pending {
			t.Fatalf("obsolete input delivery=%v %v", pending, err)
		}
	}
	var status string
	var sessionComputer *uuid.UUID
	var cursor, turns, runs, events int
	err = f.Pool.QueryRow(t.Context(), `SELECT s.status,s.computer_id,s.committed_input_sequence,(SELECT count(*) FROM session_turns WHERE session_id=s.id AND status='cancelled' AND run_id IS NULL AND terminal_event_id IS NOT NULL),(SELECT count(*) FROM runs WHERE session_id=s.id),(SELECT count(*) FROM session_events WHERE session_id=s.id AND kind='turn.cancelled') FROM sessions s JOIN computers w ON w.id=s.computer_id WHERE s.id=$1`, started.SessionID).Scan(&status, &sessionComputer, &cursor, &turns, &runs, &events)
	if err != nil || status != "closed" || sessionComputer == nil || cursor != 2 || turns != 2 || events != 2 || runs != 1 {
		t.Fatalf("state=%s sessionComputer=%v cursor=%d turns=%d events=%d runs=%d err=%v", status, sessionComputer, cursor, turns, events, runs, err)
	}
}

func TestApplyCancelRacingAdmission(t *testing.T) {
	f := sessiontest.New(t, 1)
	started, err := Start(t.Context(), f.Pool, nil, startRequest(f, 0, nil))
	if err != nil {
		t.Fatal(err)
	}
	target := Target{EnvironmentID: f.EnvironmentID, SessionID: started.SessionID}
	start := make(chan struct{})
	admitted := make(chan error, 1)
	cancelled := make(chan error, 1)
	go func() {
		<-start
		_, err := ApplyAdmission(t.Context(), f.Pool, AdmissionRequest{Target: target, Mode: EnqueueOnly, Data: json.RawMessage(`"racing"`)})
		admitted <- err
	}()
	go func() {
		<-start
		_, err := ApplyCancel(t.Context(), f.Pool, ControlRequest{Target: target, IdempotencyKey: "cancel"})
		cancelled <- err
	}()
	close(start)
	if err := <-cancelled; err != nil {
		t.Fatal(err)
	}
	if err := <-admitted; err != nil {
		var rejected *OperationError
		if !errors.As(err, &rejected) || rejected.Code != "session_not_open" {
			t.Fatal(err)
		}
	}
	reconciler, err := NewReconciler(f.Pool)
	if err != nil {
		t.Fatal(err)
	}
	if pending, err := reconciler.ReconcileLifecycle(t.Context(), f.EnvironmentID, started.SessionID); err != nil || pending {
		t.Fatalf("close=%v %v", pending, err)
	}
	var status string
	var unsettled int
	if err = f.Pool.QueryRow(t.Context(), `SELECT status,(SELECT count(*) FROM session_turns t WHERE t.session_id=s.id AND t.status<>'cancelled') FROM sessions s WHERE id=$1`, started.SessionID).Scan(&status, &unsettled); err != nil || status != "closed" || unsettled != 0 {
		t.Fatalf("status=%s unsettled=%d err=%v", status, unsettled, err)
	}
}

func TestApplyAdmissionRejectsOversizedCanonicalInputWithoutResidue(
	t *testing.T,
) {
	fixture := sessiontest.New(t, 1)
	started, err := Start(t.Context(), fixture.Pool, nil, startRequest(fixture, 0, nil))
	if err != nil {
		t.Fatal(err)
	}
	data := []byte(`"` + strings.Repeat("x", (1<<20)) + `"`)
	canonical, err := jsoncanon.Transform(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(canonical) <= (1 << 20) {
		t.Fatalf("canonical input size = %d, want over %d", len(canonical), (1 << 20))
	}

	type state struct {
		nextSequence int64
		records      int
		claims       int
		outbox       int
	}
	readState := func() state {
		var value state
		if err := fixture.Pool.QueryRow(t.Context(), `
SELECT sessions.next_input_sequence,
       (SELECT count(*) FROM session_turns WHERE session_id = sessions.id),
       (SELECT count(*) FROM idempotency_claims
         WHERE environment_id = sessions.environment_id
           AND operation = 'enqueue'),
       (SELECT count(*) FROM control_outbox
         WHERE topic = 'input.reconcile'
           AND payload->>'sessionId' = sessions.id::text)
  FROM sessions
 WHERE sessions.id = $1`,
			started.SessionID,
		).Scan(
			&value.nextSequence,
			&value.records,
			&value.claims,
			&value.outbox,
		); err != nil {
			t.Fatal(err)
		}
		return value
	}
	before := readState()
	_, err = ApplyAdmission(t.Context(), fixture.Pool, AdmissionRequest{
		Target: Target{EnvironmentID: fixture.EnvironmentID, SessionID: started.SessionID}, Mode: EnqueueOnly,
		Data:           data,
		IdempotencyKey: "oversized-input",
	})
	var failure *OperationError
	if !errors.As(err, &failure) || failure.Code != "invalid_request" {
		t.Fatalf("append error = %v, want Actor input too large", err)
	}
	after := readState()
	if after != before {
		t.Fatalf("Actor input state changed: before=%+v after=%+v", before, after)
	}
}

// The control graph locks the current Run's graph, or the Session itself when
// there is no current Run.
func TestLockControlGraphLocksTheCurrentRunOrTheSession(t *testing.T) {
	f := sessiontest.New(t, 1)
	started, err := Start(t.Context(), f.Pool, nil, startRequest(f, 0, nil))
	if err != nil {
		t.Fatal(err)
	}
	target := Target{EnvironmentID: f.EnvironmentID, SessionID: started.SessionID}
	held := func(query string, id uuid.UUID) bool {
		t.Helper()
		_, err := f.Pool.Exec(t.Context(), query, id)
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "55P03" {
			return true
		}
		if err != nil {
			t.Fatal(err)
		}
		return false
	}
	const runLocked = `SELECT 1 FROM runs WHERE id=$1 FOR UPDATE NOWAIT`
	const sessionLocked = `SELECT 1 FROM sessions WHERE id=$1 FOR UPDATE NOWAIT`
	lock := func(check func(graph run.OwnedFinalization)) error {
		tx, err := f.Pool.Begin(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(context.Background())
		graph, err := lockControlGraph(t.Context(), tx, target)
		if err == nil {
			check(graph)
		}
		return err
	}
	if err := lock(func(graph run.OwnedFinalization) {
		if reflect.DeepEqual(graph, noControlGraph) || !held(runLocked, started.BootRunID) {
			t.Fatal("current Run graph was not locked")
		}
	}); err != nil {
		t.Fatal(err)
	}
	tx, err := f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	dbtest.MustExec(t, t.Context(), tx, `SET CONSTRAINTS ALL DEFERRED`)
	dbtest.MustExec(t, t.Context(), tx, `UPDATE run_attempts SET entrypoint_entered_at=now(),terminal_session_input_sequence=0,terminal_outcome='succeeded',terminal_reason_code='completed',terminal_at=now() WHERE run_id=$1 AND number=1`, started.BootRunID)
	dbtest.MustExec(t, t.Context(), tx, `UPDATE runs SET status='succeeded',terminal_at=now(),updated_at=now() WHERE id=$1`, started.BootRunID)
	dbtest.MustExec(t, t.Context(), tx, `UPDATE sessions SET current_run_id=NULL,run_generation=run_generation+1,revision=revision+1,updated_at=now() WHERE id=$1`, started.SessionID)
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := lock(func(graph run.OwnedFinalization) {
		if !reflect.DeepEqual(graph, noControlGraph) || !held(sessionLocked, started.SessionID) || held(runLocked, started.BootRunID) {
			t.Fatal("Session without a current Run was not locked alone")
		}
	}); err != nil {
		t.Fatal(err)
	}
	target.SessionID = uuid.NewV7()
	if err := lock(func(run.OwnedFinalization) {}); err == nil {
		t.Fatal("missing Session locked a graph")
	}
}
