package computer

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/disk/blockformat"
	"github.com/helmrdotdev/helmr/internal/jsoncanon"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
	"github.com/jackc/pgx/v5"
)

// CompleteCheckpoint publishes an exact registered capture candidate as a
// ready checkpoint. A first transaction checks the candidate under the
// checkpoint source fence; object storage then confirms each runtime object
// outside any transaction; a second transaction rechecks all authority,
// records the private disk version and runtime artifacts, parks every member
// and requests the source Instance's close. Publication, member parking and
// the close intent commit together; physical reclamation remains separate.
// A committed receipt replays without consulting storage or live members,
// including with members reordered or different timings. A storage failure
// reports ErrStorageUnavailable; a fence that no longer holds, or a receipt
// that differs from the committed one, reports ErrAuthorityChanged.
func (p Publisher) CompleteCheckpoint(ctx context.Context, ref CheckpointRef, manifest CheckpointManifest) (db.ComputerCheckpoint, error) {
	var checkpoint db.ComputerCheckpoint
	err := db.RunTx(ctx, p.db, func(tx pgx.Tx) error {
		source, _, _, err := prepareCheckpointReady(ctx, tx, ref, manifest)
		if err != nil {
			return err
		}
		checkpoint = source.checkpoint
		if checkpoint.Status == "creating" {
			return source.checkLive(ctx, ref)
		}
		return nil
	})
	if err != nil {
		return db.ComputerCheckpoint{}, authorityChanged(err)
	}
	// A committed receipt is independent of current storage availability. Every
	// replay still checks the authenticated source and exact candidate identity.
	if checkpoint.Status == "ready" {
		return checkpoint, nil
	}
	var registered CheckpointManifest
	if err = json.Unmarshal(checkpoint.Manifest, &registered); err != nil {
		return db.ComputerCheckpoint{}, err
	}
	if len(registered.RuntimeState.MemoryArtifacts) != 1 {
		return db.ComputerCheckpoint{}, ErrCheckpointCandidate
	}
	var observed [4]cas.Object
	for n, d := range registered.runtimeObjects() {
		if observed[n], err = p.objects.Stat(ctx, d.Digest); err != nil {
			return db.ComputerCheckpoint{}, storageUnavailable(err)
		}
	}
	err = db.RunTx(ctx, p.db, func(tx pgx.Tx) error {
		var err error
		checkpoint, err = completeCheckpoint(ctx, tx, ref, manifest, observed)
		return err
	})
	if err != nil {
		return db.ComputerCheckpoint{}, authorityChanged(err)
	}
	return checkpoint, nil
}

// runtimeObjects are the manifest's runtime objects in publication order:
// VM configuration, VM state, memory and scratch disk.
func (m CheckpointManifest) runtimeObjects() [4]CheckpointArtifact {
	return [4]CheckpointArtifact{m.RuntimeState.ConfigArtifact, m.RuntimeState.VMStateArtifact, m.RuntimeState.MemoryArtifacts[0], m.RuntimeState.ScratchDiskArtifact}
}

// completeCheckpoint consumes the storage-observed runtime objects, in
// runtimeObjects order, never a worker's existence claim.
func completeCheckpoint(ctx context.Context, tx pgx.Tx, ref CheckpointRef, manifest CheckpointManifest, observed [4]cas.Object) (db.ComputerCheckpoint, error) {
	source, raw, fingerprint, err := prepareCheckpointReady(ctx, tx, ref, manifest)
	if err != nil {
		return db.ComputerCheckpoint{}, err
	}
	instance, cp := source.instance, source.checkpoint
	if cp.Status == "ready" {
		return cp, nil
	}
	var candidate CheckpointManifest
	if err = json.Unmarshal(raw, &candidate); err != nil {
		return db.ComputerCheckpoint{}, err
	}
	// Reuse full candidate validation and immutable pin checks. A ready receipt
	// can only reference a previously registered manifest (checked above).
	if _, err = source.register(ctx, ref, manifest); err != nil {
		return db.ComputerCheckpoint{}, err
	}
	q := db.New(tx)
	descriptors := candidate.runtimeObjects()
	for n, d := range descriptors {
		o := observed[n]
		if o.Digest != d.Digest || o.SizeBytes != d.SizeBytes || o.MediaType != d.MediaType {
			return db.ComputerCheckpoint{}, ErrCheckpointCandidate
		}
	}
	runtimeComputer := candidate.RuntimeState.Computer
	locator, err := runtimeComputer.Root.Locator(instance.ReservedGuestEphemeralDiskBytes)
	if err != nil {
		return db.ComputerCheckpoint{}, err
	}
	root, err := q.LockComputerObject(ctx, db.LockComputerObjectParams{EnvironmentID: instance.EnvironmentID, Digest: runtimeComputer.Root.Pack.Digest})
	if err != nil {
		return db.ComputerCheckpoint{}, err
	}
	var inspection blockformat.ObjectInspection
	if !root.Certified.Bool || json.Unmarshal(root.Inspection, &inspection) != nil || inspection.Pack == nil {
		return db.ComputerCheckpoint{}, ErrCheckpointCandidate
	}
	if err = inspection.Pack.CheckRoot(locator, runtimeComputer.LogicalBytes); err != nil {
		return db.ComputerCheckpoint{}, err
	}
	if err = source.requireRootPinned(ctx, ref.DesiredVersion, root.Digest); err != nil {
		return db.ComputerCheckpoint{}, err
	}
	version, err := q.CreatePrivateCheckpointComputerDiskVersion(ctx, db.CreatePrivateCheckpointComputerDiskVersionParams{ID: pgvalue.NewUUIDv7(), EnvironmentID: instance.EnvironmentID, CheckpointID: cp.ID})
	if err != nil {
		return db.ComputerCheckpoint{}, err
	}
	rootJSON, err := json.Marshal(runtimeComputer.Root)
	if err != nil {
		return db.ComputerCheckpoint{}, err
	}
	rootID, err := q.RetainComputerDiskRoot(ctx, db.RetainComputerDiskRootParams{ID: pgvalue.NewUUIDv7(), EnvironmentID: instance.EnvironmentID, Locator: rootJSON})
	if err != nil {
		return db.ComputerCheckpoint{}, err
	}
	if err = q.CreateComputerDiskVersionRoot(ctx, db.CreateComputerDiskVersionRootParams{EnvironmentID: instance.EnvironmentID, ComputerID: instance.ComputerID, VersionID: version.ID, RootID: rootID}); err != nil {
		return db.ComputerCheckpoint{}, err
	}
	kinds := []string{"computer_checkpoint_vm_config", "computer_checkpoint_vm_state", "computer_checkpoint_memory", "computer_checkpoint_scratch_disk"}
	artifacts := make([]db.Artifact, 0, 4)
	for i, d := range descriptors {
		if _, err = q.UpsertCasObject(ctx, db.UpsertCasObjectParams{OrgID: instance.OrgID, Digest: d.Digest, SizeBytes: d.SizeBytes, MediaType: d.MediaType}); err != nil {
			return db.ComputerCheckpoint{}, err
		}
		a, e := q.CreateArtifact(ctx, db.CreateArtifactParams{ID: pgvalue.NewUUIDv7(), OrgID: instance.OrgID, ProjectID: instance.ProjectID, EnvironmentID: instance.EnvironmentID, Digest: d.Digest, SizeBytes: d.SizeBytes, MediaType: d.MediaType, Kind: db.ArtifactKind(kinds[i]), CreatedByWorkerHostID: pgvalue.UUID(ref.Host.HostID)})
		if e != nil {
			return db.ComputerCheckpoint{}, e
		}
		artifacts = append(artifacts, a)
	}
	// Blocking storage rows may have outlived execution authority.
	if err = source.checkMembers(ctx, len(candidate.RecoveryPoint.Runs)); err != nil {
		return db.ComputerCheckpoint{}, err
	}
	if err = source.checkLive(ctx, ref); err != nil {
		return db.ComputerCheckpoint{}, err
	}
	phases, err := json.Marshal(manifest.Phases)
	if err != nil {
		return db.ComputerCheckpoint{}, err
	}
	if manifest.Phases == nil {
		phases = nil
	}
	cp, err = q.MarkComputerCheckpointReady(ctx, db.MarkComputerCheckpointReadyParams{EnvironmentID: instance.EnvironmentID, CheckpointID: cp.ID, DesiredVersion: ref.DesiredVersion, PrivateComputerDiskVersionID: version.ID, VMConfigArtifactID: artifacts[0].ID, VMStateArtifactID: artifacts[1].ID, MemoryArtifactID: artifacts[2].ID, ScratchDiskArtifactID: artifacts[3].ID, Manifest: raw, PhaseTimings: phases, ReadyRequestFingerprint: pgvalue.Text(fingerprint)})
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

// prepareCheckpointReady locks the checkpoint source and requires the
// candidate, without timings and with members in registration order, to be
// the registered manifest. It returns the canonical candidate and the
// readiness receipt fingerprint; a ready checkpoint must carry that receipt.
func prepareCheckpointReady(ctx context.Context, tx pgx.Tx, ref CheckpointRef, manifest CheckpointManifest) (checkpointSource, []byte, string, error) {
	source, err := lockCheckpointSource(ctx, tx, ref)
	if err != nil {
		return checkpointSource{}, nil, "", err
	}
	cp := source.checkpoint
	candidate := manifest
	candidate.Phases = nil
	// Registration sorted the sealed member set. Normalize without mutating input.
	candidate.RecoveryPoint.Runs = append([]CheckpointRun{}, candidate.RecoveryPoint.Runs...)
	sort.Slice(candidate.RecoveryPoint.Runs, func(i, j int) bool {
		return candidate.RecoveryPoint.Runs[i].RunID < candidate.RecoveryPoint.Runs[j].RunID
	})
	raw, err := json.Marshal(candidate)
	if err != nil {
		return checkpointSource{}, nil, "", err
	}
	raw, err = jsoncanon.Transform(raw)
	if err != nil {
		return checkpointSource{}, nil, "", err
	}
	stored, err := jsoncanon.Transform(cp.Manifest)
	if err != nil {
		return checkpointSource{}, nil, "", ErrCheckpointCandidate
	}
	if !bytes.Equal(raw, stored) {
		return checkpointSource{}, nil, "", ErrCheckpointCandidate
	}
	fingerprint := checkpointReadyFingerprint(ref, raw)
	if cp.Status == "ready" && cp.ReadyRequestFingerprint.String == fingerprint {
		return source, raw, fingerprint, nil
	}
	if cp.Status != "creating" {
		return checkpointSource{}, nil, "", pgx.ErrNoRows
	}
	return source, raw, fingerprint, nil
}

func checkpointReadyFingerprint(ref CheckpointRef, raw []byte) string {
	return sha256sum.DigestBytes(append([]byte(fmt.Sprintf("computer.checkpoint.ready\x00%s\x00%d\x00%d\x00", ref.InstanceID.String(), ref.WorkerEpoch, ref.DesiredVersion)), raw...))
}
