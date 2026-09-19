package dispatch

import (
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/session"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestTurnRecoveryCandidatesDoNotStarveTasks(t *testing.T) {
	for _, mode := range []string{"fresh active", "fresh held", "resume held", "resume expired checkpoint", "resume invalid checkpoint", "resume discarded private version", "resume exhausted budget", "resume valid checkpoint"} {
		t.Run(mode, func(t *testing.T) {
			actor := newRunPlacementFixtureWithSeed(t, "blocked-recovery-actor")
			// Both Tasks share the database and recovery lane with the earlier Actor.
			// A second eligible Task proves that the next bounded pass also progresses.
			tasks := []runPlacementFixture{
				addRecoveryTask(t, actor, uuid.MustParse("ffffffff-ffff-7fff-bfff-fffffffffffe")),
				addRecoveryTask(t, actor, uuid.MustParse("ffffffff-ffff-7fff-bfff-ffffffffffff")),
			}
			fresh := mode == "fresh active" || mode == "fresh held"
			var actorID, checkpointID uuid.UUID
			var actorLeaseID pgtype.UUID
			if fresh {
				actorLeaseID, _ = prepareFreshRunLeaseForFixture(t, actor)
				actorID = convertFreshRunToActor(t, actor)
			} else {
				actorID, _, checkpointID = prepareActorSuspendedRestore(t, actor)
				actorLeaseID = grantRecoveryRestore(t, actor)
			}
			turn := activateRecoveryTurn(t, actor, actorID, actorLeaseID)
			if mode == "fresh held" || mode == "resume held" {
				tx, err := actor.pool.Begin(actor.ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer tx.Rollback(actor.ctx)
				receipt, err := session.InterruptTurn(actor.ctx, db.New(tx), actor.environmentID, actorID, turn, "stop")
				if err != nil || receipt.Status != "accepted" {
					t.Fatalf("interrupt: %+v %v", receipt, err)
				}
				if err := tx.Commit(actor.ctx); err != nil {
					t.Fatal(err)
				}
			}
			switch mode {
			case "resume expired checkpoint":
				dbtest.MustExec(t, actor.ctx, actor.pool, `UPDATE run_checkpoints SET expires_at=now()-interval '1 second' WHERE id=$1`, checkpointID)
			case "resume invalid checkpoint":
				dbtest.MustExec(t, actor.ctx, actor.pool, `UPDATE run_checkpoints SET status='invalid',ready_at=NULL,invalidated_at=now(),invalidation_reason_code='test_invalid' WHERE id=$1`, checkpointID)
			case "resume discarded private version":
				dbtest.MustExec(t, actor.ctx, actor.pool, `UPDATE workspace_versions SET status='discarded',discarded_at=now() WHERE id=(SELECT private_workspace_version_id FROM run_checkpoints WHERE id=$1)`, checkpointID)
			case "resume exhausted budget":
				dbtest.MustExec(t, actor.ctx, actor.pool, `UPDATE runs SET max_active_duration_ms=5000,active_started_at=now()-interval '1 minute' WHERE id=$1`, actor.runID)
			}
			// The Actor loses lease authority first; active-budget loss, when set,
			// predates that loss and must prevent checkpoint continuation.
			expireRecoveryLease(t, actor, actorLeaseID)
			for _, task := range tasks {
				var lease pgtype.UUID
				if fresh {
					lease, _ = prepareFreshRunLeaseForFixture(t, task)
				} else {
					prepareSuspendedRestore(t, task, false)
					lease = grantRecoveryRestore(t, task)
				}
				expireRecoveryLease(t, task, lease)
				var actorFirst bool
				query := `SELECT $1::uuid < $2::uuid`
				args := []any{actor.runID, task.runID}
				if fresh {
					query = `SELECT a.expires_at < LEAST(b.expires_at,b.start_deadline_at) FROM run_leases a JOIN run_leases b ON b.id=$2 WHERE a.id=$1`
					args = []any{actorLeaseID, lease}
				}
				if err := actor.pool.QueryRow(actor.ctx, query, args...).Scan(&actorFirst); err != nil || !actorFirst {
					t.Fatalf("fixture must put blocked Actor ahead of Task under recovery order: %t %v", actorFirst, err)
				}
			}
			recoverOne := func(want uuid.UUID) {
				t.Helper()
				if fresh {
					n, err := actor.authority.RecoverRunExecutionLeases(actor.ctx, 1)
					if err != nil || n != 1 {
						t.Fatalf("fresh recovery: count=%d err=%v", n, err)
					}
				} else {
					rows, err := actor.authority.RecoverExpiredRunResumes(actor.ctx, 1)
					if err != nil || len(rows) != 1 || rows[0].RunID != pgvalue.UUID(want) {
						t.Fatalf("resume recovery: rows=%+v err=%v want=%s", rows, err, want)
					}
				}
				var current pgtype.UUID
				var status string
				if err := actor.pool.QueryRow(actor.ctx, `SELECT current_run_lease_id,status FROM runs WHERE id=$1`, want).Scan(&current, &status); err != nil {
					t.Fatal(err)
				}
				if current.Valid || status != "queued" {
					t.Fatalf("expected Run did not recover: %s %v %s", want, current, status)
				}
			}
			if mode == "resume valid checkpoint" {
				recoverOne(actor.runID)
			}
			for _, task := range tasks {
				recoverOne(task.runID)
			}
			var active uuid.UUID
			var cursor int64
			var leaseStatus, turnStatus string
			if err := actor.pool.QueryRow(actor.ctx, `SELECT s.active_turn_id,s.committed_input_sequence,l.status,i.turn_status FROM sessions s JOIN run_leases l ON l.id=$2 JOIN session_records i ON i.id=s.active_turn_id WHERE s.id=$1`, actorID, actorLeaseID).Scan(&active, &cursor, &leaseStatus, &turnStatus); err != nil {
				t.Fatal(err)
			}
			wantLease := "running"
			if mode == "resume valid checkpoint" {
				wantLease = "expired"
			}
			if active != turn || cursor != 1 || turnStatus != "running" || leaseStatus != wantLease {
				t.Fatalf("Actor authority changed: active=%s cursor=%d turn=%s lease=%s", active, cursor, turnStatus, leaseStatus)
			}
			if fresh {
				if n, err := actor.authority.RecoverRunExecutionLeases(actor.ctx, 1); err != nil || n != 0 {
					t.Fatalf("final fresh pass: %d %v", n, err)
				}
			} else if rows, err := actor.authority.RecoverExpiredRunResumes(actor.ctx, 1); err != nil || len(rows) != 0 {
				t.Fatalf("final resume pass: %+v %v", rows, err)
			}
		})
	}
}

func activateRecoveryTurn(t *testing.T, f runPlacementFixture, actorID uuid.UUID, lease pgtype.UUID) uuid.UUID {
	t.Helper()
	turn := uuid.NewV7()
	dbtest.MustExec(t, f.ctx, f.pool, `UPDATE run_leases SET status='running',claimed_at=created_at,started_at=created_at WHERE id=$1`, lease)
	dbtest.MustExec(t, f.ctx, f.pool, `UPDATE runs SET status='running',started_at=now(),active_started_at=now() WHERE id=$1`, f.runID)
	dbtest.MustExec(t, f.ctx, f.pool, `UPDATE sessions SET next_input_sequence=3 WHERE id=$1`, actorID)
	dbtest.MustExec(t, f.ctx, f.pool, `INSERT INTO session_records(id,environment_id,session_id,direction,sequence,data) VALUES($1,$2,$3,'input',2,'{}')`, turn, f.environmentID, actorID)
	tx, err := f.pool.Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(f.ctx)
	if _, err := session.ActivateTurn(f.ctx, db.New(tx), session.TurnScope{EnvironmentID: f.environmentID, SessionID: actorID, TurnID: turn, RunID: f.runID, AttemptNumber: 1, RunGeneration: 1}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(f.ctx); err != nil {
		t.Fatal(err)
	}
	return turn
}

func grantRecoveryRestore(t *testing.T, f runPlacementFixture) pgtype.UUID {
	t.Helper()
	candidate := f.candidate()
	candidate.ExpectedRunRevision = 3
	reserved, err := f.authority.PlaceReadyRun(f.ctx, candidate)
	if err != nil {
		t.Fatal(err)
	}
	markRunPlacementRuntimeReady(t, f, reserved.RuntimeInstanceID)
	mount, err := f.authority.PlaceReadyRun(f.ctx, candidate)
	if err != nil {
		t.Fatal(err)
	}
	markRunPlacementMountReady(t, f, mount.WorkspaceMountID)
	grant, err := f.authority.PlaceReadyRun(f.ctx, candidate)
	if err != nil {
		t.Fatal(err)
	}
	return grant.Lease.ID
}

func expireRecoveryLease(t *testing.T, f runPlacementFixture, lease pgtype.UUID) {
	t.Helper()
	dbtest.MustExec(t, f.ctx, f.pool, `WITH expired AS (
 UPDATE run_leases SET created_at=LEAST(created_at,now()-interval '1 second'),start_deadline_at=now()-interval '2 milliseconds',expires_at=now()-interval '1 millisecond' WHERE id=$1 RETURNING id,expires_at
) UPDATE workspace_leases SET expires_at=expired.expires_at FROM expired WHERE owner_run_lease_id=expired.id`, lease)
}

// Seed another ordinary Task in the same environment; placement, restore and
// recovery still use their real owners. No second database or recovery model.
func addRecoveryTask(t *testing.T, source runPlacementFixture, runID uuid.UUID) runPlacementFixture {
	t.Helper()
	f := source
	f.runID, f.workspaceID = runID, uuid.NewV7()
	version := uuid.NewV7()
	tx, err := f.pool.Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(f.ctx)
	dbtest.MustExec(t, f.ctx, tx, `SET CONSTRAINTS ALL DEFERRED`)
	dbtest.MustExec(t, f.ctx, tx, `INSERT INTO workspaces(id,environment_id,region_id,sandbox_declared_id,deployment_definition_id,owner_run_id,ownership_generation,head_version_id)
 SELECT $1,environment_id,region_id,sandbox_declared_id,deployment_definition_id,$2,1,$3 FROM workspaces WHERE id=$4`, f.workspaceID, f.runID, version, source.workspaceID)
	dbtest.MustExec(t, f.ctx, tx, `INSERT INTO workspace_versions(id,environment_id,workspace_id,content_digest,status,ownership_generation,writer_generation,published_at)
 SELECT $1,environment_id,$2,content_digest,'committed',0,0,now() FROM workspace_versions WHERE id=(SELECT head_version_id FROM workspaces WHERE id=$3)`, version, f.workspaceID, source.workspaceID)
	dbtest.MustExec(t, f.ctx, tx, `INSERT INTO runs(id,org_id,project_id,environment_id,deployment_id,deployment_definition_id,entrypoint_kind,entrypoint_declared_id,cause_kind,workspace_id,base_workspace_version_id,payload,queue_name,queue_origin_at,queue_score_at,max_active_duration_ms,retry_policy,trace_id,root_span_id)
 SELECT $1,org_id,project_id,environment_id,deployment_id,deployment_definition_id,'task',entrypoint_declared_id,'api',$2,$3,'{}',queue_name,now(),now(),max_active_duration_ms,retry_policy,trace_id,root_span_id FROM runs WHERE id=$4`, f.runID, f.workspaceID, version, source.runID)
	dbtest.MustExec(t, f.ctx, tx, `INSERT INTO run_attempts(run_id,number,entrypoint_kind,workspace_id,base_workspace_version_id) VALUES($1,1,'task',$2,$3)`, f.runID, f.workspaceID, version)
	if err := tx.Commit(f.ctx); err != nil {
		t.Fatal(err)
	}
	return f
}
