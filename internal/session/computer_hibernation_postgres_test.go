package session_test

import (
	"errors"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/session"
	"github.com/jackc/pgx/v5"
)

// hibernateAfter sets the automatic hibernation period of the Agent
// definition the Session is pinned to; nil means never.
func (f fixture) hibernateAfter(t *testing.T, sessionID uuid.UUID, period *time.Duration) {
	t.Helper()
	var ms *int64
	if period != nil {
		value := period.Milliseconds()
		ms = &value
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE agent_definitions d SET hibernate_after_idle_ms=$2 FROM sessions s WHERE s.id=$1 AND (d.environment_id,d.agent_id,d.deployment_id)=(s.environment_id,s.agent_id,s.deployment_id)`, sessionID, ms)
}

func (f fixture) idleFor(t *testing.T, sessionID uuid.UUID, ago time.Duration) {
	t.Helper()
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE sessions SET idle_since=clock_timestamp()-$2*interval '1 millisecond' WHERE id=$1`, sessionID, ago.Milliseconds())
}

// ownAgent pins the Session to a separate Agent so it can carry its own period.
func (f fixture) ownAgent(t *testing.T, sessionID uuid.UUID, key string) {
	t.Helper()
	dbtest.MustExec(t, t.Context(), f.pool, `INSERT INTO agents(environment_id,id,key) VALUES($1,$4,$5);
 INSERT INTO agent_definitions(environment_id,agent_id,deployment_id,template_id,has_init,runtime_mcp_policy) SELECT environment_id,$4,deployment_id,template_id,has_init,runtime_mcp_policy FROM agent_definitions WHERE environment_id=$1 AND agent_id=$2 AND deployment_id=$3;
 UPDATE sessions SET agent_id=$4 WHERE environment_id=$1 AND id=$6`, pgx.QueryExecModeSimpleProtocol, f.env, f.agent, f.deployment, uuid.NewV7(), key, sessionID)
}

func (f fixture) captureAllowed(t *testing.T) bool {
	t.Helper()
	_, err := session.BeginComputerCapture(t.Context(), f.pool, *f.host(), f.captureRequest())
	if err != nil && !errors.Is(err, session.ErrNotReady) {
		t.Fatal(err)
	}
	return err == nil
}

func durationPtr(d time.Duration) *time.Duration { return &d }

func TestComputerHibernatesAfterIdlePeriod(t *testing.T) {
	f := newFixture(t)
	f.hibernateAfter(t, f.session, durationPtr(time.Minute))
	f.idleFor(t, f.session, 30*time.Second)
	if f.captureAllowed(t) {
		t.Fatal("hibernated before its idle period")
	}
	f.idleFor(t, f.session, 2*time.Minute)
	if !f.captureAllowed(t) {
		t.Fatal("did not hibernate after its idle period")
	}
}

func TestComputerSessionWithoutIdlePeriodBlocksSharedHibernation(t *testing.T) {
	f := newFixture(t)
	peer := f.peer(t)
	f.ownAgent(t, peer.session, "never-peer")
	f.hibernateAfter(t, f.session, durationPtr(time.Minute))
	f.idleFor(t, f.session, time.Hour)
	f.idleFor(t, peer.session, time.Hour)
	if f.captureAllowed(t) {
		t.Fatal("a Session that never hibernates did not block its Computer")
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE session_processes SET status='stopped',fenced_at=clock_timestamp() WHERE session_id=$1;
 UPDATE sessions SET status='closed',close_how='close' WHERE id=$1`, pgx.QueryExecModeSimpleProtocol, peer.session)
	if !f.captureAllowed(t) {
		t.Fatal("closed peer kept blocking hibernation")
	}
}

func TestComputerWaitsForEverySessionIdlePeriod(t *testing.T) {
	f := newFixture(t)
	peer := f.peer(t)
	f.ownAgent(t, peer.session, "slow-peer")
	f.hibernateAfter(t, f.session, durationPtr(time.Minute))
	f.hibernateAfter(t, peer.session, durationPtr(10*time.Minute))
	f.idleFor(t, f.session, 2*time.Minute)
	f.idleFor(t, peer.session, 5*time.Minute)
	if f.captureAllowed(t) {
		t.Fatal("hibernated before the longer period passed")
	}
	f.idleFor(t, peer.session, 11*time.Minute)
	if !f.captureAllowed(t) {
		t.Fatal("did not hibernate after every period passed")
	}
}

func TestComputerHibernationClockRestartsOnCommandAndWakeButNotSave(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	f.hibernateAfter(t, f.session, durationPtr(time.Minute))
	f.idleFor(t, f.session, time.Hour)
	command := uuid.NewV7()
	dbtest.MustExec(t, ctx, f.pool, `INSERT INTO commands(environment_id,id,computer_id,argv,env,stdin,timeout_ms,caller_kind,caller_id,status,terminal_at,terminal_reason_code) VALUES($1,$2,$3,ARRAY['true'],'{}','',60000,'user',uuidv7(),'cancelled',clock_timestamp(),'cancelled')`, f.env, command, f.computer)
	if f.captureAllowed(t) {
		t.Fatal("a just-settled Command did not restart the clock")
	}
	dbtest.MustExec(t, ctx, f.pool, `UPDATE commands SET terminal_at=clock_timestamp()-interval '2 minutes' WHERE id=$1`, command)
	wake, err := session.ControlComputerExecution(ctx, auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, f.caller(), f.executionRequest("wake", "awake"))
	if err != nil || wake.Status != "completed" {
		t.Fatalf("wake: %+v %v", wake, err)
	}
	if f.captureAllowed(t) {
		t.Fatal("a wake did not restart the clock")
	}
	dbtest.MustExec(t, ctx, f.pool, `UPDATE computer_controls SET accepted_at=clock_timestamp()-interval '2 minutes',settled_at=clock_timestamp()-interval '2 minutes' WHERE id=$1`, wake.ID)
	// Internal saves are not work and never move the clock.
	dbtest.MustExec(t, ctx, f.pool, `INSERT INTO computer_saves(environment_id,id,computer_id,computer_lease_epoch,seq,status,failure_evidence) SELECT environment_id,$2,id,1,next_save_seq,'failed','settled capture failure' FROM computers WHERE environment_id=$1 AND id=$3;
 UPDATE computers SET next_save_seq=next_save_seq+1 WHERE environment_id=$1 AND id=$3`, pgx.QueryExecModeSimpleProtocol, f.env, uuid.NewV7(), f.computer)
	if !f.captureAllowed(t) {
		t.Fatal("an internal save restarted the clock")
	}
}

func TestComputerHeldQueuedTurnDoesNotBlockHibernation(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	f.hibernateAfter(t, f.session, durationPtr(time.Minute))
	f.enqueue(t, "held")
	dbtest.MustExec(t, ctx, f.pool, `INSERT INTO session_holds(environment_id,id,session_id,kind,issuer_kind) VALUES($1,$2,$3,'process_failed','system')`, f.env, uuid.NewV7(), f.session)
	if err := session.ReconcileSessionIdle(ctx, auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, f.env, f.session); err != nil {
		t.Fatal(err)
	}
	var idle bool
	if err := f.pool.QueryRow(ctx, `SELECT idle_since IS NOT NULL FROM sessions WHERE id=$1`, f.session).Scan(&idle); err != nil || !idle {
		t.Fatalf("held queued Turn kept the Session busy: %v %v", idle, err)
	}
	f.idleFor(t, f.session, 2*time.Minute)
	if !f.captureAllowed(t) {
		t.Fatal("held queued Turn blocked hibernation")
	}
}

func TestComputerChildOnAnotherComputerDoesNotBlockHibernation(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	other, child := uuid.NewV7(), uuid.NewV7()
	dbtest.MustExec(t, ctx, f.pool, `
 INSERT INTO computers(environment_id,id,initial_root_id,initial_root_digest,origin_template_id,origin_deployment_id,cpu_millis,memory_bytes,disk_bytes,storage_reservation_bytes) SELECT environment_id,$2,initial_root_id,initial_root_digest,origin_template_id,origin_deployment_id,cpu_millis,memory_bytes,disk_bytes,storage_reservation_bytes FROM computers WHERE id=$1;
 INSERT INTO sessions(environment_id,id,agent_id,deployment_id,computer_id,root_session_id,parent_session_id,requester_session_id,causal_depth,origin_user_id) SELECT environment_id,$3,agent_id,deployment_id,$2,id,id,id,1,origin_user_id FROM sessions WHERE id=$4;`, pgx.QueryExecModeSimpleProtocol, f.computer, other, child, f.session)
	f.hibernateAfter(t, f.session, durationPtr(time.Minute))
	setIdleClose(t, f.pool, f.session, 1000)
	if err := session.ReconcileSessionIdle(ctx, auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, f.env, f.session); err != nil {
		t.Fatal(err)
	}
	f.idleFor(t, f.session, 2*time.Minute)
	if err := session.ReconcileSessionIdle(ctx, auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, f.env, f.session); err != nil {
		t.Fatal(err)
	}
	var open bool
	if err := f.pool.QueryRow(ctx, `SELECT status='open' AND idle_since IS NOT NULL FROM sessions WHERE id=$1`, f.session).Scan(&open); err != nil || !open {
		t.Fatalf("open child did not defer idle close while the parent stays idle: %v %v", open, err)
	}
	if !f.captureAllowed(t) {
		t.Fatal("child Session on another Computer blocked hibernation")
	}
}

func TestComputerWithoutSessionsHibernatesAfterUnusedPeriod(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	dbtest.MustExec(t, ctx, f.pool, `UPDATE computers SET created_at=clock_timestamp()-interval '1 hour' WHERE id=$1;
 UPDATE session_processes SET status='stopped',fenced_at=clock_timestamp() WHERE session_id=$2;
 UPDATE sessions SET status='closed',close_how='close' WHERE id=$2;
 INSERT INTO session_events(environment_id,session_id,seq,kind,data,created_at) SELECT environment_id,id,next_event_seq,'session.closed',convert_to('{"how":"close"}','UTF8'),clock_timestamp()-interval '5 minutes' FROM sessions WHERE id=$2;
 UPDATE sessions SET next_event_seq=next_event_seq+1 WHERE id=$2`, pgx.QueryExecModeSimpleProtocol, f.computer, f.session)
	if f.captureAllowed(t) {
		t.Fatal("hibernated before the unused period after its last Session closed")
	}
	dbtest.MustExec(t, ctx, f.pool, `UPDATE session_events SET created_at=clock_timestamp()-$2*interval '1 millisecond' WHERE session_id=$1 AND kind='session.closed'`, f.session, (session.UnusedComputerHibernateAfter + time.Minute).Milliseconds())
	if !f.captureAllowed(t) {
		t.Fatal("did not hibernate after the unused period")
	}
}

func TestComputerWithoutSessionsWaitsForUnsettledCommand(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	dbtest.MustExec(t, ctx, f.pool, `UPDATE computers SET created_at=clock_timestamp()-interval '1 hour' WHERE id=$1;
 UPDATE session_processes SET status='stopped',fenced_at=clock_timestamp() WHERE session_id=$2;
 UPDATE sessions SET status='closed',close_how='close' WHERE id=$2`, pgx.QueryExecModeSimpleProtocol, f.computer, f.session)
	command := uuid.NewV7()
	dbtest.MustExec(t, ctx, f.pool, `INSERT INTO commands(environment_id,id,computer_id,computer_lease_epoch,argv,env,stdin,timeout_ms,caller_kind,caller_id,status) VALUES($1,$2,$3,1,ARRAY['true'],'{}','',60000,'user',uuidv7(),'running')`, f.env, command, f.computer)
	if f.captureAllowed(t) {
		t.Fatal("hibernated with an unsettled Command")
	}
	dbtest.MustExec(t, ctx, f.pool, `UPDATE commands SET status='cancelled',terminal_at=clock_timestamp()-interval '11 minutes',terminal_reason_code='cancelled',process_reconciled_at=clock_timestamp() WHERE id=$1`, command)
	if !f.captureAllowed(t) {
		t.Fatal("did not hibernate after the Command settled and the unused period passed")
	}
}

func TestComputerClosingSessionBlocksAutomaticHibernation(t *testing.T) {
	f := newFixture(t)
	peer := f.peer(t)
	f.hibernateAfter(t, f.session, durationPtr(time.Minute))
	f.idleFor(t, f.session, time.Hour)
	f.idleFor(t, peer.session, time.Hour)
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE sessions SET status='closing',close_how='close' WHERE id=$1`, peer.session)
	if f.captureAllowed(t) {
		t.Fatal("a closing Session allowed automatic hibernation")
	}
}
