package controlplane

import (
	"encoding/json"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run"
	"github.com/helmrdotdev/helmr/internal/session"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

// Exercise actual checkpoint failure and physical reclamation separately.
func failSessionCheckpoint(t *testing.T, f *actorCheckpointFixture, scope session.TurnScope, cursor int64) {
	t.Helper()
	r, registration := actorTokenWait(t, f, scope)
	registration.ActorSpeculativeInputSequence.Int64 = cursor
	if scope.TurnID == uuid.Nil() {
		registration.TurnID.Valid = false
		registration.RunGeneration.Valid = false
	}
	if _, err := r.RegisterWait(t.Context(), registration); err != nil {
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
}

func TestAutomaticSessionLossPreservesQueue(t *testing.T) {
	for _, active := range []bool{false, true} {
		t.Run(map[bool]string{false: "idle", true: "active"}[active], func(t *testing.T) {
			f := newActorCheckpointFixture(t)
			f.turn(t, 1)
			target := session.Target{EnvironmentID: f.EnvironmentID, SessionID: f.sessionID}
			var scope session.TurnScope
			for i := 2; i <= 3; i++ {
				if _, err := f.server.applySessionAdmission(t.Context(), session.AdmissionRequest{Target: target, Mode: session.EnqueueOnly, Data: json.RawMessage(`{"work":true}`)}); err != nil {
					t.Fatal(err)
				}
			}
			cursor := int64(1)
			if active {
				scope = f.receiveTurn(t, 2)
				cursor = 2
			}
			var queuedBefore string
			query := `SELECT jsonb_agg(jsonb_build_array(id,sequence,data) ORDER BY sequence)::text FROM session_turns WHERE session_id=$1 AND status='queued'`
			if err := f.Pool.QueryRow(t.Context(), query, f.sessionID).Scan(&queuedBefore); err != nil {
				t.Fatal(err)
			}
			failSessionCheckpoint(t, f, scope, cursor)
			r, err := session.NewReconciler(f.Pool)
			if err != nil {
				t.Fatal(err)
			}
			if deferred, err := r.ReconcileLifecycle(t.Context(), f.EnvironmentID, f.sessionID); err != nil || !deferred {
				t.Fatalf("before physical stop: %v %v", deferred, err)
			}
			f.reportRuntimeClosed(t)
			dbtest.MustExec(t, t.Context(), f.Pool, `CREATE FUNCTION reject_loss_event() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.kind='session.execution_lost' THEN RAISE EXCEPTION 'injected'; END IF; RETURN NEW; END $$; CREATE TRIGGER reject_loss_event BEFORE INSERT ON session_events FOR EACH ROW EXECUTE FUNCTION reject_loss_event()`)
			if _, err = r.ReconcileLifecycle(t.Context(), f.EnvironmentID, f.sessionID); err == nil {
				t.Fatal("event failure ignored")
			}
			var held bool
			if err = f.Pool.QueryRow(t.Context(), `SELECT dispatch_hold_reason='recovery_required' AND current_run_id=$2 FROM sessions WHERE id=$1`, f.sessionID, f.runID).Scan(&held); err != nil || !held {
				t.Fatalf("partial reconciliation: %v %v", held, err)
			}
			dbtest.MustExec(t, t.Context(), f.Pool, `DROP TRIGGER reject_loss_event ON session_events`)
			type outcome struct {
				deferred bool
				err      error
			}
			done := make(chan outcome, 2)
			for range 2 {
				go func() { d, e := r.ReconcileLifecycle(t.Context(), f.EnvironmentID, f.sessionID); done <- outcome{d, e} }()
			}
			for range 2 {
				result := <-done
				if result.err != nil || result.deferred {
					t.Fatalf("concurrent reconciliation: %+v", result)
				}
			}
			var queuedAfter string
			if err = f.Pool.QueryRow(t.Context(), query, f.sessionID).Scan(&queuedAfter); err != nil || queuedAfter != queuedBefore {
				t.Fatalf("queue changed: %s -> %s: %v", queuedBefore, queuedAfter, err)
			}
			var next uuid.UUID
			var losses, events, completed, failed int
			var actualCursor int64
			var hold *uuid.UUID
			if err = f.Pool.QueryRow(t.Context(), `SELECT current_run_id,consecutive_execution_losses,committed_input_sequence,dispatch_hold_id,
   (SELECT count(*) FROM session_events WHERE session_id=s.id AND kind='session.execution_lost'),
   (SELECT count(*) FROM session_turns WHERE session_id=s.id AND status='completed'),
   (SELECT count(*) FROM session_turns WHERE session_id=s.id AND status='failed') FROM sessions s WHERE id=$1`, f.sessionID).Scan(&next, &losses, &actualCursor, &hold, &events, &completed, &failed); err != nil {
				t.Fatal(err)
			}
			wantFailed := 0
			if active {
				wantFailed = 1
			}
			if next == f.runID || losses != 1 || actualCursor != cursor || hold != nil || events != 1 || completed != 1 || failed != wantFailed {
				t.Fatalf("next=%s losses=%d cursor=%d hold=%v events=%d completed=%d failed=%d", next, losses, actualCursor, hold, events, completed, failed)
			}
			f.runID = next
			f.placeAndStart(t)
			f.turn(t, cursor+1)
			if err = f.Pool.QueryRow(t.Context(), `SELECT consecutive_execution_losses FROM sessions WHERE id=$1`, f.sessionID).Scan(&losses); err != nil || losses != 0 {
				t.Fatalf("customer progress reset=%d %v", losses, err)
			}
		})
	}
}

func TestAutomaticSessionIdleLossWaitsForDemand(t *testing.T) {
	f := lostSessionComputer(t)
	r, _ := session.NewReconciler(f.Pool)
	if deferred, err := r.ReconcileLifecycle(t.Context(), f.EnvironmentID, f.sessionID); err != nil || deferred {
		t.Fatalf("reconcile=%v %v", deferred, err)
	}
	var idle bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT current_run_id IS NULL AND dispatch_hold_id IS NULL FROM sessions WHERE id=$1`, f.sessionID).Scan(&idle); err != nil || !idle {
		t.Fatalf("idle=%v %v", idle, err)
	}
	receipt, err := f.server.applySessionAdmission(t.Context(), session.AdmissionRequest{Target: session.Target{EnvironmentID: f.EnvironmentID, SessionID: f.sessionID}, Mode: session.EnqueueOnly, Data: json.RawMessage(`"new"`)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = r.ReconcileInput(t.Context(), f.EnvironmentID, f.sessionID, receipt.TurnID); err != nil {
		t.Fatal(err)
	}
	if err := f.Pool.QueryRow(t.Context(), `SELECT current_run_id IS NOT NULL FROM sessions WHERE id=$1`, f.sessionID).Scan(&idle); err != nil || !idle {
		t.Fatalf("new demand=%v %v", idle, err)
	}
}

func TestAutomaticSessionLossHonorsStop(t *testing.T) {
	for _, mode := range []string{"interrupt", "cancel", "close"} {
		t.Run(mode, func(t *testing.T) {
			f := newActorCheckpointFixture(t)
			f.turn(t, 1)
			target := session.Target{EnvironmentID: f.EnvironmentID, SessionID: f.sessionID}
			for range 2 {
				if _, err := f.server.applySessionAdmission(t.Context(), session.AdmissionRequest{Target: target, Mode: session.EnqueueOnly, Data: json.RawMessage(`"next"`)}); err != nil {
					t.Fatal(err)
				}
			}
			scope := f.receiveTurn(t, 2)
			if mode == "interrupt" {
				if _, err := f.server.applySessionInterrupt(t.Context(), session.InterruptRequest{ControlRequest: session.ControlRequest{Target: target}, TurnID: scope.TurnID}); err != nil {
					t.Fatal(err)
				}
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE run_leases SET created_at=LEAST(created_at,now()-interval '2 seconds'),start_deadline_at=now()-interval '2 milliseconds',expires_at=now()-interval '1 millisecond' WHERE id=$1`, f.claim.runLease.ID)
				if n, err := f.placement.RecoverRunExecutionLeases(t.Context(), 10); err != nil || n != 1 {
					t.Fatalf("lease loss=%d %v", n, err)
				}
			} else {
				failSessionCheckpoint(t, f, scope, 2)
			}
			if mode == "cancel" {
				if _, err := f.server.applySessionCancel(t.Context(), session.ControlRequest{Target: target}); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "close" {
				if _, err := f.server.applySessionClose(t.Context(), session.ControlRequest{Target: target}); err != nil {
					t.Fatal(err)
				}
			}
			f.reportRuntimeClosed(t)
			r, _ := session.NewReconciler(f.Pool)
			if deferred, err := r.ReconcileLifecycle(t.Context(), f.EnvironmentID, f.sessionID); err != nil || deferred {
				t.Fatalf("reconcile: %v %v", deferred, err)
			}
			var status, turn, queued string
			var reason *string
			var current *uuid.UUID
			if err := f.Pool.QueryRow(t.Context(), `SELECT status,dispatch_hold_reason,current_run_id,
   (SELECT status FROM session_turns WHERE session_id=s.id AND sequence=2),
   (SELECT status FROM session_turns WHERE session_id=s.id AND sequence=3) FROM sessions s WHERE id=$1`, f.sessionID).Scan(&status, &reason, &current, &turn, &queued); err != nil {
				t.Fatal(err)
			}
			if turn != "failed" {
				t.Fatalf("lost Turn=%s", turn)
			}
			switch mode {
			case "cancel":
				if status != "closed" || current != nil || queued != "cancelled" || reason != nil {
					t.Fatalf("cancel: %s %v %s %v", status, current, queued, reason)
				}
			case "interrupt":
				if status != "open" || current != nil || queued != "queued" || reason == nil || *reason != "interrupted" {
					t.Fatalf("interrupt: %s %v %s %v", status, current, queued, reason)
				}
				var hold uuid.UUID
				if err := f.Pool.QueryRow(t.Context(), `SELECT dispatch_hold_id FROM sessions WHERE id=$1`, f.sessionID).Scan(&hold); err != nil {
					t.Fatal(err)
				}
				if _, err := f.server.applySessionResume(t.Context(), session.ResumeRequest{ControlRequest: session.ControlRequest{Target: target}, HoldID: hold}); err != nil {
					t.Fatal(err)
				}
			case "close":
				if status != "closing" || current == nil || queued != "queued" || reason != nil {
					t.Fatalf("close: %s %v %s %v", status, current, queued, reason)
				}
			}
		})
	}
}

func TestAutomaticSessionLossBudgetTerminatesOwner(t *testing.T) {
	for _, kind := range []string{"session", "computer"} {
		t.Run(kind, func(t *testing.T) {
			f := newActorCheckpointFixture(t)
			failSessionCheckpoint(t, f, session.TurnScope{}, 0)
			f.reportRuntimeClosed(t)
			if kind == "session" {
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE sessions SET consecutive_execution_losses=7 WHERE id=$1`, f.sessionID)
			} else {
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computers SET recovery_preparation_count=8,recovery_runtime_id=$2,next_recovery_preparation_at=now() WHERE id=$1`, f.workspaceID, f.claim.runtime.ID)
			}
			r, _ := session.NewReconciler(f.Pool)
			for range 2 {
				if deferred, err := r.ReconcileLifecycle(t.Context(), f.EnvironmentID, f.sessionID); err != nil || deferred {
					t.Fatalf("reconcile: %v %v", deferred, err)
				}
			}
			var status, reason, computer, queued string
			var current, owner *uuid.UUID
			if err := f.Pool.QueryRow(t.Context(), `SELECT s.status,s.failure->>'code',s.current_run_id,c.status,c.owner_session_id,
   (SELECT status FROM session_turns WHERE session_id=s.id AND sequence=1) FROM sessions s JOIN computers c ON c.id=s.workspace_id WHERE s.id=$1`, f.sessionID).Scan(&status, &reason, &current, &computer, &owner, &queued); err != nil {
				t.Fatal(err)
			}
			if status != "failed" || current != nil || queued != "cancelled" {
				t.Fatalf("terminal: %s %v %s", status, current, queued)
			}
			if kind == "session" && (reason != "execution_loss_limit" || computer != "active" || owner != nil) {
				t.Fatalf("session budget poisoned Computer: %s %s %v", reason, computer, owner)
			}
			if kind == "computer" && (reason != "computer_recovery_exhausted" || computer != "recovery_required") {
				t.Fatalf("Computer exhaustion: %s %s", reason, computer)
			}
		})
	}
}

func TestAutomaticSessionIdleRunStopSurvivesLoss(t *testing.T) {
	f := newActorCheckpointFixture(t)
	c, err := run.NewCanceler(f.Pool)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.Cancel(t.Context(), run.CancellationRequest{OrgID: f.OrgID, ProjectID: f.ProjectID, EnvironmentID: f.EnvironmentID, RunID: f.runID}); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE run_leases SET created_at=LEAST(created_at,now()-interval '2 seconds'),start_deadline_at=now()-interval '2 milliseconds',expires_at=now()-interval '1 millisecond' WHERE id=$1`, f.claim.runLease.ID)
	if n, err := f.placement.RecoverRunExecutionLeases(t.Context(), 10); err != nil || n != 1 {
		t.Fatalf("loss=%d %v", n, err)
	}
	f.reportRuntimeClosed(t)
	r, _ := session.NewReconciler(f.Pool)
	if deferred, err := r.ReconcileLifecycle(t.Context(), f.EnvironmentID, f.sessionID); err != nil || deferred {
		t.Fatalf("reconcile=%v %v", deferred, err)
	}
	var held bool
	if err = f.Pool.QueryRow(t.Context(), `SELECT current_run_id IS NULL AND dispatch_hold_reason='interrupted' AND active_turn_id IS NULL FROM sessions WHERE id=$1`, f.sessionID).Scan(&held); err != nil || !held {
		t.Fatalf("idle stop lost: %v %v", held, err)
	}
}

func TestAutomaticSessionLossWaitsForOwnedChildReclaim(t *testing.T) {
	f := newActorCheckpointFixture(t)
	child := workerControlChild(t, f, false)
	if _, err := f.server.applySessionAdmission(t.Context(), session.AdmissionRequest{Target: session.Target{EnvironmentID: f.EnvironmentID, SessionID: f.sessionID}, Mode: session.EnqueueOnly, Data: json.RawMessage(`"next"`)}); err != nil {
		t.Fatal(err)
	}
	var waitID uuid.UUID
	if err := f.Pool.QueryRow(t.Context(), `SELECT id FROM run_waits WHERE run_id=$1 AND kind='child' AND child_run_id=$2`, f.runID, child.runID).Scan(&waitID); err != nil {
		t.Fatal(err)
	}
	lease, err := parseRunLeaseFence(f.fence())
	if err != nil {
		t.Fatal(err)
	}
	wait, err := f.server.requestWorkerRunWaitCheckpoint(t.Context(), f.worker, f.fence(), lease, waitID)
	if err != nil {
		t.Fatal(err)
	}
	f.workerCall(t, f.server.workerMarkCheckpointFailed, workerapi.CheckpointFailedRequest{Lease: f.fence(), RunWaitID: waitID.String(), CheckpointID: pgvalue.UUIDString(wait.SuspendCheckpointID), RequestVersion: wait.CheckpointRequestVersion, Error: "capture failed"}, nil)
	f.reportRuntimeClosed(t)
	r, _ := session.NewReconciler(f.Pool)
	if deferred, err := r.ReconcileLifecycle(t.Context(), f.EnvironmentID, f.sessionID); err != nil || !deferred {
		t.Fatalf("before child reclaim=%v %v", deferred, err)
	}
	child.reportRuntimeClosed(t)
	var mount string
	if err := f.Pool.QueryRow(t.Context(), `SELECT status FROM workspace_mounts WHERE id=$1`, child.claim.workspaceMount.ID).Scan(&mount); err != nil || mount != "unmounting" {
		t.Fatalf("child mount=%s %v", mount, err)
	}
	if deferred, err := r.ReconcileLifecycle(t.Context(), f.EnvironmentID, f.sessionID); err != nil || deferred {
		t.Fatalf("after child reclaim=%v %v", deferred, err)
	}
	var fresh bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT current_run_id IS NOT NULL AND current_run_id<>$2 FROM sessions WHERE id=$1`, f.sessionID, f.runID).Scan(&fresh); err != nil || !fresh {
		t.Fatalf("no continuation: %v %v", fresh, err)
	}
}

func TestReclaimedChildComputerMountIsReleasedForReuse(t *testing.T) {
	f := newActorCheckpointFixture(t)
	child := workerControlChild(t, f, false)
	if _, err := f.server.applySessionCancel(t.Context(), session.ControlRequest{Target: session.Target{EnvironmentID: f.EnvironmentID, SessionID: f.sessionID}}); err != nil {
		t.Fatal(err)
	}
	child.reportRuntimeClosed(t)
	if _, err := f.placement.RecoverExpiredRuntimeReservations(t.Context(), 10); err != nil {
		t.Fatal(err)
	}
	var mount string
	var unowned bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT m.status,c.owner_run_id IS NULL AND c.owner_session_id IS NULL FROM workspace_mounts m JOIN computers c ON c.id=m.workspace_id WHERE m.id=$1`, child.claim.workspaceMount.ID).Scan(&mount, &unowned); err != nil || mount != "failed" || !unowned {
		t.Fatalf("reclaimed child mount=%s unowned=%v err=%v", mount, unowned, err)
	}
}

func TestEmptyParkedInterruptionResumesWithoutStartingRun(t *testing.T) {
	f := newActorCheckpointFixture(t)
	scope := f.receiveTurn(t, 1)
	waits, registration := actorTokenWait(t, f, scope)
	registration.ActorSpeculativeInputSequence.Int64 = 1
	if _, err := waits.RegisterWait(t.Context(), registration); err != nil {
		t.Fatal(err)
	}
	f.publishWaitCheckpoint(t, registration.WaitID, f.capture(t, "parked"))
	target := session.Target{EnvironmentID: f.EnvironmentID, SessionID: f.sessionID}
	if _, err := f.server.applySessionInterrupt(t.Context(), session.InterruptRequest{ControlRequest: session.ControlRequest{Target: target}, TurnID: scope.TurnID}); err != nil {
		t.Fatal(err)
	}
	r, _ := session.NewReconciler(f.Pool)
	if deferred, err := r.ReconcileLifecycle(t.Context(), f.EnvironmentID, f.sessionID); err != nil || !deferred {
		t.Fatalf("before physical cleanup=%v %v", deferred, err)
	}
	var retained bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT active_turn_id=$2 AND current_run_id=$3 AND dispatch_hold_reason='interrupt_requested' FROM sessions WHERE id=$1`, f.sessionID, scope.TurnID, f.runID).Scan(&retained); err != nil || !retained {
		t.Fatalf("premature settlement=%v %v", retained, err)
	}
	f.reportRuntimeClosed(t)

	if deferred, err := r.ReconcileLifecycle(t.Context(), f.EnvironmentID, f.sessionID); err != nil || deferred {
		t.Fatalf("settle=%v %v", deferred, err)
	}
	var hold uuid.UUID
	if err := f.Pool.QueryRow(t.Context(), `SELECT dispatch_hold_id FROM sessions WHERE id=$1`, f.sessionID).Scan(&hold); err != nil {
		t.Fatal(err)
	}
	if _, err := f.server.applySessionResume(t.Context(), session.ResumeRequest{ControlRequest: session.ControlRequest{Target: target}, HoldID: hold}); err != nil {
		t.Fatal(err)
	}
	var idle bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT status='open' AND current_run_id IS NULL AND dispatch_hold_id IS NULL AND active_turn_id IS NULL FROM sessions WHERE id=$1`, f.sessionID).Scan(&idle); err != nil || !idle {
		t.Fatalf("resume idle=%v %v", idle, err)
	}
}
