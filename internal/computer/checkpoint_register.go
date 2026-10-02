package computer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/jsoncanon"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
	"github.com/jackc/pgx/v5"
)

// ErrCheckpointCandidate reports a checkpoint candidate or failure report
// whose content the capture source rejects: a manifest that does not match
// the sealed checkpoint, its runtime platform or its disk, or an invalid
// failure message.
var ErrCheckpointCandidate = errors.New("invalid Computer checkpoint candidate")

// RegisterCheckpoint pins a complete immutable capture candidate before its
// upload, in one transaction under the checkpoint source fence. It claims no
// remote object existence, moves no Computer head and grants no restored Run
// execution authority. An exact registered candidate replays, including with
// members reordered or different timings. A fence that no longer holds, or a
// different registered candidate, reports ErrAuthorityChanged.
func RegisterCheckpoint(ctx context.Context, txb db.TxBeginner, ref CheckpointRef, manifest CheckpointManifest) (db.ComputerCheckpoint, error) {
	var checkpoint db.ComputerCheckpoint
	err := db.RunTx(ctx, txb, func(tx pgx.Tx) error {
		source, err := lockCheckpointSource(ctx, tx, ref)
		if err != nil {
			return err
		}
		checkpoint, err = source.register(ctx, ref, manifest)
		return err
	})
	if err != nil {
		return db.ComputerCheckpoint{}, authorityChanged(err)
	}
	return checkpoint, nil
}

type checkpointObject struct {
	role, media string
	artifact    CheckpointArtifact
}

// register validates the candidate against the sealed checkpoint, its members,
// the VM platform and the Instance's disk, and records its canonical manifest
// and runtime objects. It rechecks the live source and the member set after
// the blocking object writes.
func (s checkpointSource) register(ctx context.Context, ref CheckpointRef, manifest CheckpointManifest) (db.ComputerCheckpoint, error) {
	q := db.New(s.tx)
	instance, cp := s.instance, s.checkpoint
	environmentID, computerID := instance.EnvironmentID, instance.ComputerID
	if err := s.checkLive(ctx, ref); err != nil {
		return db.ComputerCheckpoint{}, err
	}
	members, err := q.ListComputerCheckpointRuns(ctx, db.ListComputerCheckpointRunsParams{EnvironmentID: environmentID, CheckpointID: cp.ID})
	if err != nil {
		return db.ComputerCheckpoint{}, err
	}
	platform, err := q.GetVMPlatformForCheckpoint(ctx, instance.VMPlatformID)
	if err != nil {
		return db.ComputerCheckpoint{}, err
	}
	point := manifest.RecoveryPoint
	if point.ID != ref.CheckpointID.String() || point.ComputerID != pgvalue.UUIDString(computerID) || point.ComputerInstanceID != ref.InstanceID.String() || point.WriterGeneration != cp.WriterGeneration || point.MembershipRevision != cp.MembershipRevision || point.ComputerSpecID != pgvalue.UUIDString(cp.ComputerSpecID) || point.ProgramDeploymentID != pgvalue.UUIDString(cp.ProgramDeploymentID) || len(point.Runs) != len(members) || point.Runs == nil {
		return db.ComputerCheckpoint{}, ErrCheckpointCandidate
	}
	identity := point.Runtime
	if identity.Backend != "firecracker" || identity.ID != platform.ID || identity.Arch != platform.Arch || identity.Contract != platform.Contract || identity.KernelDigest != platform.KernelDigest || identity.InitramfsDigest != platform.InitramfsDigest || identity.RootfsDigest != platform.RootfsDigest || !sha256sum.ValidDigest(identity.ConfigDigest) || identity.VMVCPUCount != instance.VMVCPUCount || identity.CPUConfigDigest != instance.CPUConfigDigest {
		return db.ComputerCheckpoint{}, ErrCheckpointCandidate
	}
	captured := manifest.RuntimeState.Computer
	if captured == nil || captured.ComputerID != point.ComputerID || captured.LogicalBytes != instance.ReservedGuestEphemeralDiskBytes || captured.Root.Validate(captured.LogicalBytes) != nil || manifest.ComputerState.Base.MountPath != "/workspace" {
		return db.ComputerCheckpoint{}, ErrCheckpointCandidate
	}
	byRun := make(map[string]CheckpointRun, len(point.Runs))
	for _, member := range point.Runs {
		if _, exists := byRun[member.RunID]; exists || strings.TrimSpace(member.CorrelationID) == "" {
			return db.ComputerCheckpoint{}, ErrCheckpointCandidate
		}
		byRun[member.RunID] = member
	}
	manifest.RecoveryPoint.Runs = make([]CheckpointRun, 0, len(members))
	for _, member := range members {
		supplied, exists := byRun[pgvalue.UUIDString(member.RunID)]
		if !exists || supplied.AttemptNumber != member.AttemptNumber || supplied.RunLeaseID != pgvalue.UUIDString(member.SourceRunLeaseID) || supplied.RunWaitID != pgvalue.UUIDString(member.RunWaitID) || (supplied.SessionSpeculativeInputSequence != nil) != member.SessionSpeculativeInputSequence.Valid || (supplied.SessionSpeculativeInputSequence != nil && *supplied.SessionSpeculativeInputSequence != member.SessionSpeculativeInputSequence.Int64) {
			return db.ComputerCheckpoint{}, ErrCheckpointCandidate
		}
		manifest.RecoveryPoint.Runs = append(manifest.RecoveryPoint.Runs, supplied)
	}
	if err = s.checkMembers(ctx, len(members)); err != nil {
		return db.ComputerCheckpoint{}, err
	}

	if len(manifest.RuntimeState.MemoryArtifacts) != 1 || !json.Valid(manifest.RuntimeState.Config) {
		return db.ComputerCheckpoint{}, ErrCheckpointCandidate
	}
	objects := []checkpointObject{{"vm_config", cas.CheckpointVMConfigMediaType, manifest.RuntimeState.ConfigArtifact}, {"vm_state", cas.CheckpointVMStateMediaType, manifest.RuntimeState.VMStateArtifact}, {"scratch_disk", cas.CheckpointScratchDiskMediaType, manifest.RuntimeState.ScratchDiskArtifact}, {"memory", cas.CheckpointMemoryMediaType, manifest.RuntimeState.MemoryArtifacts[0]}}
	seen := map[string]bool{}
	for _, object := range objects {
		a := object.artifact
		if !sha256sum.ValidDigest(a.Digest) || a.SizeBytes <= 0 || a.MediaType != object.media || seen[a.Digest] {
			return db.ComputerCheckpoint{}, ErrCheckpointCandidate
		}
		seen[a.Digest] = true
	}
	manifest.Phases = nil
	encoded, err := json.Marshal(manifest)
	if err != nil {
		return db.ComputerCheckpoint{}, fmt.Errorf("%w: %v", ErrCheckpointCandidate, err)
	}
	encoded, err = jsoncanon.Transform(encoded)
	if err != nil || len(encoded) > 65536 {
		return db.ComputerCheckpoint{}, ErrCheckpointCandidate
	}
	n, err := q.RegisterCheckpointManifest(ctx, db.RegisterCheckpointManifestParams{ID: cp.ID, Manifest: encoded})
	if err != nil {
		return db.ComputerCheckpoint{}, err
	}
	if n != 1 {
		return db.ComputerCheckpoint{}, pgx.ErrNoRows
	}
	for _, object := range objects {
		a := object.artifact
		if _, err = q.RegisterCheckpointObject(ctx, db.RegisterCheckpointObjectParams{CheckpointID: cp.ID, Role: object.role, Digest: a.Digest, SizeBytes: a.SizeBytes, MediaType: a.MediaType}); err != nil {
			return db.ComputerCheckpoint{}, err
		}
	}
	if err = s.checkLive(ctx, ref); err != nil {
		return db.ComputerCheckpoint{}, err
	}
	if err = s.checkMembers(ctx, len(members)); err != nil {
		return db.ComputerCheckpoint{}, err
	}
	cp.Manifest = encoded
	return cp, nil
}
