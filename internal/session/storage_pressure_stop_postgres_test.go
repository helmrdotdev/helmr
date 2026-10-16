package session_test

import (
	"errors"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/audit"
	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/session"
	"github.com/jackc/pgx/v5"
	"uuid"
)

func pressureStopFixture(t *testing.T) (fixture, int64, session.SessionDelivery) {
	t.Helper()
	f := newFixture(t)
	f.enqueue(t, "blocked write")
	if _, err := dispatchTurnForTest(t.Context(), f.pool, f.execution()); err != nil {
		t.Fatal(err)
	}
	a, err := session.AcquireRuntimeAttachment(t.Context(), f.pool, *f.host(), f.execution())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err = session.ReportStoragePressure(t.Context(), f.pool, *f.host(), saveIdentity(f), &now); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE computer_leases SET storage_pressure_since=clock_timestamp()-interval '31 seconds'`)
	pressureReport(t, f, true)
	hold, err := session.PrepareSessionDelivery(t.Context(), f.pool, *f.host(), f.execution(), a.Sequence)
	if err != nil {
		t.Fatal(err)
	}
	if err = session.AcknowledgeSessionDelivery(t.Context(), f.pool, *f.host(), f.execution(), a.Sequence, hold.Sequence, hold.Generation, hold.Kind, "", hold.StopSequence); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE computer_leases SET storage_pressure_since=clock_timestamp()-interval '301 seconds'`)
	pressureReport(t, f, true)
	return f, a.Sequence, hold
}
func pressureReport(t *testing.T, f fixture, pressured bool) {
	t.Helper()
	var since *time.Time
	if pressured {
		now := time.Now()
		since = &now
	}
	if err := session.ReportStoragePressure(t.Context(), f.pool, *f.host(), saveIdentity(f), since); err != nil {
		t.Fatal(err)
	}
}
func pressureDeadline(t *testing.T, f fixture) time.Time {
	t.Helper()
	var deadline time.Time
	if err := f.pool.QueryRow(t.Context(), `SELECT stop_deadline_at FROM session_processes WHERE session_id=$1 AND fenced_at IS NULL`, f.session).Scan(&deadline); err != nil {
		t.Fatal(err)
	}
	return deadline
}
func TestPressureStopSupersedesHoldACKAndOnlyMatchingStopReleasesDispatch(t *testing.T) {
	f, a, hold := pressureStopFixture(t)
	deadline := pressureDeadline(t, f)
	if remaining := time.Until(deadline); remaining <= 35*time.Second || remaining > 40*time.Second {
		t.Fatalf("ordinary stop deadline: %v", remaining)
	}
	stop, err := session.PrepareSessionDelivery(t.Context(), f.pool, *f.host(), f.execution(), a)
	if err != nil || stop.Sequence <= hold.Sequence || stop.Generation <= hold.Generation || stop.StopDeadline == nil || stop.StopTurnID == nil {
		t.Fatalf("stop %+v %v", stop, err)
	}
	if err = session.AcknowledgeSessionDelivery(t.Context(), f.pool, *f.host(), f.execution(), a, hold.Sequence, hold.Generation, hold.Kind, "", hold.StopSequence); !errors.Is(err, session.ErrConflict) {
		t.Fatalf("old hold ACK %v", err)
	}
	view, err := session.GetSession(t.Context(), f.pool, f.caller(), f.env, f.session)
	if err != nil || view.Activity.Process == nil || view.Activity.Process.Stop == nil || view.Activity.Process.Stop.Deadline.IsZero() {
		t.Fatalf("pending stop hidden: %+v %v", view.Activity, err)
	}
	turn, err := session.GetTurn(t.Context(), f.pool, f.caller(), f.env, f.session, *stop.StopTurnID)
	if err != nil || turn.Stop == nil || turn.Stop.Deadline.IsZero() {
		t.Fatalf("Turn stop hidden: %+v %v", turn.Stop, err)
	}
	exact := controlRequest(f, "turn_interrupt", "already-interrupted")
	exact.TurnID = *stop.StopTurnID
	if _, err = session.ControlSession(t.Context(), f.pool, f.caller(), exact); err == nil {
		t.Fatal("exact interrupt unexpectedly changed terminal Turn")
	}
	if !pressureDeadline(t, f).Equal(deadline) {
		t.Fatal("terminal exact interrupt changed stop deadline")
	}
	queued := f.enqueue(t, "after recovery")
	pressureReport(t, f, false)
	if _, err = dispatchTurnForTest(t.Context(), f.pool, f.execution()); err == nil {
		t.Fatal("pending stop admitted queued request")
	}
	armed, err := session.PrepareSessionDelivery(t.Context(), f.pool, *f.host(), f.execution(), a)
	if err != nil || armed.StopDeadline == nil || !armed.StopDeadline.Equal(deadline) || armed.Sequence <= stop.Sequence {
		t.Fatalf("armed %+v %v", armed, err)
	}
	if err = session.AcknowledgeSessionDelivery(t.Context(), f.pool, *f.host(), f.execution(), a, armed.Sequence, armed.Generation, armed.Kind, "", armed.StopSequence+1); !errors.Is(err, session.ErrConflict) {
		t.Fatalf("wrong stop ACK %v", err)
	}
	if err = session.AcknowledgeSessionDelivery(t.Context(), f.pool, *f.host(), f.execution(), a, armed.Sequence, armed.Generation, armed.Kind, "", armed.StopSequence); err != nil {
		t.Fatal(err)
	}
	e := f.execution()
	e.AuthorityGeneration = armed.Generation
	next, err := dispatchTurnForTest(t.Context(), f.pool, e)
	if err != nil || next.TurnID != queued.TurnID {
		t.Fatalf("queued %+v %v", next, err)
	}
	var cleared bool
	if err = f.pool.QueryRow(t.Context(), `SELECT stop_turn_id IS NULL AND stop_deadline_at IS NULL FROM session_processes WHERE session_id=$1`, f.session).Scan(&cleared); err != nil || !cleared {
		t.Fatalf("stop not cleared %v %v", cleared, err)
	}
	pressureReport(t, f, true)
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE computer_leases SET storage_pressure_since=clock_timestamp()-interval '301 seconds'`)
	pressureReport(t, f, true)
	successor, err := session.PrepareSessionDelivery(t.Context(), f.pool, *f.host(), f.execution(), a)
	if err != nil || successor.StopSequence != armed.StopSequence+1 || successor.StopTurnID == nil || *successor.StopTurnID != next.TurnID || successor.StopDeadline == nil || !successor.StopDeadline.After(deadline) {
		t.Fatalf("new stop failed to acquire its own budget %+v %v", successor, err)
	}
}
func TestPressureStopDeadlineSurvivesRecurrenceAndExpiresUnderPressure(t *testing.T) {
	f, _, _ := pressureStopFixture(t)
	deadline := pressureDeadline(t, f)
	for _, pressured := range []bool{true, false, true, false, true} {
		pressureReport(t, f, pressured)
		if !pressureDeadline(t, f).Equal(deadline) {
			t.Fatal("pressure transition reset or froze stop deadline")
		}
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE session_processes SET stop_deadline_at=clock_timestamp()-interval '1 second' WHERE session_id=$1`, f.session)
	pressureReport(t, f, true)
	if err := session.ReconcileProcessStopDeadline(t.Context(), f.pool, f.env, f.session); err != nil {
		t.Fatal(err)
	}
	var failed, computerLive bool
	if err := f.pool.QueryRow(t.Context(), `SELECT p.failure_recorded_at IS NOT NULL AND p.stop_deadline_at<clock_timestamp(),l.status='active' AND l.fenced_at IS NULL
 FROM session_processes p JOIN computer_leases l ON (l.environment_id,l.computer_id,l.epoch)=(p.environment_id,p.computer_id,p.computer_lease_epoch) WHERE p.session_id=$1`, f.session).Scan(&failed, &computerLive); err != nil || !failed || !computerLive {
		t.Fatalf("failed=%v Computer live=%v err=%v", failed, computerLive, err)
	}
}
func TestPressureStopExplicitControlsAndActualFailureKeepDeadline(t *testing.T) {
	for _, kind := range []string{"interrupt", "cancel", "close", "failure", "lease_loss", "integrity"} {
		t.Run(kind, func(t *testing.T) {
			f, a, _ := pressureStopFixture(t)
			switch kind {
			case "failure":
				if err := session.ObserveSessionFailure(t.Context(), f.pool, *f.host(), f.execution(), a); err != nil {
					t.Fatal(err)
				}
			case "lease_loss":
				dbtest.MustExec(t, t.Context(), f.pool, `UPDATE computer_leases SET status='lost',expires_at=clock_timestamp()-interval '1 second'`)
			case "integrity":
				dbtest.MustExec(t, t.Context(), f.pool, `UPDATE computers SET integrity_fault_at=clock_timestamp(),integrity_fault_reason='test'`)
			default:
				if _, err := session.ControlSession(t.Context(), f.pool, f.caller(), controlRequest(f, kind, "explicit")); err != nil {
					t.Fatal(err)
				}
			}
			if kind == "lease_loss" || kind == "integrity" {
				if _, _, err := session.ReconcileSessionLifecycle(t.Context(), f.pool, session.SessionLifecyclePosition{}); err != nil {
					t.Fatal(err)
				}
			}
			var ended bool
			if err := f.pool.QueryRow(t.Context(), `SELECT stop_deadline_at IS NOT NULL FROM session_processes WHERE session_id=$1`, f.session).Scan(&ended); err != nil || !ended {
				t.Fatalf("exception persisted %v %v", ended, err)
			}
		})
	}
}

func TestPressureStopRevocationRetainsShutdown(t *testing.T) {
	f, a, _ := pressureStopFixture(t)
	_, _, store, secretID, _, _ := runtimeSecretsOnFixture(t, f, f.execution())
	if err := db.RunTx(t.Context(), f.pool, func(tx pgx.Tx) error {
		return computer.RecordSessionExposures(t.Context(), tx, f.env, f.session, 1, []uuid.UUID{secretID})
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Revoke(t.Context(), audit.System("test-fixture"), f.env, secretID, "revoke"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := session.ReconcileSessionLifecycle(t.Context(), f.pool, session.SessionLifecyclePosition{}); err != nil {
		t.Fatal(err)
	}
	var ended bool
	if err := f.pool.QueryRow(t.Context(), `SELECT stop_deadline_at IS NOT NULL FROM session_processes WHERE session_id=$1`, f.session).Scan(&ended); err != nil || !ended {
		t.Fatalf("revocation retained exception %v %v", ended, err)
	}
	// runtimeSecretsOnFixture reattaches; use that current attachment.
	if err := f.pool.QueryRow(t.Context(), `SELECT attachment_sequence FROM session_processes WHERE session_id=$1`, f.session).Scan(&a); err != nil {
		t.Fatal(err)
	}
	control, err := session.PrepareSessionDelivery(t.Context(), f.pool, *f.host(), f.execution(), a)
	if err != nil || control.Kind != "shutdown" {
		t.Fatalf("revoked control %+v %v", control, err)
	}
}

func TestOrdinaryStopDeadlineSurvivesStoragePressure(t *testing.T) {
	f := newFixture(t)
	turn := f.enqueue(t, "ordinary stop")
	if _, err := dispatchTurnForTest(t.Context(), f.pool, f.execution()); err != nil {
		t.Fatal(err)
	}
	request := controlRequest(f, "turn_interrupt", "ordinary")
	request.TurnID = turn.TurnID
	if _, err := session.ControlSession(t.Context(), f.pool, f.caller(), request); err != nil {
		t.Fatal(err)
	}
	pressureReport(t, f, true)
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE computer_leases SET storage_pressure_since=clock_timestamp()-interval '301 seconds'`)
	pressureReport(t, f, true)
	var ordinary bool
	if err := f.pool.QueryRow(t.Context(), `SELECT stop_deadline_at IS NOT NULL FROM session_processes WHERE session_id=$1`, f.session).Scan(&ordinary); err != nil || !ordinary {
		t.Fatalf("ordinary stop gained exemption %v %v", ordinary, err)
	}
}

func TestPressureStopChecksExactProcessLeaseWhenEpochsDiffer(t *testing.T) {
	for _, kind := range []string{"eligible", "expired", "integrity"} {
		t.Run(kind, func(t *testing.T) {
			f := newFixture(t)
			dbtest.MustExec(t, t.Context(), f.pool, `UPDATE session_processes SET status='lost',fenced_at=clock_timestamp() WHERE session_id=$1`, f.session)
			dbtest.MustExec(t, t.Context(), f.pool, `INSERT INTO session_processes(environment_id,session_id,epoch,computer_id,computer_lease_epoch,status,attachment_sequence,control_sequence,control_attachment,control_kind,control_generation,control_acknowledged_at) VALUES($1,$2,2,$3,1,'ready',1,1,1,'resume',1,clock_timestamp())`, f.env, f.session, f.computer)
			turn := f.enqueue(t, "second process on first lease")
			execution := f.execution()
			execution.ProcessEpoch = 2
			if _, err := dispatchTurnForTest(t.Context(), f.pool, execution); err != nil {
				t.Fatal(err)
			}
			pressureReport(t, f, true)
			dbtest.MustExec(t, t.Context(), f.pool, `UPDATE computer_leases SET storage_pressure_since=clock_timestamp()-interval '301 seconds'`)
			switch kind {
			case "expired":
				dbtest.MustExec(t, t.Context(), f.pool, `UPDATE computer_leases SET expires_at=clock_timestamp()-interval '1 second'`)
			case "integrity":
				dbtest.MustExec(t, t.Context(), f.pool, `UPDATE computers SET integrity_fault_at=clock_timestamp(),integrity_fault_reason='test'`)
			}
			if _, _, err := session.ReconcileSessionLifecycle(t.Context(), f.pool, session.SessionLifecyclePosition{}); err != nil {
				t.Fatal(err)
			}
			if kind == "expired" {
				var untouched bool
				if err := f.pool.QueryRow(t.Context(), `SELECT t.status='running' AND p.stop_sequence=0 FROM session_processes p JOIN turns t ON t.id=$2 WHERE p.session_id=$1 AND p.epoch=2`, f.session, turn.TurnID).Scan(&untouched); err != nil || !untouched {
					t.Fatalf("expired source received pressure stop: %v %v", untouched, err)
				}
				return
			}
			var ordinary, stopped bool
			if err := f.pool.QueryRow(t.Context(), `SELECT p.stop_sequence=1 AND p.stop_deadline_at>clock_timestamp(),t.status='interrupted' AND p.stop_turn_id=t.id FROM session_processes p JOIN turns t ON t.id=$2 WHERE p.session_id=$1 AND p.epoch=2`, f.session, turn.TurnID).Scan(&ordinary, &stopped); err != nil || !stopped || !ordinary {
				t.Fatalf("%s ordinary=%v stopped=%v err=%v", kind, ordinary, stopped, err)
			}
		})
	}
}

func TestPressureStopRemainsEligibleForAuthorityRenewal(t *testing.T) {
	f, _, _ := pressureStopFixture(t)
	for _, pressured := range []bool{true, false} {
		pressureReport(t, f, pressured)
		authority, err := session.RenewRuntimeAuthority(t.Context(), f.pool, *f.host(), f.execution())
		if err != nil || authority.Generation < 3 || !authority.ExpiresAt.After(time.Now().Add(25*time.Second)) {
			t.Fatalf("pressure=%v authority=%+v err=%v", pressured, authority, err)
		}
	}
}
