package session_test

import (
	"errors"
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

func TestComputerManualPolicyNeedsExplicitHibernate(t *testing.T) {
	f := newFixture(t)
	if _, err := session.BeginComputerCapture(t.Context(), f.pool, *f.host(), f.captureRequest()); !errors.Is(err, session.ErrNotReady) {
		t.Fatalf("manual capture: %v", err)
	}
	req := f.executionRequest("hibernate", "sleep")
	accepted, err := session.ControlComputerExecution(t.Context(), f.pool, f.caller(), req)
	if err != nil || accepted.Status != "accepted" {
		t.Fatalf("hibernate: %+v %v", accepted, err)
	}
	capture, err := session.BeginComputerCapture(t.Context(), f.pool, *f.host(), f.captureRequest())
	if err != nil {
		t.Fatal(err)
	}
	replay, err := session.ControlComputerExecution(t.Context(), f.pool, f.caller(), req)
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
	accepted, err := session.ControlComputerExecution(t.Context(), f.pool, f.caller(), req)
	if err != nil {
		t.Fatal(err)
	}
	f.enqueue(t, "new-work")
	var code string
	if err := f.pool.QueryRow(t.Context(), `SELECT error_code FROM computer_controls WHERE id=$1 AND status='failed'`, accepted.ID).Scan(&code); err != nil || code != "new_execution_requested" {
		t.Fatalf("intent survived new work: %s %v", code, err)
	}
	req.IdempotencyKey = "busy"
	if _, err := session.ControlComputerExecution(t.Context(), f.pool, f.caller(), req); !errors.Is(err, computer.ErrBusy) {
		t.Fatalf("queued work: %v", err)
	}
}

func TestComputerAfterTurnRequiresEveryMemberAndWakeRearms(t *testing.T) {
	f := newFixture(t)
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE agent_definitions SET suspension='after_turn' WHERE environment_id=$1 AND agent_id=$2 AND deployment_id=$3`, f.env, f.agent, f.deployment)
	if _, err := session.BeginComputerCapture(t.Context(), f.pool, *f.host(), f.captureRequest()); !errors.Is(err, session.ErrNotReady) {
		t.Fatalf("no completed turn: %v", err)
	}
	a, save := f.finalize(t, "first")
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE computer_saves SET status='failed',failure_evidence='fixture settled' WHERE id=$1`, save.ID)
	if err := completedTurnForTest(t.Context(), f.pool, f.env, f.session, a.TurnID); err != nil {
		t.Fatal(err)
	}
	peer := f.peer(t)
	manualAgent := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.pool, `INSERT INTO agents(environment_id,id,key) VALUES($1,$4,'manual-peer');
 INSERT INTO agent_definitions(environment_id,agent_id,deployment_id,template_id,has_init,suspension) SELECT environment_id,$4,deployment_id,template_id,has_init,'manual' FROM agent_definitions WHERE environment_id=$1 AND agent_id=$2 AND deployment_id=$3;
 UPDATE sessions SET agent_id=$4 WHERE environment_id=$1 AND id=$5`, pgx.QueryExecModeSimpleProtocol, f.env, f.agent, f.deployment, manualAgent, peer.session)
	if _, err := session.BeginComputerCapture(t.Context(), f.pool, *f.host(), f.captureRequest()); !errors.Is(err, session.ErrNotReady) {
		t.Fatalf("manual peer: %v", err)
	}
	// Closing/fencing the peer is irrelevant to the original member's policy.
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE session_processes SET status='stopped',fenced_at=clock_timestamp() WHERE session_id=$1`, peer.session)
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE sessions SET status='closed' WHERE id=$1`, peer.session)
	wake, err := session.ControlComputerExecution(t.Context(), f.pool, f.caller(), f.executionRequest("wake", "already-awake"))
	if err != nil || wake.Status != "completed" {
		t.Fatalf("wake: %+v %v", wake, err)
	}
	if _, err := session.BeginComputerCapture(t.Context(), f.pool, *f.host(), f.captureRequest()); !errors.Is(err, session.ErrNotReady) {
		t.Fatalf("old turn rearmed capture: %v", err)
	}
	b, save := f.finalize(t, "second")
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE computer_saves SET status='failed',failure_evidence='fixture settled' WHERE id=$1`, save.ID)
	if err := completedTurnForTest(t.Context(), f.pool, f.env, f.session, b.TurnID); err != nil {
		t.Fatal(err)
	}
	if _, err := session.BeginComputerCapture(t.Context(), f.pool, *f.host(), f.captureRequest()); err != nil {
		t.Fatalf("new completion: %v", err)
	}
}

func TestComputerExecutionRechecksAuthorityOnReplay(t *testing.T) {
	f := newFixture(t)
	req := f.executionRequest("wake", "wake")
	if _, err := session.ControlComputerExecution(t.Context(), f.pool, f.caller(), req); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `DELETE FROM permission_group_members WHERE member_id IN (SELECT id FROM org_members WHERE user_id=$1)`, f.user)
	if _, err := session.ControlComputerExecution(t.Context(), f.pool, f.caller(), req); !errors.Is(err, session.ErrDenied) {
		t.Fatalf("revoked actor replay: %v", err)
	}
}

func TestComputerExecutionDeadlineSettlesWithoutClaimingRollback(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	req := f.executionRequest("hibernate", "unreachable-host")
	accepted, err := session.ControlComputerExecution(ctx, f.pool, f.caller(), req)
	if err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, ctx, f.pool, `UPDATE computer_controls SET deadline_at=clock_timestamp()-interval '1 second' WHERE id=$1`, accepted.ID)
	if err := computer.ReconcileExecutionControls(ctx, f.pool); err != nil {
		t.Fatal(err)
	}
	replay, err := session.ControlComputerExecution(ctx, f.pool, f.caller(), req)
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
	if _, err := session.ControlComputerExecution(ctx, f.pool, f.caller(), f.executionRequest("wake", "no-room")); !errors.Is(err, computer.ErrExecutionLimit) {
		t.Fatalf("wake limit: %v", err)
	}
	var preserved bool
	if err := f.pool.QueryRow(ctx, `SELECT status='ready' AND NOT EXISTS(SELECT 1 FROM computer_controls WHERE computer_id=$2 AND kind='wake') FROM checkpoints WHERE id=$1`, checkpoint.manifest.CheckpointID, f.computer).Scan(&preserved); err != nil || !preserved {
		t.Fatalf("rejected wake changed checkpoint: %v %v", preserved, err)
	}
}

func TestComputerHibernateConfirmationBindsSharedMembership(t *testing.T) {
	f := newFixture(t)
	var scope session.ExecutionScope
	if err := db.RunTx(t.Context(), f.pool, func(tx pgx.Tx) error {
		var err error
		scope, err = session.ReadComputerExecutionScope(t.Context(), tx, f.env, f.computer)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	req := f.executionRequest("hibernate", "confirmed")
	req.ExpectedScope = scope.Digest
	peer := f.peer(t)
	if _, err := session.ControlComputerExecution(t.Context(), f.pool, f.caller(), req); !errors.Is(err, session.ErrConflict) {
		t.Fatalf("stale shared membership accepted: %v", err)
	}
	if err := db.RunTx(t.Context(), f.pool, func(tx pgx.Tx) error {
		var err error
		scope, err = session.ReadComputerExecutionScope(t.Context(), tx, f.env, f.computer)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(scope.Sessions) != 2 {
		t.Fatalf("missing shared Session %s: %+v", peer.session, scope)
	}
	req.ExpectedScope = scope.Digest
	if _, err := session.ControlComputerExecution(t.Context(), f.pool, f.caller(), req); err != nil {
		t.Fatalf("current confirmation rejected: %v", err)
	}
}

func TestComputerWakeFromClosedThreadDoesNotAdmitExecution(t *testing.T) {
	f := newFixture(t)
	req := f.executionRequest("wake", "closed-thread")
	req.OpenSessionID = f.session
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE sessions SET status='closed' WHERE id=$1`, f.session)
	if _, err := session.ControlComputerExecution(t.Context(), f.pool, f.caller(), req); !errors.Is(err, session.ErrTerminal) {
		t.Fatalf("closed thread wake: %v", err)
	}
	var count int
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM computer_controls WHERE computer_id=$1`, f.computer).Scan(&count); err != nil || count != 0 {
		t.Fatalf("rejected wake wrote control: %d %v", count, err)
	}
}
