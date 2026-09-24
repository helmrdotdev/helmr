package controlplane

import (
	"errors"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/session"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func lostSessionComputer(t *testing.T) (*actorCheckpointFixture, session.RecoverRequest) {
	t.Helper()
	f := newActorCheckpointFixture(t)
	f.turn(t, 1)
	reconciler, registration := actorTokenWait(t, f, session.TurnScope{})
	registration.ActorSpeculativeInputSequence.Int64 = 1
	registration.TurnID.Valid = false
	registration.RunGeneration.Valid = false
	if _, err := reconciler.RegisterWait(t.Context(), registration); err != nil {
		t.Fatal(err)
	}
	lease, err := parseRunLeaseFence(f.fence())
	if err != nil {
		t.Fatal(err)
	}
	wait, err := f.server.requestWorkerRunWaitCheckpoint(t.Context(), f.worker, f.fence(), lease, registration.WaitID)
	if err != nil {
		t.Fatal(err)
	}
	f.workerCall(t, f.server.workerMarkCheckpointFailed, workerapi.CheckpointFailedRequest{Lease: f.fence(), RunWaitID: registration.WaitID.String(), CheckpointID: pgvalue.UUIDString(wait.SuspendCheckpointID), RequestVersion: wait.CheckpointRequestVersion, Error: "capture failed"}, nil)
	f.reportRuntimeClosed(t)
	var hold uuid.UUID
	if err := f.Pool.QueryRow(t.Context(), `SELECT dispatch_hold_id FROM sessions WHERE id=$1`, f.sessionID).Scan(&hold); err != nil {
		t.Fatal(err)
	}
	return f, session.RecoverRequest{ResumeRequest: session.ResumeRequest{ControlRequest: session.ControlRequest{Target: session.Target{EnvironmentID: f.EnvironmentID, SessionID: f.sessionID}, IdempotencyKey: "accept-saved-computer"}, HoldID: hold}, WorkspaceVersionID: f.rootID, ReconciliationRef: "accept latest saved disk; local edits lost"}
}

func TestSessionComputerRecoveryAtomicReplayPostgres(t *testing.T) {
	f, request := lostSessionComputer(t)
	dbtest.MustExec(t, t.Context(), f.Pool, `CREATE FUNCTION reject_recovered_session() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.kind='session.recovered' THEN RAISE EXCEPTION 'injected recovery event failure'; END IF; RETURN NEW; END $$; CREATE TRIGGER reject_recovered_session BEFORE INSERT ON session_events FOR EACH ROW EXECUTE FUNCTION reject_recovered_session()`)
	if _, err := f.server.applySessionRecovery(t.Context(), request); err == nil {
		t.Fatal("event failure ignored")
	}
	assertUnrecoveredSessionComputer(t, f, request.HoldID)
	dbtest.MustExec(t, t.Context(), f.Pool, `DROP TRIGGER reject_recovered_session ON session_events`)
	type result struct {
		receipt session.ControlReceipt
		err     error
	}
	done := make(chan result, 2)
	for range 2 {
		go func() { r, err := f.server.applySessionRecovery(t.Context(), request); done <- result{r, err} }()
	}
	a, b := <-done, <-done
	if a.err != nil || b.err != nil || a.receipt.ID != b.receipt.ID || a.receipt.HoldID == nil || b.receipt.HoldID == nil || *a.receipt.HoldID != *b.receipt.HoldID {
		t.Fatalf("concurrent recovery=%+v / %+v", a, b)
	}
	var computer, dirty, reason string
	var head uuid.UUID
	var cursor, events, completed int
	var current *uuid.UUID
	if err := f.Pool.QueryRow(t.Context(), `SELECT w.status,w.dirty_state,w.head_version_id,s.dispatch_hold_reason,s.current_run_id,s.committed_input_sequence,
 (SELECT count(*) FROM session_events WHERE session_id=s.id AND kind='session.recovered'),
 (SELECT count(*) FROM session_turns WHERE session_id=s.id AND status='completed')
 FROM sessions s JOIN computers w ON w.id=s.workspace_id WHERE s.id=$1`, f.sessionID).Scan(&computer, &dirty, &head, &reason, &current, &cursor, &events, &completed); err != nil {
		t.Fatal(err)
	}
	if computer != "active" || dirty != "clean" || head != f.rootID || reason != "recovered" || current != nil || cursor != 1 || events != 1 || completed != 1 {
		t.Fatalf("recovery=%s/%s head=%s hold=%s run=%v cursor=%d events=%d completed=%d", computer, dirty, head, reason, current, cursor, events, completed)
	}
	var reconciled bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT (data->>'computer_reconciled')::boolean FROM session_events WHERE session_id=$1 AND kind='session.recovered'`, f.sessionID).Scan(&reconciled); err != nil || !reconciled {
		t.Fatalf("lost Computer audit=%t %v", reconciled, err)
	}
	conflict := request
	conflict.ReconciliationRef = "changed"
	if _, err := f.server.applySessionRecovery(t.Context(), conflict); err == nil {
		t.Fatal("conflicting replay accepted")
	}
}

func assertUnrecoveredSessionComputer(t *testing.T, f *actorCheckpointFixture, hold uuid.UUID) {
	t.Helper()
	var status, dirty string
	var actual uuid.UUID
	if err := f.Pool.QueryRow(t.Context(), `SELECT w.status,w.dirty_state,s.dispatch_hold_id FROM sessions s JOIN computers w ON w.id=s.workspace_id WHERE s.id=$1`, f.sessionID).Scan(&status, &dirty, &actual); err != nil {
		t.Fatal(err)
	}
	if status != "recovery_required" || dirty != "dirty_state_lost" || actual != hold {
		t.Fatalf("partial recovery=%s/%s hold=%s", status, dirty, actual)
	}
}

func TestSessionComputerRecoveryRejectsStaleSelectionPostgres(t *testing.T) {
	f, request := lostSessionComputer(t)
	for _, kind := range []string{"hold", "version"} {
		bad := request
		bad.IdempotencyKey = kind
		want := "not_settled"
		if kind == "hold" {
			bad.HoldID = uuid.NewV7()
			want = "stale_hold"
		} else {
			bad.WorkspaceVersionID = uuid.NewV7()
		}
		_, err := f.server.applySessionRecovery(t.Context(), bad)
		var op *session.OperationError
		if !errors.As(err, &op) || op.Code != want {
			t.Fatalf("%s error=%v", kind, err)
		}
		assertUnrecoveredSessionComputer(t, f, request.HoldID)
	}
}
