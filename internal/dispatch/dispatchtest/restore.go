// Package dispatchtest builds restores of ready Computer captures for
// dispatch's restore commit and acknowledgement.
package dispatchtest

import (
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/computer/computertest"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/disk"
	"github.com/helmrdotdev/helmr/internal/dispatch"
	"github.com/helmrdotdev/helmr/internal/run/runtest"
)

// Restore completes a ready capture and allocates the restoring Instance its
// restore commits into; see RestoreReadyCapture.
func Restore(t *testing.T, idle bool, setup ...func(runtest.Fixture, runtest.RunLease)) (runtest.Fixture, *dispatch.Authority, computer.InstanceRef) {
	t.Helper()
	f, ref, manifest, objects := computertest.ReadyCapture(t, idle, setup...)
	return RestoreReadyCapture(t, f, ref, manifest, objects)
}

// RestoreReadyCapture completes the capture, reclaims its source Instance and
// allocates the ready, restoring Instance of the next writer generation.
func RestoreReadyCapture(t *testing.T, f runtest.Fixture, ref computer.CheckpointRef, manifest computer.CheckpointManifest, objects computertest.Objects) (runtest.Fixture, *dispatch.Authority, computer.InstanceRef) {
	t.Helper()
	checkpoint := computertest.Complete(t, f, ref, manifest, objects)
	tx, err := f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	dbtest.MustExec(t, t.Context(), tx, `UPDATE computer_instances SET observed_state='closed',observed_desired_version=desired_version,mount_state='unmounted',unmounted_at=now(),terminal_at=now(),reclaimed_at=now(),reclaim_evidence='{"method":"session_closed"}',terminal_reason_code='checkpointed' WHERE id=$1`, ref.InstanceID)
	dbtest.MustExec(t, t.Context(), tx, `UPDATE run_leases SET process_reconciled_at=now() WHERE computer_instance_id=$1`, ref.InstanceID)
	instance := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), tx, `INSERT INTO computer_instances(id,org_id,project_id,environment_id,region_id,worker_group_id,worker_host_id,worker_epoch,vm_platform_id,computer_spec_id,vm_vcpu_count,cpu_config_digest,reserved_cpu_millis,reserved_memory_bytes,reserved_guest_ephemeral_disk_bytes,reserved_execution_slots,computer_id,program_deployment_id,preparation_expires_at,desired_reason,writer_generation,writer_token_hash,writer_expires_at,admission_state,source_checkpoint_id,source_disk_version_id,observed_state,observed_version,observed_desired_version,ready_at,mount_state,mounted_at)
 SELECT $2,org_id,project_id,environment_id,region_id,worker_group_id,worker_host_id,worker_epoch,vm_platform_id,computer_spec_id,vm_vcpu_count,cpu_config_digest,reserved_cpu_millis,reserved_memory_bytes,reserved_guest_ephemeral_disk_bytes,reserved_execution_slots,computer_id,program_deployment_id,now()+interval '5 minutes','restore',writer_generation+1,decode(repeat('03',32),'hex'),now()+interval '10 minutes','restoring',$3,$4,'ready',1,1,now(),'mounted',now() FROM computer_instances WHERE id=$1`, ref.InstanceID, instance, checkpoint.ID, checkpoint.PrivateComputerDiskVersionID)
	dbtest.MustExec(t, t.Context(), tx, `UPDATE computers SET writer_generation=writer_generation+1 WHERE id=$1`, checkpoint.ComputerID)
	if err = tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	key, err := disk.NewFencingKey(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	authority, err := dispatch.NewRunAuthority(f.Pool, key)
	if err != nil {
		t.Fatal(err)
	}
	return f, authority, computer.InstanceRef{Host: computer.Host{GroupID: runtest.WorkerGroupID, HostID: f.WorkerID, Epoch: 1}, ID: instance, DesiredVersion: 1}
}
