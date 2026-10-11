package agent

import (
	"errors"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	agentv1 "github.com/helmrdotdev/helmr/internal/proto/agent/v1"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"testing"
	"uuid"
)

func TestUnexpectedSessionStopHoldsOnlyFailedSession(t *testing.T) {
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
	for range 2 {
		if err = ObserveSessionStopped(t.Context(), f.pool, *f.host(), f.execution(), a.Sequence); err != nil {
			t.Fatal(err)
		}
	}
	var state string
	for id, want := range map[uuid.UUID]string{active.TurnID: "interrupted", queued.TurnID: "queued"} {
		if err = f.pool.QueryRow(t.Context(), `SELECT status FROM turns WHERE id=$1`, id).Scan(&state); err != nil || state != want {
			t.Fatalf("turn %s: %s %v", id, state, err)
		}
	}
	var hold uuid.UUID
	var count int
	if err = f.pool.QueryRow(t.Context(), `SELECT count(*) FROM session_holds`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("holds %d: %v", count, err)
	}
	if err = f.pool.QueryRow(t.Context(), `SELECT id FROM session_holds WHERE session_id=$1 AND scope='local' AND issuer_kind='system'`, f.session).Scan(&hold); err != nil {
		t.Fatal(err)
	}
	if _, err = RenewRuntimeAuthority(t.Context(), f.pool, *f.host(), f.execution()); !errors.Is(err, ErrNotReady) {
		t.Fatalf("stopped authority: %v", err)
	}
	next := peer.enqueue(t, "healthy-peer")
	if got, err := Dispatch(t.Context(), peer.pool, peer.execution()); err != nil || got.TurnID != next.TurnID {
		t.Fatalf("peer dispatch %+v: %v", got, err)
	}
	req := controlRequest(f, "resume", "release-process-failure")
	req.HoldID = hold
	if _, err = ControlSession(t.Context(), f.pool, f.caller(), req); err != nil {
		t.Fatal(err)
	}
	if err = ObserveSessionStopped(t.Context(), f.pool, *f.host(), f.execution(), a.Sequence); err != nil {
		t.Fatal(err)
	}
	if err = f.pool.QueryRow(t.Context(), `SELECT count(*) FROM session_holds WHERE released_at IS NULL`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("retry recreated hold %d: %v", count, err)
	}
}

func TestUnexpectedSessionStopPreservesRecordedResult(t *testing.T) {
	f := newFixture(t)
	result, save := f.finalize(t, "result")
	a, err := AcquireRuntimeAttachment(t.Context(), f.pool, *f.host(), f.execution())
	if err != nil {
		t.Fatal(err)
	}
	if err = ObserveSessionStopped(t.Context(), f.pool, *f.host(), f.execution(), a.Sequence); err != nil {
		t.Fatal(err)
	}
	var state string
	if err = f.pool.QueryRow(t.Context(), `SELECT status FROM turns WHERE id=$1`, result.TurnID).Scan(&state); err != nil || state != "finalizing" {
		t.Fatalf("recorded result: %s %v", state, err)
	}
	if err = f.pool.QueryRow(t.Context(), `SELECT status FROM computer_saves WHERE id=$1`, save.ID).Scan(&state); err != nil || state != "requested" {
		t.Fatalf("save: %s %v", state, err)
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
	if err = f.pool.QueryRow(t.Context(), `SELECT EXISTS(SELECT 1 FROM session_holds WHERE session_id=$1 AND released_at IS NULL)`, f.session).Scan(&held); err != nil || !held {
		t.Fatalf("completion cleared loss hold: %v %v", held, err)
	}
}

func TestContinuationCompletesAfterUnexpectedMemberStop(t *testing.T) {
	for _, target := range []bool{false, true} {
		name := "source"
		if target {
			name = "target"
		}
		t.Run(name, func(t *testing.T) {
			var f fixture
			var host workergroup.HostPrincipal
			var p *agentv1.ComputerSessionInstallation
			var commit, complete func() error
			var e Execution
			if target {
				r := newComputerRestoreFixture(t)
				f, host = r.f, r.host
				p = r.prepare(t)
				e = r.execution()
				if err := ValidateComputerRestore(t.Context(), f.pool, host, f.env, p, restoreReceipt(p, false, false)); err != nil {
					t.Fatal(err)
				}
				commit = func() error {
					return CommitComputerRestore(t.Context(), f.pool, host, f.env, p, restoreReceipt(p, true, false))
				}
				complete = func() error {
					return CompleteComputerRestore(t.Context(), f.pool, host, f.env, p, restoreReceipt(p, true, true))
				}
			} else {
				f = newFixture(t)
				host = *f.host()
				p, _ = sourceAbort(t, f)
				e = f.execution()
				if err := ValidateComputerSourceAbort(t.Context(), f.pool, host, f.env, p, abortReceipt(p, false, false)); err != nil {
					t.Fatal(err)
				}
				commit = func() error {
					return CommitComputerSourceAbort(t.Context(), f.pool, host, f.env, p, abortReceipt(p, true, false))
				}
				complete = func() error {
					return CompleteComputerSourceAbort(t.Context(), f.pool, host, f.env, p, abortReceipt(p, true, true))
				}
			}
			if err := commit(); err != nil {
				t.Fatal(err)
			}
			a, err := AcquireRuntimeAttachment(t.Context(), f.pool, host, e)
			if err != nil {
				t.Fatal(err)
			}
			if err = ObserveSessionStopped(t.Context(), f.pool, host, e, a.Sequence); err != nil {
				t.Fatal(err)
			}
			if _, err = RenewRuntimeAuthority(t.Context(), f.pool, host, e); !errors.Is(err, ErrNotReady) {
				t.Fatalf("stopped member renewed: %v", err)
			}
			if _, err = PrepareSessionDelivery(t.Context(), f.pool, host, e, a.Sequence); !errors.Is(err, ErrNotReady) {
				t.Fatalf("stopped member delivery: %v", err)
			}
			observed := abortReceipt(p, true, false)
			if target {
				observed = restoreReceipt(p, true, false)
			}
			controls, err := ReadComputerContinuationControls(t.Context(), f.pool, host, f.env, p, observed)
			if err != nil {
				t.Fatal(err)
			}
			for _, c := range controls.Sessions {
				if c.Identity.SessionId == f.session.String() && (!c.Stopped || !c.Held) {
					t.Fatalf("failed process resumed: %+v", c)
				}
			}
			acknowledgeComputerMembers(t, f, host, p)
			if err = complete(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestUnexpectedSessionStopRejectsSupersededAttachment(t *testing.T) {
	f := newFixture(t)
	old, err := AcquireRuntimeAttachment(t.Context(), f.pool, *f.host(), f.execution())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = AcquireRuntimeAttachment(t.Context(), f.pool, *f.host(), f.execution()); err != nil {
		t.Fatal(err)
	}
	if err = ObserveSessionStopped(t.Context(), f.pool, *f.host(), f.execution(), old.Sequence); !errors.Is(err, ErrDenied) {
		t.Fatalf("old attachment stopped process: %v", err)
	}
	var unchanged bool
	if err = f.pool.QueryRow(t.Context(), `SELECT p.status='ready' AND p.fenced_at IS NULL AND s.authority_generation=1 AND NOT EXISTS(SELECT 1 FROM session_holds) FROM session_processes p JOIN sessions s ON s.id=p.session_id WHERE s.id=$1`, f.session).Scan(&unchanged); err != nil || !unchanged {
		t.Fatalf("rejected stop changed state: %v %v", unchanged, err)
	}
}

func TestRevokedSessionStopInterruptsRunningTurn(t *testing.T) {
	f := newFixture(t)
	active := f.enqueue(t, "active")
	if _, err := Dispatch(t.Context(), f.pool, f.execution()); err != nil {
		t.Fatal(err)
	}
	a, err := AcquireRuntimeAttachment(t.Context(), f.pool, *f.host(), f.execution())
	if err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE deployments SET execution_revoked_at=clock_timestamp()`)
	if err = ObserveSessionStopped(t.Context(), f.pool, *f.host(), f.execution(), a.Sequence); err != nil {
		t.Fatal(err)
	}
	var state string
	if err = f.pool.QueryRow(t.Context(), `SELECT status FROM turns WHERE id=$1`, active.TurnID).Scan(&state); err != nil || state != "interrupted" {
		t.Fatalf("revoked running Turn: %s %v", state, err)
	}
}

func TestSourcePreparationStopsFailedMemberAndResumesHealthyPeer(t *testing.T) {
	f := newFixture(t)
	peer := f.peer(t)
	p, _ := sourceAbort(t, f)
	a, err := AcquireRuntimeAttachment(t.Context(), f.pool, *f.host(), peer.execution())
	if err != nil {
		t.Fatal(err)
	}
	if err = ObserveSessionStopped(t.Context(), f.pool, *f.host(), peer.execution(), a.Sequence); err != nil {
		t.Fatal(err)
	}
	initial := abortReceipt(p, false, false)
	p, err = PrepareComputerSourceAbort(t.Context(), f.pool, *f.host(), f.env, uuid.MustParse(p.Capture.CheckpointId), 1, p.Envelope.ChannelCredential, initial)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.StoppedSessions) != 1 || p.StoppedSessions[0].SessionId != peer.session.String() {
		t.Fatalf("stop set: %v", p.StoppedSessions)
	}
	if err = ValidateComputerSourceAbort(t.Context(), f.pool, *f.host(), f.env, p, initial); err != nil {
		t.Fatal(err)
	}
	if err = CommitComputerSourceAbort(t.Context(), f.pool, *f.host(), f.env, p, abortReceipt(p, true, false)); err != nil {
		t.Fatal(err)
	}
	controls, err := ReadComputerContinuationControls(t.Context(), f.pool, *f.host(), f.env, p, abortReceipt(p, true, false))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range controls.Sessions {
		if c.Identity.SessionId == f.session.String() && (c.Stopped || c.Held) {
			t.Fatal("healthy peer held")
		}
	}
	acknowledgeComputerMembers(t, f, *f.host(), p)
	if err = CompleteComputerSourceAbort(t.Context(), f.pool, *f.host(), f.env, p, abortReceipt(p, true, true)); err != nil {
		t.Fatal(err)
	}
	next := f.enqueue(t, "healthy")
	if got, err := Dispatch(t.Context(), f.pool, f.execution()); err != nil || got.TurnID != next.TurnID {
		t.Fatalf("peer continuation: %+v %v", got, err)
	}
}
