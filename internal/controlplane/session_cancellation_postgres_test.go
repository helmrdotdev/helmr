package controlplane

import (
	"encoding/json"
	"errors"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/session"
)

func TestSessionCancellationBeforeStartPostgres(t *testing.T) {
	f := newActorStartPostgresFixture(t, 1)
	started, err := f.server.startActor(t.Context(), f.request(0, nil, ""))
	if err != nil {
		t.Fatal(err)
	}
	target := session.Target{EnvironmentID: f.environmentID, SessionID: started.SessionID}
	var queuedIDs []uuid.UUID
	for range 2 {
		receipt, admitErr := session.ApplyAdmission(t.Context(), f.server.tx, session.AdmissionRequest{Target: target, Mode: session.EnqueueOnly, Data: json.RawMessage(`"queued"`)})
		if admitErr != nil {
			t.Fatal(admitErr)
		}
		queuedIDs = append(queuedIDs, receipt.TurnID)
	}

	if _, err = session.ApplyClose(t.Context(), f.server.tx, session.ControlRequest{Target: target, IdempotencyKey: "drain-first"}); err != nil {
		t.Fatal(err)
	}
	request := session.ControlRequest{Target: target, IdempotencyKey: "cancel"}
	first, err := session.ApplyCancel(t.Context(), f.server.tx, request)
	if err != nil {
		t.Fatal(err)
	}
	again, err := session.ApplyCancel(t.Context(), f.server.tx, request)
	if err != nil || first.ID != again.ID {
		t.Fatalf("replay: %+v %v", again, err)
	}
	if _, err = session.ApplyAdmission(t.Context(), f.server.tx, session.AdmissionRequest{Target: target, Mode: session.EnqueueOnly, Data: json.RawMessage(`"rejected"`)}); err == nil {
		t.Fatal("admitted input after cancellation")
	}
	reconciler, err := session.NewReconciler(f.pool)
	if err != nil {
		t.Fatal(err)
	}
	if pending, err := reconciler.ReconcileLifecycle(t.Context(), f.environmentID, started.SessionID); err != nil || pending {
		t.Fatalf("close: pending=%v err=%v", pending, err)
	}
	for _, turnID := range queuedIDs {
		if pending, err := reconciler.ReconcileInput(t.Context(), f.environmentID, started.SessionID, turnID); err != nil || pending {
			t.Fatalf("obsolete input delivery=%v %v", pending, err)
		}
	}
	var status string
	var sessionComputer *uuid.UUID
	var cursor, turns, runs, events int
	err = f.pool.QueryRow(t.Context(), `SELECT s.status,s.computer_id,s.committed_input_sequence,(SELECT count(*) FROM session_turns WHERE session_id=s.id AND status='cancelled' AND run_id IS NULL AND terminal_event_id IS NOT NULL),(SELECT count(*) FROM runs WHERE session_id=s.id),(SELECT count(*) FROM session_events WHERE session_id=s.id AND kind='turn.cancelled') FROM sessions s JOIN computers w ON w.id=s.computer_id WHERE s.id=$1`, started.SessionID).Scan(&status, &sessionComputer, &cursor, &turns, &runs, &events)
	if err != nil || status != "closed" || sessionComputer == nil || cursor != 2 || turns != 2 || events != 2 || runs != 1 {
		t.Fatalf("state=%s sessionComputer=%v cursor=%d turns=%d events=%d runs=%d err=%v", status, sessionComputer, cursor, turns, events, runs, err)
	}
}

func TestSessionCancellationRacingAdmissionPostgres(t *testing.T) {
	f := newActorStartPostgresFixture(t, 1)
	started, err := f.server.startActor(t.Context(), f.request(0, nil, ""))
	if err != nil {
		t.Fatal(err)
	}
	target := session.Target{EnvironmentID: f.environmentID, SessionID: started.SessionID}
	start := make(chan struct{})
	admitted := make(chan error, 1)
	cancelled := make(chan error, 1)
	go func() {
		<-start
		_, err := session.ApplyAdmission(t.Context(), f.server.tx, session.AdmissionRequest{Target: target, Mode: session.EnqueueOnly, Data: json.RawMessage(`"racing"`)})
		admitted <- err
	}()
	go func() {
		<-start
		_, err := session.ApplyCancel(t.Context(), f.server.tx, session.ControlRequest{Target: target, IdempotencyKey: "cancel"})
		cancelled <- err
	}()
	close(start)
	if err := <-cancelled; err != nil {
		t.Fatal(err)
	}
	if err := <-admitted; err != nil {
		var rejected *session.OperationError
		if !errors.As(err, &rejected) || rejected.Code != "session_not_open" {
			t.Fatal(err)
		}
	}
	reconciler, err := session.NewReconciler(f.pool)
	if err != nil {
		t.Fatal(err)
	}
	if pending, err := reconciler.ReconcileLifecycle(t.Context(), f.environmentID, started.SessionID); err != nil || pending {
		t.Fatalf("close=%v %v", pending, err)
	}
	var status string
	var unsettled int
	if err = f.pool.QueryRow(t.Context(), `SELECT status,(SELECT count(*) FROM session_turns t WHERE t.session_id=s.id AND t.status<>'cancelled') FROM sessions s WHERE id=$1`, started.SessionID).Scan(&status, &unsettled); err != nil || status != "closed" || unsettled != 0 {
		t.Fatalf("status=%s unsettled=%d err=%v", status, unsettled, err)
	}
}
