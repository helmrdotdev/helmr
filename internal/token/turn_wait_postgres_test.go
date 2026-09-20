package token

import (
	"context"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run"
	"github.com/helmrdotdev/helmr/internal/run/runtest"
	"github.com/helmrdotdev/helmr/internal/session"
	"github.com/jackc/pgx/v5/pgtype"
	"testing"
	"time"
	"uuid"
)

func TestTurnStopDoesNotConsumeSharedTokenOrUnrelatedWait(t *testing.T) {
	ctx := t.Context()
	fixture := newRunLeaseClaimFixture(t, ctx)
	actorWork := fixture.addWork(t, ctx, "starting", time.Now().Add(-time.Minute))
	taskWork := fixture.addWork(t, ctx, "starting", time.Now().Add(-time.Minute))
	actorID := fixture.base.ConvertToActor(t, ctx, runtest.RunLease{RunID: actorWork.runID, LeaseID: actorWork.leaseID}, `{"enabled":false}`)
	startTaskCompletionWork(t, ctx, fixture, actorWork)
	startTaskCompletionWork(t, ctx, fixture, taskWork)
	turnID := uuid.NewV7()
	// ConvertToActor seeds cursor1 and input high-watermark2. Supply that owed
	// input, then use the real activation owner to bind its execution.
	dbtest.MustExec(t, ctx, fixture.pool, `INSERT INTO session_turns(id,environment_id,session_id,sequence,data) VALUES($1,$2,$3,2,'{}')`, turnID, fixture.environmentID, actorID)
	tx, err := fixture.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	turn, err := session.ActivateTurn(ctx, db.New(tx), session.TurnScope{EnvironmentID: fixture.environmentID, SessionID: actorID, TurnID: turnID, RunID: actorWork.runID, AttemptNumber: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	tokenID := createTokenTerminalTestToken(t, ctx, fixture, time.Now().Add(time.Hour))
	reconciler, err := NewWaitReconciler(fixture.pool)
	if err != nil {
		t.Fatal(err)
	}
	stopped := tokenWaitRegistrationRequest(t, ctx, fixture, actorWork, tokenID, uuid.NewV7())
	stopped.ActorSpeculativeInputSequence = pgtype.Int8{Int64: 2, Valid: true}
	stopped.TurnID = pgvalue.UUID(turnID)
	stopped.RunGeneration = turn.RunGeneration
	unrelated := tokenWaitRegistrationRequest(t, ctx, fixture, taskWork, tokenID, uuid.NewV7())
	if _, err = reconciler.RegisterWait(ctx, stopped); err != nil {
		t.Fatal(err)
	}
	if _, err = reconciler.RegisterWait(ctx, unrelated); err != nil {
		t.Fatal(err)
	}
	tx, err = fixture.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	graph, err := run.LockOwnedFinalization(ctx, tx, run.OwnedFinalizationRequest{OrgID: fixture.orgID, ProjectID: fixture.projectID, EnvironmentID: fixture.environmentID, RunID: actorWork.runID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = session.InterruptTurn(ctx, db.New(tx), fixture.environmentID, actorID, turnID, "stop-shared-token-wait", graph); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = fixture.queries.CompleteToken(ctx, tokenCompletionParams(fixture, tokenID, "sha256:shared-token-after-stop", `{"approved":true}`)); err != nil {
		t.Fatal(err)
	}
	batch, err := reconciler.ReconcileBatch(ctx, fixture.environmentID, tokenID, 1)
	if err != nil || batch.Resolved != 1 || batch.Examined != 1 {
		t.Fatalf("unrelated wait starved: %+v %v", batch, err)
	}
	var status, stoppedCondition, otherCondition, otherSuspension, stoppedReason string
	var revoked bool
	if err = fixture.pool.QueryRow(ctx, `SELECT t.status,a.condition_status,(SELECT s.dispatch_hold_id IS NOT NULL FROM sessions s JOIN runs r ON r.id=s.current_run_id WHERE r.id=a.run_id),b.condition_status,b.suspension_status,a.condition_reason_code FROM tokens t JOIN run_waits a ON a.id=$2 JOIN run_waits b ON b.id=$3 WHERE t.id=$1`, tokenID, stopped.WaitID, unrelated.WaitID).Scan(&status, &stoppedCondition, &revoked, &otherCondition, &otherSuspension, &stoppedReason); err != nil {
		t.Fatal(err)
	}
	if status != "completed" || stoppedCondition != "failed" || stoppedReason != "session_stopped" || !revoked || otherCondition != "completed" || otherSuspension != "released" {
		t.Fatalf("shared Token authority: %s %s %v %s %s %s", status, stoppedCondition, revoked, otherCondition, otherSuspension, stoppedReason)
	}
}
