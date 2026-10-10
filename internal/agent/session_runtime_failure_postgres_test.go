package agent

import (
	"errors"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	agentv1 "github.com/helmrdotdev/helmr/internal/proto/agent/v1"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"testing"
	"uuid"
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
			if _, err := Dispatch(t.Context(), f.pool, f.execution()); err != nil {
				t.Fatal(err)
			}
			queued := f.enqueue(t, "queued")
			a, err := AcquireRuntimeAttachment(t.Context(), f.pool, *f.host(), f.execution())
			if err != nil {
				t.Fatal(err)
			}
			// A public subtree hold is distinct from the failed process's local hold.
			if alreadyInterrupted {
				if _, err = ControlSession(t.Context(), f.pool, f.caller(), controlRequest(f, "interrupt", "public-stop")); err != nil {
					t.Fatal(err)
				}
			}
			for range 2 {
				if err = ObserveSessionFailure(t.Context(), f.pool, *f.host(), f.execution(), a.Sequence); err != nil {
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
			var holds int
			var hold uuid.UUID
			if err = f.pool.QueryRow(t.Context(), `SELECT count(*) FROM session_holds WHERE scope='local' AND reason='Session runtime control failed'`).Scan(&holds); err != nil || holds != 1 {
				t.Fatalf("local holds %d %v", holds, err)
			}
			if err = f.pool.QueryRow(t.Context(), `SELECT id FROM session_holds WHERE scope='local' AND reason='Session runtime control failed'`).Scan(&hold); err != nil {
				t.Fatal(err)
			}
			req := controlRequest(f, "resume", "release-failure")
			req.HoldID = hold
			if _, err = ControlSession(t.Context(), f.pool, f.caller(), req); err != nil {
				t.Fatal(err)
			}
			if err = ObserveSessionFailure(t.Context(), f.pool, *f.host(), f.execution(), a.Sequence); err != nil {
				t.Fatal(err)
			}
			if err = f.pool.QueryRow(t.Context(), `SELECT count(*) FROM session_holds WHERE scope='local' AND released_at IS NULL`).Scan(&holds); err != nil || holds != 0 {
				t.Fatalf("retry recreated local hold: %d %v", holds, err)
			}
			next := peer.enqueue(t, "peer")
			if got, err := Dispatch(t.Context(), peer.pool, peer.execution()); err != nil || got.TurnID != next.TurnID {
				t.Fatalf("peer: %+v %v", got, err)
			}
			if err = ObserveSessionStopped(t.Context(), f.pool, *f.host(), f.execution(), a.Sequence); err != nil {
				t.Fatal(err)
			}
			if err = f.pool.QueryRow(t.Context(), `SELECT count(*) FROM session_holds WHERE scope='local' AND released_at IS NULL`).Scan(&holds); err != nil || holds != 0 {
				t.Fatalf("physical stop recreated local hold: %d %v", holds, err)
			}
		})
	}
}

func TestSessionRuntimeFailurePreservesResult(t *testing.T) {
	f := newFixture(t)
	result, save := f.finalize(t, "result")
	a, err := AcquireRuntimeAttachment(t.Context(), f.pool, *f.host(), f.execution())
	if err != nil {
		t.Fatal(err)
	}
	if err = ObserveSessionFailure(t.Context(), f.pool, *f.host(), f.execution(), a.Sequence); err != nil {
		t.Fatal(err)
	}
	storage := newSaveStorageFixture(t, f)
	cut, root := storage.cut(t, 2)
	f.capture(t, save, root)
	if err = storage.publish(t, save.ID, cut); err != nil {
		t.Fatal(err)
	}
	if err = Complete(t.Context(), f.pool, f.env, f.session, result.TurnID); err != nil {
		t.Fatal(err)
	}
	var held bool
	if err = f.pool.QueryRow(t.Context(), `SELECT EXISTS(SELECT 1 FROM session_holds WHERE session_id=$1 AND scope='local' AND released_at IS NULL)`, f.session).Scan(&held); err != nil || !held {
		t.Fatalf("completion cleared failure hold %v %v", held, err)
	}
}

func TestSessionRuntimeFailureRejectsObsoleteAttachment(t *testing.T) {
	f := newFixture(t)
	a, err := AcquireRuntimeAttachment(t.Context(), f.pool, *f.host(), f.execution())
	if err != nil {
		t.Fatal(err)
	}
	newer, err := AcquireRuntimeAttachment(t.Context(), f.pool, *f.host(), f.execution())
	if err != nil {
		t.Fatal(err)
	}
	if err = ObserveSessionFailure(t.Context(), f.pool, *f.host(), f.execution(), a.Sequence); !errors.Is(err, ErrDenied) {
		t.Fatalf("obsolete attachment: %v", err)
	}
	if err = ObserveSessionFailure(t.Context(), f.pool, *f.host(), f.execution(), newer.Sequence); err != nil {
		t.Fatal(err)
	}
}

func TestSessionRuntimeFailureSurvivesControlReattachment(t *testing.T) {
	f := newFixture(t)
	a, err := AcquireRuntimeAttachment(t.Context(), f.pool, *f.host(), f.execution())
	if err != nil {
		t.Fatal(err)
	}
	original, err := PrepareSessionDelivery(t.Context(), f.pool, *f.host(), f.execution(), a.Sequence)
	if err != nil {
		t.Fatal(err)
	}
	if err = AcknowledgeSessionDelivery(t.Context(), f.pool, *f.host(), f.execution(), a.Sequence, original.Sequence, original.Generation, original.Kind, "failed native resume"); err != nil {
		t.Fatal(err)
	}
	next, err := AcquireRuntimeAttachment(t.Context(), f.pool, *f.host(), f.execution())
	if err != nil {
		t.Fatal(err)
	}
	got, err := PrepareSessionDelivery(t.Context(), f.pool, *f.host(), f.execution(), next.Sequence)
	if err != nil || got.Error != "failed native resume" || got.Sequence != original.Sequence {
		t.Fatalf("reattachment erased failure: %+v %v", got, err)
	}
	if err = ObserveSessionFailure(t.Context(), f.pool, *f.host(), f.execution(), next.Sequence); err != nil {
		t.Fatal(err)
	}
	stop, err := PrepareSessionDelivery(t.Context(), f.pool, *f.host(), f.execution(), next.Sequence)
	if err != nil || stop.Kind != "shutdown" || stop.Error != "" || stop.Sequence <= got.Sequence {
		t.Fatalf("recorded failure did not allow stop: %+v %v", stop, err)
	}
}

func TestSessionRuntimeFailureRespectsLeaseExpiryAndRevocation(t *testing.T) {
	for _, expired := range []bool{false, true} {
		name := "revoked"
		if expired {
			name = "expired"
		}
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			active := f.enqueue(t, "active")
			if _, err := Dispatch(t.Context(), f.pool, f.execution()); err != nil {
				t.Fatal(err)
			}
			a, err := AcquireRuntimeAttachment(t.Context(), f.pool, *f.host(), f.execution())
			if err != nil {
				t.Fatal(err)
			}
			if expired {
				dbtest.MustExec(t, t.Context(), f.pool, `UPDATE computer_leases SET expires_at=clock_timestamp()-interval '1 second'`)
			} else {
				dbtest.MustExec(t, t.Context(), f.pool, `UPDATE deployments SET execution_revoked_at=clock_timestamp()`)
			}
			err = ObserveSessionFailure(t.Context(), f.pool, *f.host(), f.execution(), a.Sequence)
			if expired && !errors.Is(err, ErrDenied) {
				t.Fatalf("expired failure accepted: %v", err)
			}
			if !expired && err != nil {
				t.Fatal(err)
			}
			var recorded, unfenced bool
			var state string
			if err = f.pool.QueryRow(t.Context(), `SELECT failure_recorded_at IS NOT NULL,fenced_at IS NULL FROM session_processes WHERE session_id=$1`, f.session).Scan(&recorded, &unfenced); err != nil || recorded == expired || !unfenced {
				t.Fatalf("failure state %v %v: %v", recorded, unfenced, err)
			}
			want := "interrupted"
			if expired {
				want = "running"
			}
			if err = f.pool.QueryRow(t.Context(), `SELECT status FROM turns WHERE id=$1`, active.TurnID).Scan(&state); err != nil || state != want {
				t.Fatalf("turn %s: %v", state, err)
			}
			var holds int
			if err = f.pool.QueryRow(t.Context(), `SELECT count(*) FROM session_holds`).Scan(&holds); err != nil || holds != 0 {
				t.Fatalf("unnecessary/unauthorized hold %d: %v", holds, err)
			}
		})
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
				if err := ValidateComputerRestore(t.Context(), f.pool, host, f.env, p, restoreReceipt(p, false, false)); err != nil {
					t.Fatal(err)
				}
				commit = func() error {
					return CommitComputerRestore(t.Context(), f.pool, host, f.env, p, restoreReceipt(p, true, false))
				}
			} else {
				f = newFixture(t)
				host = *f.host()
				p, _ = sourceAbort(t, f)
				if err := ValidateComputerSourceAbort(t.Context(), f.pool, host, f.env, p, abortReceipt(p, false, false)); err != nil {
					t.Fatal(err)
				}
				commit = func() error {
					return CommitComputerSourceAbort(t.Context(), f.pool, host, f.env, p, abortReceipt(p, true, false))
				}
			}
			e := f.execution()
			e.WorkerHostID = host.HostID
			e.WorkerEpoch = host.Epoch
			e.LeaseEpoch = p.Grants[0].ComputerLeaseEpoch
			a, err := AcquireRuntimeAttachment(t.Context(), f.pool, host, e)
			if err != nil {
				t.Fatal(err)
			}
			d, err := PrepareSessionDelivery(t.Context(), f.pool, host, e, a.Sequence)
			if err != nil {
				t.Fatal(err)
			}
			if err = AcknowledgeSessionDelivery(t.Context(), f.pool, host, e, a.Sequence, d.Sequence, d.Generation, d.Kind, "retained rejection"); err != nil {
				t.Fatal(err)
			}
			if err = commit(); err != nil {
				t.Fatal(err)
			}
			got, err := PrepareSessionDelivery(t.Context(), f.pool, host, e, a.Sequence)
			if err != nil || got.Error != "retained rejection" || got.Kind != d.Kind || got.Sequence != d.Sequence {
				t.Fatalf("continuation erased failure: %+v %v", got, err)
			}
			if err = ObserveSessionFailure(t.Context(), f.pool, host, e, a.Sequence); err != nil {
				t.Fatal(err)
			}
			got, err = PrepareSessionDelivery(t.Context(), f.pool, host, e, a.Sequence)
			if err != nil || got.Kind != "shutdown" || got.Error != "" {
				t.Fatalf("pending failure did not reconcile: %+v %v", got, err)
			}
		})
	}
}
