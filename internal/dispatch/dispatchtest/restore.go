package dispatchtest

import (
	"encoding/json"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/disk"
	"github.com/helmrdotdev/helmr/internal/disk/blockformat"
	"github.com/helmrdotdev/helmr/internal/dispatch"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run/runtest"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"strings"
	"testing"
	"time"
	"uuid"
)

func Restore(t *testing.T, idle bool, setup ...func(runtest.Fixture, runtest.RunLease)) (runtest.Fixture, *dispatch.Authority, computer.InstanceRef) {
	t.Helper()
	f, worker, request, uploaded := ReadyCapture(t, idle, setup...)
	return RestoreReadyCapture(t, f, worker, request, uploaded)
}

func RestoreReadyCapture(t *testing.T, f runtest.Fixture, worker dispatch.ComputerCaptureWorker, request workerapi.RegisterCheckpointRequest, uploaded []cas.Object) (runtest.Fixture, *dispatch.Authority, computer.InstanceRef) {
	t.Helper()
	tx, err := f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	checkpoint, err := dispatch.CompleteComputerCheckpoint(t.Context(), tx, worker, request, uploaded)
	if err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), tx, `UPDATE computer_instances SET observed_state='closed',observed_desired_version=desired_version,mount_state='unmounted',unmounted_at=now(),terminal_at=now(),reclaimed_at=now(),reclaim_evidence='{"method":"session_closed"}',terminal_reason_code='checkpointed' WHERE id=$1`, request.ComputerInstanceID)
	dbtest.MustExec(t, t.Context(), tx, `UPDATE run_leases SET process_reconciled_at=now() WHERE computer_instance_id=$1`, request.ComputerInstanceID)
	instance := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), tx, `INSERT INTO computer_instances(id,org_id,project_id,environment_id,region_id,worker_group_id,worker_host_id,worker_epoch,vm_platform_id,computer_spec_id,vm_vcpu_count,cpu_config_digest,reserved_cpu_millis,reserved_memory_bytes,reserved_guest_ephemeral_disk_bytes,reserved_execution_slots,computer_id,program_deployment_id,preparation_expires_at,desired_reason,writer_generation,writer_token_hash,writer_expires_at,admission_state,source_checkpoint_id,source_disk_version_id,observed_state,observed_version,observed_desired_version,ready_at,mount_state,mounted_at)
 SELECT $2,org_id,project_id,environment_id,region_id,worker_group_id,worker_host_id,worker_epoch,vm_platform_id,computer_spec_id,vm_vcpu_count,cpu_config_digest,reserved_cpu_millis,reserved_memory_bytes,reserved_guest_ephemeral_disk_bytes,reserved_execution_slots,computer_id,program_deployment_id,now()+interval '5 minutes','restore',writer_generation+1,decode(repeat('03',32),'hex'),now()+interval '10 minutes','restoring',$3,$4,'ready',1,1,now(),'mounted',now() FROM computer_instances WHERE id=$1`, request.ComputerInstanceID, instance, checkpoint.ID, checkpoint.PrivateComputerDiskVersionID)
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

func ReadyCapture(t *testing.T, idle bool, setup ...func(runtest.Fixture, runtest.RunLease)) (runtest.Fixture, dispatch.ComputerCaptureWorker, workerapi.RegisterCheckpointRequest, []cas.Object) {
	t.Helper()
	f, worker, r := RegisteredCapture(t, idle, setup...)
	return f, worker, r, PrepareCapture(t, f, worker, r)
}

func PrepareCapture(t *testing.T, f runtest.Fixture, worker dispatch.ComputerCaptureWorker, r workerapi.RegisterCheckpointRequest) []cas.Object {
	t.Helper()
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE runs SET active_started_at=clock_timestamp(),max_active_duration_ms=3600000 WHERE current_run_lease_id IN (SELECT id FROM run_leases WHERE computer_instance_id=$1)`, r.ComputerInstanceID)
	tx, err := f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = dispatch.RegisterComputerCheckpoint(t.Context(), tx, worker, r); err != nil {
		tx.Rollback(t.Context())
		t.Fatal(err)
	}
	if err = tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	root := r.Manifest.RuntimeState.Computer.Root
	locator, err := root.Locator(root.LogicalBytes)
	if err != nil {
		t.Fatal(err)
	}
	// This fixture represents already-certified host inspection. The publication
	// byte inspector has separate tests; this suite proves the commit boundary.
	inspection := blockformat.ObjectInspection{Pack: &blockformat.PackInspection{Pages: []blockformat.PageInspection{{Locator: locator, Shape: blockformat.Root{Capacity: root.LogicalBytes}, Level: -1}}}}
	raw, err := json.Marshal(inspection)
	if err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO cas_blobs(digest,size_bytes) VALUES($1,$2)`, root.Pack.Digest, root.Pack.SizeBytes)
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO cas_objects(org_id,digest,size_bytes,media_type) VALUES($1,$2,$3,'application/octet-stream')`, f.OrgID, root.Pack.Digest, root.Pack.SizeBytes)
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO computer_objects(environment_id,computer_id,digest,org_id,project_id,size_bytes,media_type,kind,rank,inspection) VALUES($1,$2,$3,$4,$5,$6,'application/octet-stream','root',2,$7)`, f.EnvironmentID, r.Manifest.RecoveryPoint.ComputerID, root.Pack.Digest, f.OrgID, f.ProjectID, root.Pack.SizeBytes, raw)
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO computer_data_keys(id,environment_id,computer_id,wrapping_key_id,wrapped_key) VALUES($1,$2,$3,'fixture',decode('01','hex'))`, root.Page.KeyID, f.EnvironmentID, r.Manifest.RecoveryPoint.ComputerID)
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO computer_object_keys(environment_id,computer_id,digest,key_id,is_direct) VALUES($1,$2,$3,$4,true)`, f.EnvironmentID, r.Manifest.RecoveryPoint.ComputerID, root.Pack.Digest, root.Page.KeyID)
	if n, err := db.New(f.Pool).CertifyComputerObject(t.Context(), db.CertifyComputerObjectParams{EnvironmentID: pgvalue.UUID(f.EnvironmentID), ComputerID: pgvalue.UUID(uuid.MustParse(r.Manifest.RecoveryPoint.ComputerID)), Digest: root.Pack.Digest}); err != nil || n != 1 {
		t.Fatalf("certify root n=%d err=%v", n, err)
	}
	id := uuid.MustParse(r.CheckpointID)
	key := computer.CheckpointPublicationKey(id)
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO computer_object_pins(computer_instance_id,digest,environment_id,computer_id,instance_desired_version,publication_key) VALUES($1,$2,$3,$4,$5,$6)`, r.ComputerInstanceID, root.Pack.Digest, f.EnvironmentID, r.Manifest.RecoveryPoint.ComputerID, r.DesiredVersion, key)
	artifacts := []workerapi.CheckpointArtifact{r.Manifest.RuntimeState.ConfigArtifact, r.Manifest.RuntimeState.VMStateArtifact, r.Manifest.RuntimeState.MemoryArtifacts[0], r.Manifest.RuntimeState.ScratchDiskArtifact}
	var uploaded []cas.Object
	for _, a := range artifacts {
		uploaded = append(uploaded, cas.Object{Digest: a.Digest, SizeBytes: a.SizeBytes, MediaType: a.MediaType})
	}
	return uploaded
}

func RegisteredCapture(t *testing.T, idle bool, setup ...func(runtest.Fixture, runtest.RunLease)) (runtest.Fixture, dispatch.ComputerCaptureWorker, workerapi.RegisterCheckpointRequest) {
	t.Helper()
	f, target, _, begin := Capture(t)
	for _, prepare := range setup {
		prepare(f, target)
	}
	if idle {
		dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE run_leases SET status='cancelled',terminal_at=now(),terminal_reason_code='cancelled',process_reconciled_at=now() WHERE computer_instance_id=$1`, begin.ComputerInstanceID)
	}
	tx, err := f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	cp, err := dispatch.BeginComputerCapture(t.Context(), tx, begin)
	if err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	worker, request := CaptureRequest(t, f, cp)
	return f, worker, request
}

func CaptureRequest(t *testing.T, f runtest.Fixture, cp db.ComputerCheckpoint) (dispatch.ComputerCaptureWorker, workerapi.RegisterCheckpointRequest) {
	t.Helper()
	q := db.New(f.Pool)
	instance, err := q.GetComputerInstance(t.Context(), db.GetComputerInstanceParams{ID: cp.SourceComputerInstanceID, EnvironmentID: cp.EnvironmentID})
	if err != nil {
		t.Fatal(err)
	}
	platform, err := q.GetVMPlatformForCheckpoint(t.Context(), instance.VMPlatformID)
	if err != nil {
		t.Fatal(err)
	}
	members, err := q.ListComputerCheckpointRuns(t.Context(), db.ListComputerCheckpointRunsParams{EnvironmentID: cp.EnvironmentID, CheckpointID: cp.ID})
	if err != nil {
		t.Fatal(err)
	}
	point := workerapi.CheckpointRecoveryPoint{ID: pgvalue.UUIDString(cp.ID), ComputerID: pgvalue.UUIDString(cp.ComputerID), ComputerInstanceID: pgvalue.UUIDString(cp.SourceComputerInstanceID), ComputerSpecID: pgvalue.UUIDString(cp.ComputerSpecID), ProgramDeploymentID: pgvalue.UUIDString(cp.ProgramDeploymentID), WriterGeneration: cp.WriterGeneration, MembershipRevision: cp.MembershipRevision, Runs: []workerapi.CheckpointRun{}, Runtime: workerapi.CheckpointRuntime{Backend: "firecracker", ID: platform.ID, Arch: platform.Arch, Contract: platform.Contract, KernelDigest: platform.KernelDigest, InitramfsDigest: platform.InitramfsDigest, RootfsDigest: platform.RootfsDigest, ConfigDigest: dbtest.Digest("config"), VMVCPUCount: instance.VMVCPUCount, CPUConfigDigest: instance.CPUConfigDigest}}
	for _, member := range members {
		captured := workerapi.CheckpointRun{RunID: pgvalue.UUIDString(member.RunID), RunLeaseID: pgvalue.UUIDString(member.SourceRunLeaseID), RunWaitID: pgvalue.UUIDString(member.RunWaitID), AttemptNumber: member.AttemptNumber, CorrelationID: uuid.NewV7().String()}
		if member.ActorSpeculativeInputSequence.Valid {
			sequence := member.ActorSpeculativeInputSequence.Int64
			captured.ActorSpeculativeInputSequence = &sequence
		}
		point.Runs = append(point.Runs, captured)
	}
	artifact := func(name, media string) workerapi.CheckpointArtifact {
		return workerapi.CheckpointArtifact{Digest: dbtest.Digest(name), SizeBytes: 128, MediaType: media}
	}
	root := disk.GenerationRoot{FormatVersion: 1, LogicalBytes: instance.ReservedGuestEphemeralDiskBytes, Offset: 128, Pack: disk.GenerationPack{Digest: dbtest.Digest("pack"), SizeBytes: 512, Rank: 2}, Page: disk.GenerationPage{Digest: dbtest.Digest("page"), Salt: strings.Repeat("aa", 32), KeyID: uuid.NewV7().String(), Kind: 3, Count: 1, SizeBytes: 64}}
	manifest := workerapi.CheckpointManifest{RecoveryPoint: point, RuntimeState: workerapi.CheckpointRuntimeState{Computer: &workerapi.CheckpointComputer{ComputerID: point.ComputerID, LogicalBytes: root.LogicalBytes, Root: root}, ConfigArtifact: artifact("config-object", cas.CheckpointVMConfigMediaType), VMStateArtifact: artifact("state-object", cas.CheckpointVMStateMediaType), ScratchDiskArtifact: artifact("scratch-object", cas.CheckpointScratchDiskMediaType), MemoryArtifacts: []workerapi.CheckpointArtifact{artifact("memory-object", cas.CheckpointMemoryMediaType)}, Config: json.RawMessage(`{"runtime":{}}`)}, ComputerState: workerapi.CheckpointComputerState{Base: workerapi.CheckpointComputerBase{MountPath: "/workspace"}}}
	return dispatch.ComputerCaptureWorker{GroupID: instance.WorkerGroupID, HostID: instance.WorkerHostID, Epoch: instance.WorkerEpoch}, workerapi.RegisterCheckpointRequest{ComputerInstanceID: pgvalue.UUIDString(instance.ID), WorkerEpoch: instance.WorkerEpoch, DesiredVersion: instance.DesiredVersion, CheckpointID: pgvalue.UUIDString(cp.ID), Manifest: manifest}
}

func Capture(t *testing.T) (runtest.Fixture, runtest.RunLease, runtest.RunLease, db.BeginComputerCheckpointParams) {
	t.Helper()
	f := runtest.New(t)
	target := f.AddRunLease(t, "running", time.Now().Add(-time.Minute))
	peerID, peerLeaseID := uuid.NewV7(), uuid.NewV7()
	tx, err := f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
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

	for _, work := range []runtest.RunLease{target, {RunID: peerID, LeaseID: peerLeaseID}} {
		dbtest.MustExec(t, t.Context(), tx, `UPDATE runs SET status='waiting',started_at=now() WHERE id=$1`, work.RunID)
		dbtest.MustExec(t, t.Context(), tx, `UPDATE run_attempts SET entrypoint_entered_at=now() WHERE run_id=$1`, work.RunID)
		dbtest.MustExec(t, t.Context(), tx, `INSERT INTO run_waits(id,environment_id,run_id,computer_id,kind,due_at,expected_run_revision,attempt_number,current_run_lease_id)
 SELECT $2,environment_id,id,computer_id,'timer',now()+interval '1 hour',revision,1,$3 FROM runs WHERE id=$1`, work.RunID, uuid.NewV7(), work.LeaseID)
	}
	if err = tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	request := db.BeginComputerCheckpointParams{CheckpointID: pgvalue.UUID(uuid.NewV7()), EnvironmentID: pgvalue.UUID(f.EnvironmentID)}
	if err = f.Pool.QueryRow(t.Context(), `SELECT i.id,i.writer_generation,i.membership_revision,i.desired_version FROM computer_instances i JOIN run_leases l ON l.computer_instance_id=i.id WHERE l.id=$1`, target.LeaseID).Scan(&request.ComputerInstanceID, &request.WriterGeneration, &request.MembershipRevision, &request.DesiredVersion); err != nil {
		t.Fatal(err)
	}
	return f, target, runtest.RunLease{RunID: peerID, LeaseID: peerLeaseID}, request
}
