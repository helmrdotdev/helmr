package session_test

import (
	"errors"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/session"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	agentv1 "github.com/helmrdotdev/helmr/internal/proto/agent/v1"
	"github.com/helmrdotdev/helmr/internal/workergroup"
)

func TestSessionRuntimeFailureIsDurableAndLocal(t *testing.T) {
	for _, alreadyInterrupted := range []bool{false, true} {
		name := "running"
		if alreadyInterrupted {
			name = "publicly-interrupted"
		}
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			peer := f.peer(t)
			active := f.enqueue(t, "active")
			if _, err := dispatchTurnForTest(t.Context(), f.pool, f.execution()); err != nil {
				t.Fatal(err)
			}
			queued := f.enqueue(t, "queued")
			a, err := session.AcquireRuntimeAttachment(t.Context(), f.pool, *f.host(), f.execution())
			if err != nil {
				t.Fatal(err)
			}
			// A public interrupt is independent of the failed process's report.
			if alreadyInterrupted {
				if _, err = session.ControlSession(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, f.caller(), controlRequest(f, "interrupt", "public-stop")); err != nil {
					t.Fatal(err)
				}
			}
			for range 2 {
				if err = session.ObserveSessionFailure(t.Context(), auditpolicy.Policy{
					RetentionSeconds: 3600,
				}, f.pool, *f.host(), f.execution(), a.Sequence); err != nil {
					t.Fatal(err)
				}
			}
			var state string
			for id, want := range map[uuid.UUID]string{active.TurnID: "interrupted", queued.TurnID: "queued"} {
				if err = f.pool.QueryRow(t.Context(), `SELECT status FROM turns WHERE id=$1`, id).Scan(&state); err != nil || state != want {
					t.Fatalf("turn %s: %s %v", id, state, err)
				}
			}
			var held, unfenced bool
			if err = f.pool.QueryRow(t.Context(), `SELECT failure_recorded_at IS NOT NULL,status='stopping' AND fenced_at IS NULL FROM session_processes WHERE session_id=$1`, f.session).Scan(&held, &unfenced); err != nil || !held || !unfenced {
				t.Fatalf("failure intent: %v %v %v", held, unfenced, err)
			}
			// One failure is reported once and does not hold the Session.
			failures := func() (holds, events int) {
				t.Helper()
				if err := f.pool.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM session_holds WHERE kind='process_failed'),(SELECT count(*) FROM session_events WHERE session_id=$1 AND kind='session.process_failed')`, f.session).Scan(&holds, &events); err != nil {
					t.Fatal(err)
				}
				return holds, events
			}
			if holds, events := failures(); holds != 0 || events != 1 {
				t.Fatalf("failure holds %d events %d", holds, events)
			}
			next := peer.enqueue(t, "peer")
			if got, err := dispatchTurnForTest(t.Context(), peer.pool, peer.execution()); err != nil || got.TurnID != next.TurnID {
				t.Fatalf("peer: %+v %v", got, err)
			}
			if err = session.ObserveSessionStopped(t.Context(), auditpolicy.Policy{
				RetentionSeconds: 3600,
			}, f.pool, *f.host(), f.execution(), a.Sequence); err != nil {
				t.Fatal(err)
			}
			if holds, events := failures(); holds != 0 || events != 1 {
				t.Fatalf("physical stop recorded another failure: holds %d events %d", holds, events)
			}
		})
	}
}

func TestSessionRuntimeFailurePreservesResult(t *testing.T) {
	f := newFixture(t)
	result, save := f.finalize(t, "result")
	a, err := session.AcquireRuntimeAttachment(t.Context(), f.pool, *f.host(), f.execution())
	if err != nil {
		t.Fatal(err)
	}
	if err = session.ObserveSessionFailure(t.Context(), auditpolicy.Policy{
		RetentionSeconds: 3600,
	}, f.pool, *f.host(), f.execution(), a.Sequence); err != nil {
		t.Fatal(err)
	}
	storage := newSaveStorageFixture(t, f)
	cut, root := storage.cut(t, 2)
	f.capture(t, save, root)
	if err = storage.publish(t, save.ID, cut); err != nil {
		t.Fatal(err)
	}
	if err = completedTurnForTest(t.Context(), f.pool, f.env, f.session, result.TurnID); err != nil {
		t.Fatal(err)
	}
	var recorded bool
	if err = f.pool.QueryRow(t.Context(), `SELECT failure_reason='process_failed' FROM session_processes WHERE session_id=$1 AND epoch=1`, f.session).Scan(&recorded); err != nil || !recorded {
		t.Fatalf("completion cleared failure fact %v %v", recorded, err)
	}
}

func TestSessionRuntimeFailureRejectsObsoleteAttachment(t *testing.T) {
	f := newFixture(t)
	a, err := session.AcquireRuntimeAttachment(t.Context(), f.pool, *f.host(), f.execution())
	if err != nil {
		t.Fatal(err)
	}
	newer, err := session.AcquireRuntimeAttachment(t.Context(), f.pool, *f.host(), f.execution())
	if err != nil {
		t.Fatal(err)
	}
	if err = session.ObserveSessionFailure(t.Context(), auditpolicy.Policy{
		RetentionSeconds: 3600,
	}, f.pool, *f.host(), f.execution(), a.Sequence); !errors.Is(err, session.ErrDenied) {
		t.Fatalf("obsolete attachment: %v", err)
	}
	if err = session.ObserveSessionFailure(t.Context(), auditpolicy.Policy{
		RetentionSeconds: 3600,
	}, f.pool, *f.host(), f.execution(), newer.Sequence); err != nil {
		t.Fatal(err)
	}
}

func TestSessionRuntimeFailureSurvivesControlReattachment(t *testing.T) {
	f := newFixture(t)
	a, err := session.AcquireRuntimeAttachment(t.Context(), f.pool, *f.host(), f.execution())
	if err != nil {
		t.Fatal(err)
	}
	original, err := session.PrepareSessionDelivery(t.Context(), f.pool, *f.host(), f.execution(), a.Sequence)
	if err != nil {
		t.Fatal(err)
	}
	if err = session.AcknowledgeSessionDelivery(t.Context(), f.pool, *f.host(), f.execution(), a.Sequence, original.Sequence, original.Generation, original.Kind, "failed native resume", 0); err != nil {
		t.Fatal(err)
	}
	next, err := session.AcquireRuntimeAttachment(t.Context(), f.pool, *f.host(), f.execution())
	if err != nil {
		t.Fatal(err)
	}
	got, err := session.PrepareSessionDelivery(t.Context(), f.pool, *f.host(), f.execution(), next.Sequence)
	if err != nil || got.Error != "failed native resume" || got.Sequence != original.Sequence {
		t.Fatalf("reattachment erased failure: %+v %v", got, err)
	}
	if err = session.ObserveSessionFailure(t.Context(), auditpolicy.Policy{
		RetentionSeconds: 3600,
	}, f.pool, *f.host(), f.execution(), next.Sequence); err != nil {
		t.Fatal(err)
	}
	stop, err := session.PrepareSessionDelivery(t.Context(), f.pool, *f.host(), f.execution(), next.Sequence)
	if err != nil || stop.Kind != "shutdown" || stop.Error != "" || stop.Sequence <= got.Sequence {
		t.Fatalf("recorded failure did not allow stop: %+v %v", stop, err)
	}
}

func TestSessionRuntimeFailureRejectsExpiredLease(t *testing.T) {
	f := newFixture(t)
	active := f.enqueue(t, "active")
	if _, err := dispatchTurnForTest(t.Context(), f.pool, f.execution()); err != nil {
		t.Fatal(err)
	}
	a, err := session.AcquireRuntimeAttachment(t.Context(), f.pool, *f.host(), f.execution())
	if err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE computer_leases SET expires_at=clock_timestamp()-interval '1 second'`)
	if err = session.ObserveSessionFailure(t.Context(), auditpolicy.Policy{
		RetentionSeconds: 3600,
	}, f.pool, *f.host(), f.execution(), a.Sequence); !errors.Is(err, session.ErrDenied) {
		t.Fatalf("expired failure accepted: %v", err)
	}
	var recorded, unfenced bool
	if err = f.pool.QueryRow(t.Context(), `SELECT failure_recorded_at IS NOT NULL,fenced_at IS NULL FROM session_processes WHERE session_id=$1`, f.session).Scan(&recorded, &unfenced); err != nil || recorded || !unfenced {
		t.Fatalf("failure state %v %v: %v", recorded, unfenced, err)
	}
	var state string
	if err = f.pool.QueryRow(t.Context(), `SELECT status FROM turns WHERE id=$1`, active.TurnID).Scan(&state); err != nil || state != "running" {
		t.Fatalf("turn %s: %v", state, err)
	}
	var holds int
	if err = f.pool.QueryRow(t.Context(), `SELECT count(*) FROM session_holds`).Scan(&holds); err != nil || holds != 0 {
		t.Fatalf("unauthorized hold %d: %v", holds, err)
	}
}

func TestSessionRuntimeFailureSurvivesContinuationCommit(t *testing.T) {
	for _, restore := range []bool{false, true} {
		name := "source"
		if restore {
			name = "target"
		}
		t.Run(name, func(t *testing.T) {
			var f fixture
			var host workergroup.HostPrincipal
			var p *agentv1.ComputerSessionInstallation
			var commit func() error
			if restore {
				r := newComputerRestoreFixture(t)
				f, host = r.f, r.host
				p = r.prepare(t)
				if err := session.ValidateComputerRestore(t.Context(), f.pool, host, f.env, p, restoreReceipt(p, false, false)); err != nil {
					t.Fatal(err)
				}
				commit = func() error {
					return session.CommitComputerRestore(t.Context(), f.pool, host, f.env, p, restoreReceipt(p, true, false))
				}
			} else {
				f = newFixture(t)
				host = *f.host()
				p, _ = sourceAbort(t, f)
				if err := session.ValidateComputerSourceAbort(t.Context(), f.pool, host, f.env, p, abortReceipt(p, false, false)); err != nil {
					t.Fatal(err)
				}
				commit = func() error {
					return session.CommitComputerSourceAbort(t.Context(), f.pool, host, f.env, p, abortReceipt(p, true, false))
				}
			}
			e := f.execution()
			e.WorkerHostID = host.HostID
			e.WorkerEpoch = host.Epoch
			e.LeaseEpoch = p.Grants[0].ComputerLeaseEpoch
			a, err := session.AcquireRuntimeAttachment(t.Context(), f.pool, host, e)
			if err != nil {
				t.Fatal(err)
			}
			d, err := session.PrepareSessionDelivery(t.Context(), f.pool, host, e, a.Sequence)
			if err != nil {
				t.Fatal(err)
			}
			if err = session.AcknowledgeSessionDelivery(t.Context(), f.pool, host, e, a.Sequence, d.Sequence, d.Generation, d.Kind, "retained rejection", 0); err != nil {
				t.Fatal(err)
			}
			if err = commit(); err != nil {
				t.Fatal(err)
			}
			got, err := session.PrepareSessionDelivery(t.Context(), f.pool, host, e, a.Sequence)
			if err != nil || got.Error != "retained rejection" || got.Kind != d.Kind || got.Sequence != d.Sequence {
				t.Fatalf("continuation erased failure: %+v %v", got, err)
			}
			if err = session.ObserveSessionFailure(t.Context(), auditpolicy.Policy{
				RetentionSeconds: 3600,
			}, f.pool, host, e, a.Sequence); err != nil {
				t.Fatal(err)
			}
			got, err = session.PrepareSessionDelivery(t.Context(), f.pool, host, e, a.Sequence)
			if err != nil || got.Kind != "shutdown" || got.Error != "" {
				t.Fatalf("pending failure did not reconcile: %+v %v", got, err)
			}
		})
	}
}
