package session_test

import (
	"context"
	"errors"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/dispatch"
	"github.com/helmrdotdev/helmr/internal/session"
)

func TestShortStoragePressureClearNotifiesWaitingWorker(t *testing.T) {
	f := newFixture(t)
	if err := session.ReportStoragePressure(t.Context(), auditpolicy.Policy{
		RetentionSeconds: 3600,
	}, f.pool, *f.host(), saveIdentity(f), true); err != nil {
		t.Fatal(err)
	}
	listener, err := f.pool.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Release()
	if _, err = listener.Exec(t.Context(), "LISTEN helmr_changes"); err != nil {
		t.Fatal(err)
	}
	if err = session.ReportStoragePressure(t.Context(), auditpolicy.Policy{
		RetentionSeconds: 3600,
	}, f.pool, *f.host(), saveIdentity(f), false); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	want := map[string]bool{"dispatch": true, "worker:" + f.host().HostID.String(): true}
	for len(want) > 0 {
		notification, err := listener.Conn().WaitForNotification(ctx)
		if err != nil {
			t.Fatalf("pressure clear missing notifications %v: %v", want, err)
		}
		delete(want, notification.Payload)
	}
	var holds int
	if err = f.pool.QueryRow(t.Context(), "SELECT count(*) FROM session_holds").Scan(&holds); err != nil || holds != 0 {
		t.Fatalf("short pressure unexpectedly created holds: %d %v", holds, err)
	}
}

func TestStoragePressureUsesServerBudgetQueuesThenResolvesExactHold(t *testing.T) {
	f := newFixture(t)
	peer := f.peer(t)
	running := f.enqueue(t, "running")
	if _, err := dispatchTurnForTest(t.Context(), f.pool, f.execution()); err != nil {
		t.Fatal(err)
	}
	queued := peer.enqueue(t, "queued")
	// Pressure reports carry no worker clock; elapsed time belongs to the server.
	report := func(value bool) {
		t.Helper()
		if err := session.ReportStoragePressure(t.Context(), auditpolicy.Policy{
			RetentionSeconds: 3600,
		}, f.pool, *f.host(), saveIdentity(f), value); err != nil {
			t.Fatal(err)
		}
	}
	report(true)
	var valid bool
	if err := f.pool.QueryRow(t.Context(), `SELECT storage_pressure_since>clock_timestamp()-interval '5 seconds' FROM computer_leases`).Scan(&valid); err != nil || !valid {
		t.Fatalf("server budget %v %v", valid, err)
	}
	if _, err := dispatchTurnForTest(t.Context(), f.pool, peer.execution()); err == nil {
		t.Fatal("new work dispatched through pressure")
	}
	if err := f.pool.QueryRow(t.Context(), `SELECT NOT EXISTS(SELECT 1 FROM session_holds) AND NOT EXISTS(SELECT 1 FROM session_events WHERE kind='session.storage_pressure')`).Scan(&valid); err != nil || !valid {
		t.Fatalf("transient pressure exposed %v %v", valid, err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE computer_leases SET storage_pressure_since=clock_timestamp()-interval '31 seconds'`)
	for range 2 {
		report(true)
	}
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*)=2 FROM session_events WHERE kind='session.storage_pressure' AND convert_from(data,'UTF8')::jsonb->>'state'='paused'`).Scan(&valid); err != nil || !valid {
		t.Fatalf("duplicate pressure notice %v %v", valid, err)
	}
	var pressureHold uuid.UUID
	if err := f.pool.QueryRow(t.Context(), `SELECT id FROM session_holds WHERE session_id=$1 AND kind='storage_pressure' AND released_at IS NULL`, f.session).Scan(&pressureHold); err != nil {
		t.Fatal(err)
	}
	manual := controlRequest(f, "resume", "pressure-is-automatic")
	manual.HoldID = pressureHold
	if _, err := session.ControlSession(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, f.caller(), manual); !errors.Is(err, session.ErrNotReady) {
		t.Fatalf("manual resume cleared automatic pressure hold %v", err)
	}
	independent := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.pool, `INSERT INTO session_holds(environment_id,id,session_id,scope,reason) VALUES($1,$2,$3,'local','user pause')`, f.env, independent, f.session)
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE computer_leases SET storage_pressure_since=clock_timestamp()-interval '301 seconds'`)
	// The durable lifecycle owns timeout even without another worker report.
	for range 2 {
		if _, _, err := session.ReconcileSessionLifecycle(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, session.SessionLifecyclePosition{}); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.pool.QueryRow(t.Context(), `SELECT
 (SELECT status='interrupted' FROM turns WHERE id=$1)
 AND (SELECT status='queued' FROM turns WHERE id=$2)
 AND (SELECT status='active' AND fenced_at IS NULL FROM computer_leases)
 AND (SELECT count(*)=1 FROM session_events WHERE kind='session.storage_pressure' AND turn_id=$1 AND convert_from(data,'UTF8')::jsonb->>'state'='stopped')
 AND (SELECT reason='The request was stopped because storage stayed full. Saving is still retrying. New requests will wait until space is available.' AND released_at IS NULL FROM session_holds WHERE id=$3)
 AND (SELECT reason='Waiting for saving to recover' FROM session_holds WHERE session_id=$4 AND kind='storage_pressure' AND released_at IS NULL)`, running.TurnID, queued.TurnID, pressureHold, peer.session).Scan(&valid); err != nil || !valid {
		t.Fatalf("pressure timeout custody %v %v", valid, err)
	}
	report(false)
	report(false)
	var auditValid bool
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*)=4
 AND count(*) FILTER (WHERE action='session.hold_created')=2
 AND count(*) FILTER (WHERE action='session.hold_released')=2
 AND count(DISTINCT details->>'hold_id')=2
 AND bool_and(details->>'reason'='storage_pressure' AND outcome='succeeded')
 FROM audit_events WHERE action IN ('session.hold_created','session.hold_released')`).Scan(&auditValid); err != nil || !auditValid {
		t.Fatalf("hold direction, reason or replay changed: %v %v", auditValid, err)
	}
	if err := f.pool.QueryRow(t.Context(), `SELECT
 NOT EXISTS(SELECT 1 FROM session_holds WHERE kind='storage_pressure' AND released_at IS NULL)
 AND (SELECT released_at IS NULL FROM session_holds WHERE id=$1)
 AND (SELECT count(*)=2 FROM session_events WHERE kind='session.storage_pressure' AND convert_from(data,'UTF8')::jsonb->>'state'='resolved')`, independent).Scan(&valid); err != nil || !valid {
		t.Fatalf("pressure resolution changed unrelated hold %v %v", valid, err)
	}
}

func TestStoragePressureCannotPreemptLostExecutionOutcome(t *testing.T) {
	for _, change := range []struct{ name, sql string }{
		{"expired", "UPDATE computer_leases SET expires_at=clock_timestamp()-interval '1 second'"},
		{"host-lost", "UPDATE worker_hosts SET status='lost',lost_at=clock_timestamp()"},
		{"host-replaced", "UPDATE worker_hosts SET current_epoch=current_epoch+1"},
		{"host-registering", "UPDATE worker_hosts SET status='registering',current_epoch=NULL,current_service_id=NULL,epoch_started_at=NULL"},
	} {
		t.Run(change.name, func(t *testing.T) {
			f := newFixture(t)
			checkpointImageExposure(t, f)
			running := f.enqueue(t, "running")
			if _, err := dispatchTurnForTest(t.Context(), f.pool, f.execution()); err != nil {
				t.Fatal(err)
			}
			queued := f.enqueue(t, "queued")
			if err := session.ReportStoragePressure(t.Context(), auditpolicy.Policy{
				RetentionSeconds: 3600,
			}, f.pool, *f.host(), saveIdentity(f), true); err != nil {
				t.Fatal(err)
			}
			dbtest.MustExec(t, t.Context(), f.pool, `UPDATE computer_leases SET storage_pressure_since=clock_timestamp()-interval '301 seconds'`)
			dbtest.MustExec(t, t.Context(), f.pool, change.sql)
			for range 2 {
				if _, _, err := session.ReconcileSessionLifecycle(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, session.SessionLifecyclePosition{}); err != nil {
					t.Fatal(err)
				}
			}
			var valid bool
			if err := f.pool.QueryRow(t.Context(), `SELECT
    (SELECT status='running' FROM turns WHERE id=$1)
    AND NOT EXISTS(SELECT 1 FROM session_events WHERE kind='session.storage_pressure')
    AND NOT EXISTS(SELECT 1 FROM session_holds WHERE kind='storage_pressure')`, running.TurnID).Scan(&valid); err != nil || !valid {
				t.Fatalf("pressure preempted lost execution: %v %v", valid, err)
			}
			if err := dispatch.ExpireLease(t.Context(), auditpolicy.Policy{
				RetentionSeconds: 3600,
			}, f.pool, f.env, f.computer, 1); err != nil {
				t.Fatal(err)
			}
			if err := f.pool.QueryRow(t.Context(), `SELECT
    (SELECT status='interrupted' FROM turns WHERE id=$1)
    AND (SELECT status='queued' FROM turns WHERE id=$2)
    AND (SELECT count(*)=1 FROM session_events WHERE turn_id=$1 AND kind='turn.interrupted' AND convert_from(data,'UTF8')::jsonb->>'reason'='computer_execution_lost')`, running.TurnID, queued.TurnID).Scan(&valid); err != nil || !valid {
				t.Fatalf("lost execution outcome: %v %v", valid, err)
			}
		})
	}
}

func TestStoragePressureLossClearsHoldWithoutRecoveryPromise(t *testing.T) {
	f := newFixture(t)
	running := f.enqueue(t, "running")
	if _, err := dispatchTurnForTest(t.Context(), f.pool, f.execution()); err != nil {
		t.Fatal(err)
	}
	if err := session.ReportStoragePressure(t.Context(), auditpolicy.Policy{
		RetentionSeconds: 3600,
	}, f.pool, *f.host(), saveIdentity(f), true); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE computer_leases SET storage_pressure_since=clock_timestamp()-interval '31 seconds'`)
	if _, _, err := session.ReconcileSessionLifecycle(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, session.SessionLifecyclePosition{}); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE computer_leases SET expires_at=clock_timestamp()-interval '1 second'`)
	for range 2 {
		if err := dispatch.ExpireLease(t.Context(), auditpolicy.Policy{
			RetentionSeconds: 3600,
		}, f.pool, f.env, f.computer, 1); err != nil {
			t.Fatal(err)
		}
	}
	var valid bool
	if err := f.pool.QueryRow(t.Context(), `SELECT
  (SELECT count(*)=1 FROM session_holds WHERE kind='storage_pressure' AND released_at IS NOT NULL)
  AND (SELECT count(*)=1 FROM session_events WHERE kind='session.storage_pressure' AND convert_from(data,'UTF8')::jsonb->>'state'='paused')
  AND NOT EXISTS(SELECT 1 FROM session_events WHERE kind='session.storage_pressure' AND convert_from(data,'UTF8')::jsonb->>'state'='resolved')
  AND (SELECT count(*)=1 FROM session_events WHERE turn_id=$1 AND kind='turn.interrupted' AND convert_from(data,'UTF8')::jsonb->>'reason'='computer_execution_lost')`, running.TurnID).Scan(&valid); err != nil || !valid {
		t.Fatalf("loss promised recovered storage: %v %v", valid, err)
	}
}
