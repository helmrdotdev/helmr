package session_test

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/session"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestSessionIdleClosesOnlyContinuouslyQuiescentSession(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	dbtest.MustExec(t, ctx, f.pool, `UPDATE session_processes SET control_kind='resume',control_acknowledged_at=clock_timestamp(),control_generation=1 WHERE session_id=$1`, f.session)
	setIdleClose(t, f.pool, f.session, 1000)
	dbtest.MustExec(t, ctx, f.pool, `UPDATE sessions SET idle_since=clock_timestamp()-interval '1 day' WHERE id=$1`, f.session)
	admitted := f.enqueue(t, "before-expiry")
	if err := session.ReconcileSessionIdle(ctx, auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, f.env, f.session); err != nil {
		t.Fatal(err)
	}
	var state string
	var since *time.Time
	if err := f.pool.QueryRow(ctx, `SELECT status,idle_since FROM sessions WHERE id=$1`, f.session).Scan(&state, &since); err != nil || state != "open" || since != nil {
		t.Fatalf("queued work aged: %s %v %v", state, since, err)
	}
	// A hold is not work: the held queued Turn no longer keeps the Session open.
	hold := uuid.NewV7()
	dbtest.MustExec(t, ctx, f.pool, `INSERT INTO session_holds(environment_id,id,session_id,kind,issuer_kind) VALUES($1,$2,$3,'process_failed','system')`, f.env, hold, f.session)
	// Its idle process has acknowledged suspension.
	dbtest.MustExec(t, ctx, f.pool, `UPDATE session_processes SET control_kind='suspend' WHERE session_id=$1`, f.session)
	if err := session.ReconcileSessionIdle(ctx, auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, f.env, f.session); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(ctx, `SELECT status,idle_since FROM sessions WHERE id=$1`, f.session).Scan(&state, &since); err != nil || state != "open" || since == nil {
		t.Fatalf("held Session did not start its quiet clock: %s %v %v", state, since, err)
	}
	dbtest.MustExec(t, ctx, f.pool, `UPDATE sessions SET idle_since=clock_timestamp()-interval '1 day' WHERE id=$1`, f.session)
	if err := session.ReconcileSessionIdle(ctx, auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, f.env, f.session); err != nil {
		t.Fatal(err)
	}
	var turn, how string
	var released, stopping bool
	if err := f.pool.QueryRow(ctx, `SELECT s.status,s.close_how,(SELECT status FROM turns WHERE id=$2),
 NOT EXISTS(SELECT 1 FROM session_holds h WHERE h.session_id=s.id AND h.released_at IS NULL),
 EXISTS(SELECT 1 FROM session_processes p WHERE p.session_id=s.id AND p.status='stopping' AND p.fenced_at IS NULL)
 FROM sessions s WHERE id=$1`, f.session, admitted.TurnID).Scan(&state, &how, &turn, &released, &stopping); err != nil || state != "closed" || how != "idle" || turn != "cancelled" || !released || !stopping {
		t.Fatalf("held idle close: %s %s %s released=%v stopping=%v %v", state, how, turn, released, stopping, err)
	}
	var data string
	if err := f.pool.QueryRow(ctx, `SELECT convert_from(data,'UTF8') FROM session_events WHERE session_id=$1 AND kind='session.closed'`, f.session).Scan(&data); err != nil || data != `{"how":"idle"}` {
		t.Fatalf("idle close event: %s %v", data, err)
	}
	view, err := session.GetSession(ctx, auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, f.caller(), f.env, f.session)
	if err != nil || len(view.Holds) != 0 {
		t.Fatalf("closed Session reports holds: %+v %v", view.Holds, err)
	}
}

func TestSessionIdleClosesChildHeldByParentInterrupt(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	child := f.peer(t)
	setIdleClose(t, f.pool, child.session, 1000)
	dbtest.MustExec(t, ctx, f.pool, `UPDATE sessions SET parent_session_id=$1,requester_session_id=$1,root_session_id=$1,causal_depth=1 WHERE id=$2;
 UPDATE session_processes SET control_kind='resume',control_acknowledged_at=clock_timestamp(),control_generation=1 WHERE session_id=$2`, pgx.QueryExecModeSimpleProtocol, f.session, child.session)
	queued := child.enqueue(t, "child-work")
	if _, err := session.ControlSession(ctx, auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, f.caller(), controlRequest(f, "interrupt", "parent-stop")); err != nil {
		t.Fatal(err)
	}
	// The child's idle process has acknowledged its suspension.
	dbtest.MustExec(t, ctx, f.pool, `UPDATE session_processes SET stop_deadline_at=NULL,control_kind='suspend',control_acknowledged_at=clock_timestamp(),control_generation=(SELECT authority_generation FROM sessions WHERE id=$1) WHERE session_id=$1`, child.session)
	for range 2 {
		if err := session.ReconcileSessionIdle(ctx, auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, f.env, child.session); err != nil {
			t.Fatal(err)
		}
		dbtest.MustExec(t, ctx, f.pool, `UPDATE sessions SET idle_since=clock_timestamp()-interval '1 day' WHERE id=$1 AND idle_since IS NOT NULL`, child.session)
	}
	var state, turn string
	if err := f.pool.QueryRow(ctx, `SELECT s.status,(SELECT status FROM turns WHERE id=$2) FROM sessions s WHERE id=$1`, child.session, queued.TurnID).Scan(&state, &turn); err != nil || state != "closed" || turn != "cancelled" {
		t.Fatalf("held child idle close: %s %s %v", state, turn, err)
	}
}

func TestSessionIdleScanVisitsOnlySessionsThatMayBeIdle(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	admitted := f.enqueue(t, "running")
	if id, err := dispatchTurnForTest(ctx, f.pool, f.execution()); err != nil || id.TurnID != admitted.TurnID {
		t.Fatalf("dispatch %v %v", id, err)
	}
	setIdleClose(t, f.pool, f.session, 60000)
	dbtest.MustExec(t, ctx, f.pool, `UPDATE sessions SET idle_since=NULL WHERE id=$1`, f.session)
	visits := func() bool {
		t.Helper()
		candidates, err := session.LifecycleCandidates(ctx, f.pool, session.SessionLifecyclePosition{})
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range candidates {
			if c.Session() == f.session {
				return true
			}
		}
		return false
	}
	if visits() {
		t.Fatal("idle scan visited a Session with a running Turn")
	}
	// Queued work that a hold blocks is not work, so a held Session is visited.
	dbtest.MustExec(t, ctx, f.pool, `UPDATE turns SET status='failed',terminal_at=clock_timestamp(),processing_closed_at=clock_timestamp() WHERE id=$1`, admitted.TurnID)
	f.enqueue(t, "queued")
	dbtest.MustExec(t, ctx, f.pool, `UPDATE sessions SET idle_since=NULL WHERE id=$1`, f.session)
	if visits() {
		t.Fatal("idle scan visited a Session with runnable queued work")
	}
	dbtest.MustExec(t, ctx, f.pool, `INSERT INTO session_holds(environment_id,id,session_id,kind,issuer_kind) VALUES($1,$2,$3,'process_failed','system')`, f.env, uuid.NewV7(), f.session)
	if !visits() {
		t.Fatal("idle scan skipped a held Session")
	}
}

func TestStartRejectsKeyedMismatch(t *testing.T) {
	f := newAdmissionFixture(t)
	key := "retained"
	req := f.startRequest("first")
	req.SessionKey = &key
	a, err := session.Start(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, nil, f.caller(), req)
	if err != nil {
		t.Fatal(err)
	}
	b, err := session.Start(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, nil, f.caller(), req)
	if err != nil || b.SessionID != a.SessionID {
		t.Fatalf("replay: %+v %v", b, err)
	}
	req.IdempotencyKey = "mismatch"
	req.Input = json.RawMessage(`[{"type":"text","text":"different"}]`)
	if _, err = session.Start(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, nil, f.caller(), req); !errors.Is(err, session.ErrConflict) {
		t.Fatalf("keyed mismatch: %v", err)
	}
}

func TestSessionIdleDoesNotRestartAfterInternalSave(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	dbtest.MustExec(t, ctx, f.pool, `UPDATE session_processes SET control_kind='resume',control_acknowledged_at=clock_timestamp(),control_generation=1 WHERE session_id=$1`, f.session)
	setIdleClose(t, f.pool, f.session, 60000)
	dbtest.MustExec(t, ctx, f.pool, `UPDATE sessions SET idle_since=clock_timestamp()-interval '1 day' WHERE id=$1`, f.session)
	// Internal capture attempts are not new user work.
	dbtest.MustExec(t, ctx, f.pool, `INSERT INTO computer_saves(environment_id,id,computer_id,computer_lease_epoch,seq,status,failure_evidence) VALUES($1,$2,$3,1,1,'failed','settled capture failure')`, f.env, uuid.NewV7(), f.computer)
	if err := session.ReconcileSessionIdle(ctx, auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, f.env, f.session); err != nil {
		t.Fatal(err)
	}
	var renewed bool
	if err := f.pool.QueryRow(ctx, `SELECT status='closed' FROM sessions WHERE id=$1`, f.session).Scan(&renewed); err != nil || !renewed {
		t.Fatalf("internal Save extended the idle lifetime: %v %v", renewed, err)
	}
}

func TestSessionIdleBlocksUnacknowledgedResume(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	setIdleClose(t, f.pool, f.session, 60000)
	dbtest.MustExec(t, ctx, f.pool, `UPDATE sessions SET idle_since=clock_timestamp()-interval '1 day' WHERE id=$1`, f.session)
	attachment, err := session.AcquireRuntimeAttachment(ctx, f.pool, *f.host(), f.execution())
	if err != nil {
		t.Fatal(err)
	}
	delivery, err := session.PrepareSessionDelivery(ctx, f.pool, *f.host(), f.execution(), attachment.Sequence)
	if err != nil {
		t.Fatal(err)
	}
	// Even an expired clock must not close a ready process awaiting its receipt.
	dbtest.MustExec(t, ctx, f.pool, `UPDATE sessions SET idle_since=clock_timestamp()-interval '1 day' WHERE id=$1`, f.session)
	if err := session.ReconcileSessionIdle(ctx, auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, f.env, f.session); err != nil {
		t.Fatal(err)
	}
	var blocked bool
	if err := f.pool.QueryRow(ctx, `SELECT status='open' AND idle_since IS NOT NULL FROM sessions WHERE id=$1`, f.session).Scan(&blocked); err != nil || !blocked {
		t.Fatalf("unfinished control aged: %v %v", blocked, err)
	}
	if err := session.AcknowledgeSessionDelivery(ctx, f.pool, *f.host(), f.execution(), attachment.Sequence, delivery.Sequence, delivery.Generation, delivery.Kind, "", 0); err != nil {
		t.Fatal(err)
	}
	if err := session.ReconcileSessionIdle(ctx, auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, f.env, f.session); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(ctx, `SELECT status='closed' AND idle_since IS NOT NULL FROM sessions WHERE id=$1`, f.session).Scan(&blocked); err != nil || !blocked {
		t.Fatalf("acknowledged control not quiet: %v %v", blocked, err)
	}
}

func TestSessionIdleBlocksUnfinishedCheckpointTransitions(t *testing.T) {
	for _, checkpointState := range []string{"capturing", "sealed", "aborting", "consumed"} {
		t.Run(checkpointState, func(t *testing.T) {
			f := newFixture(t)
			ctx := t.Context()
			request := f.captureRequest()
			capture, _ := beginCapture(t, f, request)
			// Isolate checkpoint controls from pending Save and process-control blockers.
			dbtest.MustExec(t, ctx, f.pool, `UPDATE computer_saves SET status='failed',failure_evidence='settled',requested_at=clock_timestamp()-interval '2 days' WHERE id=$1`, capture.SaveID)
			dbtest.MustExec(t, ctx, f.pool, `UPDATE checkpoints SET status=$2 WHERE id=$1`, request.CheckpointID, checkpointState)
			dbtest.MustExec(t, ctx, f.pool, `UPDATE session_processes SET control_kind='resume',control_acknowledged_at=clock_timestamp(),control_generation=1 WHERE session_id=$1`, f.session)
			setIdleClose(t, f.pool, f.session, 60000)
			dbtest.MustExec(t, ctx, f.pool, `UPDATE sessions SET idle_since=clock_timestamp()-interval '1 day' WHERE id=$1`, f.session)
			if err := session.ReconcileSessionIdle(ctx, auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, f.env, f.session); err != nil {
				t.Fatal(err)
			}
			var blocked bool
			if err := f.pool.QueryRow(ctx, `SELECT status='open' AND idle_since IS NOT NULL FROM sessions WHERE id=$1`, f.session).Scan(&blocked); err != nil || !blocked {
				t.Fatalf("unfinished checkpoint aged: %v %v", blocked, err)
			}
			if checkpointState == "aborting" {
				dbtest.MustExec(t, ctx, f.pool, `UPDATE checkpoints SET status='ready',manifest=decode('01','hex'),vm_platform_id=(SELECT vm_platform_id FROM computer_leases WHERE environment_id=$2 AND computer_id=$3 AND epoch=1),ready_at=clock_timestamp() WHERE id=$1`, request.CheckpointID, f.env, f.computer)
				if err := session.ReconcileSessionIdle(ctx, auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, f.env, f.session); err != nil {
					t.Fatal(err)
				}
				if err := f.pool.QueryRow(ctx, `SELECT status='closed' AND idle_since IS NOT NULL FROM sessions WHERE id=$1`, f.session).Scan(&blocked); err != nil || !blocked {
					t.Fatalf("parked checkpoint prevented quiet clock: %v %v", blocked, err)
				}
			}
		})
	}
}

func TestSessionIdleIgnoresPeerTurnSave(t *testing.T) {
	f := newFixture(t)
	peer := f.peer(t)
	ctx := t.Context()
	admitted := peer.enqueue(t, "peer-save")
	dbtest.MustExec(t, ctx, f.pool, `UPDATE turns SET status='cancelled',terminal_at=clock_timestamp(),result_recorded_at=clock_timestamp(),result_digest=decode(repeat('00',32),'hex') WHERE id=$1`, admitted.TurnID)
	dbtest.MustExec(t, ctx, f.pool, `INSERT INTO computer_saves(environment_id,id,computer_id,computer_lease_epoch,seq,turn_id) VALUES($1,$2,$3,1,1,$4)`, f.env, uuid.NewV7(), f.computer, admitted.TurnID)
	dbtest.MustExec(t, ctx, f.pool, `UPDATE session_processes SET control_kind='resume',control_acknowledged_at=clock_timestamp(),control_generation=1 WHERE session_id=$1`, f.session)
	setIdleClose(t, f.pool, f.session, 60000)
	dbtest.MustExec(t, ctx, f.pool, `UPDATE sessions SET idle_since=clock_timestamp()-interval '1 day' WHERE id=$1`, f.session)
	if err := session.ReconcileSessionIdle(ctx, auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, f.env, f.session); err != nil {
		t.Fatal(err)
	}
	var state string
	if err := f.pool.QueryRow(ctx, `SELECT status FROM sessions WHERE id=$1`, f.session).Scan(&state); err != nil || state != "closed" {
		t.Fatalf("peer Turn Save extended idle policy: %s %v", state, err)
	}
}

func TestSessionIdleScanSkipsFutureQuietClock(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	setIdleClose(t, f.pool, f.session, 86400000)
	dbtest.MustExec(t, ctx, f.pool, `UPDATE sessions SET idle_since=clock_timestamp() WHERE id=$1`, f.session)
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT id FROM sessions WHERE id=$1 FOR NO KEY UPDATE`, f.session); err != nil {
		t.Fatal(err)
	}
	scan, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	if _, _, err := session.ReconcileSessionLifecycle(scan, auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, session.SessionLifecyclePosition{}); err != nil {
		t.Fatalf("future quiet clock unnecessarily locked: %v", err)
	}
}

func TestSessionReadsIdlePeriodFromPinnedDeployment(t *testing.T) {
	f := newAdmissionFixture(t)
	ctx := t.Context()
	dbtest.MustExec(t, ctx, f.pool, `UPDATE agent_definitions SET close_after_idle_ms=60000 WHERE environment_id=$1`, f.env)
	first, err := session.Start(ctx, auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, nil, f.caller(), f.startRequest("first"))
	if err != nil {
		t.Fatal(err)
	}
	promoted := uuid.NewV7()
	dbtest.MustExec(t, ctx, f.pool, `INSERT INTO deployments(environment_id,id,bundle_digest) VALUES($1,$3,'sha256:'||repeat('e',64));
 INSERT INTO template_definitions(environment_id,deployment_id,template_id,image_spec_id,cpu_millis,memory_bytes,disk_bytes,network_policy,prepare_network_policy) SELECT environment_id,$3,template_id,image_spec_id,cpu_millis,memory_bytes,disk_bytes,network_policy,prepare_network_policy FROM template_definitions WHERE environment_id=$1 AND deployment_id=$2;
 INSERT INTO agent_definitions(environment_id,agent_id,deployment_id,template_id,has_init,close_after_idle_ms,runtime_mcp_policy) SELECT environment_id,agent_id,$3,template_id,has_init,NULL,runtime_mcp_policy FROM agent_definitions WHERE environment_id=$1 AND deployment_id=$2;
 UPDATE environments SET current_deployment_id=$3 WHERE id=$1`, pgx.QueryExecModeSimpleProtocol, f.env, f.deployment, promoted)
	second, err := session.Start(ctx, auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, nil, f.caller(), f.startRequest("second"))
	if err != nil {
		t.Fatal(err)
	}
	finite := int64(60000)
	for _, want := range []struct {
		id    uuid.UUID
		close *int64
	}{{first.SessionID, &finite}, {second.SessionID, nil}} {
		got, err := session.GetSession(ctx, auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, f.caller(), f.env, want.id)
		if err != nil || !sameLifetime(got.CloseAfterIdleMs, want.close) {
			t.Fatalf("idle period = %v, want %v: %v", got.CloseAfterIdleMs, want.close, err)
		}
	}
}

// setIdleClose sets the idle close period on the Agent definition the
// Session is pinned to.
func setIdleClose(t *testing.T, pool *pgxpool.Pool, sessionID uuid.UUID, ms int64) {
	t.Helper()
	dbtest.MustExec(t, t.Context(), pool, `UPDATE agent_definitions d SET close_after_idle_ms=$2 FROM sessions s WHERE s.id=$1 AND (d.environment_id,d.agent_id,d.deployment_id)=(s.environment_id,s.agent_id,s.deployment_id)`, sessionID, ms)
}

func TestSessionIdleClockStartsWithoutIdleClosePolicy(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	dbtest.MustExec(t, ctx, f.pool, `UPDATE agent_definitions d SET close_after_idle_ms=NULL,hibernate_after_idle_ms=60000 FROM sessions s WHERE s.id=$1 AND (d.environment_id,d.agent_id,d.deployment_id)=(s.environment_id,s.agent_id,s.deployment_id);
 UPDATE sessions SET idle_since=NULL WHERE id=$1`, pgx.QueryExecModeSimpleProtocol, f.session)
	candidates, err := session.LifecycleCandidates(ctx, f.pool, session.SessionLifecyclePosition{})
	if err != nil {
		t.Fatal(err)
	}
	visited := false
	for _, c := range candidates {
		visited = visited || c.Session() == f.session
	}
	if !visited {
		t.Fatal("idle scan skipped a Session that never closes on idle")
	}
	if err := session.ReconcileSessionIdle(ctx, auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, f.env, f.session); err != nil {
		t.Fatal(err)
	}
	var open, timed bool
	if err := f.pool.QueryRow(ctx, `SELECT status='open',idle_since IS NOT NULL FROM sessions WHERE id=$1`, f.session).Scan(&open, &timed); err != nil || !open || !timed {
		t.Fatalf("idle clock: open=%v timed=%v %v", open, timed, err)
	}
}
