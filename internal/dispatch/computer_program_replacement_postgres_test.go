package dispatch_test

import (
	"errors"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/computer/computertest"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/disk"
	"github.com/helmrdotdev/helmr/internal/dispatch"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5/pgtype"
)

const (
	loseWorkerHost    = `UPDATE worker_hosts SET status='lost',lost_at=now() WHERE id=$1`
	restoreWorkerHost = `UPDATE worker_hosts SET status='active',lost_at=NULL WHERE id=$1`
)

func TestProgramReplacementPreservesCapturedDisk(t *testing.T) {
	for _, programless := range []bool{false, true} {
		name := "resident-program"
		if programless {
			name = "preparation-only"
		}
		t.Run(name, func(t *testing.T) { testProgramReplacementPreservesCapturedDisk(t, programless) })
	}
}

func testProgramReplacementPreservesCapturedDisk(t *testing.T, programless bool) {
	f, old, _, capture := computertest.Capture(t)
	key, err := disk.NewFencingKey(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	a, err := dispatch.NewRunAuthority(f.Pool, key)
	if err != nil {
		t.Fatal(err)
	}
	next, definition, runID := uuid.NewV7(), uuid.NewV7(), uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO deployments(id,org_id,project_id,environment_id,version,bundle_digest,runtime_artifact_digest,program_artifact_id,program_index_digest,queue_config) SELECT $2,org_id,project_id,environment_id,'next-program','sha256:'||repeat('a',64),runtime_artifact_digest,program_artifact_id,program_index_digest,queue_config FROM deployments WHERE id=$1`, f.DeploymentID, next)
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO deployment_definitions(id,environment_id,deployment_id,kind,declared_id,manifest_version,manifest,manifest_digest,computer_spec_id) SELECT CASE WHEN kind='task' THEN $3::uuid ELSE $4::uuid END,environment_id,$2,kind,declared_id,manifest_version,manifest,manifest_digest,computer_spec_id FROM deployment_definitions WHERE deployment_id=$1`, f.DeploymentID, next, definition, uuid.NewV7())
	tx, err := f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	dbtest.MustExec(t, t.Context(), tx, `SET CONSTRAINTS ALL DEFERRED`)
	dbtest.MustExec(t, t.Context(), tx, `INSERT INTO runs(id,org_id,project_id,environment_id,deployment_id,deployment_definition_id,entrypoint_kind,entrypoint_declared_id,cause_kind,computer_id,base_computer_disk_version_id,payload,queue_name,queue_origin_at,queue_score_at,max_active_duration_ms,retry_policy,trace_id,root_span_id)
 SELECT $2,org_id,project_id,environment_id,$3,$4,'task','test-task','api',computer_id,base_computer_disk_version_id,'{}','default',now(),now(),300000,'{"enabled":false}',trace_id,root_span_id FROM runs WHERE id=$1`, old.RunID, runID, next, definition)
	dbtest.MustExec(t, t.Context(), tx, `INSERT INTO run_attempts(run_id,number,entrypoint_kind,computer_id,base_computer_disk_version_id) SELECT id,1,entrypoint_kind,computer_id,base_computer_disk_version_id FROM runs WHERE id=$1`, runID)
	if err = tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	candidate := dispatch.RunCandidate{OrgID: pgvalue.UUID(f.OrgID), RunID: pgvalue.UUID(runID), ExpectedRunRevision: 1}
	if _, err = a.AssignRun(t.Context(), candidate); !errors.Is(err, dispatch.ErrCapacityUnavailable) {
		t.Fatalf("old live members did not block: %v", err)
	}
	// A lost host ranks after the member check, so the assignment outcome and
	// its capacity cooldown stay unchanged.
	dbtest.MustExec(t, t.Context(), f.Pool, loseWorkerHost, f.WorkerID)
	if _, err = a.AssignRun(t.Context(), candidate); !errors.Is(err, dispatch.ErrCapacityUnavailable) {
		t.Fatalf("members on a lost host did not block: %v", err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, restoreWorkerHost, f.WorkerID)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE run_leases SET status='cancelled',terminal_at=now(),terminal_reason_code='cancelled',process_reconciled_at=now() WHERE computer_instance_id=$1`, capture.InstanceID)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE runs SET status='cancelled',terminal_at=now(),failure='{"code":"cancelled","message":"Cancelled","details":{}}',current_run_lease_id=NULL,active_started_at=NULL WHERE computer_id=(SELECT computer_id FROM runs WHERE id=$1) AND id<>$1`, runID)
	dbtest.MustExec(t, t.Context(), f.Pool, loseWorkerHost, f.WorkerID)
	if _, err = a.AssignRun(t.Context(), candidate); !errors.Is(err, dispatch.ErrCandidateChanged) {
		t.Fatalf("capture on a lost host=%v", err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, restoreWorkerHost, f.WorkerID)
	if programless {
		dbtest.MustExec(t, t.Context(), f.Pool, `DELETE FROM run_waits WHERE current_run_lease_id IN (SELECT id FROM run_leases WHERE computer_instance_id=$1)`, capture.InstanceID)
		dbtest.MustExec(t, t.Context(), f.Pool, `DELETE FROM run_leases WHERE computer_instance_id=$1`, capture.InstanceID)
		dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_instances SET program_deployment_id=NULL,observed_state='allocated',observed_desired_version=0,ready_at=NULL,mount_state='pending',mounted_at=NULL WHERE id=$1`, capture.InstanceID)
		if _, err = a.AssignRun(t.Context(), candidate); err == nil {
			t.Fatal("preparing source accepted a Run")
		}
		var unchanged bool
		if err = f.Pool.QueryRow(t.Context(), `SELECT program_deployment_id IS NULL AND desired_version=1 AND capture_checkpoint_id IS NULL FROM computer_instances WHERE id=$1`, capture.InstanceID).Scan(&unchanged); err != nil || !unchanged {
			t.Fatalf("in-flight preparation changed: %v %v", unchanged, err)
		}
		dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_instances SET observed_state='ready',observed_desired_version=desired_version,ready_at=now(),mount_state='mounted',mounted_at=now() WHERE id=$1`, capture.InstanceID)
	}
	if _, err = a.AssignRun(t.Context(), candidate); !errors.Is(err, dispatch.ErrCapacityUnavailable) {
		t.Fatalf("capture start=%v", err)
	}
	var checkpointID pgtype.UUID
	if err = f.Pool.QueryRow(t.Context(), `SELECT capture_checkpoint_id FROM computer_instances WHERE id=$1`, capture.InstanceID).Scan(&checkpointID); err != nil {
		t.Fatal(err)
	}
	var cp db.ComputerCheckpoint
	tx, err = f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	var computerID pgtype.UUID
	if err = tx.QueryRow(t.Context(), `SELECT computer_id FROM runs WHERE id=$1`, runID).Scan(&computerID); err != nil {
		t.Fatal(err)
	}
	cp, err = db.New(tx).LockComputerCheckpoint(t.Context(), db.LockComputerCheckpointParams{EnvironmentID: pgvalue.UUID(f.EnvironmentID), ComputerID: computerID, CheckpointID: checkpointID})
	if err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	ref, manifest := computertest.CaptureRequest(t, f, cp)
	cp = computertest.Complete(t, f, ref, manifest, computertest.PrepareCapture(t, f, ref, manifest))
	if _, err = a.AssignRun(t.Context(), candidate); err == nil {
		t.Fatal("replacement before exclusion")
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_instances SET observed_state='closed',observed_desired_version=desired_version,mount_state='unmounted',unmounted_at=now(),terminal_at=now(),reclaimed_at=now(),reclaim_evidence='{"method":"session_closed"}',terminal_reason_code='checkpointed' WHERE id=$1`, capture.InstanceID)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computers SET writer_generation=writer_generation+1 WHERE id=$1`, computerID)
	if _, err = a.AssignRun(t.Context(), candidate); !errors.Is(err, dispatch.ErrCandidateChanged) {
		t.Fatalf("stale checkpoint generation=%v", err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computers SET writer_generation=writer_generation-1 WHERE id=$1`, computerID)
	// Unassigned work on a parked Computer must not force old RAM restoration.
	commandID, claimID := uuid.NewV7(), uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO idempotency_claims(id,environment_id,operation,slot_hash,request_fingerprint,accepted_at) VALUES($1,$2,'computer.command.start',$3,$3,now())`, claimID, f.EnvironmentID, dbtest.Hash(claimID.String()))
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO computer_commands(id,environment_id,computer_id,claim_id,argv,env,stdin,timeout_ms,created_by_subject_type,created_by_subject_id) VALUES($1,$2,$3,$4,ARRAY['true'],'{}',''::bytea,60000,'test','test')`, commandID, f.EnvironmentID, computerID, claimID)
	// Disk-only promotion cannot depend on capacity to restore the obsolete VM.
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE worker_hosts SET observed_at=now()-interval '1 hour' WHERE id=$1`, f.WorkerID)
	if _, err = a.AssignRun(t.Context(), candidate); !errors.Is(err, dispatch.ErrCapacityUnavailable) {
		t.Fatalf("disk promotion=%v", err)
	}
	var retained bool
	if err = f.Pool.QueryRow(t.Context(), `SELECT c.head_disk_version_id=cp.private_computer_disk_version_id AND c.head_disk_version_id<>cp.base_computer_disk_version_id AND v.status='committed' AND cp.status='invalid' AND cp.resume_committed_at IS NULL AND c.writer_generation=cp.writer_generation FROM computers c JOIN computer_checkpoints cp ON cp.computer_id=c.id JOIN computer_disk_versions v ON v.id=c.head_disk_version_id WHERE cp.id=$1`, cp.ID).Scan(&retained); err != nil || !retained {
		t.Fatalf("captured disk not retained: %v %v", retained, err)
	}
	if _, err = a.AssignRun(t.Context(), candidate); !errors.Is(err, dispatch.ErrCapacityUnavailable) {
		t.Fatalf("promotion replay without capacity=%v", err)
	}
	var head pgtype.UUID
	if err = f.Pool.QueryRow(t.Context(), `SELECT head_disk_version_id FROM computers WHERE id=$1`, computerID).Scan(&head); err != nil || head != cp.PrivateComputerDiskVersionID {
		t.Fatalf("promotion replay changed head: %v", err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE worker_hosts SET observed_at=now(),max_vm_slots=1,per_vm_guest_ephemeral_disk_bytes=34359738368,epoch_guest_ephemeral_disk_bytes=274877906944 WHERE id=$1`, f.WorkerID)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE worker_pools SET per_vm_guest_ephemeral_disk_bytes=34359738368,capacity_guest_ephemeral_disk_bytes=274877906944 WHERE id=(SELECT worker_pool_id FROM worker_hosts WHERE id=$1)`, f.WorkerID)
	assigned, err := a.AssignRun(t.Context(), candidate)
	if err != nil || assigned.LeaseCreated {
		t.Fatalf("fresh allocation=%+v %v", assigned, err)
	}
	instance, err := db.New(f.Pool).GetComputerInstance(t.Context(), db.GetComputerInstanceParams{EnvironmentID: pgvalue.UUID(f.EnvironmentID), ID: assigned.ComputerInstanceID})
	if err != nil {
		t.Fatal(err)
	}
	if instance.SourceCheckpointID.Valid || instance.SourceDiskVersionID != cp.PrivateComputerDiskVersionID || instance.ProgramDeploymentID != pgvalue.UUID(next) || instance.WriterGeneration <= cp.WriterGeneration {
		t.Fatalf("replacement instance=%+v", instance)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_instances SET observed_state='ready',ready_at=now(),observed_desired_version=desired_version,mount_state='mounted',mounted_at=now() WHERE id=$1`, instance.ID)
	assigned, err = a.AssignRun(t.Context(), candidate)
	if err != nil || !assigned.LeaseCreated {
		t.Fatalf("new program grant=%+v %v", assigned, err)
	}
}
