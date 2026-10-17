package session_test

import (
	"encoding/json"
	"fmt"
	"github.com/helmrdotdev/helmr/internal/audit"
	"github.com/helmrdotdev/helmr/internal/session/sessiontest"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/session"
)

func TestOutputOnlyWakesReadersWhileNewWorkWakesWorker(t *testing.T) {
	f := newFixture(t)
	running := f.enqueue(t, "running")
	if _, err := dispatchTurnForTest(t.Context(), f.pool, f.execution()); err != nil {
		t.Fatal(err)
	}
	if err := session.RuntimeRegisterMessages(t.Context(), f.pool, *f.host(), f.execution(), running.TurnID); err != nil {
		t.Fatal(err)
	}
	topics := dbtest.ListenChanges(t, f.pool)
	for range 3 {
		if _, err := session.RuntimeOutput(t.Context(), f.pool, *f.host(), f.execution(), running.TurnID, uuid.NewV7(), json.RawMessage(`[{"type":"text","text":"progress"}]`)); err != nil {
			t.Fatal(err)
		}
	}
	observed := topics()
	if !observed["session:"+f.session.String()] || !observed["slack_projection"] || observed["worker:"+f.worker.String()] || observed["dispatch"] {
		t.Fatalf("output notification targets: %v", observed)
	}
	f.enqueue(t, "successor")
	if observed = topics(); !observed["worker:"+f.worker.String()] || !observed["dispatch"] {
		t.Fatalf("queued Turn did not wake execution: %v", observed)
	}
	_, err := session.SendTurn(t.Context(), f.pool, f.caller(), session.EnqueueRequest{EnvironmentID: f.env, SessionID: f.session, IdempotencyKey: "steer", Input: json.RawMessage(`[{"type":"text","text":"steer"}]`)}, running.TurnID)
	if err != nil {
		t.Fatal(err)
	}
	if observed = topics(); !observed["worker:"+f.worker.String()] {
		t.Fatalf("steering message did not wake worker: %v", observed)
	}
}

func TestWorkAdmissionNotificationsFollowOuterTransaction(t *testing.T) {
	f := newFixture(t)
	topics := dbtest.ListenChanges(t, f.pool)
	for _, commit := range []bool{false, true} {
		tx, err := f.pool.Begin(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		_, err = session.Enqueue(t.Context(), tx, nil, f.caller(), session.EnqueueRequest{EnvironmentID: f.env, SessionID: f.session, IdempotencyKey: uuid.NewV7().String(), Input: json.RawMessage(`[]`)})
		if err != nil {
			_ = tx.Rollback(t.Context())
			t.Fatal(err)
		}
		if got := topics(); len(got) != 0 {
			t.Fatalf("uncommitted work notified: %v", got)
		}
		if commit {
			err = tx.Commit(t.Context())
		} else {
			err = tx.Rollback(t.Context())
		}
		if err != nil {
			t.Fatal(err)
		}
		got := topics()
		if !commit && len(got) != 0 {
			t.Fatalf("rolled back work notified: %v", got)
		}
		if commit && (!got["session:"+f.session.String()] || !got["worker:"+f.worker.String()] || !got["dispatch"]) {
			t.Fatalf("committed work did not wake owners: %v", got)
		}
	}
}

func TestControlReceiptReplayDoesNotWakeExecution(t *testing.T) {
	f := newFixture(t)
	attachment, err := session.AcquireRuntimeAttachment(t.Context(), f.pool, *f.host(), f.execution())
	if err != nil {
		t.Fatal(err)
	}
	delivery, err := session.PrepareSessionDelivery(t.Context(), f.pool, *f.host(), f.execution(), attachment.Sequence)
	if err != nil {
		t.Fatal(err)
	}
	topics := dbtest.ListenChanges(t, f.pool)
	for _, replay := range []bool{false, true} {
		if err := session.AcknowledgeSessionDelivery(t.Context(), f.pool, *f.host(), f.execution(), attachment.Sequence, delivery.Sequence, delivery.Generation, delivery.Kind, "", delivery.StopSequence); err != nil {
			t.Fatal(err)
		}
		got := topics()
		if replay && len(got) != 0 {
			t.Fatalf("unchanged control receipt notified: %v", got)
		}
		if !replay && !got["worker:"+f.worker.String()] {
			t.Fatalf("new control receipt did not notify: %v", got)
		}
	}
	if err := session.ObserveSessionReady(t.Context(), f.pool, *f.host(), f.execution(), attachment.Sequence); err != nil {
		t.Fatal(err)
	}
	_ = topics()
	if err := session.ObserveSessionReady(t.Context(), f.pool, *f.host(), f.execution(), attachment.Sequence); err != nil {
		t.Fatal(err)
	}
	if got := topics(); len(got) != 0 {
		t.Fatalf("unchanged ready receipt notified: %v", got)
	}
}

func TestClosingChildrenNotifyReadersBeforeTheirWorkDrains(t *testing.T) {
	for _, reconcile := range []bool{false, true} {
		t.Run(fmt.Sprintf("reconcile=%v", reconcile), func(t *testing.T) {
			f := newFixture(t)
			child := ownedFixture(t, f)
			child.enqueue(t, "child")
			if reconcile {
				parent := f.enqueue(t, "parent")
				if _, err := session.ControlSession(t.Context(), f.pool, f.caller(), controlRequest(f, "close", "close")); err != nil {
					t.Fatal(err)
				}
				dbtest.MustExec(t, t.Context(), f.pool, `UPDATE turns SET status='failed',terminal_at=clock_timestamp() WHERE id=$1`, parent.TurnID)
			}
			topics := dbtest.ListenChanges(t, f.pool)
			if reconcile {
				if err := session.ReconcileSessionClosure(t.Context(), f.pool, f.env, f.session); err != nil {
					t.Fatal(err)
				}
			} else if _, err := session.ControlSession(t.Context(), f.pool, f.caller(), controlRequest(f, "close", "close")); err != nil {
				t.Fatal(err)
			}
			var state string
			if err := f.pool.QueryRow(t.Context(), `SELECT status FROM sessions WHERE id=$1`, child.session).Scan(&state); err != nil || state != "closing" {
				t.Fatalf("child state=%s error=%v", state, err)
			}
			if got := topics(); !got["session:"+child.session.String()] {
				t.Fatalf("child closing did not notify its readers: %v", got)
			}
			if err := session.ReconcileSessionClosure(t.Context(), f.pool, f.env, f.session); err != nil {
				t.Fatal(err)
			}
			if got := topics(); len(got) != 0 {
				t.Fatalf("unchanged closing tree notified: %v", got)
			}
		})
	}
}

func TestShutdownReceiptReplayRetainsInterruptedTurnWithoutNotifying(t *testing.T) {
	f := newFixture(t)
	turn := f.enqueue(t, "running")
	if _, err := dispatchTurnForTest(t.Context(), f.pool, f.execution()); err != nil {
		t.Fatal(err)
	}
	interrupt := controlRequest(f, "turn_interrupt", "interrupt-turn")
	interrupt.TurnID = turn.TurnID
	if _, err := session.ControlSession(t.Context(), f.pool, f.caller(), interrupt); err != nil {
		t.Fatal(err)
	}
	if _, err := session.ControlSession(t.Context(), f.pool, f.caller(), controlRequest(f, "cancel", "cancel")); err != nil {
		t.Fatal(err)
	}
	delivery, err := session.PrepareSessionDelivery(t.Context(), f.pool, *f.host(), f.execution(), sessiontest.SeededAttachment)
	if err != nil || delivery.Kind != "shutdown" || delivery.StopTurnID == nil {
		t.Fatalf("shutdown with interrupted identity: %+v %v", delivery, err)
	}
	topics := dbtest.ListenChanges(t, f.pool)
	for _, replay := range []bool{false, true} {
		if err := session.AcknowledgeSessionDelivery(t.Context(), f.pool, *f.host(), f.execution(), sessiontest.SeededAttachment, delivery.Sequence, delivery.Generation, delivery.Kind, "", delivery.StopSequence); err != nil {
			t.Fatal(err)
		}
		got := topics()
		if replay && len(got) != 0 {
			t.Fatalf("unchanged shutdown notified: %v", got)
		}
		if !replay && !got["worker:"+f.worker.String()] {
			t.Fatalf("new shutdown acknowledgment did not notify: %v", got)
		}
	}
	var retained bool
	if err := f.pool.QueryRow(t.Context(), `SELECT stop_turn_id=$2 AND control_acknowledged_at IS NOT NULL AND fenced_at IS NULL FROM session_processes WHERE session_id=$1`, f.session, turn.TurnID).Scan(&retained); err != nil || !retained {
		t.Fatalf("shutdown receipt changed physical stop identity: %v %v", retained, err)
	}
}

func TestIdleStopIntentNotifiesWithoutTurnOrNewHold(t *testing.T) {
	for _, cause := range []string{"failure", "revocation", "cancelled-child"} {
		t.Run(cause, func(t *testing.T) {
			f := newFixture(t)
			var stop func() error
			switch cause {
			case "failure":
				a, err := session.AcquireRuntimeAttachment(t.Context(), f.pool, *f.host(), f.execution())
				if err != nil {
					t.Fatal(err)
				}
				dbtest.MustExec(t, t.Context(), f.pool, `UPDATE sessions SET status='cancelled' WHERE id=$1`, f.session)
				stop = func() error {
					return session.ObserveSessionFailure(t.Context(), f.pool, *f.host(), f.execution(), a.Sequence)
				}
			case "revocation":
				p := preparationResidentImage(t)
				f = p.f.fixture
				if _, err := p.f.secrets.Revoke(t.Context(), audit.System("test-fixture"), f.env, p.f.secretID, "revoke"); err != nil {
					t.Fatal(err)
				}
				// Revocation was already recorded, but the physical stop intent still needs delivery.
				dbtest.MustExec(t, t.Context(), f.pool, `UPDATE session_processes SET failure_recorded_at=clock_timestamp() WHERE session_id=$1`, f.session)
				stop = func() error { return session.ReconcileComputerSecretRevocation(t.Context(), f.pool, f.env, f.computer) }
			case "cancelled-child":
				parent := f
				f = ownedFixture(t, parent)
				dbtest.MustExec(t, t.Context(), f.pool, `UPDATE sessions SET status='cancelled' WHERE id=$1`, f.session)
				stop = func() error {
					_, err := session.ControlSession(t.Context(), parent.pool, parent.caller(), controlRequest(parent, "cancel", "cancel-tree"))
					return err
				}
			}
			topics := dbtest.ListenChanges(t, f.pool)
			if err := stop(); err != nil {
				t.Fatal(err)
			}
			got := topics()
			if !got["session:"+f.session.String()] || !got["worker:"+f.worker.String()] {
				t.Fatalf("idle stop missing reader or worker hints: %v", got)
			}
			var stopping bool
			if err := f.pool.QueryRow(t.Context(), `SELECT status='stopping' AND fenced_at IS NULL FROM session_processes WHERE session_id=$1`, f.session).Scan(&stopping); err != nil || !stopping {
				t.Fatalf("stop intent missing: %v %v", stopping, err)
			}
			if err := stop(); err != nil {
				t.Fatal(err)
			}
			if got := topics(); len(got) != 0 {
				t.Fatalf("replayed stop notified: %v", got)
			}
		})
	}
}
