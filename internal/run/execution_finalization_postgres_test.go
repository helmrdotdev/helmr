package run

import (
	"context"
	"errors"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run/runtest"
	"github.com/jackc/pgx/v5"
)

func finalizationExecutionFixture(t *testing.T, actor bool) (runtest.Fixture, ExecutionFinalization) {
	t.Helper()
	f, work, fence := executionClaimFixture(t)
	if actor {
		f.ConvertToActor(t, t.Context(), work, `{"enabled":false}`)
	}
	tx, err := f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	a, err := ClaimExecution(t.Context(), tx, fence)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = StartExecution(t.Context(), tx, fence); err != nil {
		t.Fatal(err)
	}
	if err = EnterExecution(t.Context(), tx, fence, a.Run().EntrypointKind, a.Run().EntrypointDeclaredID); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	return f, ExecutionFinalization{Fence: fence, RunID: pgvalue.UUID(work.RunID), AttemptNumber: 1, OperationID: pgvalue.UUID(uuid.NewV7()), Fingerprint: dbtest.Digest("member-finalization")}
}

func finalizeExecutionTest(t *testing.T, f runtest.Fixture, request ExecutionFinalization, commit bool) (Execution, error) {
	t.Helper()
	tx, err := f.Pool.Begin(t.Context())
	if err != nil {
		return Execution{}, err
	}
	defer tx.Rollback(context.Background())
	a, err := BeginExecutionFinalization(t.Context(), tx, request)
	if err == nil && commit {
		err = tx.Commit(t.Context())
	}
	return a, err
}

func TestExecutionFinalizationReplayAndPhysicalIndependence(t *testing.T) {
	for _, actor := range []bool{false, true} {
		name := "Task"
		if actor {
			name = "Actor"
		}
		t.Run(name, func(t *testing.T) {
			f, request := finalizationExecutionFixture(t, actor)
			var before string
			if err := f.Pool.QueryRow(t.Context(), `SELECT row_to_json(i)::text FROM computer_instances i JOIN run_leases l ON l.computer_instance_id=i.id WHERE l.id=$1`, request.Fence.LeaseID).Scan(&before); err != nil {
				t.Fatal(err)
			}
			if _, err := finalizeExecutionTest(t, f, request, false); err != nil {
				t.Fatal(err)
			}
			var running bool
			if err := f.Pool.QueryRow(t.Context(), `SELECT l.status='running' AND r.active_started_at IS NOT NULL FROM run_leases l JOIN runs r ON r.id=l.run_id WHERE l.id=$1`, request.Fence.LeaseID).Scan(&running); err != nil || !running {
				t.Fatalf("rollback running=%v err=%v", running, err)
			}
			a, err := finalizeExecutionTest(t, f, request, true)
			if err != nil {
				t.Fatal(err)
			}
			replay, err := finalizeExecutionTest(t, f, request, true)
			if err != nil {
				t.Fatal(err)
			}
			if a.Lease().Status != db.RunLeaseStatusFinalizing || a.Run().ActiveStartedAt.Valid || a.Lease().ProcessReconciledAt.Valid || a.Lease().ExpiresAt != replay.Lease().ExpiresAt || a.Lease().FinalizationStartedAt != replay.Lease().FinalizationStartedAt || a.Run().Revision != replay.Run().Revision {
				t.Fatal("incorrect member finalization receipt")
			}
			var after string
			if err = f.Pool.QueryRow(t.Context(), `SELECT row_to_json(i)::text FROM computer_instances i WHERE id=$1`, a.Instance().ID).Scan(&after); err != nil {
				t.Fatal(err)
			}
			if before != after {
				t.Fatal("member finalization changed Computer Instance")
			}
			changed := request
			changed.OperationID = pgvalue.UUID(uuid.NewV7())
			if _, err = finalizeExecutionTest(t, f, changed, true); !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("different operation replay=%v", err)
			}
			changed = request
			changed.Fingerprint = dbtest.Digest("changed-finalization")
			if _, err = finalizeExecutionTest(t, f, changed, true); !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("different payload replay=%v", err)
			}
		})
	}
}

func TestExecutionFinalizationRejectsInvalidOrExpiredMember(t *testing.T) {
	for _, test := range []struct{ name, sql string }{
		{"unentered", `UPDATE run_attempts SET entrypoint_entered_at=NULL`},
		{"expired lease", `UPDATE run_leases SET created_at=clock_timestamp()-interval '2 minutes',start_deadline_at=clock_timestamp()-interval '1 minute',expires_at=clock_timestamp()-interval '1 second'`},
		{"expired writer", `UPDATE computer_instances SET writer_expires_at=clock_timestamp()-interval '1 second'`},
		{"active budget", `UPDATE runs SET active_started_at=clock_timestamp()-interval '10 seconds',max_active_duration_ms=5000`},
		{"unresolved wait", `INSERT INTO run_waits(id,environment_id,run_id,computer_id,kind,due_at,expected_run_revision,attempt_number,current_run_lease_id) SELECT gen_random_uuid(),environment_id,id,computer_id,'timer',clock_timestamp()+interval '1 hour',revision,current_attempt_number,current_run_lease_id FROM runs`},
	} {
		t.Run(test.name, func(t *testing.T) {
			f, request := finalizationExecutionFixture(t, false)
			dbtest.MustExec(t, t.Context(), f.Pool, test.sql)
			if _, err := finalizeExecutionTest(t, f, request, true); !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("rejection=%v", err)
			}
		})
	}
}

func TestExecutionFinalizationRechecksWriterAfterBlockedWrite(t *testing.T) {
	f, request := finalizationExecutionFixture(t, false)
	dbtest.MustExec(t, t.Context(), f.Pool, `CREATE FUNCTION delay_member_finalization() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
 IF NEW.status='finalizing' THEN
 UPDATE computer_instances SET writer_expires_at=clock_timestamp()+interval '100 milliseconds' WHERE id=NEW.computer_instance_id;
 PERFORM pg_advisory_xact_lock(91827366);
 END IF; RETURN NEW; END $$`)
	dbtest.MustExec(t, t.Context(), f.Pool, `CREATE TRIGGER delay_member_finalization BEFORE UPDATE ON run_leases FOR EACH ROW EXECUTE FUNCTION delay_member_finalization()`)
	blocker, err := f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback(context.Background())
	dbtest.MustExec(t, t.Context(), blocker, `SELECT pg_advisory_xact_lock(91827366)`)
	done := make(chan error, 1)
	go func() { _, err := finalizeExecutionTest(t, f, request, true); done <- err }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var blocked bool
		if err = f.Pool.QueryRow(t.Context(), `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND wait_event='advisory' AND query LIKE '%UPDATE run_leases%')`).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("finalization did not block")
		}
		time.Sleep(time.Millisecond)
	}
	time.Sleep(110 * time.Millisecond)
	if err = blocker.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err = <-done; !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("expired writer accepted=%v", err)
	}
	var running bool
	if err = f.Pool.QueryRow(t.Context(), `SELECT l.status='running' AND r.active_started_at IS NOT NULL AND l.finalization_operation_id IS NULL FROM run_leases l JOIN runs r ON r.id=l.run_id WHERE l.id=$1`, request.Fence.LeaseID).Scan(&running); err != nil || !running {
		t.Fatalf("rollback=%v err=%v", running, err)
	}
}

func childFinalizationFixture(t *testing.T) (runtest.Fixture, ExecutionFinalization, ExecutionFinalization) {
	t.Helper()
	f, parent := finalizationExecutionFixture(t, false)
	childID, leaseID, waitID, claimID := uuid.NewV7(), uuid.NewV7(), uuid.NewV7(), uuid.NewV7()
	tx, err := f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	dbtest.MustExec(t, t.Context(), tx, `SET CONSTRAINTS ALL DEFERRED`)
	dbtest.MustExec(t, t.Context(), tx, `INSERT INTO idempotency_claims(id,environment_id,operation,slot_hash,request_fingerprint,accepted_at) VALUES($1,$2,'task.child.invoke',$3,$3,now())`, claimID, f.EnvironmentID, dbtest.Hash("resident-child"))
	dbtest.MustExec(t, t.Context(), tx, `INSERT INTO runs(id,org_id,project_id,environment_id,deployment_id,deployment_definition_id,entrypoint_kind,entrypoint_declared_id,cause_kind,computer_id,base_computer_disk_version_id,payload,queue_name,queue_origin_at,queue_score_at,max_active_duration_ms,retry_policy,root_span_id,parent_run_id,parent_owns_lifecycle,claim_id)
 SELECT $2,org_id,project_id,environment_id,deployment_id,deployment_definition_id,entrypoint_kind,entrypoint_declared_id,'child',computer_id,base_computer_disk_version_id,payload,queue_name,queue_origin_at,queue_score_at,max_active_duration_ms,retry_policy,root_span_id,id,true,$3 FROM runs WHERE id=$1`, parent.RunID, childID, claimID)
	dbtest.MustExec(t, t.Context(), tx, `INSERT INTO run_attempts(run_id,number,entrypoint_kind,computer_id,base_computer_disk_version_id) SELECT $2,1,entrypoint_kind,computer_id,base_computer_disk_version_id FROM runs WHERE id=$1`, parent.RunID, childID)
	dbtest.MustExec(t, t.Context(), tx, `INSERT INTO run_leases(id,org_id,project_id,environment_id,run_id,computer_id,region_id,lease_sequence,attempt_number,worker_group_id,worker_host_id,worker_epoch,computer_instance_id,writer_generation,deployment_id,requested_cpu_millis,requested_memory_bytes,requested_guest_ephemeral_disk_bytes,requested_execution_slots,status,created_at,start_deadline_at,expires_at)
 SELECT $2,org_id,project_id,environment_id,$3,computer_id,region_id,1,1,worker_group_id,worker_host_id,worker_epoch,computer_instance_id,writer_generation,deployment_id,requested_cpu_millis,requested_memory_bytes,requested_guest_ephemeral_disk_bytes,requested_execution_slots,'assigned',clock_timestamp(),clock_timestamp()+interval '1 minute',clock_timestamp()+interval '1 minute' FROM run_leases WHERE id=$1`, parent.Fence.LeaseID, leaseID, childID)
	dbtest.MustExec(t, t.Context(), tx, `UPDATE runs SET current_run_lease_id=$2,first_lease_at=clock_timestamp() WHERE id=$1`, childID, leaseID)
	dbtest.MustExec(t, t.Context(), tx, `UPDATE runs SET status='waiting' WHERE id=$1`, parent.RunID)
	dbtest.MustExec(t, t.Context(), tx, `INSERT INTO run_waits(id,environment_id,run_id,computer_id,kind,child_run_id,child_target_declared_id,child_claim_id,child_request,expected_run_revision,attempt_number,current_run_lease_id) SELECT $2,environment_id,id,computer_id,'child',$3,'test-task',$4,'{}',revision,current_attempt_number,current_run_lease_id FROM runs WHERE id=$1`, parent.RunID, waitID, childID, claimID)
	if err = tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	fence := parent.Fence
	fence.LeaseID = pgvalue.UUID(leaseID)
	tx, err = f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	a, err := ClaimExecution(t.Context(), tx, fence)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = StartExecution(t.Context(), tx, fence); err != nil {
		t.Fatal(err)
	}
	if err = EnterExecution(t.Context(), tx, fence, a.Run().EntrypointKind, a.Run().EntrypointDeclaredID); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	request := ExecutionFinalization{Fence: fence, RunID: pgvalue.UUID(childID), AttemptNumber: 1, OperationID: pgvalue.UUID(uuid.NewV7()), Fingerprint: dbtest.Digest("child-terminal")}
	if _, err = finalizeExecutionTest(t, f, request, true); err != nil {
		t.Fatal(err)
	}
	return f, parent, request
}

func TestChildExecutionFinalizationKeepsResidentParent(t *testing.T) {
	f, parent, request := childFinalizationFixture(t)
	var err error
	var parentLive bool
	err = f.Pool.QueryRow(t.Context(), `SELECT r.status='waiting' AND r.active_started_at IS NOT NULL AND r.current_run_lease_id=l.id AND l.status='running' AND l.process_reconciled_at IS NULL AND i.desired_state='ready' AND i.admission_state='open' AND i.writer_generation=l.writer_generation AND w.suspension_status='hot' AND w.condition_status='pending' AND i.capture_checkpoint_id IS NULL FROM runs r JOIN run_leases l ON l.id=r.current_run_lease_id JOIN computer_instances i ON i.id=l.computer_instance_id JOIN run_waits w ON w.current_run_lease_id=l.id WHERE r.id=$1`, parent.RunID).Scan(&parentLive)
	if err != nil || !parentLive {
		t.Fatalf("parent retained=%v err=%v", parentLive, err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE runs SET status='cancel_requested' WHERE id=$1`, parent.RunID)
	if _, err = finalizeExecutionTest(t, f, request, true); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("cancelled parent accepted child finalization replay=%v", err)
	}
}

func TestExecutionFinalizationRespectsSessionCancellationAndDrain(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		name := "draining"
		if cancelled {
			name = "Session cancelled"
		}
		t.Run(name, func(t *testing.T) {
			f, request := finalizationExecutionFixture(t, true)
			if cancelled {
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE sessions SET cancel_requested_at=clock_timestamp() WHERE current_run_id=$1`, request.RunID)
			} else {
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_instances SET admission_state='draining' WHERE id=(SELECT computer_instance_id FROM run_leases WHERE id=$1)`, request.Fence.LeaseID)
			}
			_, err := finalizeExecutionTest(t, f, request, true)
			if cancelled && !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("cancelled Session accepted=%v", err)
			}
			if !cancelled && err != nil {
				t.Fatalf("draining finalization rejected=%v", err)
			}
		})
	}
}

func TestExecutionFinalizationLeavesIndependentCommandActive(t *testing.T) {
	f, request := finalizationExecutionFixture(t, false)
	claimID, commandID := uuid.NewV7(), uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO idempotency_claims(id,environment_id,operation,slot_hash,request_fingerprint,accepted_at) VALUES($1,$2,'computer.command',$3,$3,now())`, claimID, f.EnvironmentID, dbtest.Hash("independent-command"))
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO computer_commands(id,environment_id,computer_id,claim_id,argv,env,stdin,timeout_ms,created_by_subject_type,created_by_subject_id) SELECT $2,environment_id,computer_id,$3,ARRAY['true'],'{}','',1000,'api_key','external' FROM runs WHERE id=$1`, request.RunID, commandID, claimID)
	if _, err := finalizeExecutionTest(t, f, request, true); err != nil {
		t.Fatalf("independent Command blocked Run finalization: %v", err)
	}
	var active bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT status='pending' AND terminal_at IS NULL AND cancel_requested_at IS NULL FROM computer_commands WHERE id=$1`, commandID).Scan(&active); err != nil || !active {
		t.Fatalf("Command changed during Run finalization: active=%v err=%v", active, err)
	}
}
