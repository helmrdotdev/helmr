package session_test

import (
	"errors"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/session"

	"github.com/helmrdotdev/helmr/internal/dispatch"

	"github.com/helmrdotdev/helmr/internal/computer"
)

func startingSessionFixture(t *testing.T) (fixture, *dispatch.Allocator, session.Execution) {
	t.Helper()
	f, a, computerID := freshComputerAllocationFixture(t)
	var sessionID uuid.UUID
	if err := f.pool.QueryRow(t.Context(), `SELECT id FROM sessions WHERE environment_id=$1 AND computer_id=$2`, f.env, computerID).Scan(&sessionID); err != nil {
		t.Fatal(err)
	}
	r, err := a.AllocateFreshLease(t.Context(), f.env, computerID, 1, f.worker)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.AllocateSessionProcess(t.Context(), f.env, sessionID, 1); err != nil {
		t.Fatalf("initial assignment missing: %v", err)
	}
	beforeReady := session.Execution{EnvironmentID: f.env, SessionID: sessionID, ProcessEpoch: 1, LeaseEpoch: 1, WorkerHostID: f.worker, WorkerEpoch: 1, AuthorityGeneration: 1}
	if _, err := session.AcquireRuntimeAttachment(t.Context(), f.pool, *f.host(), beforeReady); !errors.Is(err, session.ErrNotReady) {
		t.Fatalf("attachment before VM readiness: %v", err)
	}
	identity := computer.LeaseIdentity{EnvironmentID: f.env, ComputerID: computerID, InstanceID: r.InstanceID, Epoch: r.Epoch}
	if _, err := a.DeliverLease(t.Context(), *f.host(), identity); err != nil {
		t.Fatal(err)
	}
	if err := dispatch.ObserveFreshLeaseReady(t.Context(), f.pool, *f.host(), identity, r.Shape, r.BaseVersion); err != nil {
		t.Fatal(err)
	}
	p, err := a.AllocateSessionProcess(t.Context(), f.env, sessionID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if p.Status != "starting" || p.ComputerID != computerID || p.InstanceID != r.InstanceID {
		t.Fatalf("wrong process allocation: %+v", p)
	}
	e := session.Execution{EnvironmentID: f.env, SessionID: sessionID, ProcessEpoch: 1, LeaseEpoch: 1, WorkerHostID: f.worker, WorkerEpoch: 1, AuthorityGeneration: 1}
	return f, a, e
}

func TestProcessAdmissionSetupReceiptPrecedesTurnDispatch(t *testing.T) {
	f, a, e := startingSessionFixture(t)
	attachment, err := session.AcquireRuntimeAttachment(t.Context(), f.pool, *f.host(), e)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dispatchTurnForTest(t.Context(), f.pool, e); !errors.Is(err, session.ErrNotReady) {
		t.Fatalf("dispatched before setup: %v", err)
	}
	if err := session.ObserveSessionReady(t.Context(), f.pool, *f.host(), e, attachment.Sequence+1); !errors.Is(err, session.ErrNotReady) {
		t.Fatalf("wrong attachment ready: %v", err)
	}
	if err := session.ObserveSessionReady(t.Context(), f.pool, *f.host(), e, attachment.Sequence); err != nil {
		t.Fatal(err)
	}
	if err := session.ObserveSessionReady(t.Context(), f.pool, *f.host(), e, attachment.Sequence); err != nil {
		t.Fatal(err)
	}
	acknowledgeAdmissionControl(t, f, e, attachment.Sequence)
	if _, err := dispatchTurnForTest(t.Context(), f.pool, e); err != nil {
		t.Fatal(err)
	}
	if _, err := a.AllocateSessionProcess(t.Context(), f.env, e.SessionID, 2); !errors.Is(err, session.ErrNotReady) {
		t.Fatalf("live process replaced: %v", err)
	}
	r, err := a.AllocateSessionProcess(t.Context(), f.env, e.SessionID, 1)
	if err != nil || r.Status != "ready" {
		t.Fatalf("historical receipt: %+v %v", r, err)
	}
}

func TestProcessAdmissionFailedSetupRetainsInputUntilExplicitResume(t *testing.T) {
	f, a, e := startingSessionFixture(t)
	attachment, err := session.AcquireRuntimeAttachment(t.Context(), f.pool, *f.host(), e)
	if err != nil {
		t.Fatal(err)
	}
	if err := session.ObserveSessionFailure(t.Context(), auditpolicy.Policy{
		RetentionSeconds: 3600,
	}, f.pool, *f.host(), e, attachment.Sequence); err != nil {
		t.Fatal(err)
	}
	if err := session.ObserveSessionStopped(t.Context(), auditpolicy.Policy{
		RetentionSeconds: 3600,
	}, f.pool, *f.host(), e, attachment.Sequence); err != nil {
		t.Fatal(err)
	}
	if _, err := a.AllocateSessionProcess(t.Context(), f.env, e.SessionID, 2); !errors.Is(err, session.ErrNotReady) {
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
	if _, err := session.ControlSession(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, f.caller(), session.SessionControlRequest{EnvironmentID: f.env, SessionID: e.SessionID, Kind: "resume", IdempotencyKey: "resume-setup", HoldID: hold}); err != nil {
		t.Fatal(err)
	}
	r, err := a.AllocateSessionProcess(t.Context(), f.env, e.SessionID, 2)
	if err != nil || r.Epoch != 2 || r.Status != "starting" {
		t.Fatalf("explicit resumed setup: %+v %v", r, err)
	}
}

func TestProcessReadinessSupersededByShutdown(t *testing.T) {
	f, _, e := startingSessionFixture(t)
	attachment, err := session.AcquireRuntimeAttachment(t.Context(), f.pool, *f.host(), e)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = session.ControlSession(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, f.caller(), session.SessionControlRequest{EnvironmentID: f.env, SessionID: e.SessionID, Kind: "cancel", IdempotencyKey: "stop-before-ready"}); err != nil {
		t.Fatal(err)
	}
	if err = session.ObserveSessionReady(t.Context(), f.pool, *f.host(), e, attachment.Sequence+1); !errors.Is(err, session.ErrNotReady) {
		t.Fatalf("superseded wrong attachment accepted: %v", err)
	}
	if err = session.ObserveSessionReady(t.Context(), f.pool, *f.host(), e, attachment.Sequence); !errors.Is(err, session.ErrConflict) {
		t.Fatalf("late readiness did not retire safely: %v", err)
	}
	var status string
	if err = f.pool.QueryRow(t.Context(), `SELECT status FROM session_processes WHERE environment_id=$1 AND session_id=$2`, f.env, e.SessionID).Scan(&status); err != nil || status == "ready" {
		t.Fatalf("late readiness changed execution: %s %v", status, err)
	}
	delivery, err := session.PrepareSessionDelivery(t.Context(), f.pool, *f.host(), e, attachment.Sequence)
	if err != nil || delivery.Kind != "shutdown" {
		t.Fatalf("late Ready prevented shutdown: %+v %v", delivery, err)
	}
}

func TestCancelledProcessAbsentBeforeSetupCanBeReclaimed(t *testing.T) {
	f, _, e := startingSessionFixture(t)
	old, err := session.AcquireRuntimeAttachment(t.Context(), f.pool, *f.host(), e)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = session.ControlSession(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, f.caller(), session.SessionControlRequest{EnvironmentID: f.env, SessionID: e.SessionID, Kind: "cancel", IdempotencyKey: "cancel-before-setup"}); err != nil {
		t.Fatal(err)
	}
	current, err := session.AcquireRuntimeAttachment(t.Context(), f.pool, *f.host(), e)
	if err != nil || current.Starting || current.Stopped {
		t.Fatalf("cancelled attachment: %+v %v", current, err)
	}
	if _, err = session.AuthorizeSessionStart(t.Context(), f.pool, *f.host(), e, current.Sequence); err == nil {
		t.Fatal("cancelled setup authorized")
	}
	if err = session.ObserveSessionStopped(t.Context(), auditpolicy.Policy{
		RetentionSeconds: 3600,
	}, f.pool, *f.host(), e, old.Sequence); !errors.Is(err, session.ErrDenied) {
		t.Fatalf("stale absence accepted: %v", err)
	}
	var computerID uuid.UUID
	if err = f.pool.QueryRow(t.Context(), `SELECT computer_id FROM sessions WHERE environment_id=$1 AND id=$2`, f.env, e.SessionID).Scan(&computerID); err != nil {
		t.Fatal(err)
	}
	scope := computer.Scope{EnvironmentID: f.env}
	if err = f.pool.QueryRow(t.Context(), `SELECT org_id,project_id FROM environments WHERE id=$1`, f.env).Scan(&scope.OrgID, &scope.ProjectID); err != nil {
		t.Fatal(err)
	}
	deletion := computer.Deletion{Principal: sessiontestPrincipal(t, f), Scope: scope, ComputerID: computerID, IdempotencyKey: "delete-cancelled"}
	if _, err = computer.Delete(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, deletion); !errors.Is(err, computer.ErrBusy) {
		t.Fatalf("unproved stop permitted deletion: %v", err)
	}
	delivery, err := session.PrepareSessionDelivery(t.Context(), f.pool, *f.host(), e, current.Sequence)
	if err != nil || delivery.Kind != "shutdown" {
		t.Fatalf("cancelled control: %+v %v", delivery, err)
	}
	if err = session.ObserveSessionStopped(t.Context(), auditpolicy.Policy{
		RetentionSeconds: 3600,
	}, f.pool, *f.host(), e, current.Sequence); err != nil {
		t.Fatal(err)
	}
	if _, err = session.AuthorizeSessionStart(t.Context(), f.pool, *f.host(), e, current.Sequence); err == nil {
		t.Fatal("stopped setup authorized")
	}
	if _, err = computer.Delete(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, deletion); err != nil {
		t.Fatalf("proved stop prevented deletion: %v", err)
	}
}

func TestProcessReadinessReplayDoesNotBlockRestoreCompletion(t *testing.T) {
	r := newComputerRestoreFixture(t)
	ctx := t.Context()
	p := r.prepare(t)
	if err := session.ValidateComputerRestore(ctx, r.f.pool, r.host, r.f.env, p, restoreReceipt(p, false, false)); err != nil {
		t.Fatal(err)
	}
	if err := session.CommitComputerRestore(ctx, r.f.pool, r.host, r.f.env, p, restoreReceipt(p, true, false)); err != nil {
		t.Fatal(err)
	}
	for _, grant := range p.Grants {
		e := r.execution()
		e.SessionID = uuid.MustParse(grant.Identity.SessionId)
		e.ProcessEpoch = grant.Identity.ProcessEpoch
		e.AuthorityGeneration = grant.AuthorityGeneration
		attachment, err := session.AcquireRuntimeAttachment(ctx, r.f.pool, r.host, e)
		if err != nil {
			t.Fatal(err)
		}
		if err = session.ObserveSessionReady(ctx, r.f.pool, r.host, e, attachment.Sequence); err != nil {
			t.Fatalf("retained Ready blocked acquiring member: %v", err)
		}
		if _, err = dispatchTurnForTest(ctx, r.f.pool, e); !errors.Is(err, session.ErrNotReady) {
			t.Fatalf("readiness admitted business work before restore completion: %v", err)
		}
		control, err := session.PrepareSessionDelivery(ctx, r.f.pool, r.host, e, attachment.Sequence)
		if err != nil {
			t.Fatal(err)
		}
		if err = session.AcknowledgeSessionDelivery(ctx, r.f.pool, r.host, e, attachment.Sequence, control.Sequence, control.Generation, control.Kind, "", 0); err != nil {
			t.Fatal(err)
		}
	}
	if err := session.CompleteComputerRestore(ctx, auditpolicy.Policy{
		RetentionSeconds: 3600,
	}, r.f.pool, r.host, r.f.env, p, restoreReceipt(p, true, true)); err != nil {
		t.Fatalf("Ready control cannot complete restoration: %v", err)
	}
	var active bool
	if err := r.f.pool.QueryRow(ctx, `SELECT status='active' FROM computer_leases WHERE environment_id=$1 AND computer_id=$2 AND epoch=$3`, r.f.env, r.f.computer, r.epoch).Scan(&active); err != nil || !active {
		t.Fatalf("restore did not activate lease: %v %v", active, err)
	}
}

func acknowledgeAdmissionControl(t *testing.T, f fixture, e session.Execution, attachment int64) {
	t.Helper()
	c, err := session.PrepareSessionDelivery(t.Context(), f.pool, *f.host(), e, attachment)
	if err != nil {
		t.Fatal(err)
	}
	if err := session.AcknowledgeSessionDelivery(t.Context(), f.pool, *f.host(), e, attachment, c.Sequence, c.Generation, c.Kind, "", c.StopSequence); err != nil {
		t.Fatal(err)
	}
}

func TestProcessAdmissionCommitsQueuedRevocationOnLiveComputer(t *testing.T) {
	f, a, e := startingSessionFixture(t)
	attachment, err := session.AcquireRuntimeAttachment(t.Context(), f.pool, *f.host(), e)
	if err != nil {
		t.Fatal(err)
	}
	if err = session.ObserveSessionFailure(t.Context(), auditpolicy.Policy{
		RetentionSeconds: 3600,
	}, f.pool, *f.host(), e, attachment.Sequence); err != nil {
		t.Fatal(err)
	}
	if err = session.ObserveSessionStopped(t.Context(), auditpolicy.Policy{
		RetentionSeconds: 3600,
	}, f.pool, *f.host(), e, attachment.Sequence); err != nil {
		t.Fatal(err)
	}
	var hold uuid.UUID
	if err = f.pool.QueryRow(t.Context(), `SELECT id FROM session_holds WHERE session_id=$1 AND released_at IS NULL`, e.SessionID).Scan(&hold); err != nil {
		t.Fatal(err)
	}
	if _, err = session.ControlSession(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, f.caller(), session.SessionControlRequest{EnvironmentID: f.env, SessionID: e.SessionID, Kind: "resume", IdempotencyKey: "resume", HoldID: hold}); err != nil {
		t.Fatal(err)
	}
	if _, err = f.pool.Exec(t.Context(), `DELETE FROM permission_group_members WHERE member_id IN (SELECT id FROM org_members WHERE user_id=$1)`, f.user); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err = a.AllocateSessionProcess(t.Context(), f.env, e.SessionID, 2); !errors.Is(err, session.ErrNotReady) {
			t.Fatal("revoked process admitted", err)
		}
	}
	var settled bool
	if err = f.pool.QueryRow(t.Context(), `SELECT count(*)>0 AND bool_and(status='failed' AND error_code='permission_revoked' AND processing_closed_at IS NOT NULL) FROM turns WHERE session_id=$1`, e.SessionID).Scan(&settled); err != nil || !settled {
		t.Fatal("revocation settlement rolled back", settled, err)
	}
	var processes, events int
	if err = f.pool.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM session_processes WHERE session_id=$1 AND epoch=2),(SELECT count(*) FROM session_events WHERE session_id=$1 AND kind='turn.failed')`, e.SessionID).Scan(&processes, &events); err != nil || processes != 0 || events != 1 {
		t.Fatal("invalid process or duplicate failure event", processes, events, err)
	}
}

func TestInitialAllocationRevocationSettlesWithoutLease(t *testing.T) {
	f, a, computerID := freshComputerAllocationFixture(t)
	dbtest.MustExec(t, t.Context(), f.pool, `DELETE FROM permission_group_members WHERE member_id IN (SELECT id FROM org_members WHERE user_id=$1)`, f.user)
	if _, err := a.AllocateFreshLease(t.Context(), f.env, computerID, 1, f.worker); !errors.Is(err, dispatch.ErrNotReady) {
		t.Fatalf("revoked allocation: %v", err)
	}
	var settled bool
	if err := f.pool.QueryRow(t.Context(), `SELECT NOT EXISTS(SELECT 1 FROM computer_leases WHERE environment_id=$1 AND computer_id=$2) AND EXISTS(SELECT 1 FROM turns t JOIN sessions s ON (s.environment_id,s.id)=(t.environment_id,t.session_id) WHERE s.environment_id=$1 AND s.computer_id=$2 AND t.status='failed' AND t.error_code='permission_revoked')`, f.env, computerID).Scan(&settled); err != nil || !settled {
		t.Fatalf("revoked work did not settle without allocation: %v %v", settled, err)
	}
}
