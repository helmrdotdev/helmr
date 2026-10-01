package run_test

import (
	"context"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/disk"
	"github.com/helmrdotdev/helmr/internal/dispatch"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run"
	"github.com/helmrdotdev/helmr/internal/run/runtest"
	"github.com/helmrdotdev/helmr/internal/session"
	"github.com/helmrdotdev/helmr/internal/token"
)

// A member's terminal outcome does not prove its process has exited and must
// neither close the shared writer nor revoke another member's execution grant.
func TestCancellationPreservesSharedInstance(t *testing.T) {
	f := runtest.New(t)
	target := f.AddRunLease(t, "running", time.Now().Add(-time.Minute))
	peerID, peerLeaseID := uuid.NewV7(), uuid.NewV7()
	tx, err := f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	dbtest.MustExec(t, t.Context(), tx, `SET CONSTRAINTS ALL DEFERRED`)
	dbtest.MustExec(t, t.Context(), tx, `
        INSERT INTO runs(id,org_id,project_id,environment_id,deployment_id,
            deployment_definition_id,entrypoint_kind,entrypoint_declared_id,cause_kind,
            computer_id,base_computer_disk_version_id,payload,queue_name,queue_origin_at,
            queue_score_at,max_active_duration_ms,retry_policy,root_span_id)
        SELECT $2,org_id,project_id,environment_id,deployment_id,
            deployment_definition_id,entrypoint_kind,entrypoint_declared_id,cause_kind,
            computer_id,base_computer_disk_version_id,payload,queue_name,queue_origin_at,
            queue_score_at,max_active_duration_ms,retry_policy,root_span_id
        FROM runs WHERE id=$1`, target.RunID, peerID)
	dbtest.MustExec(t, t.Context(), tx, `
        INSERT INTO run_attempts(run_id,number,entrypoint_kind,computer_id,base_computer_disk_version_id)
        SELECT $2,1,entrypoint_kind,computer_id,base_computer_disk_version_id FROM runs WHERE id=$1`, target.RunID, peerID)
	dbtest.MustExec(t, t.Context(), tx, `
        INSERT INTO run_leases(id,org_id,project_id,environment_id,run_id,computer_id,region_id,
            lease_sequence,attempt_number,worker_group_id,worker_host_id,worker_epoch,
            computer_instance_id,writer_generation,deployment_id,requested_cpu_millis,
            requested_memory_bytes,requested_guest_ephemeral_disk_bytes,requested_execution_slots,
            status,created_at,start_deadline_at,claimed_at,started_at,expires_at)
        SELECT $2,org_id,project_id,environment_id,$3,computer_id,region_id,
            1,1,worker_group_id,worker_host_id,worker_epoch,
            computer_instance_id,writer_generation,deployment_id,requested_cpu_millis,
            requested_memory_bytes,requested_guest_ephemeral_disk_bytes,requested_execution_slots,
            status,created_at,start_deadline_at,claimed_at,started_at,expires_at
        FROM run_leases WHERE id=$1`, target.LeaseID, peerLeaseID, peerID)
	dbtest.MustExec(t, t.Context(), tx, `UPDATE runs SET current_run_lease_id=$2,status='running',started_at=now(),first_lease_at=now() WHERE id=$1`, peerID, peerLeaseID)
	dbtest.MustExec(t, t.Context(), tx, `UPDATE runs SET status='running',started_at=now() WHERE id=$1`, target.RunID)
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	canceler, err := run.NewCanceler(f.Pool)
	if err != nil {
		t.Fatal(err)
	}
	request := run.CancellationRequest{OrgID: f.OrgID, ProjectID: f.ProjectID, EnvironmentID: f.EnvironmentID, RunID: target.RunID}
	result, err := canceler.Cancel(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Changed || result.CancelledRuns != 1 {
		t.Fatalf("unexpected cancellation: %+v", result)
	}
	var targetStatus, peerStatus, instanceStatus, admission, mount string
	var generation int64
	var unreconciled, liveWriter bool
	err = f.Pool.QueryRow(t.Context(), `
        SELECT target.status,peer.status,i.observed_state,i.admission_state,i.mount_state,
            i.writer_generation,target.process_reconciled_at IS NULL,
            i.reclaimed_at IS NULL AND i.writer_expires_at>now()
        FROM run_leases target JOIN computer_instances i ON i.id=target.computer_instance_id
        JOIN run_leases peer ON peer.computer_instance_id=i.id AND peer.id=$2
        WHERE target.id=$1`, target.LeaseID, peerLeaseID).Scan(&targetStatus, &peerStatus, &instanceStatus, &admission, &mount, &generation, &unreconciled, &liveWriter)
	if err != nil {
		t.Fatal(err)
	}
	if targetStatus != "cancelled" || peerStatus != "running" || instanceStatus != "ready" || admission != "open" || mount != "mounted" || generation != 2 || !unreconciled || !liveWriter {
		t.Fatalf("cancellation crossed the member boundary: target=%s peer=%s instance=%s admission=%s mount=%s generation=%d unreconciled=%v writer=%v", targetStatus, peerStatus, instanceStatus, admission, mount, generation, unreconciled, liveWriter)
	}
	replay, err := canceler.Cancel(t.Context(), request)
	if err != nil || replay.Changed {
		t.Fatalf("cancellation replay: %+v, %v", replay, err)
	}
}

func TestRestoringTimerConditionWaitsForActivation(t *testing.T) {
	f := runtest.New(t)
	work := f.AddRunLease(t, "running", time.Now().Add(-time.Minute))
	waitID, newLease := restoringTimer(t, f, work)
	reconciler, err := run.NewTimerWaitReconciler(f.Pool)
	if err != nil {
		t.Fatal(err)
	}
	count, err := reconciler.ReconcileDue(t.Context(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("resolved %d timers", count)
	}
	var condition, suspension string
	var current uuid.UUID
	if err := f.Pool.QueryRow(t.Context(), `SELECT condition_status,suspension_status,current_run_lease_id FROM run_waits WHERE id=$1`, waitID).Scan(&condition, &suspension, &current); err != nil {
		t.Fatal(err)
	}
	if condition != "completed" || suspension != "resuming" || current != newLease {
		t.Fatalf("condition crossed activation barrier: %s %s %s", condition, suspension, current)
	}
}

func restoringTimer(t *testing.T, f runtest.Fixture, work runtest.RunLease) (uuid.UUID, uuid.UUID) {
	t.Helper()
	ctx := t.Context()
	tx, err := f.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	dbtest.MustExec(t, ctx, tx, `SET CONSTRAINTS ALL DEFERRED`)
	checkpoint, private, instance, newLease, waitID := uuid.NewV7(), uuid.NewV7(), uuid.NewV7(), uuid.NewV7(), uuid.NewV7()
	var computerID, sourceID uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT computer_id,computer_instance_id FROM run_leases WHERE id=$1`, work.LeaseID).Scan(&computerID, &sourceID); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, ctx, tx, `INSERT INTO computer_checkpoints(id,computer_id,environment_id,computer_spec_id,source_computer_instance_id,writer_generation,membership_revision,program_deployment_id,base_computer_disk_version_id)
 SELECT $2,i.computer_id,i.environment_id,i.computer_spec_id,i.id,i.writer_generation,i.membership_revision,i.program_deployment_id,c.head_disk_version_id
 FROM computer_instances i JOIN computers c ON c.id=i.computer_id WHERE i.id=$1`, sourceID, checkpoint)
	dbtest.MustExec(t, ctx, tx, `INSERT INTO computer_disk_versions(id,environment_id,computer_id,parent_version_id,root_pack_digest,logical_bytes,status,source_computer_instance_id,writer_generation)
 SELECT $2,environment_id,computer_id,base_computer_disk_version_id,$3,4096,'private',source_computer_instance_id,writer_generation FROM computer_checkpoints WHERE id=$1`, checkpoint, private, dbtest.Digest("restored-private"))
	dbtest.InsertComputerGeneration(t, ctx, tx, f.EnvironmentID, computerID, private)
	artifacts := dbtest.InsertCheckpointArtifacts(t, ctx, tx, work.RunID, "restore-timer")
	dbtest.MustExec(t, ctx, tx, `UPDATE computer_checkpoints SET status='ready',ready_at=now(),private_computer_disk_version_id=$2,
 ready_request_fingerprint=$3,manifest='{"version":1}',vm_config_artifact_id=$4,vm_state_artifact_id=$5,memory_artifact_id=$6,scratch_disk_artifact_id=$7 WHERE id=$1`, checkpoint, private, dbtest.Digest("restore-ready"), artifacts.RuntimeConfig, artifacts.VMState, artifacts.Memory, artifacts.ScratchDisk)
	dbtest.MustExec(t, ctx, tx, `UPDATE computer_instances SET desired_state='closed',desired_version=2,admission_state='closed',observed_state='closed',mount_state='unmounted',unmounted_at=now(),terminal_at=now(),reclaimed_at=now(),reclaim_evidence='{}',terminal_reason_code='checkpointed' WHERE id=$1`, sourceID)
	dbtest.MustExec(t, ctx, tx, `UPDATE run_leases SET status='checkpointed',checkpointed_at=now(),terminal_at=now(),terminal_reason_code='checkpointed',process_reconciled_at=now() WHERE id=$1`, work.LeaseID)
	dbtest.MustExec(t, ctx, tx, `INSERT INTO computer_instances(id,org_id,project_id,environment_id,region_id,worker_group_id,worker_host_id,worker_epoch,
 vm_platform_id,computer_spec_id,vm_vcpu_count,cpu_config_digest,reserved_cpu_millis,reserved_memory_bytes,reserved_guest_ephemeral_disk_bytes,reserved_execution_slots,
 computer_id,program_deployment_id,preparation_expires_at,desired_reason,observed_state,ready_at,observed_version,observed_desired_version,
 writer_generation,writer_token_hash,writer_expires_at,admission_state,mount_state,mounted_at,source_checkpoint_id,source_disk_version_id)
 SELECT $2,org_id,project_id,environment_id,region_id,worker_group_id,worker_host_id,worker_epoch,
 vm_platform_id,computer_spec_id,vm_vcpu_count,cpu_config_digest,reserved_cpu_millis,reserved_memory_bytes,reserved_guest_ephemeral_disk_bytes,reserved_execution_slots,
 computer_id,program_deployment_id,now()+interval '5 minutes','restore','ready',now(),1,1,
 3,decode(repeat('03',32),'hex'),now()+interval '10 minutes','restoring','mounted',now(),$3,$4 FROM computer_instances WHERE id=$1`, sourceID, instance, checkpoint, private)
	dbtest.MustExec(t, ctx, tx, `UPDATE computer_checkpoints SET resume_computer_instance_id=$2,resume_committed_at=now() WHERE id=$1`, checkpoint, instance)
	dbtest.MustExec(t, ctx, tx, `UPDATE computers SET writer_generation=3 WHERE id=$1`, computerID)
	dbtest.MustExec(t, ctx, tx, `INSERT INTO run_leases(id,org_id,project_id,environment_id,run_id,computer_id,region_id,lease_sequence,attempt_number,worker_group_id,worker_host_id,worker_epoch,
 computer_instance_id,writer_generation,deployment_id,requested_cpu_millis,requested_memory_bytes,requested_guest_ephemeral_disk_bytes,requested_execution_slots,
 status,start_deadline_at,claimed_at,started_at,expires_at)
 SELECT $2,org_id,project_id,environment_id,run_id,computer_id,region_id,2,attempt_number,worker_group_id,worker_host_id,worker_epoch,
 $3,3,deployment_id,requested_cpu_millis,requested_memory_bytes,requested_guest_ephemeral_disk_bytes,requested_execution_slots,
 'running',now()+interval '5 minutes',now(),now(),now()+interval '10 minutes' FROM run_leases WHERE id=$1`, work.LeaseID, newLease, instance)
	dbtest.MustExec(t, ctx, tx, `UPDATE runs SET status='waiting',current_run_lease_id=$2,active_started_at=now() WHERE id=$1`, work.RunID, newLease)
	dbtest.MustExec(t, ctx, tx, `INSERT INTO run_waits(id,environment_id,run_id,computer_id,kind,due_at,expected_run_revision,attempt_number,current_run_lease_id,prior_run_lease_id,suspend_checkpoint_id,suspension_status)
 SELECT $2,environment_id,id,computer_id,'timer',now()-interval '1 second',revision,1,$3,$4,$5,'resuming' FROM runs WHERE id=$1`, work.RunID, waitID, newLease, work.LeaseID, checkpoint)
	dbtest.MustExec(t, ctx, tx, `INSERT INTO computer_checkpoint_runs(checkpoint_id,environment_id,computer_id,run_id,attempt_number,run_wait_id,source_run_lease_id,source_computer_instance_id,writer_generation)
 SELECT $2,environment_id,computer_id,run_id,attempt_number,$3,id,computer_instance_id,writer_generation FROM run_leases WHERE id=$1`, work.LeaseID, checkpoint, waitID)
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return waitID, newLease
}

func TestRestoringTokenAcceptsCompletion(t *testing.T) {
	f := runtest.New(t)
	work := f.AddRunLease(t, "running", time.Now().Add(-time.Minute))
	waitID, _ := restoringTimer(t, f, work)
	tokenID := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO tokens(id,org_id,project_id,environment_id,expires_at,callback_secret_fingerprint,status,completed_at,completion_fingerprint,result)
 VALUES($1,$2,$3,$4,now()+interval '1 hour',$5,'completed',now(),$5,'{"ready":true}')`, tokenID, f.OrgID, f.ProjectID, f.EnvironmentID, dbtest.Hash("token-completed"))
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE run_waits SET kind='token',due_at=NULL,token_id=$2,token_registration_run_revision=1 WHERE id=$1`, waitID, tokenID)
	reconciler, err := token.NewWaitReconciler(f.Pool)
	if err != nil {
		t.Fatal(err)
	}
	result, err := reconciler.ReconcileBatch(t.Context(), f.EnvironmentID, tokenID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if result.Resolved != 1 {
		t.Fatalf("token result: %+v", result)
	}
	var preserved bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT condition_status='completed' AND condition_result='{"ready":true}'::jsonb AND suspension_status='resuming' FROM run_waits WHERE id=$1`, waitID).Scan(&preserved); err != nil {
		t.Fatal(err)
	}
	if !preserved {
		t.Fatal("token result did not preserve restore barrier")
	}
}

func TestRestoringParentAcceptsChildCancellation(t *testing.T) {
	f := runtest.New(t)
	parent := f.AddRunLease(t, "running", time.Now().Add(-time.Minute))
	waitID, _ := restoringTimer(t, f, parent)
	child := f.AddRunLease(t, "running", time.Now().Add(-time.Minute))
	claimID := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO idempotency_claims(id,environment_id,operation,slot_hash,request_fingerprint,accepted_at) VALUES($1,$2,'task.child.invoke',$3,$3,now())`, claimID, f.EnvironmentID, dbtest.Hash("restoring-child"))
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE runs SET cause_kind='child',parent_run_id=$2,parent_owns_lifecycle=true,claim_id=$3 WHERE id=$1`, child.RunID, parent.RunID, claimID)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE run_waits SET kind='child',due_at=NULL,child_run_id=$2,child_target_declared_id='test-task',child_claim_id=$3,child_request='{}' WHERE id=$1`, waitID, child.RunID, claimID)
	canceler, err := run.NewCanceler(f.Pool)
	if err != nil {
		t.Fatal(err)
	}
	result, err := canceler.Cancel(t.Context(), run.CancellationRequest{OrgID: f.OrgID, ProjectID: f.ProjectID, EnvironmentID: f.EnvironmentID, RunID: child.RunID})
	if err != nil {
		t.Fatal(err)
	}
	if result.CancelledRuns != 1 {
		t.Fatalf("child result: %+v", result)
	}
	var preserved bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT condition_status='completed' AND condition_result IS NOT NULL AND suspension_status='resuming' FROM run_waits WHERE id=$1`, waitID).Scan(&preserved); err != nil {
		t.Fatal(err)
	}
	if !preserved {
		t.Fatal("child outcome did not preserve restore barrier")
	}
}

func TestIdleSessionClosesOnStoppedComputer(t *testing.T) {
	f := runtest.New(t)
	work := f.AddRunLease(t, "running", time.Now().Add(-time.Minute))
	sessionID := f.ConvertToActor(t, t.Context(), work, `{"enabled":false}`)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE run_leases SET status='completed',terminal_at=now(),terminal_reason_code='completed',process_reconciled_at=now() WHERE id=$1`, work.LeaseID)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE runs SET status='succeeded',terminal_at=now(),current_run_lease_id=NULL WHERE id=$1`, work.RunID)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE sessions SET status='closing',close_sequence=1,next_input_sequence=2,current_run_id=NULL WHERE id=$1`, sessionID)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computers SET desired_state='stopped' WHERE id=(SELECT computer_id FROM runs WHERE id=$1)`, work.RunID)
	tx, err := f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	q := db.New(tx)
	sessionRow, err := q.GetSession(t.Context(), db.GetSessionParams{EnvironmentID: pgvalue.UUID(f.EnvironmentID), ID: pgvalue.UUID(sessionID)})
	if err != nil {
		t.Fatal(err)
	}
	closed, deferred, err := session.ReconcileClose(t.Context(), tx, sessionRow, nil)
	if err != nil {
		t.Fatal(err)
	}
	if deferred || closed.Status != "closed" {
		t.Fatalf("idle close: %s deferred=%v", closed.Status, deferred)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestUnenteredSessionCancellationDoesNotWaitForInitialDisk(t *testing.T) {
	f := runtest.New(t)
	work := f.AddRunLease(t, "assigned", time.Now().Add(-time.Minute))
	sessionID := f.ConvertToActor(t, t.Context(), work, `{"enabled":false}`)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE run_leases SET status='cancelled',terminal_at=now(),terminal_reason_code='run_cancelled',process_reconciled_at=now() WHERE id=$1`, work.LeaseID)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE runs SET status='cancelled',terminal_at=now(),current_run_lease_id=NULL,failure='{"code":"run_cancelled","message":"Run was cancelled","details":{}}' WHERE id=$1`, work.RunID)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE run_attempts SET terminal_outcome='cancelled',terminal_at=now(),terminal_reason_code='run_cancelled' WHERE run_id=$1`, work.RunID)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE sessions SET status='closing',close_sequence=1,next_input_sequence=2,cancel_requested_at=now(),dispatch_hold_id=$2,dispatch_hold_reason='interrupt_requested',dispatch_hold_run_id=current_run_id,dispatch_hold_attempt_number=1,dispatch_hold_run_generation=run_generation WHERE id=$1`, sessionID, uuid.NewV7())
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_disk_versions SET status='initializing',root_pack_digest=NULL,logical_bytes=0,published_at=NULL,publisher_computer_instance_id=NULL,publisher_desired_version=NULL,publication_request_fingerprint=NULL WHERE id=(SELECT base_computer_disk_version_id FROM runs WHERE id=$1)`, work.RunID)
	reconciler, err := session.NewReconciler(f.Pool)
	if err != nil {
		t.Fatal(err)
	}
	deferred, err := reconciler.ReconcileLifecycle(t.Context(), f.EnvironmentID, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if deferred {
		t.Fatal("cancelled Session waits for initial disk")
	}
	var status string
	var claimedDisk bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT s.status,EXISTS(SELECT 1 FROM session_events e WHERE e.session_id=s.id AND e.kind='session.held' AND e.computer_disk_version_id IS NOT NULL) FROM sessions s WHERE s.id=$1`, sessionID).Scan(&status, &claimedDisk); err != nil {
		t.Fatal(err)
	}
	if status != "closed" || claimedDisk {
		t.Fatalf("initial cancellation: %s claimed disk=%v", status, claimedDisk)
	}
}

func TestExhaustedRestoreInvalidatesCheckpointAndFailsParkedRun(t *testing.T) {
	for _, beforeCommit := range []bool{false, true} {
		t.Run(map[bool]string{false: "consumed", true: "unconsumed"}[beforeCommit], func(t *testing.T) {
			f := runtest.New(t)
			work := f.AddRunLease(t, "running", time.Now().Add(-time.Minute))
			waitID, lease := restoringTimer(t, f, work)
			if beforeCommit {
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_checkpoints SET resume_committed_at=NULL,resume_computer_instance_id=NULL WHERE id=(SELECT suspend_checkpoint_id FROM run_waits WHERE id=$1)`, waitID)
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE run_waits SET current_run_lease_id=NULL,suspension_status='parked' WHERE id=$1`, waitID)
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE runs SET current_run_lease_id=NULL,active_started_at=NULL WHERE id=$1`, work.RunID)
			}
			dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computers c SET preparation_attempt_count=8,preparation_instance_id=l.computer_instance_id
 FROM run_leases l WHERE l.id=$1 AND c.id=l.computer_id`, lease)
			dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_instances SET desired_state='closed',desired_version=desired_version+1,admission_state='closed'
 WHERE id=(SELECT computer_instance_id FROM run_leases WHERE id=$1)`, lease)
			key, err := disk.NewFencingKey(make([]byte, 32))
			if err != nil {
				t.Fatal(err)
			}
			owner, err := dispatch.NewRunAuthority(f.Pool, key)
			if err != nil {
				t.Fatal(err)
			}
			for range 2 {
				if _, err = owner.ReconcileComputerInstances(t.Context(), 10); err != nil {
					t.Fatal(err)
				}
			}
			var status, code, checkpointStatus, reason string
			var consumed, retained bool
			if err = f.Pool.QueryRow(t.Context(), `SELECT r.status,r.failure->>'code',cp.status,cp.invalidation_reason_code,
 cp.resume_committed_at IS NOT NULL,c.head_disk_version_id=cp.base_computer_disk_version_id
 FROM runs r JOIN run_waits w ON w.run_id=r.id JOIN computer_checkpoints cp ON cp.id=w.suspend_checkpoint_id JOIN computers c ON c.id=r.computer_id
 WHERE w.id=$1`, waitID).Scan(&status, &code, &checkpointStatus, &reason, &consumed, &retained); err != nil {
				t.Fatal(err)
			}
			if status != "system_failed" || code != "computer_preparation_exhausted" || checkpointStatus != "invalid" || reason != code || consumed == beforeCommit || !retained {
				t.Fatalf("Run=%s/%s checkpoint=%s/%s consumed=%v retained=%v", status, code, checkpointStatus, reason, consumed, retained)
			}
		})
	}
}
