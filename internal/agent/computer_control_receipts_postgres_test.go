package agent

import (
	"errors"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"testing"
	"uuid"

	agentv1 "github.com/helmrdotdev/helmr/internal/proto/agent/v1"
	"github.com/helmrdotdev/helmr/internal/workergroup"
)

// Exercise the durable host receipt API; no guest/VM execution is claimed.
func acknowledgeComputerMembers(t *testing.T, f fixture, host workergroup.HostPrincipal, p *agentv1.ComputerSessionInstallation) {
	t.Helper()
	for _, g := range p.Grants {
		e := f.execution()
		e.SessionID = uuid.MustParse(g.Identity.SessionId)
		e.ProcessEpoch = g.Identity.ProcessEpoch
		e.LeaseEpoch = g.ComputerLeaseEpoch
		e.WorkerHostID = host.HostID
		e.WorkerEpoch = host.Epoch
		var stopped bool
		if err := f.pool.QueryRow(t.Context(), `SELECT status='stopped' AND fenced_at IS NOT NULL FROM session_processes WHERE environment_id=$1 AND session_id=$2 AND epoch=$3`, f.env, e.SessionID, e.ProcessEpoch).Scan(&stopped); err != nil {
			t.Fatal(err)
		}
		if stopped {
			continue
		}
		a, err := AcquireRuntimeAttachment(t.Context(), f.pool, host, e)
		if err != nil {
			t.Fatal(err)
		}
		d, err := PrepareSessionDelivery(t.Context(), f.pool, host, e, a.Sequence)
		if err != nil {
			t.Fatal(err)
		}
		if d.Error != "" {
			if err := ObserveSessionFailure(t.Context(), f.pool, host, e, a.Sequence); err != nil {
				t.Fatal(err)
			}
			d, err = PrepareSessionDelivery(t.Context(), f.pool, host, e, a.Sequence)
			if err != nil || d.Kind != "shutdown" || d.Error != "" {
				t.Fatalf("failure did not request stop: %+v %v", d, err)
			}
		}
		if err := AcknowledgeSessionDelivery(t.Context(), f.pool, host, e, a.Sequence, d.Sequence, d.Generation, d.Kind, ""); err != nil {
			t.Fatal(err)
		}
		if d.Kind == "shutdown" {
			if err := ObserveSessionStopped(t.Context(), f.pool, host, e, a.Sequence); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestComputerCompletionRequiresDurableReceipts(t *testing.T) {
	for _, restore := range []bool{false, true} {
		name := "source"
		if restore {
			name = "target"
		}
		for _, scenario := range []string{"missing", "error", "attachment", "generation", "kind", "shutdown", "pre-activation"} {
			t.Run(name+"/"+scenario, func(t *testing.T) {
				var f fixture
				var host workergroup.HostPrincipal
				var p *agentv1.ComputerSessionInstallation
				var commit func() error
				var complete func() error
				if restore {
					target := newComputerRestoreFixture(t)
					f, host = target.f, target.host
					p = target.prepare(t)
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
				if scenario == "pre-activation" {
					acknowledgeComputerMembers(t, f, host, p)
				}
				if err := commit(); err != nil {
					t.Fatal(err)
				}
				if scenario != "missing" && scenario != "pre-activation" {
					acknowledgeComputerMembers(t, f, host, p)
				}
				// Fault injection verifies completion consults the stored process receipt.
				switch scenario {
				case "error":
					dbtest.MustExec(t, t.Context(), f.pool, `UPDATE session_processes SET control_acknowledged_at=NULL,control_error='failed application' WHERE environment_id=$1 AND session_id=$2`, f.env, f.session)
				case "attachment":
					dbtest.MustExec(t, t.Context(), f.pool, `UPDATE session_processes SET attachment_sequence=attachment_sequence+1 WHERE environment_id=$1 AND session_id=$2`, f.env, f.session)
				case "generation":
					dbtest.MustExec(t, t.Context(), f.pool, `UPDATE session_processes SET control_generation=0 WHERE environment_id=$1 AND session_id=$2`, f.env, f.session)
				case "kind":
					dbtest.MustExec(t, t.Context(), f.pool, `UPDATE session_processes SET control_kind='suspend' WHERE environment_id=$1 AND session_id=$2`, f.env, f.session)
				case "shutdown":
					dbtest.MustExec(t, t.Context(), f.pool, `UPDATE sessions SET status='cancelled' WHERE environment_id=$1 AND id=$2`, f.env, f.session)
					dbtest.MustExec(t, t.Context(), f.pool, `UPDATE session_processes SET control_kind='shutdown' WHERE environment_id=$1 AND session_id=$2`, f.env, f.session)
				}
				if err := complete(); !errors.Is(err, ErrNotReady) {
					t.Fatalf("incomplete control admitted: %v", err)
				}
				acknowledgeComputerMembers(t, f, host, p)
				if err := commit(); err != nil {
					t.Fatal(err)
				}
				if err := complete(); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

func TestSourceContinuationRejectsDelayedPreFreezeReceipt(t *testing.T) {
	f := newFixture(t)
	a, err := AcquireRuntimeAttachment(t.Context(), f.pool, *f.host(), f.execution())
	if err != nil {
		t.Fatal(err)
	}
	old, err := PrepareSessionDelivery(t.Context(), f.pool, *f.host(), f.execution(), a.Sequence)
	if err != nil {
		t.Fatal(err)
	}
	p, _ := sourceAbort(t, f)
	if err := ValidateComputerSourceAbort(t.Context(), f.pool, *f.host(), f.env, p, abortReceipt(p, false, false)); err != nil {
		t.Fatal(err)
	}
	if err := CommitComputerSourceAbort(t.Context(), f.pool, *f.host(), f.env, p, abortReceipt(p, true, false)); err != nil {
		t.Fatal(err)
	}
	if err := AcknowledgeSessionDelivery(t.Context(), f.pool, *f.host(), f.execution(), a.Sequence, old.Sequence, old.Generation, old.Kind, ""); !errors.Is(err, ErrConflict) {
		t.Fatalf("delayed pre-freeze receipt: %v", err)
	}
	current, err := PrepareSessionDelivery(t.Context(), f.pool, *f.host(), f.execution(), a.Sequence)
	if err != nil || current.Sequence <= old.Sequence || current.Acknowledged {
		t.Fatalf("current delivery: %+v %v", current, err)
	}
	if err := AcknowledgeSessionDelivery(t.Context(), f.pool, *f.host(), f.execution(), a.Sequence, current.Sequence, current.Generation, current.Kind, ""); err != nil {
		t.Fatal(err)
	}
	// An uncertain activation-commit reply retries without invalidating the new receipt.
	if err := CommitComputerSourceAbort(t.Context(), f.pool, *f.host(), f.env, p, abortReceipt(p, true, false)); err != nil {
		t.Fatal(err)
	}
	if err := CompleteComputerSourceAbort(t.Context(), f.pool, *f.host(), f.env, p, abortReceipt(p, true, true)); err != nil {
		t.Fatal(err)
	}
}

func TestSourceContinuationCompletesAfterStoppedPeerGenerationAdvances(t *testing.T) {
	f := newFixture(t)
	peer := f.peer(t)
	held, err := ControlSession(t.Context(), f.pool, f.caller(), controlRequest(peer, "interrupt", "held-peer"))
	if err != nil {
		t.Fatal(err)
	}
	p, _ := sourceAbort(t, f)
	if err := ValidateComputerSourceAbort(t.Context(), f.pool, *f.host(), f.env, p, abortReceipt(p, false, false)); err != nil {
		t.Fatal(err)
	}
	if err := CommitComputerSourceAbort(t.Context(), f.pool, *f.host(), f.env, p, abortReceipt(p, true, false)); err != nil {
		t.Fatal(err)
	}
	if _, err := ControlSession(t.Context(), f.pool, f.caller(), controlRequest(peer, "cancel", "cancel-peer")); err != nil {
		t.Fatal(err)
	}
	acknowledgeComputerMembers(t, f, *f.host(), p)
	release := controlRequest(peer, "resume", "release-stopped-peer")
	release.HoldID = held.HoldID
	if _, err := ControlSession(t.Context(), f.pool, f.caller(), release); err != nil {
		t.Fatal(err)
	}
	if _, err := RenewRuntimeAuthority(t.Context(), f.pool, *f.host(), peer.execution()); !errors.Is(err, ErrNotReady) {
		t.Fatalf("stopped peer still obtains runtime authority: %v", err)
	}
	if err := CompleteComputerSourceAbort(t.Context(), f.pool, *f.host(), f.env, p, abortReceipt(p, true, true)); err != nil {
		t.Fatal(err)
	}
	queued := f.enqueue(t, "healthy-after-stopped-peer")
	dispatched, err := Dispatch(t.Context(), f.pool, f.execution())
	if err != nil || dispatched.TurnID != queued.TurnID {
		t.Fatalf("healthy peer dispatch: %+v %v", dispatched, err)
	}
}
