package session

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run"
	"github.com/helmrdotdev/helmr/internal/run/runtest"
	"github.com/jackc/pgx/v5"
)

func actorMemberCompletionFixture(t *testing.T, kind string) (runtest.Fixture, run.ExecutionFence, ActorCompletion) {
	t.Helper()
	f := runtest.New(t)
	work := f.AddRunLease(t, "assigned", time.Now())
	sid := f.ConvertToActor(t, t.Context(), work, `{"enabled":false}`)
	fence := run.ExecutionFence{LeaseID: pgvalue.UUID(work.LeaseID), LeaseSequence: 1, WorkerHostID: pgvalue.UUID(f.WorkerID), WorkerGroupID: pgvalue.UUID(runtest.WorkerGroupID), WorkerEpoch: 1}
	if err := f.Pool.QueryRow(t.Context(), `SELECT h.claim_version,g.claim_version FROM worker_hosts h JOIN worker_groups g ON g.id=h.worker_group_id WHERE h.id=$1`, f.WorkerID).Scan(&fence.HostClaimVersion, &fence.GroupClaimVersion); err != nil {
		t.Fatal(err)
	}
	tx, err := f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	a, err := run.ClaimExecution(t.Context(), tx, fence)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = run.StartExecution(t.Context(), tx, fence); err != nil {
		t.Fatal(err)
	}
	if err = run.EnterExecution(t.Context(), tx, fence, a.Run().EntrypointKind, a.Run().EntrypointDeclaredID); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	operation := uuid.NewV7()
	completion := ActorCompletion{Kind: ActorSucceeded, RunGeneration: a.Session().RunGeneration, OperationID: operation, Fingerprint: dbtest.Digest("Actor completion " + kind)}
	if kind != "no progress" {
		dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE sessions SET committed_input_sequence=2 WHERE id=$1`, sid)
	}
	switch kind {
	case "failure":
		completion.Kind = ActorFailed
		completion.Error = json.RawMessage(`{"details":{},"message":"failed"}`)
	case "close":
		dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE sessions SET status='closing',close_sequence=2 WHERE id=$1`, sid)
	case "continue":
		dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE sessions SET next_input_sequence=4 WHERE id=$1`, sid)
	case "interrupt":
		hold := uuid.NewV7()
		dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE sessions SET dispatch_hold_id=$2,dispatch_hold_reason='interrupt_requested',dispatch_hold_run_id=current_run_id,dispatch_hold_attempt_number=1,dispatch_hold_run_generation=run_generation WHERE id=$1`, sid, hold)
		completion.Kind = ActorInterrupted
		completion.HoldID = hold
	}
	tx, err = f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if _, err = run.BeginExecutionFinalization(t.Context(), tx, run.ExecutionFinalization{Fence: fence, RunID: pgvalue.UUID(work.RunID), AttemptNumber: 1, OperationID: pgvalue.UUID(operation), Fingerprint: dbtest.Digest("Actor finalization")}); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	return f, fence, completion
}
func completeActorMemberTest(t *testing.T, f runtest.Fixture, fence run.ExecutionFence, completion ActorCompletion, commit bool) error {
	t.Helper()
	tx, err := f.Pool.Begin(t.Context())
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	err = completeActor(t.Context(), tx, fence, completion)
	if err == nil && commit {
		err = tx.Commit(t.Context())
	}
	return err
}
func TestActorMemberCompletionPreservesComputer(t *testing.T) {
	for _, kind := range []string{"success", "failure", "no progress", "close", "continue", "interrupt"} {
		t.Run(kind, func(t *testing.T) {
			f, fence, completion := actorMemberCompletionFixture(t, kind)
			diskSnapshot := func() string {
				t.Helper()
				var value string
				if err := f.Pool.QueryRow(t.Context(), `SELECT jsonb_build_object('head',c.head_disk_version_id,'versions',(SELECT jsonb_agg(to_jsonb(v) ORDER BY v.id) FROM computer_disk_versions v WHERE v.computer_id=c.id))::text FROM computers c JOIN run_leases l ON l.computer_id=c.id WHERE l.id=$1`, fence.LeaseID).Scan(&value); err != nil {
					t.Fatal(err)
				}
				return value
			}
			diskBefore := diskSnapshot()
			var before string
			if err := f.Pool.QueryRow(t.Context(), `SELECT row_to_json(i)::text FROM computer_instances i JOIN run_leases l ON l.computer_instance_id=i.id WHERE l.id=$1`, fence.LeaseID).Scan(&before); err != nil {
				t.Fatal(err)
			}
			if err := completeActorMemberTest(t, f, fence, completion, false); err != nil {
				t.Fatal(err)
			}
			for range 2 {
				if err := completeActorMemberTest(t, f, fence, completion, true); err != nil {
					t.Fatal(err)
				}
			}
			if diskSnapshot() != diskBefore {
				t.Fatal("Actor completion or replay published a disk version or changed the saved head")
			}
			if kind == "success" || kind == "failure" {
				var committed, terminal int64
				var noHold bool
				if err := f.Pool.QueryRow(t.Context(), `SELECT s.committed_input_sequence,a.terminal_session_input_sequence,s.dispatch_hold_id IS NULL AND s.dispatch_hold_reason IS NULL FROM run_leases l JOIN runs r ON r.id=l.run_id JOIN sessions s ON s.id=r.session_id JOIN run_attempts a ON a.run_id=r.id AND a.number=l.attempt_number WHERE l.id=$1`, fence.LeaseID).Scan(&committed, &terminal, &noHold); err != nil || committed != 2 || terminal != 2 || !noHold {
					t.Fatalf("Actor cursors committed=%d terminal=%d noHold=%v: %v", committed, terminal, noHold, err)
				}
			}
			var after, status, sessionStatus string
			var unclean, continuation, held bool
			if err := f.Pool.QueryRow(t.Context(), `SELECT row_to_json(i)::text,r.status,s.status,l.process_reconciled_at IS NULL,(s.current_run_id IS NOT NULL AND s.current_run_id<>r.id),coalesce(s.dispatch_hold_reason='interrupted',false) FROM run_leases l JOIN runs r ON r.id=l.run_id JOIN sessions s ON s.id=r.session_id JOIN computer_instances i ON i.id=l.computer_instance_id WHERE l.id=$1`, fence.LeaseID).Scan(&after, &status, &sessionStatus, &unclean, &continuation, &held); err != nil {
				t.Fatal(err)
			}
			if before != after || !unclean {
				t.Fatal("member completion changed physical authority")
			}
			switch kind {
			case "failure", "no progress":
				if status != "failed" || sessionStatus != "failed" {
					t.Fatalf("status=%s Session=%s", status, sessionStatus)
				}
			case "interrupt":
				if status != "cancelled" || !held {
					t.Fatalf("status=%s held=%v", status, held)
				}
			default:
				if status != "succeeded" {
					t.Fatalf("status=%s", status)
				}
			}
			if kind == "continue" || kind == "close" {
				if continuation {
					t.Fatal("continued before process cleanup")
				}
				if kind == "close" && sessionStatus != "closing" {
					t.Fatalf("premature close=%s", sessionStatus)
				}
				var sid uuid.UUID
				if err := f.Pool.QueryRow(t.Context(), `SELECT r.session_id FROM runs r JOIN run_leases l ON l.run_id=r.id WHERE l.id=$1`, fence.LeaseID).Scan(&sid); err != nil {
					t.Fatal(err)
				}
				reconciler, err := NewReconciler(f.Pool)
				if err != nil {
					t.Fatal(err)
				}
				if deferred, err := reconciler.ReconcileLifecycle(t.Context(), f.EnvironmentID, sid); err != nil || !deferred {
					t.Fatalf("cleanup barrier=%v %v", deferred, err)
				}
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE run_leases SET process_reconciled_at=clock_timestamp() WHERE id=$1`, fence.LeaseID)
				if deferred, err := reconciler.ReconcileLifecycle(t.Context(), f.EnvironmentID, sid); err != nil || deferred {
					t.Fatalf("cleanup release=%v %v", deferred, err)
				}
				if err := f.Pool.QueryRow(t.Context(), `SELECT status,current_run_id IS NOT NULL FROM sessions WHERE id=$1`, sid).Scan(&sessionStatus, &continuation); err != nil {
					t.Fatal(err)
				}
				if kind == "continue" && !continuation {
					t.Fatal("continuation missing after cleanup")
				}
				if kind == "close" && sessionStatus != "closed" {
					t.Fatalf("settled close=%s", sessionStatus)
				}
			}

			completion.Fingerprint = dbtest.Digest("changed Actor outcome")
			if err := completeActorMemberTest(t, f, fence, completion, true); !errors.Is(err, ErrStaleCompletion) {
				t.Fatalf("changed replay=%v", err)
			}
		})
	}
}

func TestActorMemberInterruptionWaitsForTerminalChildCleanup(t *testing.T) {
	f, fence, completion := actorMemberCompletionFixture(t, "interrupt")
	child := terminalActorChild(t, f, fence)
	var err error

	if err = completeActorMemberTest(t, f, fence, completion, true); !errors.Is(err, ErrStopCleanupPending) {
		t.Fatalf("terminal child bypassed cleanup=%v", err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE run_leases SET process_reconciled_at=clock_timestamp() WHERE id=$1`, child.LeaseID)
	if err = completeActorMemberTest(t, f, fence, completion, true); err != nil {
		t.Fatalf("reconciled child rejected=%v", err)
	}
}

func TestActorMemberInterruptionSettlesActiveTurnCursor(t *testing.T) {
	f, fence, completion := actorMemberCompletionFixture(t, "interrupt")
	turn := uuid.NewV7()
	completion.TurnID = &turn
	tx, err := f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	dbtest.MustExec(t, t.Context(), tx, `SET CONSTRAINTS ALL DEFERRED`)
	dbtest.MustExec(t, t.Context(), tx, `INSERT INTO session_turns(id,environment_id,session_id,sequence,data,status,run_generation,run_id,attempt_number,interrupt_requested_at) SELECT $2,r.environment_id,r.session_id,3,'{}','running',s.run_generation,r.id,1,clock_timestamp() FROM runs r JOIN sessions s ON s.id=r.session_id JOIN run_leases l ON l.run_id=r.id WHERE l.id=$1`, fence.LeaseID, turn)
	dbtest.MustExec(t, t.Context(), tx, `UPDATE sessions SET next_input_sequence=4,active_turn_id=$2 WHERE current_run_id=(SELECT run_id FROM run_leases WHERE id=$1)`, fence.LeaseID, turn)
	if err = tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err = completeActorMemberTest(t, f, fence, completion, true); err != nil {
		t.Fatal(err)
	}
	var settled bool
	if err = f.Pool.QueryRow(t.Context(), `SELECT s.committed_input_sequence=3 AND a.terminal_session_input_sequence=3 AND t.status='interrupted' AND t.terminal_event_id IS NOT NULL AND s.active_turn_id IS NULL FROM run_leases l JOIN runs r ON r.id=l.run_id JOIN run_attempts a ON a.run_id=r.id AND a.number=l.attempt_number JOIN sessions s ON s.id=r.session_id JOIN session_turns t ON t.id=$2 WHERE l.id=$1`, fence.LeaseID, turn).Scan(&settled); err != nil || !settled {
		t.Fatalf("Turn/attempt cursor=%v %v", settled, err)
	}
}

func terminalActorChild(t *testing.T, f runtest.Fixture, fence run.ExecutionFence) runtest.RunLease {
	t.Helper()
	child := f.AddRunLease(t, "running", time.Now())
	claim := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO idempotency_claims(id,environment_id,operation,slot_hash,request_fingerprint,accepted_at) VALUES($1,$2,'task.child.invoke',$3,$3,now())`, claim, f.EnvironmentID, dbtest.Hash("stop-child"))
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE runs SET cause_kind='child',parent_run_id=(SELECT run_id FROM run_leases WHERE id=$2),parent_owns_lifecycle=true,claim_id=$3 WHERE id=$1`, child.RunID, fence.LeaseID, claim)
	tx, err := f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	dbtest.MustExec(t, t.Context(), tx, `UPDATE run_leases SET status='cancelled',terminal_at=clock_timestamp(),terminal_reason_code='run_cancelled' WHERE id=$1`, child.LeaseID)
	dbtest.MustExec(t, t.Context(), tx, `UPDATE run_attempts SET terminal_outcome='cancelled',terminal_reason_code='run_cancelled',terminal_at=clock_timestamp() WHERE run_id=$1`, child.RunID)
	dbtest.MustExec(t, t.Context(), tx, `UPDATE runs SET status='cancelled',current_run_lease_id=NULL,active_started_at=NULL,terminal_at=clock_timestamp(),failure='{"code":"run_cancelled","message":"cancelled","details":{}}' WHERE id=$1`, child.RunID)
	if err = tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}

	return child
}

func TestActorContinuationWaitsForOwnedScopeCleanup(t *testing.T) {
	for _, path := range []string{"input", "lifecycle"} {
		t.Run(path, func(t *testing.T) {
			f, fence, completion := actorMemberCompletionFixture(t, "continue")
			child := terminalActorChild(t, f, fence)
			peerID, peerLeaseID := uuid.NewV7(), uuid.NewV7()
			tx, err := f.Pool.Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(context.Background())
			dbtest.MustExec(t, t.Context(), tx, `SET CONSTRAINTS ALL DEFERRED`)
			dbtest.MustExec(t, t.Context(), tx, `INSERT INTO runs(id,org_id,project_id,environment_id,deployment_id,deployment_definition_id,entrypoint_kind,entrypoint_declared_id,cause_kind,computer_id,base_computer_disk_version_id,payload,queue_name,queue_origin_at,queue_score_at,max_active_duration_ms,retry_policy,root_span_id)
 SELECT $2,r.org_id,r.project_id,r.environment_id,r.deployment_id,$3,'task','test-task','api',r.computer_id,r.base_computer_disk_version_id,'{}',r.queue_name,r.queue_origin_at,r.queue_score_at,r.max_active_duration_ms,r.retry_policy,r.root_span_id FROM runs r JOIN run_leases l ON l.run_id=r.id WHERE l.id=$1`, fence.LeaseID, peerID, f.TaskDefinitionID)
			dbtest.MustExec(t, t.Context(), tx, `INSERT INTO run_attempts(run_id,number,entrypoint_kind,computer_id,base_computer_disk_version_id) SELECT id,1,'task',computer_id,base_computer_disk_version_id FROM runs WHERE id=$1`, peerID)
			dbtest.MustExec(t, t.Context(), tx, `INSERT INTO run_leases(id,org_id,project_id,environment_id,run_id,computer_id,region_id,lease_sequence,attempt_number,worker_group_id,worker_host_id,worker_epoch,computer_instance_id,writer_generation,deployment_id,requested_cpu_millis,requested_memory_bytes,requested_guest_ephemeral_disk_bytes,requested_execution_slots,status,created_at,start_deadline_at,expires_at)
 SELECT $2,org_id,project_id,environment_id,$3,computer_id,region_id,1,1,worker_group_id,worker_host_id,worker_epoch,computer_instance_id,writer_generation,deployment_id,requested_cpu_millis,requested_memory_bytes,requested_guest_ephemeral_disk_bytes,requested_execution_slots,'assigned',clock_timestamp(),clock_timestamp()+interval '1 minute',clock_timestamp()+interval '1 minute' FROM run_leases WHERE id=$1`, fence.LeaseID, peerLeaseID, peerID)
			dbtest.MustExec(t, t.Context(), tx, `UPDATE runs SET current_run_lease_id=$2,first_lease_at=clock_timestamp() WHERE id=$1`, peerID, peerLeaseID)
			if err = tx.Commit(t.Context()); err != nil {
				t.Fatal(err)
			}
			if err = completeActorMemberTest(t, f, fence, completion, true); err != nil {
				t.Fatal(err)
			}
			dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE run_leases SET process_reconciled_at=clock_timestamp() WHERE id=$1`, fence.LeaseID)
			var sid uuid.UUID
			if err = f.Pool.QueryRow(t.Context(), `SELECT r.session_id FROM runs r JOIN run_leases l ON l.run_id=r.id WHERE l.id=$1`, fence.LeaseID).Scan(&sid); err != nil {
				t.Fatal(err)
			}
			attempt := func() (bool, error) {
				if path == "lifecycle" {
					r, err := NewReconciler(f.Pool)
					if err != nil {
						return false, err
					}
					return r.ReconcileLifecycle(t.Context(), f.EnvironmentID, sid)
				}
				tx, err := f.Pool.Begin(t.Context())
				if err != nil {
					return false, err
				}
				defer tx.Rollback(context.Background())
				q := db.New(tx)
				a, err := q.GetSession(t.Context(), db.GetSessionParams{EnvironmentID: pgvalue.UUID(f.EnvironmentID), ID: pgvalue.UUID(sid)})
				if err != nil {
					return false, err
				}
				c, err := q.LockSessionCloseComputer(t.Context(), db.LockSessionCloseComputerParams{EnvironmentID: a.EnvironmentID, ComputerID: a.ComputerID, SessionID: a.ID})
				if err != nil {
					return false, err
				}
				if _, err = CreateContinuation(t.Context(), tx, a, c, nil); errors.Is(err, pgx.ErrNoRows) {
					return true, nil
				} else if err != nil {
					return false, err
				}
				return false, tx.Commit(t.Context())
			}
			if deferred, err := attempt(); err != nil || !deferred {
				t.Fatalf("owned child cleanup barrier=%v %v", deferred, err)
			}
			dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE run_leases SET process_reconciled_at=clock_timestamp() WHERE id=$1`, child.LeaseID)
			if deferred, err := attempt(); err != nil || deferred {
				t.Fatalf("unrelated shared member blocked continuation=%v %v", deferred, err)
			}
		})
	}
}

func TestActorMemberCompletionUsesAttemptBaseIndependentlyOfRunBase(t *testing.T) {
	f, fence, completion := actorMemberCompletionFixture(t, "success")
	var computerID, runID uuid.UUID
	var attemptBase string
	if err := f.Pool.QueryRow(t.Context(), `SELECT l.computer_id,l.run_id,a.base_computer_disk_version_id::text FROM run_leases l JOIN run_attempts a ON a.run_id=l.run_id AND a.number=l.attempt_number WHERE l.id=$1`, fence.LeaseID).Scan(&computerID, &runID, &attemptBase); err != nil {
		t.Fatal(err)
	}
	newBase := uuid.NewV7()
	tx, err := f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	dbtest.MustExec(t, t.Context(), tx, `SET CONSTRAINTS ALL DEFERRED`)
	dbtest.MustExec(t, t.Context(), tx, `INSERT INTO computer_disk_versions(id,environment_id,computer_id,parent_version_id,root_pack_digest,logical_bytes,status,writer_generation,source_computer_instance_id,publisher_computer_instance_id,publisher_desired_version,publisher_save_sequence,publication_request_fingerprint,published_at)
 SELECT $2,v.environment_id,v.computer_id,v.id,v.root_pack_digest,v.logical_bytes,'committed',i.writer_generation,i.id,i.id,i.desired_version,1,$3,now() FROM run_leases l JOIN computer_instances i ON i.id=l.computer_instance_id JOIN computers c ON c.id=i.computer_id JOIN computer_disk_versions v ON v.id=c.head_disk_version_id WHERE l.id=$1`, fence.LeaseID, newBase, dbtest.Hash(newBase.String()))
	dbtest.MustExec(t, t.Context(), tx, `INSERT INTO computer_disk_version_roots(environment_id,computer_id,version_id,locator) SELECT environment_id,computer_id,$2,locator FROM computer_disk_version_roots WHERE version_id=$1`, attemptBase, newBase)
	dbtest.MustExec(t, t.Context(), tx, `UPDATE computers SET head_disk_version_id=$2 WHERE id=$1`, computerID, newBase)
	dbtest.MustExec(t, t.Context(), tx, `UPDATE run_attempts SET base_computer_disk_version_id=$2 WHERE run_id=$1 AND number=1`, runID, newBase)
	if err = tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := completeActorMemberTest(t, f, fence, completion, true); err != nil {
		t.Fatal(err)
	}
	var preserved bool
	if err = f.Pool.QueryRow(t.Context(), `SELECT r.status='succeeded' AND r.base_computer_disk_version_id=$2::uuid AND a.base_computer_disk_version_id=$3 FROM runs r JOIN run_attempts a ON a.run_id=r.id AND a.number=r.current_attempt_number WHERE r.id=$1`, runID, attemptBase, newBase).Scan(&preserved); err != nil || !preserved {
		t.Fatalf("Actor completion altered or conflated bases: %v %v", preserved, err)
	}
}
