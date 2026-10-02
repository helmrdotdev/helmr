package controlplane

import (
	"context"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/computer/computerdbtest"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run/runtest"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestComputerRestoreDiscoveryProjectsPersistedCapture(t *testing.T) {
	f := runtest.New(t)
	work := f.AddRunLease(t, "running", time.Now())
	checkpoint, private, instance := uuid.NewV7(), uuid.NewV7(), uuid.NewV7()
	var source pgtype.UUID
	if err := f.Pool.QueryRow(t.Context(), `SELECT computer_instance_id FROM run_leases WHERE id=$1`, work.LeaseID).Scan(&source); err != nil {
		t.Fatal(err)
	}
	tx, err := f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	dbtest.MustExec(t, t.Context(), tx, `SET CONSTRAINTS ALL DEFERRED`)
	dbtest.MustExec(t, t.Context(), tx, `INSERT INTO computer_checkpoints(id,computer_id,environment_id,computer_spec_id,source_computer_instance_id,writer_generation,membership_revision,program_deployment_id,base_computer_disk_version_id)
 SELECT $2,i.computer_id,i.environment_id,i.computer_spec_id,i.id,i.writer_generation,i.membership_revision,i.program_deployment_id,c.head_disk_version_id FROM computer_instances i JOIN computers c ON c.id=i.computer_id WHERE i.id=$1`, source, checkpoint)
	dbtest.MustExec(t, t.Context(), tx, `INSERT INTO computer_disk_versions(id,environment_id,computer_id,parent_version_id,status,source_computer_instance_id,writer_generation)
 SELECT $2,environment_id,computer_id,base_computer_disk_version_id,'private',source_computer_instance_id,writer_generation   FROM computer_checkpoints WHERE id=$1`, checkpoint, private)
	var computerID uuid.UUID
	if err = tx.QueryRow(t.Context(), `SELECT computer_id FROM computer_instances WHERE id=$1`, source).Scan(&computerID); err != nil {
		t.Fatal(err)
	}
	computerdbtest.InsertComputerVersion(t, t.Context(), tx, f.EnvironmentID, computerID, private)
	artifacts := computerdbtest.InsertCheckpointArtifacts(t, t.Context(), tx, work.RunID, "restore-discovery")
	dbtest.MustExec(t, t.Context(), tx, `UPDATE computer_checkpoints SET status='ready',ready_at=now(),private_computer_disk_version_id=$2,ready_request_fingerprint=$3,manifest='{"version":1}',vm_config_artifact_id=$4,vm_state_artifact_id=$5,memory_artifact_id=$6,scratch_disk_artifact_id=$7 WHERE id=$1`, checkpoint, private, dbtest.Digest("restore-discovery"), artifacts.RuntimeConfig, artifacts.VMState, artifacts.Memory, artifacts.ScratchDisk)
	dbtest.MustExec(t, t.Context(), tx, `UPDATE computer_instances SET desired_state='closed',desired_version=2,admission_state='closed',observed_state='closed',mount_state='unmounted',unmounted_at=now(),terminal_at=now(),reclaimed_at=now(),reclaim_evidence='{}',terminal_reason_code='checkpointed' WHERE id=$1`, source)
	dbtest.MustExec(t, t.Context(), tx, `INSERT INTO computer_instances(id,org_id,project_id,environment_id,region_id,worker_group_id,worker_host_id,worker_epoch,vm_platform_id,computer_spec_id,vm_vcpu_count,cpu_config_digest,reserved_cpu_millis,reserved_memory_bytes,reserved_guest_ephemeral_disk_bytes,reserved_execution_slots,computer_id,program_deployment_id,preparation_expires_at,desired_reason,writer_generation,writer_token_hash,writer_expires_at,admission_state,source_checkpoint_id,source_disk_version_id)
 SELECT $2,org_id,project_id,environment_id,region_id,worker_group_id,worker_host_id,worker_epoch,vm_platform_id,computer_spec_id,vm_vcpu_count,cpu_config_digest,reserved_cpu_millis,reserved_memory_bytes,reserved_guest_ephemeral_disk_bytes,reserved_execution_slots,computer_id,program_deployment_id,now()+interval '5 minutes','restore',3,decode(repeat('03',32),'hex'),now()+interval '10 minutes','restoring',$3,$4 FROM computer_instances WHERE id=$1`, source, instance, checkpoint, private)
	dbtest.MustExec(t, t.Context(), tx, `UPDATE computers SET writer_generation=3,preparation_attempt_count=1,preparation_instance_id=$2 WHERE id=$1`, computerID, instance)
	if err = tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}

	params := db.GetComputerInstanceRestoreCheckpointParams{
		EnvironmentID: pgvalue.UUID(f.EnvironmentID), ComputerInstanceID: pgvalue.UUID(instance),
		WorkerGroupID: pgvalue.UUID(runtest.WorkerGroupID), WorkerHostID: pgvalue.UUID(f.WorkerID),
		WorkerEpoch: 1, DesiredVersion: 1,
	}
	q := db.New(f.Pool)
	ready, err := q.GetComputerInstanceRestoreCheckpoint(t.Context(), params)
	if err != nil {
		t.Fatal(err)
	}
	if ready.ComputerCheckpoint.ID != pgvalue.UUID(checkpoint) || ready.MemoryDigest != dbtest.Digest("restore-discovery-memory") {
		t.Fatal("wrong checkpoint or artifact descriptor")
	}

	cp := ready.ComputerCheckpoint
	_, _, manifest := restoreProjectionFixture(t, 0)
	manifest.RecoveryPoint = workerapi.CheckpointRecoveryPoint{ID: pgvalue.UUIDString(cp.ID), ComputerID: pgvalue.UUIDString(cp.ComputerID), ComputerInstanceID: pgvalue.UUIDString(cp.SourceComputerInstanceID), ComputerSpecID: pgvalue.UUIDString(cp.ComputerSpecID), ProgramDeploymentID: pgvalue.UUIDString(cp.ProgramDeploymentID), WriterGeneration: cp.WriterGeneration, MembershipRevision: cp.MembershipRevision, Runs: []workerapi.CheckpointRun{}}
	manifest.RuntimeState = workerapi.CheckpointRuntimeState{
		ConfigArtifact:      workerapi.CheckpointArtifact{Digest: ready.VMConfigDigest, SizeBytes: ready.VMConfigSizeBytes, MediaType: ready.VMConfigMediaType},
		VMStateArtifact:     workerapi.CheckpointArtifact{Digest: ready.VMStateDigest, SizeBytes: ready.VMStateSizeBytes, MediaType: ready.VMStateMediaType},
		MemoryArtifacts:     []workerapi.CheckpointArtifact{{Digest: ready.MemoryDigest, SizeBytes: ready.MemorySizeBytes, MediaType: ready.MemoryMediaType}},
		ScratchDiskArtifact: workerapi.CheckpointArtifact{Digest: ready.ScratchDiskDigest, SizeBytes: ready.ScratchDiskSizeBytes, MediaType: ready.ScratchDiskMediaType},
	}
	encodeRestoreManifest(t, &ready, manifest)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_checkpoints SET manifest=$2 WHERE id=$1`, checkpoint, ready.ComputerCheckpoint.Manifest)
	host := computer.Host{GroupID: runtest.WorkerGroupID, HostID: f.WorkerID, Epoch: 1}
	restoreTarget := func() *computer.ReconcileTarget {
		t.Helper()
		targets, err := computer.ReconcileTargets(t.Context(), q, host, 64)
		if err != nil {
			t.Fatal(err)
		}
		for _, target := range targets {
			if target.Instance.ID == params.ComputerInstanceID {
				return &target
			}
		}
		t.Fatal("restore destination not discovered")
		return nil
	}
	target := restoreTarget()
	if target.Action != computer.ReconcilePrepare || target.Restore == nil {
		t.Fatalf("restore destination target: %+v", target)
	}
	restore, err := projectComputerInstanceRestore(target.Restore.Checkpoint, target.Restore.Members)
	if err != nil {
		t.Fatal(err)
	}
	if restore.CheckpointID != checkpoint.String() || len(restore.Artifacts) != 4 {
		t.Fatalf("restore projection: %+v", restore)
	}
	// Discovery does not retain authority after admission opens.
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_instances SET admission_state='open' WHERE id=$1`, instance)
	if target := restoreTarget(); target.Restore != nil {
		t.Fatal("discovery after admission opened produced a restore payload")
	}
}
