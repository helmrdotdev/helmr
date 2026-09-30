// Package computertest builds Computer capture checkpoints over a Run test
// database: a sealed resident set, its registered candidate and the stored
// runtime objects its readiness observes.
package computertest

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/disk"
	"github.com/helmrdotdev/helmr/internal/disk/blockformat"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run/runtest"
	"github.com/jackc/pgx/v5"
)

// Objects is object storage holding exactly these objects.
type Objects []cas.Object

// Stat returns the stored object with the digest.
func (o Objects) Stat(_ context.Context, digest string) (cas.Object, error) {
	for _, object := range o {
		if object.Digest == digest {
			return object, nil
		}
	}
	return cas.Object{}, errors.New("object is not stored")
}

// Complete publishes the registered candidate as ready with storage holding
// the objects.
func Complete(t *testing.T, f runtest.Fixture, ref computer.CheckpointRef, manifest computer.CheckpointManifest, objects Objects) db.ComputerCheckpoint {
	t.Helper()
	publisher, err := computer.NewPublisher(f.Pool, objects)
	if err != nil {
		t.Fatal(err)
	}
	cp, err := publisher.CompleteCheckpoint(t.Context(), ref, manifest)
	if err != nil {
		t.Fatal(err)
	}
	return cp
}

// ReadyCapture is a registered capture whose certified disk root is pinned
// and whose runtime objects are stored, ready to complete.
func ReadyCapture(t *testing.T, idle bool, setup ...func(runtest.Fixture, runtest.RunLease)) (runtest.Fixture, computer.CheckpointRef, computer.CheckpointManifest, Objects) {
	t.Helper()
	f, ref, manifest := RegisteredCapture(t, idle, setup...)
	return f, ref, manifest, PrepareCapture(t, f, ref, manifest)
}

// PrepareCapture registers the candidate, certifies and pins its disk root
// for the capture and returns the storage holding its runtime objects.
func PrepareCapture(t *testing.T, f runtest.Fixture, ref computer.CheckpointRef, manifest computer.CheckpointManifest) Objects {
	t.Helper()
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE runs SET active_started_at=clock_timestamp(),max_active_duration_ms=3600000 WHERE current_run_lease_id IN (SELECT id FROM run_leases WHERE computer_instance_id=$1)`, ref.InstanceID)
	if _, err := computer.RegisterCheckpoint(t.Context(), f.Pool, ref, manifest); err != nil {
		t.Fatal(err)
	}
	root := manifest.RuntimeState.Computer.Root
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
	computerID := manifest.RecoveryPoint.ComputerID
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO cas_blobs(digest,size_bytes) VALUES($1,$2)`, root.Pack.Digest, root.Pack.SizeBytes)
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO cas_objects(org_id,digest,size_bytes,media_type) VALUES($1,$2,$3,'application/octet-stream')`, f.OrgID, root.Pack.Digest, root.Pack.SizeBytes)
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO computer_objects(environment_id,computer_id,digest,org_id,project_id,size_bytes,media_type,kind,rank,inspection) VALUES($1,$2,$3,$4,$5,$6,'application/octet-stream','root',2,$7)`, f.EnvironmentID, computerID, root.Pack.Digest, f.OrgID, f.ProjectID, root.Pack.SizeBytes, raw)
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO computer_data_keys(id,environment_id,computer_id,wrapping_key_id,wrapped_key) VALUES($1,$2,$3,'fixture',decode('01','hex'))`, root.Page.KeyID, f.EnvironmentID, computerID)
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO computer_object_keys(environment_id,computer_id,digest,key_id,is_direct) VALUES($1,$2,$3,$4,true)`, f.EnvironmentID, computerID, root.Pack.Digest, root.Page.KeyID)
	if n, err := db.New(f.Pool).CertifyComputerObject(t.Context(), db.CertifyComputerObjectParams{EnvironmentID: pgvalue.UUID(f.EnvironmentID), ComputerID: pgvalue.UUID(uuid.MustParse(computerID)), Digest: root.Pack.Digest}); err != nil || n != 1 {
		t.Fatalf("certify root n=%d err=%v", n, err)
	}
	// The capture pins the certified root through the owner's reuse, under the
	// Instance's write key, which encrypts the root page.
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_instances SET write_key_id=$2 WHERE id=$1`, ref.InstanceID, root.Page.KeyID)
	store, err := cas.NewFile(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	publisher, err := computer.NewPublisher(f.Pool, store)
	if err != nil {
		t.Fatal(err)
	}
	if err = publisher.ReuseCheckpointObject(t.Context(), ref, inspection); err != nil {
		t.Fatalf("pin capture root: %v", err)
	}
	state := manifest.RuntimeState
	var objects Objects
	for _, a := range []computer.CheckpointArtifact{state.ConfigArtifact, state.VMStateArtifact, state.MemoryArtifacts[0], state.ScratchDiskArtifact} {
		objects = append(objects, cas.Object{Digest: a.Digest, SizeBytes: a.SizeBytes, MediaType: a.MediaType})
	}
	return objects
}

// RegisteredCapture begins the capture of a two-member resident set, or of
// the same Instance without members when idle, and returns the candidate a
// worker host registers for it. setup runs on the fixture and its target
// lease before the capture begins.
func RegisteredCapture(t *testing.T, idle bool, setup ...func(runtest.Fixture, runtest.RunLease)) (runtest.Fixture, computer.CheckpointRef, computer.CheckpointManifest) {
	t.Helper()
	f, target, _, capture := Capture(t)
	for _, prepare := range setup {
		prepare(f, target)
	}
	if idle {
		dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE run_leases SET status='cancelled',terminal_at=now(),terminal_reason_code='cancelled',process_reconciled_at=now() WHERE computer_instance_id=$1`, capture.InstanceID)
	}
	var cp db.ComputerCheckpoint
	if err := db.RunTx(t.Context(), f.Pool, func(tx pgx.Tx) error {
		var err error
		cp, err = computer.BeginCapture(t.Context(), tx, capture)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	ref, manifest := CaptureRequest(t, f, cp)
	return f, ref, manifest
}

// CaptureRequest is the source reference and candidate manifest a worker
// host reports for the creating checkpoint: its sealed members, the
// Instance's VM platform and disk, and four distinct runtime objects.
func CaptureRequest(t *testing.T, f runtest.Fixture, cp db.ComputerCheckpoint) (computer.CheckpointRef, computer.CheckpointManifest) {
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
	point := computer.CheckpointRecoveryPoint{ID: pgvalue.UUIDString(cp.ID), ComputerID: pgvalue.UUIDString(cp.ComputerID), ComputerInstanceID: pgvalue.UUIDString(cp.SourceComputerInstanceID), ComputerSpecID: pgvalue.UUIDString(cp.ComputerSpecID), ProgramDeploymentID: pgvalue.UUIDString(cp.ProgramDeploymentID), WriterGeneration: cp.WriterGeneration, MembershipRevision: cp.MembershipRevision, Runs: []computer.CheckpointRun{}, Runtime: computer.CheckpointRuntime{Backend: "firecracker", ID: platform.ID, Arch: platform.Arch, Contract: platform.Contract, KernelDigest: platform.KernelDigest, InitramfsDigest: platform.InitramfsDigest, RootfsDigest: platform.RootfsDigest, ConfigDigest: dbtest.Digest("config"), VMVCPUCount: instance.VMVCPUCount, CPUConfigDigest: instance.CPUConfigDigest}}
	for _, member := range members {
		captured := computer.CheckpointRun{RunID: pgvalue.UUIDString(member.RunID), RunLeaseID: pgvalue.UUIDString(member.SourceRunLeaseID), RunWaitID: pgvalue.UUIDString(member.RunWaitID), AttemptNumber: member.AttemptNumber, CorrelationID: uuid.NewV7().String()}
		if member.ActorSpeculativeInputSequence.Valid {
			sequence := member.ActorSpeculativeInputSequence.Int64
			captured.ActorSpeculativeInputSequence = &sequence
		}
		point.Runs = append(point.Runs, captured)
	}
	artifact := func(name, media string) computer.CheckpointArtifact {
		return computer.CheckpointArtifact{Digest: dbtest.Digest(name), SizeBytes: 128, MediaType: media}
	}
	root := disk.GenerationRoot{FormatVersion: 1, LogicalBytes: instance.ReservedGuestEphemeralDiskBytes, Offset: 128, Pack: disk.GenerationPack{Digest: dbtest.Digest("pack"), SizeBytes: 512, Rank: 2}, Page: disk.GenerationPage{Digest: dbtest.Digest("page"), Salt: strings.Repeat("aa", 32), KeyID: uuid.NewV7().String(), Kind: 3, Count: 1, SizeBytes: 64}}
	manifest := computer.CheckpointManifest{RecoveryPoint: point, RuntimeState: computer.CheckpointRuntimeState{Computer: &computer.CheckpointComputer{ComputerID: point.ComputerID, LogicalBytes: root.LogicalBytes, Root: root}, ConfigArtifact: artifact("config-object", cas.CheckpointVMConfigMediaType), VMStateArtifact: artifact("state-object", cas.CheckpointVMStateMediaType), ScratchDiskArtifact: artifact("scratch-object", cas.CheckpointScratchDiskMediaType), MemoryArtifacts: []computer.CheckpointArtifact{artifact("memory-object", cas.CheckpointMemoryMediaType)}, Config: json.RawMessage(`{"runtime":{}}`)}, ComputerState: computer.CheckpointComputerState{Base: computer.CheckpointComputerBase{MountPath: "/workspace"}}}
	ref := computer.CheckpointRef{
		Host:       computer.Host{GroupID: pgvalue.MustUUIDValue(instance.WorkerGroupID), HostID: pgvalue.MustUUIDValue(instance.WorkerHostID), Epoch: instance.WorkerEpoch},
		InstanceID: pgvalue.MustUUIDValue(instance.ID), WorkerEpoch: instance.WorkerEpoch, DesiredVersion: instance.DesiredVersion, CheckpointID: pgvalue.MustUUIDValue(cp.ID),
	}
	return ref, manifest
}

// Capture is a running Run lease's Instance with a second resident Run on
// it; both Runs wait on a pending timer. It returns the fixture, the target
// and peer leases and the capture of the Instance.
func Capture(t *testing.T) (runtest.Fixture, runtest.RunLease, runtest.RunLease, computer.Capture) {
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
	capture := computer.Capture{CheckpointID: uuid.NewV7(), EnvironmentID: f.EnvironmentID}
	if err = f.Pool.QueryRow(t.Context(), `SELECT i.id,i.writer_generation,i.membership_revision,i.desired_version FROM computer_instances i JOIN run_leases l ON l.computer_instance_id=i.id WHERE l.id=$1`, target.LeaseID).Scan(&capture.InstanceID, &capture.WriterGeneration, &capture.MembershipRevision, &capture.DesiredVersion); err != nil {
		t.Fatal(err)
	}
	return f, target, runtest.RunLease{RunID: peerID, LeaseID: peerLeaseID}, capture
}
