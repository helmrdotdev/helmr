package session_test

import (
	"errors"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/session"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
)

func TestSessionStartRequiresCurrentSetupAuthority(t *testing.T) {
	for _, mode := range []string{"current", "stale attachment", "held", "cancelled", "ready", "stopping", "failed", "expired", "acquiring", "deleted"} {
		t.Run(mode, func(t *testing.T) {
			f, _, e := startingSessionFixture(t)
			attachment, err := session.AcquireRuntimeAttachment(t.Context(), f.pool, *f.host(), e)
			if err != nil {
				t.Fatal(err)
			}
			sequence := attachment.Sequence
			switch mode {
			case "stale attachment":
				sequence++
			case "held":
				dbtest.MustExec(t, t.Context(), f.pool, `INSERT INTO session_holds(environment_id,id,session_id,scope,reason) VALUES($1,gen_random_uuid(),$2,'local','test')`, f.env, e.SessionID)
			case "cancelled":
				dbtest.MustExec(t, t.Context(), f.pool, `UPDATE sessions SET status='cancelled' WHERE environment_id=$1 AND id=$2`, f.env, e.SessionID)
			case "ready", "stopping":
				dbtest.MustExec(t, t.Context(), f.pool, `UPDATE session_processes SET status=$3 WHERE environment_id=$1 AND session_id=$2`, f.env, e.SessionID, mode)
			case "failed":
				dbtest.MustExec(t, t.Context(), f.pool, `UPDATE session_processes SET failure_recorded_at=clock_timestamp() WHERE environment_id=$1 AND session_id=$2`, f.env, e.SessionID)
			case "expired":
				dbtest.MustExec(t, t.Context(), f.pool, `UPDATE computer_leases SET expires_at=clock_timestamp()-interval '1 second' WHERE environment_id=$1`, f.env)
			case "acquiring":
				dbtest.MustExec(t, t.Context(), f.pool, `UPDATE computer_leases SET status='acquiring' WHERE environment_id=$1`, f.env)
			case "deleted":
				dbtest.MustExec(t, t.Context(), f.pool, `UPDATE computers SET deleted_at=clock_timestamp() WHERE environment_id=$1`, f.env)
			}

			if mode == "current" || mode == "held" || mode == "cancelled" || mode == "ready" || mode == "stopping" {
				fresh, err := session.AcquireRuntimeAttachment(t.Context(), f.pool, *f.host(), e)
				wantRetirement := mode == "cancelled" || mode == "stopping"
				if err != nil || fresh.RetireAbsent != wantRetirement {
					t.Fatalf("retirement authority: %+v %v", fresh, err)
				}
				sequence = fresh.Sequence
			}
			start, err := session.AuthorizeSessionStart(t.Context(), f.pool, *f.host(), e, sequence)
			if mode == "current" {
				if err != nil || start.BundleDigest == "" || start.AgentKey == "" || start.Authority.Generation != 1 || !start.Authority.ExpiresAt.After(time.Now()) {
					t.Fatalf("start: %+v %v", start, err)
				}
				again, err := session.AuthorizeSessionStart(t.Context(), f.pool, *f.host(), e, sequence)
				if err != nil || again.BundleDigest != start.BundleDigest || again.DeploymentID != start.DeploymentID {
					t.Fatalf("retry: %+v %v", again, err)
				}
			} else if !errors.Is(err, session.ErrNotReady) {
				t.Fatalf("invalid start: %+v %v", start, err)
			}
			if mode == "held" || mode == "cancelled" || mode == "revoked" || mode == "stopping" {
				if _, err := session.RenewRuntimeAuthority(t.Context(), f.pool, *f.host(), e); err != nil {
					t.Fatalf("control still required: %v", err)
				}
			}
		})
	}
}

func TestPrestartInterruptRetiresEpochAndAllowsFreshWork(t *testing.T) {
	f, a, e := startingSessionFixture(t)
	f.session = e.SessionID
	before, err := session.AcquireRuntimeAttachment(t.Context(), f.pool, *f.host(), e)
	if err != nil || before.RetireAbsent {
		t.Fatalf("initial attachment: %+v %v", before, err)
	}
	req := controlRequest(f, "interrupt", "early-stop")
	req.InterruptQueued = true
	stop, err := session.ControlSession(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, f.caller(), req)
	if err != nil {
		t.Fatal(err)
	}
	attachment, err := session.AcquireRuntimeAttachment(t.Context(), f.pool, *f.host(), e)
	if err != nil || !attachment.RetireAbsent || !attachment.Starting {
		t.Fatalf("retirement: %+v %v", attachment, err)
	}
	if _, err := session.AuthorizeSessionStart(t.Context(), f.pool, *f.host(), e, attachment.Sequence); !errors.Is(err, session.ErrNotReady) {
		t.Fatalf("stopped startup authorized: %v", err)
	}
	control, err := session.PrepareSessionDelivery(t.Context(), f.pool, *f.host(), e, attachment.Sequence)
	if err != nil || control.Kind != "shutdown" {
		t.Fatalf("prestart control: %+v %v", control, err)
	}
	again, err := session.PrepareSessionDelivery(t.Context(), f.pool, *f.host(), e, attachment.Sequence)
	if err != nil || again.Kind != "shutdown" || again.Sequence != control.Sequence {
		t.Fatalf("control replay: %+v %v", again, err)
	}
	if err := session.ObserveSessionStopped(t.Context(), auditpolicy.Policy{
		RetentionSeconds: 3600,
	}, f.pool, *f.host(), e, attachment.Sequence); err != nil {
		t.Fatal(err)
	}
	var clean bool
	if err := f.pool.QueryRow(t.Context(), `SELECT status='stopped' AND fenced_at IS NOT NULL AND failure_recorded_at IS NULL FROM session_processes WHERE session_id=$1 AND epoch=1`, f.session).Scan(&clean); err != nil || !clean {
		t.Fatalf("expected cessation became failure: %v %v", clean, err)
	}
	resume := controlRequest(f, "resume", "resume-after-stop")
	resume.HoldID = stop.HoldID
	if _, err := session.ControlSession(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, f.caller(), resume); err != nil {
		t.Fatal(err)
	}
	next := f.enqueue(t, "new-message")
	allocation, err := a.AllocateSessionProcess(t.Context(), f.env, f.session, 2)
	if err != nil || allocation.Epoch != 2 {
		t.Fatalf("fresh allocation: %+v %v", allocation, err)
	}
	e.ProcessEpoch = 2
	attachment, err = session.AcquireRuntimeAttachment(t.Context(), f.pool, *f.host(), e)
	if err != nil || attachment.RetireAbsent {
		t.Fatalf("fresh attachment: %+v %v", attachment, err)
	}
	if err := session.ObserveSessionReady(t.Context(), f.pool, *f.host(), e, attachment.Sequence); err != nil {
		t.Fatal(err)
	}
	e.AuthorityGeneration = attachment.Authority.Generation
	acknowledgeAdmissionControl(t, f, e, attachment.Sequence)
	dispatched, err := dispatchTurnForTest(t.Context(), f.pool, e)
	if err != nil || dispatched.TurnID != next.TurnID {
		t.Fatalf("old input replayed or fresh input blocked: %+v %v", dispatched, err)
	}
}
