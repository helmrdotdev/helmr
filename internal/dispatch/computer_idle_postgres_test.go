package dispatch_test

import (
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/computer/computertest"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/disk"
	"github.com/helmrdotdev/helmr/internal/dispatch"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run/runtest"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
)

func TestIdleComputerReconcilerRequestsCapture(t *testing.T) {
	f, _, _, request := computertest.Capture(t)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE run_waits SET idle_timeout_ms=1,created_at=clock_timestamp()-interval '1 minute'`)
	key, err := disk.NewFencingKey(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	authority, err := dispatch.NewRunAuthority(f.Pool, key)
	if err != nil {
		t.Fatal(err)
	}
	count, err := authority.ReconcileComputerInstances(t.Context(), 10)
	if err != nil || count != 1 {
		t.Fatalf("reconcile=%d %v", count, err)
	}
	var captured bool
	err = f.Pool.QueryRow(t.Context(), `SELECT admission_state='checkpointing' AND capture_checkpoint_id IS NOT NULL AND desired_version=$2 FROM computer_instances WHERE id=$1`, request.InstanceID, request.DesiredVersion+1).Scan(&captured)
	if err != nil || !captured {
		t.Fatalf("capture not scheduled: %v %v", captured, err)
	}
	count, err = authority.ReconcileComputerInstances(t.Context(), 10)
	if err != nil || count != 0 {
		t.Fatalf("repeated reconcile=%d %v", count, err)
	}
}

func TestIdleComputerReconcilerAdvancesPastRejectedCandidate(t *testing.T) {
	f, _, _, request := computertest.Capture(t)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE run_waits SET idle_timeout_ms=1,created_at=clock_timestamp()-interval '1 minute'`)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computers SET writer_generation=writer_generation+1 WHERE id=(SELECT computer_id FROM computer_instances WHERE id=$1)`, request.InstanceID)
	newer := f.AddRunLease(t, "running", time.Now().Add(-time.Minute))
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE runs SET status='running',started_at=now() WHERE id=$1`, newer.RunID)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE run_leases SET status='cancelled',terminal_at=now(),terminal_reason_code='cancelled',process_reconciled_at=now() WHERE id=$1`, newer.LeaseID)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computers SET last_activity_at=clock_timestamp()-interval '1 minute' WHERE id=(SELECT computer_id FROM run_leases WHERE id=$1)`, newer.LeaseID)
	key, err := disk.NewFencingKey(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	authority, err := dispatch.NewRunAuthority(f.Pool, key)
	if err != nil {
		t.Fatal(err)
	}
	count, err := authority.ReconcileComputerInstances(t.Context(), 1)
	if err != nil || count != 1 {
		t.Fatalf("reconcile behind rejected oldest candidate = %d %v", count, err)
	}
	var captured bool
	err = f.Pool.QueryRow(t.Context(), `SELECT capture_checkpoint_id IS NOT NULL FROM computer_instances WHERE id=(SELECT computer_instance_id FROM run_leases WHERE id=$1)`, newer.LeaseID).Scan(&captured)
	if err != nil || !captured {
		t.Fatalf("newer Computer starved: %v %v", captured, err)
	}
}

func TestDrainCaptureSkipsIdleDelayAndPreservesQueuedCommands(t *testing.T) {
	for _, warm := range []bool{false, true} {
		t.Run(map[bool]string{false: "managed waits", true: "warm empty Computer"}[warm], func(t *testing.T) {
			f, work, _, request := computertest.Capture(t)
			dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE worker_hosts SET status='draining',draining_at = clock_timestamp(), drain_reason = 'shutdown' WHERE id=$1`, f.WorkerID)
			dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_instances SET admission_state='draining' WHERE id=$1`, request.InstanceID)
			dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE run_waits SET idle_timeout_ms=NULL`)
			if warm {
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE run_leases SET status='cancelled',terminal_at=now(),terminal_reason_code='cancelled',process_reconciled_at=now() WHERE computer_instance_id=$1`, request.InstanceID)
			}
			commandID, claim := uuid.NewV7(), uuid.NewV7()
			dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO idempotency_claims(id,environment_id,operation,slot_hash,request_fingerprint,accepted_at) VALUES($1,$2,'computer.command.start',$3,$3,now())`, claim, f.EnvironmentID, dbtest.Hash(claim.String()))
			dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO computer_commands(id,environment_id,computer_id,claim_id,argv,env,stdin,timeout_ms,created_by_subject_type,created_by_subject_id)
 SELECT $2,environment_id,computer_id,$3,ARRAY['true'],'{}',''::bytea,60000,'api_key',run_id::text FROM run_leases WHERE id=$1`, work.LeaseID, commandID, claim)
			// A stale event or another Host must not select this source.
			count, err := dispatch.CaptureDrainingComputers(t.Context(), f.Pool, uuid.NewV7(), request.InstanceID, 10)
			if err != nil || count != 0 {
				t.Fatalf("unrelated host: %d %v", count, err)
			}
			count, err = dispatch.CaptureDrainingComputers(t.Context(), f.Pool, f.WorkerID, request.InstanceID, 10)
			if err != nil || count != 1 {
				t.Fatalf("drain capture: %d %v", count, err)
			}
			var pending bool
			if err := f.Pool.QueryRow(t.Context(), `SELECT status='pending' AND computer_instance_id IS NULL FROM computer_commands WHERE id=$1`, commandID).Scan(&pending); err != nil || !pending {
				t.Fatalf("queued Command changed: %v %v", pending, err)
			}
			var members int
			if err := f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM computer_checkpoint_runs WHERE checkpoint_id=(SELECT capture_checkpoint_id FROM computer_instances WHERE id=$1)`, pgvalue.UUID(request.InstanceID)).Scan(&members); err != nil {
				t.Fatal(err)
			}
			want := 2
			if warm {
				want = 0
			}
			if members != want {
				t.Fatalf("sealed members=%d want=%d", members, want)
			}
			q := db.New(f.Pool)
			cp, err := q.GetComputerInstanceCaptureCheckpoint(t.Context(), db.GetComputerInstanceCaptureCheckpointParams{ComputerInstanceID: pgvalue.UUID(request.InstanceID), EnvironmentID: pgvalue.UUID(f.EnvironmentID), WorkerGroupID: pgvalue.UUID(runtest.WorkerGroupID), WorkerHostID: pgvalue.UUID(f.WorkerID), WorkerEpoch: 1, DesiredVersion: request.DesiredVersion + 1, WorkerFreshnessSeconds: workergroup.ObservationFreshnessSeconds})
			if err != nil {
				t.Fatal(err)
			}
			ref, manifest := computertest.CaptureRequest(t, f, cp)
			objects := computertest.PrepareCapture(t, f, ref, manifest)
			computertest.Complete(t, f, ref, manifest, objects)
			dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_instances SET observed_state='closed',observed_desired_version=desired_version,mount_state='unmounted',unmounted_at=now(),terminal_at=now(),reclaimed_at=now(),reclaim_evidence='{"method":"machine_closed"}',terminal_reason_code='checkpointed' WHERE id=$1`, request.InstanceID)
			dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE run_leases SET process_reconciled_at=now() WHERE computer_instance_id=$1`, request.InstanceID)
			destination := uuid.NewV7()
			dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO worker_hosts(id,resource_id,worker_group_id,worker_pool_id,status,current_epoch,current_service_id,vm_platform_id,epoch_cpu_millis,epoch_memory_bytes,epoch_guest_ephemeral_disk_bytes,per_vm_cpu_millis,per_vm_memory_bytes,per_vm_guest_ephemeral_disk_bytes,max_vm_slots,max_vm_starts,cpu_environment,cpu_environment_digest,observed_at,epoch_started_at,activated_at)
 SELECT $2::uuid,($2::uuid)::text,worker_group_id,worker_pool_id,'active',1,$3,vm_platform_id,epoch_cpu_millis,epoch_memory_bytes,epoch_guest_ephemeral_disk_bytes,per_vm_cpu_millis,per_vm_memory_bytes,per_vm_guest_ephemeral_disk_bytes,max_vm_slots,max_vm_starts,cpu_environment,cpu_environment_digest,now(),now(),now() FROM worker_hosts WHERE id=$1`, f.WorkerID, destination, uuid.NewV7())
			dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE worker_hosts SET epoch_guest_ephemeral_disk_bytes=68719476736,per_vm_guest_ephemeral_disk_bytes=34359738368 WHERE id=$1`, destination)
			key, err := disk.NewFencingKey(make([]byte, 32))
			if err != nil {
				t.Fatal(err)
			}
			authority, err := dispatch.NewRunAuthority(f.Pool, key)
			if err != nil {
				t.Fatal(err)
			}
			candidate := dispatch.CommandCandidate{OrgID: pgvalue.UUID(f.OrgID), CommandID: pgvalue.UUID(commandID), ExpectedRevision: 1}
			assigned, err := authority.AssignCommand(t.Context(), candidate)
			if err != nil {
				t.Fatalf("pending Command did not start restore: %v", err)
			}
			if assigned.ProcessBound || assigned.WorkerHostID != pgvalue.UUID(destination) {
				t.Fatalf("assignment=%+v", assigned)
			}
			instance, err := q.GetComputerInstance(t.Context(), db.GetComputerInstanceParams{EnvironmentID: pgvalue.UUID(f.EnvironmentID), ID: assigned.ComputerInstanceID})
			if err != nil {
				t.Fatal(err)
			}
			if instance.SourceCheckpointID != cp.ID || instance.WriterGeneration != cp.WriterGeneration+1 {
				t.Fatalf("restore lost checkpoint: %+v", instance)
			}
			// Report the destination mount, then exercise the real restore commit and
			// activation receipt before the queued Command can bind to its new writer.
			dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_instances SET observed_state='ready',observed_version=1,observed_desired_version=desired_version,ready_at=now(),mount_state='mounted',mounted_at=now() WHERE id=$1`, instance.ID)
			fence := computer.InstanceRef{Host: computer.Host{GroupID: runtest.WorkerGroupID, HostID: destination, Epoch: 1}, ID: pgvalue.MustUUIDValue(instance.ID), DesiredVersion: instance.DesiredVersion}
			if err := db.RunTx(t.Context(), f.Pool, func(tx pgx.Tx) error { _, err := authority.CommitRestore(t.Context(), tx, fence); return err }); err != nil {
				t.Fatal(err)
			}
			if _, err := computer.AcknowledgeRestore(t.Context(), f.Pool, fence, cp.ID, instance.WriterGeneration, installedRestoreGrants(t, f, fence)); err != nil {
				t.Fatal(err)
			}
			assigned, err = authority.AssignCommand(t.Context(), candidate)
			if err != nil || !assigned.ProcessBound || assigned.ComputerInstanceID != instance.ID {
				t.Fatalf("Command after restore=%+v %v", assigned, err)
			}
		})
	}
}
