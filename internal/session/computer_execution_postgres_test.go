package session_test

import (
	"errors"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/session"
	"github.com/jackc/pgx/v5"
)

func (f fixture) executionRequest(kind, key string) session.ComputerExecutionRequest {
	return session.ComputerExecutionRequest{EnvironmentID: f.env, ComputerID: f.computer, Kind: kind, IdempotencyKey: key}
}

func TestComputerWithoutIdlePeriodNeedsExplicitHibernate(t *testing.T) {
	f := newFixture(t)
	if _, err := session.BeginComputerCapture(t.Context(), f.pool, *f.host(), f.captureRequest()); !errors.Is(err, session.ErrNotReady) {
		t.Fatalf("capture without idle period: %v", err)
	}
	req := f.executionRequest("hibernate", "sleep")
	accepted, err := session.ControlComputerExecution(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, f.caller(), req)
	if err != nil || accepted.Status != "accepted" {
		t.Fatalf("hibernate: %+v %v", accepted, err)
	}
	capture, err := session.BeginComputerCapture(t.Context(), f.pool, *f.host(), f.captureRequest())
	if err != nil {
		t.Fatal(err)
	}
	replay, err := session.ControlComputerExecution(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, f.caller(), req)
	if err != nil || replay.ID != accepted.ID {
		t.Fatalf("replay: %+v %v", replay, err)
	}
	var bound bool
	if err := f.pool.QueryRow(t.Context(), `SELECT checkpoint_id IS NOT NULL FROM computer_controls WHERE id=$1`, accepted.ID).Scan(&bound); err != nil || !bound || capture.SaveID == uuid.Nil() {
		t.Fatalf("capture binding: %v %v", bound, err)
	}
}

func TestComputerHibernateRequiresNoForegroundAndNewInputCancelsIntent(t *testing.T) {
	f := newFixture(t)
	req := f.executionRequest("hibernate", "sleep")
	accepted, err := session.ControlComputerExecution(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, f.caller(), req)
	if err != nil {
		t.Fatal(err)
	}
	f.enqueue(t, "new-work")
	var code string
	if err := f.pool.QueryRow(t.Context(), `SELECT error_code FROM computer_controls WHERE id=$1 AND status='failed'`, accepted.ID).Scan(&code); err != nil || code != "new_execution_requested" {
		t.Fatalf("intent survived new work: %s %v", code, err)
	}
	req.IdempotencyKey = "busy"
	if _, err := session.ControlComputerExecution(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, f.caller(), req); !errors.Is(err, computer.ErrBusy) {
		t.Fatalf("queued work: %v", err)
	}
}

func TestComputerExecutionRechecksAuthorityOnReplay(t *testing.T) {
	f := newFixture(t)
	req := f.executionRequest("wake", "wake")
	if _, err := session.ControlComputerExecution(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, f.caller(), req); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `DELETE FROM permission_group_members WHERE member_id IN (SELECT id FROM org_members WHERE user_id=$1)`, f.user)
	if _, err := session.ControlComputerExecution(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, f.caller(), req); !errors.Is(err, session.ErrDenied) {
		t.Fatalf("revoked actor replay: %v", err)
	}
}

func TestComputerExecutionDeadlineSettlesWithoutClaimingRollback(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	req := f.executionRequest("hibernate", "unreachable-host")
	accepted, err := session.ControlComputerExecution(ctx, auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, f.caller(), req)
	if err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, ctx, f.pool, `UPDATE computer_controls SET deadline_at=clock_timestamp()-interval '1 second' WHERE id=$1`, accepted.ID)
	if err := computer.ReconcileExecutionControls(ctx, auditpolicy.Policy{
		RetentionSeconds: 3600,
	}, f.pool); err != nil {
		t.Fatal(err)
	}
	replay, err := session.ControlComputerExecution(ctx, auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, f.caller(), req)
	if err != nil || replay.Status != "failed" || replay.ErrorCode == nil || *replay.ErrorCode != "execution_deadline_exceeded" {
		t.Fatalf("deadline receipt: %+v %v", replay, err)
	}
	var awake bool
	if err := f.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM computer_leases WHERE computer_id=$1 AND status='active' AND fenced_at IS NULL)`, f.computer).Scan(&awake); err != nil || !awake {
		t.Fatalf("failure changed execution: %v %v", awake, err)
	}
}

func TestComputerParkedProjectionAndRejectedWakePreserveCheckpoint(t *testing.T) {
	checkpoint := readyCheckpointFixture(t)
	f := checkpoint.f
	ctx := t.Context()
	var scope computer.Scope
	scope.EnvironmentID = f.env
	if err := f.pool.QueryRow(ctx, `SELECT org_id,project_id FROM environments WHERE id=$1`, f.env).Scan(&scope.OrgID, &scope.ProjectID); err != nil {
		t.Fatal(err)
	}
	snapshot, err := computer.Read(ctx, f.pool, scope, f.computer)
	if err != nil || snapshot.Residency != "parked" || snapshot.ExecutionControl == nil || snapshot.ExecutionControl.Status != "completed" {
		t.Fatalf("parked: %+v %v", snapshot, err)
	}
	dbtest.MustExec(t, ctx, f.pool, `UPDATE environments SET max_memory_bytes=1 WHERE id=$1`, f.env)
	if _, err := session.ControlComputerExecution(ctx, auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, f.caller(), f.executionRequest("wake", "no-room")); !errors.Is(err, computer.ErrExecutionLimit) {
		t.Fatalf("wake limit: %v", err)
	}
	var preserved bool
	if err := f.pool.QueryRow(ctx, `SELECT status='ready' AND NOT EXISTS(SELECT 1 FROM computer_controls WHERE computer_id=$2 AND kind='wake') FROM checkpoints WHERE id=$1`, checkpoint.manifest.CheckpointID, f.computer).Scan(&preserved); err != nil || !preserved {
		t.Fatalf("rejected wake changed checkpoint: %v %v", preserved, err)
	}
}

func TestComputerHibernatePreviewCountsOpenSessions(t *testing.T) {
	f := newFixture(t)
	preview := func() int64 {
		t.Helper()
		var count int64
		if err := db.RunTx(t.Context(), f.pool, func(tx pgx.Tx) error {
			var err error
			count, err = session.ComputerHibernatePreview(t.Context(), tx, f.caller(), f.env, f.computer)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return count
	}
	if got := preview(); got != 1 {
		t.Fatalf("one Session: %d", got)
	}
	peer := f.peer(t)
	if got := preview(); got != 2 {
		t.Fatalf("shared Computer: %d", got)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE sessions SET status='closed',close_how='close' WHERE id=$1`, peer.session)
	if got := preview(); got != 1 {
		t.Fatalf("closed peer counted: %d", got)
	}
	if err := db.RunTx(t.Context(), f.pool, func(tx pgx.Tx) error {
		_, err := session.ComputerHibernatePreview(t.Context(), tx, f.caller(), f.env, uuid.NewV7())
		return err
	}); !errors.Is(err, session.ErrDenied) {
		t.Fatalf("missing Computer: %v", err)
	}
}

func TestComputerWakeFromClosedThreadDoesNotAdmitExecution(t *testing.T) {
	f := newFixture(t)
	req := f.executionRequest("wake", "closed-thread")
	req.OpenSessionID = f.session
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE sessions SET status='closed',close_how='close' WHERE id=$1`, f.session)
	if _, err := session.ControlComputerExecution(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, f.caller(), req); !errors.Is(err, session.ErrTerminal) {
		t.Fatalf("closed thread wake: %v", err)
	}
	var count int
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM computer_controls WHERE computer_id=$1`, f.computer).Scan(&count); err != nil || count != 0 {
		t.Fatalf("rejected wake wrote control: %d %v", count, err)
	}
}
