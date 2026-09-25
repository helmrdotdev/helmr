package run

import (
	"errors"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestActorCancellationUsesExactSessionAuthority(t *testing.T) {
	for _, mode := range []string{"active", "idle", "queued without worker", "stale successor"} {
		t.Run(mode, func(t *testing.T) {
			ctx := t.Context()
			f := newPostgresFixture(t)
			work := f.addRun(t, "assigned", time.Now().Add(-time.Minute))
			actor := f.convertToActor(t, ctx, work, `{"enabled":false}`)
			var turn uuid.UUID
			switch mode {
			case "active":
				turn = activateCancellationTurn(t, f, work, actor)
			case "queued without worker":
				dbtest.MustExec(t, ctx, f.pool, `UPDATE runs SET current_run_lease_id=NULL WHERE id=$1`, work.runID)
				dbtest.MustExec(t, ctx, f.pool, `UPDATE run_leases SET status='cancelled',terminal_at=now(),terminal_reason_code='fixture_prestart' WHERE id=$1`, work.leaseID)
				dbtest.MustExec(t, ctx, f.pool, `UPDATE workspace_leases SET status='released',released_at=now(),terminal_at=now() WHERE owner_run_lease_id=$1`, work.leaseID)
			case "stale successor":
				successor := uuid.NewV7()
				tx, err := f.pool.Begin(ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer tx.Rollback(ctx)
				dbtest.MustExec(t, ctx, tx, `SET CONSTRAINTS ALL DEFERRED`)
				dbtest.MustExec(t, ctx, tx, `UPDATE runs SET status='cancelled',terminal_at=now(),current_run_lease_id=NULL,failure='{"code":"run_cancelled","message":"Previous execution stopped","details":{}}' WHERE id=$1`, work.runID)
				dbtest.MustExec(t, ctx, tx, `UPDATE run_attempts SET terminal_outcome='cancelled',terminal_reason_code='fixture_previous',terminal_at=now() WHERE run_id=$1`, work.runID)
				dbtest.MustExec(t, ctx, tx, `INSERT INTO runs(id,org_id,project_id,environment_id,deployment_id,deployment_definition_id,entrypoint_kind,entrypoint_declared_id,cause_kind,workspace_id,base_workspace_version_id,session_id,session_input_start_sequence,session_input_high_watermark,payload,queue_name,queue_origin_at,queue_score_at,max_active_duration_ms,retry_policy,trace_id,root_span_id)
SELECT $2,org_id,project_id,environment_id,deployment_id,deployment_definition_id,entrypoint_kind,entrypoint_declared_id,cause_kind,workspace_id,base_workspace_version_id,session_id,session_input_start_sequence,session_input_high_watermark,payload,queue_name,queue_origin_at,queue_score_at,max_active_duration_ms,retry_policy,trace_id,root_span_id FROM runs WHERE id=$1`, work.runID, successor)
				dbtest.MustExec(t, ctx, tx, `INSERT INTO run_attempts(run_id,number,entrypoint_kind,workspace_id,base_workspace_version_id,session_input_start_sequence) SELECT $2,1,entrypoint_kind,workspace_id,base_workspace_version_id,session_input_start_sequence FROM runs WHERE id=$1`, work.runID, successor)
				dbtest.MustExec(t, ctx, tx, `UPDATE sessions SET current_run_id=$2,run_generation=run_generation+1 WHERE id=$1`, actor, successor)
				if err := tx.Commit(ctx); err != nil {
					t.Fatal(err)
				}
			}
			canceler, err := NewCanceler(f.pool)
			if err != nil {
				t.Fatal(err)
			}
			request := CancellationRequest{OrgID: f.orgID, ProjectID: f.projectID, EnvironmentID: f.environmentID, RunID: work.runID, IdempotencyKey: "cancel"}
			result, err := canceler.Cancel(ctx, request)
			wantCode := ""
			if mode == "active" {
				wantCode = "turn_not_active"
			}
			if mode == "stale successor" {
				wantCode = "stale_execution"
			}
			var rejected *CancellationRejectionError
			if wantCode != "" {
				if !errors.As(err, &rejected) || rejected.Code != wantCode {
					t.Fatalf("rejection=%+v %v", result, err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if result.Actor == nil || result.Actor.RunID != work.runID || result.Actor.SessionID != actor || result.Actor.ID == uuid.Nil() {
				t.Fatalf("receipt=%+v", result)
			}
			if wantCode == "" {
				if result.Actor.Status != "accepted" || result.Actor.HoldID == uuid.Nil() {
					t.Fatalf("acceptance=%+v", result.Actor)
				}
			} else if result.Actor.Status != "rejected" {
				t.Fatalf("rejection receipt=%+v", result.Actor)
			}
			first := *result.Actor
			replay, replayErr := canceler.Cancel(ctx, request)
			if replay.Actor == nil || *replay.Actor != first {
				t.Fatalf("durable replay changed: %+v %+v %v", first, replay, replayErr)
			}
			var runStatus, sessionStatus string
			var hold, current, active pgtype.UUID
			if err := f.pool.QueryRow(ctx, `SELECT r.status,s.status,s.dispatch_hold_id,s.current_run_id,s.active_turn_id FROM runs r JOIN sessions s ON s.id=$2 WHERE r.id=$1`, work.runID, actor).Scan(&runStatus, &sessionStatus, &hold, &current, &active); err != nil {
				t.Fatal(err)
			}
			if sessionStatus != "open" {
				t.Fatalf("cancellation terminalized Session: %s", sessionStatus)
			}
			if mode == "queued without worker" {
				if runStatus != "cancelled" || current != pgvalue.UUID(work.runID) || hold != pgvalue.UUID(first.HoldID) {
					t.Fatalf("unleased stop orphan: %s %v %v", runStatus, current, hold)
				}
			} else if mode == "idle" {
				if runStatus != "queued" || current != pgvalue.UUID(work.runID) || hold != pgvalue.UUID(first.HoldID) {
					t.Fatalf("acceptance claimed terminal: %s %v %v", runStatus, current, hold)
				}
				// Stop admission wins over an attempted subsequent Turn activation.
				tx, err := f.pool.Begin(ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer tx.Rollback(ctx)
				candidate := uuid.NewV7()
				dbtest.MustExec(t, ctx, tx, `INSERT INTO session_turns(id,environment_id,session_id,sequence,data) VALUES($1,$2,$3,2,'{}')`, candidate, f.environmentID, actor)
				if _, err := db.New(tx).ActivateSessionTurn(ctx, db.ActivateSessionTurnParams{EnvironmentID: pgvalue.UUID(f.environmentID), SessionID: pgvalue.UUID(actor), TurnID: pgvalue.UUID(candidate), RunID: pgvalue.UUID(work.runID), AttemptNumber: pgtype.Int4{Int32: 1, Valid: true}, InputSequence: 2}); err == nil {
					t.Fatal("activation crossed accepted stop")
				}
			} else if hold.Valid {
				t.Fatalf("rejected cancellation changed hold: %v turn=%s", hold, turn)
			}
		})
	}
}

func activateCancellationTurn(t *testing.T, f postgresFixture, work leasedRun, actor uuid.UUID) uuid.UUID {
	t.Helper()
	ctx := t.Context()
	turn := uuid.NewV7()
	dbtest.MustExec(t, ctx, f.pool, `UPDATE runs SET status='running',started_at=now(),active_started_at=now() WHERE id=$1`, work.runID)
	dbtest.MustExec(t, ctx, f.pool, `UPDATE run_leases SET status='running',claimed_at=created_at,started_at=created_at WHERE id=$1`, work.leaseID)
	dbtest.MustExec(t, ctx, f.pool, `INSERT INTO session_turns(id,environment_id,session_id,sequence,data) VALUES($1,$2,$3,2,'{}')`, turn, f.environmentID, actor)
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := db.New(tx).ActivateSessionTurn(ctx, db.ActivateSessionTurnParams{EnvironmentID: pgvalue.UUID(f.environmentID), SessionID: pgvalue.UUID(actor), TurnID: pgvalue.UUID(turn), RunID: pgvalue.UUID(work.runID), AttemptNumber: pgtype.Int4{Int32: 1, Valid: true}, InputSequence: 2}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return turn
}

func TestForcedActorFailurePreservesRecoveryAuthority(t *testing.T) {
	for _, active := range []bool{false, true} {
		name := "null Turn"
		if active {
			name = "active Turn"
		}
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			f := newPostgresFixture(t)
			work := f.addRun(t, "assigned", time.Now().Add(-time.Minute))
			actor := f.convertToActor(t, ctx, work, `{"enabled":false}`)
			var turn pgtype.UUID
			if active {
				turn = pgvalue.UUID(activateCancellationTurn(t, f, work, actor))
			}
			tx, err := f.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			graph, err := LockOwnedFinalization(ctx, tx, OwnedFinalizationRequest{OrgID: f.orgID, ProjectID: f.projectID, EnvironmentID: f.environmentID, RunID: work.runID})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := graph.FailCurrentForSecretRevocation(ctx); err != nil {
				t.Fatal(err)
			}
			if err := tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			var sessionStatus, reason, runStatus, leaseStatus, physicalStatus, desired string
			var current, heldRun, heldTurn, owner pgtype.UUID
			var cursor, events int64
			if err := f.pool.QueryRow(ctx, `SELECT s.status,s.dispatch_hold_reason,r.status,l.status,wl.status,ri.desired_state,s.current_run_id,s.dispatch_hold_run_id,s.active_turn_id,w.owner_session_id,s.committed_input_sequence,(SELECT count(*) FROM session_events WHERE session_id=s.id AND kind='session.held')
FROM sessions s JOIN runs r ON r.id=s.current_run_id JOIN run_leases l ON l.id=$2 JOIN workspace_leases wl ON wl.owner_run_lease_id=l.id JOIN runtime_instances ri ON ri.id=l.runtime_instance_id JOIN computers w ON w.id=s.workspace_id WHERE s.id=$1`, actor, work.leaseID).Scan(&sessionStatus, &reason, &runStatus, &leaseStatus, &physicalStatus, &desired, &current, &heldRun, &heldTurn, &owner, &cursor, &events); err != nil {
				t.Fatal(err)
			}
			wantLease := "rejected"
			if active {
				wantLease = "failed"
			}
			if sessionStatus != "open" || reason != "recovery_required" || runStatus != "failed" || leaseStatus != wantLease || physicalStatus != "fenced" || desired != "closed" || current != pgvalue.UUID(work.runID) || heldRun != current || heldTurn != turn || owner != pgvalue.UUID(actor) || cursor != 1 || events != 1 {
				t.Fatalf("forced failure lost Session authority: %s %s %s %s %s %s current=%v hold=%v turn=%v owner=%v cursor=%d events=%d", sessionStatus, reason, runStatus, leaseStatus, physicalStatus, desired, current, heldRun, heldTurn, owner, cursor, events)
			}
		})
	}
}

func TestActorCancellationConcurrentAcceptance(t *testing.T) {
	ctx := t.Context()
	f := newPostgresFixture(t)
	work := f.addRun(t, "assigned", time.Now().Add(-time.Minute))
	actor := f.convertToActor(t, ctx, work, `{"enabled":false}`)
	canceler, err := NewCanceler(f.pool)
	if err != nil {
		t.Fatal(err)
	}
	type outcome struct {
		result CancellationResult
		err    error
	}
	done := make(chan outcome, 2)
	for range 2 {
		go func() {
			// Key omission is a new invocation, still ordered by the same Session lock.
			result, err := canceler.Cancel(ctx, CancellationRequest{OrgID: f.orgID, ProjectID: f.projectID, EnvironmentID: f.environmentID, RunID: work.runID})
			done <- outcome{result, err}
		}()
	}
	accepted, rejected := 0, 0
	for range 2 {
		r := <-done
		var rejection *CancellationRejectionError
		if r.err == nil && r.result.Actor != nil && r.result.Actor.Status == "accepted" {
			accepted++
		} else if errors.As(r.err, &rejection) && rejection.Code == "session_held" {
			rejected++
		} else {
			t.Fatalf("unexpected concurrent result: %+v %v", r.result, r.err)
		}
	}
	var heldEvents, claims int
	if err := f.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM session_events WHERE session_id=$1 AND kind='session.held'),(SELECT count(*) FROM idempotency_claims WHERE operation='session.run.cancel' AND status='completed')`, actor).Scan(&heldEvents, &claims); err != nil {
		t.Fatal(err)
	}
	if accepted != 1 || rejected != 1 || heldEvents != 1 || claims != 2 {
		t.Fatalf("admission order: accepted=%d rejected=%d held=%d claims=%d", accepted, rejected, heldEvents, claims)
	}
}
