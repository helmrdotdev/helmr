package dispatch

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/helmrdotdev/helmr/internal/computer"
	"sort"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/computer/blockformat"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/jsoncanon"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
)

// CompleteComputerCheckpoint consumes server-observed CAS descriptors; callers
// must obtain these from storage, never from a Worker-supplied existence claim.
// The caller owns commit/rollback. Publication, all member suspension and source
// close intent are one transaction; physical reclamation remains separate.
func CompleteComputerCheckpoint(ctx context.Context, tx pgx.Tx, worker ComputerCaptureWorker, request workerapi.RegisterCheckpointRequest, uploaded []cas.Object) (db.ComputerCheckpoint, error) {
	source, raw, fingerprint, err := prepareComputerCheckpointReady(ctx, tx, worker, request)
	if err != nil {
		return db.ComputerCheckpoint{}, err
	}
	instance, cp := source.instance, source.checkpoint
	if cp.Status == "ready" {
		return cp, nil
	}
	var candidate workerapi.CheckpointManifest
	if err = json.Unmarshal(raw, &candidate); err != nil {
		return db.ComputerCheckpoint{}, err
	}
	// Reuse full candidate validation and immutable pin checks. A ready receipt
	// can only reference a previously registered manifest (checked above).
	if _, err = registerComputerCheckpoint(ctx, tx, worker, request, source); err != nil {
		return db.ComputerCheckpoint{}, err
	}
	q := db.New(tx)
	descriptors := []workerapi.CheckpointArtifact{candidate.RuntimeState.ConfigArtifact, candidate.RuntimeState.VMStateArtifact, candidate.RuntimeState.MemoryArtifacts[0], candidate.RuntimeState.ScratchDiskArtifact}
	if len(uploaded) != len(descriptors) {
		return db.ComputerCheckpoint{}, ErrCheckpointCandidate
	}
	observed := make(map[string]cas.Object, len(uploaded))
	for _, object := range uploaded {
		if _, ok := observed[object.Digest]; ok {
			return db.ComputerCheckpoint{}, ErrCheckpointCandidate
		}
		observed[object.Digest] = object
	}
	for _, d := range descriptors {
		o, ok := observed[d.Digest]
		if !ok || o.SizeBytes != d.SizeBytes || o.MediaType != d.MediaType {
			return db.ComputerCheckpoint{}, ErrCheckpointCandidate
		}
	}
	disk := candidate.RuntimeState.Computer
	locator, err := disk.Root.Locator(instance.ReservedGuestEphemeralDiskBytes)
	if err != nil {
		return db.ComputerCheckpoint{}, err
	}
	root, err := q.LockComputerObject(ctx, db.LockComputerObjectParams{EnvironmentID: instance.EnvironmentID, ComputerID: instance.ComputerID, Digest: disk.Root.Pack.Digest})
	if err != nil {
		return db.ComputerCheckpoint{}, err
	}
	var inspection blockformat.ObjectInspection
	if !root.Certified.Bool || json.Unmarshal(root.Inspection, &inspection) != nil || inspection.Pack == nil {
		return db.ComputerCheckpoint{}, ErrCheckpointCandidate
	}
	if err = inspection.Pack.CheckRoot(locator, disk.LogicalBytes); err != nil {
		return db.ComputerCheckpoint{}, err
	}
	publicationKey := computer.PublicationKey("checkpoint", pgvalue.MustUUIDValue(cp.ID), pgvalue.MustUUIDValue(cp.ID))
	if _, err = q.RequireComputerObjectPin(ctx, db.RequireComputerObjectPinParams{ComputerInstanceID: instance.ID, PublicationKey: publicationKey, InstanceDesiredVersion: request.DesiredVersion, Digest: root.Digest}); err != nil {
		return db.ComputerCheckpoint{}, err
	}
	version, err := q.CreatePrivateCheckpointComputerDiskVersion(ctx, db.CreatePrivateCheckpointComputerDiskVersionParams{ID: pgvalue.NewUUIDv7(), EnvironmentID: instance.EnvironmentID, CheckpointID: cp.ID, RootPackDigest: pgvalue.Text(disk.Root.Pack.Digest), LogicalBytes: disk.LogicalBytes})
	if err != nil {
		return db.ComputerCheckpoint{}, err
	}
	rootJSON, err := json.Marshal(disk.Root)
	if err != nil {
		return db.ComputerCheckpoint{}, err
	}
	if err = q.CreateComputerDiskVersionRoot(ctx, db.CreateComputerDiskVersionRootParams{EnvironmentID: instance.EnvironmentID, ComputerID: instance.ComputerID, VersionID: version.ID, Locator: rootJSON}); err != nil {
		return db.ComputerCheckpoint{}, err
	}
	kinds := []string{"computer_checkpoint_vm_config", "computer_checkpoint_vm_state", "computer_checkpoint_memory", "computer_checkpoint_scratch_disk"}
	artifacts := make([]db.Artifact, 0, 4)
	for i, d := range descriptors {
		if _, err = q.UpsertCasObject(ctx, db.UpsertCasObjectParams{OrgID: instance.OrgID, Digest: d.Digest, SizeBytes: d.SizeBytes, MediaType: d.MediaType}); err != nil {
			return db.ComputerCheckpoint{}, err
		}
		a, e := q.CreateArtifact(ctx, db.CreateArtifactParams{ID: pgvalue.NewUUIDv7(), OrgID: instance.OrgID, ProjectID: instance.ProjectID, EnvironmentID: instance.EnvironmentID, Digest: d.Digest, SizeBytes: d.SizeBytes, MediaType: d.MediaType, Kind: db.ArtifactKind(kinds[i]), CreatedByWorkerHostID: worker.HostID})
		if e != nil {
			return db.ComputerCheckpoint{}, e
		}
		artifacts = append(artifacts, a)
	}
	// Blocking storage rows may have outlived execution authority.
	if err = validateComputerCheckpointMembers(ctx, tx, instance, cp, len(candidate.RecoveryPoint.Runs)); err != nil {
		return db.ComputerCheckpoint{}, err
	}
	if _, err = q.GetComputerInstanceCaptureCheckpoint(ctx, db.GetComputerInstanceCaptureCheckpointParams{ComputerInstanceID: instance.ID, EnvironmentID: instance.EnvironmentID, WorkerGroupID: worker.GroupID, WorkerHostID: worker.HostID, WorkerEpoch: worker.Epoch, DesiredVersion: request.DesiredVersion, WorkerFreshnessSeconds: workergroup.ObservationFreshnessSeconds}); err != nil {
		return db.ComputerCheckpoint{}, err
	}
	phases, err := json.Marshal(request.Manifest.Phases)
	if err != nil {
		return db.ComputerCheckpoint{}, err
	}
	if request.Manifest.Phases == nil {
		phases = nil
	}
	cp, err = q.MarkComputerCheckpointReady(ctx, db.MarkComputerCheckpointReadyParams{EnvironmentID: instance.EnvironmentID, CheckpointID: cp.ID, DesiredVersion: request.DesiredVersion, PrivateComputerDiskVersionID: version.ID, VMConfigArtifactID: artifacts[0].ID, VMStateArtifactID: artifacts[1].ID, MemoryArtifactID: artifacts[2].ID, ScratchDiskArtifactID: artifacts[3].ID, Manifest: raw, PhaseTimings: phases, ReadyRequestFingerprint: pgvalue.Text(fingerprint)})
	if err != nil {
		return db.ComputerCheckpoint{}, err
	}
	// A condition may have resolved during capture. Preserve that result and park
	// the whole set; restoration gives every member fresh authority together.
	for _, m := range candidate.RecoveryPoint.Runs {
		result, e := tx.Exec(ctx, `UPDATE runs SET active_elapsed_ms=active_elapsed_ms+floor(extract(epoch FROM (clock_timestamp()-active_started_at))*1000)::bigint,active_started_at=NULL,current_run_lease_id=NULL,updated_at=clock_timestamp() WHERE id=$1 AND current_run_lease_id=$2 AND status='waiting' AND active_started_at IS NOT NULL AND clock_timestamp()<active_started_at+((max_active_duration_ms-active_elapsed_ms)*interval '1 millisecond')`, m.RunID, m.RunLeaseID)
		if e != nil {
			return db.ComputerCheckpoint{}, e
		}
		if result.RowsAffected() != 1 {
			return db.ComputerCheckpoint{}, pgx.ErrNoRows
		}
		if _, err = tx.Exec(ctx, `UPDATE run_leases SET status='checkpointed',checkpointed_at=$2,terminal_at=$2,terminal_reason_code='checkpointed' WHERE id=$1`, m.RunLeaseID, cp.ReadyAt); err != nil {
			return db.ComputerCheckpoint{}, err
		}
		if _, err = tx.Exec(ctx, `UPDATE run_waits SET suspension_status=CASE WHEN condition_status='pending' THEN 'parked' ELSE 'resume_pending' END,prior_run_lease_id=current_run_lease_id,current_run_lease_id=NULL,updated_at=$2 WHERE id=$1`, m.RunWaitID, cp.ReadyAt); err != nil {
			return db.ComputerCheckpoint{}, err
		}
	}
	if _, err = q.RequestComputerInstanceClose(ctx, db.RequestComputerInstanceCloseParams{ID: instance.ID, WriterGeneration: instance.WriterGeneration, Reason: "checkpoint_ready", FinalizationAction: pgvalue.Text("capture")}); err != nil {
		return db.ComputerCheckpoint{}, err
	}
	return cp, nil
}

// CheckComputerCheckpointReady validates an exact registered candidate before
// external storage I/O. The caller ends this transaction before remote reads;
// CompleteComputerCheckpoint rechecks all authority in the final transaction.
func CheckComputerCheckpointReady(ctx context.Context, tx pgx.Tx, worker ComputerCaptureWorker, request workerapi.RegisterCheckpointRequest) (db.ComputerCheckpoint, error) {
	source, _, _, err := prepareComputerCheckpointReady(ctx, tx, worker, request)
	if err != nil {
		return db.ComputerCheckpoint{}, err
	}
	if source.checkpoint.Status == "creating" {
		_, err = db.New(tx).GetComputerInstanceCaptureCheckpoint(ctx, db.GetComputerInstanceCaptureCheckpointParams{ComputerInstanceID: source.instance.ID, EnvironmentID: source.instance.EnvironmentID, WorkerGroupID: worker.GroupID, WorkerHostID: worker.HostID, WorkerEpoch: worker.Epoch, DesiredVersion: request.DesiredVersion, WorkerFreshnessSeconds: workergroup.ObservationFreshnessSeconds})
	}
	return source.checkpoint, err
}

func prepareComputerCheckpointReady(ctx context.Context, tx pgx.Tx, worker ComputerCaptureWorker, request workerapi.RegisterCheckpointRequest) (computerCheckpointSource, []byte, string, error) {
	source, err := lockComputerCheckpointSource(ctx, tx, worker, computerCheckpointFence{request.ComputerInstanceID, request.WorkerEpoch, request.DesiredVersion, request.CheckpointID})
	if err != nil {
		return computerCheckpointSource{}, nil, "", err
	}
	cp := source.checkpoint
	candidate := request.Manifest
	candidate.Phases = nil
	// Registration sorted the sealed member set. Normalize without mutating input.
	candidate.RecoveryPoint.Runs = append([]workerapi.CheckpointRun{}, candidate.RecoveryPoint.Runs...)
	sort.Slice(candidate.RecoveryPoint.Runs, func(i, j int) bool {
		return candidate.RecoveryPoint.Runs[i].RunID < candidate.RecoveryPoint.Runs[j].RunID
	})
	raw, err := json.Marshal(candidate)
	if err != nil {
		return computerCheckpointSource{}, nil, "", err
	}
	raw, err = jsoncanon.Transform(raw)
	if err != nil {
		return computerCheckpointSource{}, nil, "", err
	}
	stored, err := jsoncanon.Transform(cp.Manifest)
	if err != nil {
		return computerCheckpointSource{}, nil, "", ErrCheckpointCandidate
	}
	if !bytes.Equal(raw, stored) {
		return computerCheckpointSource{}, nil, "", ErrCheckpointCandidate
	}
	fingerprint := sha256sum.DigestBytes(append([]byte(fmt.Sprintf("computer.checkpoint.ready\x00%s\x00%d\x00%d\x00", request.ComputerInstanceID, request.WorkerEpoch, request.DesiredVersion)), raw...))
	if cp.Status == "ready" && cp.ReadyRequestFingerprint.String == fingerprint {
		return source, raw, fingerprint, nil
	}
	if cp.Status != "creating" {
		return computerCheckpointSource{}, nil, "", pgx.ErrNoRows
	}

	return source, raw, fingerprint, nil
}
