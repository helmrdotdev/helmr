package agent

import (
	"errors"
	"testing"
	"uuid"
)

func startingSessionFixture(t *testing.T) (fixture, *Allocator, Execution) {
	t.Helper()
	f, a, computer := freshComputerAllocationFixture(t)
	var session uuid.UUID
	if err := f.pool.QueryRow(t.Context(), `SELECT id FROM sessions WHERE environment_id=$1 AND computer_id=$2`, f.env, computer).Scan(&session); err != nil {
		t.Fatal(err)
	}
	r, err := a.AllocateFreshComputer(t.Context(), f.env, computer, 1, f.worker)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.AllocateSessionProcess(t.Context(), f.env, session, 1); err != nil {
		t.Fatalf("initial assignment missing: %v", err)
	}
	beforeReady := Execution{EnvironmentID: f.env, SessionID: session, ProcessEpoch: 1, LeaseEpoch: 1, WorkerHostID: f.worker, WorkerEpoch: 1, AuthorityGeneration: 1}
	if _, err := AcquireRuntimeAttachment(t.Context(), f.pool, *f.host(), beforeReady); !errors.Is(err, ErrNotReady) {
		t.Fatalf("attachment before VM readiness: %v", err)
	}
	identity := ComputerLeaseIdentity{EnvironmentID: f.env, ComputerID: computer, InstanceID: r.InstanceID, Epoch: r.Epoch}
	if _, err := a.DeliverComputer(t.Context(), *f.host(), identity); err != nil {
		t.Fatal(err)
	}
	if err := ObserveFreshComputerReady(t.Context(), f.pool, *f.host(), identity, r.Shape, r.BaseVersion); err != nil {
		t.Fatal(err)
	}
	p, err := a.AllocateSessionProcess(t.Context(), f.env, session, 1)
	if err != nil {
		t.Fatal(err)
	}
	if p.Status != "starting" || p.ComputerID != computer || p.InstanceID != r.InstanceID {
		t.Fatalf("wrong process allocation: %+v", p)
	}
	e := Execution{EnvironmentID: f.env, SessionID: session, ProcessEpoch: 1, LeaseEpoch: 1, WorkerHostID: f.worker, WorkerEpoch: 1, AuthorityGeneration: 1}
	return f, a, e
}

func TestProcessAdmissionSetupReceiptPrecedesTurnDispatch(t *testing.T) {
	f, a, e := startingSessionFixture(t)
	attachment, err := AcquireRuntimeAttachment(t.Context(), f.pool, *f.host(), e)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Dispatch(t.Context(), f.pool, e); !errors.Is(err, ErrNotReady) {
		t.Fatalf("dispatched before setup: %v", err)
	}
	if err := ObserveSessionReady(t.Context(), f.pool, *f.host(), e, attachment.Sequence+1); !errors.Is(err, ErrNotReady) {
		t.Fatalf("wrong attachment ready: %v", err)
	}
	if err := ObserveSessionReady(t.Context(), f.pool, *f.host(), e, attachment.Sequence); err != nil {
		t.Fatal(err)
	}
	if err := ObserveSessionReady(t.Context(), f.pool, *f.host(), e, attachment.Sequence); err != nil {
		t.Fatal(err)
	}
	if _, err := Dispatch(t.Context(), f.pool, e); err != nil {
		t.Fatal(err)
	}
	if _, err := a.AllocateSessionProcess(t.Context(), f.env, e.SessionID, 2); !errors.Is(err, ErrNotReady) {
		t.Fatalf("live process replaced: %v", err)
	}
	r, err := a.AllocateSessionProcess(t.Context(), f.env, e.SessionID, 1)
	if err != nil || r.Status != "ready" {
		t.Fatalf("historical receipt: %+v %v", r, err)
	}
}

func TestProcessAdmissionFailedSetupRetainsInputUntilExplicitResume(t *testing.T) {
	f, a, e := startingSessionFixture(t)
	attachment, err := AcquireRuntimeAttachment(t.Context(), f.pool, *f.host(), e)
	if err != nil {
		t.Fatal(err)
	}
	if err := ObserveSessionFailure(t.Context(), f.pool, *f.host(), e, attachment.Sequence); err != nil {
		t.Fatal(err)
	}
	if err := ObserveSessionStopped(t.Context(), f.pool, *f.host(), e, attachment.Sequence); err != nil {
		t.Fatal(err)
	}
	if _, err := a.AllocateSessionProcess(t.Context(), f.env, e.SessionID, 2); !errors.Is(err, ErrNotReady) {
		t.Fatalf("setup failure retried without resume: %v", err)
	}
	var hold uuid.UUID
	if err := f.pool.QueryRow(t.Context(), `SELECT id FROM session_holds WHERE environment_id=$1 AND session_id=$2 AND released_at IS NULL`, f.env, e.SessionID).Scan(&hold); err != nil {
		t.Fatal(err)
	}
	var queued bool
	if err := f.pool.QueryRow(t.Context(), `SELECT status='queued' AND process_epoch IS NULL AND started_at IS NULL FROM turns WHERE environment_id=$1 AND session_id=$2`, f.env, e.SessionID).Scan(&queued); err != nil || !queued {
		t.Fatalf("failed setup consumed input: %v %v", queued, err)
	}
	if _, err := ControlSession(t.Context(), f.pool, f.caller(), SessionControlRequest{EnvironmentID: f.env, SessionID: e.SessionID, Kind: "resume", RetryKey: "resume-setup", HoldID: hold}); err != nil {
		t.Fatal(err)
	}
	r, err := a.AllocateSessionProcess(t.Context(), f.env, e.SessionID, 2)
	if err != nil || r.Epoch != 2 || r.Status != "starting" {
		t.Fatalf("explicit resumed setup: %+v %v", r, err)
	}
}

func TestProcessReadinessSupersededByShutdown(t *testing.T) {
	for _, revoke := range []bool{false, true} {
		t.Run(map[bool]string{false: "cancel", true: "revocation"}[revoke], func(t *testing.T) {
			f, _, e := startingSessionFixture(t)
			attachment, err := AcquireRuntimeAttachment(t.Context(), f.pool, *f.host(), e)
			if err != nil {
				t.Fatal(err)
			}
			if revoke {
				if _, err = f.pool.Exec(t.Context(), `UPDATE deployments SET execution_revoked_at=clock_timestamp() WHERE environment_id=$1`, f.env); err != nil {
					t.Fatal(err)
				}
			} else {
				if _, err = ControlSession(t.Context(), f.pool, f.caller(), SessionControlRequest{EnvironmentID: f.env, SessionID: e.SessionID, Kind: "cancel", RetryKey: "stop-before-ready"}); err != nil {
					t.Fatal(err)
				}
			}
			if err = ObserveSessionReady(t.Context(), f.pool, *f.host(), e, attachment.Sequence+1); !errors.Is(err, ErrNotReady) {
				t.Fatalf("superseded wrong attachment accepted: %v", err)
			}
			if err = ObserveSessionReady(t.Context(), f.pool, *f.host(), e, attachment.Sequence); !errors.Is(err, ErrConflict) {
				t.Fatalf("late readiness did not retire safely: %v", err)
			}
			var status string
			if err = f.pool.QueryRow(t.Context(), `SELECT status FROM session_processes WHERE environment_id=$1 AND session_id=$2`, f.env, e.SessionID).Scan(&status); err != nil || status == "ready" {
				t.Fatalf("late readiness changed execution: %s %v", status, err)
			}
			delivery, err := PrepareSessionDelivery(t.Context(), f.pool, *f.host(), e, attachment.Sequence)
			if err != nil || delivery.Kind != "shutdown" {
				t.Fatalf("late Ready prevented shutdown: %+v %v", delivery, err)
			}
		})
	}
}

func TestProcessReadinessReplayDoesNotBlockRestoreCompletion(t *testing.T) {
	r := newComputerRestoreFixture(t)
	ctx := t.Context()
	p := r.prepare(t)
	if err := ValidateComputerRestore(ctx, r.f.pool, r.host, r.f.env, p, restoreReceipt(p, false, false)); err != nil {
		t.Fatal(err)
	}
	if err := CommitComputerRestore(ctx, r.f.pool, r.host, r.f.env, p, restoreReceipt(p, true, false)); err != nil {
		t.Fatal(err)
	}
	for _, grant := range p.Grants {
		e := r.execution()
		e.SessionID = uuid.MustParse(grant.Identity.SessionId)
		e.ProcessEpoch = grant.Identity.ProcessEpoch
		e.AuthorityGeneration = grant.AuthorityGeneration
		attachment, err := AcquireRuntimeAttachment(ctx, r.f.pool, r.host, e)
		if err != nil {
			t.Fatal(err)
		}
		if err = ObserveSessionReady(ctx, r.f.pool, r.host, e, attachment.Sequence); err != nil {
			t.Fatalf("retained Ready blocked acquiring member: %v", err)
		}
		if _, err = Dispatch(ctx, r.f.pool, e); !errors.Is(err, ErrNotReady) {
			t.Fatalf("readiness admitted business work before restore completion: %v", err)
		}
		control, err := PrepareSessionDelivery(ctx, r.f.pool, r.host, e, attachment.Sequence)
		if err != nil {
			t.Fatal(err)
		}
		if err = AcknowledgeSessionDelivery(ctx, r.f.pool, r.host, e, attachment.Sequence, control.Sequence, control.Generation, control.Kind, ""); err != nil {
			t.Fatal(err)
		}
	}
	if err := CompleteComputerRestore(ctx, r.f.pool, r.host, r.f.env, p, restoreReceipt(p, true, true)); err != nil {
		t.Fatalf("Ready/control outbox cannot complete restoration: %v", err)
	}
	var active bool
	if err := r.f.pool.QueryRow(ctx, `SELECT status='active' FROM computer_leases WHERE environment_id=$1 AND computer_id=$2 AND epoch=$3`, r.f.env, r.f.computer, r.epoch).Scan(&active); err != nil || !active {
		t.Fatalf("restore did not activate lease: %v %v", active, err)
	}
}
