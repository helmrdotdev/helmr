package controlplane

import (
	"encoding/json"
	"errors"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/session"
	"github.com/jackc/pgx/v5/pgtype"
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
		receipt, admitErr := f.server.applySessionAdmission(t.Context(), session.AdmissionRequest{Target: target, Mode: session.EnqueueOnly, Data: json.RawMessage(`"queued"`)})
		if admitErr != nil {
			t.Fatal(admitErr)
		}
		queuedIDs = append(queuedIDs, receipt.TurnID)
	}

	if _, err = f.server.applySessionClose(t.Context(), session.ControlRequest{Target: target, IdempotencyKey: "drain-first"}); err != nil {
		t.Fatal(err)
	}
	request := session.ControlRequest{Target: target, IdempotencyKey: "cancel"}
	first, err := f.server.applySessionCancel(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	again, err := f.server.applySessionCancel(t.Context(), request)
	if err != nil || first.ID != again.ID {
		t.Fatalf("replay: %+v %v", again, err)
	}
	if _, err = f.server.applySessionAdmission(t.Context(), session.AdmissionRequest{Target: target, Mode: session.EnqueueOnly, Data: json.RawMessage(`"rejected"`)}); err == nil {
		t.Fatal("admitted input after cancellation")
	}
	reconciler, err := session.NewReconciler(f.pool)
	if err != nil {
		t.Fatal(err)
	}
	if pending, err := reconciler.ReconcileClose(t.Context(), f.environmentID, started.SessionID); err != nil || pending {
		t.Fatalf("close: pending=%v err=%v", pending, err)
	}
	for _, turnID := range queuedIDs {
		if pending, err := reconciler.ReconcileInput(t.Context(), f.environmentID, started.SessionID, turnID); err != nil || pending {
			t.Fatalf("obsolete input delivery=%v %v", pending, err)
		}
	}
	var status string
	var owner *uuid.UUID
	var cursor, turns, runs, events int
	err = f.pool.QueryRow(t.Context(), `SELECT s.status,w.owner_session_id,s.committed_input_sequence,(SELECT count(*) FROM session_turns WHERE session_id=s.id AND status='cancelled' AND run_id IS NULL AND terminal_event_id IS NOT NULL),(SELECT count(*) FROM runs WHERE session_id=s.id),(SELECT count(*) FROM session_events WHERE session_id=s.id AND kind='turn.cancelled') FROM sessions s JOIN workspaces w ON w.id=s.workspace_id WHERE s.id=$1`, started.SessionID).Scan(&status, &owner, &cursor, &turns, &runs, &events)
	if err != nil || status != "closed" || owner != nil || cursor != 2 || turns != 2 || events != 2 || runs != 1 {
		t.Fatalf("state=%s owner=%v cursor=%d turns=%d events=%d runs=%d err=%v", status, owner, cursor, turns, events, runs, err)
	}
}

func TestSessionCancellationWaitsForPhysicalStopPostgres(t *testing.T) {
	f := newActorCheckpointFixture(t)
	scope := f.receiveTurn(t, 1)
	message := admitMessage(t, f, "unclaimed-before-cancel")
	target := session.Target{EnvironmentID: f.EnvironmentID, SessionID: f.sessionID}
	for range 2 {
		if _, err := f.server.applySessionAdmission(t.Context(), session.AdmissionRequest{Target: target, Mode: session.EnqueueOnly, Data: json.RawMessage(`"queued"`)}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.server.applySessionCancel(t.Context(), session.ControlRequest{Target: target, IdempotencyKey: "cancel"}); err != nil {
		t.Fatal(err)
	}
	var messageStatus string
	if err := f.Pool.QueryRow(t.Context(), `SELECT status FROM session_messages WHERE id=$1`, message.MessageID).Scan(&messageStatus); err != nil || messageStatus != "rejected" {
		t.Fatalf("queued message=%s err=%v", messageStatus, err)
	}
	reconciler, err := session.NewReconciler(f.Pool)
	if err != nil {
		t.Fatal(err)
	}
	if pending, err := reconciler.ReconcileClose(t.Context(), f.EnvironmentID, f.sessionID); err != nil || !pending {
		t.Fatalf("active close: %v %v", pending, err)
	}
	var hold uuid.UUID
	var cursor int
	if err = f.Pool.QueryRow(t.Context(), `SELECT dispatch_hold_id,committed_input_sequence FROM sessions WHERE id=$1`, f.sessionID).Scan(&hold, &cursor); err != nil || cursor != 0 {
		t.Fatalf("cursor advanced before active settlement: %d %v", cursor, err)
	}
	if _, err = f.server.applySessionResume(t.Context(), session.ResumeRequest{ControlRequest: session.ControlRequest{Target: target}, HoldID: hold}); err == nil {
		t.Fatal("cancelled Session resumed")
	}
	req := interruptedCompletionRequest(t, f, hold, &scope.TurnID)
	parsed, err := parseActorCompletionRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.server.completeActor(t.Context(), f.worker, req, parsed); err != nil {
		t.Fatal(err)
	}
	if pending, err := reconciler.ReconcileClose(t.Context(), f.EnvironmentID, f.sessionID); err != nil || !pending {
		t.Fatalf("closed before physical cleanup: %v %v", pending, err)
	}
	f.reportRuntimeClosed(t)
	if pending, err := reconciler.ReconcileClose(t.Context(), f.EnvironmentID, f.sessionID); err != nil || pending {
		t.Fatalf("settled close: %v %v", pending, err)
	}
	var status string
	var owner *uuid.UUID
	if err = f.Pool.QueryRow(t.Context(), `SELECT s.status,s.committed_input_sequence,w.owner_session_id FROM sessions s JOIN workspaces w ON w.id=s.workspace_id WHERE s.id=$1`, f.sessionID).Scan(&status, &cursor, &owner); err != nil || status != "closed" || cursor != 3 || owner != nil {
		t.Fatalf("state=%s cursor=%d owner=%v err=%v", status, cursor, owner, err)
	}
}

func TestSessionCancellationParkedRecoveryPostgres(t *testing.T) {
	f := newActorCheckpointFixture(t)
	capture := f.capture(t, "retained head")
	f.turn(t, 1, capture, true)
	target := session.Target{EnvironmentID: f.EnvironmentID, SessionID: f.sessionID}
	if _, err := f.server.applySessionAdmission(t.Context(), session.AdmissionRequest{Target: target, Mode: session.EnqueueOnly, Data: json.RawMessage(`"active"`)}); err != nil {
		t.Fatal(err)
	}
	scope := f.receiveTurn(t, 2)
	reconciler, registration := actorTokenWait(t, f, scope)
	registration.ActorSpeculativeInputSequence = pgtype.Int8{Int64: 2, Valid: true}
	if _, err := reconciler.RegisterWait(t.Context(), registration); err != nil {
		t.Fatal(err)
	}
	f.suspendWait(t, registration.WaitID, capture)
	if _, err := f.server.applySessionAdmission(t.Context(), session.AdmissionRequest{Target: target, Mode: session.EnqueueOnly, Data: json.RawMessage(`"cancelled"`)}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.server.applySessionCancel(t.Context(), session.ControlRequest{Target: target, IdempotencyKey: "cancel"}); err != nil {
		t.Fatal(err)
	}
	closeReconciler, err := session.NewReconciler(f.Pool)
	if err != nil {
		t.Fatal(err)
	}
	if waiting, err := closeReconciler.ReconcileClose(t.Context(), f.EnvironmentID, f.sessionID); err != nil || !waiting {
		t.Fatalf("parked close=%v %v", waiting, err)
	}
	var hold, head uuid.UUID
	if err = f.Pool.QueryRow(t.Context(), `SELECT s.dispatch_hold_id,w.head_version_id FROM sessions s JOIN workspaces w ON w.id=s.workspace_id WHERE s.id=$1`, f.sessionID).Scan(&hold, &head); err != nil {
		t.Fatal(err)
	}
	if _, err = f.server.applySessionRecovery(t.Context(), session.RecoverRequest{ResumeRequest: session.ResumeRequest{ControlRequest: session.ControlRequest{Target: target, IdempotencyKey: "recover"}, HoldID: hold}, TurnID: &scope.TurnID, WorkspaceVersionID: head, ReconciliationRef: "test:parked-cleanup", Disposition: "interrupted"}); err != nil {
		t.Fatal(err)
	}
	if waiting, err := closeReconciler.ReconcileClose(t.Context(), f.EnvironmentID, f.sessionID); err != nil || waiting {
		t.Fatalf("recovered close=%v %v", waiting, err)
	}
	var status string
	var cursor int
	if err = f.Pool.QueryRow(t.Context(), `SELECT status,committed_input_sequence FROM sessions WHERE id=$1`, f.sessionID).Scan(&status, &cursor); err != nil || status != "closed" || cursor != 3 {
		t.Fatalf("status=%s cursor=%d err=%v", status, cursor, err)
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
		_, err := f.server.applySessionAdmission(t.Context(), session.AdmissionRequest{Target: target, Mode: session.EnqueueOnly, Data: json.RawMessage(`"racing"`)})
		admitted <- err
	}()
	go func() {
		<-start
		_, err := f.server.applySessionCancel(t.Context(), session.ControlRequest{Target: target, IdempotencyKey: "cancel"})
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
	if pending, err := reconciler.ReconcileClose(t.Context(), f.environmentID, started.SessionID); err != nil || pending {
		t.Fatalf("close=%v %v", pending, err)
	}
	var status string
	var unsettled int
	if err = f.pool.QueryRow(t.Context(), `SELECT status,(SELECT count(*) FROM session_turns t WHERE t.session_id=s.id AND t.status<>'cancelled') FROM sessions s WHERE id=$1`, started.SessionID).Scan(&status, &unsettled); err != nil || status != "closed" || unsettled != 0 {
		t.Fatalf("status=%s unsettled=%d err=%v", status, unsettled, err)
	}
}
