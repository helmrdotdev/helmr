package session_test

import (
	"context"
	"errors"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/session"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/lifetime"
	"github.com/jackc/pgx/v5"
)

func TestSessionIdleClosesOnlyContinuouslyQuiescentSession(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	dbtest.MustExec(t, ctx, f.pool, `UPDATE session_processes SET control_kind='resume',control_acknowledged_at=clock_timestamp(),control_generation=1 WHERE session_id=$1`, f.session)
	dbtest.MustExec(t, ctx, f.pool, `UPDATE sessions SET close_after_idle_ms=1000,idle_since=clock_timestamp()-interval '1 day' WHERE id=$1`, f.session)
	hold := uuid.NewV7()
	dbtest.MustExec(t, ctx, f.pool, `INSERT INTO session_holds(environment_id,id,session_id,scope,reason) VALUES($1,$2,$3,'local','human pause')`, f.env, hold, f.session)
	if err := session.ReconcileSessionIdle(ctx, f.pool, f.env, f.session); err != nil {
		t.Fatal(err)
	}
	var state string
	var since *time.Time
	if err := f.pool.QueryRow(ctx, `SELECT status,idle_since FROM sessions WHERE id=$1`, f.session).Scan(&state, &since); err != nil || state != "open" || since != nil {
		t.Fatalf("held Session aged: %s %v %v", state, since, err)
	}
	dbtest.MustExec(t, ctx, f.pool, `UPDATE session_holds SET released_at=clock_timestamp() WHERE id=$1`, hold)
	if err := session.ReconcileSessionIdle(ctx, f.pool, f.env, f.session); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(ctx, `SELECT status,idle_since FROM sessions WHERE id=$1`, f.session).Scan(&state, &since); err != nil || state != "open" || since == nil {
		t.Fatalf("quiet clock: %s %v %v", state, since, err)
	}
	dbtest.MustExec(t, ctx, f.pool, `UPDATE sessions SET idle_since=clock_timestamp()-interval '1 day' WHERE id=$1`, f.session)
	admitted := f.enqueue(t, "before-expiry")
	if err := session.ReconcileSessionIdle(ctx, f.pool, f.env, f.session); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(ctx, `SELECT status,idle_since FROM sessions WHERE id=$1`, f.session).Scan(&state, &since); err != nil || state != "open" || since != nil {
		t.Fatalf("queued work aged: %s %v %v", state, since, err)
	}
	dbtest.MustExec(t, ctx, f.pool, `UPDATE turns SET status='cancelled',terminal_at=clock_timestamp() WHERE id=$1`, admitted.TurnID)
	dbtest.MustExec(t, ctx, f.pool, `UPDATE sessions SET idle_since=clock_timestamp()-interval '1 day' WHERE id=$1`, f.session)
	if err := session.ReconcileSessionIdle(ctx, f.pool, f.env, f.session); err != nil {
		t.Fatal(err)
	}
	if err := session.ReconcileSessionClosure(ctx, auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, f.env, f.session); err != nil {
		t.Fatal(err)
	}
	var stopping bool
	if err := f.pool.QueryRow(ctx, `SELECT s.status,EXISTS(SELECT 1 FROM session_processes p WHERE p.session_id=s.id AND p.status='stopping' AND p.fenced_at IS NULL) FROM sessions s WHERE id=$1`, f.session).Scan(&state, &stopping); err != nil || state != "closed" || !stopping {
		t.Fatalf("close bypassed physical stop: %s %v %v", state, stopping, err)
	}
}

func TestStartSnapshotsIdlePolicyAndRejectsKeyedMismatch(t *testing.T) {
	f := newAdmissionFixture(t)
	key := "retained"
	req := f.startRequest("first")
	req.SessionKey = &key
	a, err := session.Start(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, nil, f.caller(), req)
	if err != nil || a.CloseAfterIdleMs == nil || *a.CloseAfterIdleMs != lifetime.DefaultMilliseconds {
		t.Fatalf("default: %+v %v", a, err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE agent_definitions SET close_after_idle_never=true WHERE environment_id=$1`, f.env)
	req.IdempotencyKey = "first"
	b, err := session.Start(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, nil, f.caller(), req)
	if err != nil || b.SessionID != a.SessionID || !sameLifetime(a.CloseAfterIdleMs, b.CloseAfterIdleMs) {
		t.Fatalf("redeployment changed policy: %+v %v", b, err)
	}
	req.IdempotencyKey = "mismatch"
	req.CloseAfterIdle = lifetime.Policy{Set: true}
	if _, err = session.Start(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, nil, f.caller(), req); !errors.Is(err, session.ErrConflict) {
		t.Fatalf("keyed mismatch: %v", err)
	}
	req.SessionKey = nil
	req.IdempotencyKey = "never"
	c, err := session.Start(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, nil, f.caller(), req)
	if err != nil || c.CloseAfterIdleMs != nil {
		t.Fatalf("explicit never: %+v %v", c, err)
	}
}

func TestSessionIdleDoesNotRestartAfterInternalSave(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	dbtest.MustExec(t, ctx, f.pool, `UPDATE session_processes SET control_kind='resume',control_acknowledged_at=clock_timestamp(),control_generation=1 WHERE session_id=$1`, f.session)
	dbtest.MustExec(t, ctx, f.pool, `UPDATE sessions SET close_after_idle_ms=60000,idle_since=clock_timestamp()-interval '1 day' WHERE id=$1`, f.session)
	// Internal capture attempts are not new user work.
	dbtest.MustExec(t, ctx, f.pool, `INSERT INTO computer_saves(environment_id,id,computer_id,computer_lease_epoch,seq,status,failure_evidence) VALUES($1,$2,$3,1,1,'failed','settled capture failure')`, f.env, uuid.NewV7(), f.computer)
	if err := session.ReconcileSessionIdle(ctx, f.pool, f.env, f.session); err != nil {
		t.Fatal(err)
	}
	var renewed bool
	if err := f.pool.QueryRow(ctx, `SELECT status='closing' FROM sessions WHERE id=$1`, f.session).Scan(&renewed); err != nil || !renewed {
		t.Fatalf("internal Save extended the idle lifetime: %v %v", renewed, err)
	}
}

func TestSessionIdleBlocksUnacknowledgedResume(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	dbtest.MustExec(t, ctx, f.pool, `UPDATE sessions SET close_after_idle_ms=60000,idle_since=clock_timestamp()-interval '1 day' WHERE id=$1`, f.session)
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
	if err := session.ReconcileSessionIdle(ctx, f.pool, f.env, f.session); err != nil {
		t.Fatal(err)
	}
	var blocked bool
	if err := f.pool.QueryRow(ctx, `SELECT status='open' AND idle_since IS NOT NULL FROM sessions WHERE id=$1`, f.session).Scan(&blocked); err != nil || !blocked {
		t.Fatalf("unfinished control aged: %v %v", blocked, err)
	}
	if err := session.AcknowledgeSessionDelivery(ctx, f.pool, *f.host(), f.execution(), attachment.Sequence, delivery.Sequence, delivery.Generation, delivery.Kind, "", 0); err != nil {
		t.Fatal(err)
	}
	if err := session.ReconcileSessionIdle(ctx, f.pool, f.env, f.session); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(ctx, `SELECT status='closing' AND idle_since IS NOT NULL FROM sessions WHERE id=$1`, f.session).Scan(&blocked); err != nil || !blocked {
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
			dbtest.MustExec(t, ctx, f.pool, `UPDATE sessions SET close_after_idle_ms=60000,idle_since=clock_timestamp()-interval '1 day' WHERE id=$1`, f.session)
			if err := session.ReconcileSessionIdle(ctx, f.pool, f.env, f.session); err != nil {
				t.Fatal(err)
			}
			var blocked bool
			if err := f.pool.QueryRow(ctx, `SELECT status='open' AND idle_since IS NOT NULL FROM sessions WHERE id=$1`, f.session).Scan(&blocked); err != nil || !blocked {
				t.Fatalf("unfinished checkpoint aged: %v %v", blocked, err)
			}
			if checkpointState == "aborting" {
				dbtest.MustExec(t, ctx, f.pool, `UPDATE checkpoints SET status='ready',manifest=decode('01','hex'),vm_platform_id=(SELECT vm_platform_id FROM computer_leases WHERE environment_id=$2 AND computer_id=$3 AND epoch=1),ready_at=clock_timestamp() WHERE id=$1`, request.CheckpointID, f.env, f.computer)
				if err := session.ReconcileSessionIdle(ctx, f.pool, f.env, f.session); err != nil {
					t.Fatal(err)
				}
				if err := f.pool.QueryRow(ctx, `SELECT status='closing' AND idle_since IS NOT NULL FROM sessions WHERE id=$1`, f.session).Scan(&blocked); err != nil || !blocked {
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
	dbtest.MustExec(t, ctx, f.pool, `UPDATE sessions SET close_after_idle_ms=60000,idle_since=clock_timestamp()-interval '1 day' WHERE id=$1`, f.session)
	if err := session.ReconcileSessionIdle(ctx, f.pool, f.env, f.session); err != nil {
		t.Fatal(err)
	}
	var state string
	if err := f.pool.QueryRow(ctx, `SELECT status FROM sessions WHERE id=$1`, f.session).Scan(&state); err != nil || state != "closing" {
		t.Fatalf("peer Turn Save extended idle policy: %s %v", state, err)
	}
}

func TestSessionIdleScanSkipsFutureQuietClock(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	dbtest.MustExec(t, ctx, f.pool, `UPDATE sessions SET close_after_idle_ms=86400000,idle_since=clock_timestamp() WHERE id=$1`, f.session)
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

func TestStartPinsSuspensionPolicyAcrossDeploymentPromotion(t *testing.T) {
	f := newAdmissionFixture(t)
	ctx := t.Context()
	first, err := session.Start(ctx, auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, nil, f.caller(), f.startRequest("manual"))
	if err != nil {
		t.Fatal(err)
	}
	promoted := uuid.NewV7()
	dbtest.MustExec(t, ctx, f.pool, `INSERT INTO deployments(environment_id,id,bundle_digest) VALUES($1,$3,'sha256:'||repeat('e',64));
 INSERT INTO template_definitions(environment_id,deployment_id,template_id,image_spec_id,cpu_millis,memory_bytes,disk_bytes,network_policy,prepare_network_policy) SELECT environment_id,$3,template_id,image_spec_id,cpu_millis,memory_bytes,disk_bytes,network_policy,prepare_network_policy FROM template_definitions WHERE environment_id=$1 AND deployment_id=$2;
 INSERT INTO agent_definitions(environment_id,agent_id,deployment_id,template_id,has_init,suspension,runtime_mcp_policy) SELECT environment_id,agent_id,$3,template_id,has_init,'after_turn',runtime_mcp_policy FROM agent_definitions WHERE environment_id=$1 AND deployment_id=$2;
 UPDATE environments SET current_deployment_id=$3 WHERE id=$1`, pgx.QueryExecModeSimpleProtocol, f.env, f.deployment, promoted)
	second, err := session.Start(ctx, auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, nil, f.caller(), f.startRequest("after-turn"))
	if err != nil {
		t.Fatal(err)
	}
	replay, err := session.Start(ctx, auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, nil, f.caller(), f.startRequest("manual"))
	if err != nil || replay.SessionID != first.SessionID {
		t.Fatalf("replay: %+v %v", replay, err)
	}
	for _, want := range []struct {
		id     uuid.UUID
		policy string
	}{{first.SessionID, "manual"}, {second.SessionID, "after_turn"}} {
		got, err := session.GetSession(ctx, auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, f.caller(), f.env, want.id)
		if err != nil || got.Suspension != want.policy {
			t.Fatalf("policy = %q, want %q: %v", got.Suspension, want.policy, err)
		}
	}
}
